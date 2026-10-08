package main

import (
	"os"
	"sort"
	"strings"
	"time"
)

func searchServiceVolumesGlobalExtOnly(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	return searchServiceVolumesGlobalExtOnlySnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts, countOnly)
}

func searchServiceVolumesGlobalExtOnlySnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	volumes := snapshot.volumes
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, true, err
	}
	globalEnabled := globalPlannerEnabled()
	if !globalEnabled && !globalExtDefaultSupported(pq) {
		return nil, false, nil
	}
	if !globalExtOnlySupported(pq) {
		if globalEnabled {
			opts.Trace.replaceDecline("global-ext:unsupported-query")
		}
		return nil, false, nil
	}
	extFilters, _ := globalExtPostingFilters(pq)
	extFilter := extFilters[0]
	limit := normalizedLimit(opts.Limit, false)
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-ext:overlay-snapshot-missing")
		return nil, false, nil
	}
	if countOnly {
		ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
		if !ok {
			return nil, false, nil
		}
		ids = filterGlobalIDsHidden(ids, snapshots)
		ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
		if err != nil {
			return nil, true, err
		}
		results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, 0))
		opts.Trace.setPlannerMode("global-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: len(ids),
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, len(ids))
		opts.Trace.setComplete(true)
		return results, true, nil
	}
	if pq.SortColumn != "" {
		ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
		if !ok {
			return nil, false, nil
		}
		ids = filterGlobalIDsHidden(ids, snapshots)
		ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
		if err != nil {
			return nil, true, err
		}
		results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
		opts.Trace.setPlannerMode("global-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: len(ids),
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, len(ids))
		opts.Trace.setComplete(true)
		return results, true, nil
	}
	if !globalVolumesHaveRankForQuery(volumes, pq) {
		if globalSnapshotsHaveOverlayRecords(snapshots) {
			opts.Trace.replaceDecline("global-ext:rankless-overlay")
			return nil, false, nil
		}
		ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
		if !ok {
			return nil, false, nil
		}
		ids = filterGlobalIDsHidden(ids, snapshots)
		results, err := entriesFromGlobalIDs(volumes, ids, pq)
		if err != nil {
			return nil, true, err
		}
		sortSearchAllEntries(results, pq)
		if limit > 0 && len(results) > limit {
			results = results[:limit]
		}
		opts.Trace.setPlannerMode("global-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: len(ids),
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, len(ids))
		opts.Trace.setComplete(true)
		return results, true, nil
	}

	ids := make([]globalRecordID, 0, limit*len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.addDeclineForVolume("global-ext:missing-volume", "")
			return nil, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return nil, true, err
		}
		posting, ok := vol.extPostingCountCandidate(extFilter.ext)
		if !ok {
			opts.Trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
			return nil, false, nil
		}
		if !globalSnapshotsHaveHidden(snapshots) {
			if localIDs, ok := vol.extTopPosting(extFilter.ext, limit, pq); ok {
				for _, local := range localIDs {
					ids = append(ids, globalRecordID{volume: volumeIndex, local: local})
				}
				continue
			}
		}
		it := newGlobalPostingIteratorWithTrace(volumeIndex, posting, opts.Trace)
		var source globalIDIterator = &it
		if globalSnapshotsHaveHidden(snapshots) {
			hiddenIt := newGlobalHiddenIterator(&it, snapshots)
			source = &hiddenIt
		}
		if pq.Type == "file" || pq.Type == "dir" {
			// The rank-truncated top-N below must be drawn from
			// type-matching records only; truncating the unfiltered
			// posting first could discard every match of the wanted
			// type (e.g. the rare dirs among a common extension).
			typeIt := &globalTypeFilterIterator{base: source, volumes: volumes, typ: pq.Type}
			source = typeIt
		}
		rankOf := candidateRanker(vol.index, vol.rankForQuery(pq))
		ids = append(ids, collectGlobalTopN([]globalIDIterator{source}, limit, func(id globalRecordID) int {
			return rankOf(id.local)
		})...)
	}
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
	if err != nil {
		return nil, true, err
	}
	results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
	opts.Trace.setPlannerMode("global-ext")
	opts.Trace.addTerm(traceTerm{
		Term:      extFilter.ext,
		Kind:      "extension",
		Source:    "global:" + extFilter.source,
		CountHint: len(ids),
		Exact:     true,
	})
	opts.Trace.setSource("global:"+extFilter.source, len(ids))
	opts.Trace.setComplete(true)
	return results, true, nil
}

