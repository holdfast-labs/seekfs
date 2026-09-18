package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func compactOrderLen(order []int, recordCount int) int {
	if order == nil {
		return recordCount
	}
	return len(order)
}

func compactOrderAt(order []int, pos int) int {
	if order == nil {
		return pos
	}
	return order[pos]
}

func uint32OrderToInts(order []uint32) []int {
	if len(order) == 0 {
		return nil
	}
	out := make([]int, len(order))
	for i, id := range order {
		out[i] = int(id)
	}
	return out
}

func (idx *Index) compactRecordCount() int {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.Len()
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.Len()
	}
	return len(idx.Records)
}

// compactHasSize reports whether the index carries per-record file sizes.
// Older USN-built indexes may not capture sizes; size: filters must error
// against those indexes rather than silently match nothing.
func (idx *Index) compactHasSize() bool {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.hasSize
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.Size32 != nil
	}
	for i := range idx.Records {
		if idx.Records[i].Size != 0 {
			return true
		}
	}
	return false
}

// compactHasModTime reports whether the index carries per-record modification
// times. Used to gate dm:, --recent, and --modified-after.
func (idx *Index) compactHasModTime() bool {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.hasModUnix
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.ModUnix != nil
	}
	for i := range idx.Records {
		if idx.Records[i].ModUnix != 0 {
			return true
		}
	}
	return false
}

func (idx *Index) compactHasAttrs() bool {
	return idx != nil && idx.CompactAttrs
}

func (idx *Index) compactRecord(i int) CompactRecord {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.At(i)
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.At(i)
	}
	if i < 0 || i >= len(idx.Records) {
		return CompactRecord{}
	}
	return idx.Records[i]
}

func (idx *Index) compactNameAt(i int) string {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.nameAtRecord(i)
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.nameAt(i)
	}
	if i < 0 || i >= len(idx.Records) {
		return ""
	}
	return idx.Records[i].Name
}

func (idx *Index) compactLowerNameAt(i int) string {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.lowerNameAt(i)
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.lowerNameAt(i)
	}
	return compactLowerName(idx.compactRecord(i))
}

// compactLowerNameOf lowercases a record's name from a record the caller already
// parsed, so rank builders avoid a second mmap recordRefs parse per record.
func (idx *Index) compactLowerNameOf(i int, rec CompactRecord) string {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.lowerNameByID(rec.NameOff, rec.Name)
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.lowerNameAt(i)
	}
	return compactLowerName(rec)
}

// compactDeletedAt reports a record's deleted flag without materializing the
// record or its name; the rank builders only need liveness in their first pass.
func (idx *Index) compactDeletedAt(i int) bool {
	if idx.MMapRecords != nil {
		return idx.MMapRecords.deletedAt(i)
	}
	if idx.PackedRecords != nil {
		return idx.PackedRecords.deletedAt(i)
	}
	if i < 0 || i >= len(idx.Records) {
		return false
	}
	return idx.Records[i].Deleted
}

func (idx *Index) setCompactRecord(i int, rec CompactRecord) {
	if idx.MMapRecords != nil {
		return
	}
	if idx.PackedRecords != nil {
		idx.PackedRecords.Set(i, rec)
	}
	if i >= 0 && i < len(idx.Records) {
		idx.Records[i] = rec
	}
}

func (idx *Index) appendCompactRecord(rec CompactRecord) int {
	id := idx.compactRecordCount()
	if idx.MMapRecords != nil {
		return -1
	}
	if idx.PackedRecords != nil {
		idx.PackedRecords.Append(rec)
	}
	if idx.PackedRecords == nil || idx.Records != nil {
		idx.Records = append(idx.Records, rec)
	}
	return id
}

func searchCompactWithCache(idx *Index, opts queryOptions, countOnly bool, pathCache map[int]string, candidateFn func(parsedQuery) ([]int, bool)) ([]Entry, error) {
	return searchCompactWithCacheHidden(idx, opts, countOnly, pathCache, candidateFn, hiddenBaseIDs{}, nil)
}

type hiddenBaseIDs struct {
	tombstone []int32
	shadowed  []int32
}

