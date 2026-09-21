package main

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// countPathTermPostingLive is the count-only analogue of pathTermPosting.  It
// counts live records whose name contains the term plus live descendants of
// directory roots whose name contains the term, without materializing the ID
// slice.  It is exact for the same input as pathTermPosting (bare term, no
// path separators) and is intended for the count terminal where only the total
// is needed.  `hidden` applies overlay tombstone/shadow filtering to match the
// verified iterator path.  Every counted record is verified with
// compactPathContainsTerm so a malformed/legacy component posting that is a
// superset (rather than an exact path predicate) cannot over-count.
func (vol *serviceVolumeIndex) countPathTermPostingLive(term string, hidden func(int) bool) int {
	if vol == nil || vol.index == nil || term == "" {
		return 0
	}
	recordCount := vol.index.compactRecordCount()
	count := 0
	seen := make(map[int]struct{}, 64)
	// Name self-hits: records whose name contains the term.
	for _, id := range vol.nameTermPosting(term) {
		if id < 0 || id >= recordCount {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if hidden != nil && hidden(id) {
			continue
		}
		if !vol.index.compactPathContainsTerm(id, term) {
			continue
		}
		count++
	}
	if strings.ContainsAny(term, `\/*?[]:`) {
		return count
	}
	if len(vol.childOffsets) > 0 || vol.children != nil {
		traversed := make(map[int]struct{}, 64)
		for _, rootID := range vol.pathTermRootIDs(term) {
			if rootID < 0 || rootID >= recordCount {
				continue
			}
			root := vol.index.compactRecord(rootID)
			if root.Deleted || root.Mode&uint32(os.ModeDir) == 0 {
				continue
			}
			// A legitimate directory root whose own name contains the term
			// guarantees every descendant's path contains the term, so the
			// descendant walk needs no per-record path check.  A malformed or
			// legacy component posting can supply a superset root whose name
			// does NOT contain the term; those descendants must be verified
			// individually so the count stays exact.
			rootExact := strings.Contains(vol.index.compactLowerNameAt(rootID), term) ||
				(vol.index.Volume != "" && containsFoldASCII(vol.index.Volume, term))
			stack := []int{rootID}
			for len(stack) > 0 {
				last := len(stack) - 1
				id := stack[last]
				stack = stack[:last]
				if _, ok := traversed[id]; ok || id < 0 || id >= recordCount {
					continue
				}
				traversed[id] = struct{}{}
				rec := vol.index.compactRecord(id)
				if !rec.Deleted {
					if _, ok := seen[id]; !ok {
						seen[id] = struct{}{}
						if hidden == nil || !hidden(id) {
							if rootExact || vol.index.compactPathContainsTerm(id, term) {
								count++
							}
						}
					}
				}
				for _, childID := range vol.childIDsForRecord(id) {
					stack = append(stack, int(childID))
				}
			}
		}
		return count
	}
	// No child graph: scan paths directly.
	for _, id := range vol.scanPathTermPosting(term) {
		if id < 0 || id >= recordCount {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if hidden != nil && hidden(id) {
			continue
		}
		if !vol.index.compactPathContainsTerm(id, term) {
			continue
		}
		count++
	}
	return count
}

func (vol *serviceVolumeIndex) pathComponentPosting(term string) []int {
	if vol == nil || vol.index == nil || term == "" || strings.ContainsAny(term, `\/*?[]:`) {
		return vol.pathTermPosting(term)
	}
	cacheKey := "\x00pathcomponent:" + term
	vol.termMu.Lock()
	if vol.pathTermCache != nil {
		if entry, ok := vol.pathTermCache[cacheKey]; ok {
			if vol.cacheStampValid(entry.gen) {
				vol.termMu.Unlock()
				return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
					id, ok := vol.idForFRN(rec.FRN)
					return ok && vol.index.compactPathContainsTerm(id, term)
				})
			}
		}
	}
	vol.termMu.Unlock()

	roots := vol.pathComponentRootIDs(term)
	if len(roots) == 0 {
		vol.cachePathPosting(cacheKey, nil)
		return nil
	}
	seen := make(map[int]struct{}, 256)
	out := make([]int, 0, 256)
	for _, rootID := range roots {
		for _, id := range vol.underDescendants(rootID) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Ints(out)
	vol.cachePathPosting(cacheKey, out)
	return out
}

func (vol *serviceVolumeIndex) pathComponentPostingAvailable(term string) bool {
	if vol == nil || vol.index == nil || term == "" {
		return false
	}
	if strings.ContainsAny(term, `\/*?[]:`) {
		return true
	}
	if len(vol.componentPosting32(term)) > 0 {
		return true
	}
	// exactNameIDs is only a cheap membership probe when the resident name
	// order (or exact-names map) is available.  In lowmem/mapped mode that
	// would fall back to a full-record scan just to answer "is this a
	// component?", which is far more expensive than any posting this function
	// gates.  The pathGrams check below is the actual lowmem posting source.
	if vol.exactNames != nil || (vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0) {
		if len(vol.exactNameIDs(strings.ToLower(term))) > 0 {
			return true
		}
	}
	return vol.queryIndex != nil && vol.queryIndex.pathGrams != nil
}

type componentCoverageInterval struct {
	start uint32
	end   uint32
}

// mappedComponentCoverage is the exact base-record union for one component:
// every PCMP directory root contributes its SUBT interval, and filename
// self-hits contribute records outside those intervals.  The intervals are
// merged before either counting or walking so nested roots cannot multiply
// work or cardinality.
type mappedComponentCoverage struct {
	rootCount       int
	intervals       []componentCoverageInterval
	selfIDs         []int
	selfIDsComplete bool
	cardinality     int
	membership      []uint64
}

func (vol *serviceVolumeIndex) baseHasDeletedRecords() bool {
	if vol == nil || vol.index == nil {
		return false
	}
	if state := vol.index.baseDeletedState.Load(); state != 0 {
		return state == 2
	}
	hasDeleted := false
	idx := vol.index
	if idx.MMapRecords != nil {
		m := idx.MMapRecords
		refBytes := 6
		stride := compactDiskRecordBytes
		if m.wideRefs {
			refBytes = 8
			stride = compactWideDiskRecordBytes
		}
		deletedOffset := 16 + refBytes + 4 + 8 + 8
		for base := deletedOffset; base < len(m.recordData); base += stride {
			if m.recordData[base] != 0 {
				hasDeleted = true
				break
			}
		}
	} else if idx.PackedRecords != nil {
		for _, word := range idx.PackedRecords.DeletedBits {
			if word != 0 {
				hasDeleted = true
				break
			}
		}
	} else {
		for _, rec := range idx.Records {
			if rec.Deleted {
				hasDeleted = true
				break
			}
		}
	}
	if hasDeleted {
		vol.index.baseDeletedState.Store(2)
	} else {
		vol.index.baseDeletedState.Store(1)
	}
	return hasDeleted
}

func mergeComponentCoverageIntervals(intervals []componentCoverageInterval) []componentCoverageInterval {
	if len(intervals) < 2 {
		return intervals
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start == intervals[j].start {
			return intervals[i].end > intervals[j].end
		}
		return intervals[i].start < intervals[j].start
	})
	merged := intervals[:0]
	for _, current := range intervals {
		if len(merged) == 0 || current.start > merged[len(merged)-1].end {
			merged = append(merged, current)
			continue
		}
		if current.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = current.end
		}
	}
	return merged
}