func searchServiceVolumesGlobalComponentsOnly(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	return searchServiceVolumesGlobalComponentsOnlySnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts, countOnly)
}

func searchServiceVolumesGlobalComponentsOnlySnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	volumes := snapshot.volumes
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, true, err
	}
	terms := nonVolumeTerms(pq.Terms)
	globalEnabled := globalPlannerEnabled()
	if !globalEnabled && !globalComponentDefaultSupported(pq, terms) {
		return nil, false, nil
	}
	if !globalComponentQuerySupportedMulti(pq, terms, len(volumes) > 1) {
		opts.Trace.replaceDecline("global-components:unsupported-query")
		return nil, false, nil
	}
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-components:overlay-snapshot-missing")
		return nil, false, nil
	}
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-components:missing-volume")
			return nil, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return nil, true, err
		}
	}
	limit := normalizedLimit(opts.Limit, false)
	if countOnly {
		limit = 0
	}
	if !countOnly && (len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0) {
		if ids, ok := globalSimplePathORTopIDs(volumes, pq, limit); ok && !globalSnapshotsHaveHidden(snapshots) && !globalSnapshotsHaveOverlayRecords(snapshots) {
			topIt := newGlobalIDSliceIterator(ids)
			base, verified, err := collectGlobalVerifiedTopN(&topIt, volumes, snapshots, pq, limit)
			if err != nil {
				return nil, true, err
			}
			if opts.Trace != nil {
				opts.Trace.ComponentRecordsVerified += verified
				opts.Trace.setPlannerMode("global-components")
				opts.Trace.setSource("global:boolean-persisted-top", len(ids))
				opts.Trace.setComplete(true)
			}
			return globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, base, pq, limit)), true, nil
		}
		componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
		if !ok {
			if opts.Trace == nil || opts.Trace.Decline == "" {
				opts.Trace.replaceDecline("global-components:boolean-missing-source")
			}
			return nil, false, nil
		}
		base, verified, err := collectGlobalVerifiedTopN(componentIt, volumes, snapshots, pq, limit)
		if err != nil {
			return nil, true, err
		}
		if opts.Trace != nil {
			opts.Trace.ComponentRecordsVerified += verified
			opts.Trace.setPlannerMode("global-components")
			opts.Trace.setSource("global:boolean-iterator", len(base))
			opts.Trace.setComplete(true)
		}
		results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, base, pq, limit))
		return results, true, nil
	}
	var ids []globalRecordID
	topUsed := false
	if !countOnly && !globalSnapshotsHaveHidden(snapshots) {
		if topIDs, ok := globalComponentTopIDs(volumes, pq, limit); ok {
			ids = topIDs
			topUsed = true
		}
	}
	if !topUsed {
		componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
		if !ok {
			if opts.Trace == nil || opts.Trace.Decline == "" {
				opts.Trace.replaceDecline("global-components:missing-source")
			}
			return nil, false, nil
		}
		if !countOnly && limit > 0 {
			// Stream the iterator and verify on the fly, keeping only the
			// top-N in a bounded heap.  The old path materialized every
			// candidate id (potentially millions, e.g. a broad regex-literal
			// query) and then re-verified them in a second pass.  Streaming
			// keeps memory O(limit) and runs one verification pass, which is
			// the difference between 28s and tens of ms for regex-literal
			// queries.
			base, verified, err := collectGlobalVerifiedTopN(componentIt, volumes, snapshots, pq, limit)
			if err != nil {
				return nil, true, err
			}
			if opts.Trace != nil {
				opts.Trace.ComponentRecordsVerified += verified
			}
			results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, base, pq, limit))
			opts.Trace.setPlannerMode("global-components")
			addGlobalComponentTraceTerms(opts.Trace, pq, len(base))
			opts.Trace.setSource("global:components", len(base))
			opts.Trace.setComplete(true)
			return results, true, nil
		}
		canceled := false
		if globalSnapshotsHaveHidden(snapshots) {
			hidden := newGlobalHiddenIterator(componentIt, snapshots)
			ids, canceled = collectGlobalIteratorCancelable(&hidden, 0, func() bool { return queryCanceled(pq) })
		} else {
			ids, canceled = collectGlobalIteratorCancelable(componentIt, 0, func() bool { return queryCanceled(pq) })
		}
		if canceled {
			return nil, true, errQueryCanceled
		}
	}
	ids = filterGlobalIDsByType(volumes, ids, pq.Type)
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
	if err != nil {
		return nil, true, err
	}
	results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
	opts.Trace.setPlannerMode("global-components")
	addGlobalComponentTraceTerms(opts.Trace, pq, len(ids))
	if topUsed {
		opts.Trace.setSource("global:component-top", len(ids))
	} else {
		opts.Trace.setSource("global:components", len(ids))
	}
	opts.Trace.setComplete(true)
	return results, true, nil
}

