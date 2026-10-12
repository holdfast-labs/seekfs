package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

func isVolumeRoot(path string) bool {
	clean := normalizeFilterPath(path)
	vol := filepath.VolumeName(clean)
	if vol == "" {
		return false
	}
	rest := strings.TrimPrefix(clean, vol)
	return rest == `\` || rest == `/` || rest == ""
}

func countServiceVolumes(volumes []*serviceVolumeIndex, opts queryOptions) (int, bool, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	markCountDivergent(opts, pq)
	if pq.Impossible {
		opts.Trace.setPlannerMode("impossible-query")
		opts.Trace.setSource("impossible-query", 0)
		return 0, true, nil
	}
	volumes, err = serviceVolumesForQuery(volumes, opts)
	if err != nil {
		return 0, true, err
	}
	if len(volumes) == 0 {
		opts.Trace.setEligibleVolumes(volumes)
		opts.Trace.setPlannerMode("volume-empty")
		opts.Trace.setSource("volume-empty", 0)
		return 0, true, nil
	}
	volumes = prioritizeServiceVolumesForPathTerms(volumes, opts)
	opts.Trace.setEligibleVolumes(volumes)
	if queryHasAnyContentLeaf(pq) {
		if contentGlobalLaneEnabled() && queryHasPositiveContentLeaf(pq) {
			snapshot := newGlobalQuerySnapshot(volumes, opts.Trace)
			if n, handled, err := countServiceVolumesGlobalContentComponentsSnapshot(snapshot, opts, pq); handled {
				return n, true, err
			}
		}
		// Content counts run the exact same per-volume candidate + inline
		// verification path as content searches so count == len(search results)
		// for every shape that does not require a stat. Unusable volumes degrade
		// the result; only an all-unusable query is refused. A query still
		// answerable without content also counts the unusable volume's filename
		// matches (PF-7b).
		usable, skipped := contentUsableVolumes(volumes, pq)
		if len(usable) == 0 && !filenameAnswerable(pq) {
			return 0, true, contentUnavailableError()
		}
		markContentQueryDegraded(opts.Trace, skipped)
		markContentQueryIncomplete(opts.Trace, usable)
		total := 0
		for _, vol := range volumes {
			if queryCanceled(parsedQuery{DeadlineUnix: opts.DeadlineUnix, Cancel: opts.Cancel}) {
				return 0, true, errQueryCanceled
			}
			var n int
			var err error
			if vol != nil && vol.contentUsableForQuery() {
				n, err = vol.countContentVolume(opts)
			} else if filenameAnswerable(pq) && vol != nil {
				n, err = vol.countFilenameOnlyVolume(opts, pq)
			} else {
				continue
			}
			if err != nil {
				return 0, true, err
			}
			total += n
		}
		// A capped candidate superset means the count is not exact. Refuse it
		// instead of returning a partial number that looks complete.
		if opts.Trace != nil && opts.Trace.ContentIncomplete {
			return 0, true, errContentIncomplete
		}
		opts.Trace.setPlannerMode("service-content")
		opts.Trace.setComplete(opts.Trace == nil || (!opts.Trace.ContentPartial && !opts.Trace.ContentIncomplete))
		return total, true, nil
	}
	snapshot := newGlobalQuerySnapshot(volumes, opts.Trace)
	if count, handled, err := countServiceVolumesGlobalOnlySnapshot(snapshot, opts); handled {
		if count == 0 && opts.Trace != nil && opts.Trace.PlannerMode == "global-count-name" {
			opts.Trace.setSource("exact-empty", 0)
		}
		return count, true, err
	}
	if count, handled, err := countServiceVolumesGlobalNameSnapshot(snapshot, opts); handled {
		if count == 0 && opts.Trace != nil && opts.Trace.PlannerMode == "global-count-name" {
			opts.Trace.setSource("exact-empty", 0)
		}
		return count, true, err
	}
	if count, handled, err := countServiceVolumesGlobalScalarSnapshot(snapshot, opts); handled {
		return count, true, err
	}
	if count, handled, err := countServiceVolumesGlobalBoundedFallbackSnapshot(snapshot, opts); handled {
		return count, true, err
	}
	if len(volumes) > 1 {
		return 0, true, globalMultiVolumePlannerDeclineError(opts, volumes)
	}
	if opts.Trace != nil && strings.HasPrefix(opts.Trace.Decline, "global-") {
		opts.Trace.setFallback("service-count-single-volume")
	}
	opts.Trace.setPlannerMode("service-count-single-volume")
	pq, err = parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	vol := volumes[0]
	dropSatisfiedVolumeTerms(&pq, vol.index.Volume)
	if err := checkQueryCapabilities(pq, vol.index); err != nil {
		return 0, true, err
	}
	if vol.hasActiveOverlay() {
		if count, ok := vol.overlayAwareFastCount(pq); ok {
			return count, true, nil
		}
		pathCache := make(map[int]string)
		matches, err := searchCompactWithCacheHidden(vol.index, opts, true, pathCache, vol.nameTermCandidates, vol.snapshotHiddenBaseIDs(), nil)
		if err != nil {
			return 0, true, err
		}
		matches = vol.mergeOverlayMatches(matches, opts, true, pathCache)
		return len(matches), true, nil
	}
	count, ok := vol.fastPostingCount(pq)
	if !ok {
		return 0, false, nil
	}
	return count, true, nil
}

// overlayAwareFastCount is the review-G7 / plan-R2.6 sanctioned stopgap: it
// answers a count query exactly while a v9 overlay is active, without
// falling back to the full search+merge path. It is
//
//	(base fast posting count, filtered against tombstoned/shadowed base ids)
//	+ (linear count of live overlay records matching pq)
//
// It reads the volume's snapshot exactly once and only touches snapshot
// slices (records[:watermark], tombstoneIDs, shadowedIDs) Ã¢â‚¬â€ never
// vol.overlay's live maps, which the apply goroutine mutates concurrently
// (review G6). If the base fast-count route cannot evaluate pq (same decline
// conditions as fastPostingCount today), this declines too (ok=false) rather
// than guess, per the R2.6 invariant: any route that can't see the overlay
// exactly must decline, not answer stale/wrong.
func (vol *serviceVolumeIndex) overlayAwareFastCount(pq parsedQuery) (int, bool) {
	if vol == nil || vol.index == nil || queryHasAnyContentLeaf(pq) {
		return 0, false
	}
	snap := vol.snap.Load()
	if snap == nil {
		// No snapshot published yet even though hasActiveOverlay() said the
		// overlay was active (e.g. legacy overlay.watermark path without a
		// published snapshot) -- decline rather than risk reading the live
		// overlay maps outside the snapshot.
		return 0, false
	}
	hidden := hiddenBaseIDs{tombstone: snap.tombstoneIDs, shadowed: snap.shadowedIDs}
	baseCount, ok := vol.fastPostingCountHidden(pq, hidden)
	if !ok {
		return 0, false
	}
	overlayCount := vol.overlayLiveMatchCount(snap, pq)
	return baseCount + overlayCount, true
}

func (vol *serviceVolumeIndex) hasActiveOverlay() bool {
	if vol == nil {
		return false
	}
	if snap := vol.snap.Load(); snap != nil {
		return snap.watermark > 0
	}
	return vol.overlay != nil && vol.overlay.watermark.Load() > 0
}

func serviceVolumesForQuery(volumes []*serviceVolumeIndex, opts queryOptions) ([]*serviceVolumeIndex, error) {
	if len(volumes) == 0 {
		return nil, errors.New("service has no search indexes loaded")
	}
	wantVolume := queryVolumeConstraint(opts)
	ready := make([]*serviceVolumeIndex, 0, len(volumes))
	stale := make([]string, 0, len(volumes))
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			continue
		}
		if wantVolume != "" && !serviceVolumeMatchesConstraint(vol, wantVolume) {
			continue
		}
		if vol.state != "" && vol.state != "ready" {
			stale = append(stale, fmt.Sprintf("%s: %s", vol.index.Volume, vol.staleReason))
		}
		ready = append(ready, vol)
	}
	if len(ready) > 0 {
		return ready, nil
	}
	if len(stale) > 0 {
		return nil, fmt.Errorf("matching search index is stale: %s", strings.Join(stale, "; "))
	}
	if wantVolume != "" {
		// An explicit volume anchor that is absent from this snapshot is an
		// exact empty federated scope. Return an empty eligible set so the
		// caller can terminate before planner-family routing, posting decode,
		// record verification, or filesystem fallback.
		return []*serviceVolumeIndex{}, nil
	}
	return nil, errors.New("service has no ready search indexes loaded")
}

func serviceVolumeMatchesConstraint(vol *serviceVolumeIndex, wantVolume string) bool {
	if vol == nil || vol.index == nil || wantVolume == "" {
		return true
	}
	if vol.index.Volume != "" {
		return strings.EqualFold(vol.index.Volume, wantVolume)
	}
	for _, root := range vol.index.Roots {
		if strings.EqualFold(filepath.VolumeName(filepath.Clean(root)), wantVolume) {
			return true
		}
	}
	return false
}

func queryVolumeConstraint(opts queryOptions) string {
	if underVolume := strings.ToUpper(filepath.VolumeName(filepath.Clean(opts.Under))); underVolume != "" {
		return underVolume
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return ""
	}
	volume := ""
	for _, term := range pq.Terms {
		if !isVolumeQueryTerm(term) {
			continue
		}
		normalized := strings.ToUpper(term)
		if volume == "" {
			volume = normalized
			continue
		}
		if !strings.EqualFold(volume, normalized) {
			return ""
		}
	}
	return volume
}

func (vol *serviceVolumeIndex) fastPostingCount(pq parsedQuery) (int, bool) {
	return vol.fastPostingCountHidden(pq, hiddenBaseIDs{})
}

// fastPostingCountHidden is fastPostingCount plus an id-level exclusion set
// for base records tombstoned/shadowed by the active v9 overlay. Every
// candidate-id loop below checks hidden before counting, so the result stays
// exact while an overlay is active instead of being a stale base-only count
// (review G7 / plan R2.6).
func (vol *serviceVolumeIndex) fastPostingCountHidden(pq parsedQuery, hidden hiddenBaseIDs) (int, bool) {
	if queryHasAnyContentLeaf(pq) {
		// These posting counts never evaluate content; decline so the verified
		// content path answers.
		return 0, false
	}
	if count, ok := vol.plannedCountHidden(pq, hidden); ok {
		return count, true
	}
	if count, ok := vol.fastBareExtensionPathCountHidden(pq, hidden); ok {
		return count, true
	}
	globExts, globsOK := simpleGlobExts(pq.Globs)
	if vol == nil || vol.index == nil || vol.queryIndex == nil || pq.CaseSensitive || pq.Under != "" || pq.Exists || pq.HasModAfter || len(pq.AttrFilters) > 0 || len(pq.Regexps) > 0 || !globsOK || len(pq.Dirs) > 0 || len(pq.Parents) > 0 {
		return 0, false
	}
	lists := make([]postingCountCandidate, 0, len(pq.Exts)+len(globExts)+len(pq.Terms)+1)
	for _, ext := range pq.Exts {
		list, ok := vol.extPostingCountCandidate(ext)
		if !ok || list.len() == 0 {
			return vol.countRecentOnlyHidden(pq, hidden), true
		}
		lists = append(lists, list)
	}
	for _, ext := range globExts {
		list, ok := vol.extPostingCountCandidate(ext)
		if !ok || list.len() == 0 {
			return vol.countRecentOnlyHidden(pq, hidden), true
		}
		lists = append(lists, list)
	}
	switch pq.Type {
	case "":
	case "file":
		if len(lists) == 0 {
			return 0, false
		}
	case "dir":
		lists = append(lists, postingCountCandidate{ids: vol.queryIndex.dirs})
	default:
		return 0, false
	}
	for _, term := range pq.Terms {
		if !pq.MatchPath || !isVolumeQueryTerm(term) {
			return 0, false
		}
		if !strings.EqualFold(term, vol.volume) {
			return vol.countRecentOnlyHidden(pq, hidden), true
		}
	}
	if len(lists) == 0 {
		return 0, false
	}
	sortPostingCountCandidatesByLen(lists)
	candidates := lists[0].materialize()
	for _, list := range lists[1:] {
		if list.mapped {
			candidates = intersectSortedUint32sWithPostingIterator(candidates, list.it)
		} else {
			candidates = intersectSortedUint32s(candidates, list.ids)
		}
		if len(candidates) == 0 {
			break
		}
	}
	recent := vol.recentIDs
	count := 0
	for _, id := range candidates {
		if _, ok := recent[int(id)]; ok {
			continue
		}
		if !hidden.empty() && hidden.contains(int(id)) {
			continue
		}
		count++
	}
	for id := range recent {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
			count++
		}
	}
	return count, true
}

func (vol *serviceVolumeIndex) fastBareExtensionPathCountHidden(pq parsedQuery, hidden hiddenBaseIDs) (int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CaseSensitive ||
		pq.Under != "" || pq.Exists || pq.HasModAfter ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.Parents) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		pq.Type == "dir" || countNonVolumeTerms(pq.Terms) < 2 {
		return 0, false
	}
	hasAnchor := false
	var bestExt string
	var best postingCountCandidate
	bestSet := false
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		if strings.ContainsAny(term, `\/*?[]:`) {
			return 0, false
		}
		if ext, ok := pathExtensionCandidateTerm(term); ok && vol.pathTermIsUsableExtensionCandidate(term) {
			candidate, ok := vol.extPostingCountCandidate(ext)
			if !ok {
				continue
			}
			if candidate.len() == 0 {
				return vol.countRecentOnlyHidden(pq, hidden), true
			}
			if candidate.len() > serviceComponentMultiTermScanMaxIDs {
				continue
			}
			if !bestSet || candidate.len() < best.len() {
				bestExt = ext
				best = candidate
				bestSet = true
			}
			continue
		}
		if len(term) >= 4 {
			hasAnchor = true
		}
	}
	if !hasAnchor || !bestSet {
		return 0, false
	}
	matchesExt := func(rec CompactRecord) bool {
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		return strings.EqualFold(actual, bestExt)
	}
	pathCache := make(map[int]string)
	recent := vol.recentIDs
	count := 0
	for _, id32 := range best.materialize() {
		id := int(id32)
		if _, ok := recent[id]; ok {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !matchesExt(rec) {
			continue
		}
		if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, pathCache, true, false); ok {
			count++
		}
	}
	for id := range recent {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !matchesExt(rec) {
			continue
		}
		if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, pathCache, true, false); ok {
			count++
		}
	}
	return count, true
}

type postingCountCandidate struct {
	ids    []uint32
	it     postingBlockIterator
	count  int
	mapped bool
}

func (candidate postingCountCandidate) len() int {
	if candidate.mapped {
		return candidate.count
	}
	return len(candidate.ids)
}

func (candidate postingCountCandidate) materialize() []uint32 {
	if candidate.mapped {
		return materializePostingBlockIterator(candidate.it, candidate.count)
	}
	return append([]uint32(nil), candidate.ids...)
}

func isVolumeQueryTerm(term string) bool {
	if len(term) != 2 || term[1] != ':' {
		return false
	}
	c := term[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func (vol *serviceVolumeIndex) countRecentOnly(pq parsedQuery) int {
	return vol.countRecentOnlyHidden(pq, hiddenBaseIDs{})
}

func (vol *serviceVolumeIndex) countRecentOnlyHidden(pq parsedQuery, hidden hiddenBaseIDs) int {
	count := 0
	for id := range vol.recentIDs {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
			count++
		}
	}
	return count
}

func searchCompact(idx *Index, opts queryOptions, countOnly bool) ([]Entry, error) {
	return searchCompactWithCache(idx, opts, countOnly, nil, nil)
}
