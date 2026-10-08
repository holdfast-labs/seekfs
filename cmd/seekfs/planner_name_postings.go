package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unsafe"
)

func (vol *serviceVolumeIndex) pathDirFilterCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || len(pq.Terms) == 0 || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive {
		return nil, false
	}
	if len(pq.Exts) == 0 && len(pq.Dirs) == 0 && len(pq.Globs) == 0 && pq.Type == "" && !pq.HasModAfter {
		return nil, false
	}
	type rootTerm struct {
		id   int
		term string
	}
	roots := make([]rootTerm, 0, 4)
	for _, term := range pq.Terms {
		if strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		for _, id := range vol.exactNameIDs(term) {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
				roots = append(roots, rootTerm{id: id, term: term})
			}
		}
	}
	if len(roots) == 0 {
		return nil, false
	}
	prefilter := vol.underPrefilter(pq)
	if prefilter == nil {
		return nil, false
	}
	out := make([]int, 0, 64)
	seen := make(map[int]struct{}, 64)
	for _, root := range roots {
		for id := range prefilter {
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() || !vol.isDescendantOrSelf(id, root.id) {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted || !compactRecordPrecheck(rec, pq, true) || !vol.recordPathContainsRemainingTerms(id, pq.Terms, root.term) {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) recordPathContainsRemainingTerms(id int, terms []string, rootTerm string) bool {
	for _, term := range terms {
		if term == rootTerm {
			continue
		}
		if !vol.index.compactPathContainsTerm(id, term) {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) filterCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive {
		return nil, false
	}
	if len(pq.Exts) == 0 && len(pq.Dirs) == 0 {
		return nil, false
	}
	lists := make([][]int, 0, len(pq.Exts)+len(pq.Dirs)+len(pq.Terms))
	for _, ext := range pq.Exts {
		list := vol.extPosting(ext)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	for _, dir := range pq.Dirs {
		list := vol.pathComponentPosting(dir)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	for _, term := range pq.Terms {
		list := vol.nameTermPosting(term)
		if pq.MatchPath {
			list = vol.pathTermPosting(term)
		}
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	if len(lists) == 0 {
		return nil, false
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

func (vol *serviceVolumeIndex) regexLiteralCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || len(pq.Regexps) == 0 || len(pq.RegexTerms) != 1 || pq.CaseSensitive {
		return nil, false
	}
	lists := make([][]int, 0, len(pq.RegexTerms))
	for _, term := range pq.RegexTerms {
		list := vol.pathTermPosting(term)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
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

func (vol *serviceVolumeIndex) pathRootLimitedCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || len(pq.Terms) < 2 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive || pq.Limit <= 0 || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	type rootTerm struct {
		id   int
		term string
	}
	roots := make([]rootTerm, 0, 4)
	for _, term := range pq.Terms {
		if strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		for _, id := range vol.pathComponentRootIDs(term) {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
				roots = append(roots, rootTerm{id: id, term: term})
			}
		}
	}
	if len(roots) == 0 {
		return nil, false
	}
	out := make([]int, 0, pq.Limit)
	seen := make(map[int]struct{}, pq.Limit)
	for _, root := range roots {
		for _, id := range vol.subtreeIDsInOrder(root.id) {
			if len(out) >= pq.Limit {
				sort.Ints(out)
				return out, true
			}
			if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted || !compactRecordPrecheck(rec, pq, true) || !vol.recordPathContainsRemainingTerms(id, pq.Terms, root.term) {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) subtreeIDsInOrder(rootID int) []int {
	if vol == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return nil
	}
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			out := make([]int, 0, int(end-start))
			for _, id32 := range vol.subtreeOrder[start:end] {
				out = append(out, int(id32))
			}
			return out
		}
	}
	return vol.underDescendants(rootID)
}

func (vol *serviceVolumeIndex) exactDirCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Terms) != 1 || pq.Type != "dir" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 || pq.HasModAfter || pq.Exists || pq.CaseSensitive {
		return nil, false
	}
	list := vol.exactNameIDs(pq.Terms[0])
	out := make([]int, 0, len(list))
	for _, id := range list {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
			out = append(out, id)
		}
	}
	return out, true
}

func (vol *serviceVolumeIndex) pathTermSubtreeCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || len(pq.Terms) < 2 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || pq.CaseSensitive {
		return nil, false
	}
	lists := make([][]int, 0, len(pq.Terms))
	for _, term := range pq.Terms {
		list := vol.pathPlanTermPosting(term)
		if len(list) == 0 {
			return []int{}, true
		}
		lists = append(lists, list)
	}
	sortIntListsByLen(lists)
	candidates := append([]int(nil), lists[0]...)
	for _, list := range lists[1:] {
		candidates = intersectSortedInts(candidates, list)
		if len(candidates) == 0 {
			break
		}
	}
	if len(candidates) > 4096 {
		if nameList, ok := vol.unionNamePostings(pq.Terms); ok {
			candidates = intersectSortedInts(candidates, nameList)
		}
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) unionNamePostings(terms []string) ([]int, bool) {
	seen := make(map[int]struct{}, 64)
	out := make([]int, 0, 64)
	for _, term := range terms {
		for _, id := range vol.nameTermPosting(term) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) exactNameCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Terms) != 1 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 || pq.Type != "" || pq.HasModAfter || pq.Exists || pq.CaseSensitive {
		return nil, false
	}
	term := pq.Terms[0]
	if !strings.Contains(term, ".") {
		return nil, false
	}
	list := vol.exactNameIDs(term)
	out := make([]int, 0, len(list))
	for _, id := range list {
		if id >= 0 && id < vol.index.compactRecordCount() && !vol.index.compactRecord(id).Deleted {
			out = append(out, id)
		}
	}
	return out, len(out) > 0
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA == nil {
		a = aa
	}
	if errB == nil {
		b = bb
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func (vol *serviceVolumeIndex) namePrefixCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || len(pq.Terms) != 1 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 || pq.Type != "" || pq.HasModAfter || pq.Exists || pq.CaseSensitive {
		return nil, false
	}
	term := pq.Terms[0]
	if len(term) < 8 || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	var order []uint32
	if vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0 {
		order = vol.queryIndex.nameOrder
	} else if len(vol.index.CompactNameOrder) > 0 {
		order = make([]uint32, len(vol.index.CompactNameOrder))
		for i, id := range vol.index.CompactNameOrder {
			order[i] = uint32(id)
		}
	}
	if len(order) == 0 {
		return nil, false
	}
	start := sort.Search(len(order), func(i int) bool {
		return vol.index.compactLowerNameAt(int(order[i])) >= term
	})
	out := make([]int, 0, 8)
	seen := make(map[int]struct{})
	for i := start; i < len(order); i++ {
		id := int(order[i])
		rec := vol.index.compactRecord(id)
		if !strings.HasPrefix(vol.index.compactLowerNameAt(id), term) {
			break
		}
		if !rec.Deleted {
			out = append(out, id)
			seen[id] = struct{}{}
		}
	}
	for id := range vol.recentIDs {
		if _, ok := seen[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && strings.HasPrefix(vol.index.compactLowerNameAt(id), term) {
			out = append(out, id)
		}
	}
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) exactNameIDs(name string) []int {
	if vol == nil || vol.index == nil || name == "" {
		return nil
	}
	if vol.exactNames != nil {
		return append([]int(nil), vol.exactNames[name]...)
	}
	cacheKey := "\x00exact:" + name
	vol.termMu.Lock()
	if vol.termCache != nil {
		if entry, ok := vol.termCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.index.compactLowerNameAt(id) == name
				})
			}
		}
	}
	vol.termMu.Unlock()
	if vol.queryIndex == nil || len(vol.queryIndex.nameOrder) == 0 {
		out := vol.scanExactNameIDs(name)
		vol.cacheNamePosting(cacheKey, out)
		return out
	}
	order := vol.queryIndex.nameOrder
	start := sort.Search(len(order), func(i int) bool {
		return vol.index.compactLowerNameAt(int(order[i])) >= name
	})
	if start >= len(order) || vol.index.compactLowerNameAt(int(order[start])) != name {
		return nil
	}
	out := make([]int, 0, 4)
	for i := start; i < len(order); i++ {
		id := int(order[i])
		if vol.index.compactLowerNameAt(id) != name {
			break
		}
		out = append(out, id)
	}
	for id := range vol.recentIDs {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	out = uniqueSortedInts(out)
	return out
}

func (vol *serviceVolumeIndex) scanExactNameIDs(name string) []int {
	if ext := strings.TrimPrefix(filepath.Ext(name), "."); ext != "" && vol.queryIndex != nil {
		if extIDs := vol.extPosting32(ext); len(extIDs) > 0 {
			out := make([]int, 0, 4)
			for _, id32 := range extIDs {
				id := int(id32)
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
					out = append(out, id)
				}
			}
			for id := range vol.recentIDs {
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
					out = append(out, id)
				}
			}
			sort.Ints(out)
			return uniqueSortedInts(out)
		}
	}
	out := make([]int, 0, 4)
	recordCount := vol.index.compactRecordCount()
	for id := 0; id < recordCount; id++ {
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && vol.index.compactLowerNameAt(id) == name {
			out = append(out, id)
		}
	}
	return out
}

func (vol *serviceVolumeIndex) nameTermPosting(term string) []int {
	vol.termMu.Lock()
	if vol.termCache == nil {
		vol.termCache = make(map[string]postingCacheEntry)
	}
	if entry, ok := vol.termCache[term]; ok {
		if vol.cacheStampValid(entry.gen) {
			vol.termMu.Unlock()
			return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
				id, ok := vol.idForFRN(rec.FRN)
				return ok && strings.Contains(vol.index.compactLowerNameAt(id), term)
			})
		}
	}
	vol.termMu.Unlock()
	list := vol.scanNameTermPosting(term)
	vol.cacheNamePosting(term, list)
	return list
}

