package main

import (
	"cmp"
	"container/heap"
	"sort"
	"strings"
)

func (plan candidatePlan) execute() []int {
	if plan.empty {
		return []int{}
	}
	sortCandidatePlanSourcesByLen(plan.sources)
	out := plan.sources[0].materialize()
	for _, source := range plan.sources[1:] {
		out = source.intersect(out)
		if len(out) == 0 {
			break
		}
	}
	if plan.vol != nil && len(plan.vol.recentIDs) > 0 {
		out = append(out, mapKeys(plan.vol.recentIDs)...)
		sort.Ints(out)
		out = uniqueSortedInts(out)
	}
	if plan.underPathFallback != "" {
		out = plan.filterUnderPath(out)
	}
	return out
}

type localVerifiedTopItem struct {
	id    int
	entry Entry
}

type localVerifiedTopHeap struct {
	items []localVerifiedTopItem
	pq    parsedQuery
}

func (h localVerifiedTopHeap) Len() int { return len(h.items) }

func (h localVerifiedTopHeap) Less(i, j int) bool {
	if n := compareSearchAllEntries(h.items[i].entry, h.items[j].entry, h.pq); n != 0 {
		return n > 0
	}
	return h.items[i].id > h.items[j].id
}

func (h localVerifiedTopHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *localVerifiedTopHeap) Push(x any) { h.items = append(h.items, x.(localVerifiedTopItem)) }

