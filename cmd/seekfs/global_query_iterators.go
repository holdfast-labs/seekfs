package main

import (
	"slices"
	"sort"
	"strings"
)

func globalPathTermIterator(volumes []*serviceVolumeIndex, term string) (globalIDIterator, bool) {
	if term == "" {
		return nil, false
	}
	children := make([]globalIDIterator, 0, len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil || !vol.pathComponentPostingAvailable(term) {
			return nil, false
		}
		if len(volumes) == 1 && vol.index.compactRecordCount() > serviceResidentChildRangeMaxRecords &&
			len(vol.subtreeOrder) == 0 && len(vol.childOffsets) > 0 {
			// Without persisted subtree order, estimating a broad component
			// would walk the entire child graph. Let the bounded single-volume
			// planner choose its indexed candidate source instead.
			return nil, false
		}
		// When persisted subtree order is absent, do not walk every compact
		// record and repeatedly test its parent chain. The exact path posting
		// traverses the child ranges once and is cached, preserving a global
		// set source without reintroducing a per-volume terminal search.
		if len(vol.subtreeOrder) == 0 && (len(vol.childOffsets) > 0 || vol.children != nil) {
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: vol.pathTermPosting(term)})
			continue
		}
		// Old indexes without child/subtree metadata retain the exact existing
		// scan semantics. They are a bounded compatibility fallback, not a
		// partially-correct posting source.
		if !vol.hasDescendantIndex() && vol.children == nil {
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: vol.pathTermPosting(term)})
			continue
		}
		nameIDs := vol.nameTermPosting(term)
		parts := make([]globalIDIterator, 0, 2)
		if len(nameIDs) > 0 {
			parts = append(parts, &globalRecordIterator{volume: volumeIndex, ids: nameIDs})
		}
		roots := vol.pathTermRootIDs(term)
		if len(roots) > 0 {
			if len(volumes) == 1 {
				for _, root := range roots {
					if vol.estimatedDescendantOrSelfCount(root) > serviceComponentTrigramExpansionMaxIDs {
						// Let the single-volume candidate planner choose its
						// bounded source; never eagerly materialize a huge
						// subtree just to return a small top-N page.
						return nil, false
					}
				}
			}
			parts = append(parts, newGlobalSubtreeIterator(volumeIndex, vol, roots))
		}
		if len(parts) == 0 {
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: nil})
		} else {
			children = append(children, newGlobalMergeIterator(parts...))
		}
	}
	return newGlobalMergeIterator(children...), true
}

func sortGlobalPathProbeTerms(volumes []*serviceVolumeIndex, terms []string) []string {
	probes := append([]string(nil), terms...)
	sort.SliceStable(probes, func(i, j int) bool {
		return estimateGlobalPathTerm(volumes, probes[i]) < estimateGlobalPathTerm(volumes, probes[j])
	})
	return probes
}

func estimateGlobalPathTerm(volumes []*serviceVolumeIndex, term string) int {
	if term == "" {
		return 0
	}
	total := 0
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			continue
		}
		candidate, ok := vol.componentPostingCountCandidate(term)
		if ok && candidate.mapped {
			// Root postings can themselves be large (e.g. Users).  Use their
			// count as a conservative selectivity estimate instead of decoding
			// the entire root list merely to choose probe order.
			if candidate.count > 10_000 {
				total += candidate.count
				continue
			}
			for _, root32 := range candidate.materialize() {
				root := int(root32)
				if root >= 0 && root < len(vol.subtreeStart) && root < len(vol.subtreeEnd) {
					start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
					if start != ^uint32(0) && start <= end {
						total += int(end - start)
					}
				}
			}
			continue
		}
		if ok {
			total += candidate.len()
			continue
		}
		total += len(vol.nameTermPosting(term))
	}
	return total
}

func globalUnderIterator(volumes []*serviceVolumeIndex, under string) (globalIDIterator, bool) {
	roots, ok := globalUnderRoots(volumes, under)
	if !ok {
		return nil, false
	}
	byVolume := make(map[int][]int)
	for _, root := range roots {
		byVolume[root.volume] = append(byVolume[root.volume], root.local)
	}
	children := make([]globalIDIterator, 0, len(byVolume))
	for volume, volumeRoots := range byVolume {
		children = append(children, newGlobalSubtreeIterator(volume, volumes[volume], volumeRoots))
	}
	return newGlobalMergeIterator(children...), true
}

func globalParentIterator(volumes []*serviceVolumeIndex, parent string) (globalIDIterator, bool) {
	if parent == "" || strings.ContainsAny(parent, `\/:*?[]`) {
		return nil, false
	}
	children := make([]globalIDIterator, 0, len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		children = append(children, &globalRecordIterator{volume: volumeIndex, ids: vol.parentIDs(parent)})
	}
	return newGlobalMergeIterator(children...), true
}