func postingListCacheMaxBytes() int64 {
	maxBytes := postingBlockCacheMaxBytes()
	if maxBytes <= 0 {
		return 0
	}
	return maxBytes / 4
}

func postingListBytes(list []int) int64 {
	return int64(len(list)) * int64(unsafe.Sizeof(int(0)))
}

func (vol *serviceVolumeIndex) shouldCachePosting(list []int) bool {
	maxBytes := postingListCacheMaxBytes()
	return maxBytes > 0 && postingListBytes(list) <= maxBytes
}

func postingEntryCacheBytes(cache map[string]postingCacheEntry) int64 {
	var total int64
	for _, entry := range cache {
		total += postingListBytes(entry.ids)
	}
	return total
}

func postingRootCacheBytes(cache map[int]postingCacheEntry) int64 {
	var total int64
	for _, entry := range cache {
		total += postingListBytes(entry.ids)
	}
	return total
}

func (vol *serviceVolumeIndex) postingListCacheBytesLocked() int64 {
	if vol == nil {
		return 0
	}
	return postingEntryCacheBytes(vol.termCache) +
		postingEntryCacheBytes(vol.pathTermCache) +
		postingEntryCacheBytes(vol.extCache) +
		postingEntryCacheBytes(vol.underRootCache) +
		postingRootCacheBytes(vol.underCache)
}

