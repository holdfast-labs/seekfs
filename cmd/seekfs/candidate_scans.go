package main

import (
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

func (vol *serviceVolumeIndex) boundedPathTermPlanSource(term string) ([]int, bool) {
	if vol == nil || vol.index == nil || term == "" || isVolumeQueryTerm(term) ||
		strings.ContainsAny(term, `\/*?[]:`) || len(term) < 3 {
		return nil, false
	}
	// A term whose required filename grams are proven absent by the complete
	// name-gram metadata (PNGR counts complete, or the PNGC companion) cannot
	// match anything: return a proven empty source so the plan marks itself
	// empty instead of materializing a broad extension/scan.  This is the same
	// exact-zero proof the fast count path uses.
	if _, _, exactZero, complete := completeSelfNameGramIterators(vol.index, term); complete && exactZero {
		return []int{}, true
	}
	// First try the posting path; it is cheaper than a scan for a selective
	// term that has persisted gram or component postings.
	ids, ok := vol.completeNameTrigramPathTermPosting(term)
	if ok && len(ids) <= serviceComponentTrigramExpansionMaxIDs {
		return ids, true
	}
	// The posting path declined (for example an omitted-common gram, a subtree
	// estimate above the expansion cap, or a term with zero name matches).  For
	// a loose multi-term query the plan still needs a bounded source so it does
	// not drive off a huge promoted extension posting and verify every other
	// term against it.  A bounded parallel name scan proves a zero-match term
	// empty immediately, and a small match set becomes a path posting source
	// (name self-hits expanded to descendants) for the driving term.
	if scanned := vol.scanNameTermBounded(term, serviceComponentTrigramExpansionMaxIDs); scanned != nil {
		if len(scanned) == 0 {
			return []int{}, true
		}
		if ids, ok := vol.expandNameMatchesToPathTermPosting("", term, scanned); ok && len(ids) <= serviceComponentTrigramExpansionMaxIDs {
			return ids, true
		}
	}
	if vol.index.compactRecordCount() > serviceResidentChildRangeMaxRecords {
		return nil, false
	}
	ids, ok = vol.scannedNamePathTermPosting(term)
	if !ok || len(ids) > serviceComponentTrigramExpansionMaxIDs {
		return nil, false
	}
	return ids, true
}

// scanNameTermBounded scans the compact records in parallel up to maxMatches
// and returns the matched record IDs.  It returns nil when the match set
// exceeds the bound, so the caller declines to the exhaustive scan rather than
// materializing a huge posting.  A term with provably zero matches returns an
// empty non-nil slice.  This works in every mode (resident, lowmem, mapped)
// because it scans record IDs directly, mirroring scanNameTermPosting.
func (vol *serviceVolumeIndex) scanNameTermBounded(term string, maxMatches int) []int {
	if vol == nil || vol.index == nil || term == "" || maxMatches <= 0 {
		return nil
	}
	recordCount := vol.index.compactRecordCount()
	if recordCount == 0 {
		return []int{}
	}
	// The mapped fast path scans the persisted lower-name blob in bulk instead
	// of reconstructing every CompactRecord; reuse it when available.
	if ids, ok := vol.index.scanCompactLowerNameTerm(term); ok {
		if len(ids) > maxMatches {
			return nil
		}
		return ids
	}
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
				if len(out) > maxMatches {
					return nil
				}
			}
		}
		return out
	}
	parts := make([][]int, workers)
	exceeded := make([]bool, workers)
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
					if len(local) > maxMatches {
						exceeded[worker] = true
						return
					}
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	total := 0
	for _, ex := range exceeded {
		if ex {
			return nil
		}
	}
	for _, part := range parts {
		total += len(part)
	}
	out := make([]int, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func (vol *serviceVolumeIndex) completeNameTrigramNameTermPostingLimited(term string, maxIDs int) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	if vol == nil || trigrams == nil {
		return nil, false
	}
	cacheKey := "\x00complete-ngram-name:" + term
	vol.termMu.Lock()
	if vol.termCache != nil {
		if entry, ok := vol.termCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.nameTrigramCandidateMatches(id, term)
				}), true
			}
		}
	}
	vol.termMu.Unlock()

	ids, ok, missing := trigrams.selectiveCandidateIDs(term, maxIDs)
	if len(term) >= 6 {
		ids, ok, missing = trigrams.selectiveIntersectCandidateIDs(term, maxIDs)
	}
	if !ok {
		return nil, false
	}
	if missing {
		return vol.nameTrigramRecentMatches(term), true
	}
	out := uniqueSortedInts(vol.verifyNameTrigramCandidateIDs(ids, term))
	vol.cacheNamePosting(cacheKey, out)
	return vol.withNameTrigramRecentCandidates(out, term), true
}