func (h hiddenBaseIDs) empty() bool {
	return len(h.tombstone) == 0 && len(h.shadowed) == 0
}

func (h hiddenBaseIDs) contains(id int) bool {
	if id < 0 {
		return false
	}
	id32 := int32(id)
	if pos := sort.Search(len(h.tombstone), func(i int) bool { return h.tombstone[i] >= id32 }); pos < len(h.tombstone) && h.tombstone[pos] == id32 {
		return true
	}
	if pos := sort.Search(len(h.shadowed), func(i int) bool { return h.shadowed[i] >= id32 }); pos < len(h.shadowed) && h.shadowed[pos] == id32 {
		return true
	}
	return false
}

// searchCompactWithCacheHidden verifies candidates from idx. contentVol is
// non-nil only on the content query path: content semantics are then enforced
// inline (before a match is appended or counted toward the limit), and the
// ordinary limit-based collection is restored. A nil contentVol is the
// unchanged name/scalar path.
func searchCompactWithCacheHidden(idx *Index, opts queryOptions, countOnly bool, pathCache map[int]string, candidateFn func(parsedQuery) ([]int, bool), hidden hiddenBaseIDs, contentVol *serviceVolumeIndex) ([]Entry, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, err
	}
	if pq.Impossible {
		pq.Trace.setPlannerMode("impossible-query")
		pq.Trace.setSource("impossible-query", 0)
		return []Entry{}, nil
	}
	if err := checkQueryCapabilities(pq, idx); err != nil {
		return nil, err
	}
	dropSatisfiedVolumeTerms(&pq, idx.Volume)
	limit := normalizedLimit(opts.Limit, countOnly)
	pq.Limit = limit
	pq.CountOnly = countOnly
	var contentMatcher *contentLeafMatcher
	if contentVol != nil && queryHasAnyContentLeaf(pq) {
		contentMatcher = newContentLeafMatcher(pq)
	}
	order := idx.CompactNameOrder
	if pq.SortColumn == "size" {
		order = uint32OrderToInts((&serviceVolumeIndex{index: idx}).sizeOrderForRank())
	} else if pq.SortColumn == "modified" {
		order = uint32OrderToInts((&serviceVolumeIndex{index: idx}).modifiedOrderForRank())
	} else if pq.SortColumn == "extension" {
		order = uint32OrderToInts((&serviceVolumeIndex{index: idx}).extensionOrderForRank())
	} else if pq.SortColumn == "type" {
		order = uint32OrderToInts((&serviceVolumeIndex{index: idx}).typeOrderForRank())
	} else if pq.SortColumn == "path" {
		order = uint32OrderToInts((&serviceVolumeIndex{index: idx}).pathOrderForRank())
	}
	usedCandidates := false
	if candidateFn != nil {
		if candidates, ok := candidateFn(pq); ok {
			if candidates == nil {
				candidates = []int{}
			}
			order = candidates
			usedCandidates = true
		}
	}
	if !usedCandidates && queryCanceled(pq) {
		return nil, errQueryCanceled
	}
	if !usedCandidates {
		pq.Trace.setSource("compact-name-order-scan", compactOrderLen(order, idx.compactRecordCount()))
	}
	if pq.RootBias != "" || pq.CWDBias != "" {
		order = idx.biasOrderCompact(order, firstNonEmpty(pq.CWDBias, pq.RootBias))
	}
	results := make([]Entry, 0, min(limit, 1024))
	if pathCache == nil {
		pathCache = make(map[int]string)
	}
	skipEntryMatches := compactCandidateCanSkipEntryMatches(pq, usedCandidates)
	if usedCandidates && !countOnly && len(order) >= serviceTrigramParallelVerifyMinIDs {
		return verifyCompactCandidateOrderParallel(idx, pq, order, pathCache, limit, skipEntryMatches, hidden, contentVol, contentMatcher)
	}
	for pos := 0; pos < compactOrderLen(order, idx.compactRecordCount()); pos++ {
		if pos&1023 == 0 && queryCanceled(pq) {
			return nil, errQueryCanceled
		}
		recIndex := compactOrderAt(order, pos)
		if hidden.contains(recIndex) {
			continue
		}
		rec := idx.compactRecord(recIndex)
		if rec.Deleted {
			continue
		}
		if !compactRecordPrecheck(rec, pq, pq.MatchPath) {
			continue
		}
		if queryPathTermPrecheckSafe(pq) && !idx.compactPathContainsAll(recIndex, pq.Terms) {
			continue
		}
		if skipEntryMatches {
			results = append(results, compactEntryFromRecord(idx, recIndex, rec, pathCache, false))
			if !countOnly && len(results) >= limit {
				break
			}
			continue
		}
		// Counting mode skips path reconstruction when the query has no path
		// constraint, and tallies the match instead of retaining an Entry.
		withPath := true
		if opts.contentCount != nil {
			withPath = queryNeedsEntryPath(pq)
		}
		entry := compactEntryFromRecordPath(idx, recIndex, rec, pathCache, true, withPath)
		if entryMatchesWithContentMatcher(contentVol, entry, pq, pq.MatchPath, contentMatcher) {
			if opts.contentCount != nil {
				*opts.contentCount++
				continue
			}
			results = append(results, entry)
			if !countOnly && len(results) >= limit {
				break
			}
		}
	}
	return results, nil
}

