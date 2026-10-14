package main

import (
	"encoding/binary"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
)

const trigramSegmentTargetRecords = 500_000
const trigramBuildMaxWorkers = 4
const trigramStoredPostingMaxCount = 25_000
const trigramLowMemoryStoredPostingMaxCount = 250_000
const gramPostingMetadataMagic = uint32(0x47524d31) // "GRM1"

type trigramSegment struct {
	start        int
	end          int
	postings     map[uint32]compressedPosting
	grams        []uint32
	postingList  []compressedPosting
	flatPostings []flatPosting
	postingBytes int
}

type compressedPosting struct {
	count  int
	data   []byte
	offset uint64
	size   uint32
}

type flatPosting struct {
	count  int
	offset uint64
	size   uint32
}

type compressedTrigramIndex struct {
	segments           []trigramSegment
	counts             map[uint32]int
	countKeys          []uint32
	countValues        []int
	omitted            map[uint32]struct{}
	gramCountsComplete bool
	mappedGrams        *mappedPostingSection
	gramSize           int
	recordCount        int
	postingBytes       int
	gramUnionComplete  bool
}

func buildNameTrigramIndex(idx *Index) *compressedTrigramIndex {
	return buildCompactTrigramIndex(idx, func(id int, cache map[int]string) string {
		return idx.compactLowerNameAt(id)
	})
}

func buildSelectiveNameTrigramIndex(idx *Index, maxPostingCount int) *compressedTrigramIndex {
	return buildSelectiveCompactNameGramIndex(idx, 3, maxPostingCount)
}

// buildSelectiveNameGramIndexWithCompanion is the persist-path entry point: it
// returns the selective PNGR index and the PNGC companion from one pass.
func buildSelectiveNameGramIndexWithCompanion(idx *Index, maxPostingCount int) (*compressedTrigramIndex, *compressedTrigramIndex) {
	return buildSelectiveCompactNameGramIndexWithCompanion(idx, 3, maxPostingCount)
}

func buildSelectiveNameQuadgramIndex(idx *Index, maxPostingCount int) *compressedTrigramIndex {
	return buildSelectiveCompactNameGramIndex(idx, 4, maxPostingCount)
}

func buildPathTrigramIndex(idx *Index) *compressedTrigramIndex {
	return buildCompactTrigramIndex(idx, func(id int, cache map[int]string) string {
		return strings.ToLower(idx.reconstructCompactPathCached(id, cache))
	})
}

func buildCompactTrigramIndex(idx *Index, text func(id int, cache map[int]string) string) *compressedTrigramIndex {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	ti := &compressedTrigramIndex{
		counts:             make(map[uint32]int),
		gramCountsComplete: true,
		gramSize:           3,
		recordCount:        recordCount,
	}
	if idx == nil || recordCount == 0 {
		return ti
	}

	chunks := trigramBuildChunks(recordCount)
	workers := min(len(chunks), min(trigramBuildMaxWorkers, max(1, runtime.GOMAXPROCS(0))))
	results := make([]trigramSegment, len(chunks))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkID := range jobs {
				start, end := chunks[chunkID][0], chunks[chunkID][1]
				results[chunkID] = buildCompactTrigramSegment(idx, start, end, text)
			}
		}()
	}
	for chunkID := range chunks {
		jobs <- chunkID
	}
	close(jobs)
	wg.Wait()

	ti.segments = make([]trigramSegment, 0, len(results))
	for _, segment := range results {
		if segment.end <= segment.start {
			continue
		}
		ti.segments = append(ti.segments, segment)
		ti.postingBytes += segment.postingBytes
		for gram, posting := range segment.postings {
			ti.counts[gram] += posting.count
		}
	}
	return ti
}

