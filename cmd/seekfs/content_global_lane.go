package main

// WP7 phase 1: the compound content lane. A query that ANDs a positive content
// leaf with a selective filename selector (ext:/under:/dir:/name:) is answered
// by driving the filename candidate iterator and verifying each candidate's
// content inline, instead of materializing the content candidate superset. The
// two sides are both requirements, so the lane returns exactly the set the
// per-volume content path does; it is a cost optimization, not a semantic
// change.
//
// contentCandidatesBounded already maps matching content docIDs to local record
// IDs, so the content set is in globalRecordID space; the lane instead drives
// the filename side and filters by content. A bare `ext:` selector is the
// ext-only source, anything else with a root is the component source.
//
// Scope (phase 1): single volume, no overlay/hidden records, a content-usable
// volume, and a filename root (ext-only or component). Anything else declines
// (handled=false) and the existing content path answers. Off by default behind
// SEEKFS_CONTENT_GLOBAL_LANE so it is validated against the content path before
// becoming the default route. Multi-volume routing, count parity, and the
// overlay/hidden cases are the next phase.

import "os"

func contentGlobalLaneEnabled() bool {
	return os.Getenv("SEEKFS_CONTENT_GLOBAL_LANE") == "1"
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
// verification. It declines for anything outside the phase-1 scope, leaving the
// per-volume content path to answer.
func searchServiceVolumesGlobalContentComponentsSnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool, pq parsedQuery) ([]Entry, bool, error) {
	if countOnly || !contentGlobalLaneEnabled() {
		return nil, false, nil
	}
	volumes := snapshot.volumes
	if len(volumes) != 1 {
		return nil, false, nil
	}
	if !snapshot.overlaysOK || globalSnapshotsHaveHidden(snapshot.overlays) || globalSnapshotsHaveOverlayRecords(snapshot.overlays) {
		// A content lane must not under-fill on shadowed records; the content
		// path owns those cases until the lane handles them.
		return nil, false, nil
	}
	vol := volumes[0]
	if vol == nil || vol.index == nil || vol.content == nil || !vol.content.usableForQuery() {
		return nil, false, nil
	}
	filenamePQ := stripContentLeaves(pq)
	if filenamePQ.isEmpty() {
		return nil, false, nil
	}
	// The lane drives from the filename projection, which is a complete superset
	// of the result set only when every positive content leaf is a top-level
	// conjunct. Inside an OR/NOT group, stripContentLeaves drops a content-only
	// alternative, so the filename iterator would miss every record that matched
	// only that alternative — decline those shapes to the content path.
	if contentLeavesUnderBooleanGroups(pq) {
		return nil, false, nil
	}
	// The lane verifies and orders by the filename rank; it does not
	// relevance-rank, so a relevance sort must stay on the content path.
	if pq.SortColumn == "relevance" {
		return nil, false, nil
	}
	if err := checkQueryCapabilities(pq, vol.index); err != nil {
		return nil, true, err
	}
	it, ok := globalContentFilenameIterator(volumes, filenamePQ, opts.Trace)
	if !ok {
		return nil, false, nil
	}
	// Verify the original predicate (filename + content) inline; the matcher is
	// built once and compactCandidateEntryIfMatchIn consumes base and delta
	// content through the volume's reader/resolver.
	matcher := newContentLeafMatcher(pq)
	limit := normalizedLimit(opts.Limit, false)
	base, verified, err := collectGlobalVerifiedTopN(it, volumes, snapshot.overlays, pq, limit, matcher)
	if err != nil {
		return nil, true, err
	}
	if opts.Trace != nil {
		opts.Trace.ComponentRecordsVerified += verified
		opts.Trace.setPlannerMode("global-content-components")
		opts.Trace.setSource("global:content-components", len(base))
		opts.Trace.setComplete(true)
	}
	results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshot.overlays, base, pq, limit))
	return results, true, nil
}