func (vol *serviceVolumeIndex) completeNameTrigramPathTermPosting(term string) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	cacheKey := "\x00complete-trigram-path:" + term
	vol.termMu.Lock()
	if vol.pathTermCache != nil {
		if entry, ok := vol.pathTermCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.index.compactPathContainsTerm(id, term)
				}), true
			}
		}
	}
	vol.termMu.Unlock()

	nameMatches, ok := vol.completeNameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
		return nil, false
	}
	return vol.expandNameMatchesToPathTermPosting(cacheKey, term, nameMatches)
}

func (vol *serviceVolumeIndex) scannedNamePathTermPosting(term string) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	cacheKey := "\x00scan-name-path:" + term
	vol.termMu.Lock()
	if vol.pathTermCache != nil {
		if entry, ok := vol.pathTermCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.index.compactPathContainsTerm(id, term)
				}), true
			}
		}
	}
	vol.termMu.Unlock()

	nameMatches := vol.nameTermPosting(term)
	if len(nameMatches) > servicePathNameTrigramCandidateMaxIDs {
		return nil, false
	}
	return vol.expandNameMatchesToPathTermPosting(cacheKey, term, nameMatches)
}

func (vol *serviceVolumeIndex) expandNameMatchesToPathTermPosting(cacheKey, term string, nameMatches []int) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	seen := make(map[int]struct{}, len(nameMatches))
	out := make([]int, 0, len(nameMatches))
	estimated := 0
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if rec.Mode&uint32(os.ModeDir) == 0 {
			estimated++
		} else {
			if !vol.hasDescendantIndex() {
				return nil, false
			}
			estimated += vol.estimatedDescendantOrSelfCount(id)
		}
		if estimated > serviceComponentTrigramExpansionMaxIDs {
			return nil, false
		}
	}
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			out = append(out, id)
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if !vol.hasDescendantIndex() {
			return nil, false
		}
		for _, childID := range vol.underDescendants(id) {
			child := int(childID)
			if _, exists := seen[child]; exists {
				continue
			}
			seen[child] = struct{}{}
			out = append(out, child)
			if len(out) > serviceComponentTrigramExpansionMaxIDs {
				return nil, false
			}
		}
	}
	sort.Ints(out)
	if cacheKey != "" {
		vol.cachePathPosting(cacheKey, out)
	}
	return out, true
}