func buildSelectiveCompactTrigramIndex(idx *Index, maxPostingCount int, text func(id int, cache map[int]string) string) *compressedTrigramIndex {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	ti := &compressedTrigramIndex{
		counts:             make(map[uint32]int),
		gramCountsComplete: true,
		gramSize:           3,
		recordCount:        recordCount,
	}
	if idx == nil || recordCount == 0 || maxPostingCount <= 0 {
		return ti
	}
	chunks := trigramBuildChunks(recordCount)
	workers := min(len(chunks), min(trigramBuildMaxWorkers, max(1, runtime.GOMAXPROCS(0))))
	localCounts := make([]map[uint32]int, len(chunks))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkID := range jobs {
				start, end := chunks[chunkID][0], chunks[chunkID][1]
				localCounts[chunkID] = countCompactTrigramSegment(idx, start, end, text)
			}
		}()
	}
	for chunkID := range chunks {
		jobs <- chunkID
	}
	close(jobs)
	wg.Wait()
	for _, counts := range localCounts {
		for gram, count := range counts {
			ti.counts[gram] += count
		}
	}
	results := make([]trigramSegment, len(chunks))
	jobs = make(chan int)
	wg = sync.WaitGroup{}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkID := range jobs {
				start, end := chunks[chunkID][0], chunks[chunkID][1]
				results[chunkID] = buildSelectiveCompactTrigramSegment(idx, start, end, text, ti.counts, maxPostingCount)
			}
		}()
	}
	for chunkID := range chunks {
		jobs <- chunkID
	}
	close(jobs)
	wg.Wait()
	ti.segments = make([]trigramSegment, 0, len(results))
	for _, segment := range results {
		if segment.end <= segment.start {
			continue
		}
		ti.segments = append(ti.segments, segment)
		ti.postingBytes += segment.postingBytes
	}
	ti.markOmittedPostings(maxPostingCount)
	return ti
}

func buildSelectiveCompactNameTrigramIndex(idx *Index, maxPostingCount int) *compressedTrigramIndex {
	return buildSelectiveCompactNameGramIndex(idx, 3, maxPostingCount)
}

func buildSelectiveCompactNameGramIndex(idx *Index, gramSize int, maxPostingCount int) *compressedTrigramIndex {
	selective, _ := buildSelectiveCompactNameGramIndexInternal(idx, gramSize, maxPostingCount, false)
	return selective
}

// buildSelectiveCompactNameGramIndexWithCompanion builds the selective PNGR
// index and the PNGC companion (postings for the grams the selective index
// omitted) in a single pass.  The old persist path built a full second index
// with buildNameTrigramIndex just to recover the omitted grams' postings, which
// doubled both the build time and the peak heap; routing stored and omitted
// grams together removes that second full build.
func buildSelectiveCompactNameGramIndexWithCompanion(idx *Index, gramSize int, maxPostingCount int) (*compressedTrigramIndex, *compressedTrigramIndex) {
	return buildSelectiveCompactNameGramIndexInternal(idx, gramSize, maxPostingCount, true)
}

