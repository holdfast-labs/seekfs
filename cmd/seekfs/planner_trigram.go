package main

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
)

func (vol *serviceVolumeIndex) nameTrigramCandidates(pq parsedQuery) ([]int, bool) {
	if pq.MatchPath {
		return vol.componentTrigramCandidates(pq)
	}
	return vol.filenameTrigramCandidates(pq)
}

func (vol *serviceVolumeIndex) filenameTrigramCandidates(pq parsedQuery) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	if vol == nil || vol.index == nil || (trigrams == nil && vol.index.Derived.SelfNameTrigrams == nil) || pq.CaseSensitive ||
		pq.MatchPath || pq.Under != "" || pq.Type != "" ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" {
		return nil, false
	}
	// Glob literals can supply the driving terms when the query has no plain
	// terms of its own (glob:*acme*), or join them.  Without this a
	// glob-only query is all-orphan and drops to the bounded scan.
	if len(pq.Terms) == 0 && len(pq.Globs) > 0 && !pqGramsCouldDriveGlobs(pq) {
		return nil, false
	}
	return vol.filenameNgramCandidates(pq, trigrams, serviceNameTrigramCandidateMaxIDs)
}

// pqGramsCouldDriveGlobs reports whether the query's globs can be driven by
// gram candidate generation: every glob must yield a literal run of at least
// three runes so a substring of it can select candidates, and the globs must
// be verifiable on the record name in the final fold.  A leading wildcard
// glob like "glob:*acme*" is a substring match, which the trigram lane
// answers exactly; the blanket "no globs in the fast lane" rule is what made
// it fall to a 22s bounded scan.
func pqGramsCouldDriveGlobs(pq parsedQuery) bool {
	literals := globLiteralTerms(pq.Globs, pq.CaseSensitive)
	if len(literals) == 0 {
		return false
	}
	for _, glob := range pq.Globs {
		if strings.ContainsAny(glob, `[]`) || strings.Contains(glob, "?") {
			// Character classes and single-char wildcards cannot be reduced
			// to a plain substring literal.
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) filenameNgramCandidates(pq parsedQuery, trigrams *compressedTrigramIndex, maxIDs int) ([]int, bool) {
	if vol == nil {
		return nil, false
	}
	if trigrams == nil {
		trigrams = vol.index.Derived.SelfNameTrigrams
	}
	if trigrams == nil {
		return nil, false
	}
	// Glob literals ride into the driving term set when the glob is a plain
	// substring pattern (glob:*acme*).  pq.Globs stays intact for the
	// final fold, so this only widens candidate selection, never weakens the
	// verification.  Without it, any query carrying a glob was forced to the
	// bounded scan even though a literal run inside it is exactly the kind of
	// substring the gram lane answers.
	if len(pq.Globs) > 0 && pqGramsCouldDriveGlobs(pq) {
		combined := append([]string(nil), pq.Terms...)
		seen := make(map[string]struct{}, len(combined))
		for _, t := range combined {
			seen[t] = struct{}{}
		}
		for _, gl := range globLiteralTerms(pq.Globs, pq.CaseSensitive) {
			if _, ok := seen[gl]; !ok {
				combined = append(combined, gl)
				seen[gl] = struct{}{}
			}
		}
		pq.Terms = combined
	}
	// Multi-term queries: the selective single-best-term lane verifies only
	// the best term, admitting false positives for the other terms.  When the
	// companion PNGC section is present, answer exactly by intersecting
	// postings across every term.  This is also the only exact lane for
	// common-gram queries whose grams are omitted from PNGR.
	if len(pq.Terms) >= 2 && vol.index != nil && vol.index.Derived.SelfNameTrigrams != nil &&
		vol.index.Derived.SelfNameTrigrams.mappedGrams != nil {
		if candidates, ok := vol.completeMultiTermNameGramCandidates(pq.Terms, maxIDs, pq); ok {
			return candidates, true
		}
	}
	bestTerm := ""
	bestCount := maxIDs + 1
	exactEmpty := false
	exactEmptyTerm := ""
	for _, term := range pq.Terms {
		if len(term) < max(3, trigrams.gramSize) {
			continue
		}
		if !asciiOnlyString(term) {
			return nil, false
		}
		termBest := maxIDs + 1
		termMissing := false
		termExactEmpty := false
		for _, gram := range trigrams.termGramKeys(strings.ToLower(term)) {
			_, count, stored, state, isExactEmpty := vol.nameGramPosting(gram)
			if isExactEmpty {
				termExactEmpty = true
				break
			}
			if !stored {
				termMissing = true
				if state == "omitted-common" || state == "missing-section" {
					break
				}
				continue
			}
			if count < termBest {
				termBest = count
			}
		}
		if termExactEmpty {
			exactEmpty = true
			exactEmptyTerm = term
			continue
		}
		if termMissing || termBest > maxIDs {
			continue
		}
		if termBest < bestCount {
			bestTerm = term
			bestCount = termBest
		}
	}
	if exactEmpty {
		// A complete PNGR count table proves that at least one required gram
		// has no base records.  Do not turn that fact into a recent-match or
		// bounded-scan fallback: overlays are merged by the caller, while the
		// persisted base candidate set is exactly empty.
		recent := vol.nameTrigramRecentMatches(exactEmptyTerm)
		pq.Trace.setSource("exact-empty", len(recent))
		return recent, true
	}
	if bestTerm == "" {
		for _, term := range pq.Terms {
			state := ""
			for _, gram := range trigrams.termGramKeys(strings.ToLower(term)) {
				_, _, _, gramState, _ := vol.nameGramPosting(gram)
				if gramState == "omitted-common" || gramState == "missing-section" {
					state = gramState
					break
				}
			}
			if state != "" {
				pq.Trace.setDecline("name-trigram:" + state)
				break
			}
		}
		// Selective lane declined (every gram over cap or omitted-common).
		if rescued, ok := vol.rescueWithCompleteGramLane(pq, maxIDs); ok {
			return rescued, true
		}
		return nil, false
	}
	candidates, ok := vol.nameNgramNameTermPosting(bestTerm, trigrams, maxIDs)
	if !ok {
		pq.Trace.setDecline("name-trigram:" + trigrams.lookupState(bestTerm))
		if rescued, rescuedOK := vol.rescueWithCompleteGramLane(pq, maxIDs); rescuedOK {
			return rescued, true
		}
		return nil, false
	}
	if len(candidates) > maxIDs {
		if rescued, rescuedOK := vol.rescueWithCompleteGramLane(pq, maxIDs); rescuedOK {
			return rescued, true
		}
		return nil, false
	}
	return candidates, true
}

// rescueWithCompleteGramLane retries a declined selective-trigram query
// through the complete PNGC intersection lane, which answers broad terms
// exactly instead of falling to the bounded scan.
func (vol *serviceVolumeIndex) rescueWithCompleteGramLane(pq parsedQuery, maxIDs int) ([]int, bool) {
	if vol == nil || vol.index == nil || vol.index.Derived.SelfNameTrigrams == nil ||
		vol.index.Derived.SelfNameTrigrams.mappedGrams == nil {
		return nil, false
	}
	out, ok := vol.completeMultiTermNameGramCandidates(pq.Terms, maxIDs, pq)
	return out, ok
}

func (vol *serviceVolumeIndex) componentTrigramCandidates(pq parsedQuery) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	if vol == nil || vol.index == nil {
		pq.Trace.setDecline("component-trigram:no-volume")
		return nil, false
	}
	if trigrams == nil {
		pq.Trace.setDecline("component-trigram:not-ready")
		return nil, false
	}
	if pq.CaseSensitive || !pq.MatchPath || len(pq.Terms) == 0 || pq.Under != "" || pq.Type != "" ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" {
		pq.Trace.setDecline("component-trigram:unsupported-query")
		return nil, false
	}
	bestTerm := ""
	bestCount := serviceComponentTrigramCandidateMaxIDs + 1
	missingTerm := ""
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		if len(term) < 3 {
			continue
		}
		if !asciiOnlyString(term) {
			pq.Trace.setDecline("component-trigram:non-ascii-term")
			return nil, false
		}
		count, ok := trigrams.postingCount(term)
		if !ok {
			pq.Trace.setDecline("component-trigram:no-posting-count")
			continue
		}
		if count == 0 {
			missingTerm = term
			continue
		}
		if count > serviceComponentTrigramCandidateMaxIDs {
			if len(term) < 6 {
				continue
			}
			count = serviceComponentTrigramCandidateMaxIDs
		}
		if count < bestCount {
			bestTerm = term
			bestCount = count
		}
	}
	if bestTerm == "" {
		if missingTerm != "" {
			candidates, ok := vol.nameTrigramPathTermPosting(missingTerm)
			if !ok {
				pq.Trace.setDecline("component-trigram:" + trigrams.lookupState(missingTerm))
				return nil, false
			}
			if len(candidates) > serviceComponentTrigramExpansionMaxIDs {
				pq.Trace.setDecline("component-trigram:missing-term-expanded-too-large")
				return nil, false
			}
			return candidates, true
		}
		for _, term := range pq.Terms {
			if state := trigrams.lookupState(term); state == "omitted-common" || state == "missing-section" {
				pq.Trace.setDecline("component-trigram:" + state)
				break
			}
		}
		pq.Trace.setDecline("component-trigram:no-selective-term")
		return nil, false
	}
	candidates, ok := vol.nameTrigramPathTermPosting(bestTerm)
	if !ok {
		pq.Trace.setDecline("component-trigram:" + trigrams.lookupState(bestTerm))
		return nil, false
	}
	if len(candidates) > serviceComponentTrigramExpansionMaxIDs {
		pq.Trace.setDecline("component-trigram:expanded-too-large")
		return nil, false
	}
	return candidates, true
}

