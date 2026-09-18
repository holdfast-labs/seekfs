package main

// Read side of the offline content index: dictionary lookup, trigram candidate
// generation, and substring verification against the stored text.

import (
	"errors"
	"sort"
	"strings"
)

// contentHit is one matching document.
type contentHit struct {
	Path   string
	DocID  uint32
	Offset int
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
	candidates := r.candidates(t)
	var hits []contentHit
	for _, id := range candidates {
		text := r.docText(id)
		if len(text) == 0 {
			continue
		}
		lower := text
		if !isLowerASCII(lower) {
			lower = []byte(strings.ToLower(string(text)))
		}
		if off := strings.Index(string(lower), t); off >= 0 {
			hits = append(hits, contentHit{Path: r.docPath(id), DocID: id, Offset: off})
			if limit > 0 && len(hits) >= limit {
				break
			}
		}
	}
	return hits
}

// candidates returns the docIDs that could contain term: trigram intersection
// when the term has at least three bytes, otherwise every document. The result
// is always a superset; verification decides.
func (r *contentReader) candidates(term string) []uint32 {
	if len(term) >= 3 && r.grams != nil {
		var current []uint32
		first := true
		for i := 0; i+3 <= len(term); i++ {
			postings, ok := r.grams.lookup(term[i : i+3])
			if !ok || len(postings) == 0 {
				return nil
			}
			ids := make([]uint32, len(postings))
			for j := range postings {
				ids[j] = postings[j].docID
			}
			if first {
				current = ids
				first = false
			} else {
				current = intersectSortedDocIDs(current, ids)
			}
			if len(current) == 0 {
				return nil
			}
		}
		return current
	}
	all := make([]uint32, len(r.idx.Docs))
	for i := range all {
		all[i] = uint32(i)
	}
	return all
}

func intersectSortedDocIDs(a, b []uint32) []uint32 {
	out := a[:0:0]
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

func isLowerASCII(b []byte) bool {
	for _, c := range b {
		if c >= 'A' && c <= 'Z' {
			return false
		}
	}
	return true
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