func searchServiceVolumesGlobalBoundedFallback(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	return searchServiceVolumesGlobalBoundedFallbackSnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts, countOnly)
}

func searchServiceVolumesGlobalBoundedFallbackSnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	volumes := snapshot.volumes
	if len(volumes) < 2 {
		return nil, false, nil
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, true, err
	}
	terms := nonVolumeTerms(pq.Terms)
	if !globalPlannerEnabled() && !globalExtDefaultSupported(pq) && !globalComponentDefaultSupported(pq, terms) && !globalBoundedFallbackDefaultSupported(pq) {
		return nil, false, nil
	}
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-bounded-scan:overlay-snapshot-missing")
		return nil, false, nil
	}
	if !globalBoundedScanBudgetOK(volumes, pq, 3) {
		opts.Trace.replaceDecline("global-bounded-scan:budget")
		return nil, false, nil
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	if countOnly {
		limit = 0
	}
	ids := make([]globalRecordID, 0, 1024)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-bounded-scan:missing-volume")
			return nil, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return nil, true, err
		}
		volumePQ := pq
		volumePQ.Limit = limit
		volumePQ.CountOnly = countOnly
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		var localIDs []int
		var ok bool
		orderReady := len(vol.orderForQuery(volumePQ)) >= vol.index.compactRecordCount()
		if countOnly || !orderReady || volumePQ.RootBias != "" || volumePQ.CWDBias != "" {
			volumePQ.Limit = 0
			volumePQ.CountOnly = true
			var filter *boundedScanMembershipFilter
			exactEmpty, filterOK := vol.boundedScanPrefilter(volumePQ, &filter)
			if exactEmpty {
				localIDs = []int{}
				ok = true
			} else if filterOK {
				localIDs, ok = vol.boundedScanCandidatesFiltered(volumePQ, filter)
			} else {
				localIDs, ok = vol.boundedScanCandidates(volumePQ)
			}
		} else {
			hidden := hiddenBaseIDs{}
			if volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
				hidden = hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
			}
			// Pre-filter the name/id-order scan with a cheap exact posting
			// (ext:, glob-ext:, a bounded type:dir subtree, or a required regex
			// literal run) when present, so broad queries like "test ext:py",
			// "type:dir docs", or "regex:README\.(md|txt)$" skip non-matching
			// records instead of verifying every record.  The scan order is
			// unchanged, preserving the bounded scan's top-N semantics.
			var filter *boundedScanMembershipFilter
			exactEmpty, filterOK := vol.boundedScanPrefilter(volumePQ, &filter)
			if !exactEmpty && filterOK {
				localIDs, ok = vol.boundedScanCandidatesHiddenTopFiltered(volumePQ, hidden, limit, filter)
			} else if exactEmpty {
				localIDs = []int{}
				ok = true
			} else {
				localIDs, ok = vol.boundedScanCandidatesHiddenTop(volumePQ, hidden, limit)
			}
		}
		if !ok {
			opts.Trace.replaceDecline("global-bounded-scan:canceled")
			return nil, false, nil
		}
		for _, local := range localIDs {
			ids = append(ids, globalRecordID{volume: volumeIndex, local: local})
		}
	}
	ids = filterGlobalIDsHidden(ids, snapshots)
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq)
	if err != nil {
		return nil, true, err
	}
	results := globalRankedEntriesToEntries(mergeGlobalOverlayEntries(volumes, snapshots, ranked, pq, limit))
	opts.Trace.setFallback("global-bounded-scan")
	opts.Trace.setPlannerMode("global-bounded-scan")
	opts.Trace.setSource("global:bounded-scan", len(ids))
	opts.Trace.setComplete(true)
	return results, true, nil
}