// broadPathScanCandidates is retained for direct benchmark/test coverage of
// the old broad path scanner. The live route now uses boundedScanCandidates for
// this family.
//
// It only engages when the query is purely plain terms in path mode with no
// other constraints that an earlier, cheaper strategy already covers.
func (vol *serviceVolumeIndex) broadPathScanCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive || !pq.MatchPath {
		return nil, false
	}
	if pq.Under != "" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.OrGroups) > 0 {
		return nil, false
	}
	terms := make([]string, 0, len(pq.Terms))
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		terms = append(terms, term)
	}
	if len(terms) == 0 {
		return nil, false
	}

	recordCount := vol.index.compactRecordCount()
	workers := min(max(1, recordCountWorkers(recordCount)), 16)
	if workers <= 1 {
		out := make([]int, 0, 256)
		for i := 0; i < recordCount; i++ {
			if i&1023 == 0 && queryCanceled(pq) {
				return nil, false
			}
			rec := vol.index.compactRecord(i)
			if rec.Deleted {
				continue
			}
			if vol.index.compactPathContainsAll(i, terms) {
				out = append(out, i)
			}
		}
		out = vol.withRecentCandidates(out, 0, func(rec CompactRecord) bool {
			id, ok := vol.idForFRN(rec.FRN)
			return ok && vol.index.compactPathContainsAll(id, terms)
		})
		sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
		return capBroadCandidates(out, pq), true
	}

	parts := make([][]int, workers)
	var canceled atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * recordCount / workers
		end := (w + 1) * recordCount / workers
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			local := make([]int, 0, 256)
			for i := start; i < end; i++ {
				if i&1023 == 0 && queryCanceled(pq) {
					canceled.Store(true)
					return
				}
				rec := vol.index.compactRecord(i)
				if rec.Deleted {
					continue
				}
				if vol.index.compactPathContainsAll(i, terms) {
					local = append(local, i)
				}
			}
			parts[w] = local
		}(w, start, end)
	}
	wg.Wait()
	if canceled.Load() {
		return nil, false
	}

	total := 0
	for _, p := range parts {
		total += len(p)
	}
	out := make([]int, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	out = vol.withRecentCandidates(out, 0, func(rec CompactRecord) bool {
		id, ok := vol.idForFRN(rec.FRN)
		return ok && vol.index.compactPathContainsAll(id, terms)
	})
	sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
	return capBroadCandidates(out, pq), true
}

// boundedScanCandidates is the universal candidate floor. It accepts any query
// shape by scanning a bounded compact-record order and evaluating the same
// predicate the shared verifier uses. Specialized postings can beat this, but
// no query should need to fall through to older per-term reconstruction routes.
func (vol *serviceVolumeIndex) boundedScanCandidates(pq parsedQuery) ([]int, bool) {
	return vol.boundedScanCandidatesFiltered(pq, nil)
}

