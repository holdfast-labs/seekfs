package main

import (
	"cmp"
	"container/heap"
	"slices"
	"sort"
	"strings"
)

func globalExtOnlySupported(pq parsedQuery) bool {
	extFilters, ok := globalExtPostingFilters(pq)
	if !ok || len(extFilters) != 1 || len(nonVolumeTerms(pq.Terms)) != 0 || len(pq.Dirs) != 0 ||
		len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 ||
		len(pq.Parents) != 0 || pq.Under != "" || pq.Exists || pq.HasModAfter || len(pq.SizeFilters) != 0 ||
		len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 || len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 ||
		queryHasAnyContentLeaf(pq) ||
		pq.CaseSensitive {
		return false
	}
	return pq.Type == ""
}

func globalExtDefaultSupported(pq parsedQuery) bool {
	return globalExtOnlySupported(pq) && (pq.SortColumn == "" || pq.SortColumn == "path" ||
		pq.SortColumn == "size" || pq.SortColumn == "modified" || pq.SortColumn == "extension" || pq.SortColumn == "type") &&
		(!pq.MatchPath || len(pq.Terms) == 0)
}

func globalBoundedFallbackDefaultSupported(pq parsedQuery) bool {
	return !pq.isEmpty()
}

func globalComponentDefaultSupported(pq parsedQuery, terms []string) bool {
	if pq.SortColumn != "" && pq.SortColumn != "path" && pq.SortColumn != "size" &&
		pq.SortColumn != "modified" && pq.SortColumn != "extension" && pq.SortColumn != "type" {
		return false
	}
	if !globalComponentDefaultHasRoot(pq) {
		return false
	}
	if !globalComponentDefaultTermsLong(pq, terms) {
		return false
	}
	return globalComponentQuerySupported(pq, terms)
}

func globalComponentDefaultHasRoot(pq parsedQuery) bool {
	extFilters, extOK := globalExtPostingFilters(pq)
	if (pq.MatchPath && (queryHasExplicitPathTerm(pq.Raw) || globalComponentDefaultImplicitPathRoot(pq))) ||
		globalComponentVolumeAnchored(pq) ||
		len(pq.Dirs) != 0 || len(pq.Parents) != 0 || pq.Under != "" || len(pq.AttrFilters) != 0 ||
		globalRegexLiteralSupported(pq) ||
		(extOK && len(extFilters) != 0 &&
			(pq.Type != "" || pq.Exists || pq.HasModAfter || len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0)) {
		return true
	}
	// Validate every alternative in every OR group, matching the per-group
	// walk in globalComponentDefaultTermsLong. Groups are intersected and
	// alternatives unioned, so one unrooted alternative means the default
	// component cannot answer from postings alone; declining here only routes
	// the query to the fallback path.
	for _, group := range pq.OrGroups {
		if len(group) == 0 {
			return false
		}
		for _, alt := range group {
			if !globalComponentDefaultHasRoot(alt) {
				return false
			}
		}
	}
	return len(pq.OrGroups) > 0
}

func globalComponentVolumeAnchored(pq parsedQuery) bool {
	if !pq.MatchPath {
		return false
	}
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			return true
		}
	}
	return false
}

func globalComponentDefaultImplicitPathRoot(pq parsedQuery) bool {
	if !pq.MatchPath {
		return false
	}
	terms := nonVolumeTerms(pq.Terms)
	if len(terms) >= 2 {
		return true
	}
	extFilters, ok := globalExtPostingFilters(pq)
	return ok && len(terms) >= 1 && len(extFilters) > 0
}