func countServiceVolumesGlobalOnly(volumes []*serviceVolumeIndex, opts queryOptions) (int, bool, error) {
	return countServiceVolumesGlobalOnlySnapshot(newGlobalQuerySnapshot(volumes, opts.Trace), opts)
}

// globalBoundedScanBudgetOK reports whether the remaining query deadline can
// plausibly cover a full per-volume record scan.  A full scan is linear in the
// compact record count (roughly 30-140ms per million records measured on a
// 26.8M-record two-volume service: search-path candidate scans near 30ms/M,
// count-path verification near 140ms/M), so a pessimistic 100ms/M is a safe
// base.  Starting a scan when the remaining budget is too tight would only
// block until the deadline and then cancel, so the margin keeps the check
// generous while still avoiding clearly-doomed scans.  The scan itself remains
// deadline-cancellable, so a slightly optimistic estimate only costs waiting
// until the deadline rather than a wrong answer.
func globalBoundedScanBudgetOK(volumes []*serviceVolumeIndex, pq parsedQuery, margin float64) bool {
	if pq.DeadlineUnix <= 0 {
		return true
	}
	remaining := time.Until(time.Unix(0, pq.DeadlineUnix))
	if remaining <= 0 {
		return false
	}
	var records int64
	for _, vol := range volumes {
		if vol != nil && vol.index != nil {
			records += int64(vol.index.compactRecordCount())
		}
	}
	estimatedMS := float64(records) / 1e6 * 100
	return float64(remaining.Milliseconds()) >= estimatedMS*margin
}

// globalTypeTermCountShape reports whether pq is a bare `type:<typ> <term>`
// query: exactly one non-volume term and no other filter.  These shapes used
// to force the global bounded fallback into a full per-volume record scan.
func globalTypeTermCountShape(pq parsedQuery, typ string) (string, bool) {
	if pq.Type != typ {
		return "", false
	}
	terms := nonVolumeTerms(pq.Terms)
	if len(terms) != 1 || len(pq.Exts) != 0 || len(pq.Dirs) != 0 || len(pq.Globs) != 0 ||
		len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 || len(pq.Parents) != 0 ||
		pq.Under != "" || pq.Exists || pq.HasModAfter || len(pq.SizeFilters) != 0 ||
		len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 || len(pq.OrGroups) != 0 ||
		len(pq.NotGroups) != 0 || pq.CaseSensitive || pq.RootBias != "" || pq.CWDBias != "" {
		return "", false
	}
	return terms[0], true
}

// globalDirTermCountShape reports whether pq is a bare `type:dir <term>`
// query routed through the capped dir-posting count.
func globalDirTermCountShape(pq parsedQuery) (string, bool) {
	return globalTypeTermCountShape(pq, "dir")
}