func buildSelectiveCompactNameGramIndexInternal(idx *Index, gramSize int, maxPostingCount int, wantCompanion bool) (ti, companion *compressedTrigramIndex) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	ti = &compressedTrigramIndex{
		counts:             make(map[uint32]int),
		gramCountsComplete: true,
		gramSize:           gramSize,
		recordCount:        recordCount,
	}
	if idx == nil || recordCount == 0 || maxPostingCount <= 0 {
		return ti, nil
	}
	chunks := trigramBuildChunks(recordCount)
	var omittedSegments []trigramSegment
	if serviceLowMemoryMode() {
		for _, chunk := range chunks {
			counts := countCompactNameGramSegment(idx, chunk[0], chunk[1], gramSize)
			for gram, count := range counts {
				ti.counts[gram] += count
			}
		}
		debug.FreeOSMemory()
		ti.segments = make([]trigramSegment, 0, len(chunks))
		for _, chunk := range chunks {
			if wantCompanion {
				stored, omitted := buildSelectiveCompactNameGramSegmentsBoth(idx, chunk[0], chunk[1], gramSize, ti.counts, maxPostingCount)
				if stored.end > stored.start {
					ti.segments = append(ti.segments, stored)
					ti.postingBytes += stored.postingBytes
				}
				if omitted.end > omitted.start && len(omitted.postings) > 0 {
					omittedSegments = append(omittedSegments, omitted)
				}
				continue
			}
			segment := buildSelectiveCompactNameGramSegment(idx, chunk[0], chunk[1], gramSize, ti.counts, maxPostingCount)
			if segment.end <= segment.start {
				continue
			}
			ti.segments = append(ti.segments, segment)
			ti.postingBytes += segment.postingBytes
		}
		ti.markOmittedPostings(maxPostingCount)
		ti.compactForLowMemory()
		return ti, companionFromOmittedSegments(omittedSegments, recordCount, gramSize)
	}
	workers := min(len(chunks), min(trigramBuildMaxWorkers, max(1, runtime.GOMAXPROCS(0))))
	localCounts := make([]map[uint32]int, len(chunks))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkID := range jobs {
				start, end := chunks[chunkID][0], chunks[chunkID][1]
				localCounts[chunkID] = countCompactNameGramSegment(idx, start, end, gramSize)
			}
		}()
	}
	for chunkID := range chunks {
		jobs <- chunkID
	}
	close(jobs)
	wg.Wait()
	for _, counts := range localCounts {
		for gram, count := range counts {
			ti.counts[gram] += count
		}
	}
	storedResults := make([]trigramSegment, len(chunks))
	omittedResults := make([]trigramSegment, len(chunks))
	jobs = make(chan int)
	wg = sync.WaitGroup{}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkID := range jobs {
				start, end := chunks[chunkID][0], chunks[chunkID][1]
				if wantCompanion {
					storedResults[chunkID], omittedResults[chunkID] = buildSelectiveCompactNameGramSegmentsBoth(idx, start, end, gramSize, ti.counts, maxPostingCount)
					continue
				}
				storedResults[chunkID] = buildSelectiveCompactNameGramSegment(idx, start, end, gramSize, ti.counts, maxPostingCount)
			}
		}()
	}
	for chunkID := range chunks {
		jobs <- chunkID
	}
	close(jobs)
	wg.Wait()
	ti.segments = make([]trigramSegment, 0, len(storedResults))
	for _, segment := range storedResults {
		if segment.end <= segment.start {
			continue
		}
		ti.segments = append(ti.segments, segment)
		ti.postingBytes += segment.postingBytes
	}
	ti.markOmittedPostings(maxPostingCount)
	if wantCompanion {
		for i := range omittedResults {
			if omittedResults[i].end > omittedResults[i].start && len(omittedResults[i].postings) > 0 {
				omittedSegments = append(omittedSegments, omittedResults[i])
			}
		}
	}
	return ti, companionFromOmittedSegments(omittedSegments, recordCount, gramSize)
}

// buildSelectiveCompactNameGramSegmentsBoth is the selective build pass with
// both outputs: grams at or below the posting cap go to stored, the rest to
// omitted.  Ids stay in record order, so omitting chunks concatenate into
// globally sorted postings exactly as trigramPostingIDs would produce.
func buildSelectiveCompactNameGramSegmentsBoth(idx *Index, start, end int, gramSize int, counts map[uint32]int, maxPostingCount int) (stored, omitted trigramSegment) {
	stored = trigramSegment{start: start, end: end, postings: make(map[uint32]compressedPosting)}
	omitted = trigramSegment{start: start, end: end, postings: make(map[uint32]compressedPosting)}
	storedRaw := make(map[uint32][]uint32)
	omittedRaw := make(map[uint32][]uint32)
	var grams []uint32
	for id := start; id < end; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		grams = appendUniqueFixedGramKeysFoldASCII(grams, idx.compactLowerNameAt(id), gramSize)
		for _, gram := range grams {
			if counts[gram] > maxPostingCount {
				omittedRaw[gram] = append(omittedRaw[gram], uint32(id))
				continue
			}
			storedRaw[gram] = append(storedRaw[gram], uint32(id))
		}
	}
	for gram, ids := range storedRaw {
		encoded := encodeDeltaUvarint32(ids)
		stored.postings[gram] = compressedPosting{count: len(ids), data: encoded}
		stored.postingBytes += len(encoded)
	}
	for gram, ids := range omittedRaw {
		encoded := encodeDeltaUvarint32(ids)
		omitted.postings[gram] = compressedPosting{count: len(ids), data: encoded}
		omitted.postingBytes += len(encoded)
	}
	return stored, omitted
}

func companionFromOmittedSegments(segments []trigramSegment, recordCount, gramSize int) *compressedTrigramIndex {
	if len(segments) == 0 {
		return nil
	}
	out := &compressedTrigramIndex{
		counts:             make(map[uint32]int),
		gramCountsComplete: true,
		gramSize:           gramSize,
		recordCount:        recordCount,
		segments:           segments,
	}
	for _, segment := range segments {
		for gram, posting := range segment.postings {
			out.counts[gram] += posting.count
			out.postingBytes += len(posting.data)
		}
	}
	if len(out.counts) == 0 {
		return nil
	}
	return out
}