func subtractComponentCoverageIntervals(incoming, covered []componentCoverageInterval) []componentCoverageInterval {
	if len(incoming) == 0 {
		return nil
	}
	if len(covered) == 0 {
		return append([]componentCoverageInterval(nil), incoming...)
	}
	newIntervals := make([]componentCoverageInterval, 0, len(incoming))
	for _, current := range incoming {
		start := current.start
		for _, existing := range covered {
			if existing.end <= start {
				continue
			}
			if existing.start >= current.end {
				break
			}
			if existing.start > start {
				end := existing.start
				if end > current.end {
					end = current.end
				}
				newIntervals = append(newIntervals, componentCoverageInterval{start: start, end: end})
			}
			if existing.end > start {
				start = existing.end
			}
			if start >= current.end {
				break
			}
		}
		if start < current.end {
			newIntervals = append(newIntervals, componentCoverageInterval{start: start, end: current.end})
		}
	}
	return newIntervals
}

func (coverage mappedComponentCoverage) containsInterval(vol *serviceVolumeIndex, id int) bool {
	if vol == nil || id < 0 || id >= vol.index.compactRecordCount() {
		return false
	}
	if id >= len(vol.subtreeStart) {
		return false
	}
	position := vol.subtreeStart[id]
	if position == ^uint32(0) {
		return false
	}
	interval := sort.Search(len(coverage.intervals), func(i int) bool {
		return coverage.intervals[i].start > position
	}) - 1
	return interval >= 0 && position < coverage.intervals[interval].end
}

func (coverage mappedComponentCoverage) contains(vol *serviceVolumeIndex, id int) bool {
	if id >= 0 && id/64 < len(coverage.membership) {
		return coverage.membership[id/64]&(uint64(1)<<uint(id%64)) != 0
	}
	if coverage.containsInterval(vol, id) {
		return true
	}
	pos := sort.SearchInts(coverage.selfIDs, id)
	return pos < len(coverage.selfIDs) && coverage.selfIDs[pos] == id
}

// buildMembership creates a compact transient membership filter for the
// persisted-order top-N driver. It is bounded by one bit per record and avoids
// repeating a binary search over merged SUBT intervals for every rank entry;
// it does not materialize CompactRecord values or alter the persisted index.
func (coverage *mappedComponentCoverage) buildMembership(vol *serviceVolumeIndex) {
	if coverage == nil || vol == nil || vol.index == nil || coverage.cardinality <= 0 {
		return
	}
	recordCount := vol.index.compactRecordCount()
	if recordCount <= 0 {
		return
	}
	coverage.membership = make([]uint64, (recordCount+63)/64)
	for _, current := range coverage.intervals {
		for pos := current.start; pos < current.end && int(pos) < len(vol.subtreeOrder); pos++ {
			id := vol.subtreeOrder[pos]
			if int(id) < recordCount {
				coverage.membership[id/64] |= uint64(1) << uint(id%64)
			}
		}
	}
	for _, id := range coverage.selfIDs {
		if id >= 0 && id < recordCount {
			coverage.membership[id/64] |= uint64(1) << uint(id%64)
		}
	}
}

func (vol *serviceVolumeIndex) buildMappedComponentCoverage(component string, selfIDs []int) (mappedComponentCoverage, bool) {
	coverage := mappedComponentCoverage{selfIDs: uniqueSortedInts(append([]int(nil), selfIDs...)), selfIDsComplete: true}
	if vol == nil || vol.index == nil || component == "" ||
		len(vol.subtreeOrder) == 0 || len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 {
		return coverage, false
	}
	candidate, ok := vol.componentPostingCountCandidate(component)
	if !ok || (!candidate.mapped && candidate.ids == nil) {
		return coverage, false
	}
	return vol.buildMappedComponentCoverageFromRoots(candidate.materialize(), coverage.selfIDs)
}

func (vol *serviceVolumeIndex) buildMappedComponentCoverageFromRoots(roots []uint32, selfIDs []int) (mappedComponentCoverage, bool) {
	coverage := mappedComponentCoverage{selfIDs: uniqueSortedInts(append([]int(nil), selfIDs...)), selfIDsComplete: true}
	if vol == nil || vol.index == nil || len(vol.subtreeOrder) == 0 || len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 {
		return coverage, false
	}
	uniqueRoots := make(map[uint32]struct{}, len(roots))
	for _, root := range roots {
		uniqueRoots[root] = struct{}{}
	}
	roots = roots[:0]
	for root := range uniqueRoots {
		roots = append(roots, root)
	}
	coverage.rootCount = len(roots)
	intervals := make([]componentCoverageInterval, 0, len(roots))
	for _, root32 := range roots {
		root := int(root32)
		if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) {
			return mappedComponentCoverage{}, false
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			return mappedComponentCoverage{}, false
		}
		intervals = append(intervals, componentCoverageInterval{start: start, end: end})
	}
	coverage.intervals = mergeComponentCoverageIntervals(intervals)
	if !vol.baseHasDeletedRecords() {
		for _, current := range coverage.intervals {
			coverage.cardinality += int(current.end - current.start)
		}
	} else {
		for _, current := range coverage.intervals {
			for pos := current.start; pos < current.end; pos++ {
				id := int(vol.subtreeOrder[pos])
				if id >= 0 && id < vol.index.compactRecordCount() && !vol.index.compactRecord(id).Deleted {
					coverage.cardinality++
				}
			}
		}
	}
	for _, id := range coverage.selfIDs {
		if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted ||
			vol.index.compactRecord(id).Mode&uint32(os.ModeDir) != 0 || coverage.containsInterval(vol, id) {
			continue
		}
		coverage.cardinality++
	}
	return coverage, true
}

func (coverage mappedComponentCoverage) countLive(vol *serviceVolumeIndex, hidden func(int) bool) (count, verified int) {
	if vol == nil || vol.index == nil {
		return 0, 0
	}
	if hidden == nil && !vol.baseHasDeletedRecords() {
		count = coverage.cardinality
		return count, 0
	}
	for _, current := range coverage.intervals {
		for pos := current.start; pos < current.end; pos++ {
			id := int(vol.subtreeOrder[pos])
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			verified++
			if vol.index.compactRecord(id).Deleted || (hidden != nil && hidden(id)) {
				continue
			}
			count++
		}
	}
	for _, id := range coverage.selfIDs {
		if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted ||
			vol.index.compactRecord(id).Mode&uint32(os.ModeDir) != 0 || coverage.containsInterval(vol, id) {
			continue
		}
		verified++
		if hidden == nil || !hidden(id) {
			count++
		}
	}
	return count, verified
}

