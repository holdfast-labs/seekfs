package main

// WP7: the compound content lane. A query that ANDs a positive content leaf
// with a selective filename selector (ext: or a component root such as dir:)
// is answered by driving the filename candidate iterator and verifying each
// candidate's content inline, instead of materializing the content candidate
// superset. The two sides are both requirements, so the lane returns exactly
// the set the per-volume content path does; it is a cost optimization, not a
// semantic change.
//
// contentCandidatesBounded already maps matching content docIDs to local record
// IDs, so the content set is in globalRecordID space; the lane instead drives
// the filename side and filters by content. A bare `ext:` selector is the
// ext-only source, anything else with a root is the component source.
//
// Multi-volume (M8) is handled by the content path, not the lane: the content
// path pre-truncates each volume at the user limit in rank order before applying
// the shared comparator, a rule the lane cannot mirror exactly, so the lane is
// single-volume (where per-volume truncation keeps the same top by the same
// order, so the lane is provably parity). A volume without usable content still
// never blocks the query — the content path skips it and reports partial.
//
// ON by default; SEEKFS_CONTENT_GLOBAL_LANE=0 is the kill switch back to the
// per-volume content path. Declines to the content path: relevance order,
// boolean content groups, multi-usable-volume, any shape whose filename part
// is not a supported root, a truncated catch-up (healthIncomplete), a missing
// overlay snapshot, and a biased query while the volume carries overlay
// activity (hidden base IDs or overlay records: the content path's merged
// re-sort cannot be mirrored from a biased page, so the content path owns
// that corner).
//
// `under:` is narrowed on the filename side by the under-root subtree
// iterator (globalUnderIterator over vol.underRootIDs, the same narrowing the
// per-volume candidate planner in candidate_build.go applies) and enforced
// exactly by inline verification plus the search-time implicit-under
// filesystem re-check; on a count the M9 search-stats split is surfaced with
// ContentCountDivergent, mirroring searchContentVolume. `exists:` is enforced
// by the same inline verification (entryMatchesScalar stats the path) with
// the same divergent-count flag. Overlays are handled lane-natively: hidden
// base IDs are skipped during the drive and live overlay records are merged
// with content verification (overlayRankedMatches), mirroring
// mergeOverlayMatches including its merged re-sort whenever overlay records
// exist. A biased query on a pristine volume applies the same stable
// root-first partition the per-volume scan's biasOrderCompact applies.
//
// Known metadata difference: the lane verifies every filename candidate, so its
// set is exact and it reports complete even where the content path's per-volume
// window probe would conservatively set ContentIncomplete (a match set larger
// than its completeness window). The result set/order/count still match; only
// the completeness flag is more accurate. The count path refuses nothing the
// slow path would refuse except through the shared healthIncomplete decline:
// the lane's filename-driven set is never budget-capped, so a count it answers
// is exact.

import (
	"cmp"
	"os"
	"slices"
	"sort"
)

// contentGlobalLaneEnabled reports whether the compound content lane may
// engage. It is ON by default; SEEKFS_CONTENT_GLOBAL_LANE=0 is the kill
// switch back to the per-volume content path (SEEKFS_CONTENT_GLOBAL_LANE=1
// keeps it explicitly enabled).
func contentGlobalLaneEnabled() bool {
	return os.Getenv("SEEKFS_CONTENT_GLOBAL_LANE") != "0"
}