func globalComponentDefaultTermsLong(pq parsedQuery, terms []string) bool {
	for _, term := range terms {
		if len(term) < 3 && !slices.Contains(pq.ImplicitPathTerms, term) {
			return false
		}
	}
	for _, dir := range pq.Dirs {
		if len(dir) < 3 || strings.ContainsAny(dir, `\/*?[]:`) {
			return false
		}
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			if !globalComponentDefaultTermsLong(alt, nonVolumeTerms(alt.Terms)) {
				return false
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if !globalComponentDefaultTermsLong(neg, nonVolumeTerms(neg.Terms)) {
			return false
		}
	}
	return true
}

func queryHasExplicitPathTerm(raw string) bool {
	for _, field := range strings.Fields(raw) {
		lower := strings.ToLower(strings.TrimLeft(field, "!-"))
		if strings.HasPrefix(lower, "path:") || strings.HasPrefix(lower, "fullpath:") ||
			strings.HasPrefix(lower, "full-path:") || strings.HasPrefix(lower, "full_path:") ||
			strings.HasPrefix(lower, "location:") {
			return true
		}
	}
	return false
}

type globalExtFilter struct {
	ext    string
	source string
}

func globalExtPostingFilters(pq parsedQuery) ([]globalExtFilter, bool) {
	if len(pq.Exts) > 0 && len(pq.Globs) > 0 {
		return nil, false
	}
	if len(pq.Exts) > 1 {
		return nil, false
	}
	if len(pq.Exts) == 1 {
		ext := pq.Exts[0]
		return []globalExtFilter{{ext: ext, source: "ext:" + ext}}, true
	}
	if len(pq.Globs) == 0 {
		return nil, true
	}
	exts, ok := simpleGlobExts(pq.Globs)
	if !ok || len(exts) != 1 {
		return nil, false
	}
	ext := exts[0]
	return []globalExtFilter{{ext: ext, source: "glob-ext:" + ext}}, true
}

func globalComponentQuerySupported(pq parsedQuery, terms []string) bool {
	return globalComponentQuerySupportedMulti(pq, terms, false)
}

// globalComponentQuerySupportedMulti is globalComponentQuerySupported with an
// explicit multi-volume flag.  Multi-volume pure-substring regexes are declined
// from the components lane so they route to the bounded-scan regex pre-filter
// (which matches the full-scan order and stops at the top-N); single-volume
// regexes keep the components/literal lane so the persisted per-volume regex
// candidates continue to serve them.
func globalComponentQuerySupportedMulti(pq parsedQuery, terms []string, multi bool) bool {
	if queryHasAnyContentLeaf(pq) {
		// The components lane verifies only name/path; a content query must
		// route through the content candidate + post-filter path.
		return false
	}
	minTerms := 2
	if queryHasExplicitPathTerm(pq.Raw) || globalComponentVolumeAnchored(pq) {
		minTerms = 1
	}
	if len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 {
		minTerms = 1
	}
	if len(pq.OrGroups) != 0 {
		minTerms = 0
	}
	if len(pq.Dirs) != 0 || len(pq.Parents) != 0 || pq.Under != "" || len(pq.AttrFilters) != 0 {
		minTerms = 0
	}
	if !globalComponentTermsSupportedMulti(pq, terms, minTerms, true, multi) {
		return false
	}
	for _, group := range pq.OrGroups {
		if len(group) == 0 {
			return false
		}
		for _, alt := range group {
			if len(alt.OrGroups) != 0 || len(alt.NotGroups) != 0 {
				return false
			}
			if !globalComponentTermsSupportedMulti(alt, nonVolumeTerms(alt.Terms), 1, true, multi) {
				return false
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if len(neg.OrGroups) != 0 || len(neg.NotGroups) != 0 {
			return false
		}
		if !globalComponentTermsSupportedMulti(neg, nonVolumeTerms(neg.Terms), 1, true, multi) {
			return false
		}
	}
	return true
}

func globalComponentTermsSupported(pq parsedQuery, terms []string, minTerms int, allowExtFilters bool) bool {
	return globalComponentTermsSupportedMulti(pq, terms, minTerms, allowExtFilters, false)
}

func globalComponentTermsSupportedMulti(pq parsedQuery, terms []string, minTerms int, allowExtFilters bool, multi bool) bool {
	extFilters, extFiltersOK := globalExtPostingFilters(pq)
	if !extFiltersOK || (!allowExtFilters && len(extFilters) != 0) {
		return false
	}
	if len(pq.Dirs) != 0 || len(pq.Parents) != 0 || pq.Under != "" || len(pq.AttrFilters) != 0 || len(extFilters) != 0 {
		minTerms = 0
	}
	if globalRegexLiteralSupported(pq) {
		minTerms = 0
	}
	// A pure-substring regex (.*literal.*) on a multi-volume service is served
	// exactly and far faster by the bounded-scan regex pre-filter (which walks
	// the name/id order and stops at the top-N), matching the full-scan result
	// order.  The components iterator would otherwise materialize every path
	// containing the literal and verify each with the regex -- seconds to
	// minutes for a common term.  Declining here routes the query to the
	// bounded fallback instead.  Single-volume queries keep the components
	// regex-literal lane.
	if multi && globalRegexLiteralSupported(pq) && queryIsPureSubstringRegex(pq) {
		return false
	}
	if (len(terms) > 0 && !pq.MatchPath) || (!pq.MatchPath && len(pq.Dirs) == 0 && len(pq.Parents) == 0 && pq.Under == "" && len(extFilters) == 0 && len(pq.AttrFilters) == 0 && len(pq.OrGroups) == 0 && !globalRegexLiteralSupported(pq)) ||
		len(terms) < minTerms || (pq.Type != "" && pq.Type != "file" && pq.Type != "dir") ||
		(len(pq.Regexps) != 0 && !globalRegexLiteralSupported(pq)) ||
		(len(pq.RegexTerms) != 0 && !globalRegexLiteralSupported(pq)) {
		return false
	}
	for _, dir := range pq.Dirs {
		if dir == "" || strings.ContainsAny(dir, `\/*?[]:`) {
			return false
		}
	}
	for _, term := range terms {
		if term == "" || strings.ContainsAny(term, `\/*?[]:`) {
			return false
		}
	}
	return true
}

// queryIsPureSubstringRegex reports whether the query is exactly one
// case-insensitive path regex whose single literal run makes it a pure
// substring match (.*literal.*), with no other terms or filters.
func queryIsPureSubstringRegex(pq parsedQuery) bool {
	if !pq.MatchPath || pq.CaseSensitive || len(pq.Regexps) != 1 || len(pq.RegexTerms) != 1 ||
		len(pq.Terms) != 0 || len(pq.Exts) != 0 || len(pq.Globs) != 0 || len(pq.Dirs) != 0 ||
		len(pq.Parents) != 0 || len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 ||
		len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 ||
		pq.Type != "" || pq.Under != "" || pq.Exists || pq.HasModAfter ||
		pq.RootBias != "" || pq.CWDBias != "" {
		return false
	}
	literal := regexRequiredLiteral(pq.Regexps[0].String())
	return len(literal) >= 3 && literal == pq.RegexTerms[0]
}

func globalRegexLiteralSupported(pq parsedQuery) bool {
	return pq.MatchPath && !pq.CaseSensitive && len(pq.Regexps) > 0 && len(pq.RegexTerms) == 1 && len(pq.RegexTerms[0]) >= 3 && !strings.ContainsAny(pq.RegexTerms[0], `\/*?[]:`)
}

func collectGlobalTopN(iterators []globalIDIterator, limit int, rankOf func(globalRecordID) int) []globalRecordID {
	if limit <= 0 || rankOf == nil {
		return nil
	}
	h := make(globalRankMaxHeap, 0, limit)
	for _, it := range iterators {
		if it == nil {
			continue
		}
		for {
			id, ok := it.Next()
			if !ok {
				break
			}
			item := globalRankItem{id: id, rank: rankOf(id)}
			if len(h) < limit {
				heap.Push(&h, item)
				continue
			}
			if globalRankItemBetter(item, h[0]) {
				h[0] = item
				heap.Fix(&h, 0)
			}
		}
	}
	out := make([]globalRankItem, len(h))
	copy(out, h)
	slices.SortFunc(out, func(a, b globalRankItem) int {
		if n := cmp.Compare(a.rank, b.rank); n != 0 {
			return n
		}
		return compareGlobalRecordID(a.id, b.id)
	})
	ids := make([]globalRecordID, len(out))
	for i, item := range out {
		ids[i] = item.id
	}
	return ids
}

type globalVerifiedTopHeap struct {
	items  []globalRankedEntry
	pq     parsedQuery
	global bool
}

func (h globalVerifiedTopHeap) Len() int { return len(h.items) }

func (h globalVerifiedTopHeap) Less(i, j int) bool {
	return compareGlobalVerifiedEntries(h.items[i], h.items[j], h.pq, h.global) > 0
}

func (h globalVerifiedTopHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *globalVerifiedTopHeap) Push(x any) {
	h.items = append(h.items, x.(globalRankedEntry))
}

func (h *globalVerifiedTopHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

func compareGlobalVerifiedEntries(a, b globalRankedEntry, pq parsedQuery, global bool) int {
	if global {
		if n := compareSearchAllEntries(a.entry, b.entry, pq); n != 0 {
			return n
		}
	}
	if n := cmp.Compare(a.rank, b.rank); n != 0 {
		return n
	}
	if n := cmp.Compare(a.volume, b.volume); n != 0 {
		return n
	}
	return strings.Compare(a.tie, b.tie)
}

// collectGlobalVerifiedTopN consumes the set iterator once and retains only
// the requested top-N entries. Boolean branches stay lazy: OR sources merge
// their posting iterators and NOT advances the exclusion iterator with
// SeekGE, while this verifier owns the only bounded result heap.
func collectGlobalVerifiedTopN(it globalIDIterator, volumes []*serviceVolumeIndex, snapshots []*volumeSnapshot, pq parsedQuery, limit int) ([]globalRankedEntry, int, error) {
	if it == nil || limit <= 0 {
		return nil, 0, nil
	}
	global := len(volumes) > 1 || globalSnapshotsHaveOverlayRecords(snapshots)
	h := &globalVerifiedTopHeap{pq: pq, global: global}
	heap.Init(h)
	pathCaches := make([]map[int]string, len(volumes))
	rankers := make([]func(int) int, len(volumes))
	for i, vol := range volumes {
		if vol != nil && vol.index != nil {
			rankers[i] = candidateRanker(vol.index, vol.rankForQuery(pq))
		}
	}
	verified := 0
	for {
		id, ok := it.Next()
		if !ok {
			break
		}
		if verified&1023 == 0 && queryCanceled(pq) {
			return nil, verified, errQueryCanceled
		}
		if globalHiddenContains(snapshots, id) || id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		entry, ok := compactCandidateEntryIfMatch(vol.index, volumePQ, id.local, pathCaches[id.volume], true, false)
		verified++
		if !ok {
			continue
		}
		rank := int(^uint(0) >> 1)
		if rankers[id.volume] != nil {
			rank = rankers[id.volume](id.local)
		}
		item := globalRankedEntry{entry: entry, rank: rank, volume: id.volume, tie: entry.Path}
		if h.Len() < limit {
			heap.Push(h, item)
			continue
		}
		if compareGlobalVerifiedEntries(item, h.items[0], pq, global) < 0 {
			h.items[0] = item
			heap.Fix(h, 0)
		}
	}
	out := append([]globalRankedEntry(nil), h.items...)
	sortGlobalRankedEntries(out, pq)
	return out, verified, nil
}

func countGlobalVerifiedIterator(it globalIDIterator, volumes []*serviceVolumeIndex, snapshots []*volumeSnapshot, pq parsedQuery) (int, int, error) {
	if it == nil {
		return 0, 0, nil
	}
	pathCaches := make([]map[int]string, len(volumes))
	count, verified := 0, 0
	for {
		id, ok := it.Next()
		if !ok {
			break
		}
		if verified&1023 == 0 && queryCanceled(pq) {
			return 0, verified, errQueryCanceled
		}
		if globalHiddenContains(snapshots, id) || id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		_, ok = compactCandidateEntryIfMatch(vol.index, volumePQ, id.local, pathCaches[id.volume], true, false)
		verified++
		if ok {
			count++
		}
	}
	return count, verified, nil
}

func globalRankItemBetter(a, b globalRankItem) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	return compareGlobalRecordID(a.id, b.id) < 0
}

func globalRankItemWorse(a, b globalRankItem) bool {
	if a.rank != b.rank {
		return a.rank > b.rank
	}
	return compareGlobalRecordID(a.id, b.id) > 0
}

type globalRankItem struct {
	id   globalRecordID
	rank int
}

type globalRankMaxHeap []globalRankItem

func (h globalRankMaxHeap) Len() int { return len(h) }

func (h globalRankMaxHeap) Less(i, j int) bool { return globalRankItemWorse(h[i], h[j]) }

func (h globalRankMaxHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *globalRankMaxHeap) Push(x any) {
	*h = append(*h, x.(globalRankItem))
}

func (h *globalRankMaxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func (it *globalRecordIterator) CountHint() int {
	if it == nil || it.pos >= len(it.ids) {
		return 0
	}
	return len(it.ids) - it.pos
}

func (it *globalRecordIterator) Next() (globalRecordID, bool) {
	if it == nil || it.pos >= len(it.ids) {
		return globalRecordID{}, false
	}
	id := globalRecordID{volume: it.volume, local: it.ids[it.pos]}
	it.pos++
	return id, true
}

func (it *globalRecordIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil {
		return globalRecordID{}, false
	}
	if target.volume > it.volume {
		it.pos = len(it.ids)
		return globalRecordID{}, false
	}
	minLocal := target.local
	if target.volume < it.volume {
		minLocal = 0
	}
	for it.pos < len(it.ids) && it.ids[it.pos] < minLocal {
		it.pos++
	}
	return it.Next()
}

func (it *globalPostingIterator) CountHint() int {
	if it == nil {
		return 0
	}
	return it.remaining
}

func (it *globalPostingIterator) Next() (globalRecordID, bool) {
	if it == nil || it.remaining <= 0 {
		return globalRecordID{}, false
	}
	if !it.posting.mapped {
		if it.pos >= len(it.posting.ids) {
			it.remaining = 0
			return globalRecordID{}, false
		}
		id := it.posting.ids[it.pos]
		it.pos++
		it.remaining--
		return globalRecordID{volume: it.volume, local: int(id)}, true
	}
	for it.blockPos >= len(it.block) {
		block, _, ok := it.posting.it.nextBlock()
		if !ok {
			it.remaining = 0
			return globalRecordID{}, false
		}
		it.trace.addPostingBlocks(1, 0)
		it.block = block
		it.blockPos = 0
	}
	id := it.block[it.blockPos]
	it.blockPos++
	it.remaining--
	return globalRecordID{volume: it.volume, local: int(id)}, true
}

func (it *globalPostingIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.remaining <= 0 {
		return globalRecordID{}, false
	}
	if target.volume > it.volume {
		it.remaining = 0
		return globalRecordID{}, false
	}
	minLocal := target.local
	if target.volume < it.volume || minLocal < 0 {
		minLocal = 0
	}
	if !it.posting.mapped {
		skipped := sort.Search(len(it.posting.ids)-it.pos, func(i int) bool {
			return int(it.posting.ids[it.pos+i]) >= minLocal
		})
		it.pos += skipped
		it.remaining -= skipped
		return it.Next()
	}
	if it.blockPos < len(it.block) {
		skipped := sort.Search(len(it.block)-it.blockPos, func(i int) bool {
			return int(it.block[it.blockPos+i]) >= minLocal
		})
		it.blockPos += skipped
		it.remaining -= skipped
		if it.blockPos < len(it.block) {
			return it.Next()
		}
	}
	for it.posting.it.next < it.posting.it.end {
		meta, ok := it.posting.it.blockMetaAt(it.posting.it.next)
		if !ok {
			it.remaining = 0
			return globalRecordID{}, false
		}
		if int(meta.maxID) < minLocal {
			it.posting.it.next++
			it.remaining -= int(meta.count)
			it.trace.addPostingBlocks(0, 1)
			continue
		}
		block, _, ok := it.posting.it.nextBlock()
		if !ok {
			it.remaining = 0
			return globalRecordID{}, false
		}
		it.block = block
		it.trace.addPostingBlocks(1, 0)
		it.blockPos = sort.Search(len(block), func(i int) bool { return int(block[i]) >= minLocal })
		it.remaining -= it.blockPos
		return it.Next()
	}
	it.remaining = 0
	return globalRecordID{}, false
}

func intersectGlobalIterators(a, b globalIDIterator, limit int) []globalRecordID {
	if a == nil || b == nil {
		return nil
	}
	av, aok := a.Next()
	bv, bok := b.Next()
	out := make([]globalRecordID, 0, minPositiveCountHint(a.CountHint(), b.CountHint()))
	for aok && bok {
		switch compareGlobalRecordID(av, bv) {
		case 0:
			out = append(out, av)
			if limit > 0 && len(out) >= limit {
				return out
			}
			av, aok = a.Next()
			bv, bok = b.Next()
		case -1:
			av, aok = a.SeekGE(bv)
		default:
			bv, bok = b.SeekGE(av)
		}
	}
	return out
}

func unionGlobalIterators(a, b globalIDIterator, limit int) []globalRecordID {
	if a == nil {
		return collectGlobalIterator(b, limit)
	}
	if b == nil {
		return collectGlobalIterator(a, limit)
	}
	av, aok := a.Next()
	bv, bok := b.Next()
	out := make([]globalRecordID, 0, positiveCountHint(a.CountHint())+positiveCountHint(b.CountHint()))
	for aok || bok {
		var next globalRecordID
		switch {
		case !bok || (aok && compareGlobalRecordID(av, bv) < 0):
			next = av
			av, aok = a.Next()
		case !aok || compareGlobalRecordID(av, bv) > 0:
			next = bv
			bv, bok = b.Next()
		default:
			next = av
			av, aok = a.Next()
			bv, bok = b.Next()
		}
		out = append(out, next)
		if limit > 0 && len(out) >= limit {
			return out
		}
	}
	return out
}

func excludeGlobalIterator(include, exclude globalIDIterator, limit int) []globalRecordID {
	if include == nil {
		return nil
	}
	if exclude == nil {
		return collectGlobalIterator(include, limit)
	}
	iv, iok := include.Next()
	ev, eok := exclude.Next()
	out := make([]globalRecordID, 0, positiveCountHint(include.CountHint()))
	for iok {
		for eok && compareGlobalRecordID(ev, iv) < 0 {
			ev, eok = exclude.SeekGE(iv)
		}
		if !eok || compareGlobalRecordID(iv, ev) != 0 {
			out = append(out, iv)
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
		iv, iok = include.Next()
	}
	return out
}

func collectGlobalIterator(it globalIDIterator, limit int) []globalRecordID {
	if it == nil {
		return nil
	}
	out := make([]globalRecordID, 0, positiveCountHint(it.CountHint()))
	for {
		id, ok := it.Next()
		if !ok {
			return out
		}
		out = append(out, id)
		if limit > 0 && len(out) >= limit {
			return out
		}
	}
}

// collectGlobalIteratorCancelable is collectGlobalIterator with a per-yield
// cancellation probe.  Materializing a broad multi-term component can walk
// millions of IDs; the caller bails with errQueryCanceled instead of letting
// a superseded query run to completion.
func collectGlobalIteratorCancelable(it globalIDIterator, limit int, canceled func() bool) ([]globalRecordID, bool) {
	if it == nil {
		return nil, false
	}
	out := make([]globalRecordID, 0, positiveCountHint(it.CountHint()))
	for {
		if canceled != nil && canceled() {
			return nil, true
		}
		id, ok := it.Next()
		if !ok {
			return out, false
		}
		out = append(out, id)
		if limit > 0 && len(out) >= limit {
			return out, false
		}
	}
}

func minPositiveCountHint(a, b int) int {
	a = positiveCountHint(a)
	b = positiveCountHint(b)
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	return min(a, b)
}

func positiveCountHint(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func sortIntListsByLen(lists [][]int) {
	slices.SortFunc(lists, func(a, b []int) int {
		return cmp.Compare(len(a), len(b))
	})
}

func sortUint32ListsByLen(lists [][]uint32) {
	slices.SortFunc(lists, func(a, b []uint32) int {
		return cmp.Compare(len(a), len(b))
	})
}

func sortPostingCountCandidatesByLen(lists []postingCountCandidate) {
	slices.SortFunc(lists, func(a, b postingCountCandidate) int {
		return cmp.Compare(a.len(), b.len())
	})
}

func sortIDsByRank(ids []int, rankOf func(int) int) {
	slices.SortFunc(ids, func(a, b int) int {
		if n := cmp.Compare(rankOf(a), rankOf(b)); n != 0 {
			return n
		}
		return cmp.Compare(a, b)
	})
}

func sortCandidatePlanSourcesByLen(sources []candidatePlanSource) {
	slices.SortFunc(sources, func(a, b candidatePlanSource) int {
		if n := cmp.Compare(a.len(), b.len()); n != 0 {
			return n
		}
		return cmp.Compare(a.name, b.name)
	})
}