// countMappedComponentSelfHits walks the persisted SUBT order and advances
// over merged descendant intervals monotonically.  It reads folded LOWR bytes
// and record flags directly from the mapped tables; no paths, strings, or ID
// slices are materialized.  The caller adds hidden/overlay corrections once
// at the terminal count.
func (vol *serviceVolumeIndex) countMappedComponentSelfHits(term string, coverage mappedComponentCoverage, hidden func(int) bool, pq parsedQuery) (count, visited int, ok bool) {
	if vol == nil || vol.index == nil || term == "" || len(vol.subtreeOrder) < vol.index.compactRecordCount() {
		return 0, 0, false
	}
	termBytes := []byte(term)
	intervalPos := 0
	order := vol.subtreeOrder
	if m := vol.index.MMapRecords; m != nil {
		derived := m.fileDerived()
		if len(derived.LowerOffs) == 0 || len(derived.LowerLens) != len(derived.LowerOffs) {
			return 0, 0, false
		}
		size := compactDiskRecordBytes
		refBytes := 6
		if m.wideRefs {
			size = compactWideDiskRecordBytes
			refBytes = 8
		}
		lowerBytes := func(nameID uint32) []byte {
			if nameID >= uint32(len(derived.LowerOffs)) {
				return nil
			}
			off := derived.LowerOffs[nameID]
			if off == packedLowerSameAsName {
				nameOff := int(nameID) * 6
				if nameOff < 0 || nameOff+6 > len(m.tokenTable) {
					return nil
				}
				start := binary.LittleEndian.Uint32(m.tokenTable[nameOff:])
				length := binary.LittleEndian.Uint16(m.tokenTable[nameOff+4:])
				end := int(start) + int(length)
				if end < int(start) || end > len(m.nameBlob) {
					return nil
				}
				return m.nameBlob[int(start):end]
			}
			end := int(off) + int(derived.LowerLens[nameID])
			if end < int(off) || end > len(derived.LowerBlob) {
				return nil
			}
			return derived.LowerBlob[int(off):end]
		}
		for pos := 0; pos < len(order); {
			if pos&1023 == 0 && queryCanceled(pq) {
				return 0, visited, false
			}
			for intervalPos < len(coverage.intervals) && uint32(pos) >= coverage.intervals[intervalPos].end {
				intervalPos++
			}
			if intervalPos < len(coverage.intervals) && uint32(pos) >= coverage.intervals[intervalPos].start {
				pos = int(coverage.intervals[intervalPos].end)
				continue
			}
			id := int(order[pos])
			pos++
			if id < 0 || id >= m.count {
				continue
			}
			visited++
			base, valid := m.recordOffset(id)
			if !valid || m.recordData[base+size-1] != 0 {
				continue
			}
			modeOff := base + 16 + refBytes
			if binary.LittleEndian.Uint32(m.recordData[modeOff:])&uint32(os.ModeDir) != 0 {
				continue
			}
			_, nameID := m.recordRefs(base + 16)
			if bytes.Contains(lowerBytes(nameID), termBytes) && (hidden == nil || !hidden(id)) {
				count++
			}
		}
		return count, visited, true
	}
	if p := vol.index.PackedRecords; p != nil && p.Len() >= len(order) {
		for pos := 0; pos < len(order); {
			if pos&1023 == 0 && queryCanceled(pq) {
				return 0, visited, false
			}
			for intervalPos < len(coverage.intervals) && uint32(pos) >= coverage.intervals[intervalPos].end {
				intervalPos++
			}
			if intervalPos < len(coverage.intervals) && uint32(pos) >= coverage.intervals[intervalPos].start {
				pos = int(coverage.intervals[intervalPos].end)
				continue
			}
			id := int(order[pos])
			pos++
			if id < 0 || id >= p.Len() {
				continue
			}
			visited++
			rec := p.At(id)
			if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 || hidden != nil && hidden(id) {
				continue
			}
			if strings.Contains(p.lowerNameAt(id), term) {
				count++
			}
		}
		return count, visited, true
	}
	return 0, visited, false
}

func (vol *serviceVolumeIndex) componentSelfHits(component string, pq parsedQuery) ([]int, bool) {
	namePQ := pq
	namePQ.MatchPath = false
	namePQ.Terms = []string{component}
	if vol != nil && vol.nameTrigramIndex() != nil {
		ids, ok := vol.filenameTrigramCandidates(namePQ)
		if !ok {
			return nil, false
		}
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) == 0 && strings.Contains(vol.index.compactLowerNameAt(id), strings.ToLower(component)) {
				out = append(out, id)
			}
		}
		return uniqueSortedInts(out), true
	}
	ids := vol.nameTermPosting(strings.ToLower(component))
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && rec.Mode&uint32(os.ModeDir) == 0 {
			out = append(out, id)
		}
	}
	return uniqueSortedInts(out), true
}

func (vol *serviceVolumeIndex) mappedComponentCoverageForQuery(component string, pq parsedQuery) (mappedComponentCoverage, bool) {
	if candidate, candidateOK := vol.componentPostingCountCandidate(component); candidateOK && candidate.mapped && candidate.len() == 0 {
		return mappedComponentCoverage{}, false
	}
	selfIDs, ok := vol.componentSelfHits(component, pq)
	if !ok {
		return mappedComponentCoverage{}, false
	}
	coverage, ok := vol.buildMappedComponentCoverage(component, selfIDs)
	if ok {
		if candidate, candidateOK := vol.componentPostingCountCandidate(component); candidateOK && candidate.mapped && candidate.len() == 0 {
			// A missing exact key does not prove that no longer directory name
			// contains this substring; let the complete PCMP dictionary route
			// enumerate supersets instead.
			return mappedComponentCoverage{}, false
		}
	}
	return coverage, ok
}

// mappedComponentSubstringCoverage is the complete fallback when PNGR cannot
// prove filename self-hit coverage.  PCMP's key dictionary is complete for
// persisted directory components, so enumerate only keys containing the
// query and decode their root postings.  LOWR/name order supplies the complete
// base filename verification without reconstructing paths.
func (vol *serviceVolumeIndex) mappedComponentSubstringCoverage(component string) (mappedComponentCoverage, bool) {
	return vol.mappedComponentSubstringCoverageMode(component, true)
}

func (vol *serviceVolumeIndex) mappedComponentSubstringCoverageForTop(component string) (mappedComponentCoverage, bool) {
	return vol.mappedComponentSubstringCoverageMode(component, false)
}