// boundedScanCandidatesFiltered is boundedScanCandidates with an optional cheap
// posting membership pre-filter.  Records outside the filter are skipped before
// the full entry verification, so a count or non-order-ready scan for a query
// with a cheap exact superset (ext:, a bounded type:dir subtree, or a required
// regex literal run) touches only the candidate subset instead of every record.
func (vol *serviceVolumeIndex) boundedScanCandidatesFiltered(pq parsedQuery, filter *boundedScanMembershipFilter) ([]int, bool) {
	if vol == nil || vol.index == nil {
		return nil, false
	}
	if filter != nil && filter.members == nil && filter.source.hasPosting {
		ids := filter.source.posting.materialize()
		filter.members = make(map[int]struct{}, len(ids))
		for _, id := range ids {
			filter.members[int(id)] = struct{}{}
		}
	}
	recordCount := vol.index.compactRecordCount()
	if recordCount == 0 {
		return []int{}, true
	}
	order := vol.orderForQuery(pq)
	limit := pq.Limit
	contentQuery := queryHasAnyContentLeaf(pq)
	var contentMatcher *contentLeafMatcher
	if contentQuery {
		contentMatcher = newContentLeafMatcher(pq)
	}
	// Bias re-ranks the verified candidates: the bias root's subtree moves to
	// the front, in scan order, and the caller re-applies the same stable bias
	// to the candidate slice. The scan can therefore still stop once it has the
	// top of the biased order -- the root's matches first, then the rest -- so
	// it only needs to defer the non-root matches it has already collected.
	// A post-verify relevance sort is the one order the caller cannot rebuild
	// from a prefix, so it keeps the full candidate set. contentFullCandidates
	// selects the historical full-candidate arm for differential testing, which
	// must scan before re-ranking. A pending overlay hidden set also disables
	// the bias early stop: scanCandidateMatches sees the base record only, so a
	// hidden base match would fill the root page and the caller's verify loop
	// would drop it, under-filling the result with no way to recover. The guard
	// applies to the biased case only (non-biased early stop is unchanged).
	biased := pq.RootBias != "" || pq.CWDBias != ""
	canStopAtLimit := !pq.CountOnly && limit > 0 &&
		(!biased || (!pq.contentFullCandidates && pq.SortColumn != "relevance" && vol.snapshotHiddenBaseIDs().empty()))
	if canStopAtLimit {
		biasRoot := ""
		if biased {
			biasRoot = firstNonEmpty(pq.CWDBias, pq.RootBias)
		}
		out := make([]int, 0, min(limit, 1024))
		var deferred []int
		cache := make(map[int]string)
		visited := 0
		for pos := 0; pos < compactUint32OrderLen(order, recordCount); pos++ {
			if pos&1023 == 0 && queryCanceled(pq) {
				return nil, false
			}
			// A content scan can walk the whole volume when matches are sparse,
			// so it is bounded in visited records and in path-cache size. On
			// reaching the budget, return what was found and say it is
			// incomplete rather than scan forever. The non-content path is
			// unchanged (both checks are gated on contentQuery).
			if contentQuery && visited >= contentScanVisitBudgetOf(pq) {
				pq.Trace.setContentIncomplete()
				break
			}
			if contentQuery {
				cache = boundContentPathCache(cache)
			}
			id := compactUint32OrderAt(order, pos)
			if filter != nil && !filter.contains(id) {
				continue
			}
			if contentQuery {
				visited++
			}
			if !scanCandidateMatches(vol, vol.index, pq, id, cache, contentMatcher) {
				continue
			}
			if biasRoot != "" && !pathUnder(vol.index.reconstructCompactPathCached(id, cache), biasRoot) {
				// Outside the biased subtree: every root match precedes it, so
				// hold it until the root's matches are exhausted (or the page
				// fills without it).
				if len(deferred) < limit {
					deferred = append(deferred, id)
				}
				continue
			}
			out = append(out, id)
			if len(out) >= limit {
				return out, true
			}
		}
		if len(out) < limit {
			out = append(out, deferred...)
			if len(out) > limit {
				out = out[:limit]
			}
		}
		return out, true
	}

	if contentQuery {
		// A count, or a biased scan whose caller re-ranks after verification,
		// cannot stop early, and content verification drops candidates, so
		// materialize the whole matching set up to the memory budget. Past
		// either budget the superset is incomplete: mark it rather than
		// silently truncate. A sparse-match scan can visit the whole volume
		// while producing few matches, so the visited-record budget is enforced
		// independently of the match budget, and the path memo is reset past a
		// fixed cap to keep memory O(budget).
		budget := contentCandidateBudgetOf(pq)
		visitBudget := contentScanVisitBudgetOf(pq)
		out := make([]int, 0, min(recordCount, 1024))
		cache := make(map[int]string)
		visited := 0
		incomplete := false
		for pos := 0; pos < compactUint32OrderLen(order, recordCount); pos++ {
			if pos&1023 == 0 && queryCanceled(pq) {
				return nil, false
			}
			if visited >= visitBudget {
				incomplete = true
				break
			}
			id := compactUint32OrderAt(order, pos)
			if filter != nil && !filter.contains(id) {
				continue
			}
			visited++
			cache = boundContentPathCache(cache)
			if _, ok := compactCandidateEntryIfMatchIn(vol, vol.index, pq, id, cache, true, false, contentMatcher); !ok {
				continue
			}
			if len(out) >= budget {
				incomplete = true
				break
			}
			out = append(out, id)
		}
		if incomplete {
			pq.Trace.setContentIncomplete()
		}
		return out, true
	}

	workers := min(max(1, recordCountWorkers(recordCount)), 16)
	if workers <= 1 || recordCount < 8192 {
		out := make([]int, 0, min(recordCount, 1024))
		cache := make(map[int]string)
		for pos := 0; pos < compactUint32OrderLen(order, recordCount); pos++ {
			if pos&1023 == 0 && queryCanceled(pq) {
				return nil, false
			}
			id := compactUint32OrderAt(order, pos)
			if filter != nil && !filter.contains(id) {
				continue
			}
			if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, cache, true, false); ok {
				out = append(out, id)
			}
		}
		return out, true
	}

	orderLen := compactUint32OrderLen(order, recordCount)
	parts := make([][]int, workers)
	var canceled atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * orderLen / workers
		end := (w + 1) * orderLen / workers
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			local := make([]int, 0, 256)
			cache := make(map[int]string)
			for pos := start; pos < end; pos++ {
				if pos&1023 == 0 && queryCanceled(pq) {
					canceled.Store(true)
					return
				}
				id := compactUint32OrderAt(order, pos)
				if filter != nil && !filter.contains(id) {
					continue
				}
				if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, cache, true, false); ok {
					local = append(local, id)
				}
			}
			parts[w] = local
		}(w, start, end)
	}
	wg.Wait()
	if canceled.Load() {
		return nil, false
	}
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

