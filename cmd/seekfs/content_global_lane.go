package main

// WP7: the compound content lane. A query that ANDs a positive content leaf
// with a selective filename selector (ext: or a component root such as dir:) is
// answered by driving the filename candidate iterator and verifying each
// candidate's content inline, instead of materializing the content candidate
// superset. The two sides are both requirements, so the lane returns exactly the
// set the per-volume content path does; it is a cost optimization, not a
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
// Off by default behind SEEKFS_CONTENT_GLOBAL_LANE so it
// is validated against the content path before becoming the default route.
// Declines to the content path: overlays/hidden, relevance order, `under:`/
// `exists:` (the M9 search-stats/count-does-not split), biased order, boolean
// content groups, multi-volume, and any shape whose filename part is not a
// supported root.
//
// Known metadata difference: the lane verifies every filename candidate, so its
// set is exact and it reports complete even where the content path's per-volume
// window probe would conservatively set ContentIncomplete (a match set larger
// than its completeness window). The result set/order/count still match; only
// the completeness flag is more accurate. Reconcile this before the lane becomes
// the default route.

import (
	"cmp"
	"os"
	"slices"
)

func contentGlobalLaneEnabled() bool {
	return os.Getenv("SEEKFS_CONTENT_GLOBAL_LANE") == "1"
}

// collectContentLaneEntries verifies every candidate against the volume's
// content and orders the result exactly like the per-volume content path
// (content_verify.go:1042-1056): each volume's matches keep their rankForQuery
// order with record-id ties (the stable sort preserves the iterator's local-id
// order), and only when the merged set spans volumes is the shared
// multi-volume comparator applied. collectGlobalVerifiedTopN cannot be reused
// here because its heap compares by a different global rule than its final
// sort, which drops or misorders tied basenames at a limit.
func collectContentLaneEntries(it globalIDIterator, volumes []*serviceVolumeIndex, pq parsedQuery, matcher *contentLeafMatcher) ([]Entry, int, error) {
	perVol := make([][]globalRankedEntry, len(volumes))
	rankers := make([]func(int) int, len(volumes))
	for i, vol := range volumes {
		if vol != nil && vol.index != nil {
			rankers[i] = candidateRanker(vol.index, vol.rankForQuery(pq))
		}
	}
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
		ranked = append(ranked, bucket...)
	}
	if globalRankedEntriesSpanMultipleVolumes(ranked) {
		sortGlobalRankedEntries(ranked, pq)
	}
	out := make([]Entry, len(ranked))
	for i, r := range ranked {
		out[i] = r.entry
	}
	return out, verified, nil
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
	if !snapshot.overlaysOK || globalSnapshotsHaveHidden(snapshot.overlays) || globalSnapshotsHaveOverlayRecords(snapshot.overlays) {
		// A content lane must not under-fill on shadowed records; the content
		// path owns those cases until the lane handles them.
		return nil, nil, false
	}
	// `under:`/`exists:` can stat on search but not on count (M9); the lane's
	// shared verifier does not reproduce that split. A content leaf under an
	// OR/NOT group is not a superset under stripContentLeaves. Biased order
	// (RootBias/CWDBias) is not reproduced by the lane's rank order.
	if pq.Under != "" || pq.Exists || contentLeavesUnderBooleanGroups(pq) {
		return nil, nil, false
	}
	if pq.RootBias != "" || pq.CWDBias != "" {
		return nil, nil, false
	}
	if needOrder && pq.SortColumn == "relevance" {
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
	return usable, skipped, true
}

// globalContentFilenameIterator builds the filename-side candidate iterator for
// a content query's filename part: the ext-only posting source when the part is
// ext-only, otherwise the component source. ok=false means the filename part is
// not a shape the lane supports, so the content path answers.
func globalContentFilenameIterator(volumes []*serviceVolumeIndex, filenamePQ parsedQuery, trace *searchTrace) (globalIDIterator, bool) {
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
	it, ok := globalContentFilenameIterator(usable, stripContentLeaves(pq), opts.Trace)
	if !ok {
		return nil, false, nil
	}
	// Verify the original predicate (filename + content) inline; the matcher is
	// built once and compactCandidateEntryIfMatchIn consumes base and delta
	// content through the volume's reader/resolver. No overlays/hidden here (the
	// scope declined them), so no snapshots are needed.
	matcher := newContentLeafMatcher(pq)
	limit := normalizedLimit(opts.Limit, false)
	base, verified, err := collectContentLaneEntries(it, usable, pq, matcher)
	if err != nil {
		return nil, true, err
	}
	markContentQueryDegraded(opts.Trace, skipped)
	if opts.Trace != nil {
		opts.Trace.ComponentRecordsVerified += verified
		opts.Trace.setPlannerMode("global-content-components")
		opts.Trace.setSource("global:content-components", len(base))
		opts.Trace.setComplete(opts.Trace == nil || !opts.Trace.ContentPartial)
	}
	if limit > 0 && len(base) > limit {
		base = base[:limit]
	}
	return base, true, nil
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
	it, ok := globalContentFilenameIterator(usable, stripContentLeaves(pq), opts.Trace)
	if !ok {
		return 0, false, nil
	}
	matcher := newContentLeafMatcher(pq)
	count, verified, err := countGlobalVerifiedIterator(it, usable, nil, pq, matcher)
	if err != nil {
		return 0, true, err
	}
	markContentQueryDegraded(opts.Trace, skipped)
	if opts.Trace != nil {
		opts.Trace.ComponentRecordsVerified += verified
		opts.Trace.setPlannerMode("global-content-components")
		opts.Trace.setSource("global:content-components-count", count)
		opts.Trace.setComplete(opts.Trace == nil || !opts.Trace.ContentPartial)
	}
	return count, true, nil
}
