package main

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

func (vol *serviceVolumeIndex) underCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.Under == "" {
		return nil, false
	}
	under := filepath.Clean(pq.Under)
	if vol.index.Volume != "" && !strings.EqualFold(filepath.VolumeName(under), vol.index.Volume) {
		return []int{}, true
	}
	base := strings.ToLower(filepath.Base(under))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return nil, false
	}
	roots := vol.underRootIDs(under)
	if len(roots) == 0 {
		return []int{}, true
	}
	if candidates, ok := vol.underLimitedTermCandidates(roots, pq); ok {
		return candidates, true
	}
	out := make([]int, 0, 256)
	prefilter := vol.underPrefilter(pq)
	for _, rootID := range roots {
		if rootID < 0 || rootID >= vol.index.compactRecordCount() || vol.index.compactRecord(rootID).Deleted {
			continue
		}
		if len(vol.childOffsets) == 0 && vol.children == nil {
			if prefilter != nil {
				prefilterIDs := make([]int, 0, len(prefilter))
				for id := range prefilter {
					prefilterIDs = append(prefilterIDs, id)
				}
				sort.Ints(prefilterIDs)
				for _, id := range prefilterIDs {
					if id < 0 || id >= vol.index.compactRecordCount() {
						continue
					}
					rec := vol.index.compactRecord(id)
					if vol.isDescendantOrSelf(id, rootID) && !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
						out = append(out, id)
					}
				}
				continue
			}
			descendants := vol.underDescendants(rootID)
			for _, id := range descendants {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
					out = append(out, id)
				}
			}
			continue
		}
		if prefilter != nil {
			for id := range prefilter {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if vol.isDescendantOrSelf(id, rootID) && !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
					out = append(out, id)
				}
			}
			continue
		}
		seen := make(map[int]struct{}, 256)
		stack := []int{rootID}
		for len(stack) > 0 {
			last := len(stack) - 1
			id := stack[last]
			stack = stack[:last]
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			seen[id] = struct{}{}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && compactRecordPrecheck(rec, pq, true) {
				out = append(out, id)
			}
			for _, childID := range vol.childIDsForRecord(id) {
				stack = append(stack, int(childID))
			}
		}
	}
	sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
	return out, true
}

func (vol *serviceVolumeIndex) underRootIDs(under string) []int {
	if vol == nil || vol.index == nil {
		return nil
	}
	cacheKey := strings.ToLower(filepath.Clean(under))
	vol.termMu.Lock()
	if vol.underRootCache != nil {
		if entry, ok := vol.underRootCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return append([]int(nil), entry.ids...)
			}
		}
	}
	vol.termMu.Unlock()
	var roots []int
	volume := filepath.VolumeName(under)
	rest := strings.TrimPrefix(under, volume)
	rest = strings.Trim(rest, `\/`)
	if rest == "" {
		if len(vol.rootIDs) > 0 {
			roots = make([]int, 0, len(vol.rootIDs))
			for _, id := range vol.rootIDs {
				roots = append(roots, int(id))
			}
		} else {
			roots = []int{0}
		}
		vol.cacheUnderRoots(cacheKey, roots)
		return append([]int(nil), roots...)
	}
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' })
	candidates := make([]int, 0, 4)
	recordCount := vol.index.compactRecordCount()
	if len(vol.rootIDs) > 0 {
		for _, id := range vol.rootIDs {
			if int(id) < recordCount {
				candidates = append(candidates, int(id))
			}
		}
	} else {
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Parent < 0 && !rec.Deleted {
				candidates = append(candidates, id)
			}
		}
	}
	if len(vol.childOffsets) == 0 && vol.children == nil {
		if roots := vol.underRootIDsByBasename(under); len(roots) > 0 {
			vol.cacheUnderRoots(cacheKey, roots)
			return append([]int(nil), roots...)
		}
		roots = vol.underRootIDsByParentScans(candidates, parts)
		vol.cacheUnderRoots(cacheKey, roots)
		return append([]int(nil), roots...)
	}
	for _, part := range parts {
		want := strings.ToLower(part)
		next := make([]int, 0, 4)
		for _, parentID := range candidates {
			for _, childID32 := range vol.childIDsForRecord(parentID) {
				childID := int(childID32)
				if childID < 0 || childID >= recordCount {
					continue
				}
				rec := vol.index.compactRecord(childID)
				if !rec.Deleted && strings.EqualFold(vol.index.compactLowerNameAt(childID), want) {
					next = append(next, childID)
				}
			}
		}
		if len(next) == 0 {
			if roots := vol.underRootIDsByBasename(under); len(roots) > 0 {
				vol.cacheUnderRoots(cacheKey, roots)
				return append([]int(nil), roots...)
			}
			vol.cacheUnderRoots(cacheKey, nil)
			return nil
		}
		candidates = next
	}
	vol.cacheUnderRoots(cacheKey, candidates)
	return append([]int(nil), candidates...)
}