func (vol *serviceVolumeIndex) boundedScanCandidatesHiddenTop(pq parsedQuery, hidden hiddenBaseIDs, limit int) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 {
		return nil, false
	}
	recordCount := vol.index.compactRecordCount()
	order := vol.orderForQuery(pq)
	out := make([]int, 0, min(limit, 1024))
	cache := make(map[int]string)
	for pos := 0; pos < compactUint32OrderLen(order, recordCount); pos++ {
		if pos&1023 == 0 && queryCanceled(pq) {
			return nil, false
		}
		id := compactUint32OrderAt(order, pos)
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, cache, true, false); !ok {
			continue
		}
		out = append(out, id)
		if len(out) >= limit {
			break
		}
	}
	return out, true
}

// boundedScanMembershipFilter pre-filters the bounded name/id-order scan to a
// selective ext/glob-ext posting so broad queries with a cheap extension filter
// do not walk every record.  The scan order is unchanged (id order when the
// resident name order is absent, name order otherwise), so the top-N semantics
// of boundedScanCandidatesHiddenTop are preserved exactly.
type boundedScanMembershipFilter struct {
	source  candidatePlanSource
	members map[int]struct{}
}

func (f *boundedScanMembershipFilter) contains(id int) bool {
	if f == nil || f.members == nil {
		return true
	}
	_, ok := f.members[id]
	return ok
}

// boundedScanCandidatesHiddenTopFiltered is boundedScanCandidatesHiddenTop with
// an optional cheap posting membership pre-filter.  Records outside the filter
// are skipped without verifying the full entry, so a query like "test ext:py"
// scans only the .py subset of the volume while preserving the exact id/name
// order of the unfiltered bounded scan.
func (vol *serviceVolumeIndex) boundedScanCandidatesHiddenTopFiltered(pq parsedQuery, hidden hiddenBaseIDs, limit int, filter *boundedScanMembershipFilter) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 {
		return nil, false
	}
	if filter != nil && filter.members == nil && filter.source.hasPosting {
		ids := filter.source.posting.materialize()
		filter.members = make(map[int]struct{}, len(ids))
		for _, id := range ids {
			filter.members[int(id)] = struct{}{}
		}
	}
	recordCount := vol.index.compactRecordCount()
	order := vol.orderForQuery(pq)
	out := make([]int, 0, min(limit, 1024))
	cache := make(map[int]string)
	for pos := 0; pos < compactUint32OrderLen(order, recordCount); pos++ {
		if pos&1023 == 0 && queryCanceled(pq) {
			return nil, false
		}
		id := compactUint32OrderAt(order, pos)
		if filter != nil && !filter.contains(id) {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, cache, true, false); !ok {
			continue
		}
		out = append(out, id)
		if len(out) >= limit {
			break
		}
	}
	return out, true
}

