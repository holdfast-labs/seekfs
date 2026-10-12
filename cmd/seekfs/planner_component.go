package main

import (
	"os"
	"strings"
)

func queryHasNonASCIIPlainTerm(pq parsedQuery) bool {
	for _, term := range pq.Terms {
		for _, r := range term {
			if r > 127 {
				return true
			}
		}
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			if queryHasNonASCIIPlainTerm(alt) {
				return true
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if queryHasNonASCIIPlainTerm(neg) {
			return true
		}
	}
	return false
}

func queryPathTermPrecheckSafe(pq parsedQuery) bool {
	return pq.MatchPath && len(pq.Terms) > 0 && !queryHasNonASCIIPlainTerm(pq)
}

func (vol *serviceVolumeIndex) componentRootTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 ||
		len(pq.Globs) > 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" || countNonVolumeTerms(pq.Terms) != 1 || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	term := ""
	for _, candidate := range pq.Terms {
		if isVolumeQueryTerm(candidate) {
			continue
		}
		term = candidate
		break
	}
	if term == "" || strings.ContainsAny(term, `\/*?[]:.-`) {
		return nil, false
	}
	var roots []int
	if it, _, ok := vol.componentPostingBlockIterator(term); ok {
		ordinal := 0
		for it.next < it.end {
			block, _, ok := it.nextBlock()
			if !ok {
				return nil, false
			}
			for _, id32 := range block {
				if ordinal&1023 == 0 && queryCanceled(pq) {
					return nil, false
				}
				ordinal++
				rootID := int(id32)
				if vol.estimatedDescendantOrSelfCount(rootID) >= 100_000 {
					roots = append(roots, rootID)
					if len(roots) >= pq.Limit*4 {
						break
					}
				}
			}
			if len(roots) >= pq.Limit*4 {
				break
			}
		}
	} else {
		for i, id32 := range vol.componentPosting32(term) {
			if i&1023 == 0 && queryCanceled(pq) {
				return nil, false
			}
			rootID := int(id32)
			if vol.estimatedDescendantOrSelfCount(rootID) >= 100_000 {
				roots = append(roots, rootID)
				if len(roots) >= pq.Limit*4 {
					break
				}
			}
		}
	}
	if len(roots) == 0 && vol.queryIndex == nil {
		for _, rootID := range vol.pathComponentRootIDs(term) {
			if vol.estimatedDescendantOrSelfCount(rootID) >= 100_000 {
				roots = append(roots, rootID)
			}
		}
	}
	if len(roots) == 0 {
		return nil, false
	}
	if serviceLowMemoryMode() && (len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 || len(vol.subtreeOrder) == 0) {
		return topCandidateIDsByRank(roots, pq.Limit, vol.index, vol.rankForQuery(pq)), true
	}
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	add := func(id int) bool {
		if id < 0 || id >= recordCount {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) >= pq.Limit
	}
	for rootPos, rootID := range roots {
		if rootPos&127 == 0 && queryCanceled(pq) {
			return nil, false
		}
		if add(rootID) {
			return out, true
		}
		if rootID < 0 || rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start == ^uint32(0) || start >= end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		for pos := start; pos < end; pos++ {
			if pos&4095 == 0 && queryCanceled(pq) {
				return nil, false
			}
			if add(int(vol.subtreeOrder[pos])) {
				return out, true
			}
		}
	}
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) componentDirectTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || vol.queryIndex == nil ||
		!serviceLowMemoryMode() || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) != 1 || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	term := ""
	for _, candidate := range pq.Terms {
		if isVolumeQueryTerm(candidate) {
			continue
		}
		term = candidate
		break
	}
	if len(term) < 3 || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	candidates := vol.componentPosting32(term)
	if len(candidates) == 0 {
		if len(vol.extPosting32(term)) > 0 {
			return nil, false
		}
		if len(vol.queryIndex.pathGrams) == 0 {
			return nil, false
		}
		grams := uniqueTrigramKeys(term)
		if len(grams) == 0 {
			return nil, false
		}
		for _, gram := range grams {
			list := vol.queryIndex.pathGrams[trigramStringFromKey(gram)]
			if len(list) == 0 {
				return nil, false
			}
			if candidates == nil || len(list) < len(candidates) {
				candidates = list
			}
		}
		for _, gram := range grams {
			list := vol.queryIndex.pathGrams[trigramStringFromKey(gram)]
			if len(list) == 0 || sameUint32Slice(list, candidates) {
				continue
			}
			candidates = intersectSortedUint32s(candidates, list)
			if len(candidates) == 0 {
				return nil, false
			}
		}
	}
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	add := func(id int) bool {
		if id < 0 || id >= recordCount {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) >= pq.Limit
	}
	for _, id32 := range candidates {
		id := int(id32)
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 || !containsFoldASCII(vol.index.compactNameAt(id), term) {
			continue
		}
		if add(id) {
			return out, true
		}
		if id >= 0 && id < len(vol.subtreeStart) && id < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
			start, end := vol.subtreeStart[id], vol.subtreeEnd[id]
			if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
				for pos := start; pos < end; pos++ {
					if add(int(vol.subtreeOrder[pos])) {
						return out, true
					}
				}
			}
		}
	}
	if len(out) > 0 && len(out) < pq.Limit && len(vol.subtreeOrder) == 0 {
		roots := append([]int(nil), out...)
		scanned := vol.scanOrderedLimited(pq, pq.Limit-len(out), func(id int) bool {
			if _, ok := seen[id]; ok {
				return false
			}
			return vol.isDescendantOrSelfAnyFast(id, roots) && vol.index.compactPathContainsTerm(id, term)
		})
		for _, id := range scanned {
			if add(id) {
				return out, true
			}
		}
	}
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) componentMultiTermTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || vol.queryIndex == nil || !vol.hasDescendantIndex() ||
		!serviceLowMemoryMode() || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) < 2 || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	var best []int
	bestEstimate := int(^uint(0) >> 1)
	for _, term := range pq.Terms {
		if len(term) < 3 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:.`) ||
			vol.pathTermIsUsableExtensionCandidate(term) {
			continue
		}
		nameMatches, roots, complete := vol.pathDirectoryTermSource(term)
		if complete && len(nameMatches) == 0 {
			return []int{}, true
		}
		if len(roots) == 0 {
			for _, id32 := range vol.componentPosting32(term) {
				roots = append(roots, int(id32))
			}
		}
		if len(roots) == 0 {
			continue
		}
		estimate := 0
		for _, id := range roots {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			if len(vol.subtreeOrder) == 0 && (len(vol.childOffsets) > 0 || vol.children != nil) {
				estimate += len(vol.underDescendantsLimited(id, serviceComponentMultiTermScanMaxIDs+1))
			} else {
				estimate += vol.estimatedDescendantOrSelfCount(id)
			}
			if estimate > bestEstimate {
				break
			}
		}
		if estimate > 0 && estimate < bestEstimate {
			best = roots
			bestEstimate = estimate
		}
	}
	if len(best) == 0 {
		return nil, false
	}
	if len(vol.subtreeOrder) == 0 {
		var bestTerm string
		for _, term := range pq.Terms {
			if len(term) < 3 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:.`) ||
				vol.pathTermIsUsableExtensionCandidate(term) {
				continue
			}
			_, roots, _ := vol.pathDirectoryTermSource(term)
			if len(roots) == 0 {
				for _, id32 := range vol.componentPosting32(term) {
					roots = append(roots, int(id32))
				}
			}
			if len(roots) == len(best) {
				same := true
				for i := range roots {
					if roots[i] != best[i] {
						same = false
						break
					}
				}
				if same {
					bestTerm = term
					break
				}
			}
		}
		if bestTerm != "" && bestEstimate <= serviceComponentMultiTermScanMaxIDs {
			seenCandidates := make(map[int]struct{}, len(best))
			candidates := make([]int, 0, min(bestEstimate, serviceComponentMultiTermScanMaxIDs))
			for _, id := range best {
				for _, candidate := range vol.underDescendantsLimited(id, serviceComponentMultiTermScanMaxIDs+1) {
					if _, ok := seenCandidates[candidate]; ok {
						continue
					}
					seenCandidates[candidate] = struct{}{}
					candidates = append(candidates, candidate)
				}
			}
			out := make([]int, 0, min(len(candidates), pq.Limit))
			for _, id := range candidates {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if rec.Deleted || !vol.index.compactPathContainsAll(id, pq.Terms) {
					continue
				}
				out = append(out, id)
			}
			if len(out) > 0 {
				sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
				return out, true
			}
		}
		roots := make([]int, 0, min(len(best), 16))
		for _, id := range best {
			if id >= 0 && id < vol.index.compactRecordCount() {
				roots = append(roots, id)
				if len(roots) >= 16 {
					break
				}
			}
		}
		if len(roots) == 0 {
			return nil, false
		}
		out := vol.scanOrderedLimited(pq, pq.Limit, func(id int) bool {
			if !vol.isDescendantOrSelfAnyFast(id, roots) {
				return false
			}
			rec := vol.index.compactRecord(id)
			return !rec.Deleted && vol.index.compactPathContainsAll(id, pq.Terms)
		})
		if len(out) >= pq.Limit {
			return out, true
		}
		return nil, false
	}
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	scanned := 0
	add := func(id int) bool {
		if id < 0 || id >= recordCount {
			return false
		}
		scanned++
		if _, ok := seen[id]; ok {
			return false
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsAll(id, pq.Terms) {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) >= pq.Limit
	}
	for rootIndex, id := range best {
		if rootIndex&127 == 0 && queryCanceled(pq) {
			return nil, false
		}
		if add(id) {
			return out, true
		}
		if id < 0 || id >= len(vol.subtreeStart) || id >= len(vol.subtreeEnd) || len(vol.subtreeOrder) == 0 {
			continue
		}
		start, end := vol.subtreeStart[id], vol.subtreeEnd[id]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		for pos := start; pos < end; pos++ {
			if bestEstimate > serviceComponentTrigramExpansionMaxIDs && scanned >= serviceComponentMultiTermScanMaxIDs {
				if len(out) >= pq.Limit {
					return out, true
				}
				return nil, false
			}
			if pos&4095 == 0 && queryCanceled(pq) {
				return nil, false
			}
			if add(int(vol.subtreeOrder[pos])) {
				return out, true
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func (vol *serviceVolumeIndex) selectiveNamePathTermCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) < 2 || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	bestTerm := ""
	bestIDs := []int(nil)
	for _, term := range pq.Terms {
		if len(term) < 4 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:`) || !filenameLikePathTerm(term) {
			continue
		}
		ids, ok := vol.nameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
		if !ok || len(ids) > serviceComponentTrigramExpansionMaxIDs {
			continue
		}
		if vol.hasDirectoryCandidate(ids) {
			continue
		}
		if bestTerm == "" || len(ids) < len(bestIDs) {
			bestTerm = term
			bestIDs = ids
		}
	}
	if bestTerm == "" {
		return nil, false
	}
	out := make([]int, 0, min(pq.Limit, len(bestIDs)))
	for _, id := range bestIDs {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsAll(id, pq.Terms) {
			continue
		}
		out = append(out, id)
		if len(out) >= pq.Limit {
			break
		}
	}
	if len(out) == 0 {
		return []int{}, true
	}
	return topCandidateIDsByRank(out, pq.Limit, vol.index, vol.rankForQuery(pq)), true
}

func filenameLikePathTerm(term string) bool {
	return strings.ContainsAny(term, "._-")
}

func asciiOnlyString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) multiTermEmptyPathCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 || len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.HasModAfter || pq.Exists || pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) < 2 {
		return nil, false
	}
	for _, term := range pq.Terms {
		if len(term) < 3 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		if !asciiOnlyString(term) {
			return nil, false
		}
		if len(term) >= 6 && vol.queryIndex != nil && len(vol.componentPosting32(term)) == 0 {
			if trigrams := vol.nameTrigramIndex(); trigrams != nil {
				_, ok, missing := trigrams.selectiveIntersectCandidateIDs(term, servicePathNameTrigramCandidateMaxIDs)
				if ok && missing {
					return []int{}, true
				}
			}
		}
	}
	return nil, false
}
