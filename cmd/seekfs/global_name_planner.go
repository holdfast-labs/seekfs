package main

import "strings"

func globalNameQuerySupported(pq parsedQuery) bool {
	if queryHasAnyContentLeaf(pq) {
		return false
	}
	hasGlobs := len(pq.Globs) > 0
	if hasGlobs && !pqGramsCouldDriveGlobs(pq) {
		return false
	}
	hasTerms := len(nonVolumeTerms(pq.Terms)) > 0 || (hasGlobs && len(globLiteralTerms(pq.Globs, pq.CaseSensitive)) > 0)
	// Extra predicates (Under/Type/Dirs/Parents/Exists/ModAfter/Attrs/case)
	// are allowed: the complete candidate path derives the full name set
	// from the trigram (which declines when broad) and the later
	// verification enforces every predicate exactly. The truncated
	// top-ranked path is skipped whenever such a predicate is present.
	return !pq.MatchPath && hasTerms &&
		len(pq.Regexps) == 0 && len(pq.RegexTerms) == 0 &&
		len(pq.OrGroups) == 0 && len(pq.NotGroups) == 0 && pq.CWDBias == "" && pq.RootBias == ""
}

func searchServiceVolumesGlobalNameSnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, true, err
	}
	pq.Limit = normalizedLimit(opts.Limit, false)
	if !globalNameQuerySupported(pq) {
		return nil, false, nil
	}
	// Top-ranked truncation is unsafe with path-needing predicates
	// (Under/Dirs/Parents/Exists are invisible to recordMatchesNonPath, so
	// top-N by rank filtered later can underfill); those queries use the
	// complete path below.
	if !countOnly && countNonVolumeTerms(pq.Terms) == 1 && pq.Limit > 0 &&
		pq.Under == "" && len(pq.Dirs) == 0 && len(pq.Parents) == 0 && !pq.Exists {
		if ranked, ok, err := globalNameTopRanked(snapshot, pq, opts.Trace); ok {
			if err != nil {
				return nil, true, err
			}
			ranked = mergeGlobalVerifiedEntries(snapshot, ranked, pq, normalizedLimit(opts.Limit, false))
			setGlobalNameTrace(opts.Trace, pq, len(ranked), false)
			return globalRankedEntriesToEntries(ranked), true, nil
		}
	}
	ids, ok := globalNameCandidateIDs(snapshot, pq, opts.Trace)
	if !ok {
		return nil, false, nil
	}
	ranked, err := rankedEntriesFromGlobalIDs(snapshot.volumes, ids, pq, nil)
	if err != nil {
		return nil, true, err
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	if countOnly {
		limit = 0
	}
	ranked = mergeGlobalVerifiedEntries(snapshot, ranked, pq, limit)
	if len(ids) == 0 && globalNameAllExactEmpty(snapshot, pq) && opts.Trace != nil {
		opts.Trace.setSource("exact-empty", 0)
	}
	setGlobalNameTrace(opts.Trace, pq, len(ids), false)
	return globalRankedEntriesToEntries(ranked), true, nil
}