func (vol *serviceVolumeIndex) mappedComponentSubstringCoverageMode(component string, includeSelfIDs bool) (mappedComponentCoverage, bool) {
	if vol == nil || vol.index == nil || component == "" || vol.index.Derived.Postings == nil {
		return mappedComponentCoverage{}, false
	}
	key := strings.ToLower(component)
	if includeSelfIDs {
		vol.index.componentCoverageMu.Lock()
		if vol.index.componentCoverageCache != nil {
			if coverage, ok := vol.index.componentCoverageCache[key]; ok {
				vol.index.componentCoverageMu.Unlock()
				return coverage, true
			}
		}
		vol.index.componentCoverageMu.Unlock()
	}

	section, exists := vol.index.Derived.Postings[indexSectionPCMP]
	if !exists || len(section.Data) == 0 {
		return mappedComponentCoverage{}, false
	}
	keys, complete := section.matchingStringPostingKeys(key)
	if !complete {
		return mappedComponentCoverage{}, false
	}
	roots := make([]uint32, 0, len(keys))
	for _, postingKey := range keys {
		it, _, ok := section.stringPostingIterator(postingKey)
		if !ok {
			return mappedComponentCoverage{}, false
		}
		for {
			ids, _, ok := it.nextBlock()
			if !ok {
				break
			}
			roots = append(roots, ids...)
		}
	}
	order := vol.mappedOrCompactNameOrder()
	recordCount := vol.index.compactRecordCount()
	if len(order) < recordCount {
		return mappedComponentCoverage{}, false
	}
	selfIDs := []int(nil)
	if includeSelfIDs {
		selfIDs = make([]int, 0, 32)
		for _, id32 := range order {
			id := int(id32)
			if id < 0 || id >= recordCount {
				return mappedComponentCoverage{}, false
			}
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) == 0 && strings.Contains(vol.index.compactLowerNameAt(id), key) {
				selfIDs = append(selfIDs, id)
			}
		}
	}
	coverage, ok := vol.buildMappedComponentCoverageFromRoots(roots, selfIDs)
	if !ok {
		return mappedComponentCoverage{}, false
	}
	coverage.selfIDsComplete = includeSelfIDs
	if includeSelfIDs {
		vol.index.componentCoverageMu.Lock()
		if vol.index.componentCoverageCache == nil {
			vol.index.componentCoverageCache = make(map[string]mappedComponentCoverage)
		}
		vol.index.componentCoverageCache[key] = coverage
		vol.index.componentCoverageMu.Unlock()
	}
	return coverage, true
}

// mappedComponentCount counts the exact mapped union without materializing
// every descendant ID as a separate posting.  It remains as a small wrapper
// for callers/tests that do not have a parsed query and therefore use the
// complete legacy name scan for file self-hits.
func (vol *serviceVolumeIndex) mappedComponentCount(component string) (int, bool) {
	if vol == nil || vol.index == nil || component == "" ||
		len(vol.subtreeOrder) == 0 || len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 {
		return 0, false
	}
	selfIDs := vol.nameTermPosting(strings.ToLower(component))
	coverage, ok := vol.buildMappedComponentCoverage(component, selfIDs)
	if !ok {
		return 0, false
	}
	count, _ := coverage.countLive(vol, nil)
	return count, true
}

