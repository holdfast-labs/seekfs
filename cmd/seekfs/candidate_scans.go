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
	if vol.pathTermSatisfiedByVolume(term) {
		// Volume-satisfied terms (UNC volume substrings) match every path;
		// the gram-emptiness proof and bounded sources below only see names
		// and would falsely empty or underfill the plan.
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
			if vol.index.compactPathContainsAllFolded(i, terms, pq) {
				out = append(out, i)
			}
		}
		out = vol.withRecentCandidates(out, 0, func(rec CompactRecord) bool {
			id, ok := vol.idForFRN(rec.FRN)
			return ok && vol.index.compactPathContainsAllFolded(id, terms, pq)
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
				if vol.index.compactPathContainsAllFolded(i, terms, pq) {
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
		return ok && vol.index.compactPathContainsAllFolded(id, terms, pq)
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
	if filter != nil && filter.bits == nil && filter.source.hasPosting {
		ids := filter.source.posting.materialize()
		nf := newBoundedScanMembershipFilter(vol.index.compactRecordCount())
		for _, id := range ids {
			nf.add(int(id))
		}
		filter.bits, filter.count, filter.n = nf.bits, nf.count, nf.n
	}
	// Name-memo lane: the full ordered scan with bit checks instead of
	// per-candidate path verification. Hidden exclusion does not apply on
	// this floor (callers without snapshots filter afterwards), matching
	// the legacy contract exactly.
	if ids, ok := vol.memoScanAll(pq, hiddenBaseIDs{}, filter); ok {
		return ids, true
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
	// the early stop for every query shape: scanCandidateMatches sees the base
	// record only, so a hidden base match would fill the page and the caller's
	// verify loop would drop it, under-filling the result with no way to
	// recover. The guard is the caller's captured hidden set (threaded through
	// pq), so the scan and the verify loop drop exactly the same records; it is
	// a cheap length check and is evaluated last, so the common no-overlay path
	// pays nothing.
	biased := pq.RootBias != "" || pq.CWDBias != ""
	canStopAtLimit := !pq.CountOnly && limit > 0 &&
		(!biased || (!pq.contentFullCandidates && pq.SortColumn != "relevance")) &&
		pq.hidden.empty()
	if canStopAtLimit {
		biasRoot := ""
		if biased {
			biasRoot = firstNonEmpty(pq.CWDBias, pq.RootBias)
		}
		// A content scan that may stop at the page fans out to an ordered
		// parallel walk: the feeder streams record order, workers verify,
		// and the merger emits the first limit matches in order, stopping
		// there like the serial walk (over-verification past an early
		// limit is bounded by the channel buffer plus one job per
		// worker). Biased queries keep the serial deferred-partition
		// walk, and name/scalar scans keep theirs; both are unchanged.
		if contentQuery && contentMatcher != nil && biasRoot == "" &&
			contentParallelWorkers() > 1 &&
			compactUint32OrderLen(order, recordCount) >= contentParallelVerifyMinIDs {
			return vol.boundedScanCandidatesParallel(pq, order, recordCount, limit, filter, contentMatcher)
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
				cache = boundPathCache(cache)
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
			cache = boundPathCache(cache)
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

// boundedScanCandidatesParallel is the parallel form of the bounded scan's
// early-stop walk (canStopAtLimit without bias): the first limit matches in
// scan order, verified across workers. The feeder is the only goroutine
// that walks the order (applying the membership pre-filter and the
// visited-record budget, marking the trace incomplete past it); workers
// run the full scan predicate; the merger emits in order and stops at the
// limit. Proven identical to the serial walk by
// TestContentParallelScanMatchesSerial across shapes x limits.
func (vol *serviceVolumeIndex) boundedScanCandidatesParallel(pq parsedQuery, order []uint32, recordCount, limit int, filter *boundedScanMembershipFilter, matcher *contentLeafMatcher) ([]int, bool) {
	orderLen := compactUint32OrderLen(order, recordCount)
	workers := contentParallelWorkers()
	if workers < 2 || orderLen == 0 || limit <= 0 {
		return nil, false
	}
	visitBudget := contentScanVisitBudgetOf(pq)
	quit := make(chan struct{})
	var quitOnce sync.Once
	stop := func() { quitOnce.Do(func() { close(quit) }) }
	defer stop()
	type scanJob struct {
		seq uint64
		pos int
	}
	type scanResult struct {
		seq uint64
		id  int
		ok  bool
	}
	work := make(chan scanJob, 64)
	out := make(chan scanResult, 64)
	var wg sync.WaitGroup
	var visited atomic.Int64
	var canceled atomic.Bool
	// Feeder: the only goroutine that walks the order.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(work)
		var seq uint64
		for pos := 0; pos < orderLen; pos++ {
			if pos&1023 == 0 && queryCanceled(pq) {
				canceled.Store(true)
				return
			}
			id := compactUint32OrderAt(order, pos)
			if filter != nil && !filter.contains(id) {
				continue
			}
			if visited.Add(1) > int64(visitBudget) {
				pq.Trace.setContentIncomplete()
				return
			}
			select {
			case work <- scanJob{seq: seq, pos: pos}:
				seq++
			case <-quit:
				return
			}
		}
	}()
	// Workers: verify only; no ordering, no limits here.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			localCache := make(map[int]string)
			var done int
			for {
				select {
				case job, ok := <-work:
					if !ok {
						return
					}
					done++
					if done&1023 == 0 && queryCanceled(pq) {
						canceled.Store(true)
						return
					}
					id := compactUint32OrderAt(order, job.pos)
					localCache = boundPathCache(localCache)
					res := scanResult{seq: job.seq, id: id}
					if scanCandidateMatches(vol, vol.index, pq, id, localCache, matcher) {
						res.ok = true
					}
					select {
					case out <- res:
					case <-quit:
						return
					}
				case <-quit:
					return
				}
			}
		}()
	}
	// Closer: once the feeder and every worker are done, no more results.
	go func() {
		wg.Wait()
		close(out)
	}()
	// Merger (this goroutine): emit in scan order, stopping at exactly
	// the limit like the serial walk.
	ids := make([]int, 0, min(limit, 1024))
	pending := make(map[uint64]scanResult)
	var expect uint64
	emit := func() bool {
		for limit <= 0 || len(ids) < limit {
			res, ok := pending[expect]
			if !ok {
				return true
			}
			delete(pending, expect)
			expect++
			if res.ok {
				ids = append(ids, res.id)
			}
		}
		return false
	}
	for res := range out {
		pending[res.seq] = res
		if !emit() {
			return ids, true
		}
	}
	emit()
	if canceled.Load() {
		return nil, false
	}
	return ids, true
}

func (vol *serviceVolumeIndex) boundedScanCandidatesHiddenTop(pq parsedQuery, hidden hiddenBaseIDs, limit int) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 {
		return nil, false
	}
	return vol.boundedScanHiddenTop(pq, hidden, limit, nil)
}

// boundedScanSearchCandidates is the top-N floor for single-volume searches:
// cheap exact-superset pre-filters (ext posting, dir subtree, required regex
// literal runs, trigram exact-empty) narrow the scan, and the scan itself
// stops at the limit in scan order. It mirrors the global bounded lane's
// per-volume logic, so order and exactness match the exhaustive scan the
// verify loop would otherwise truncate to the same limit. Count queries,
// biased queries (whose downstream re-ranking needs the full set), and
// content queries (whose inline verification needs its own budgets) keep the
// exhaustive floor.
func (vol *serviceVolumeIndex) boundedScanSearchCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CountOnly || pq.Limit <= 0 ||
		pq.RootBias != "" || pq.CWDBias != "" || queryHasAnyContentLeaf(pq) {
		return vol.boundedScanCandidates(pq)
	}
	// Posting-derived pre-filters describe base records; uncommitted
	// recent/trigram-recent additions are merged by the scan itself, so only
	// filter when there is nothing uncommitted to miss (mirrors
	// plainTermNameTrigramExactEmpty's recentIDs gate).
	if len(vol.recentIDs) == 0 && len(vol.nameTrigramRecent) == 0 {
		var filter *boundedScanMembershipFilter
		exactEmpty, filterOK := vol.boundedScanPrefilter(pq, &filter)
		if exactEmpty {
			return []int{}, true
		}
		if filterOK {
			return vol.boundedScanCandidatesHiddenTopFiltered(pq, pq.hidden, pq.Limit, filter)
		}
	}
	return vol.boundedScanCandidatesHiddenTop(pq, pq.hidden, pq.Limit)
}

// boundedScanMembershipFilter pre-filters the bounded name/id-order scan to a
// selective ext/glob-ext posting so broad queries with a cheap extension filter
// do not walk every record.  The scan order is unchanged (id order when the
// resident name order is absent, name order otherwise), so the top-N semantics
// of boundedScanCandidatesHiddenTop are preserved exactly.
//
// Membership is a bitset over record IDs, not a map: the scan probes every
// position, so a map lookup per position dominates the check it guards. A
// bitset probe is a bounds check plus one word load.
type boundedScanMembershipFilter struct {
	source candidatePlanSource
	bits   []uint64
	count  int
	n      int
}

func newBoundedScanMembershipFilter(recordCount int) *boundedScanMembershipFilter {
	return &boundedScanMembershipFilter{
		bits: make([]uint64, (recordCount+63)/64),
		n:    recordCount,
	}
}

func (f *boundedScanMembershipFilter) add(id int) {
	if f == nil || id < 0 || id >= f.n {
		return
	}
	w := id / 64
	b := uint64(1) << uint(id%64)
	if f.bits[w]&b == 0 {
		f.bits[w] |= b
		f.count++
	}
}

func (f *boundedScanMembershipFilter) len() int {
	if f == nil {
		return 0
	}
	return f.count
}

func (f *boundedScanMembershipFilter) contains(id int) bool {
	if f == nil || f.bits == nil {
		return true
	}
	if id < 0 || id >= f.n {
		return false
	}
	return f.bits[id/64]&(uint64(1)<<uint(id%64)) != 0
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
	if filter != nil && filter.bits == nil && filter.source.hasPosting {
		ids := filter.source.posting.materialize()
		nf := newBoundedScanMembershipFilter(vol.index.compactRecordCount())
		for _, id := range ids {
			nf.add(int(id))
		}
		filter.bits, filter.count, filter.n = nf.bits, nf.count, nf.n
	}
	return vol.boundedScanHiddenTop(pq, hidden, limit, filter)
}

// boundedScanHiddenTop is the shared first-limit-in-scan-order scan behind
// boundedScanCandidatesHiddenTop(Filtered). A serial prefix preserves the
// cheap dense-hit early stop (matches clustered early in the order return
// without spawning workers); only when the prefix underfills does the
// remainder fan out. Ranges are contiguous and ordered, so taking the
// prefix hits plus workers' hits greedily in partition order yields
// exactly the serial prefix set.
func (vol *serviceVolumeIndex) boundedScanHiddenTop(pq parsedQuery, hidden hiddenBaseIDs, limit int, filter *boundedScanMembershipFilter) ([]int, bool) {
	recordCount := vol.index.compactRecordCount()
	order := vol.orderForQuery(pq)
	orderLen := compactUint32OrderLen(order, recordCount)
	// Name-memo lane first: bit checks plus scalar record reads replace
	// posting decode and per-candidate path verification, preserving the
	// order, hidden, filter, limit and cancellation contract exactly.
	if ids, ok := vol.memoScanTop(pq, hidden, limit, filter, order, orderLen); ok {
		return ids, true
	}
	const serialPrefixPositions = 4 * serviceTrigramParallelVerifyMinIDs
	// A narrowing membership filter makes most positions a cheap map
	// lookup, so the serial early stop beats parallel fan-out; parallelize
	// only the unfiltered scan, where every position pays full entry
	// verification.
	if filter != nil || orderLen < 2*serviceTrigramParallelVerifyMinIDs {
		return vol.boundedScanHiddenTopRange(pq, hidden, limit, filter, order, 0, orderLen, nil, nil, 0)
	}
	prefixEnd := min(orderLen, serialPrefixPositions)
	out, ok := vol.boundedScanHiddenTopRange(pq, hidden, limit, filter, order, 0, prefixEnd, nil, nil, 0)
	if !ok || len(out) >= limit || prefixEnd >= orderLen {
		return out, ok
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, (orderLen-prefixEnd)/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		rest, ok := vol.boundedScanHiddenTopRange(pq, hidden, limit-len(out), filter, order, prefixEnd, orderLen, nil, nil, 0)
		if !ok {
			return nil, false
		}
		return append(out, rest...), true
	}
	var stopped atomic.Bool
	counts := make([]atomic.Int64, workers)
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := prefixEnd + w*(orderLen-prefixEnd)/workers
		end := prefixEnd + (w+1)*(orderLen-prefixEnd)/workers
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			parts[w], _ = vol.boundedScanHiddenTopRange(pq, hidden, limit, filter, order, start, end, &stopped, counts, w)
		}(w, start, end)
	}
	wg.Wait()
	if stopped.Load() {
		return nil, false
	}
	for _, part := range parts {
		for _, id := range part {
			out = append(out, id)
			if len(out) >= limit {
				return out, true
			}
		}
	}
	return out, true
}

