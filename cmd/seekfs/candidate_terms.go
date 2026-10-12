package main

import (
	"container/heap"
	"os"
	"sort"
	"strings"
)

func compactUint32OrderLen(order []uint32, recordCount int) int {
	if len(order) == 0 {
		return recordCount
	}
	return len(order)
}

func compactUint32OrderAt(order []uint32, pos int) int {
	if len(order) == 0 {
		return pos
	}
	return int(order[pos])
}

// capBroadCandidates trims a fully-path-verified, name-order-sorted candidate
// list to just enough entries to satisfy a search limit. The broad scan has
// already confirmed the path predicate, so when the query carries no record-level
// filter that could still reject a candidate (type/ext/glob/size/dm/NOT), the
// downstream loop will accept candidates in order until it hits the limit;
// returning more than that is wasted reconstruction. For count queries, or when
// a rejecting filter is present, the full set is returned so the count/limit
// remains exact.
func capBroadCandidates(ids []int, pq parsedQuery) []int {
	if pq.CountOnly || pq.Limit <= 0 {
		return ids
	}
	if pq.RootBias != "" || pq.CWDBias != "" {
		// Bias re-ranks downstream; capping by name order would drop preferred
		// results before they can be promoted.
		return ids
	}
	if pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists {
		return ids
	}
	if len(ids) <= pq.Limit {
		return ids
	}
	return ids[:pq.Limit]
}

func recordCountWorkers(recordCount int) int {
	return max(1, recordCount/250_000)
}

// hasNonVolumeTerm reports whether terms contains at least one plain term that
// is not a bare volume/drive token.
func hasNonVolumeTerm(terms []string) bool {
	for _, term := range terms {
		if !isVolumeQueryTerm(term) {
			return true
		}
	}
	return false
}

func pathPlanProbeTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		if term == "" || isVolumeQueryTerm(term) {
			continue
		}
		out = append(out, term)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		aDot, bDot := strings.Contains(a, "."), strings.Contains(b, ".")
		if aDot != bDot {
			return aDot
		}
		if len(a) != len(b) {
			return len(a) > len(b)
		}
		return a < b
	})
	return out
}

func (vol *serviceVolumeIndex) namePlanTermPosting(term string) []int {
	if strings.Contains(term, ".") {
		if exact := vol.exactNameIDs(term); len(exact) > 0 {
			return exact
		}
	}
	return vol.nameTermPosting(term)
}

func (vol *serviceVolumeIndex) pathPlanTermPosting(term string) []int {
	if strings.Contains(term, ".") {
		if exact := vol.exactNameIDs(term); len(exact) > 0 {
			out := make([]int, 0, len(exact))
			for _, id := range exact {
				if id < 0 || vol == nil || vol.index == nil || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if rec.Deleted {
					continue
				}
				if rec.Mode&uint32(os.ModeDir) == 0 {
					out = append(out, id)
					continue
				}
				out = append(out, vol.underDescendants(id)...)
			}
			sort.Ints(out)
			return uniqueSortedInts(out)
		}
	}
	return vol.pathTermPosting(term)
}

func (vol *serviceVolumeIndex) parentIDs(parent string) []int {
	if vol == nil || vol.index == nil || parent == "" || strings.ContainsAny(parent, `\/:*?[]`) {
		return nil
	}
	roots := vol.pathComponentRootIDs(parent)
	if len(roots) == 0 {
		return nil
	}
	out := make([]int, 0, 64)
	for _, root := range roots {
		if root < 0 || root >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(root)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 || !strings.EqualFold(vol.index.compactLowerNameAt(root), parent) {
			continue
		}
		for _, childID := range vol.childIDsForRecord(root) {
			id := int(childID)
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			if !vol.index.compactRecord(id).Deleted {
				out = append(out, id)
			}
		}
	}
	sort.Ints(out)
	return uniqueSortedInts(out)
}

func (vol *serviceVolumeIndex) attrIDsForMask(mask uint32) ([]int, bool) {
	if vol == nil || vol.queryIndex == nil || vol.queryIndex.attrBits == nil || mask == 0 {
		return nil, false
	}
	var ids []uint32
	haveIDs := false
	for _, bit := range queryAttrBits() {
		if mask&bit != bit {
			continue
		}
		bitIDs := vol.queryIndex.attrBits[bit]
		if len(bitIDs) == 0 {
			return []int{}, true
		}
		if !haveIDs {
			ids = append([]uint32(nil), bitIDs...)
			haveIDs = true
			continue
		}
		ids = intersectSortedUint32s(ids, bitIDs)
		if len(ids) == 0 {
			break
		}
	}
	if !haveIDs {
		return nil, false
	}
	return uint32sToInts(ids), true
}

func attribMaskString(mask uint32) string {
	var b strings.Builder
	for _, item := range []struct {
		bit uint32
		ch  byte
	}{
		{fileAttributeReadonly, 'R'},
		{fileAttributeHidden, 'H'},
		{fileAttributeSystem, 'S'},
		{fileAttributeDir, 'D'},
		{fileAttributeArchive, 'A'},
	} {
		if mask&item.bit == item.bit {
			b.WriteByte(item.ch)
		}
	}
	if b.Len() == 0 {
		return "0"
	}
	return b.String()
}

func (vol *serviceVolumeIndex) limitedPathTermCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || countNonVolumeTerms(pq.Terms) != 1 || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		candidates, ok := vol.pathPlanTermPostingLimited(term, pq)
		if !ok {
			return nil, false
		}
		sortCandidateIDs(candidates, pq, vol.index, vol.rankForQuery(pq))
		return candidates, true
	}
	return nil, false
}