// globalFileTermCountShape reports whether pq is a bare `type:file <term>`
// query routed through the capped term-posting count.
func globalFileTermCountShape(pq parsedQuery) (string, bool) {
	return globalTypeTermCountShape(pq, "file")
}

// boundedDirTermPosting returns the term's posting capped to
// serviceComponentMultiTermScanMaxIDs.  The path form reuses the ext-filter
// bounded builder (name matches plus capped descendant expansion); the name
// form uses the trigram-limited name posting, falling back to the scanned name
// posting only when its size is known to be within the cap.
func (vol *serviceVolumeIndex) boundedDirTermPosting(term string, matchPath bool) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	if matchPath {
		return vol.pathTermPostingForExtFilter(term, serviceComponentMultiTermScanMaxIDs)
	}
	if ids, ok := vol.completeNameTrigramNameTermPostingLimited(term, serviceComponentMultiTermScanMaxIDs); ok {
		return ids, true
	}
	ids := vol.nameTermPosting(term)
	if len(ids) > serviceComponentMultiTermScanMaxIDs {
		return nil, false
	}
	return ids, true
}

// countDirTermLive is an exact, bounded count for `type:dir <term>`.  The
// resident dir list is the type:dir posting; the term's bounded name/path
// posting drives an intersection, so a selective term never scans the whole
// volume.  Live recentIDs are reconciled the same way extTopPosting does, so
// the count stays complete while the legacy engine is live-updating base
// records in place.
func (vol *serviceVolumeIndex) countDirTermLive(term string, matchPath bool, hidden hiddenBaseIDs) (int, bool) {
	if vol == nil || vol.index == nil || vol.queryIndex == nil || !vol.queryIndex.dirsReady {
		return 0, false
	}
	dirs := vol.queryIndex.dirs
	ids, ok := vol.boundedDirTermPosting(term, matchPath)
	if !ok || len(ids) > serviceComponentMultiTermScanMaxIDs {
		return 0, false
	}
	var seen map[int]struct{}
	if len(vol.recentIDs) > 0 {
		seen = make(map[int]struct{}, len(ids))
	}
	count := 0
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() || !hidden.empty() && hidden.contains(id) {
			continue
		}
		pos := sort.Search(len(dirs), func(i int) bool { return dirs[i] >= uint32(id) })
		if pos >= len(dirs) || dirs[pos] != uint32(id) {
			continue
		}
		count++
		if seen != nil {
			seen[id] = struct{}{}
		}
	}
	for id := range vol.recentIDs {
		if id < 0 || id >= vol.index.compactRecordCount() || !hidden.empty() && hidden.contains(id) {
			continue
		}
		if seen != nil {
			if _, exists := seen[id]; exists {
				continue
			}
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if matchPath {
			if vol.index.compactPathContainsTerm(id, term) {
				count++
			}
		} else if strings.Contains(vol.index.compactLowerNameAt(id), term) {
			count++
		}
	}
	return count, true
}

// countFileTermLive is the type:file mirror of countDirTermLive.  The term's
// bounded name/path posting drives the intersection; each candidate is kept
// only when its record is not a directory.  There is no persisted file posting
// (a complement of dirs would be the full volume), so the predicate is applied
// per candidate exactly as the verified path does, keeping count/search parity.
func (vol *serviceVolumeIndex) countFileTermLive(term string, matchPath bool, hidden hiddenBaseIDs) (int, bool) {
	if vol == nil || vol.index == nil {
		return 0, false
	}
	ids, ok := vol.boundedDirTermPosting(term, matchPath)
	if !ok || len(ids) > serviceComponentMultiTermScanMaxIDs {
		return 0, false
	}
	var seen map[int]struct{}
	if len(vol.recentIDs) > 0 {
		seen = make(map[int]struct{}, len(ids))
	}
	count := 0
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() || !hidden.empty() && hidden.contains(id) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 {
			continue
		}
		count++
		if seen != nil {
			seen[id] = struct{}{}
		}
	}
	for id := range vol.recentIDs {
		if id < 0 || id >= vol.index.compactRecordCount() || !hidden.empty() && hidden.contains(id) {
			continue
		}
		if seen != nil {
			if _, exists := seen[id]; exists {
				continue
			}
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 {
			continue
		}
		if matchPath {
			if vol.index.compactPathContainsTerm(id, term) {
				count++
			}
		} else if strings.Contains(vol.index.compactLowerNameAt(id), term) {
			count++
		}
	}
	return count, true
}