// boundedScanPrefilter tries the cheap exact superset pre-filters for the
// bounded scan in priority order: ext:/glob-ext:, a bounded type:dir subtree,
// and a required regex literal run.  It returns (exactEmpty, filterOK):
// exactEmpty means the query provably matches nothing on this volume (caller
// short-circuits), filterOK means a membership filter was set, and neither
// means no filter applies and the unfiltered scan runs.
func (vol *serviceVolumeIndex) boundedScanPrefilter(pq parsedQuery, filter **boundedScanMembershipFilter) (exactEmpty, filterOK bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive {
		return false, false
	}
	if source, hasSource := vol.planExtFilterSource(pq); hasSource {
		*filter = &boundedScanMembershipFilter{source: source}
		return false, true
	}
	if dirFilter, hasDirFilter := vol.planDirSubtreeFilter(pq); hasDirFilter {
		*filter = dirFilter
		return false, true
	}
	if regexFilter, regexEmpty, hasRegexFilter := vol.planRegexLiteralFilter(pq); hasRegexFilter {
		if regexEmpty {
			return true, true
		}
		*filter = regexFilter
		return false, true
	}
	return false, false
}

// planExtFilterSource returns a candidatePlanSource whose posting is a cheap,
// exact superset pre-filter (ext: or glob-ext:) for the query, when one exists.
func (vol *serviceVolumeIndex) planExtFilterSource(pq parsedQuery) (candidatePlanSource, bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive {
		return candidatePlanSource{}, false
	}
	for _, ext := range pq.Exts {
		if candidate, ok := vol.extPostingCountCandidate(ext); ok {
			// Cap the posting so a very common extension (e.g. .dll on a huge
			// index) does not materialize a multi-hundred-MB membership map.
			// Above the cap the unfiltered scan is no worse, and skipping the
			// filter avoids a pathological memory spike.
			if candidate.len() > serviceComponentMultiTermScanMaxIDs {
				continue
			}
			return candidatePlanSource{posting: candidate, hasPosting: true}, true
		}
	}
	if globExts, ok := simpleGlobExts(pq.Globs); ok && len(globExts) == 1 {
		if candidate, ok := vol.extPostingCountCandidate(globExts[0]); ok {
			if candidate.len() > serviceComponentMultiTermScanMaxIDs {
				return candidatePlanSource{}, false
			}
			return candidatePlanSource{posting: candidate, hasPosting: true}, true
		}
	}
	return candidatePlanSource{}, false
}

// regexRequiredLiteral finds a literal run that is guaranteed to appear in any
// match of the regex: a depth-0 run not inside a top-level alternation and not
// made optional by a trailing `?`/`*`/`{` quantifier.  Such a run is a safe
// superset pre-filter for the bounded scan (every matching record's path must
// contain it).  Returns "" when no required literal can be proven.
func regexRequiredLiteral(pat string) string {
	pat = strings.TrimPrefix(pat, "(?i)")
	depth := 0
	required := ""
	var run []byte
	escaped := false
	inClass := false
	flush := func() {
		if depth == 0 && len(run) >= 2 {
			required = string(run)
		}
		run = run[:0]
	}
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		if escaped {
			escaped = false
			// An escaped literal rune (e.g. \_) continues the run; any other
			// escape ends it.  Note this branch is reached with c == the
			// escaped character.
			if isRegexLiteralRune(rune(c)) && depth == 0 {
				run = append(run, c)
			} else {
				flush()
			}
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '[':
			flush()
			inClass = true
		case ']':
			inClass = false
		case '(', '|':
			flush()
			if c == '(' {
				depth++
			}
		case ')':
			flush()
			if depth > 0 {
				depth--
			}
		case '?', '*', '+', '{':
			// A quantifier makes the preceding run optional/repeatable; a run
			// with a preceding quantifier is not provably required.
			flush()
		case '.':
			if i+1 < len(pat) && (pat[i+1] == '*' || pat[i+1] == '+') {
				flush()
				i++
				continue
			}
			flush()
		default:
			if !inClass && isRegexLiteralRune(rune(c)) {
				if depth == 0 {
					run = append(run, c)
				} else {
					flush()
				}
			} else {
				flush()
			}
		}
	}
	flush()
	return required
}