func countServiceVolumesGlobalNameSnapshot(snapshot globalQuerySnapshot, opts queryOptions) (int, bool, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	if !globalNameQuerySupported(pq) {
		return 0, false, nil
	}
	// The posting-intersection fast count verifies the name substring only;
	// any other predicate (including ext:/glob: filters) must take the
	// verifying path below so counts stay exact.
	if countNonVolumeTerms(pq.Terms) == 1 && len(pq.SizeFilters) == 0 && len(pq.DateFilters) == 0 &&
		pq.Type == "" && len(pq.Dirs) == 0 && len(pq.Parents) == 0 && pq.Under == "" &&
		!pq.Exists && !pq.HasModAfter && !pq.CaseSensitive && len(pq.AttrFilters) == 0 &&
		len(pq.Exts) == 0 && len(pq.Globs) == 0 &&
		!globalSnapshotsHaveHidden(snapshot.overlays) {
		count := 0
		completeSource := true
		usedPNGC := false
		for _, vol := range snapshot.volumes {
			volumePQ := pq
			volumePQ.Trace = &searchTrace{}
			term := nonVolumeTerms(pq.Terms)[0]
			if exact, ok := vol.completeFilenameCountPosting(term, volumePQ); ok {
				count += exact
				if volumePQ.Trace.FilenameDriver == "posting-intersection-pngc" {
					usedPNGC = true
				}
				if volumePQ.Trace != nil {
					opts.Trace.FilenameDriver = volumePQ.Trace.FilenameDriver
					opts.Trace.FilenameRequiredGrams += volumePQ.Trace.FilenameRequiredGrams
					opts.Trace.FilenamePostingHint = max(opts.Trace.FilenamePostingHint, volumePQ.Trace.FilenamePostingHint)
					opts.Trace.FilenameRecordsVerified += volumePQ.Trace.FilenameRecordsVerified
					opts.Trace.BlocksDecoded += volumePQ.Trace.BlocksDecoded
					opts.Trace.BlocksSkipped += volumePQ.Trace.BlocksSkipped
					opts.Trace.PostingPrefetchBytes += volumePQ.Trace.PostingPrefetchBytes
					opts.Trace.PostingPrefetchRanges += volumePQ.Trace.PostingPrefetchRanges
					opts.Trace.PostingPrefetchPages += volumePQ.Trace.PostingPrefetchPages
				}
				continue
			}
			completeSource = false
			ids, ok := vol.filenameTrigramCandidates(volumePQ)
			if !ok {
				return 0, false, nil
			}
			count += len(ids)
		}
		count += globalOverlayMatchCount(snapshot.volumes, snapshot.overlays, pq)
		if completeSource {
			if usedPNGC {
				opts.Trace.setSource("count-fast-pngc", count)
			} else {
				opts.Trace.setSource("count-fast-pngr", count)
			}
		}
		setGlobalNameTrace(opts.Trace, pq, count, true)
		if count == 0 && opts.Trace != nil {
			opts.Trace.setSource("exact-empty", 0)
		}
		return count, true, nil
	}
	ids, ok := globalNameCandidateIDs(snapshot, pq, opts.Trace)
	if !ok {
		return 0, false, nil
	}
	if len(ids) == 0 && globalNameAllExactEmpty(snapshot, pq) && opts.Trace != nil {
		opts.Trace.setSource("exact-empty", 0)
	}
	count := 0
	// Path-needing predicates (Under/Dirs/Parents/Exists/Features) cannot be
	// decided from the record alone; reconstruct and match the full entry
	// exactly like the search path does. Under was already intersected
	// above, so this re-check only confirms the subtree pre-filter.
	needPath := queryNeedsPath(pq)
	pathCaches := make([]map[int]string, len(snapshot.volumes))
	for pos, id := range ids {
		if pos&1023 == 0 && queryCanceled(pq) {
			return 0, true, errQueryCanceled
		}
		if id.volume < 0 || id.volume >= len(snapshot.volumes) {
			continue
		}
		vol := snapshot.volumes[id.volume]
		if id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		rec := vol.index.compactRecord(id.local)
		if rec.Deleted {
			continue
		}
		if !needPath {
			if vol.recordMatchesNonPath(id.local, rec, volumePQ) {
				count++
			}
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		if _, ok := compactCandidateEntryIfMatch(vol.index, volumePQ, id.local, pathCaches[id.volume], true, false); ok {
			count++
		}
	}
	count += globalOverlayMatchCount(snapshot.volumes, snapshot.overlays, pq)
	setGlobalNameTrace(opts.Trace, pq, len(ids), true)
	if count == 0 && opts.Trace != nil {
		opts.Trace.setSource("exact-empty", 0)
	}
	return count, true, nil
}

func globalNameTopRanked(snapshot globalQuerySnapshot, pq parsedQuery, trace *searchTrace) ([]globalRankedEntry, bool, error) {
	if !globalPlannerSnapshotReady(snapshot, trace, "global-name") {
		return nil, false, nil
	}
	limit := normalizedLimit(pq.Limit, false)
	ids := make([]globalRecordID, 0, limit*len(snapshot.volumes))
	for volumeIndex, vol := range snapshot.volumes {
		if vol == nil || vol.index == nil {
			return nil, false, nil
		}
		volumePQ := pq
		volumePQ.Trace = &searchTrace{}
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		candidates, ok := vol.completeFilenameTopPosting(nonVolumeTerms(pq.Terms)[0], pq.Limit, volumePQ)
		if ok {
			if volumePQ.Trace != nil && volumePQ.Trace.FilenameDriver != "" {
				trace.FilenameDriver = volumePQ.Trace.FilenameDriver
				trace.FilenameRequiredGrams += volumePQ.Trace.FilenameRequiredGrams
				trace.FilenameRecordsVerified += volumePQ.Trace.FilenameRecordsVerified
				trace.BlocksDecoded += volumePQ.Trace.BlocksDecoded
				trace.BlocksSkipped += volumePQ.Trace.BlocksSkipped
				trace.PostingPrefetchBytes += volumePQ.Trace.PostingPrefetchBytes
				trace.PostingPrefetchRanges += volumePQ.Trace.PostingPrefetchRanges
				trace.PostingPrefetchPages += volumePQ.Trace.PostingPrefetchPages
				globalSource := "global:filename-pngr"
				if strings.Contains(volumePQ.Trace.FilenameDriver, "pngc") {
					globalSource = "global:filename-pngc"
				}
				trace.setSource(globalSource, len(candidates))
			}
		} else {
			candidates, ok = vol.filenameTrigramCandidates(volumePQ)
		}
		if !ok {
			return nil, false, nil
		}
		if len(candidates) == 0 && volumePQ.Trace != nil && volumePQ.Trace.Source == "exact-empty" && len(snapshot.volumes) == 1 {
			trace.setSource("exact-empty", 0)
		}
		visible := candidates
		if globalSnapshotsHaveHidden(snapshot.overlays) {
			visible = make([]int, 0, len(candidates))
			for _, local := range candidates {
				if !globalHiddenContains(snapshot.overlays, globalRecordID{volume: volumeIndex, local: local}) {
					visible = append(visible, local)
				}
			}
		}
		// The rank integers are local to a volume and are not a global
		// tie-breaker. Keep the complete bounded posting from each volume,
		// then verify and apply compareSearchAllEntries across the merged set.
		// Truncating here can discard the other volume's equal-name/path tie.
		for _, local := range visible {
			ids = append(ids, globalRecordID{volume: volumeIndex, local: local})
		}
	}
	ranked, err := rankedEntriesFromGlobalIDs(snapshot.volumes, ids, pq, nil)
	if err != nil {
		return nil, true, err
	}
	return ranked, true, nil
}

func globalNameCandidateIDs(snapshot globalQuerySnapshot, pq parsedQuery, trace *searchTrace) ([]globalRecordID, bool) {
	if !globalPlannerSnapshotReady(snapshot, trace, "global-name") {
		return nil, false
	}
	// Under filtering is name-first: trigram gives the complete name set
	// (declines when broad), then we intersect with the Under subtree.
	// Verification later re-checks Under via entryMatches, so this is exact.
	var underRoots []globalRecordID
	if pq.Under != "" {
		var ok bool
		underRoots, ok = globalUnderRoots(snapshot.volumes, pq.Under)
		if !ok {
			return nil, false
		}
		if len(underRoots) == 0 {
			// Scope not in index: decline so the single-volume path can
			// try the filesystem-under fallback (new/unindexed files).
			return nil, false
		}
	}
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range snapshot.volumes {
		volumePQ := pq
		volumePQ.Trace = &searchTrace{}
		// Trigram lane declines on extra predicates; clear them here. The
		// trigram returns the complete name superset (it declines when
		// broad), Under is intersected below, and every other predicate is
		// enforced exactly by verification later. Clearing case only widens
		// to the insensitive superset, which verification narrows exactly.
		volumePQ.Under = ""
		volumePQ.Type = ""
		volumePQ.Dirs = nil
		volumePQ.Exists = false
		volumePQ.HasModAfter = false
		volumePQ.CaseSensitive = false
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		ids, ok := vol.filenameTrigramCandidates(volumePQ)
		if !ok {
			reason := "global-name:no-selective-trigram"
			if vol.nameTrigramIndex() == nil {
				reason = "global-name:trigram-not-ready"
			}
			trace.addDeclineForVolume(reason, vol.volume)
			return nil, false
		}
		if volumePQ.Trace != nil && volumePQ.Trace.FilenameDriver != "" {
			trace.FilenameDriver = volumePQ.Trace.FilenameDriver
			trace.FilenameRequiredGrams += volumePQ.Trace.FilenameRequiredGrams
			trace.FilenameRecordsVerified += volumePQ.Trace.FilenameRecordsVerified
			trace.BlocksDecoded += volumePQ.Trace.BlocksDecoded
			trace.BlocksSkipped += volumePQ.Trace.BlocksSkipped
			trace.PostingPrefetchBytes += volumePQ.Trace.PostingPrefetchBytes
			trace.PostingPrefetchRanges += volumePQ.Trace.PostingPrefetchRanges
			trace.PostingPrefetchPages += volumePQ.Trace.PostingPrefetchPages
		}
		if len(ids) == 0 && volumePQ.Trace != nil && volumePQ.Trace.Source == "exact-empty" && len(snapshot.volumes) == 1 {
			trace.setSource("exact-empty", 0)
		}
		for _, id := range ids {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	sortGlobalRecordIDs(out)
	if len(underRoots) > 0 {
		out = filterGlobalIDsBySubtrees(snapshot.volumes, underRoots, out)
		if len(out) == 0 {
			// Nothing under the scope in the index: decline so the
			// single-volume path can try the filesystem-under fallback
			// (new/unindexed files). Returning fast-empty here would
			// silently miss files the old path used to find.
			return nil, false
		}
	}
	return filterGlobalIDsHidden(out, snapshot.overlays), true
}

func setGlobalNameTrace(trace *searchTrace, pq parsedQuery, candidates int, count bool) {
	if trace == nil {
		return
	}
	mode := "global-name"
	if count {
		mode = "global-count-name"
	}
	trace.setPlannerMode(mode)
	if trace.Source != "exact-empty" {
		if trace.FilenameDriver != "" && strings.Contains(trace.FilenameDriver, "pngc") {
			trace.setSource("global:filename-pngc", candidates)
		} else {
			trace.setSource("global:filename-trigram", candidates)
		}
	}
	termSource := "global:filename-trigram"
	if trace.FilenameDriver != "" && strings.Contains(trace.FilenameDriver, "pngc") {
		termSource = "global:filename-pngc"
	}
	for _, term := range nonVolumeTerms(pq.Terms) {
		trace.addTerm(traceTerm{Term: term, Kind: "name-substring", Source: termSource, CountHint: candidates, Exact: false})
	}
	trace.setComplete(true)
}

func globalNameAllExactEmpty(snapshot globalQuerySnapshot, pq parsedQuery) bool {
	if len(snapshot.volumes) == 0 {
		return false
	}
	for _, vol := range snapshot.volumes {
		if vol == nil || vol.index == nil {
			return false
		}
		for _, term := range nonVolumeTerms(pq.Terms) {
			grams := vol.nameTrigramIndex()
			if grams == nil {
				grams = vol.index.Derived.NameTrigrams
			}
			if grams == nil {
				return false
			}
			for _, gram := range grams.termGramKeys(term) {
				_, _, _, _, exactEmpty := vol.nameGramPosting(gram)
				if !exactEmpty {
					return false
				}
			}
		}
	}
	return true
}