func trigramBuildChunks(recordCount int) [][2]int {
	if recordCount <= 0 {
		return nil
	}
	chunkCount := (recordCount + trigramSegmentTargetRecords - 1) / trigramSegmentTargetRecords
	chunks := make([][2]int, 0, chunkCount)
	for start := 0; start < recordCount; start += trigramSegmentTargetRecords {
		end := min(recordCount, start+trigramSegmentTargetRecords)
		chunks = append(chunks, [2]int{start, end})
	}
	return chunks
}

func buildCompactTrigramSegment(idx *Index, start, end int, text func(id int, cache map[int]string) string) trigramSegment {
	segment := trigramSegment{
		start:    start,
		end:      end,
		postings: make(map[uint32]compressedPosting),
	}
	raw := make(map[uint32][]uint32)
	cache := make(map[int]string)
	var grams []uint32
	for id := start; id < end; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		grams = appendUniqueTrigramKeys(grams, text(id, cache))
		for _, gram := range grams {
			raw[gram] = append(raw[gram], uint32(id))
		}
	}
	for gram, ids := range raw {
		encoded := encodeDeltaUvarint32(ids)
		segment.postings[gram] = compressedPosting{count: len(ids), data: encoded}
		segment.postingBytes += len(encoded)
	}
	return segment
}

func countCompactTrigramSegment(idx *Index, start, end int, text func(id int, cache map[int]string) string) map[uint32]int {
	counts := make(map[uint32]int)
	cache := make(map[int]string)
	var grams []uint32
	for id := start; id < end; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		grams = appendUniqueTrigramKeys(grams, text(id, cache))
		for _, gram := range grams {
			counts[gram]++
		}
	}
	return counts
}

func countCompactNameGramSegment(idx *Index, start, end int, gramSize int) map[uint32]int {
	counts := make(map[uint32]int)
	var grams []uint32
	for id := start; id < end; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		grams = appendUniqueFixedGramKeysFoldASCII(grams, idx.compactLowerNameAt(id), gramSize)
		for _, gram := range grams {
			counts[gram]++
		}
	}
	return counts
}

func buildSelectiveCompactTrigramSegment(idx *Index, start, end int, text func(id int, cache map[int]string) string, counts map[uint32]int, maxPostingCount int) trigramSegment {
	segment := trigramSegment{
		start:    start,
		end:      end,
		postings: make(map[uint32]compressedPosting),
	}
	raw := make(map[uint32][]uint32)
	cache := make(map[int]string)
	var grams []uint32
	for id := start; id < end; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		grams = appendUniqueTrigramKeys(grams, text(id, cache))
		for _, gram := range grams {
			if counts[gram] > maxPostingCount {
				continue
			}
			raw[gram] = append(raw[gram], uint32(id))
		}
	}
	for gram, ids := range raw {
		encoded := encodeDeltaUvarint32(ids)
		segment.postings[gram] = compressedPosting{count: len(ids), data: encoded}
		segment.postingBytes += len(encoded)
	}
	return segment
}

func buildSelectiveCompactNameGramSegment(idx *Index, start, end int, gramSize int, counts map[uint32]int, maxPostingCount int) trigramSegment {
	segment := trigramSegment{
		start:    start,
		end:      end,
		postings: make(map[uint32]compressedPosting),
	}
	raw := make(map[uint32][]uint32)
	var grams []uint32
	for id := start; id < end; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		grams = appendUniqueFixedGramKeysFoldASCII(grams, idx.compactLowerNameAt(id), gramSize)
		for _, gram := range grams {
			if counts[gram] > maxPostingCount {
				continue
			}
			raw[gram] = append(raw[gram], uint32(id))
		}
	}
	for gram, ids := range raw {
		encoded := encodeDeltaUvarint32(ids)
		segment.postings[gram] = compressedPosting{count: len(ids), data: encoded}
		segment.postingBytes += len(encoded)
	}
	return segment
}

