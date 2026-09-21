package main

// Read side of the offline content index: dictionary lookup, trigram candidate
// generation, and substring verification against the stored text.

import (
	"errors"
	"sort"
	"strings"
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

// contentReader is a decoded `.gsx` ready to query.
type contentReader struct {
	idx   *contentIndex
	terms *contentPostingIndex
	grams *contentPostingIndex
	text  []byte
	paths []string
}

// openContentReader decodes the queryable sections of idx.
func openContentReader(idx *contentIndex) (*contentReader, error) {
	if idx == nil {
		return nil, errors.New("nil content index")
	}
	r := &contentReader{
		idx:   idx,
		text:  idx.Sections[contentSectionText],
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
		t, err := decodeContentPostingIndex(sec)
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
	return r, nil
}

// docText returns the stored text of a document.
func (r *contentReader) docText(docID uint32) []byte {
	if int(docID) >= len(r.idx.Docs) {
		return nil
	}
	d := r.idx.Docs[docID]
	end := d.TextOff + uint64(d.TextLen)
	if end > uint64(len(r.text)) {
		return nil
	}
	return r.text[d.TextOff:end]
}

// docPath returns the path for a docID.
func (r *contentReader) docPath(docID uint32) string {
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
	candidates := r.candidates(t, 0)
	var hits []contentHit
	for _, id := range candidates {
		text := r.docText(id)
		if len(text) == 0 {
			continue
		}
		// The stored text preserves case (PF-6a), so fold it for the
		// case-insensitive match; the window is still taken from the original.
		lower := contentFoldText(text)
		if off := strings.Index(string(lower), t); off >= 0 {
			// Folding preserves the rune index but not the byte offset: a
			// byte-length-changing fold rune (İ 2->1, ẞ 3->2, KELVIN 3->1)
			// shifts every later offset. Map both window boundaries back to
			// the raw text, exactly as contentSnippet does, so the snippet
			// windows the real bytes and Offset points at the raw match start.
			rawOff := contentFoldOffsetToRaw(text, lower, off)
			rawEnd := contentFoldOffsetToRaw(text, lower, off+len(t))
			hits = append(hits, contentHit{
				Path:    r.docPath(id),
				DocID:   id,
				Offset:  rawOff,
				Snippet: contentSnippetWindow(text, rawOff, rawEnd-rawOff),
			})
			if limit > 0 && len(hits) >= limit {
				break
			}
		}
	}
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
	if len(term) < 3 || r.grams == nil {
		return r.allDocIDStream()
	}
	streams := make([]func() (uint32, bool), 0, len(term)-2)
	for i := 0; i+3 <= len(term); i++ {
		postings, found := r.grams.postings(term[i : i+3])
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

// sliceDocIDStream adapts a materialized ascending docID slice to a stream.
func sliceDocIDStream(ids []uint32) func() (uint32, bool) {
	i := 0
	return func() (uint32, bool) {
		if i >= len(ids) {
			return 0, false
		}
		id := ids[i]
		i++
		return id, true
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