func (vol *serviceVolumeIndex) cacheNamePosting(term string, list []int) {
	if !vol.shouldCachePosting(list) {
		return
	}
	vol.termMu.Lock()
	if vol.termCache == nil {
		vol.termCache = make(map[string]postingCacheEntry)
	}
	vol.termCache[term] = postingCacheEntry{ids: list, gen: vol.cacheGeneration()}
	vol.termMu.Unlock()
}

func (vol *serviceVolumeIndex) cachePathPosting(term string, list []int) {
	if !vol.shouldCachePosting(list) {
		return
	}
	vol.termMu.Lock()
	if vol.pathTermCache == nil {
		vol.pathTermCache = make(map[string]postingCacheEntry)
	}
	vol.pathTermCache[term] = postingCacheEntry{ids: list, gen: vol.cacheGeneration()}
	vol.termMu.Unlock()
}

func (vol *serviceVolumeIndex) cacheExtPosting(ext string, list []int) {
	if !vol.shouldCachePosting(list) {
		return
	}
	vol.termMu.Lock()
	if vol.extCache == nil {
		vol.extCache = make(map[string]postingCacheEntry)
	}
	vol.extCache[ext] = postingCacheEntry{ids: list, gen: vol.cacheGeneration()}
	vol.termMu.Unlock()
}

