package main

// Read side of the offline content index: dictionary lookup, trigram candidate
// generation, and substring verification against the stored text.

import (
	"bytes"
	"errors"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Snippet window bounds. The window is measured in runes (not bytes) and gets
// an ASCII ellipsis on each truncated side.
const (
	contentSnippetMaxRunes     = 200
	contentSnippetContextRunes = 60
)

var contentSnippetSpace = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ")

// contentHit is one matching document.
type contentHit struct {
	Path    string
	DocID   uint32
	Offset  int
	Snippet string
}

// contentSnippetWindow returns a bounded window of text around the match at byte
// offset off. Newlines and tabs are collapsed to spaces so the snippet stays one
// line. Only the bytes around the window are decoded, so the work is O(window)
// rather than O(len(text)); the start and end snap to rune boundaries so a
// multibyte rune is never split. text must be valid UTF-8 (the stored form),
// which preserves case since PF-6a, so no case-carrying source is needed.
func contentSnippetWindow(text []byte, off, matchLen int) string {
	// off == len(text) is not a match start: there is no byte to window around,
	// and indexing text[off] below would panic.
	if off < 0 || off >= len(text) {
		return ""
	}
	if matchLen < 0 {
		matchLen = 0
	}
	end := off + matchLen
	if end > len(text) {
		end = len(text)
	}
	// Snap to rune boundaries: a caller offset can land inside a multibyte rune
	// (and a boundary-cut window must not split one). off < len(text) is
	// rechecked so the loop can never index past the end.
	for off > 0 && off < len(text) && !utf8.RuneStart(text[off]) {
		off--
	}
	for end > off && end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	// Up to contentSnippetContextRunes runes of context before the match.
	start := off
	for n := 0; n < contentSnippetContextRunes && start > 0; n++ {
		_, size := utf8.DecodeLastRune(text[:start])
		start -= size
	}
	// The window is contentSnippetMaxRunes runes starting at start.
	stop := start
	for n := 0; n < contentSnippetMaxRunes && stop < len(text); n++ {
		_, size := utf8.DecodeRune(text[stop:])
		stop += size
	}
	if stop < end {
		stop = end
	}
	window := []rune(contentSnippetSpace.Replace(string(text[start:stop])))
	if start > 0 {
		window = append([]rune("..."), window...)
	}
	if stop < len(text) {
		window = append(window, []rune("...")...)
	}
	return string(window)
}

// contentReader is a decoded `.gsx` ready to query. cache is the shared
// decompressed-block cache: one per reader, shared across a composite's
// shards, dropped with the reader. See contentBlockCache for lifetime and
// pin-safety notes.
type contentReader struct {
	idx   *contentIndex
	terms *contentTermDict
	grams *contentPostingIndex
	store *contentTextStore
	paths []string
	cache *contentBlockCache
}

// openContentReader decodes the queryable sections of idx.
func openContentReader(idx *contentIndex) (*contentReader, error) {
	if idx == nil {
		return nil, errors.New("nil content index")
	}
	if idx.shards != nil {
		// One cache for the whole composite: every shard reader shares
		// it, so a query touching many shards still warms once.
		r := &contentReader{idx: idx, cache: newContentBlockCache()}
		for _, sh := range idx.shards {
			if sh != nil && sh.reader != nil {
				sh.reader.cache = r.cache
				if sh.reader.store != nil {
					sh.reader.store.cache = r.cache
				}
			}
		}
		return r, nil
	}
	r := &contentReader{
		idx:   idx,
		paths: make([]string, len(idx.Docs)),
	}
	for i := range idx.Docs {
		r.paths[i] = ""
	}
	if sec := idx.Sections[contentSectionPaths]; len(sec) > 0 {
		paths, err := decodeContentPaths(sec, len(idx.Docs))
		if err != nil {
			return nil, err
		}
		r.paths = paths
	}
	if sec := idx.Sections[contentSectionTerms]; len(sec) > 0 {
		t, err := decodeContentTermDict(sec)
		if err != nil {
			return nil, err
		}
		r.terms = t
	}
	if sec := idx.Sections[contentSectionGrams]; len(sec) > 0 {
		g, err := decodeContentPostingIndex(sec)
		if err != nil {
			return nil, err
		}
		r.grams = g
	}
	if sec := idx.Sections[contentSectionText]; len(sec) > 0 {
		st, err := openContentTextStore(sec)
		if err != nil {
			return nil, err
		}
		r.store = st
	}
	r.cache = newContentBlockCache()
	if r.store != nil {
		r.store.cache = r.cache
	}
	return r, nil
}

// docText returns a fresh copy of the stored text of a document: the v7 text
// store is block-compressed, so the bytes are decompressed (through the
// block LRU) and copied out. A corrupt offset or undecodable block reads as
// absent, never as a wrong slice. Callers use the bytes transiently.
func (r *contentReader) docText(docID uint32) []byte {
	if r.idx.shards != nil {
		if sh, local, ok := r.shardOf(docID); ok {
			return sh.reader.docText(local)
		}
		return nil
	}
	if int(docID) >= len(r.idx.Docs) {
		return nil
	}
	d := r.idx.Docs[docID]
	if d.TextLen == 0 {
		return []byte{}
	}
	if r.store == nil {
		return nil
	}
	text, ok := r.store.textFor(d.TextOff, d.TextLen)
	if !ok {
		return nil
	}
	return text
}

// docPath returns the path for a docID.
func (r *contentReader) docPath(docID uint32) string {
	if r.idx.shards != nil {
		if sh, local, ok := r.shardOf(docID); ok {
			return sh.reader.docPath(local)
		}
		return ""
	}
	if int(docID) < len(r.paths) {
		return r.paths[docID]
	}
	return ""
}

// search returns documents whose text contains term (case-insensitive
// substring), ordered by docID, up to limit (limit <= 0 means unlimited).
func (r *contentReader) search(term string, limit int) []contentHit {
	t := strings.ToLower(term)
	if t == "" {
		return nil
	}
	if contentSearchWorkers() < 2 {
		return r.searchSerial(t, limit)
	}
	return r.searchParallel(t, limit, contentSearchWorkers())
}

// verifyDocContent tests one candidate document and builds its hit (nil when
// the document does not match). It is the single verification both the
// serial and parallel searches share, so their results are identical by
// construction: same needle, same fold, same offset mapping, same window.
// needle is the pre-folded query bytes, hoisted by the caller so a broad
// query does not re-allocate it per document; matching runs on bytes, never
// through a string conversion.
func (r *contentReader) verifyDocContent(id uint32, needle []byte) *contentHit {
	text := r.docText(id)
	if len(text) == 0 {
		return nil
	}
	// The stored text preserves case (PF-6a), so fold it for the
	// case-insensitive match; the window is still taken from the original.
	lower := contentFoldText(text)
	off := bytes.Index(lower, needle)
	if off < 0 {
		return nil
	}
	// Folding preserves the rune index but not the byte offset: a
	// byte-length-changing fold rune (İ 2->1, ẞ 3->2, KELVIN 3->1)
	// shifts every later offset. Map both window boundaries back to
	// the raw text in one pass, exactly as contentSnippet does, so the
	// snippet windows the real bytes and Offset points at the raw match
	// start.
	rawOff, rawEnd := contentFoldSpanToRaw(text, lower, off, off+len(needle))
	return &contentHit{
		Path:    r.docPath(id),
		DocID:   id,
		Offset:  rawOff,
		Snippet: contentSnippetWindow(text, rawOff, rawEnd-rawOff),
	}
}

// searchSerial is the historical streaming search: candidates verify one at
// a time in docID order, stopping at the limit.
func (r *contentReader) searchSerial(t string, limit int) []contentHit {
	if t == "" {
		return nil
	}
	needle := []byte(t)
	next := r.candidateStream(t)
	var hits []contentHit
	for {
		id, ok := next()
		if !ok {
			break
		}
		if hit := r.verifyDocContent(id, needle); hit != nil {
			hits = append(hits, *hit)
			if limit > 0 && len(hits) >= limit {
				break
			}
		}
	}
	return hits
}

// contentSearchMaxWorkers caps the parallel search fan-out: decode-bound
// verification scales with cores, and past a handful of workers the ordered
// merge and the shared block cache contend instead of helping.
var contentSearchMaxWorkers = 8

// contentSearchWorkers sizes the parallel search: GOMAXPROCS workers up to
// the cap (1 keeps the zero-overhead serial path). A var-free func so tests
// exercise both arms by pinning GOMAXPROCS.
func contentSearchWorkers() int {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	if n > contentSearchMaxWorkers {
		n = contentSearchMaxWorkers
	}
	return n
}

// contentSearchJob is one candidate for the parallel verifier: seq numbers
// the candidateStream order (ascending docIDs), so the merger restores the
// exact serial order regardless of which worker finishes first.
type contentSearchJob struct {
	seq uint64
	id  uint32
}

// contentSearchResult is one verified candidate: hit is nil for a miss, but
// every job reports (hit or miss) so the merger can advance past it.
type contentSearchResult struct {
	seq uint64
	hit *contentHit
}

// searchParallel verifies candidates across workers (bounded by
// contentSearchWorkers) and merges hits in candidate order with the exact
// serial limit. Verification is CPU-bound (block decompress + fold +
// substring), so workers scale near-linearly; the feeder, the merger and the
// limit accounting stay single-threaded. Over-verification past an early
// limit is bounded by the channel buffer plus one job per worker.
func (r *contentReader) searchParallel(t string, limit int, workers int) []contentHit {
	if t == "" {
		return nil
	}
	if workers < 2 {
		return r.searchSerial(t, limit)
	}
	needle := []byte(t)
	next := r.candidateStream(t)
	quit := make(chan struct{})
	var quitOnce sync.Once
	stop := func() { quitOnce.Do(func() { close(quit) }) }
	defer stop()
	work := make(chan contentSearchJob, 64)
	out := make(chan contentSearchResult, 64)
	var wg sync.WaitGroup
	// Feeder: the only goroutine that pulls the candidate stream.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(work)
		var seq uint64
		for {
			id, ok := next()
			if !ok {
				return
			}
			select {
			case work <- contentSearchJob{seq: seq, id: id}:
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
			for {
				select {
				case job, ok := <-work:
					if !ok {
						return
					}
					hit := r.verifyDocContent(job.id, needle)
					select {
					case out <- contentSearchResult{seq: job.seq, hit: hit}:
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
	// Merger (this goroutine): emit in stream order, stopping at exactly
	// the limit like the serial path (one emit batch can hold several
	// ready hits, so the limit is enforced inside emit, not after it).
	var hits []contentHit
	pending := make(map[uint64]*contentHit)
	var expect uint64
	emit := func() bool {
		for limit <= 0 || len(hits) < limit {
			hit, ok := pending[expect]
			if !ok {
				return true
			}
			delete(pending, expect)
			expect++
			if hit != nil {
				hits = append(hits, *hit)
			}
		}
		return false
	}
	for res := range out {
		pending[res.seq] = res.hit
		if !emit() {
			return hits
		}
	}
	emit()
	return hits
}

// candidates returns the docIDs that could contain term: trigram intersection
// when the term has at least three bytes, otherwise every document. The result
// is always a superset; verification decides. At most budget docIDs are
// materialized (budget <= 0 means all), so a broad list is not decoded in full.
func (r *contentReader) candidates(term string, budget int) []uint32 {
	return collectDocIDStream(r.candidateStream(term), budget)
}

// candidateStream returns a lazy, ascending stream of the docIDs that could
// contain term: the merge-intersection of its trigram lists, or every document
// when the term has no trigram constraint. Each trigram list is pulled block by
// block, so the lists are never held in memory at once.
func (r *contentReader) candidateStream(term string) func() (uint32, bool) {
	return r.candidateStreamIn(term, nil)
}

// candidateStreamIn is candidateStream limited to the shards mask selects
// (see shardMaskUnder); a nil mask, or an unsharded reader, means all docs.
func (r *contentReader) candidateStreamIn(term string, mask []bool) func() (uint32, bool) {
	if r.idx.shards != nil {
		return r.shardedCandidateStream(term, mask)
	}
	if len(term) < 3 || r.grams == nil {
		return r.allDocIDStream()
	}
	streams := make([]func() (uint32, bool), 0, len(term)-2)
	seen := make(map[string]struct{})
	for i := 0; i+3 <= len(term); i++ {
		gram := term[i : i+3]
		if _, ok := seen[gram]; ok {
			continue
		}
		seen[gram] = struct{}{}
		postings, found := r.grams.postings(gram)
		if !found {
			return contentEmptyDocIDStream
		}
		streams = append(streams, func() (uint32, bool) {
			docID, _, ok := postings()
			return docID, ok
		})
	}
	return intersectDocIDStreams(streams)
}

// allDocIDStream yields every document index in ascending order without
// materializing the slice.
func (r *contentReader) allDocIDStream() func() (uint32, bool) {
	next := uint32(0)
	total := uint32(len(r.idx.Docs))
	return func() (uint32, bool) {
		if next >= total {
			return 0, false
		}
		id := next
		next++
		return id, true
	}
}

// contentEmptyDocIDStream yields nothing.
func contentEmptyDocIDStream() (uint32, bool) { return 0, false }

// unionDocIDStreams lazily k-way unions ascending docID streams into one
// ascending stream of DISTINCT docIDs, holding one cursor per stream rather than
// any stream's full list. Every stream sitting on an emitted docID is advanced
// before the next value, so duplicates collapse. A consumer that stops at a
// budget (the candidate cap) stops pulling every source, so no alternative's
// postings are decoded in full.
func unionDocIDStreams(streams []func() (uint32, bool)) func() (uint32, bool) {
	if len(streams) == 0 {
		return contentEmptyDocIDStream
	}
	cur := make([]uint32, len(streams))
	live := make([]bool, len(streams))
	for i, s := range streams {
		cur[i], live[i] = s()
	}
	return func() (uint32, bool) {
		var minID uint32
		any := false
		for i := range streams {
			if live[i] && (!any || cur[i] < minID) {
				minID = cur[i]
				any = true
			}
		}
		if !any {
			return 0, false
		}
		for i := range streams {
			if live[i] && cur[i] == minID {
				cur[i], live[i] = streams[i]()
			}
		}
		return minID, true
	}
}

// intersectDocIDStreams lazily merge-intersects ascending docID streams, holding
// one cursor per stream rather than the posting lists themselves. A stream that
// has no common doc left stops the intersection.
func intersectDocIDStreams(streams []func() (uint32, bool)) func() (uint32, bool) {
	if len(streams) == 0 {
		return contentEmptyDocIDStream
	}
	cur := make([]uint32, len(streams))
	live := make([]bool, len(streams))
	for i, s := range streams {
		cur[i], live[i] = s()
	}
	return func() (uint32, bool) {
		for {
			var maxID uint32
			for i := range streams {
				if !live[i] {
					return 0, false
				}
				if cur[i] > maxID {
					maxID = cur[i]
				}
			}
			equal := true
			for i := range streams {
				if cur[i] != maxID {
					equal = false
					break
				}
			}
			if equal {
				for i := range streams {
					cur[i], live[i] = streams[i]()
				}
				return maxID, true
			}
			for i := range streams {
				for live[i] && cur[i] < maxID {
					cur[i], live[i] = streams[i]()
				}
			}
		}
	}
}

// collectDocIDStream materializes up to budget docIDs from an ascending stream
// (budget <= 0 means all). Callers that pass a budget never pull the rest of the
// stream, so its blocks are never decoded.
func collectDocIDStream(next func() (uint32, bool), budget int) []uint32 {
	var out []uint32
	for {
		id, ok := next()
		if !ok {
			return out
		}
		out = append(out, id)
		if budget > 0 && len(out) >= budget {
			return out
		}
	}
}

// contentDocCount is a convenience for telemetry and tests.
func (r *contentReader) contentDocCount() int { return len(r.idx.Docs) }

// sortedDocIDs is used by tests and the CLI to enumerate documents.
func (r *contentReader) sortedDocIDs() []uint32 {
	ids := make([]uint32, len(r.idx.Docs))
	for i := range ids {
		ids[i] = uint32(i)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