func countServiceVolumesGlobalBoundedFallbackSnapshot(snapshot globalQuerySnapshot, opts queryOptions) (int, bool, error) {
	if len(snapshot.volumes) < 2 {
		return 0, false, nil
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	terms := nonVolumeTerms(pq.Terms)
	if !globalPlannerEnabled() && !globalExtDefaultSupported(pq) && !globalComponentDefaultSupported(pq, terms) && !globalBoundedFallbackDefaultSupported(pq) {
		return 0, false, nil
	}
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-bounded-scan:overlay-snapshot-missing")
		return 0, false, nil
	}
	if !globalBoundedScanBudgetOK(snapshot.volumes, pq, 3) {
		opts.Trace.replaceDecline("global-bounded-scan:budget")
		return 0, false, nil
	}
	total := 0
	for volumeIndex, vol := range snapshot.volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-bounded-scan:missing-volume")
			return 0, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return 0, true, err
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		hidden := hiddenBaseIDs{}
		if volumeIndex < len(snapshot.overlays) && snapshot.overlays[volumeIndex] != nil {
			snap := snapshot.overlays[volumeIndex]
			hidden = hiddenBaseIDs{tombstone: snap.tombstoneIDs, shadowed: snap.shadowedIDs}
		}
		if term, ok := globalDirTermCountShape(volumePQ); ok {
			if count, ok := vol.countDirTermLive(term, volumePQ.MatchPath, hidden); ok {
				total += count
				continue
			}
		}
		if term, ok := globalFileTermCountShape(volumePQ); ok {
			if count, ok := vol.countFileTermLive(term, volumePQ.MatchPath, hidden); ok {
				total += count
				continue
			}
		}
		cache := make(map[int]string)
		for id := 0; id < vol.index.compactRecordCount(); id++ {
			if id&1023 == 0 && queryCanceled(pq) {
				return 0, true, errQueryCanceled
			}
			if !hidden.empty() && hidden.contains(id) {
				continue
			}
			if _, ok := compactCandidateEntryIfMatch(vol.index, volumePQ, id, cache, true, false); ok {
				total++
			}
		}
	}
	total += globalOverlayMatchCount(snapshot.volumes, snapshot.overlays, pq)
	opts.Trace.setFallback("global-bounded-scan")
	opts.Trace.setPlannerMode("global-bounded-scan")
	opts.Trace.setSource("global:bounded-scan", total)
	opts.Trace.setComplete(true)
	return total, true, nil
}