func (vol *serviceVolumeIndex) cacheUnderRoots(key string, roots []int) {
	if key == "" {
		return
	}
	vol.termMu.Lock()
	defer vol.termMu.Unlock()
	if vol.underRootCache == nil {
		vol.underRootCache = make(map[string]postingCacheEntry)
	}
	vol.underRootCache[key] = postingCacheEntry{ids: append([]int(nil), roots...), gen: vol.cacheGeneration()}
}

func (vol *serviceVolumeIndex) underLimitedTermCandidates(roots []int, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CountOnly || pq.Limit <= 0 || len(roots) == 0 || len(pq.Terms) != 1 || len(pq.Exts) > 0 || len(pq.Dirs) > 0 || len(pq.Globs) > 0 || len(pq.Regexps) > 0 || pq.Type != "" || pq.HasModAfter || pq.Exists || pq.CaseSensitive || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	term := pq.Terms[0]
	if term == "" || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	if out, ok := vol.scanUnderRootsTermLimited(roots, term, pq.Limit); ok {
		return out, true
	}
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	for _, rootID := range roots {
		for _, id := range vol.subtreeIDsInOrder(rootID) {
			if len(out) >= pq.Limit {
				sort.Ints(out)
				return out, true
			}
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if strings.Contains(vol.index.compactLowerNameAt(id), term) {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) scanUnderRootsTermLimited(roots []int, term string, limit int) ([]int, bool) {
	if len(roots) == 0 {
		return nil, false
	}
	intervals := make([]interval, 0, len(roots))
	for _, rootID := range roots {
		if rootID < 0 || rootID >= vol.index.compactRecordCount() || rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		intervals = append(intervals, interval{start: int(start), end: int(end)})
	}
	if len(intervals) == 0 {
		return nil, false
	}
	return vol.scanIntervalsTermLimited(intervals, term, limit), true
}

func (vol *serviceVolumeIndex) scanUnderTermLimited(rootID int, term string, limit int) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil, false
	}
	if rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) || len(vol.subtreeOrder) == 0 {
		return nil, false
	}
	start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
	if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
		return nil, false
	}
	return vol.scanIntervalsTermLimited([]interval{{start: int(start), end: int(end)}}, term, limit), true
}

type interval struct {
	start int
	end   int
}

func (vol *serviceVolumeIndex) scanIntervalsTermLimited(intervals []interval, term string, limit int) []int {
	total := 0
	for _, iv := range intervals {
		if iv.end > iv.start {
			total += iv.end - iv.start
		}
	}
	n := total
	if n == 0 {
		return nil
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, n/100_000))
	out := make([]int, 0, limit)
	var mu sync.Mutex
	var found atomic.Int32
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		a := worker * n / workers
		b := (worker + 1) * n / workers
		wg.Add(1)
		go func(a, b int) {
			defer wg.Done()
			local := make([]int, 0, 8)
			for logical := a; logical < b && int(found.Load()) < limit; logical++ {
				pos := intervalPosition(intervals, logical)
				if pos < 0 {
					continue
				}
				id := int(vol.subtreeOrder[pos])
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if rec.Deleted {
					continue
				}
				if strings.Contains(vol.index.compactLowerNameAt(id), term) {
					if found.Add(1) <= int32(limit) {
						local = append(local, id)
					}
				}
			}
			if len(local) > 0 {
				mu.Lock()
				out = append(out, local...)
				mu.Unlock()
			}
		}(a, b)
	}
	wg.Wait()
	sort.Ints(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func intervalPosition(intervals []interval, logical int) int {
	for _, iv := range intervals {
		n := iv.end - iv.start
		if logical < n {
			return iv.start + logical
		}
		logical -= n
	}
	return -1
}

func (vol *serviceVolumeIndex) underRootIDsByBasename(under string) []int {
	base := strings.ToLower(filepath.Base(under))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return nil
	}
	cleanUnder := filepath.Clean(under)
	candidates := vol.exactNameIDs(base)
	out := vol.filterUnderRootCandidates(candidates, base, cleanUnder)
	if len(out) == 0 {
		out = vol.filterUnderRootCandidates(vol.nameTermPosting(base), base, cleanUnder)
	}
	sort.Ints(out)
	return uniqueSortedInts(out)
}