func (vol *serviceVolumeIndex) scanNameTermPosting(term string) []int {
	if vol != nil && vol.index != nil {
		if ids, ok := vol.index.scanCompactLowerNameTerm(term); ok {
			return ids
		}
	}
	recordCount := vol.index.compactRecordCount()
	workers := min(runtime.GOMAXPROCS(0), max(1, recordCount/250_000))
	if workers <= 1 {
		out := make([]int, 0, 64)
		for i := 0; i < recordCount; i++ {
			rec := vol.index.compactRecord(i)
			if rec.Deleted {
				continue
			}
			if strings.Contains(vol.index.compactLowerNameAt(i), term) {
				out = append(out, i)
			}
		}
		return out
	}
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * recordCount / workers
		end := (worker + 1) * recordCount / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := make([]int, 0, 64)
			for i := start; i < end; i++ {
				rec := vol.index.compactRecord(i)
				if rec.Deleted {
					continue
				}
				if strings.Contains(vol.index.compactLowerNameAt(i), term) {
					local = append(local, i)
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]int, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// countBareTermParallel counts records whose lower name contains the term, in
// parallel, without materializing the matching ID slice.  It returns ok=false
// when a mapped bulk scan is available so the caller keeps using the
// slice-based posting (which is cached and reused by search), and only pays the
// count-only walk when a full materialization would be required anyway.
func (vol *serviceVolumeIndex) countBareTermParallel(term string, hidden hiddenBaseIDs, pq parsedQuery) (int, bool) {
	if vol == nil || vol.index == nil || term == "" {
		return 0, false
	}
	if vol.index.MMapRecords != nil {
		// The mapped fast path returns the slice cheaply; prefer it so search
		// and count share the same cached posting.
		if _, ok := vol.index.scanCompactLowerNameTerm(term); ok {
			return 0, false
		}
	}
	recordCount := vol.index.compactRecordCount()
	workers := min(runtime.GOMAXPROCS(0), max(1, recordCount/250_000))
	count := 0
	if workers <= 1 {
		for i := 0; i < recordCount; i++ {
			if i&1023 == 0 && queryCanceled(pq) {
				return 0, false
			}
			rec := vol.index.compactRecord(i)
			if rec.Deleted {
				continue
			}
			if !hidden.empty() && hidden.contains(i) {
				continue
			}
			if strings.Contains(vol.index.compactLowerNameAt(i), term) {
				count++
			}
		}
		return count, true
	}
	parts := make([]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * recordCount / workers
		end := (worker + 1) * recordCount / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := 0
			for i := start; i < end; i++ {
				if i&1023 == 0 && queryCanceled(pq) {
					return
				}
				rec := vol.index.compactRecord(i)
				if rec.Deleted {
					continue
				}
				if !hidden.empty() && hidden.contains(i) {
					continue
				}
				if strings.Contains(vol.index.compactLowerNameAt(i), term) {
					local++
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	if queryCanceled(pq) {
		return 0, false
	}
	for _, part := range parts {
		count += part
	}
	return count, true
}

// scanCompactLowerNameTerm is the complete mapped-name fallback used when a
// persisted trigram posting is selective/incomplete.  The normal compact
// accessors intentionally reconstruct a CompactRecord on every visit; that
// is needlessly expensive for this exact, read-only name-table scan.  Read
// the lower-name token and deleted bit directly from the mapped record table
// while preserving the same record/substring semantics.
func (idx *Index) scanCompactLowerNameTerm(term string) ([]int, bool) {
	if idx == nil || term == "" {
		return nil, false
	}
	if m := idx.MMapRecords; m != nil {
		derived := m.fileDerived()
		if len(derived.LowerOffs) == 0 || len(derived.LowerLens) != len(derived.LowerOffs) {
			return nil, false
		}
		size := compactDiskRecordBytes
		if m.wideRefs {
			size = compactWideDiskRecordBytes
		}
		if size <= 0 || len(m.recordData) < m.count*size {
			return nil, false
		}
		termBytes := []byte(term)
		nameBytesForToken := func(token uint32) []byte {
			if token >= uint32(len(derived.LowerOffs)) {
				return nil
			}
			off := derived.LowerOffs[token]
			var nameBytes []byte
			if off == packedLowerSameAsName {
				nameOff := int(token) * 6
				if nameOff >= 0 && nameOff+6 <= len(m.tokenTable) {
					nameStart := binary.LittleEndian.Uint32(m.tokenTable[nameOff:])
					nameLen := binary.LittleEndian.Uint16(m.tokenTable[nameOff+4:])
					nameEnd := int(nameStart) + int(nameLen)
					if nameEnd >= int(nameStart) && nameEnd <= len(m.nameBlob) {
						nameBytes = m.nameBlob[int(nameStart):nameEnd]
					}
				}
			} else {
				end := int(off) + int(derived.LowerLens[token])
				if end >= int(off) && end <= len(derived.LowerBlob) {
					nameBytes = derived.LowerBlob[int(off):end]
				}
			}
			return nameBytes
		}
		tokenMatches := make([]bool, len(derived.LowerOffs))
		matchWorkers := min(runtime.GOMAXPROCS(0), max(1, len(derived.LowerOffs)/250_000))
		var matchWG sync.WaitGroup
		for worker := 0; worker < matchWorkers; worker++ {
			start := worker * len(derived.LowerOffs) / matchWorkers
			end := (worker + 1) * len(derived.LowerOffs) / matchWorkers
			matchWG.Add(1)
			go func(start, end int) {
				defer matchWG.Done()
				for nameID := start; nameID < end; nameID++ {
					tokenMatches[nameID] = bytes.Contains(nameBytesForToken(uint32(nameID)), termBytes)
				}
			}(start, end)
		}
		matchWG.Wait()
		workers := min(runtime.GOMAXPROCS(0), max(1, m.count/250_000))
		parts := make([][]int, workers)
		var wg sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			start := worker * m.count / workers
			end := (worker + 1) * m.count / workers
			wg.Add(1)
			go func(worker, start, end int) {
				defer wg.Done()
				local := make([]int, 0, 64)
				for i := start; i < end; i++ {
					base := i * size
					if m.recordData[base+size-1] != 0 {
						continue
					}
					_, nameID := m.recordRefs(base + 16)
					if nameID >= uint32(len(derived.LowerOffs)) {
						continue
					}
					if tokenMatches[nameID] {
						local = append(local, i)
					}
				}
				parts[worker] = local
			}(worker, start, end)
		}
		wg.Wait()
		total := 0
		for _, part := range parts {
			total += len(part)
		}
		out := make([]int, 0, total)
		for _, part := range parts {
			out = append(out, part...)
		}
		return out, true
	}
	if p := idx.PackedRecords; p != nil && len(p.LowerOffs) == p.Len() {
		workers := min(runtime.GOMAXPROCS(0), max(1, p.Len()/250_000))
		parts := make([][]int, workers)
		var wg sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			start := worker * p.Len() / workers
			end := (worker + 1) * p.Len() / workers
			wg.Add(1)
			go func(worker, start, end int) {
				defer wg.Done()
				local := make([]int, 0, 64)
				for i := start; i < end; i++ {
					rec := p.At(i)
					if rec.Deleted || !strings.Contains(p.lowerNameAt(i), term) {
						continue
					}
					local = append(local, i)
				}
				parts[worker] = local
			}(worker, start, end)
		}
		wg.Wait()
		total := 0
		for _, part := range parts {
			total += len(part)
		}
		out := make([]int, 0, total)
		for _, part := range parts {
			out = append(out, part...)
		}
		return out, true
	}
	return nil, false
}

func (vol *serviceVolumeIndex) pathTermPosting(term string) []int {
	vol.termMu.Lock()
	if vol.pathTermCache == nil {
		vol.pathTermCache = make(map[string]postingCacheEntry)
	}
	if entry, ok := vol.pathTermCache[term]; ok {
		if vol.cacheStampValid(entry.gen) {
			vol.termMu.Unlock()
			return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
				id, ok := vol.idForFRN(rec.FRN)
				return ok && vol.index.compactPathContainsTerm(id, term)
			})
		}
	}
	vol.termMu.Unlock()

	seen := make(map[int]struct{}, 64)
	out := make([]int, 0, 64)
	for _, id := range vol.nameTermPosting(term) {
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if !strings.ContainsAny(term, `\/*?[]:`) && (len(vol.childOffsets) > 0 || vol.children != nil) {
		traversed := make(map[int]struct{}, 64)
		for _, rootID := range vol.pathTermRootIDs(term) {
			if rootID < 0 || rootID >= vol.index.compactRecordCount() {
				continue
			}
			root := vol.index.compactRecord(rootID)
			if root.Deleted || root.Mode&uint32(os.ModeDir) == 0 {
				continue
			}
			stack := []int{rootID}
			for len(stack) > 0 {
				last := len(stack) - 1
				id := stack[last]
				stack = stack[:last]
				if _, ok := traversed[id]; ok || id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				traversed[id] = struct{}{}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted {
					if _, ok := seen[id]; !ok {
						seen[id] = struct{}{}
						out = append(out, id)
					}
				}
				for _, childID := range vol.childIDsForRecord(id) {
					stack = append(stack, int(childID))
				}
			}
		}
	} else if !strings.ContainsAny(term, `\/*?[]:`) {
		for _, id := range vol.scanPathTermPosting(term) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	vol.cachePathPosting(term, out)
	return out
}