// contentLaneUsableSnapshots aligns the query snapshot's overlay snapshots
// with the lane's usable-volume slice. The filename iterator the lane drives
// is built over usable (indexing it 0..len(usable)-1), so hidden filtering
// and the overlay merge must use the same alignment; a usable volume missing
// from the snapshot (or a missing snapshot overall) declines the lane.
func contentLaneUsableSnapshots(snapshot globalQuerySnapshot, usable []*serviceVolumeIndex) ([]*volumeSnapshot, bool) {
	if !snapshot.overlaysOK || len(snapshot.overlays) != len(snapshot.volumes) {
		return nil, false
	}
	out := make([]*volumeSnapshot, len(usable))
	for i, vol := range usable {
		found := false
		for v, snapVol := range snapshot.volumes {
			if v < len(snapshot.overlays) && snapVol == vol {
				out[i] = snapshot.overlays[v]
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return out, true
}

// contentLaneHasOverlayActivity reports whether any usable volume carries
// overlay state: hidden base IDs (tombstoned/shadowed records the drive must
// skip) or overlay records (live slots the merge must add). A biased query
// declines on any activity: the content path merges and re-sorts by the shared
// comparator whenever overlay records exist, which a biased page cannot
// mirror, and hidden base IDs change which biased candidates survive.
func contentLaneHasOverlayActivity(snaps []*volumeSnapshot) bool {
	for _, snap := range snaps {
		if snap == nil {
			continue
		}
		if len(snap.tombstoneIDs) > 0 || len(snap.shadowedIDs) > 0 || len(snap.records) > 0 {
			return true
		}
	}
	return false
}

// collectContentLaneEntries verifies every candidate against the volume's
// content and orders the result exactly like the per-volume content path
// (content_verify.go:1042-1056): each volume's matches keep their rankForQuery
// order with record-id ties (the stable sort preserves the iterator's local-id
// order), and only when the merged set spans volumes is the shared
// multi-volume comparator applied. collectGlobalVerifiedTopN cannot be reused
// here because its heap compares by a different global rule than its final
// sort, which drops or misorders tied basenames at a limit.
//
// Hidden base IDs from the usable-aligned snapshots are skipped during the
// drive, mirroring the content path's hidden-aware scan; live overlay records
// are merged afterward by the caller with mergeGlobalOverlayEntries. A biased
// query applies the same stable root-first partition the per-volume scan's
// biasOrderCompact applies (the scope only engages bias on overlay-quiet
// volumes, so no merged re-sort can disturb it).
func collectContentLaneEntries(it globalIDIterator, volumes []*serviceVolumeIndex, snaps []*volumeSnapshot, pq parsedQuery, matcher *contentLeafMatcher) ([]globalRankedEntry, int, error) {
	perVol := make([][]globalRankedEntry, len(volumes))
	rankers := make([]func(int) int, len(volumes))
	for i, vol := range volumes {
		if vol != nil && vol.index != nil {
			rankers[i] = candidateRanker(vol.index, vol.rankForQuery(pq))
		}
	}
	biasRoot := firstNonEmpty(pq.CWDBias, pq.RootBias)
	pathCaches := make([]map[int]string, len(volumes))
	verified := 0
	for {
		id, ok := it.Next()
		if !ok {
			break
		}
		if verified&1023 == 0 && queryCanceled(pq) {
			return nil, verified, errQueryCanceled
		}
		if id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		if globalHiddenContains(snaps, id) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		entry, ok := compactCandidateEntryIfMatchIn(vol, vol.index, volumePQ, id.local, pathCaches[id.volume], true, false, matcher)
		verified++
		if !ok {
			continue
		}
		rank := int(^uint(0) >> 1)
		if rankers[id.volume] != nil {
			rank = rankers[id.volume](id.local)
		}
		perVol[id.volume] = append(perVol[id.volume], globalRankedEntry{entry: entry, rank: rank, volume: id.volume, tie: entry.Path})
	}
	var ranked []globalRankedEntry
	for _, bucket := range perVol {
		if len(bucket) == 0 {
			continue
		}
		slices.SortStableFunc(bucket, func(a, b globalRankedEntry) int { return cmp.Compare(a.rank, b.rank) })
		if biasRoot != "" {
			// Mirror biasOrderCompact: a stable root-first partition of
			// the rank order. entry.Path is the same reconstructed
			// compact path the scan tests (built with the same cache
			// above), and pathUnder normalizes both spellings through
			// filepath, so membership verdicts agree exactly.
			slices.SortStableFunc(bucket, func(a, b globalRankedEntry) int {
				au, bu := pathUnder(a.entry.Path, biasRoot), pathUnder(b.entry.Path, biasRoot)
				if au == bu {
					return 0
				}
				if au {
					return -1
				}
				return 1
			})
		}
		ranked = append(ranked, bucket...)
	}
	if globalRankedEntriesSpanMultipleVolumes(ranked) {
		sortGlobalRankedEntries(ranked, pq)
	}
	return ranked, verified, nil
}

// contentLeavesUnderBooleanGroups reports whether any content leaf sits inside an
// OR or NOT group, where stripContentLeaves cannot preserve it as a superset.
func contentLeavesUnderBooleanGroups(pq parsedQuery) bool {
	for _, group := range pq.OrGroups {
		for i := range group {
			if queryHasAnyContentLeaf(group[i]) {
				return true
			}
		}
	}
	for i := range pq.NotGroups {
		if queryHasAnyContentLeaf(pq.NotGroups[i]) {
			return true
		}
	}
	return false
}

// contentLaneScope returns the content-usable volumes the lane may drive and the
// skipped (unusable) volume labels, or ok=false when the lane does not apply and
// the per-volume content path must answer. needOrder is true for a search (which
// orders results) and false for a count.
func contentLaneScope(snapshot globalQuerySnapshot, pq parsedQuery, needOrder bool) (usable []*serviceVolumeIndex, skipped []string, ok bool) {
	if !snapshot.overlaysOK {
		// Without a coherent overlay snapshot the lane cannot filter hidden
		// base IDs; the filename lanes decline this the same way.
		return nil, nil, false
	}
	// A content leaf under an OR/NOT group is not a superset under
	// stripContentLeaves. A case-sensitive filename part cannot drive the
	// lowercase-keyed filename postings, so the iterator would not be a
	// superset; the content path owns it.
	if contentLeavesUnderBooleanGroups(pq) || stripContentLeaves(pq).CaseSensitive {
		return nil, nil, false
	}
	if needOrder && pq.SortColumn == "relevance" {
		// Relevance is a post-verify rank over an enlarged window, not a
		// per-record order the lane can reproduce. Counts ignore order, so
		// they stay eligible.
		return nil, nil, false
	}
	if stripContentLeaves(pq).isEmpty() {
		return nil, nil, false
	}
	usable, skipped = contentUsableVolumes(snapshot.volumes, pq)
	if len(usable) != 1 {
		// Single volume only: the content path's per-volume pre-truncation
		// (rank order) before the shared comparator cannot be mirrored exactly
		// for multiple volumes, so multi-volume stays on the content path (M8
		// there: an unusable volume is skipped and surfaced partial).
		return nil, nil, false
	}
	for _, vol := range usable {
		// A truncated catch-up leaves the volume usable but incomplete: the
		// content path marks the trace incomplete and refuses a count, which the
		// lane does not reproduce, so decline and let the content path own the
		// signal.
		if vol == nil || vol.index == nil || vol.content == nil || !vol.content.usableForQuery() || vol.content.healthIncomplete() {
			return nil, nil, false
		}
	}
	if usableSnaps, snapOK := contentLaneUsableSnapshots(snapshot, usable); snapOK {
		if (pq.RootBias != "" || pq.CWDBias != "") && contentLaneHasOverlayActivity(usableSnaps) {
			// The content path merges overlay records and re-sorts by the
			// shared comparator whenever they exist, which a biased page
			// cannot mirror; hidden base IDs likewise change which biased
			// candidates survive. Bias on a pristine volume is exact, so
			// only the overlay-active corner declines.
			return nil, nil, false
		}
	} else {
		return nil, nil, false
	}
	return usable, skipped, true
}

// contentLaneDriveFloor bounds the lane's filename-side drive from below:
// the lane verifies every filename candidate inline (~1-2µs each), so below
// ~1k candidates it is comparable to the slow path's window machinery while
// staying exact. Above the budget (scaled by 4x the user limit, since the
// slow path must fully verify at least limit matches) the slow path's bounded
// scan wins, so the lane declines and the content path answers.
const contentLaneDriveFloor = 1024

// contentLaneDriveBudget caps how many filename candidates the lane may
// verify: at most 4x the user limit (the slow path fully verifies at least
// that many matches), with a floor so tiny limits still admit selective
// sides. It is a cost heuristic, not a correctness bound: exceeding it only
// declines to the exact slow path.
func contentLaneDriveBudget(limit int) int {
	if b := 4 * limit; b > contentLaneDriveFloor {
		return b
	}
	return contentLaneDriveFloor
}

// contentLaneEstOver is the "too big or unknown" estimate: anything above the
// drive budget declines the lane.
const contentLaneEstOver = int(^uint(0) >> 1)

// contentLaneSubtreeBoundedTotal counts the union of root subtrees, stopping
// past cap (saturating). Overlapping roots over-count, which is safe: the
// total stays an upper bound. Roots without usable intervals on an index
// without child data are unknowable (a full scan would be needed), so they
// report over and the lane declines.
func contentLaneSubtreeBoundedTotal(vol *serviceVolumeIndex, roots []int, cap int) int {
	total := 0
	for _, r := range roots {
		if total > cap {
			return contentLaneEstOver
		}
		if vol.subtreeIntervalValid(r) {
			total += int(vol.subtreeEnd[r] - vol.subtreeStart[r])
			continue
		}
		if len(vol.childOffsets) == 0 && vol.children == nil {
			return contentLaneEstOver
		}
		total += len(vol.underDescendantsLimited(r, cap-total+1))
	}
	if total > cap {
		return contentLaneEstOver
	}
	return total
}

// contentLaneUnderTotal bounds one volume's under: subtree size: exact via
// intervals (deleted records over-counted, which is safe), bounded walk
// otherwise, unknown when neither traversal exists.
func contentLaneUnderTotal(vol *serviceVolumeIndex, under string, cap int) int {
	if vol == nil || vol.index == nil {
		return contentLaneEstOver
	}
	return contentLaneSubtreeBoundedTotal(vol, vol.underRootIDs(under), cap)
}

// contentLaneExtTotal bounds one volume's ext posting size (O(1)): the posting
// length plus live recent IDs, which the iterator may additionally yield.
func contentLaneExtTotal(vol *serviceVolumeIndex, ext string) int {
	if vol == nil || vol.index == nil {
		return contentLaneEstOver
	}
	posting, ok := vol.extPostingCountCandidate(ext)
	if !ok {
		return contentLaneEstOver
	}
	return posting.len() + len(vol.recentIDs)
}

// contentLaneDirTotal bounds one volume's dir: subtree size: the dirs carrying
// the component plus their bounded subtrees. Unknown when the component has
// no resolvable roots.
func contentLaneDirTotal(vol *serviceVolumeIndex, dir string, cap int) int {
	if vol == nil || vol.index == nil {
		return contentLaneEstOver
	}
	return contentLaneSubtreeBoundedTotal(vol, vol.pathComponentRootIDs(dir), cap)
}

// contentLaneParentTotal bounds one volume's parent: set: the direct children
// of dirs carrying the component (one parent per record, so disjoint).
func contentLaneParentTotal(vol *serviceVolumeIndex, parent string, cap int) int {
	if vol == nil || vol.index == nil {
		return contentLaneEstOver
	}
	total := 0
	for _, root := range vol.pathComponentRootIDs(parent) {
		if total > cap {
			return contentLaneEstOver
		}
		total += len(vol.childIDsForRecord(root)) + len(vol.recentIDs)
	}
	if total > cap {
		return contentLaneEstOver
	}
	return total
}

// contentLaneDriveEstimate upper-bounds the lane's filename-side candidate
// count for one volume. Every estimated dimension is an upper bound on the
// final intersection (conjuncts only shrink it), so the min over estimated
// dimensions is too; dimensions without a cheap bound (terms, regexps, attrs,
// type, scalar filters, negations) are skipped, never guessed. OR-unions can
// exceed every dimension, so any OR group declines outright. No estimated
// dimension at all means unknown: decline.
func contentLaneDriveEstimateOne(vol *serviceVolumeIndex, filenamePQ parsedQuery, cap int) int {
	est := contentLaneEstOver
	known := false
	note := func(v int) {
		if v < est {
			est = v
			known = true
		}
	}
	if filenamePQ.Under != "" {
		note(contentLaneUnderTotal(vol, filenamePQ.Under, cap))
	}
	if filters, ok := globalExtPostingFilters(filenamePQ); ok && len(filters) == 1 {
		note(contentLaneExtTotal(vol, filters[0].ext))
	} else if exts, ok := simpleGlobExts(filenamePQ.Globs); ok && len(exts) == 1 {
		note(contentLaneExtTotal(vol, exts[0]))
	}
	for _, dir := range filenamePQ.Dirs {
		note(contentLaneDirTotal(vol, dir, cap))
	}
	for _, parent := range filenamePQ.Parents {
		note(contentLaneParentTotal(vol, parent, cap))
	}
	if !known {
		return contentLaneEstOver
	}
	return est
}

// contentLaneDriveEstimate sums the per-volume upper bounds, saturating past
// cap: the volumes' shares are disjoint, so the sum stays an upper bound.
func contentLaneDriveEstimate(volumes []*serviceVolumeIndex, filenamePQ parsedQuery, cap int) int {
	total := 0
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			return contentLaneEstOver
		}
		total += contentLaneDriveEstimateOne(vol, filenamePQ, cap)
		if total > cap {
			return contentLaneEstOver
		}
	}
	return total
}

// globalContentFilenameIterator builds the filename-side candidate iterator for
// a content query's filename part: the ext-only posting source when the part is
// ext-only, otherwise the component source. ok=false means the filename part is
// not a shape the lane supports, so the content path answers.
//
// An `under:` scope narrows the drive to the under-root subtrees on the
// filename side (globalUnderIterator over vol.underRootIDs, the same
// narrowing the per-volume candidate planner in candidate_build.go applies
// via its under source). The iterator is intersected with the rest of the
// filename part; inline verification enforces the exact predicate.
//
// budget caps the drive: the lane verifies every filename candidate, so it
// only engages when the estimated side is small (see contentLaneDriveBudget).
// Anything bigger, or anything the estimator cannot bound, declines to the
// exact slow path.
func globalContentFilenameIterator(volumes []*serviceVolumeIndex, filenamePQ parsedQuery, trace *searchTrace, budget int) (globalIDIterator, bool) {
	if len(filenamePQ.OrGroups) != 0 {
		// A union can exceed every dimension's estimate, so the upper
		// bound is unsound: decline (fallback stays exact).
		return nil, false
	}
	if contentLaneDriveEstimate(volumes, filenamePQ, budget) > budget {
		return nil, false
	}
	if filenamePQ.Under != "" {
		underIt, ok := globalUnderIterator(volumes, filenamePQ.Under)
		if !ok {
			return nil, false
		}
		rest := filenamePQ
		rest.Under = ""
		restIt, ok := globalContentFilenameIteratorNoUnder(volumes, rest, trace)
		if !ok {
			// The filename part is just the under: scope (no other
			// narrowing constraint): drive the under-root subtrees
			// alone. Anything that narrows the set (terms, exts, dirs
			// and friends) must have a supported iterator above, or
			// the content path answers.
			if !filenamePartNarrows(rest) {
				return underIt, true
			}
			return nil, false
		}
		if restIt == nil {
			return underIt, true
		}
		return newGlobalIntersectionIterator(underIt, restIt), true
	}
	return globalContentFilenameIteratorNoUnder(volumes, filenamePQ, trace)
}

// filenamePartNarrows reports whether the (content-stripped, under-stripped)
// filename part constrains the candidate set beyond "every record". Exists,
// sort, bias and path-mode flags ride along for verification/ordering only;
// everything else needs its own iterator, so the under-alone fallback must
// not claim it.
func filenamePartNarrows(pq parsedQuery) bool {
	return len(pq.Terms) != 0 || len(pq.Exts) != 0 || len(pq.Dirs) != 0 ||
		len(pq.Globs) != 0 || len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 ||
		len(pq.Parents) != 0 || len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0 ||
		len(pq.AttrFilters) != 0 || len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 ||
		len(pq.Features) != 0 || pq.Type != "" || pq.HasModAfter
}

// globalContentFilenameIteratorNoUnder is globalContentFilenameIterator for a
// filename part without an `under:` scope: the ext-only posting source when
// the part is ext-only, otherwise the component source.
func globalContentFilenameIteratorNoUnder(volumes []*serviceVolumeIndex, filenamePQ parsedQuery, trace *searchTrace) (globalIDIterator, bool) {
	if globalExtOnlySupported(filenamePQ) {
		extFilters, ok := globalExtPostingFilters(filenamePQ)
		if !ok || len(extFilters) != 1 {
			return nil, false
		}
		ext := extFilters[0].ext
		iters := make([]globalIDIterator, 0, len(volumes))
		for i, vol := range volumes {
			if vol == nil || vol.index == nil {
				return nil, false
			}
			posting, ok := vol.extPostingCountCandidate(ext)
			if !ok {
				return nil, false
			}
			it := newGlobalPostingIteratorWithTrace(i, posting, trace)
			iters = append(iters, &it)
		}
		switch len(iters) {
		case 0:
			return nil, false
		case 1:
			return iters[0], true
		default:
			return newGlobalMergeIterator(iters...), true
		}
	}
	if !globalComponentDefaultHasRoot(filenamePQ) {
		return nil, false
	}
	if !globalComponentQuerySupportedMulti(filenamePQ, nonVolumeTerms(filenamePQ.Terms), len(volumes) > 1) {
		return nil, false
	}
	return globalComponentQueryIterator(volumes, filenamePQ, trace)
}

// searchServiceVolumesGlobalContentComponentsSnapshot answers a compound
// content + filename query from the filename iterator with inline content
// verification. It declines for anything outside the lane's scope, leaving the
// per-volume content path to answer.
func searchServiceVolumesGlobalContentComponentsSnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool, pq parsedQuery) ([]Entry, bool, error) {
	if countOnly || !contentGlobalLaneEnabled() {
		return nil, false, nil
	}
	usable, skipped, ok := contentLaneScope(snapshot, pq, true)
	if !ok {
		return nil, false, nil
	}
	for _, vol := range usable {
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return nil, true, err
		}
	}
	usableSnaps, snapOK := contentLaneUsableSnapshots(snapshot, usable)
	if !snapOK {
		return nil, false, nil
	}
	// Verify the original predicate (filename + content) inline; the matcher is
	// built once and compactCandidateEntryIfMatchIn consumes base and delta
	// content through the volume's reader/resolver. Hidden base IDs are
	// skipped during the drive; live overlay records merge afterward.
	matcher := newContentLeafMatcher(pq)
	limit := normalizedLimit(opts.Limit, false)
	it, ok := globalContentFilenameIterator(usable, stripContentLeaves(pq), opts.Trace, contentLaneDriveBudget(limit))
	if !ok {
		return nil, false, nil
	}
	base, verified, err := collectContentLaneEntries(it, usable, usableSnaps, pq, matcher)
	if err != nil {
		return nil, true, err
	}
	// Merge live overlay records exactly as mergeOverlayMatches does per
	// volume: no overlay records means the base set is returned as-is (the
	// lane's rank/bias order above is final); otherwise the merged set is
	// stable-sorted by the shared comparator, which owns the order there
	// (the scope only engages bias on overlay-quiet volumes, so bias never
	// meets this re-sort). mergeGlobalOverlayEntries cannot be reused: it
	// re-sorts by rank even with no overlay entries, which would destroy
	// the bias partition above.
	var merged []globalRankedEntry
	if globalSnapshotsHaveOverlayRecords(usableSnaps) {
		merged = append(merged, base...)
		pathCache := make(map[int]string)
		for i, vol := range usable {
			if i >= len(usableSnaps) || usableSnaps[i] == nil {
				continue
			}
			for _, overlay := range vol.overlayRankedMatches(usableSnaps[i], pq, pathCache) {
				merged = append(merged, globalRankedEntry{entry: overlay.entry, volume: i, tie: overlay.entry.Path, overlay: true})
			}
		}
		sort.SliceStable(merged, func(i, j int) bool {
			return compareSearchAllEntries(merged[i].entry, merged[j].entry, pq) < 0
		})
	} else {
		merged = base
	}
	if globalRankedEntriesSpanMultipleVolumes(merged) {
		sortGlobalRankedEntries(merged, pq)
	}
	filtered := globalRankedEntriesToEntries(merged)
	// The content path applies the implicit-under filesystem re-check
	// per volume BEFORE the caller's global merge and limit; the lane holds
	// the single volume's full verified set, so filter before trimming to
	// match exactly (a stale path must not consume a result slot).
	filtered = filterImplicitUnderExisting(filtered, opts, false)
	markContentQueryDegraded(opts.Trace, skipped)
	if opts.Trace != nil {
		opts.Trace.ComponentRecordsVerified += verified
		opts.Trace.setPlannerMode("global-content-components")
		opts.Trace.setSource("global:content-components", len(filtered))
		opts.Trace.setComplete(opts.Trace == nil || !opts.Trace.ContentPartial)
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	// Snippets are attached last so the work is bounded by the returned set,
	// mirroring searchContentServiceVolumes.
	if len(filtered) > 0 {
		volByPath := make(map[string]*serviceVolumeIndex, len(filtered))
		for i := range filtered {
			volByPath[filtered[i].Path] = usable[0]
		}
		attachContentSnippets(filtered, volByPath, contentPositiveLeaves(pq), matcher)
	}
	return filtered, true, nil
}