func (vol *serviceVolumeIndex) filterUnderRootCandidates(candidates []int, base, cleanUnder string) []int {
	out := make([]int, 0, 1)
	pathCache := make(map[int]string)
	for _, id := range candidates {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 || vol.index.compactLowerNameAt(id) != base {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, pathCache)
		if strings.EqualFold(filepath.Clean(path), cleanUnder) {
			out = append(out, id)
		}
	}
	return out
}

func (vol *serviceVolumeIndex) underRootIDsByParentScans(candidates []int, parts []string) []int {
	if len(candidates) == 0 {
		return nil
	}
	recordCount := vol.index.compactRecordCount()
	for _, part := range parts {
		want := strings.ToLower(part)
		parentFRNs := make(map[uint64]struct{}, len(candidates))
		for _, id := range candidates {
			if id < 0 || id >= recordCount {
				continue
			}
			frn := vol.index.compactRecord(id).FRN
			if frn != 0 {
				parentFRNs[frn] = struct{}{}
			}
		}
		if len(parentFRNs) == 0 {
			return nil
		}
		next := make([]int, 0, 4)
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if _, ok := parentFRNs[rec.ParentFRN]; !ok {
				continue
			}
			if vol.index.compactLowerNameAt(id) == want {
				next = append(next, id)
			}
		}
		if len(next) == 0 {
			return nil
		}
		candidates = next
	}
	return candidates
}

func (vol *serviceVolumeIndex) isDescendantOrSelf(id, rootID int) bool {
	if vol != nil && id >= 0 && rootID >= 0 && id < len(vol.subtreeStart) && rootID < len(vol.subtreeStart) {
		pos, start, end := vol.subtreeStart[id], vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && pos != ^uint32(0) {
			return pos >= start && pos < end
		}
	}
	seen := make(map[int]struct{}, 16)
	cur := id
	for depth := 0; depth < 1024; depth++ {
		if cur == rootID {
			return true
		}
		if cur < 0 || cur >= vol.index.compactRecordCount() {
			return false
		}
		if _, ok := seen[cur]; ok {
			return false
		}
		seen[cur] = struct{}{}
		parent := vol.index.compactRecord(cur).Parent
		if parent < 0 {
			return false
		}
		cur = int(parent)
	}
	return false
}

func (vol *serviceVolumeIndex) isDescendantOrSelfAnyFast(id int, roots []int) bool {
	if vol == nil || vol.index == nil || id < 0 || len(roots) == 0 {
		return false
	}
	recordCount := vol.index.compactRecordCount()
	cur := id
	for depth := 0; depth < 1024; depth++ {
		if cur < 0 || cur >= recordCount {
			return false
		}
		for _, rootID := range roots {
			if cur == rootID {
				return true
			}
		}
		parent := int(vol.index.compactRecord(cur).Parent)
		if parent < 0 || parent == cur {
			return false
		}
		cur = parent
	}
	return false
}

func (vol *serviceVolumeIndex) underDescendants(rootID int) []int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil
	}
	vol.termMu.Lock()
	if vol.underCache != nil {
		if entry, ok := vol.underCache[rootID]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.isDescendantOrSelf(id, rootID)
				})
			}
		}
	}
	vol.termMu.Unlock()
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, 256)
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			out = make([]int, 0, int(end-start))
			for _, id32 := range vol.subtreeOrder[start:end] {
				id := int(id32)
				if id < 0 || id >= recordCount {
					continue
				}
				if !vol.index.compactRecord(id).Deleted {
					out = append(out, id)
				}
			}
		}
	} else if len(vol.childOffsets) > 0 || vol.children != nil {
		stack := []int{rootID}
		seen := make(map[int]struct{}, 256)
		for len(stack) > 0 {
			last := len(stack) - 1
			id := stack[last]
			stack = stack[:last]
			if _, ok := seen[id]; ok || id < 0 || id >= recordCount {
				continue
			}
			seen[id] = struct{}{}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted {
				out = append(out, id)
			}
			for _, childID := range vol.childIDsForRecord(id) {
				stack = append(stack, int(childID))
			}
		}
	} else {
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if vol.isDescendantOrSelf(id, rootID) {
				out = append(out, id)
			}
		}
	}
	if len(out) == 0 {
		return out
	}
	sort.Ints(out)
	if vol.shouldCachePosting(out) {
		vol.termMu.Lock()
		if vol.underCache == nil {
			vol.underCache = make(map[int]postingCacheEntry)
		}
		vol.underCache[rootID] = postingCacheEntry{ids: out, gen: vol.cacheGeneration()}
		vol.termMu.Unlock()
	}
	return out
}

