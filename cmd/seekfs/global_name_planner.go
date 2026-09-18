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
	return !pq.CaseSensitive && !pq.MatchPath && hasTerms &&
		pq.Type == "" && pq.Under == "" && !pq.Exists && !pq.HasModAfter &&
		len(pq.Dirs) == 0 &&
		len(pq.Regexps) == 0 && len(pq.RegexTerms) == 0 && len(pq.Parents) == 0 &&
		len(pq.AttrFilters) == 0 &&
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
	if !countOnly && countNonVolumeTerms(pq.Terms) == 1 && pq.Limit > 0 {
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
	ranked, err := rankedEntriesFromGlobalIDs(snapshot.volumes, ids, pq)
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
	if countNonVolumeTerms(pq.Terms) == 1 && len(pq.SizeFilters) == 0 && len(pq.DateFilters) == 0 &&
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
		if !rec.Deleted && vol.recordMatchesNonPath(id.local, rec, volumePQ) {
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
	ranked, err := rankedEntriesFromGlobalIDs(snapshot.volumes, ids, pq)
	if err != nil {
		return nil, true, err
	}
	return ranked, true, nil
}

func globalNameCandidateIDs(snapshot globalQuerySnapshot, pq parsedQuery, trace *searchTrace) ([]globalRecordID, bool) {
	if !globalPlannerSnapshotReady(snapshot, trace, "global-name") {
		return nil, false
	}
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range snapshot.volumes {
		volumePQ := pq
		volumePQ.Trace = &searchTrace{}
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
