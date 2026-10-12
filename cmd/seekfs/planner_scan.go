package main

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

func (vol *serviceVolumeIndex) hasDescendantIndex() bool {
	return vol != nil && (len(vol.subtreeOrder) > 0 || len(vol.childOffsets) > 0 || vol.children != nil)
}

func (vol *serviceVolumeIndex) estimatedDescendantOrSelfCount(rootID int) int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return 0
	}
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			return int(end - start)
		}
	}
	vol.termMu.Lock()
	if vol.underCache != nil {
		if cached, ok := vol.underCache[rootID]; ok {
			vol.termMu.Unlock()
			return len(cached.ids)
		}
	}
	vol.termMu.Unlock()
	return len(vol.underDescendantsLimited(rootID, serviceComponentTrigramExpansionMaxIDs+1))
}

func (vol *serviceVolumeIndex) limitedSingleTermCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CountOnly || pq.Limit <= 0 || pq.CaseSensitive ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" || pq.SortColumn != "" || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	if !pq.MatchPath && len(pq.Terms) == 1 && len(pq.Dirs) == 0 {
		return vol.scanNameTermLimited(pq, pq.Terms[0], pq.Limit), true
	}
	if len(pq.Terms) == 0 && len(pq.Dirs) == 1 {
		return vol.scanPathTermLimited(pq, pq.Dirs[0], pq.Limit), true
	}
	return nil, false
}

func (vol *serviceVolumeIndex) scanNameTermLimited(pq parsedQuery, term string, limit int) []int {
	if term == "" || limit <= 0 {
		return nil
	}
	return vol.scanOrderedLimited(pq, limit, func(i int) bool {
		return containsFoldASCII(vol.index.compactNameAt(i), term)
	})
}

func (vol *serviceVolumeIndex) scanPathTermLimited(pq parsedQuery, term string, limit int) []int {
	if term == "" || limit <= 0 {
		return nil
	}
	return vol.scanOrderedLimited(pq, limit, func(i int) bool {
		return vol.index.compactPathContainsTerm(i, term)
	})
}

func (vol *serviceVolumeIndex) scanPathTermPrefixLimited(pq parsedQuery, term string, limit int, maxScan int) []int {
	if term == "" || limit <= 0 || maxScan <= 0 {
		return nil
	}
	var orderLen int
	if vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0 {
		orderLen = len(vol.queryIndex.nameOrder)
	} else {
		orderLen = compactOrderLen(vol.index.CompactNameOrder, vol.index.compactRecordCount())
	}
	end := min(orderLen, maxScan)
	return vol.scanOrderedLimitedRange(pq, 0, end, limit, func(i int) bool {
		return vol.index.compactPathContainsTerm(i, term)
	})
}