func (ti *compressedTrigramIndex) candidateIDs(term string) ([]int, bool) {
	term = strings.ToLower(term)
	grams := ti.termGramKeys(term)
	if len(grams) == 0 || ti == nil {
		return nil, false
	}
	sort.Slice(grams, func(i, j int) bool {
		return ti.countForGram(grams[i]) < ti.countForGram(grams[j])
	})
	if !ti.hasAllGrams(grams) {
		return nil, true
	}
	if ti.mappedGrams != nil {
		var candidates []uint32
		for i, gram := range grams {
			ids := ti.idsForGram(gram)
			if len(ids) == 0 {
				return nil, true
			}
			if i == 0 {
				candidates = ids
				continue
			}
			candidates = intersectSortedUint32s(candidates, ids)
			if len(candidates) == 0 {
				return nil, true
			}
		}
		return uint32sToInts(candidates), true
	}
	out := make([]int, 0, ti.countForGram(grams[0]))
	for _, segment := range ti.segments {
		ids := ti.segmentCandidateIDs(segment, grams)
		out = append(out, uint32sToInts(ids)...)
	}
	return out, true
}

func (ti *compressedTrigramIndex) selectiveCandidateIDs(term string, maxIDs int) ([]int, bool, bool) {
	term = strings.ToLower(term)
	grams := ti.termGramKeys(term)
	if len(grams) == 0 || ti == nil {
		return nil, false, false
	}
	missing := false
	filtered := grams[:0]
	for _, gram := range grams {
		count := ti.countForGram(gram)
		if count == 0 {
			if ti.gramCountsComplete {
				missing = true
			} else {
				return nil, false, false
			}
			continue
		}
		if ti.isOmittedGram(gram) {
			return nil, false, false
		}
		if !ti.hasStoredPosting(gram) {
			// Counted but not stored (a dropped common posting): the
			// posting is a capped hint, not an exact source. Decline so
			// callers fall back to a verified scan instead of serving a
			// false empty.
			return nil, false, false
		}
		if maxIDs <= 0 || count <= maxIDs {
			filtered = append(filtered, gram)
		}
	}
	if missing {
		return nil, true, true
	}
	if len(filtered) == 0 {
		return nil, false, false
	}
	sort.Slice(filtered, func(i, j int) bool {
		return ti.countForGram(filtered[i]) < ti.countForGram(filtered[j])
	})
	ids := ti.idsForGram(filtered[0])
	return uint32sToInts(ids), true, false
}

func (ti *compressedTrigramIndex) selectiveIntersectCandidateIDs(term string, maxIDs int) ([]int, bool, bool) {
	term = strings.ToLower(term)
	grams := ti.termGramKeys(term)
	if len(grams) == 0 || ti == nil {
		return nil, false, false
	}
	for _, gram := range grams {
		count := ti.countForGram(gram)
		if count == 0 {
			if ti.gramCountsComplete {
				return nil, true, true
			}
			return nil, false, false
		}
		if ti.isOmittedGram(gram) {
			return nil, false, false
		}
	}
	stored := grams[:0]
	for _, gram := range grams {
		if ti.hasStoredPosting(gram) {
			stored = append(stored, gram)
		}
	}
	if len(stored) == 0 {
		return nil, false, false
	}
	sort.Slice(stored, func(i, j int) bool {
		return ti.countForGram(stored[i]) < ti.countForGram(stored[j])
	})
	if maxIDs > 0 && ti.countForGram(stored[0]) > maxIDs {
		return nil, false, false
	}
	if ti.mappedGrams != nil {
		firstIt, firstCount, ok := ti.mappedGrams.gramPostingIterator(stored[0])
		if !ok {
			return nil, true, false
		}
		candidates := materializePostingBlockIterator(firstIt, firstCount)
		if len(candidates) == 0 {
			return nil, true, false
		}
		for _, gram := range stored[1:] {
			it, _, ok := ti.mappedGrams.gramPostingIterator(gram)
			if !ok {
				return nil, true, false
			}
			candidates = intersectSortedUint32sWithPostingIterator(candidates, it)
			if len(candidates) == 0 {
				return nil, true, false
			}
			if maxIDs > 0 && len(candidates) > maxIDs {
				return nil, false, false
			}
		}
		return uint32sToInts(candidates), true, false
	}
	out := make([]int, 0, ti.countForGram(stored[0]))
	for _, segment := range ti.segments {
		ids := ti.segmentCandidateIDs(segment, stored)
		if len(ids) == 0 {
			continue
		}
		out = append(out, uint32sToInts(ids)...)
		if maxIDs > 0 && len(out) > maxIDs {
			return nil, false, false
		}
	}
	return out, true, false
}