// countServiceVolumesGlobalContentComponentsSnapshot is the count twin of the
// compound content lane: it drives the same filename iterator and tallies the
// inline content verification, so count == len(search) for the in-scope shapes.
// A count never uses a capped content superset, so it is exact.
func countServiceVolumesGlobalContentComponentsSnapshot(snapshot globalQuerySnapshot, opts queryOptions, pq parsedQuery) (int, bool, error) {
	if !contentGlobalLaneEnabled() {
		return 0, false, nil
	}
	usable, skipped, ok := contentLaneScope(snapshot, pq, false)
	if !ok {
		return 0, false, nil
	}
	for _, vol := range usable {
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return 0, true, err
		}
	}
	it, ok := globalContentFilenameIterator(usable, stripContentLeaves(pq), opts.Trace, contentLaneDriveBudget(normalizedLimit(opts.Limit, true)))
	if !ok {
		return 0, false, nil
	}
	usableSnaps, snapOK := contentLaneUsableSnapshots(snapshot, usable)
	if !snapOK {
		return 0, false, nil
	}
	matcher := newContentLeafMatcher(pq)
	count, verified, err := countGlobalVerifiedIterator(it, usable, usableSnaps, pq, matcher)
	if err != nil {
		return 0, true, err
	}
	// The overlay merge owns the live-slot count, mirroring countContentVolume
	// (base tally plus overlayLiveMatchCountCancellable); both sides verify
	// the full predicate including content. A count never stats, so the
	// `under:`/Exists shapes can exceed a matching search: surface the M9
	// split with ContentCountDivergent exactly as searchContentVolume does.
	count += globalOverlayMatchCount(usable, usableSnaps, pq)
	markContentQueryDegraded(opts.Trace, skipped)
	if opts.Trace != nil && (opts.Under != "" || opts.Exists) {
		opts.Trace.ContentCountDivergent = true
	}
	if opts.Trace != nil {
		opts.Trace.ComponentRecordsVerified += verified
		opts.Trace.setPlannerMode("global-content-components")
		opts.Trace.setSource("global:content-components-count", count)
		opts.Trace.setComplete(opts.Trace == nil || !opts.Trace.ContentPartial)
	}
	return count, true, nil
}