func compactCandidateCanSkipEntryMatches(pq parsedQuery, usedCandidates bool) bool {
	if !usedCandidates {
		return false
	}
	if queryHasAnyContentLeaf(pq) {
		// Content verification is inline and needs the materialized entry, so
		// the name/scalar precheck cannot be skipped.
		return false
	}
	if pq.Under != "" ||
		pq.Exists ||
		len(pq.Dirs) > 0 ||
		len(pq.Regexps) > 0 ||
		len(pq.Parents) > 0 ||
		len(pq.SizeFilters) > 0 ||
		len(pq.DateFilters) > 0 ||
		len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 ||
		len(pq.NotGroups) > 0 {
		return false
	}
	if len(pq.Terms) == 0 {
		return len(pq.Globs) == 0 &&
			pq.Type == "" &&
			!pq.HasModAfter &&
			pq.CWDBias == "" &&
			pq.RootBias == ""
	}
	return pq.MatchPath &&
		countNonVolumeTerms(pq.Terms) == 1 &&
		len(pq.Exts) == 0 &&
		len(pq.Globs) == 0 &&
		pq.Type == "" &&
		!pq.HasModAfter &&
		pq.CWDBias == "" &&
		pq.RootBias == ""
}

func compactEntryFromRecord(idx *Index, recIndex int, rec CompactRecord, pathCache map[int]string, withLower bool) Entry {
	return compactEntryFromRecordPath(idx, recIndex, rec, pathCache, withLower, true)
}

// compactEntryFromRecordPath is compactEntryFromRecord with an explicit path
// switch. withPath=false skips reconstructing Entry.Path (used by the content
// count path when the query reads only the record's name/metadata).
func compactEntryFromRecordPath(idx *Index, recIndex int, rec CompactRecord, pathCache map[int]string, withLower, withPath bool) Entry {
	path := ""
	if withPath {
		path = idx.reconstructCompactPathCached(recIndex, pathCache)
	}
	entry := Entry{
		Path:        path,
		Name:        rec.Name,
		Mode:        rec.Mode,
		Size:        rec.Size,
		ModUnix:     rec.ModUnix,
		IndexSource: idx.Source,
		FRN:         rec.FRN,
	}
	if rec.Mode&uint32(os.ModeDir) != 0 && recIndex >= 0 && recIndex < len(idx.Derived.SubtreeBytes) {
		size := int64(idx.Derived.SubtreeBytes[recIndex])
		if delta := idx.dirSizeDelta.Load(); delta != nil {
			size += (*delta)[recIndex]
		}
		if size < 0 {
			size = 0
		}
		entry.Size = size
	}
	if withLower {
		entry.LowerPath = strings.ToLower(path)
		entry.LowerName = idx.compactLowerNameAt(recIndex)
	}
	return entry
}