func (vol *serviceVolumeIndex) underDescendantsLimited(rootID, limit int) []int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil
	}
	if limit <= 0 {
		return vol.underDescendants(rootID)
	}
	recordCount := vol.index.compactRecordCount()
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			out := make([]int, 0, min(limit, int(end-start)))
			for _, id32 := range vol.subtreeOrder[start:end] {
				if len(out) >= limit {
					break
				}
				id := int(id32)
				if id < 0 || id >= recordCount {
					continue
				}
				if !vol.index.compactRecord(id).Deleted {
					out = append(out, id)
				}
			}
			sort.Ints(out)
			return out
		}
	}
	out := make([]int, 0, min(limit, 256))
	stack := []int{rootID}
	seen := make(map[int]struct{}, 256)
	for len(stack) > 0 && len(out) < limit {
		last := len(stack) - 1
		id := stack[last]
		stack = stack[:last]
		if _, ok := seen[id]; ok || id < 0 || id >= recordCount {
			continue
		}
		seen[id] = struct{}{}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted {
			out = append(out, id)
		}
		for _, childID := range vol.childIDsForRecord(id) {
			stack = append(stack, int(childID))
		}
	}
	sort.Ints(out)
	return out
}

func (vol *serviceVolumeIndex) underPrefilter(pq parsedQuery) map[int]struct{} {
	if vol == nil || len(pq.Regexps) > 0 || pq.CaseSensitive || pq.HasModAfter || pq.Exists {
		return nil
	}
	lists := make([][]int, 0, len(pq.Exts)+len(pq.Dirs)+len(pq.Terms)+len(pq.Globs))
	for _, ext := range pq.Exts {
		list := vol.extPosting(ext)
		if len(list) == 0 {
			return map[int]struct{}{}
		}
		lists = append(lists, list)
	}
	globExts, globsOK := simpleGlobExts(pq.Globs)
	if globsOK {
		for _, ext := range globExts {
			list := vol.extPosting(ext)
			if len(list) == 0 {
				return map[int]struct{}{}
			}
			lists = append(lists, list)
		}
	} else {
		for _, ext := range complexGlobExts(pq.Globs) {
			list := vol.extPosting(ext)
			if len(list) == 0 {
				return map[int]struct{}{}
			}
			lists = append(lists, list)
		}
		for _, globTerm := range globLiteralTerms(pq.Globs, pq.CaseSensitive) {
			list := vol.nameTermPosting(globTerm)
			if len(list) == 0 {
				continue
			}
			lists = append(lists, list)
		}
	}
	for _, dir := range pq.Dirs {
		list := vol.pathComponentPosting(dir)
		if len(list) == 0 {
			return map[int]struct{}{}
		}
		lists = append(lists, list)
	}
	hasDottedTerm := false
	for _, term := range pq.Terms {
		if strings.Contains(term, ".") {
			hasDottedTerm = true
			break
		}
	}
	for _, term := range pq.Terms {
		if pq.MatchPath && hasDottedTerm && !strings.Contains(term, ".") {
			continue
		}
		list := []int(nil)
		if ext, ok := dottedExtensionTerm(term); ok {
			list = vol.extPosting(ext)
		} else if strings.Contains(term, ".") {
			list = vol.exactNameIDs(term)
		}
		if len(list) == 0 {
			list = vol.nameTermPosting(term)
		}
		if pq.MatchPath && len(list) == 0 {
			list = vol.pathTermPosting(term)
		}
		if len(list) == 0 {
			return map[int]struct{}{}
		}
		lists = append(lists, list)
	}
	if len(lists) == 0 {
		return nil
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	out := make(map[int]struct{}, len(candidates))
	for _, id := range candidates {
		out[id] = struct{}{}
	}
	return out
}
