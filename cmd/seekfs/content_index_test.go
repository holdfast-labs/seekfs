package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

func contentTestDocs() []contentDoc {
	mk := func(id uint32, frn uint64, mod int64) contentDoc {
		var h [contentHashLen]byte
		for i := range h {
			h[i] = byte(frn + uint64(i))
		}
		return contentDoc{
			DocID:            id,
			FRN:              frn,
			ContentType:      1,
			ExtractorVersion: 2,
			ContentHash:      h,
			RawSize:          int64(1000 + frn),
			DocLen:           id + 7,
			ModUnix:          mod,
			TextOff:          uint64(id) * 128,
			TextLen:          uint32(120 + id),
		}
	}
	docs := []contentDoc{
		mk(0, 101, 1_700_000_000),
		mk(1, 205, 1_700_000_100),
		mk(2, 999, 1_700_000_200),
	}
	return docs
}

func TestContentIndexRoundTrip(t *testing.T) {
	idx := newContentIndex()
	idx.BuiltAt = time.Unix(0, 1_700_000_000_000_000_000)
	idx.Docs = contentTestDocs()
	idx.Sections[contentSectionGrams] = []byte("gram-payload")
	idx.Sections[contentSectionText] = []byte("some stored text")

	data := contentIndexEncode(idx)
	got, err := contentIndexDecode(data)
	if err != nil {
		t.Fatalf("contentDecode: %v", err)
	}
	if len(got.Docs) != len(idx.Docs) {
		t.Fatalf("docs = %d, want %d", len(got.Docs), len(idx.Docs))
	}
	for i := range idx.Docs {
		if got.Docs[i] != idx.Docs[i] {
			t.Fatalf("doc %d = %+v, want %+v", i, got.Docs[i], idx.Docs[i])
		}
	}
	if !bytes.Equal(got.Sections[contentSectionGrams], []byte("gram-payload")) {
		t.Fatalf("grams section = %q", got.Sections[contentSectionGrams])
	}
	if !bytes.Equal(got.Sections[contentSectionText], []byte("some stored text")) {
		t.Fatalf("text section = %q", got.Sections[contentSectionText])
	}
	if !got.BuiltAt.Equal(idx.BuiltAt) {
		t.Fatalf("BuiltAt = %v, want %v", got.BuiltAt, idx.BuiltAt)
	}
}

func TestContentIndexEncodingIsDeterministic(t *testing.T) {
	mk := func() *contentIndex {
		idx := newContentIndex()
		idx.BuiltAt = time.Unix(0, 5)
		idx.Docs = contentTestDocs()
		idx.Sections[contentSectionGrams] = []byte("g")
		idx.Sections[contentSectionText] = []byte("t")
		idx.Sections[contentSectionTerms] = []byte("term")
		return idx
	}
	a := contentIndexEncode(mk())
	b := contentIndexEncode(mk())
	if !bytes.Equal(a, b) {
		t.Fatal("encoding the same index twice must be byte-identical")
	}
}

func TestContentLookupFRN(t *testing.T) {
	docs := contentTestDocs()
	contentDocSortByFRN(docs)
	if i, ok := contentLookupFRN(docs, 205); !ok || docs[i].DocID != 1 {
		t.Fatalf("lookup 205 = %d, %v", i, ok)
	}
	if _, ok := contentLookupFRN(docs, 206); ok {
		t.Fatal("lookup of a missing FRN must fail")
	}
}

func TestContentIndexFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seekfs_c.gsx")
	idx := newContentIndex()
	idx.Docs = contentTestDocs()
	idx.Sections[contentSectionText] = []byte("payload")
	if err := contentSaveFile(path, idx); err != nil {
		t.Fatalf("contentSaveFile: %v", err)
	}
	got, err := contentLoadFile(path)
	if err != nil {
		t.Fatalf("contentLoadFile: %v", err)
	}
	if len(got.Docs) != len(idx.Docs) || !bytes.Equal(got.Sections[contentSectionText], []byte("payload")) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestContentIndexRejectsBadMagic(t *testing.T) {
	if _, err := contentIndexDecode([]byte("not-a-gsx-file-at-all")); err == nil {
		t.Fatal("expected an error for bad magic")
	}
}