func (ti *compressedTrigramIndex) hasStoredPosting(gram uint32) bool {
	if ti == nil {
		return false
	}
	if ti.mappedGrams != nil {
		_, _, ok := ti.mappedGrams.gramPostingIterator(gram)
		return ok && !ti.isOmittedGram(gram)
	}
	for _, segment := range ti.segments {
		if segment.postingForGram(gram).count > 0 {
			return true
		}
	}
	return false
}

func (ti *compressedTrigramIndex) postingCount(term string) (int, bool) {
	grams := ti.termGramKeys(strings.ToLower(term))
	if len(grams) == 0 || ti == nil {
		return 0, false
	}
	minCount := -1
	for _, gram := range grams {
		count := ti.countForGram(gram)
		if count == 0 {
			if !ti.gramCountsComplete {
				return 0, false
			}
			return 0, true
		}
		if minCount < 0 || count < minCount {
			minCount = count
		}
	}
	return minCount, true
}

func (ti *compressedTrigramIndex) isOmittedGram(gram uint32) bool {
	if ti == nil || len(ti.omitted) == 0 {
		return false
	}
	_, ok := ti.omitted[gram]
	return ok
}

func (ti *compressedTrigramIndex) lookupState(term string) string {
	if ti == nil {
		return "missing-section"
	}
	for _, gram := range ti.termGramKeys(strings.ToLower(term)) {
		if ti.isOmittedGram(gram) {
			return "omitted-common"
		}
		if ti.countForGram(gram) == 0 {
			if ti.gramCountsComplete {
				return "exact-empty"
			}
			return "missing-section"
		}
	}
	return "present"
}

func (ti *compressedTrigramIndex) markOmittedPostings(maxPostingCount int) {
	if ti == nil || maxPostingCount <= 0 {
		return
	}
	if ti.omitted == nil {
		ti.omitted = make(map[uint32]struct{})
	}
	ti.forEachCount(func(gram uint32, count int) {
		if count > maxPostingCount && !ti.hasStoredPosting(gram) {
			ti.omitted[gram] = struct{}{}
		}
	})
}

func (ti *compressedTrigramIndex) keyCount() int {
	if ti == nil {
		return 0
	}
	if ti.counts != nil {
		return len(ti.counts)
	}
	return len(ti.countKeys)
}

func (ti *compressedTrigramIndex) dropCommonPostings(maxCount int) {
	if ti == nil || maxCount <= 0 {
		return
	}
	ti.forEachCount(func(gram uint32, count int) {
		if count <= maxCount {
			return
		}
		for i := range ti.segments {
			posting := ti.segments[i].postingForGram(gram)
			if posting.count == 0 {
				continue
			}
			ti.postingBytes -= len(posting.data)
			ti.segments[i].postingBytes -= len(posting.data)
			ti.segments[i].deletePosting(gram)
		}
	})
}

func (ti *compressedTrigramIndex) hasAllGrams(grams []uint32) bool {
	for _, gram := range grams {
		if ti.countForGram(gram) == 0 {
			return false
		}
	}
	return true
}

func (ti *compressedTrigramIndex) termGramKeys(term string) []uint32 {
	if ti == nil {
		return nil
	}
	gramSize := ti.gramSize
	if gramSize == 0 {
		gramSize = 3
	}
	return uniqueFixedGramKeys(term, gramSize)
}

func (ti *compressedTrigramIndex) countForGram(gram uint32) int {
	if ti == nil {
		return 0
	}
	if ti.counts != nil {
		return ti.counts[gram]
	}
	i := sort.Search(len(ti.countKeys), func(i int) bool { return ti.countKeys[i] >= gram })
	if i < len(ti.countKeys) && ti.countKeys[i] == gram {
		return ti.countValues[i]
	}
	return 0
}

func (ti *compressedTrigramIndex) forEachCount(fn func(uint32, int)) {
	if ti == nil || fn == nil {
		return
	}
	if ti.counts != nil {
		for gram, count := range ti.counts {
			fn(gram, count)
		}
		return
	}
	for i, gram := range ti.countKeys {
		fn(gram, ti.countValues[i])
	}
}