func (vol *serviceVolumeIndex) scanOrderedLimited(pq parsedQuery, limit int, match func(int) bool) []int {
	recordCount := vol.index.compactRecordCount()
	orderLen := recordCount
	useResidentOrder := vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0
	if useResidentOrder {
		orderLen = len(vol.queryIndex.nameOrder)
	} else {
		orderLen = compactOrderLen(vol.index.CompactNameOrder, recordCount)
	}
	prefixEnd := min(orderLen, 4_096)
	out := vol.scanOrderedLimitedRange(pq, 0, prefixEnd, limit, match)
	if len(out) >= limit || prefixEnd >= orderLen {
		return out
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, orderLen/25_000))
	if workers <= 1 || orderLen < 50_000 {
		tail := vol.scanOrderedLimitedRange(pq, prefixEnd, orderLen, limit-len(out), match)
		return append(out, tail...)
	}
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := prefixEnd + worker*(orderLen-prefixEnd)/workers
		end := prefixEnd + (worker+1)*(orderLen-prefixEnd)/workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			parts[worker] = vol.scanOrderedLimitedRange(pq, start, end, limit-len(out), match)
		}(worker, start, end)
	}
	wg.Wait()
	for _, part := range parts {
		for _, id := range part {
			out = append(out, id)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func (vol *serviceVolumeIndex) scanOrderedLimitedRange(pq parsedQuery, start, end, limit int, match func(int) bool) []int {
	out := make([]int, 0, limit)
	recordCount := vol.index.compactRecordCount()
	useResidentOrder := vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0
	for pos := start; pos < end; pos++ {
		if pos&4095 == 0 && queryCanceled(pq) {
			return out
		}
		i := pos
		if useResidentOrder {
			i = int(vol.queryIndex.nameOrder[pos])
		} else {
			i = compactOrderAt(vol.index.CompactNameOrder, pos)
		}
		if i < 0 || i >= recordCount {
			continue
		}
		rec := vol.index.compactRecord(i)
		if rec.Deleted {
			continue
		}
		if match(i) {
			out = append(out, i)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func (vol *serviceVolumeIndex) cachedMultiNameTermCandidates(terms []string) ([]int, bool) {
	vol.termMu.Lock()
	if vol.termCache == nil {
		vol.termMu.Unlock()
		return nil, false
	}
	lists := make([][]int, 0, len(terms))
	seqs := make([]uint64, 0, len(terms))
	for _, term := range terms {
		entry, ok := vol.termCache[term]
		if !ok {
			vol.termMu.Unlock()
			return nil, false
		}
		if !vol.cacheStampValid(entry.gen) {
			vol.termMu.Unlock()
			return nil, false
		}
		lists = append(lists, append([]int(nil), entry.ids...))
		seqs = append(seqs, entry.gen)
	}
	vol.termMu.Unlock()
	for i, term := range terms {
		lists[i] = vol.withRecentCandidates(lists[i], seqs[i], func(rec CompactRecord) bool {
			id, ok := vol.idForFRN(rec.FRN)
			return ok && strings.Contains(vol.index.compactLowerNameAt(id), term)
		})
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) multiNameTermCandidates(terms []string) []int {
	lists := make([][]int, len(terms))
	for termIndex, term := range terms {
		lists[termIndex] = vol.nameTermPosting(term)
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	return candidates
}

func (vol *serviceVolumeIndex) plannerCandidates(pq parsedQuery) ([]int, bool) {
	globExts, globsOK := simpleGlobExts(pq.Globs)
	if vol == nil || vol.index == nil || vol.queryIndex == nil || pq.CaseSensitive || pq.Under != "" || pq.Exists || pq.HasModAfter || !globsOK {
		return nil, false
	}
	strong := make([][]uint32, 0, len(pq.Terms)+len(pq.Exts)+len(globExts)+2)
	addStrong := func(list []uint32) bool {
		if len(list) == 0 {
			return false
		}
		strong = append(strong, list)
		return true
	}
	qi := vol.queryIndex
	lastBareExt := []uint32(nil)
	for _, ext := range pq.Exts {
		if !addStrong(qi.ext[ext]) {
			return []int{}, true
		}
	}
	for _, ext := range globExts {
		if !addStrong(qi.ext[ext]) {
			return []int{}, true
		}
	}
	for _, term := range pq.RegexTerms {
		if list := qi.ext[term]; len(list) > 0 {
			addStrong(list)
		}
	}
	if pq.Type == "dir" {
		addStrong(qi.dirs)
	}
	for _, term := range pq.Terms {
		if pq.MatchPath {
			if strings.HasSuffix(term, ":") {
				if !strings.EqualFold(term, vol.volume) {
					return []int{}, true
				}
				continue
			}
			if strings.HasPrefix(term, ".") && len(term) > 1 {
				if list := qi.ext[strings.TrimPrefix(term, ".")]; len(list) > 0 {
					addStrong(list)
					continue
				}
			}
			if list := qi.ext[term]; len(list) > 0 {
				lastBareExt = list
			}
			if ext := strings.TrimPrefix(filepath.Ext(term), "."); ext != "" {
				if list := qi.ext[ext]; len(list) > 0 {
					lastBareExt = list
				}
			}
			continue
		}
		if strings.HasPrefix(term, ".") && len(term) > 1 {
			if list := qi.ext[strings.TrimPrefix(term, ".")]; len(list) > 0 {
				addStrong(list)
				continue
			}
		}
		if list := qi.ext[term]; len(list) > 0 {
			lastBareExt = list
		}
		if ext := strings.TrimPrefix(filepath.Ext(term), "."); ext != "" {
			if list := qi.ext[ext]; len(list) > 0 {
				lastBareExt = list
			}
		}
	}
	if len(strong) == 0 && len(lastBareExt) > 0 {
		addStrong(lastBareExt)
	}
	if len(strong) == 0 {
		return nil, false
	}
	lists := strong
	sortUint32ListsByLen(lists)
	candidates := append([]uint32(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedUint32s(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	out := uint32sToInts(candidates)
	if len(vol.recentIDs) > 0 {
		out = append(out, mapKeys(vol.recentIDs)...)
		sort.Ints(out)
		out = uniqueSortedInts(out)
	}
	return out, true
}

func simpleGlobExts(globs []string) ([]string, bool) {
	if len(globs) == 0 {
		return nil, true
	}
	exts := make([]string, 0, len(globs))
	for _, glob := range globs {
		if !strings.HasPrefix(glob, "*.") || strings.Count(glob, "*") != 1 || strings.ContainsAny(strings.TrimPrefix(glob, "*."), `\/*?[]:`) {
			return nil, false
		}
		ext := strings.ToLower(strings.TrimPrefix(glob, "*."))
		if ext == "" {
			return nil, false
		}
		exts = append(exts, ext)
	}
	return exts, true
}