// planRegexLiteralFilter builds a bounded-scan membership pre-filter from a
// required regex literal run.  A record matching the regex must contain the
// run in its path, so the run's path posting is an exact superset of the match
// set; the bounded scan then only regex-verifies those records.  This turns
// rare-match regexes like `regex:README\.(md|txt)$` from a full-volume scan
// (minutes) into a scan of just the README-containing records (sub-second).
// empty reports that no record contains the required literal, so the query is
// provably a zero-match on this volume and the caller can short-circuit.
func (vol *serviceVolumeIndex) planRegexLiteralFilter(pq parsedQuery) (filter *boundedScanMembershipFilter, empty bool, ok bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive || len(pq.Regexps) != 1 ||
		len(pq.RegexTerms) == 0 || !pq.MatchPath {
		return nil, false, false
	}
	disjuncts := regexRequiredLiteralAlternatives(pq.Regexps[0].String())
	if len(disjuncts) == 0 {
		return nil, false, false
	}
	members := make(map[int]struct{}, 64)
	kept := 0
	for _, runs := range disjuncts {
		hasUsable := false
		var best []int
		for _, run := range runs {
			if len(run) < 3 || strings.ContainsAny(run, `\/*?[]:`) {
				continue
			}
			hasUsable = true
			ids := vol.pathTermPosting(run)
			if len(ids) == 0 {
				// This run is absent from the volume; another run (or another
				// disjunct) may still narrow the scan, so do not treat it as an
				// empty query here.
				continue
			}
			if best == nil || len(ids) < len(best) {
				best = ids
			}
		}
		if !hasUsable {
			// This disjunct has no run we can prove required.  Skipping it
			// would let a match through with none of the chosen literals, so
			// no sound filter can be built from the pattern.
			return nil, false, false
		}
		if best == nil {
			// Every usable run of this disjunct is absent, so this branch can
			// never match.  The union of the remaining branches is still an
			// exact superset of the match set.
			continue
		}
		if len(members)+len(best) > serviceComponentMultiTermScanMaxIDs {
			return nil, false, false
		}
		for _, id := range best {
			members[id] = struct{}{}
		}
		kept++
	}
	if kept == 0 {
		// Every disjunct was proven impossible, so the query matches nothing on
		// this volume.
		return nil, true, true
	}
	return &boundedScanMembershipFilter{members: members}, false, true
}

// planDirSubtreeFilter builds a membership pre-filter for `type:dir <term>`
// path queries: every matching directory is either a `term`-named directory or
// a descendant of one, so the union of each term root's descendant interval is
// an exact superset of the match set.  The filter is only built when the total
// descendant count is bounded, so a huge `term` subtree (e.g. a root directory)
// falls back to the unfiltered scan rather than materializing millions of ids.
// The scan order is unchanged, preserving bounded-scan top-N semantics.
func (vol *serviceVolumeIndex) planDirSubtreeFilter(pq parsedQuery) (*boundedScanMembershipFilter, bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive || pq.Type != "dir" ||
		len(pq.Terms) != 1 || len(pq.Exts) != 0 || len(pq.Globs) != 0 ||
		len(pq.Dirs) != 0 || len(pq.Regexps) != 0 || len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 ||
		len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0 || len(pq.AttrFilters) != 0 ||
		pq.Under != "" || pq.HasModAfter || pq.Exists {
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
	roots := vol.pathTermRootIDs(term)
	if len(roots) == 0 {
		return nil, false
	}
	if len(vol.subtreeOrder) == 0 {
		return nil, false
	}
	total := 0
	for _, root := range roots {
		if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start != ^uint32(0) && start <= end {
			total += int(end - start)
			if total > serviceComponentTrigramExpansionMaxIDs {
				return nil, false
			}
		}
	}
	if total == 0 {
		return nil, false
	}
	members := make(map[int]struct{}, total)
	for _, root := range roots {
		if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		for _, id32 := range vol.subtreeOrder[start:end] {
			members[int(id32)] = struct{}{}
		}
	}
	return &boundedScanMembershipFilter{members: members}, true
}