func (vol *serviceVolumeIndex) nameTrigramPathNameTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CaseSensitive ||
		!pq.MatchPath || pq.CountOnly || pq.Limit <= 0 || len(pq.Terms) == 0 ||
		pq.Under != "" || pq.Type != "" || len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.OrGroups) > 0 ||
		len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		pq.CWDBias != "" || pq.RootBias != "" || countNonVolumeTerms(pq.Terms) != 1 || pq.hiddenBlocksTruncation() {
		pq.Trace.replaceDecline("path-name-trigram-top:unsupported-query")
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
	if len(term) < 6 || strings.ContainsAny(term, `\/*?[]:`) {
		pq.Trace.replaceDecline("path-name-trigram-top:bad-term")
		return nil, false
	}
	nameMatches, ok := vol.nameTrigramNameTermTopPosting(term, servicePathNameTrigramCandidateMaxIDs, servicePathNameTrigramCandidateMaxIDs)
	if !ok || len(nameMatches) == 0 {
		pq.Trace.replaceDecline("path-name-trigram-top:" + vol.nameTrigramIndex().lookupState(term))
		return nil, false
	}
	direct := make([]int, 0, len(nameMatches))
	seen := make(map[int]struct{}, len(nameMatches)+pq.Limit)
	sawUnexpandedDir := false
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			direct = append(direct, id)
		}
		if rec.Mode&uint32(os.ModeDir) != 0 && vol.estimatedDescendantOrSelfCount(id) > serviceComponentTrigramExpansionMaxIDs {
			direct = vol.appendTopSubtreeCandidatesByRank(direct, seen, id, pq.Limit*4)
		} else if rec.Mode&uint32(os.ModeDir) != 0 {
			sawUnexpandedDir = true
		}
	}
	if sawUnexpandedDir {
		pq.Trace.replaceDecline("path-name-trigram-top:directory-needs-expansion")
		return nil, false
	}
	if len(direct) < pq.Limit {
		pq.Trace.replaceDecline("path-name-trigram-top:too-few-direct")
		return nil, false
	}
	return topCandidateIDsByRank(direct, pq.Limit, vol.index, vol.rankForQuery(pq)), true
}