func (h *localVerifiedTopHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

// executeTop is the bounded local equivalent of the global boolean iterator.
// It only handles a single OR union made from already sorted posting slices;
// all other plans retain the exact materializing/fallback path.
func (plan candidatePlan) executeTop(pq parsedQuery) ([]int, int, bool) {
	if plan.vol == nil || plan.vol.index == nil || pq.CountOnly || pq.Limit <= 0 ||
		len(plan.sources) != 1 || len(plan.sources[0].union) == 0 ||
		len(plan.vol.recentIDs) != 0 || plan.underPathFallback != "" ||
		pq.RootBias != "" || pq.CWDBias != "" || pq.hiddenBlocksTruncation() {
		return nil, 0, false
	}
	parts := plan.sources[0].union
	for _, part := range parts {
		if part.hasPosting || len(part.union) != 0 || len(part.roots) != 0 {
			return nil, 0, false
		}
	}
	h := &localVerifiedTopHeap{pq: pq}
	heap.Init(h)
	orderedParts := make([][]int, len(parts))
	rankRanks := plan.vol.rankForQuery(pq)
	rankOf := candidateRanker(plan.vol.index, rankRanks)
	for i, part := range parts {
		// A posting is a candidate source, not necessarily an exact predicate
		// source (name trigrams in particular can contain false positives). Keep
		// the complete ordered stream and stop only after the next rank cannot
		// beat the current verified threshold. Truncating before verification can
		// hide a real top-N match behind false positives.
		orderedParts[i] = append([]int(nil), part.ids...)
		sortCandidateIDs(orderedParts[i], pq, plan.vol.index, rankRanks)
	}
	positions := make([]int, len(orderedParts))
	pathCache := make(map[int]string)
	last, haveLast := 0, false
	scanned := 0
	for {
		best := -1
		for i, ids := range orderedParts {
			if positions[i] >= len(ids) {
				continue
			}
			id := ids[positions[i]]
			if best < 0 || compareCandidateRank(id, orderedParts[best][positions[best]], rankOf) < 0 {
				best = i
			}
		}
		if best < 0 {
			break
		}
		id := orderedParts[best][positions[best]]
		if h.Len() == pq.Limit && compareCandidateRank(id, h.items[0].id, rankOf) > 0 {
			break
		}
		positions[best]++
		if haveLast && id == last {
			continue
		}
		last, haveLast = id, true
		scanned++
		if scanned&1023 == 0 && queryCanceled(pq) {
			return nil, scanned, false
		}
		entry, ok := compactCandidateEntryIfMatch(plan.vol.index, pq, id, pathCache, true, false)
		if !ok {
			continue
		}
		item := localVerifiedTopItem{id: id, entry: entry}
		if h.Len() < pq.Limit {
			heap.Push(h, item)
			continue
		}
		if compareSearchAllEntries(item.entry, h.items[0].entry, pq) < 0 ||
			(compareSearchAllEntries(item.entry, h.items[0].entry, pq) == 0 && item.id < h.items[0].id) {
			h.items[0] = item
			heap.Fix(h, 0)
		}
	}
	out := make([]int, len(h.items))
	for i, item := range h.items {
		out[i] = item.id
	}
	sortCandidateIDs(out, pq, plan.vol.index, plan.vol.rankForQuery(pq))
	return out, scanned, true
}

func compareCandidateRank(a, b int, rankOf func(int) int) int {
	if n := cmp.Compare(rankOf(a), rankOf(b)); n != 0 {
		return n
	}
	return cmp.Compare(a, b)
}

func (plan candidatePlan) executeUnionCount(pq parsedQuery, hidden hiddenBaseIDs) (int, int, bool) {
	if plan.vol == nil || plan.vol.index == nil || len(plan.sources) != 1 ||
		len(plan.sources[0].union) == 0 || len(plan.vol.recentIDs) != 0 ||
		plan.underPathFallback != "" || pq.RootBias != "" || pq.CWDBias != "" {
		return 0, 0, false
	}
	parts := plan.sources[0].union
	for _, part := range parts {
		if part.hasPosting || len(part.union) != 0 || len(part.roots) != 0 {
			return 0, 0, false
		}
	}
	positions := make([]int, len(parts))
	last, haveLast := 0, false
	pathCache := make(map[int]string)
	count, scanned := 0, 0
	for {
		best := -1
		for i, part := range parts {
			if positions[i] >= len(part.ids) {
				continue
			}
			id := part.ids[positions[i]]
			if best < 0 || id < parts[best].ids[positions[best]] {
				best = i
			}
		}
		if best < 0 {
			break
		}
		id := parts[best].ids[positions[best]]
		positions[best]++
		if haveLast && id == last {
			continue
		}
		last, haveLast = id, true
		scanned++
		if scanned&1023 == 0 && queryCanceled(pq) {
			return 0, scanned, false
		}
		if id < 0 || id >= plan.vol.index.compactRecordCount() || (!hidden.empty() && hidden.contains(id)) {
			continue
		}
		matched := false
		if queryNeedsPath(pq) {
			_, matched = compactCandidateEntryIfMatch(plan.vol.index, pq, id, pathCache, true, false)
		} else {
			rec := plan.vol.index.compactRecord(id)
			matched = !rec.Deleted && plan.vol.recordMatchesNonPath(id, rec, pq)
		}
		if matched {
			count++
		}
	}
	return count, scanned, true
}

func (plan candidatePlan) sourceSummary() string {
	if plan.empty {
		return "empty"
	}
	if len(plan.sources) == 0 {
		return "none"
	}
	names := make([]string, 0, len(plan.sources))
	for _, source := range plan.sources {
		names = append(names, source.name)
	}
	sort.Strings(names)
	return strings.Join(names, "+")
}

func (plan candidatePlan) traceTerms() []traceTerm {
	if len(plan.sources) == 0 {
		return nil
	}
	volume := ""
	if plan.vol != nil && plan.vol.index != nil {
		volume = plan.vol.index.Volume
	}
	out := make([]traceTerm, 0, len(plan.sources))
	for _, source := range plan.sources {
		if len(source.union) > 0 {
			for _, part := range source.union {
				out = append(out, traceTermForCandidateSource(part, volume))
			}
			continue
		}
		out = append(out, traceTermForCandidateSource(source, volume))
	}
	return out
}

func (plan candidatePlan) filterUnderPath(ids []int) []int {
	if plan.vol == nil || plan.vol.index == nil || plan.underPathFallback == "" || len(ids) == 0 {
		return ids
	}
	pathCache := make(map[int]string)
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id < 0 || id >= plan.vol.index.compactRecordCount() {
			continue
		}
		path := plan.vol.index.reconstructCompactPathCached(id, pathCache)
		if pathUnder(path, plan.underPathFallback) {
			out = append(out, id)
		}
	}
	return out
}

// orGroupPosting returns the union of candidate postings for an OR group when
// every alternative is cheaply postable. The bool is false when at least one
// alternative cannot be turned into a posting, in which case the caller should
// let the group be verified against the broader candidate set instead.
func (vol *serviceVolumeIndex) orGroupPosting(group []parsedQuery, matchPath bool) ([]int, bool) {
	source, ok := vol.orGroupPlanSource(group, matchPath)
	if !ok {
		return nil, false
	}
	return source.materialize(), true
}

func (vol *serviceVolumeIndex) orGroupPlanSource(group []parsedQuery, matchPath bool) (candidatePlanSource, bool) {
	parts := make([]candidatePlanSource, 0, len(group))
	for _, alt := range group {
		source, ok := vol.altPlanSource(alt, matchPath)
		if !ok {
			return candidatePlanSource{}, false
		}
		parts = append(parts, source)
	}
	return candidatePlanSource{name: "or-group", union: parts}, true
}