func (vol *serviceVolumeIndex) pathComponentRootIDs(term string) []int {
	if vol == nil || vol.index == nil || term == "" {
		return nil
	}
	term = strings.ToLower(term)
	if ids := vol.componentPosting32(term); len(ids) > 0 {
		out := make([]int, 0, len(ids))
		for _, id := range ids {
			out = append(out, int(id))
		}
		return out
	}
	if vol.queryIndex == nil {
		return vol.exactNameIDs(term)
	}
	candidates := make([]uint32, 0, 8)
	for _, id := range vol.exactNameIDs(term) {
		if id >= 0 && id < vol.index.compactRecordCount() {
			rec := vol.index.compactRecord(id)
			if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
				candidates = append(candidates, uint32(id))
			}
		}
	}
	if len(candidates) == 0 && len(term) >= 3 && vol.queryIndex.pathGrams != nil {
		grams := componentGrams(term)
		lists := make([][]uint32, 0, len(grams))
		for _, gram := range grams {
			list := vol.queryIndex.pathGrams[gram]
			if len(list) == 0 {
				return nil
			}
			lists = append(lists, list)
		}
		sortUint32ListsByLen(lists)
		if len(lists) > 0 {
			candidates = append([]uint32(nil), lists[0]...)
			for _, list := range lists[1:] {
				candidates = intersectSortedUint32s(candidates, list)
				if len(candidates) == 0 {
					return nil
				}
			}
		}
	}
	out := make([]int, 0, len(candidates))
	for _, id32 := range candidates {
		id := int(id32)
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if strings.Contains(vol.index.compactLowerNameAt(id), term) {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return uniqueSortedInts(out)
}

func (vol *serviceVolumeIndex) scanPathTermPosting(term string) []int {
	recordCount := vol.index.compactRecordCount()
	workers := min(runtime.GOMAXPROCS(0), max(1, recordCount/250_000))
	if workers <= 1 {
		out := make([]int, 0, 64)
		for i := 0; i < recordCount; i++ {
			rec := vol.index.compactRecord(i)
			if !rec.Deleted && vol.index.compactPathContainsTerm(i, term) {
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
				if !rec.Deleted && vol.index.compactPathContainsTerm(i, term) {
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

func (vol *serviceVolumeIndex) pathTermRootIDs(term string) []int {
	roots := vol.pathComponentRootIDs(term)
	if len(roots) == 0 {
		roots = vol.exactNameIDs(term)
	}
	for _, id := range vol.nameTermPosting(term) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 || vol.index.compactLowerNameAt(id) == term {
			continue
		}
		roots = append(roots, id)
	}
	return roots
}

func (vol *serviceVolumeIndex) extPosting(ext string) []int {
	if ids32 := vol.extPosting32(ext); ids32 != nil {
		list := make([]int, 0, len(ids32))
		for _, id := range ids32 {
			list = append(list, int(id))
		}
		return vol.withRecentCandidates(list, 0, func(rec CompactRecord) bool {
			actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
			return strings.EqualFold(actual, ext)
		})
	}
	vol.termMu.Lock()
	if vol.extCache == nil {
		vol.extCache = make(map[string]postingCacheEntry)
	}
	if entry, ok := vol.extCache[ext]; ok {
		if vol.cacheStampValid(entry.gen) {
			vol.termMu.Unlock()
			return vol.withRecentCandidates(entry.ids, entry.gen, func(rec CompactRecord) bool {
				actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
				return strings.EqualFold(actual, ext)
			})
		}
	}
	vol.termMu.Unlock()

	list := make([]int, 0, 64)
	for i := 0; i < vol.index.compactRecordCount(); i++ {
		rec := vol.index.compactRecord(i)
		if rec.Deleted {
			continue
		}
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if strings.EqualFold(actual, ext) {
			list = append(list, i)
		}
	}
	vol.cacheExtPosting(ext, list)
	return list
}

func (vol *serviceVolumeIndex) extPosting32(ext string) []uint32 {
	if vol == nil {
		return nil
	}
	key := strings.ToLower(ext)
	if vol.index != nil && vol.index.Derived.Postings != nil {
		if ids := vol.index.Derived.Postings[indexSectionPEXT].stringPosting(key); ids != nil {
			return ids
		}
	}
	if vol.queryIndex != nil && vol.queryIndex.ext != nil {
		if ids, ok := vol.queryIndex.ext[key]; ok {
			return ids
		}
	}
	return nil
}

func (vol *serviceVolumeIndex) extPostingCountCandidate(ext string) (postingCountCandidate, bool) {
	if vol == nil {
		return postingCountCandidate{}, false
	}
	key := strings.ToLower(ext)
	if vol.index != nil && vol.index.Derived.Postings != nil {
		if section, exists := vol.index.Derived.Postings[indexSectionPEXT]; exists {
			if it, count, ok := section.stringPostingIterator(key); ok {
				return postingCountCandidate{it: it, count: count, mapped: true}, true
			}
			return postingCountCandidate{mapped: true}, true
		}
	}
	if vol.queryIndex != nil && vol.queryIndex.ext != nil {
		if ids, ok := vol.queryIndex.ext[key]; ok {
			return postingCountCandidate{ids: ids}, true
		}
		return postingCountCandidate{}, true
	}
	return postingCountCandidate{}, false
}

func (vol *serviceVolumeIndex) extPostingCount(ext string) int {
	candidate, ok := vol.extPostingCountCandidate(ext)
	if !ok {
		return 0
	}
	return candidate.len()
}

func (vol *serviceVolumeIndex) componentPosting32(component string) []uint32 {
	if vol == nil {
		return nil
	}
	key := strings.ToLower(component)
	if vol.index != nil && vol.index.Derived.Postings != nil {
		if ids := vol.index.Derived.Postings[indexSectionPCMP].stringPosting(key); ids != nil {
			return ids
		}
	}
	if vol.queryIndex != nil && vol.queryIndex.components != nil {
		if ids, ok := vol.queryIndex.components[key]; ok {
			return ids
		}
	}
	return nil
}

func (vol *serviceVolumeIndex) componentPostingBlockIterator(component string) (postingBlockIterator, int, bool) {
	if vol == nil || vol.index == nil || vol.index.Derived.Postings == nil {
		return postingBlockIterator{}, 0, false
	}
	key := strings.ToLower(component)
	return vol.index.Derived.Postings[indexSectionPCMP].stringPostingIterator(key)
}

func (vol *serviceVolumeIndex) componentPostingCount(component string) int {
	candidate, ok := vol.componentPostingCountCandidate(component)
	if !ok {
		return 0
	}
	return candidate.len()
}

func (vol *serviceVolumeIndex) componentPostingCountCandidate(component string) (postingCountCandidate, bool) {
	if vol == nil {
		return postingCountCandidate{}, false
	}
	key := strings.ToLower(component)
	if vol.index != nil && vol.index.Derived.Postings != nil {
		if section, exists := vol.index.Derived.Postings[indexSectionPCMP]; exists {
			if it, count, ok := section.stringPostingIterator(key); ok {
				return postingCountCandidate{it: it, count: count, mapped: true}, true
			}
			return postingCountCandidate{mapped: true}, true
		}
	}
	if vol.queryIndex != nil && vol.queryIndex.components != nil {
		if ids, ok := vol.queryIndex.components[key]; ok {
			return postingCountCandidate{ids: ids}, true
		}
		return postingCountCandidate{}, true
	}
	return postingCountCandidate{}, false
}

func (vol *serviceVolumeIndex) extTopPosting(ext string, limit int, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.queryIndex == nil || limit <= 0 {
		return nil, false
	}
	if vol.queryIndex.extTop != nil && pq.SortColumn == "" {
		ids32, ok := vol.queryIndex.extTop[strings.ToLower(ext)]
		if !ok || len(ids32) < limit {
			return nil, false
		}
		list := make([]int, 0, len(ids32)+len(vol.recentIDs))
		seen := make(map[int]struct{}, len(ids32)+len(vol.recentIDs))
		for _, id32 := range ids32 {
			id := int(id32)
			list = append(list, id)
			seen[id] = struct{}{}
		}
		for id := range vol.recentIDs {
			if _, exists := seen[id]; exists || id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			rec := vol.index.compactRecord(id)
			actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
			if strings.EqualFold(actual, ext) {
				list = append(list, id)
			}
		}
		return topCandidateIDsByRank(list, limit, vol.index, vol.nameOrderRanks()), true
	}
	if ids, ok := vol.mappedExtTopPosting(ext, limit, pq); ok {
		return ids, true
	}
	return nil, false
}

// countExtPostingWithRecent is the count-side twin of extTopPosting's recentID
// merge.  The legacy engine mutates base records in place and only tracks live
// changes in vol.recentIDs (no overlay snapshot is published), so the persisted
// posting count must be reconciled against recent records whose actual
// extension matches.  It mirrors extTopPosting's merge gate (resident extTop
// and default sort) so search and count stay in parity.
func (vol *serviceVolumeIndex) countExtPostingWithRecent(ext string, pq parsedQuery) (int, bool) {
	if vol == nil {
		return 0, false
	}
	posting, ok := vol.extPostingCountCandidate(ext)
	if !ok {
		return 0, false
	}
	if pq.SortColumn != "" || vol.queryIndex == nil || vol.queryIndex.extTop == nil || len(vol.recentIDs) == 0 {
		return posting.len(), true
	}
	ids := posting.materialize()
	seen := make(map[int]struct{}, len(ids))
	for _, id32 := range ids {
		seen[int(id32)] = struct{}{}
	}
	count := len(ids)
	for id := range vol.recentIDs {
		if _, exists := seen[id]; exists || id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if strings.EqualFold(actual, ext) {
			count++
		}
	}
	return count, true
}

func (vol *serviceVolumeIndex) mappedExtTopPosting(ext string, limit int, pq parsedQuery) ([]int, bool) {
	if limit <= 0 || limit > serviceExtTopPostingLimit {
		return nil, false
	}
	candidate, ok := vol.extPostingCountCandidate(ext)
	if !ok || !candidate.mapped || candidate.len() == 0 {
		return nil, false
	}
	ranks := vol.rankForQuery(pq)
	boundSort := pq.SortColumn
	if boundSort == "" && pq.MatchPath {
		boundSort = "type"
	}
	blocks, canSkipByBlockRank := candidate.it.rankOrderedBlockRefsForSort(boundSort)
	if len(blocks) == 0 {
		return nil, false
	}
	h := make(extRankMaxHeap, 0, limit)
	recordCount := vol.index.compactRecordCount()
	decodedBlocks := 0
	skippedBlocks := 0
	for blockPos, ref := range blocks {
		if canSkipByBlockRank && len(h) >= limit && ref.meta.minRank > h[0].rank {
			skippedBlocks = len(blocks) - blockPos
			break
		}
		ids, _, ok := candidate.it.blockAt(ref.index)
		if !ok {
			return nil, false
		}
		decodedBlocks++
		for _, id := range ids {
			if int(id) < 0 || int(id) >= recordCount {
				continue
			}
			rec := vol.index.compactRecord(int(id))
			if rec.Deleted {
				continue
			}
			item := extRankItem{id: id, rank: extRankOf(id, ranks)}
			if len(h) < limit {
				heap.Push(&h, item)
				continue
			}
			if extRankLess(item, h[0]) {
				h[0] = item
				heap.Fix(&h, 0)
			}
		}
	}
	pq.Trace.addPostingBlocks(decodedBlocks, skippedBlocks)
	if len(h) == 0 {
		return []int{}, true
	}
	top := make([]uint32, len(h), len(h)+len(vol.recentIDs))
	seen := make(map[uint32]struct{}, len(h)+len(vol.recentIDs))
	for i := range h {
		top[i] = h[i].id
		seen[h[i].id] = struct{}{}
	}
	for id := range vol.recentIDs {
		if id < 0 || id >= recordCount {
			continue
		}
		id32 := uint32(id)
		if _, exists := seen[id32]; exists {
			continue
		}
		rec := vol.index.compactRecord(id)
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if rec.Deleted || !strings.EqualFold(actual, ext) {
			continue
		}
		top = append(top, id32)
	}
	sortExtTopByRank(top, ranks)
	if len(top) > limit {
		top = top[:limit]
	}
	out := make([]int, len(top))
	for i, id := range top {
		out[i] = int(id)
	}
	pq.Trace.addTerm(traceTerm{
		Term:      strings.ToLower(ext),
		Kind:      "extension",
		Source:    "mapped-ext-top",
		CountHint: candidate.len(),
		Exact:     true,
		Volume:    vol.index.Volume,
	})
	return out, true
}

// mappedComponentTopPosting returns top-ranked records from component
// subtrees. PCMP block bounds are computed from descendant SUBT minima, so a
// skipped block cannot contain a better descendant than the current heap
// threshold. This route is intentionally narrow; filtered/overlay queries use
// the complete component iterator instead of risking a truncated result.
func (vol *serviceVolumeIndex) mappedComponentTopPosting(component string, limit int, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 || len(vol.recentIDs) > 0 ||
		len(vol.subtreeSizeRank) == 0 || len(vol.subtreeModRank) == 0 || len(vol.subtreeExtRank) == 0 ||
		len(vol.subtreeTypeRank) == 0 || len(vol.subtreePathRank) == 0 {
		return nil, false
	}
	rankPQ := pq
	if rankPQ.MatchPath && rankPQ.SortColumn == "" {
		rankPQ.SortColumn = "type"
	}
	// A complete PCMP dictionary is sufficient to supply directory roots;
	// defer LOWR self-hit verification to the persisted-order driver so top-N
	// searches do not materialize the full self-hit set first.
	if substringCoverage, substringOK := vol.mappedComponentSubstringCoverageForTop(component); substringOK {
		return vol.mappedComponentTopFromCoverage(component, substringCoverage, limit, pq)
	}
	coverage, exactOK := vol.mappedComponentCoverageForQuery(component, pq)
	candidate, candidateOK := vol.componentPostingCountCandidate(component)
	if !exactOK || !candidateOK || !candidate.mapped {
		if substringCoverage, substringOK := vol.mappedComponentSubstringCoverageForTop(component); substringOK {
			return vol.mappedComponentTopFromCoverage(component, substringCoverage, limit, pq)
		}
		return nil, false
	}
	if candidate.len() == 0 {
		if pq.Trace != nil {
			pq.Trace.addComponentStats("self-only", coverage.rootCount, len(coverage.intervals), coverage.cardinality, len(coverage.selfIDs), len(coverage.selfIDs), false)
			pq.Trace.addTerm(traceTerm{Term: strings.ToLower(component), Kind: "path-subtree", Source: "mapped-component-top", CountHint: 0, Exact: true, Volume: vol.index.Volume})
		}
		return topCandidateIDsByRank(coverage.selfIDs, limit, vol.index, vol.rankForQuery(rankPQ)), true
	}
	blocks, canSkipByBlockRank := candidate.it.rankOrderedBlockRefsForSort(rankPQ.SortColumn)
	if len(blocks) == 0 {
		return nil, false
	}
	ranks := vol.rankForQuery(rankPQ)
	h := make(extRankMaxHeap, 0, limit)
	addID := func(id32 uint32) {
		item := extRankItem{id: id32, rank: extRankOf(id32, ranks)}
		if len(h) < limit {
			heap.Push(&h, item)
		} else if extRankLess(item, h[0]) {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	verified := 0
	driver := "interval-bounds"
	decodedBlocks := 0
	skippedBlocks := 0
	// Once the exact coverage is broad, the persisted global order is the
	// cheaper driver: membership checks stop at the requested top-N instead of
	// walking every descendant in a large SUBT interval.  The final global
	// merge still compares actual entries and tie-breaks across volumes.
	order := vol.orderForQuery(rankPQ)
	if coverage.cardinality >= max(limit*8, 4096) && len(order) >= vol.index.compactRecordCount() {
		driver = "persisted-order"
		coverage.buildMembership(vol)
		for _, id32 := range order {
			if queryCanceled(pq) {
				return nil, false
			}
			id := int(id32)
			if !coverage.contains(vol, id) || id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted {
				continue
			}
			verified++
			addID(id32)
			if len(h) >= limit {
				break
			}
		}
		skippedBlocks = len(blocks)
	} else {
		processedIntervals := make([]componentCoverageInterval, 0, candidate.len()/1024+1)
		for blockPos, ref := range blocks {
			if canSkipByBlockRank && len(h) >= limit && ref.meta.minRank > h[0].rank {
				skippedBlocks = len(blocks) - blockPos
				break
			}
			roots, _, ok := candidate.it.blockAt(ref.index)
			if !ok {
				return nil, false
			}
			decodedBlocks++
			blockIntervals := make([]componentCoverageInterval, 0, len(roots))
			for _, root32 := range roots {
				root := int(root32)
				if root < 0 || root >= len(vol.subtreeStart) || root >= len(vol.subtreeEnd) {
					return nil, false
				}
				start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
				if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
					return nil, false
				}
				blockIntervals = append(blockIntervals, componentCoverageInterval{start: start, end: end})
			}
			blockIntervals = mergeComponentCoverageIntervals(blockIntervals)
			for _, current := range subtractComponentCoverageIntervals(blockIntervals, processedIntervals) {
				for pos := current.start; pos < current.end; pos++ {
					id32 := vol.subtreeOrder[pos]
					id := int(id32)
					if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted {
						continue
					}
					verified++
					addID(id32)
				}
			}
			processedIntervals = mergeComponentCoverageIntervals(append(processedIntervals, blockIntervals...))
		}
		for _, id := range coverage.selfIDs {
			if id >= 0 && id < vol.index.compactRecordCount() && !vol.index.compactRecord(id).Deleted && !coverage.containsInterval(vol, id) {
				verified++
				addID(uint32(id))
			}
		}
	}
	if pq.Trace != nil {
		pq.Trace.addPostingBlocks(decodedBlocks, skippedBlocks)
		pq.Trace.addComponentStats(driver, coverage.rootCount, len(coverage.intervals), coverage.cardinality, len(coverage.selfIDs), verified, canSkipByBlockRank)
		pq.Trace.addTerm(traceTerm{Term: strings.ToLower(component), Kind: "path-subtree", Source: "mapped-component-top", CountHint: candidate.len(), Exact: true, Volume: vol.index.Volume})
	}
	out := make([]uint32, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sortExtTopByRank(out, ranks)
	if len(out) > limit {
		out = out[:limit]
	}
	ids := make([]int, len(out))
	for i, id := range out {
		ids[i] = int(id)
	}
	return ids, true
}

func (vol *serviceVolumeIndex) mappedComponentTopFromCoverage(component string, coverage mappedComponentCoverage, limit int, pq parsedQuery) ([]int, bool) {
	if len(coverage.intervals) == 0 {
		if ids, ok := vol.completeSelfNameGramTop(component, limit, pq); ok {
			return ids, true
		}
	}
	if vol == nil || vol.index == nil || limit <= 0 {
		return nil, false
	}
	term := strings.ToLower(component)
	rankPQ := pq
	if rankPQ.MatchPath && rankPQ.SortColumn == "" {
		rankPQ.SortColumn = "type"
	}
	ranks := vol.rankForQuery(rankPQ)
	h := make(extRankMaxHeap, 0, limit)
	addID := func(id int) {
		item := extRankItem{id: uint32(id), rank: extRankOf(uint32(id), ranks)}
		if len(h) < limit {
			heap.Push(&h, item)
		} else if extRankLess(item, h[0]) {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	verified := 0
	driver := "interval-substring"
	selfHitCount := len(coverage.selfIDs)
	order := vol.orderForQuery(rankPQ)
	if coverage.cardinality >= max(limit*8, 4096) && len(order) >= vol.index.compactRecordCount() {
		selfPQ := rankPQ
		// Component default order promotes directories by type; complete
		// self-name postings only contribute file self-hits, so name order is
		// the exact tie-breaker for this subset.
		if selfPQ.SortColumn == "type" {
			selfPQ.SortColumn = ""
		}
		selfTop, selfOK := vol.completeSelfNameGramTop(term, limit, selfPQ)
		if selfOK {
			driver = "persisted-order-pngc-self"
			selfHitCount = len(selfTop)
			seen := make(map[int]struct{}, len(selfTop)+limit)
			for _, id := range selfTop {
				if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted {
					continue
				}
				seen[id] = struct{}{}
				verified++
				addID(id)
			}
			coverage.buildMembership(vol)
			for _, id32 := range order {
				if queryCanceled(pq) {
					return nil, false
				}
				if len(h) >= limit && extRankOf(id32, ranks) > h[0].rank {
					break
				}
				id := int(id32)
				if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted || !coverage.contains(vol, id) {
					continue
				}
				if _, already := seen[id]; already {
					continue
				}
				seen[id] = struct{}{}
				verified++
				addID(id)
			}
		} else {
			driver = "persisted-order-substring"
			for _, id32 := range order {
				if queryCanceled(pq) {
					return nil, false
				}
				id := int(id32)
				if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted {
					continue
				}
				matched := coverage.contains(vol, id)
				if !matched {
					rec := vol.index.compactRecord(id)
					matched = rec.Mode&uint32(os.ModeDir) == 0 && strings.Contains(vol.index.compactLowerNameAt(id), term)
				}
				if !matched {
					continue
				}
				verified++
				addID(id)
				if len(h) >= limit {
					break
				}
			}
		}
	} else {
		if !coverage.selfIDsComplete {
			return nil, false
		}
		for _, current := range coverage.intervals {
			for pos := current.start; pos < current.end; pos++ {
				id := int(vol.subtreeOrder[pos])
				if id < 0 || id >= vol.index.compactRecordCount() || vol.index.compactRecord(id).Deleted {
					continue
				}
				verified++
				addID(id)
			}
		}
		for _, id := range coverage.selfIDs {
			if id >= 0 && id < vol.index.compactRecordCount() && !vol.index.compactRecord(id).Deleted && !coverage.containsInterval(vol, id) {
				verified++
				addID(id)
			}
		}
	}
	if pq.Trace != nil {
		if driver == "persisted-order-substring" {
			pq.Trace.addPostingBlocks(0, max(0, len(order)-verified))
		}
		pq.Trace.addComponentStats(driver, coverage.rootCount, len(coverage.intervals), coverage.cardinality, selfHitCount, verified, false)
		pq.Trace.addTerm(traceTerm{Term: strings.ToLower(component), Kind: "path-substring", Source: "mapped-component-substring", CountHint: coverage.cardinality, Exact: true, Volume: vol.index.Volume})
	}
	out := make([]uint32, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sortExtTopByRank(out, ranks)
	if len(out) > limit {
		out = out[:limit]
	}
	ids := make([]int, len(out))
	for i, id := range out {
		ids[i] = int(id)
	}
	return ids, true
}

func (vol *serviceVolumeIndex) extTopPathTermCandidates(ext string, terms []string, limit int) ([]int, bool) {
	if vol == nil || vol.queryIndex == nil || vol.queryIndex.extTop == nil || limit <= 0 || len(terms) == 0 {
		return nil, false
	}
	ids32, ok := vol.queryIndex.extTop[strings.ToLower(ext)]
	if !ok {
		return nil, false
	}
	out := make([]int, 0, limit)
	seen := make(map[int]struct{}, min(len(ids32)+len(vol.recentIDs), serviceExtTopPostingLimit))
	for _, id32 := range ids32 {
		id := int(id32)
		seen[id] = struct{}{}
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsAll(id, terms) {
			continue
		}
		out = append(out, id)
		if len(out) >= limit {
			return out, true
		}
	}
	for id := range vol.recentIDs {
		if _, exists := seen[id]; exists || id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if rec.Deleted || !strings.EqualFold(actual, ext) || !vol.index.compactPathContainsAll(id, terms) {
			continue
		}
		out = append(out, id)
	}
	if len(out) < limit && len(ids32) >= serviceExtTopPostingLimit {
		return nil, false
	}
	return topCandidateIDsByRank(out, limit, vol.index, vol.nameOrderRanks()), true
}

func (vol *serviceVolumeIndex) extPostingPathTermCandidates(ext string, terms []string, limit int) ([]int, bool) {
	if vol == nil || vol.index == nil || limit <= 0 || len(terms) == 0 {
		return nil, false
	}
	ids := vol.extPosting(ext)
	if len(ids) == 0 || len(ids) > serviceComponentMultiTermScanMaxIDs {
		return nil, false
	}
	out := make([]int, 0, min(limit, len(ids)))
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsAll(id, terms) {
			continue
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return []int{}, true
	}
	return topCandidateIDsByRank(out, limit, vol.index, vol.nameOrderRanks()), true
}

func (vol *serviceVolumeIndex) withRecentCandidates(base []int, seq uint64, keep func(CompactRecord) bool) []int {
	if vol == nil {
		return base
	}
	return base
}

func (vol *serviceVolumeIndex) cacheGeneration() uint64 {
	if vol == nil {
		return 0
	}
	if snap := vol.snap.Load(); snap != nil {
		return snap.gen
	}
	return vol.snapshotGen.Load()
}

func (vol *serviceVolumeIndex) cacheStampValid(stamp uint64) bool {
	if vol == nil {
		return true
	}
	return stamp == vol.cacheGeneration()
}

func intersectSortedInts(a, b []int) []int {
	out := a[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

func intersectSortedUint32s(a, b []uint32) []uint32 {
	out := a[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

func materializePostingBlockIterator(it postingBlockIterator, count int) []uint32 {
	if count <= 0 {
		return nil
	}
	out := make([]uint32, 0, count)
	for it.next < it.end {
		ids, _, ok := it.nextBlock()
		if !ok {
			return nil
		}
		out = append(out, ids...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func intersectSortedUint32sWithPostingIterator(a []uint32, it postingBlockIterator) []uint32 {
	out := a[:0]
	cursor := 0
	for cursor < len(a) && it.next < it.end {
		block, meta, ok := it.nextBlock()
		if !ok {
			return nil
		}
		if len(block) == 0 {
			continue
		}
		for cursor < len(a) && a[cursor] < meta.minID {
			cursor++
		}
		if cursor >= len(a) {
			break
		}
		if a[cursor] > meta.maxID {
			continue
		}
		j := 0
		for cursor < len(a) && j < len(block) {
			av := a[cursor]
			if av > meta.maxID {
				break
			}
			bv := block[j]
			switch {
			case av == bv:
				out = append(out, av)
				cursor++
				j++
			case av < bv:
				cursor++
			default:
				j++
			}
		}
		for cursor < len(a) && a[cursor] <= meta.maxID {
			cursor++
		}
	}
	return out
}

func sortUint32s(values []uint32) {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
}

func uniqueSortedInts(in []int) []int {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	last := in[0]
	for _, v := range in[1:] {
		if v == last {
			continue
		}
		out = append(out, v)
		last = v
	}
	return out
}

func uniqueSortedUint32s(in []uint32) []uint32 {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	last := in[0]
	for _, v := range in[1:] {
		if v == last {
			continue
		}
		out = append(out, v)
		last = v
	}
	return out
}

func uint32sToInts(in []uint32) []int {
	out := make([]int, len(in))
	for i, v := range in {
		out[i] = int(v)
	}
	return out
}

func mapKeys(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (idx *Index) compactPathContainsAll(i int, terms []string) bool {
	for _, term := range terms {
		if !idx.compactPathContainsTerm(i, term) {
			return false
		}
	}
	return true
}

// cloneParsedQuery returns a deep copy of pq: every slice field gets its own
// backing array and OR/NOT subqueries are copied recursively, so a caller can
// mutate the result without touching pq. parseQuery uses it on the
// parsedOverride path (the filename-only fallback), which runs only when a
// volume's content index is unusable, not on the hot filename path.
func cloneParsedQuery(pq parsedQuery) parsedQuery {
	out := pq
	out.Terms = append([]string(nil), pq.Terms...)
	out.ImplicitPathTerms = append([]string(nil), pq.ImplicitPathTerms...)
	out.Exts = append([]string(nil), pq.Exts...)
	out.Dirs = append([]string(nil), pq.Dirs...)
	out.Globs = append([]string(nil), pq.Globs...)
	out.Regexps = append([]*regexp.Regexp(nil), pq.Regexps...)
	out.RegexTerms = append([]string(nil), pq.RegexTerms...)
	out.Parents = append([]string(nil), pq.Parents...)
	out.SizeFilters = append([]sizeFilter(nil), pq.SizeFilters...)
	out.DateFilters = append([]dateFilter(nil), pq.DateFilters...)
	out.AttrFilters = append([]uint32(nil), pq.AttrFilters...)
	out.Content = append([]contentLeaf(nil), pq.Content...)
	if len(pq.OrGroups) > 0 {
		out.OrGroups = make([][]parsedQuery, len(pq.OrGroups))
		for i, group := range pq.OrGroups {
			out.OrGroups[i] = make([]parsedQuery, len(group))
			for j := range group {
				out.OrGroups[i][j] = cloneParsedQuery(group[j])
			}
		}
	}
	if len(pq.NotGroups) > 0 {
		out.NotGroups = make([]parsedQuery, len(pq.NotGroups))
		for i := range pq.NotGroups {
			out.NotGroups[i] = cloneParsedQuery(pq.NotGroups[i])
		}
	}
	return out
}

func parseQuery(opts queryOptions) (parsedQuery, error) {
	if opts.parsedOverride != nil {
		// PF-7b: a caller supplied a pre-built tree (the stripped filename-only
		// query). Deep-copy it so a downstream in-place mutator
		// (dropSatisfiedVolumeTerms) cannot reach the shared request-level tree.
		return cloneParsedQuery(*opts.parsedOverride), nil
	}
	pq := parsedQuery{
		Raw:                    opts.Query,
		MatchPath:              opts.MatchPath || queryLooksPathScoped(opts.Query),
		CaseSensitive:          opts.CaseSensitive,
		Fuzzy:                  opts.Fuzzy,
		Under:                  normalizeFilterPath(opts.Under),
		Exists:                 opts.Exists,
		CWDBias:                normalizeFilterPath(opts.CWDBias),
		RootBias:               normalizeFilterPath(opts.RootBias),
		DeadlineUnix:           opts.DeadlineUnix,
		Cancel:                 opts.Cancel,
		Trace:                  opts.Trace,
		ContentCandidateBudget: opts.ContentCandidateBudget,
		ContentScanVisitBudget: opts.ContentScanVisitBudget,
	}
	if opts.ModifiedAfter != "" {
		t, err := parseTimeValue(opts.ModifiedAfter)
		if err != nil {
			return pq, err
		}
		pq.ModifiedAfter = t
		pq.HasModAfter = true
	}
	if opts.Recent != "" {
		d, err := time.ParseDuration(opts.Recent)
		if err != nil {
			return pq, fmt.Errorf("invalid --recent duration: %w", err)
		}
		pq.ModifiedAfter = time.Now().Add(-d)
		pq.HasModAfter = true
	}
	tokens := strings.Fields(opts.Query)
	if queryHasContentToken(opts.Query) {
		// Content queries need the quote/regex-span-aware tokenizer so
		// content:"a b" and content:/a|b/ stay one constraint. Non-content
		// queries keep the historical strings.Fields behavior.
		tokens = contentTokenizeQuery(opts.Query)
	}
	for _, raw := range tokens {
		if implicitPathSeparatorToken(raw) {
			pq.ImplicitPathTerms = append(pq.ImplicitPathTerms, queryPlainTerms(raw, pq.CaseSensitive, true)...)
		}
		if err := applyQueryToken(&pq, raw); err != nil {
			return pq, err
		}
	}
	promotePathExtensionTerms(&pq)
	// Assign deterministic content LeafIDs from the parsed tree, so the content
	// verifier and relevance ordering can key on stable per-leaf IDs. A query
	// with no content leaves is a no-op; a content: token only returns from
	// applyQueryToken, not from parseQuery, so this runs for content queries too.
	contentAssignLeafIDs(&pq)
	if pq.isEmpty() {
		return pq, errors.New("query has no searchable terms or filters")
	}
	return pq, nil
}