func countServiceVolumesGlobalOnlySnapshot(snapshot globalQuerySnapshot, opts queryOptions) (int, bool, error) {
	volumes := snapshot.volumes
	pq, err := parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	globalEnabled := globalPlannerEnabled()
	extOnlySupported := globalExtOnlySupported(pq)
	terms := nonVolumeTerms(pq.Terms)
	if !globalEnabled && !globalExtDefaultSupported(pq) && !globalComponentDefaultSupported(pq, terms) {
		return 0, false, nil
	}
	snapshots := snapshot.overlays
	if !snapshot.overlaysOK {
		opts.Trace.replaceDecline("global-count:overlay-snapshot-missing")
		return 0, false, nil
	}
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			opts.Trace.replaceDecline("global-count:missing-volume")
			return 0, false, nil
		}
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return 0, true, err
		}
	}
	if extOnlySupported {
		extFilters, _ := globalExtPostingFilters(pq)
		extFilter := extFilters[0]
		baseCount := 0
		typed := pq.Type == "file" || pq.Type == "dir"
		if typed {
			// The raw posting length counts every extension match
			// regardless of record type; verify the mode per posting id
			// (plus the legacy recent merge when no overlay hides base
			// records, mirroring the untyped sources exactly).
			for volumeIndex, vol := range volumes {
				var hidden hiddenBaseIDs
				if volumeIndex >= 0 && volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
					hidden = hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
				}
				count, ok := vol.countExtPostingTyped(extFilter.ext, pq.Type, hidden, !globalSnapshotsHaveHidden(snapshots))
				if !ok {
					opts.Trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
					return 0, false, nil
				}
				baseCount += count
			}
		} else if globalSnapshotsHaveHidden(snapshots) {
			ids, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, opts.Trace)
			if !ok {
				return 0, false, nil
			}
			baseCount = len(filterGlobalIDsHidden(ids, snapshots))
		} else {
			for _, vol := range volumes {
				count, ok := vol.countExtPostingWithRecent(extFilter.ext, pq)
				if !ok {
					opts.Trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
					return 0, false, nil
				}
				baseCount += count
			}
		}
		overlayCount := globalOverlayMatchCount(volumes, snapshots, pq)
		opts.Trace.setPlannerMode("global-count-ext")
		opts.Trace.addTerm(traceTerm{
			Term:      extFilter.ext,
			Kind:      "extension",
			Source:    "global:" + extFilter.source,
			CountHint: baseCount,
			Exact:     true,
		})
		opts.Trace.setSource("global:"+extFilter.source, baseCount)
		opts.Trace.setComplete(true)
		return baseCount + overlayCount, true, nil
	}
	if !globalEnabled && !globalComponentDefaultSupported(pq, terms) {
		return 0, false, nil
	}
	if !globalComponentQuerySupportedMulti(pq, terms, len(volumes) > 1) {
		opts.Trace.replaceDecline("global-count:unsupported-query")
		return 0, false, nil
	}
	if term, ok := globalExactPathComponentTerm(pq); ok {
		baseCount := 0
		for volumeIndex, vol := range volumes {
			if coverage, fastOK := vol.mappedComponentSubstringCoverageForTop(term); fastOK {
				var hidden func(int) bool
				if volumeIndex >= 0 && volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
					h := hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
					hidden = h.contains
				}
				intervalCount, intervalVerified := coverage.countLive(vol, hidden)
				gramSelfCount, gramVisited, gramExact, gramOK := vol.countMappedComponentSelfNameGramHits(term, coverage, hidden, pq)
				if gramOK {
					baseCount += intervalCount + gramSelfCount
					driver := "mapped-pngc-self-gram"
					if gramExact {
						driver = "mapped-pngr-exact-zero"
					}
					opts.Trace.addComponentStats(driver, coverage.rootCount, len(coverage.intervals), coverage.cardinality+gramSelfCount, gramSelfCount, intervalVerified+gramVisited, false)
					continue
				}
				selfCount, selfVisited, scanOK := vol.countMappedComponentSelfHits(term, coverage, hidden, pq)
				if scanOK {
					baseCount += intervalCount + selfCount
					opts.Trace.addComponentStats("mapped-lowr-count", coverage.rootCount, len(coverage.intervals), coverage.cardinality+selfCount, selfCount, intervalVerified+selfVisited, false)
					continue
				}
			}
			coverage, fastOK := vol.mappedComponentCoverageForQuery(term, pq)
			if !fastOK {
				coverage, fastOK = vol.mappedComponentSubstringCoverage(term)
			}
			if fastOK {
				var hidden func(int) bool
				if volumeIndex >= 0 && volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
					h := hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
					hidden = h.contains
				}
				count, verified := coverage.countLive(vol, hidden)
				baseCount += count
				opts.Trace.addComponentStats("interval-count", coverage.rootCount, len(coverage.intervals), coverage.cardinality, len(coverage.selfIDs), verified, false)
				continue
			}
			var hidden func(int) bool
			if volumeIndex >= 0 && volumeIndex < len(snapshots) && snapshots[volumeIndex] != nil {
				h := hiddenBaseIDs{tombstone: snapshots[volumeIndex].tombstoneIDs, shadowed: snapshots[volumeIndex].shadowedIDs}
				hidden = h.contains
			}
			// Count the exact path component without materializing the full
			// posting slice.  This is exact for a bare term and avoids building
			// a huge []int that the verified iterator would only walk once.
			if strings.ContainsAny(term, `\/*?[]:`) {
				it := newGlobalRecordIterator(volumeIndex, vol.pathTermPosting(term))
				count, verified, err := countGlobalVerifiedIterator(&it, volumes, snapshots, pq)
				if err != nil {
					return 0, true, err
				}
				baseCount += count
				if opts.Trace != nil {
					opts.Trace.ComponentRecordsVerified += verified
				}
				continue
			}
			count := vol.countPathTermPostingLive(term, hidden)
			if queryCanceled(pq) {
				return 0, true, errQueryCanceled
			}
			baseCount += count
			if opts.Trace != nil {
				opts.Trace.ComponentRecordsVerified += count
			}
		}
		total := baseCount + globalOverlayMatchCount(volumes, snapshots, pq)
		opts.Trace.setPlannerMode("global-count-components")
		addGlobalComponentTraceTerms(opts.Trace, pq, baseCount)
		opts.Trace.setSource("global:components", baseCount)
		opts.Trace.setComplete(true)
		return total, true, nil
	}
	if len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 {
		if count, ok := globalSimplePathORCount(volumes, pq); ok && !globalSnapshotsHaveHidden(snapshots) && !globalSnapshotsHaveOverlayRecords(snapshots) {
			opts.Trace.setPlannerMode("global-count-components")
			opts.Trace.setSource("global:boolean-persisted-count", count)
			opts.Trace.setComplete(true)
			return count, true, nil
		}
		componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
		if !ok {
			if opts.Trace == nil || opts.Trace.Decline == "" {
				opts.Trace.replaceDecline("global-count-components:boolean-missing-source")
			}
			return 0, false, nil
		}
		baseCount, verified, err := countGlobalVerifiedIterator(componentIt, volumes, snapshots, pq)
		if err != nil {
			return 0, true, err
		}
		baseCount += globalOverlayMatchCount(volumes, snapshots, pq)
		if opts.Trace != nil {
			opts.Trace.ComponentRecordsVerified += verified
			opts.Trace.setPlannerMode("global-count-components")
			opts.Trace.setSource("global:boolean-iterator", baseCount)
			opts.Trace.setComplete(true)
		}
		return baseCount, true, nil
	}
	componentIt, ok := globalComponentQueryIterator(volumes, pq, opts.Trace)
	if !ok {
		if opts.Trace == nil || opts.Trace.Decline == "" {
			opts.Trace.replaceDecline("global-count-components:missing-source")
		}
		return 0, false, nil
	}
	var ids []globalRecordID
	canceled := false
	if globalSnapshotsHaveHidden(snapshots) {
		hidden := newGlobalHiddenIterator(componentIt, snapshots)
		ids, canceled = collectGlobalIteratorCancelable(&hidden, 0, func() bool { return queryCanceled(pq) })
	} else {
		ids, canceled = collectGlobalIteratorCancelable(componentIt, 0, func() bool { return queryCanceled(pq) })
	}
	if canceled {
		return 0, true, errQueryCanceled
	}
	ids = filterGlobalIDsByType(volumes, ids, pq.Type)
	baseCount, err := countVerifiedGlobalIDs(volumes, ids, pq)
	if err != nil {
		return 0, true, err
	}
	overlayCount := globalOverlayMatchCount(volumes, snapshots, pq)
	opts.Trace.setPlannerMode("global-count-components")
	addGlobalComponentTraceTerms(opts.Trace, pq, len(ids))
	opts.Trace.setSource("global:components", len(ids))
	opts.Trace.setComplete(true)
	return baseCount + overlayCount, true, nil
}