// altPosting returns a posting for a single OR alternative if it is a lone
// ext:, simple glob extension, or plain term. Returns ok=false otherwise.
func (vol *serviceVolumeIndex) altPosting(alt parsedQuery, matchPath bool) ([]int, bool) {
	source, ok := vol.altPlanSource(alt, matchPath)
	if !ok {
		return nil, false
	}
	return source.materialize(), true
}

func (vol *serviceVolumeIndex) altPlanSource(alt parsedQuery, matchPath bool) (candidatePlanSource, bool) {
	switch {
	case len(alt.Exts) == 1 && alt.isOnly("ext"):
		ext := alt.Exts[0]
		if candidate, ok := vol.extPostingCountCandidate(ext); ok {
			return candidatePlanSource{name: "ext:" + ext, posting: candidate, hasPosting: true}, true
		}
		return candidatePlanSource{name: "ext:" + ext, ids: uniqueSortedInts(vol.extPosting(ext))}, true
	case len(alt.Globs) == 1 && alt.isOnly("glob"):
		if exts, ok := simpleGlobExts(alt.Globs); ok && len(exts) == 1 {
			ext := exts[0]
			if candidate, ok := vol.extPostingCountCandidate(ext); ok {
				return candidatePlanSource{name: "glob-ext:" + ext, posting: candidate, hasPosting: true}, true
			}
			return candidatePlanSource{name: "glob-ext:" + ext, ids: uniqueSortedInts(vol.extPosting(ext))}, true
		}
		return candidatePlanSource{}, false
	case len(alt.Terms) == 1 && alt.isOnly("term"):
		if matchPath {
			term := alt.Terms[0]
			return candidatePlanSource{name: "path-term:" + term, ids: uniqueSortedInts(vol.pathTermPosting(term))}, true
		}
		term := alt.Terms[0]
		return candidatePlanSource{name: "term:" + term, ids: uniqueSortedInts(vol.nameTermPosting(term))}, true
	case len(alt.Parents) == 1 && alt.isOnly("parent"):
		parent := alt.Parents[0]
		return candidatePlanSource{name: "parent:" + parent, ids: uniqueSortedInts(vol.parentIDs(parent))}, true
	case len(alt.AttrFilters) == 1 && alt.isOnly("attrib"):
		mask := alt.AttrFilters[0]
		ids, ok := vol.attrIDsForMask(mask)
		if !ok {
			return candidatePlanSource{}, false
		}
		return candidatePlanSource{name: "attrib:" + attribMaskString(mask), ids: uniqueSortedInts(ids)}, true
	default:
		return candidatePlanSource{}, false
	}
}

// isOnly reports whether the alternative carries exactly one kind of constraint
// (named by kind) and nothing else, so it can be turned into a single posting.
func (pq parsedQuery) isOnly(kind string) bool {
	counts := map[string]int{
		"ext":    len(pq.Exts),
		"glob":   len(pq.Globs),
		"term":   len(pq.Terms),
		"parent": len(pq.Parents),
		"attrib": len(pq.AttrFilters),
	}
	other := len(pq.Dirs) + len(pq.Regexps) + len(pq.SizeFilters) +
		len(pq.DateFilters) + len(pq.OrGroups) + len(pq.NotGroups)
	if pq.Type != "" {
		other++
	}
	if other != 0 {
		return false
	}
	for k, v := range counts {
		if k == kind {
			continue
		}
		if v != 0 {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) unionUnderDescendants(roots []int) []int {
	if len(roots) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, 256)
	out := make([]int, 0, 256)
	for _, root := range roots {
		for _, id := range vol.underDescendants(root) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func shouldUseUnderPlanSource(underEstimatedSize int, sources []candidatePlanSource) bool {
	if len(sources) == 0 {
		return true
	}
	if underEstimatedSize < 0 {
		return false
	}
	smallest := sources[0].len()
	for _, source := range sources[1:] {
		if source.len() < smallest {
			smallest = source.len()
		}
	}
	return underEstimatedSize <= smallest
}

func (vol *serviceVolumeIndex) estimateUnderDescendantCount(roots []int) int {
	if len(roots) == 0 {
		return 0
	}
	total := 0
	for _, root := range roots {
		if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) || len(vol.subtreeOrder) == 0 {
			return -1
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start == ^uint32(0) || start > end {
			return -1
		}
		total += int(end - start)
	}
	return total
}