func (vol *serviceVolumeIndex) limitedDottedPathScanCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || pq.Type != "" || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" ||
		len(pq.Exts) > 0 || len(pq.Dirs) > 0 || len(pq.Globs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		countNonVolumeTerms(pq.Terms) != 1 || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		if !strings.Contains(term, ".") || strings.ContainsAny(term, `\/*?[]:`) {
			return nil, false
		}
		out := vol.scanPathTermPrefixLimited(pq, term, pq.Limit, 16_384)
		if len(out) >= pq.Limit {
			return out, true
		}
		out = vol.scanPathTermLimited(pq, term, pq.Limit)
		if len(out) >= pq.Limit {
			return out, true
		}
		return nil, false
	}
	return nil, false
}

func (vol *serviceVolumeIndex) pathDirectoryTermTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !vol.hasDescendantIndex() ||
		!pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || pq.Type != "" || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" ||
		len(pq.Exts) != 1 || len(pq.Dirs) > 0 || len(pq.Globs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		countNonVolumeTerms(pq.Terms) != 1 || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	term := ""
	for _, candidate := range pq.Terms {
		if !isVolumeQueryTerm(candidate) {
			term = candidate
			break
		}
	}
	if len(term) < 3 || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	if vol.pathTermIsUsableExtensionCandidate(term) {
		return nil, false
	}
	nameMatches, roots, complete := vol.pathDirectoryTermSource(term)
	if complete && len(nameMatches) == 0 {
		return []int{}, true
	}
	if len(roots) == 0 {
		return nil, false
	}
	recordCount := vol.index.compactRecordCount()
	rankOf := candidateRanker(vol.index, vol.rankForQuery(pq))
	seen := make(map[int]struct{}, pq.Limit*4)
	h := make(candidateRankMaxHeap, 0, pq.Limit)
	add := func(id int) {
		if id < 0 || id >= recordCount {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !compactRecordPrecheck(rec, pq, true) || !vol.index.compactPathContainsTerm(id, term) {
			return
		}
		seen[id] = struct{}{}
		item := candidateRankItem{id: id, rank: rankOf(id)}
		if len(h) < pq.Limit {
			heap.Push(&h, item)
			return
		}
		if item.rank < h[0].rank {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	for _, id := range nameMatches {
		add(id)
	}
	if ext := pq.Exts[0]; ext != "" {
		if ids, ok := vol.extTopPosting(ext, max(pq.Limit*8, pq.Limit), pq); ok {
			for _, id := range ids {
				add(id)
			}
		}
	}
	scanned := 0
	for rootIndex, rootID := range roots {
		if rootIndex&127 == 0 && queryCanceled(pq) {
			return nil, false
		}
		if len(vol.subtreeOrder) > 0 && rootID >= 0 && rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) {
			start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
			if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
				for pos := start; pos < end; pos++ {
					if scanned >= serviceComponentMultiTermScanMaxIDs {
						return heapIDsByRank(h, rankOf), len(h) > 0
					}
					scanned++
					if pos&4095 == 0 && queryCanceled(pq) {
						return nil, false
					}
					add(int(vol.subtreeOrder[pos]))
				}
				continue
			}
		}
		for _, childID := range vol.underDescendantsLimited(rootID, serviceComponentMultiTermScanMaxIDs-scanned+1) {
			if scanned >= serviceComponentMultiTermScanMaxIDs {
				return heapIDsByRank(h, rankOf), len(h) > 0
			}
			scanned++
			add(int(childID))
		}
	}
	out := heapIDsByRank(h, rankOf)
	return out, len(out) > 0
}

func (vol *serviceVolumeIndex) pathDirectoryTermRoots(term string) ([]int, bool) {
	_, roots, complete := vol.pathDirectoryTermSource(term)
	return roots, complete
}

func (vol *serviceVolumeIndex) pathDirectoryTermSource(term string) (nameMatches []int, roots []int, complete bool) {
	if vol == nil || vol.index == nil || len(term) < 3 || isVolumeQueryTerm(term) ||
		strings.ContainsAny(term, `\/*?[]:`) {
		return nil, nil, false
	}
	// A term whose required filename grams are proven absent cannot match any
	// directory: report a complete empty source instead of running an expensive
	// trigram intersect that will only decline.
	if _, _, exactZero, completeGrams := completeSelfNameGramIterators(vol.index, term); completeGrams && exactZero {
		return nil, nil, true
	}
	nameMatches, ok := vol.completeNameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
		// The trigram posting is unavailable (omitted-common gram, incomplete
		// metadata).  Use the bounded name scan instead of pathComponentRootIDs,
		// which on a large volume without a resident name order falls back to a
		// full-record scan just to answer a membership question.
		if scanned := vol.scanNameTermBounded(term, servicePathNameTrigramCandidateMaxIDs); scanned != nil {
			if len(scanned) == 0 {
				return nil, nil, true
			}
			return scanned, nil, true
		}
		roots = vol.pathComponentRootIDs(term)
		return nil, roots, false
	}
	seen := make(map[int]struct{}, len(nameMatches))
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if !strings.Contains(vol.index.compactLowerNameAt(id), term) {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		roots = append(roots, id)
	}
	if len(roots) == 0 {
		roots = vol.pathComponentRootIDs(term)
	}
	sortCandidateIDs(roots, parsedQuery{}, vol.index, vol.nameOrderRanks())
	return nameMatches, roots, true
}