func (segment *trigramSegment) postingForGram(gram uint32) compressedPosting {
	if segment == nil {
		return compressedPosting{}
	}
	if segment.postings != nil {
		return segment.postings[gram]
	}
	i := sort.Search(len(segment.grams), func(i int) bool { return segment.grams[i] >= gram })
	if i < len(segment.grams) && segment.grams[i] == gram && len(segment.flatPostings) > 0 {
		posting := segment.flatPostings[i]
		return compressedPosting{count: posting.count, offset: posting.offset, size: posting.size}
	}
	if i < len(segment.grams) && segment.grams[i] == gram && len(segment.postingList) > 0 {
		return segment.postingList[i]
	}
	return compressedPosting{}
}

func (segment *trigramSegment) deletePosting(gram uint32) {
	if segment == nil {
		return
	}
	if segment.postings != nil {
		delete(segment.postings, gram)
		return
	}
	i := sort.Search(len(segment.grams), func(i int) bool { return segment.grams[i] >= gram })
	if i >= len(segment.grams) || segment.grams[i] != gram {
		return
	}
	segment.grams = append(segment.grams[:i], segment.grams[i+1:]...)
	if len(segment.flatPostings) > 0 {
		segment.flatPostings = append(segment.flatPostings[:i], segment.flatPostings[i+1:]...)
	}
	if len(segment.postingList) > 0 {
		segment.postingList = append(segment.postingList[:i], segment.postingList[i+1:]...)
	}
}

func (ti *compressedTrigramIndex) compactForLowMemory() {
	if ti == nil {
		return
	}
	if ti.counts != nil {
		ti.countKeys = make([]uint32, 0, len(ti.counts))
		for gram := range ti.counts {
			ti.countKeys = append(ti.countKeys, gram)
		}
		sort.Slice(ti.countKeys, func(i, j int) bool { return ti.countKeys[i] < ti.countKeys[j] })
		ti.countValues = make([]int, len(ti.countKeys))
		for i, gram := range ti.countKeys {
			ti.countValues[i] = ti.counts[gram]
		}
		ti.counts = nil
	}
	for i := range ti.segments {
		segment := &ti.segments[i]
		if segment.postings == nil {
			continue
		}
		segment.grams = make([]uint32, 0, len(segment.postings))
		for gram := range segment.postings {
			segment.grams = append(segment.grams, gram)
		}
		sort.Slice(segment.grams, func(i, j int) bool { return segment.grams[i] < segment.grams[j] })
		segment.postingList = make([]compressedPosting, len(segment.grams))
		for j, gram := range segment.grams {
			segment.postingList[j] = segment.postings[gram]
		}
		segment.postings = nil
	}
	debug.FreeOSMemory()
}

func (ti *compressedTrigramIndex) postingData(posting compressedPosting) []byte {
	if len(posting.data) > 0 {
		return posting.data
	}
	return nil
}

func (ti *compressedTrigramIndex) segmentCandidateIDs(segment trigramSegment, grams []uint32) []uint32 {
	var candidates []uint32
	for _, gram := range grams {
		posting := segment.postingForGram(gram)
		if posting.count == 0 {
			return nil
		}
	}
	for i, gram := range grams {
		posting := segment.postingForGram(gram)
		ids := decodeDeltaUvarint32(ti.postingData(posting), posting.count)
		if i == 0 {
			candidates = ids
			continue
		}
		candidates = intersectSortedUint32s(candidates, ids)
		if len(candidates) == 0 {
			return nil
		}
	}
	return candidates
}

func (ti *compressedTrigramIndex) idsForGram(gram uint32) []uint32 {
	total := ti.countForGram(gram)
	if total == 0 {
		return nil
	}
	if ti.mappedGrams != nil {
		return ti.mappedGrams.gramPosting(gram)
	}
	out := make([]uint32, 0, total)
	for _, segment := range ti.segments {
		posting := segment.postingForGram(gram)
		if posting.count == 0 {
			continue
		}
		out = append(out, decodeDeltaUvarint32(ti.postingData(posting), posting.count)...)
	}
	return out
}

func uniqueTrigramKeys(s string) []uint32 {
	return uniqueFixedGramKeys(s, 3)
}