func globalAttributeIterator(volumes []*serviceVolumeIndex, filters []uint32) (globalIDIterator, bool) {
	if len(filters) == 0 {
		return nil, false
	}
	var current globalIDIterator
	for _, mask := range filters {
		children := make([]globalIDIterator, 0, len(volumes))
		for volumeIndex, vol := range volumes {
			if vol == nil || vol.index == nil {
				return nil, false
			}
			ids, ok := vol.attrIDsForMask(mask)
			if !ok {
				return nil, false
			}
			children = append(children, &globalRecordIterator{volume: volumeIndex, ids: ids})
		}
		next := newGlobalMergeIterator(children...)
		if current == nil {
			current = next
		} else {
			current = newGlobalIntersectionIterator(current, next)
		}
	}
	return current, current != nil
}

func globalExtensionIterator(volumes []*serviceVolumeIndex, ext string, trace *searchTrace) (globalIDIterator, bool) {
	if ext == "" {
		return nil, false
	}
	children := make([]globalIDIterator, 0, len(volumes))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		posting, ok := vol.extPostingCountCandidate(ext)
		if !ok {
			trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
			return nil, false
		}
		postingIt := newGlobalPostingIteratorWithTrace(volumeIndex, posting, trace)
		children = append(children, &postingIt)
	}
	return newGlobalMergeIterator(children...), true
}