func verifyCompactCandidateOrderParallel(idx *Index, pq parsedQuery, order []int, pathCache map[int]string, limit int, skipEntryMatches bool, hidden hiddenBaseIDs, contentVol *serviceVolumeIndex, matcher *contentLeafMatcher) ([]Entry, error) {
	if len(order) == 0 || limit <= 0 {
		return nil, nil
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, len(order)/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		return verifyCompactCandidateOrderRange(idx, pq, order, pathCache, limit, skipEntryMatches, hidden, contentVol, matcher)
	}
	parts := make([][]Entry, workers)
	var canceled atomic.Bool
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * len(order) / workers
		end := (worker + 1) * len(order) / workers
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			localCache := make(map[int]string)
			local := make([]Entry, 0, min(limit, end-start))
			for pos := start; pos < end; pos++ {
				if pos&1023 == 0 && queryCanceled(pq) {
					canceled.Store(true)
					return
				}
				recIndex := order[pos]
				if hidden.contains(recIndex) {
					continue
				}
				if entry, ok := compactCandidateEntryIfMatchIn(contentVol, idx, pq, recIndex, localCache, true, skipEntryMatches, matcher); ok {
					local = append(local, entry)
					if len(local) >= limit {
						break
					}
				}
			}
			parts[worker] = local
		}(worker, start, end)
	}
	wg.Wait()
	if canceled.Load() {
		return nil, errQueryCanceled
	}
	out := make([]Entry, 0, min(limit, 1024))
	for _, part := range parts {
		for _, entry := range part {
			out = append(out, entry)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func verifyCompactCandidateOrderRange(idx *Index, pq parsedQuery, order []int, pathCache map[int]string, limit int, skipEntryMatches bool, hidden hiddenBaseIDs, contentVol *serviceVolumeIndex, matcher *contentLeafMatcher) ([]Entry, error) {
	out := make([]Entry, 0, min(limit, 1024))
	for pos, recIndex := range order {
		if pos&1023 == 0 && queryCanceled(pq) {
			return nil, errQueryCanceled
		}
		if hidden.contains(recIndex) {
			continue
		}
		if entry, ok := compactCandidateEntryIfMatchIn(contentVol, idx, pq, recIndex, pathCache, true, skipEntryMatches, matcher); ok {
			out = append(out, entry)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func compactCandidateEntryIfMatch(idx *Index, pq parsedQuery, recIndex int, pathCache map[int]string, usedCandidates bool, skipEntryMatches bool) (Entry, bool) {
	return compactCandidateEntryIfMatchIn(nil, idx, pq, recIndex, pathCache, usedCandidates, skipEntryMatches, nil)
}

// compactCandidateEntryIfMatchIn is compactCandidateEntryIfMatch with inline
// content verification: when vol has content and pq carries a content leaf, the
// entry must satisfy the joint name+content predicate. Other callers pass nil
// vol and keep the pure name/scalar behavior.
func compactCandidateEntryIfMatchIn(vol *serviceVolumeIndex, idx *Index, pq parsedQuery, recIndex int, pathCache map[int]string, usedCandidates bool, skipEntryMatches bool, matcher *contentLeafMatcher) (Entry, bool) {
	rec := idx.compactRecord(recIndex)
	if rec.Deleted {
		return Entry{}, false
	}
	if !compactRecordPrecheck(rec, pq, pq.MatchPath) {
		return Entry{}, false
	}
	if queryPathTermPrecheckSafe(pq) && !idx.compactPathContainsAll(recIndex, pq.Terms) {
		return Entry{}, false
	}
	if skipEntryMatches {
		return compactEntryFromRecord(idx, recIndex, rec, pathCache, false), true
	}
	entry := compactEntryFromRecord(idx, recIndex, rec, pathCache, true)
	if !entryMatchesWithContentMatcher(vol, entry, pq, pq.MatchPath, matcher) {
		return Entry{}, false
	}
	return entry, true
}

func dropSatisfiedVolumeTerms(pq *parsedQuery, volume string) {
	if pq == nil || volume == "" || len(pq.Terms) == 0 {
		return
	}
	out := pq.Terms[:0]
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) && strings.EqualFold(term, volume) {
			continue
		}
		out = append(out, term)
	}
	pq.Terms = out
}

var errQueryCanceled = errors.New("query superseded")

var errGlobalMultiVolumePlannerDeclined = errors.New("global planner declined multi-volume query")

func globalMultiVolumePlannerDeclineError(opts queryOptions) error {
	if queryCanceled(parsedQuery{DeadlineUnix: opts.DeadlineUnix, Cancel: opts.Cancel}) {
		return errQueryCanceled
	}
	if opts.Trace != nil && opts.Trace.Decline != "" {
		return fmt.Errorf("%w: %s", errGlobalMultiVolumePlannerDeclined, opts.Trace.Decline)
	}
	return errGlobalMultiVolumePlannerDeclined
}

func queryCanceled(pq parsedQuery) bool {
	if pq.Cancel != nil && pq.Cancel() {
		return true
	}
	return pq.DeadlineUnix > 0 && time.Now().UnixNano() > pq.DeadlineUnix
}

// checkQueryCapabilities rejects queries whose filters need data the index does
// not carry. Older indexes can lack file sizes or modification times, so size:,
// dm:, --recent, and --modified-after would otherwise silently match nothing.
// Failing loudly is consistent with rejecting unknown filters at parse time.
func checkQueryCapabilities(pq parsedQuery, idx *Index) error {
	needsSize, needsMod := queryNeedsSizeOrMod(pq)
	if needsSize && !idx.compactHasSize() {
		return errors.New("size: filters require an index with file sizes; the current index has none (rebuild with a size-capable indexer)")
	}
	if needsMod && !idx.compactHasModTime() {
		return errors.New("dm:/--recent/--modified-after require an index with modification times; the current index has none")
	}
	if queryNeedsAttrs(pq) && !idx.compactHasAttrs() {
		return errors.New("attrib: filters require an index with file attributes; rebuild the index with a current seekfs version")
	}
	return nil
}

func queryNeedsSizeOrMod(pq parsedQuery) (size bool, mod bool) {
	if len(pq.SizeFilters) > 0 {
		size = true
	}
	if pq.SortColumn == "size" {
		size = true
	}
	if pq.SortColumn == "modified" {
		mod = true
	}
	if len(pq.DateFilters) > 0 || pq.HasModAfter {
		mod = true
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			s, m := queryNeedsSizeOrMod(alt)
			size = size || s
			mod = mod || m
		}
	}
	for _, neg := range pq.NotGroups {
		s, m := queryNeedsSizeOrMod(neg)
		size = size || s
		mod = mod || m
	}
	return size, mod
}

func queryNeedsAttrs(pq parsedQuery) bool {
	if len(pq.AttrFilters) > 0 {
		return true
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			if queryNeedsAttrs(alt) {
				return true
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if queryNeedsAttrs(neg) {
			return true
		}
	}
	return false
}

func compactRecordPrecheck(rec CompactRecord, pq parsedQuery, matchPath bool) bool {
	name := rec.Name
	cmpName := normalizeCase(name, pq.CaseSensitive)
	if !matchPath && !containsAll(cmpName, pq.Terms) {
		return false
	}
	if pq.Type == "file" && rec.Mode&uint32(os.ModeDir) != 0 {
		return false
	}
	if pq.Type == "dir" && rec.Mode&uint32(os.ModeDir) == 0 {
		return false
	}
	if !attrFiltersMatch(rec.Mode, pq.AttrFilters) {
		return false
	}
	if pq.HasModAfter {
		if rec.ModUnix == 0 || !time.Unix(0, rec.ModUnix).After(pq.ModifiedAfter) {
			return false
		}
	}
	for _, ext := range pq.Exts {
		actual := strings.TrimPrefix(filepath.Ext(name), ".")
		if normalizeCase(actual, pq.CaseSensitive) != ext {
			return false
		}
	}
	for _, glob := range pq.Globs {
		ok, err := filepath.Match(glob, cmpName)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) nameTermCandidates(pq parsedQuery) ([]int, bool) {
	// Content is a first-class constraint. A content posting superset is
	// preferred; when none exists (mixed OR, negative-only, an unindexable
	// gram, or any other decline) the bounded ordered scan is the correct
	// superset and inline verification narrows it. Returning ok=true even for
	// an empty set is a real answer, not a fallback.
	if queryHasAnyContentLeaf(pq) {
		if queryHasPositiveContentLeaf(pq) {
			if candidates, ok := vol.contentCandidates(pq); ok {
				// Single-volume filename parity: the filename path orders its
				// candidates by rankForQuery(pq) (name rank for the default
				// order; size/modified/extension/type/path for explicit sorts),
				// so order the content posting set the same way. sort:relevance
				// keeps its own post-verify ranking (rankForQuery returns nil
				// for it), so it is left in candidate order here.
				if pq.SortColumn != "relevance" {
					sortCandidateIDs(candidates, pq, vol.index, vol.rankForQuery(pq))
				}
				pq.Trace.setSource("content-candidates", len(candidates))
				return candidates, true
			}
		}
		// The bounded scan is already produced in orderForQuery(pq) order, which
		// is the same rank sequence for every sort column.
		if candidates, ok := vol.boundedScanCandidates(pq); ok {
			pq.Trace.setSource("content-scan", len(candidates))
			return candidates, true
		}
		return nil, false
	}
	if !pq.CountOnly && !pq.MatchPath && (len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0) {
		if candidates, ok := vol.plannedCandidates(pq); ok {
			pq.Trace.setSource("planned:boolean", len(candidates))
			return candidates, true
		}
	}
	if pq.CountOnly {
		if candidates, ok := vol.boundedScanCandidates(pq); ok {
			pq.Trace.setSource("bounded-scan", len(candidates))
			return candidates, true
		}
	}
	if len(pq.Terms) == 0 && len(pq.Exts) == 0 && len(pq.Globs) == 0 && len(pq.OrGroups) == 0 {
		if candidates, ok := vol.boundedScanCandidates(pq); ok {
			pq.Trace.setSource("bounded-scan", len(candidates))
			return candidates, true
		}
	}
	if queryHasNonASCIIPlainTerm(pq) {
		if candidates, ok := vol.boundedScanCandidates(pq); ok {
			pq.Trace.setSource("bounded-scan", len(candidates))
			return candidates, true
		}
	}
	if pq.MatchPath {
		if candidates, ok := vol.extensionShapedPathTopCandidates(pq); ok {
			pq.Trace.setSource("path-extension-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.bareExtensionMultiPathTopCandidates(pq); ok {
			pq.Trace.setSource("path-bare-extension-multi-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.selectiveNamePathTermCandidates(pq); ok {
			pq.Trace.setSource("path-selective-name-term", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.pathDirectoryTermTopCandidates(pq); ok {
			pq.Trace.setSource("path-directory-term-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.componentDirectTopCandidates(pq); ok {
			pq.Trace.setSource("path-component-direct-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.componentRootTopCandidates(pq); ok {
			pq.Trace.setSource("path-component-root-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.componentMultiTermTopCandidates(pq); ok {
			pq.Trace.setSource("path-component-multi-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.multiTermEmptyPathCandidates(pq); ok {
			pq.Trace.setSource("path-multi-empty", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.nameTrigramCandidates(pq); ok {
			if compactCandidateCanSkipEntryMatches(pq, true) && pq.Limit > 0 {
				candidates = topCandidateIDsByRank(candidates, pq.Limit, vol.index, vol.rankForQuery(pq))
			} else {
				sortCandidateIDs(candidates, pq, vol.index, vol.rankForQuery(pq))
			}
			pq.Trace.setSource("path-component-trigram", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.nameTrigramPathNameTopCandidates(pq); ok {
			pq.Trace.setSource("path-name-trigram-top", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.limitedPathTermCandidates(pq); ok {
			pq.Trace.setSource("path-term-limited", len(candidates))
			return candidates, true
		}
		if candidates, ok := vol.limitedDottedPathScanCandidates(pq); ok {
			pq.Trace.setSource("path-dotted-limited-scan", len(candidates))
			return candidates, true
		}
	} else {
		if candidates, ok := vol.nameTrigramCandidates(pq); ok {
			sortCandidateIDs(candidates, pq, vol.index, vol.rankForQuery(pq))
			pq.Trace.setSource("name-trigram", len(candidates))
			return candidates, true
		}
	}
	if candidates, ok := vol.limitedSingleTermCandidates(pq); ok {
		pq.Trace.setSource("limited-single-term", len(candidates))
		return candidates, true
	}
	if !pq.MatchPath {
		if candidates, ok := vol.plannedCandidates(pq); ok {
			pq.Trace.setSource("planned", len(candidates))
			return candidates, true
		}
	}
	if pq.MatchPath {
		if candidates, ok := vol.nameTrigramCandidates(pq); ok {
			if compactCandidateCanSkipEntryMatches(pq, true) && pq.Limit > 0 {
				candidates = topCandidateIDsByRank(candidates, pq.Limit, vol.index, vol.rankForQuery(pq))
			} else {
				sortCandidateIDs(candidates, pq, vol.index, vol.rankForQuery(pq))
			}
			pq.Trace.setSource("path-component-trigram", len(candidates))
			return candidates, true
		}
	}
	if candidates, ok := vol.plannedCandidates(pq); ok {
		pq.Trace.setSource("planned", len(candidates))
		return candidates, true
	}
	if pq.MatchPath {
		if candidates, ok := vol.componentTrigramCandidates(pq); ok {
			pq.Trace.setSource("component-trigram", len(candidates))
			return candidates, true
		}
	}
	if candidates, ok := vol.underCandidates(pq); ok {
		pq.Trace.setSource("under", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.boundedScanCandidates(pq); ok {
		pq.Trace.setSource("bounded-scan", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.plannerCandidates(pq); ok {
		pq.Trace.setSource("legacy-planner", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.pathDirFilterCandidates(pq); ok {
		pq.Trace.setSource("path-dir-filter", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.filterCandidates(pq); ok {
		pq.Trace.setSource("filter", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.pathRootLimitedCandidates(pq); ok {
		pq.Trace.setSource("path-root-limited", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.pathTermSubtreeCandidates(pq); ok {
		pq.Trace.setSource("path-term-subtree", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.regexLiteralCandidates(pq); ok {
		pq.Trace.setSource("regex-literal", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.exactDirCandidates(pq); ok {
		pq.Trace.setSource("exact-dir", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.exactNameCandidates(pq); ok {
		pq.Trace.setSource("exact-name", len(candidates))
		return candidates, true
	}
	if candidates, ok := vol.namePrefixCandidates(pq); ok {
		pq.Trace.setSource("name-prefix", len(candidates))
		return candidates, true
	}
	if vol == nil || vol.index == nil || len(pq.Terms) == 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || pq.Under != "" {
		return nil, false
	}
	if len(pq.Terms) > 1 && !pq.CaseSensitive && !pq.MatchPath {
		if candidates, ok := vol.cachedMultiNameTermCandidates(pq.Terms); ok {
			pq.Trace.setSource("cached-multi-name-term", len(candidates))
			return candidates, true
		}
		candidates := vol.multiNameTermCandidates(pq.Terms)
		pq.Trace.setSource("multi-name-term", len(candidates))
		return candidates, true
	}
	lists := make([][]int, 0, len(pq.Terms))
	for _, term := range pq.Terms {
		list := vol.nameTermPosting(term)
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
	pq.Trace.setSource("name-term-posting", len(candidates))
	return candidates, true
}

func (vol *serviceVolumeIndex) overlayAwareNameTermCandidates(pq parsedQuery) ([]int, bool) {
	candidates, ok := vol.nameTermCandidates(pq)
	if !ok {
		return nil, false
	}
	if len(candidates) == 0 {
		// A content query with no candidate postings is a real empty answer;
		// only fall back for the name lanes.
		if queryHasPositiveContentLeaf(pq) {
			return []int{}, true
		}
		return nil, false
	}
	return candidates, true
}