// boundedScanHiddenTopRange scans one [start, end) slice of the scan order,
// collecting up to limit matches. It is the serial whole-order path as well
// as the per-worker parallel path. In the parallel path, counts/workerIndex
// implement cooperative early termination: worker k aborts once strictly
// earlier workers have already banked limit matches, because the greedy
// ordered merge would cut everything worker k finds anyway. This keeps the
// dense-hit cost at the serial early-stop level while sparse scans still
// fan out.
func (vol *serviceVolumeIndex) boundedScanHiddenTopRange(pq parsedQuery, hidden hiddenBaseIDs, limit int, filter *boundedScanMembershipFilter, order []uint32, start, end int, stopped *atomic.Bool, counts []atomic.Int64, workerIndex int) ([]int, bool) {
	out := make([]int, 0, min(limit, 1024))
	cache := make(map[int]string)
	for pos := start; pos < end; pos++ {
		if pos&1023 == 0 && (stopped != nil && stopped.Load() || queryCanceled(pq)) {
			if stopped != nil {
				stopped.Store(true)
			}
			return nil, false
		}
		if counts != nil && pos&1023 == 0 && pos > start {
			var earlier int64
			for k := 0; k < workerIndex && earlier < int64(limit); k++ {
				earlier += counts[k].Load()
			}
			if earlier >= int64(limit) {
				return out, true
			}
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
		if counts != nil {
			counts[workerIndex].Add(1)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, true
}

// plainTermNameTrigramExactEmpty reports whether a top-level plain term
// provably matches nothing on this volume: some required name gram is
// absent from both the selective index and the companion (the same
// exact-zero proof globalNameAllExactEmpty uses, but metadata-only -- no
// posting iterator is constructed, so a miss costs map lookups only).
// Every path-mode match must contain each top-level term as a substring of
// its full path (entryMatches enforces containsAll over pq.Terms), and every
// path component is some record's name, so a term absent from all names
// cannot match any path. Only case-insensitive ASCII terms qualify, and
// volumes with overlay additions are skipped because recent record names are
// not covered by the base gram counts.
func (vol *serviceVolumeIndex) plainTermNameTrigramExactEmpty(pq parsedQuery) bool {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CaseSensitive || pq.Fuzzy {
		return false
	}
	if len(vol.recentIDs) > 0 {
		return false
	}
	for _, term := range nonVolumeTerms(pq.Terms) {
		if len(term) < 3 || strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		if strings.IndexFunc(term, func(r rune) bool { return r > 127 }) >= 0 || vol.plainTermMayOccurInPathPrefix(term) {
			continue
		}
		grams := vol.nameTrigramIndex().termGramKeys(term)
		if len(grams) == 0 && vol.index.Derived.SelfNameTrigrams != nil {
			grams = vol.index.Derived.SelfNameTrigrams.termGramKeys(term)
		}
		for _, gram := range grams {
			if _, _, _, _, exactEmpty := vol.nameGramPosting(gram); exactEmpty {
				return true
			}
		}
	}
	return false
}

// planPlainTermPathFilter builds a membership pre-filter for bounded scans
// of plain-term path queries from the trigram path-term posting (name matches
// plus descendant expansion, the same source the single-volume lane plans
// from). Every path-mode match must contain each top-level term in its full
// path, so the smallest term posting is an exact superset of the match set,
// and an empty posting proves the volume cannot match. The posting is only
// attempted when the rarest required gram is selective (metadata-only
// check): intersecting and expanding a common term costs more than the
// unfiltered scan it would narrow, so common terms decline exactly as if
// the filter did not exist. Scan order is unchanged, preserving
// bounded-scan top-N semantics exactly like the ext/dir/regex filters.
func (vol *serviceVolumeIndex) planPlainTermPathFilter(pq parsedQuery) (filter *boundedScanMembershipFilter, empty bool, ok bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CaseSensitive || pq.Fuzzy {
		return nil, false, false
	}
	if len(vol.recentIDs) > 0 {
		return nil, false, false
	}
	var best []int
	for _, term := range nonVolumeTerms(pq.Terms) {
		if len(term) < 3 || strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		if strings.IndexFunc(term, func(r rune) bool { return r > 127 }) >= 0 || vol.plainTermMayOccurInPathPrefix(term) {
			continue
		}
		if !vol.plainTermPostingWorthBuilding(term) {
			continue
		}
		ids, ok := vol.completeNameTrigramPathTermPosting(term)
		if !ok {
			continue
		}
		if len(ids) == 0 {
			return nil, true, true
		}
		if best == nil || len(ids) < len(best) {
			best = ids
		}
	}
	if best == nil || len(best) > serviceComponentMultiTermScanMaxIDs {
		return nil, false, false
	}
	filter = newBoundedScanMembershipFilter(vol.index.compactRecordCount())
	for _, id := range best {
		filter.add(id)
	}
	return filter, false, true
}

// plainTermMayOccurInPathPrefix protects name-only trigram proofs from path
// components supplied by index metadata. Reconstructed paths start at Volume
// or Roots[0], whose components need not have corresponding compact records.
func (vol *serviceVolumeIndex) plainTermMayOccurInPathPrefix(term string) bool {
	if vol == nil || vol.index == nil || term == "" {
		return false
	}
	if containsFoldASCII(vol.index.Volume, term) {
		return true
	}
	return len(vol.index.Roots) > 0 && containsFoldASCII(vol.index.Roots[0], term)
}

// pathTermSatisfiedByVolume reports whether term holds on every reconstructed
// path through the volume prefix alone, mirroring compactPathContainsTerm's
// volume check (the record-name chain check there is the constraining part).
// Such a term is vacuous as a candidate source: any name/posting-derived
// subset misses the volume-only matches, so sourcing lanes must skip it when
// choosing multi-term drivers, or decline single-term drivers and let the
// verified scan decide. Verification still re-checks the term (vacuously
// true), so skipping only widens candidates, never results.
func (vol *serviceVolumeIndex) pathTermSatisfiedByVolume(term string) bool {
	return vol != nil && vol.index != nil && vol.index.Volume != "" && term != "" &&
		containsFoldASCII(vol.index.Volume, term)
}

// plainTermPostingWorthBuilding reports whether intersecting a term's name
// grams is cheap enough to attempt: every required gram must be stored with
// a small posting. The intersection result is bounded by the rarest gram,
// so a term whose rarest gram already exceeds the expansion budget can only
// produce a filter too large to help (or a declined build after paying full
// intersect+expand cost); skipping it keeps the prefilter metadata-only.
func (vol *serviceVolumeIndex) plainTermPostingWorthBuilding(term string) bool {
	grams := vol.nameTrigramIndex().termGramKeys(term)
	if len(grams) == 0 && vol.index.Derived.SelfNameTrigrams != nil {
		grams = vol.index.Derived.SelfNameTrigrams.termGramKeys(term)
	}
	if len(grams) == 0 {
		return false
	}
	for _, gram := range grams {
		_, count, stored, _, exactEmpty := vol.nameGramPosting(gram)
		if exactEmpty {
			return true
		}
		if !stored || count > serviceComponentTrigramExpansionMaxIDs {
			return false
		}
	}
	return true
}

// boundedScanPrefilter tries the cheap exact superset pre-filters for the
// bounded scan in priority order: a trigram-proven empty plain term,
// ext:/glob-ext:, a bounded type:dir subtree, a required regex literal run,
// and finally the smallest plain-term path posting.  It returns
// (exactEmpty, filterOK): exactEmpty means the query provably matches
// nothing on this volume (caller short-circuits), filterOK means a membership
// filter was set, and neither means no filter applies and the unfiltered
// scan runs.
func (vol *serviceVolumeIndex) boundedScanPrefilter(pq parsedQuery, filter **boundedScanMembershipFilter) (exactEmpty, filterOK bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive {
		return false, false
	}
	if vol.accentUnionActive(pq) {
		// Gram-absence proofs and posting membership filters read unfolded
		// bytes: mark-stripping can create adjacencies (x+mark+y+z folds to
		// xyz) that unfolded proofs cannot disprove, and postings miss
		// accent names. Run the unfiltered scan; its verifier folds.
		return false, false
	}
	if vol.plainTermNameTrigramExactEmpty(pq) {
		return true, true
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
	if plainFilter, plainEmpty, hasPlainFilter := vol.planPlainTermPathFilter(pq); hasPlainFilter {
		if plainEmpty {
			return true, true
		}
		*filter = plainFilter
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
	filter = newBoundedScanMembershipFilter(vol.index.compactRecordCount())
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
		if filter.len()+len(best) > serviceComponentMultiTermScanMaxIDs {
			return nil, false, false
		}
		for _, id := range best {
			filter.add(id)
		}
		kept++
	}
	if kept == 0 {
		// Every disjunct was proven impossible, so the query matches nothing on
		// this volume.
		return nil, true, true
	}
	return filter, false, true
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
		pq.Under != "" || pq.HasModAfter || pq.lateVerified() {
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
	filter := newBoundedScanMembershipFilter(vol.index.compactRecordCount())
	for _, root := range roots {
		if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		for _, id32 := range vol.subtreeOrder[start:end] {
			filter.add(int(id32))
		}
	}
	return filter, true
}