func (vol *serviceVolumeIndex) nameTrigramNameTermTopPosting(term string, maxIDs, limit int) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	if vol == nil || trigrams == nil || limit <= 0 || !asciiOnlyString(term) {
		return nil, false
	}
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
	out := make([]int, 0, min(limit, len(ids)))
	seen := make(map[int]struct{}, min(limit, len(ids)))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		if vol.nameTrigramCandidateMatches(id, term) {
			seen[id] = struct{}{}
			out = append(out, id)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, true
}

func (vol *serviceVolumeIndex) appendTopSubtreeCandidatesByRank(out []int, seen map[int]struct{}, rootID int, limit int) []int {
	if vol == nil || vol.index == nil || limit <= 0 || rootID < 0 ||
		rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) ||
		len(vol.subtreeOrder) == 0 {
		return out
	}
	start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
	if start == ^uint32(0) || start >= end {
		return out
	}
	recordCount := vol.index.compactRecordCount()
	orderLen := recordCount
	useResidentOrder := vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0
	if useResidentOrder {
		orderLen = len(vol.queryIndex.nameOrder)
	} else {
		orderLen = compactOrderLen(vol.index.CompactNameOrder, recordCount)
	}
	for pos := 0; pos < orderLen && len(out) < limit; pos++ {
		id := pos
		if useResidentOrder {
			id = int(vol.queryIndex.nameOrder[pos])
		} else {
			id = compactOrderAt(vol.index.CompactNameOrder, pos)
		}
		if id < 0 || id >= recordCount || id >= len(vol.subtreeStart) {
			continue
		}
		treePos := vol.subtreeStart[id]
		if treePos < start || treePos >= end {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (vol *serviceVolumeIndex) nameTrigramNameTermPosting(term string) ([]int, bool) {
	return vol.nameTrigramNameTermPostingLimited(term, serviceNameTrigramCandidateMaxIDs)
}

func (vol *serviceVolumeIndex) nameTrigramNameTermPostingLimited(term string, maxIDs int) ([]int, bool) {
	trigrams := vol.nameTrigramIndex()
	return vol.nameNgramNameTermPosting(term, trigrams, maxIDs)
}

func (vol *serviceVolumeIndex) nameNgramNameTermPosting(term string, trigrams *compressedTrigramIndex, maxIDs int) ([]int, bool) {
	if vol == nil || !asciiOnlyString(term) {
		return nil, false
	}
	if trigrams == nil {
		trigrams = vol.nameTrigramIndex()
		if trigrams == nil && vol.index != nil {
			trigrams = vol.index.Derived.SelfNameTrigrams
		}
	}
	if trigrams == nil {
		return nil, false
	}
	extra := vol.index != nil && vol.index.Derived.SelfNameTrigrams != nil && vol.index.Derived.SelfNameTrigrams.mappedGrams != nil
	if extra {
		its, counts, exactZero, complete := completeSelfNameGramIterators(vol.index, term)
		if !complete {
			return nil, false
		}
		if exactZero {
			return vol.nameTrigramRecentMatches(term), true
		}
		if len(its) == 0 || (maxIDs > 0 && counts[0] > maxIDs) {
			return nil, false
		}
		ids := materializePostingBlockIterator(its[0], counts[0])
		for i := 1; i < len(its) && len(ids) > 0; i++ {
			ids = intersectSortedUint32sWithPostingIterator(ids, its[i])
		}
		if maxIDs > 0 && len(ids) > maxIDs {
			return nil, false
		}
		out := uniqueSortedInts(vol.verifyNameTrigramCandidateIDs(uint32sToInts(ids), term))
		return vol.withNameTrigramRecentCandidates(out, term), true
	}
	cacheKey := fmt.Sprintf("\x00ngram%dname:%s", trigrams.gramSize, term)
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
	out := vol.verifyNameTrigramCandidateIDs(ids, term)
	out = uniqueSortedInts(out)
	vol.cacheNamePosting(cacheKey, out)
	return vol.withNameTrigramRecentCandidates(out, term), true
}

func (vol *serviceVolumeIndex) nameTrigramRecentMatches(term string) []int {
	if vol == nil || len(vol.nameTrigramRecent) == 0 {
		return nil
	}
	out := make([]int, 0, min(len(vol.nameTrigramRecent), 64))
	for id := range vol.nameTrigramRecent {
		if vol.nameTrigramCandidateMatches(id, term) {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func (vol *serviceVolumeIndex) withNameTrigramRecentCandidates(base []int, term string) []int {
	if vol == nil || len(vol.nameTrigramRecent) == 0 {
		return base
	}
	out := append([]int(nil), base...)
	seen := make(map[int]struct{}, len(out))
	for _, id := range out {
		seen[id] = struct{}{}
	}
	for id := range vol.nameTrigramRecent {
		if _, ok := seen[id]; ok {
			continue
		}
		if vol.nameTrigramCandidateMatches(id, term) {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func (vol *serviceVolumeIndex) verifyNameTrigramCandidateIDs(ids []int, term string) []int {
	if len(ids) == 0 || vol == nil || vol.index == nil {
		return nil
	}
	if len(ids) < serviceTrigramParallelVerifyMinIDs {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if vol.nameTrigramCandidateMatches(id, term) {
				out = append(out, id)
			}
		}
		return out
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, len(ids)/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if vol.nameTrigramCandidateMatches(id, term) {
				out = append(out, id)
			}
		}
		return out
	}
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * len(ids) / workers
		end := (worker + 1) * len(ids) / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := make([]int, 0, end-start)
			for _, id := range ids[start:end] {
				if vol.nameTrigramCandidateMatches(id, term) {
					local = append(local, id)
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

// verifyMultiTermCandidateIDs runs the multi-term substring check over a
// candidate set in parallel, mirroring verifyNameTrigramCandidateIDs.
func (vol *serviceVolumeIndex) verifyMultiTermCandidateIDs(ids []int, matches func(int) bool) []int {
	if len(ids) == 0 || vol == nil || vol.index == nil {
		return nil
	}
	if len(ids) < serviceTrigramParallelVerifyMinIDs {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if matches(id) {
				out = append(out, id)
			}
		}
		return uniqueSortedInts(out)
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, len(ids)/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if matches(id) {
				out = append(out, id)
			}
		}
		return uniqueSortedInts(out)
	}
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * len(ids) / workers
		end := (worker + 1) * len(ids) / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			local := make([]int, 0, end-start)
			for _, id := range ids[start:end] {
				if matches(id) {
					local = append(local, id)
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
	return uniqueSortedInts(out)
}

func (vol *serviceVolumeIndex) nameTrigramCandidateMatches(id int, term string) bool {
	if id < 0 || id >= vol.index.compactRecordCount() {
		return false
	}
	rec := vol.index.compactRecord(id)
	if rec.Deleted {
		return false
	}
	return containsFoldASCII(vol.index.compactNameAt(id), term)
}

func (vol *serviceVolumeIndex) nameTrigramPathTermPosting(term string) ([]int, bool) {
	cacheKey := "\x00trigrampath:" + term
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
	ids, ok := vol.nameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
		return nil, false
	}
	seen := make(map[int]struct{}, len(ids))
	out := make([]int, 0, len(ids))
	estimated := 0
	for _, id := range ids {
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
	for _, id := range ids {
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
	vol.cachePathPosting(cacheKey, out)
	return out, true
}