func uniqueFixedGramKeys(s string, n int) []uint32 {
	if len(s) < 3 {
		return nil
	}
	if n != 3 && n != 4 {
		return nil
	}
	if len(s) < n {
		return nil
	}
	seen := make(map[uint32]struct{}, len(s)-n+1)
	out := make([]uint32, 0, len(s)-n+1)
	for i := 0; i+n <= len(s); i++ {
		key := fixedGramKey(s, i, n)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func uniqueTrigramKeysFoldASCII(s string) []uint32 {
	return uniqueFixedGramKeysFoldASCII(s, 3)
}

func uniqueFixedGramKeysFoldASCII(s string, n int) []uint32 {
	if n != 3 && n != 4 {
		return nil
	}
	if len(s) < n {
		return nil
	}
	seen := make(map[uint32]struct{}, len(s)-n+1)
	out := make([]uint32, 0, len(s)-n+1)
	for i := 0; i+n <= len(s); i++ {
		key := fixedGramKeyFoldASCII(s, i, n)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

// appendUniqueTrigramKeys appends the unique 3-grams of s to dst and returns the
// result.  The segment build loops reuse one scratch slice per worker so the
// per-record map allocation of uniqueFixedGramKeys is avoided.  The returned
// slice aliases dst, so callers must consume it before the next call.
func appendUniqueTrigramKeys(dst []uint32, s string) []uint32 {
	return appendUniqueFixedGramKeys(dst, s, 3)
}

func appendUniqueFixedGramKeys(dst []uint32, s string, n int) []uint32 {
	dst = dst[:0]
	if (n != 3 && n != 4) || len(s) < n {
		return dst
	}
	for i := 0; i+n <= len(s); i++ {
		key := fixedGramKey(s, i, n)
		dup := false
		for _, k := range dst {
			if k == key {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, key)
		}
	}
	return dst
}

func appendUniqueFixedGramKeysFoldASCII(dst []uint32, s string, n int) []uint32 {
	dst = dst[:0]
	if (n != 3 && n != 4) || len(s) < n {
		return dst
	}
	for i := 0; i+n <= len(s); i++ {
		key := fixedGramKeyFoldASCII(s, i, n)
		dup := false
		for _, k := range dst {
			if k == key {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, key)
		}
	}
	return dst
}

func fixedGramKey(s string, start int, n int) uint32 {
	if n == 4 {
		return uint32(s[start])<<24 | uint32(s[start+1])<<16 | uint32(s[start+2])<<8 | uint32(s[start+3])
	}
	return uint32(s[start])<<16 | uint32(s[start+1])<<8 | uint32(s[start+2])
}

func fixedGramKeyFoldASCII(s string, start int, n int) uint32 {
	if n == 4 {
		return uint32(foldASCII(s[start]))<<24 | uint32(foldASCII(s[start+1]))<<16 | uint32(foldASCII(s[start+2]))<<8 | uint32(foldASCII(s[start+3]))
	}
	return uint32(foldASCII(s[start]))<<16 | uint32(foldASCII(s[start+1]))<<8 | uint32(foldASCII(s[start+2]))
}

func trigramKey(s string) uint32 {
	if len(s) != 3 {
		return 0
	}
	return uint32(s[0])<<16 | uint32(s[1])<<8 | uint32(s[2])
}

func trigramStringFromKey(key uint32) string {
	var buf [3]byte
	buf[0] = byte(key >> 16)
	buf[1] = byte(key >> 8)
	buf[2] = byte(key)
	return string(buf[:])
}

func trigramKeyFoldASCII(a, b, c byte) uint32 {
	return uint32(foldASCII(a))<<16 | uint32(foldASCII(b))<<8 | uint32(foldASCII(c))
}

func foldASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func encodeDeltaUvarint32(ids []uint32) []byte {
	if len(ids) == 0 {
		return nil
	}
	out := make([]byte, 0, len(ids)*2)
	var prev uint32
	var buf [binary.MaxVarintLen32]byte
	for _, id := range ids {
		delta := id - prev
		n := binary.PutUvarint(buf[:], uint64(delta))
		out = append(out, buf[:n]...)
		prev = id
	}
	return out
}

func decodeDeltaUvarint32(encoded []byte, count int) []uint32 {
	out := make([]uint32, 0, count)
	var prev uint32
	for len(encoded) > 0 {
		value, n := binary.Uvarint(encoded)
		if n <= 0 {
			return out
		}
		id := prev + uint32(value)
		out = append(out, id)
		prev = id
		encoded = encoded[n:]
	}
	return out
}