func globalComponentSubqueryIterator(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) (globalIDIterator, bool) {
	var current globalIDIterator
	intersect := func(next globalIDIterator) {
		if current == nil {
			current = next
		} else {
			current = newGlobalIntersectionIterator(current, next)
		}
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		probes := sortGlobalPathProbeTerms(volumes, pathPlanProbeTerms(terms))
		if len(probes) == 0 {
			return nil, false
		}
		if estimateGlobalPathTerm(volumes, probes[0]) > serviceComponentMultiTermScanMaxIDs {
			return nil, false
		}
		it, ok := globalPathTermIterator(volumes, probes[0])
		if !ok {
			return nil, false
		}
		current = it
		for _, term := range probes[1:] {
			filtered := newGlobalPathTermFilterIterator(current, volumes, term)
			current = &filtered
		}
	}
	for _, dir := range pq.Dirs {
		it, ok := globalPathTermIterator(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if globalRegexLiteralSupported(pq) {
		it, ok := globalPathTermIterator(volumes, pq.RegexTerms[0])
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, parent := range pq.Parents {
		it, ok := globalParentIterator(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if len(pq.AttrFilters) > 0 {
		it, ok := globalAttributeIterator(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, extFilter := range mustGlobalExtFilters(pq) {
		it, ok := globalExtensionIterator(volumes, extFilter.ext, trace)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if pq.Type != "" {
		if current == nil {
			return nil, false
		}
		current = &globalTypeFilterIterator{base: current, volumes: volumes, typ: pq.Type}
	}
	return current, current != nil
}

func mustGlobalExtFilters(pq parsedQuery) []globalExtFilter {
	filters, ok := globalExtPostingFilters(pq)
	if !ok {
		return nil
	}
	return filters
}

func globalComponentQueryIterator(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) (globalIDIterator, bool) {
	var current globalIDIterator
	intersect := func(next globalIDIterator) {
		if current == nil {
			current = next
		} else {
			current = newGlobalIntersectionIterator(current, next)
		}
	}
	if pq.Under != "" {
		under, ok := globalUnderIterator(volumes, pq.Under)
		if !ok {
			return nil, false
		}
		current = under
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		probes := sortGlobalPathProbeTerms(volumes, pathPlanProbeTerms(terms))
		if len(probes) == 0 {
			return nil, false
		}
		if estimateGlobalPathTerm(volumes, probes[0]) > serviceComponentMultiTermScanMaxIDs {
			// A multi-term plan drives off the smallest probe.  If that probe
			// is already unbounded, materializing the intersection would build
			// a slice proportional to the whole volume.  Decline so the caller
			// routes to the bounded exhaustive scan instead.
			return nil, false
		}
		it, ok := globalPathTermIterator(volumes, probes[0])
		if !ok {
			return nil, false
		}
		intersect(it)
		for _, term := range probes[1:] {
			filtered := newGlobalPathTermFilterIterator(current, volumes, term)
			current = &filtered
		}
	}
	for _, group := range pq.OrGroups {
		alternatives := make([]globalIDIterator, 0, len(group))
		for _, alt := range group {
			it, ok := globalComponentSubqueryIterator(volumes, alt, trace)
			if !ok {
				return nil, false
			}
			alternatives = append(alternatives, it)
		}
		intersect(newGlobalMergeIterator(alternatives...))
	}
	for _, dir := range pq.Dirs {
		it, ok := globalPathTermIterator(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if globalRegexLiteralSupported(pq) {
		it, ok := globalPathTermIterator(volumes, pq.RegexTerms[0])
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, parent := range pq.Parents {
		it, ok := globalParentIterator(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if len(pq.AttrFilters) > 0 {
		it, ok := globalAttributeIterator(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	for _, extFilter := range mustGlobalExtFilters(pq) {
		it, ok := globalExtensionIterator(volumes, extFilter.ext, trace)
		if !ok {
			return nil, false
		}
		intersect(it)
	}
	if pq.Type != "" {
		if current == nil {
			return nil, false
		}
		current = &globalTypeFilterIterator{base: current, volumes: volumes, typ: pq.Type}
	}
	for _, neg := range pq.NotGroups {
		if current == nil {
			all := make([]globalIDIterator, 0, len(volumes))
			for volumeIndex, vol := range volumes {
				if vol == nil || vol.index == nil {
					return nil, false
				}
				ids := make([]int, vol.index.compactRecordCount())
				for id := range ids {
					ids[id] = id
				}
				all = append(all, &globalRecordIterator{volume: volumeIndex, ids: ids})
			}
			current = newGlobalMergeIterator(all...)
		}
		excluded, ok := globalComponentSubqueryIterator(volumes, neg, trace)
		if !ok {
			return nil, false
		}
		current = newGlobalExclusionIterator(current, excluded)
	}
	return current, current != nil
}

func globalSimplePathORTerms(pq parsedQuery) ([]string, bool) {
	if !pq.MatchPath || len(pq.OrGroups) != 1 || len(pq.NotGroups) != 0 || pq.Type != "" ||
		pq.Under != "" || pq.Exists || pq.HasModAfter || len(pq.Exts) != 0 || len(pq.Dirs) != 0 ||
		len(pq.Globs) != 0 || len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 || len(pq.Parents) != 0 ||
		len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 ||
		pq.CaseSensitive || len(pq.OrGroups[0]) == 0 {
		return nil, false
	}
	if len(nonVolumeTerms(pq.Terms)) != 0 {
		return nil, false
	}
	terms := make([]string, 0, len(pq.OrGroups[0]))
	for _, alt := range pq.OrGroups[0] {
		altTerms := nonVolumeTerms(alt.Terms)
		if len(altTerms) != 1 || len(alt.OrGroups) != 0 || len(alt.NotGroups) != 0 ||
			len(alt.Dirs) != 0 || len(alt.Globs) != 0 || len(alt.Regexps) != 0 ||
			len(alt.RegexTerms) != 0 || len(alt.Parents) != 0 || len(alt.Exts) != 0 ||
			len(alt.SizeFilters) != 0 || len(alt.DateFilters) != 0 || len(alt.AttrFilters) != 0 ||
			alt.Type != "" || alt.Under != "" || alt.Exists || alt.HasModAfter {
			return nil, false
		}
		terms = append(terms, altTerms[0])
	}
	return terms, true
}

func globalSimplePathORTopIDs(volumes []*serviceVolumeIndex, pq parsedQuery, limit int) ([]globalRecordID, bool) {
	if pq.Cancel != nil {
		return nil, false
	}
	terms, ok := globalSimplePathORTerms(pq)
	if !ok || limit <= 0 {
		return nil, false
	}
	seen := make(map[globalRecordID]struct{}, len(volumes)*limit*len(terms))
	out := make([]globalRecordID, 0, len(volumes)*limit*len(terms))
	for volumeIndex, vol := range volumes {
		if queryCanceled(pq) {
			return nil, false
		}
		if vol == nil || vol.index == nil {
			return nil, false
		}
		ranks := vol.rankForQuery(pq)
		for _, term := range terms {
			if queryCanceled(pq) {
				return nil, false
			}
			if !vol.pathComponentPostingAvailable(term) {
				return nil, false
			}
			for _, local := range topCandidateIDsByRank(vol.pathTermPosting(term), limit, vol.index, ranks) {
				id := globalRecordID{volume: volumeIndex, local: local}
				if _, exists := seen[id]; exists {
					continue
				}
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	return out, true
}

func globalSimplePathORCount(volumes []*serviceVolumeIndex, pq parsedQuery) (int, bool) {
	if pq.Cancel != nil {
		return 0, false
	}
	terms, ok := globalSimplePathORTerms(pq)
	if !ok {
		return 0, false
	}
	children := make([]globalIDIterator, 0, len(volumes)*len(terms))
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return 0, false
		}
		for _, term := range terms {
			if !vol.pathComponentPostingAvailable(term) {
				return 0, false
			}
			it := newGlobalRecordIterator(volumeIndex, vol.pathTermPosting(term))
			children = append(children, &it)
		}
	}
	merged := newGlobalMergeIterator(children...)
	count, _, err := countGlobalVerifiedIterator(merged, volumes, nil, pq, nil)
	return count, err == nil
}

func globalComponentTopIDs(volumes []*serviceVolumeIndex, pq parsedQuery, limit int) ([]globalRecordID, bool) {
	term, ok := globalExactPathComponentTerm(pq)
	if limit <= 0 || !ok {
		return nil, false
	}
	out := make([]globalRecordID, 0, len(volumes)*limit)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		var ids []int
		var ok bool
		if len(vol.subtreeOrder) == 0 && (len(vol.childOffsets) > 0 || vol.children != nil) {
			// Low-memory indexes may retain exact child ranges but omit SUBT
			// rank metadata. The cached exact path posting is still a
			// complete set source; rank only that set instead of materializing
			// every match through the generic component iterator.
			ids = topCandidateIDsByRank(vol.pathTermPosting(term), limit, vol.index, vol.rankForQuery(pq))
			ok = true
		} else {
			ids, ok = vol.mappedComponentTopPosting(term, limit, pq)
		}
		if !ok {
			return nil, false
		}
		for _, id := range ids {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	return out, true
}

func globalExactPathComponentTerm(pq parsedQuery) (string, bool) {
	terms := nonVolumeTerms(pq.Terms)
	if !pq.MatchPath || len(terms) != 1 || pq.CaseSensitive ||
		pq.Type != "" || pq.Under != "" || pq.Exists || pq.HasModAfter ||
		len(pq.Exts) != 0 || len(pq.Dirs) != 0 || len(pq.Globs) != 0 ||
		len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 || len(pq.Parents) != 0 ||
		len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 ||
		len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 || pq.RootBias != "" || pq.CWDBias != "" {
		return "", false
	}
	return terms[0], true
}

func globalExtPostingIDs(volumes []*serviceVolumeIndex, ext string, limit int, trace *searchTrace) ([]globalRecordID, bool) {
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			trace.addDeclineForVolume("global-ext:missing-volume", "")
			return nil, false
		}
		posting, ok := vol.extPostingCountCandidate(ext)
		if !ok {
			trace.addDeclineForVolume("global-ext:missing-posting", vol.volume)
			return nil, false
		}
		it := newGlobalPostingIteratorWithTrace(volumeIndex, posting, trace)
		remaining := 0
		if limit > 0 {
			remaining = limit - len(out)
			if remaining <= 0 {
				return out, true
			}
		}
		out = append(out, collectGlobalIterator(&it, remaining)...)
	}
	return out, true
}

func globalComponentRootIDs(volumes []*serviceVolumeIndex, component string, limit int) ([]globalRecordID, bool) {
	if component == "" {
		return nil, false
	}
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil || !vol.pathComponentPostingAvailable(component) {
			return nil, false
		}
		for _, id := range vol.pathComponentRootIDs(component) {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
			if limit > 0 && len(out) >= limit {
				return out, true
			}
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func globalSubtreeIDs(volumes []*serviceVolumeIndex, roots []globalRecordID, limit int) ([]globalRecordID, bool) {
	if len(roots) == 0 {
		return nil, true
	}
	out := make([]globalRecordID, 0)
	seenByVolume := make(map[int]map[int]struct{})
	for _, root := range roots {
		if root.volume < 0 || root.volume >= len(volumes) {
			return nil, false
		}
		vol := volumes[root.volume]
		if vol == nil || vol.index == nil || root.local < 0 || root.local >= vol.index.compactRecordCount() {
			return nil, false
		}
		seen := seenByVolume[root.volume]
		if seen == nil {
			seen = make(map[int]struct{})
			seenByVolume[root.volume] = seen
		}
		var descendants []int
		if limit > 0 {
			descendants = vol.underDescendantsLimited(root.local, limit-len(out))
		} else {
			descendants = vol.underDescendants(root.local)
		}
		for _, id := range descendants {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, globalRecordID{volume: root.volume, local: id})
			if limit > 0 && len(out) >= limit {
				return out, true
			}
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func sortGlobalRecordIDs(ids []globalRecordID) {
	slices.SortFunc(ids, compareGlobalRecordID)
}
