package main

import (
	"bytes"
	"encoding/binary"
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
	idx.Sections[contentSectionFilter] = []byte("filter-payload")

	data := contentIndexEncode(idx)
	got, err := contentIndexDecode(data)
	if err != nil {
		t.Fatalf("contentDecode: %v", err)
	}
	if len(got.Docs) != len(idx.Docs) {
		t.Fatalf("docs = %d, want %d", len(got.Docs), len(idx.Docs))
	}
	for i := range idx.Docs {
		// v9 drops RawSize/DocLen/ModUnix from the wire and v10 drops
		// TextOff with the text store (no query path reads them); they
		// decode as zero while every other field round-trips.
		want := idx.Docs[i]
		want.RawSize, want.DocLen, want.ModUnix = 0, 0, 0
		if got.Docs[i] != want {
			t.Fatalf("doc %d = %+v, want %+v", i, got.Docs[i], want)
		}
	}
	if !bytes.Equal(got.Sections[contentSectionGrams], []byte("gram-payload")) {
		t.Fatalf("grams section = %q", got.Sections[contentSectionGrams])
	}
	if !bytes.Equal(got.Sections[contentSectionFilter], []byte("filter-payload")) {
		t.Fatalf("filter section = %q", got.Sections[contentSectionFilter])
	}
	if !got.BuiltAt.Equal(idx.BuiltAt) {
		t.Fatalf("BuiltAt = %v, want %v", got.BuiltAt, idx.BuiltAt)
	}
}

// A sidecar stamped with the previous doc-table version must fail the
// version gate (never attach stale): the service sees no usable base and
// its PF-3 build path rebuilds it. The current section decoders never see
// older bytes.
func TestContentIndexRejectsPreviousVersion(t *testing.T) {
	idx := newContentIndex()
	idx.Docs = contentTestDocs()
	data := contentIndexEncode(idx)
	if len(data) < contentHeaderSize() {
		t.Fatal("encoded index shorter than its header")
	}
	bad := append([]byte(nil), data...)
	binary.LittleEndian.PutUint32(bad[8:], uint32(contentIndexVersion-1))
	if _, err := contentIndexDecode(bad); err == nil {
		t.Fatal("previous-version sidecar decoded without error")
	}
	// The manifest version still decodes (manifests carry no doc table).
	if contentManifestVersion == contentIndexVersion {
		t.Fatal("manifest and index versions must differ for this test")
	}
}

func TestContentIndexPolicyRoundTrip(t *testing.T) {
	idx := newContentIndex()
	idx.Docs = contentTestDocs()
	idx.Policy = contentBuildPolicy{MaxRaw: 1 << 20, MaxText: 1 << 19, Skipped: 3, Truncated: 2, ScopeDropped: 1, ScopeDroppedBytes: 42, BudgetBytes: 1 << 20, Binary: 7, ReserveBytes: 20 << 30}
	got, err := contentIndexDecode(contentIndexEncode(idx))
	if err != nil {
		t.Fatalf("contentDecode: %v", err)
	}
	if got.Policy != idx.Policy {
		t.Fatalf("policy = %+v; want %+v", got.Policy, idx.Policy)
	}
}

func TestContentIndexPolicyDecodesOlderSizes(t *testing.T) {
	// A 40-byte CXPL (pre dropped-bytes/budget fields) decodes with the new
	// fields zero; a 32-byte one (pre scope-dropped) likewise; a 56-byte one
	// (pre binary) likewise.
	old := contentBuildPolicy{MaxRaw: 8, MaxText: 4, Skipped: 2, Truncated: 1, ScopeDropped: 1}
	b := make([]byte, 40)
	binary.LittleEndian.PutUint64(b[0:], uint64(old.MaxRaw))
	binary.LittleEndian.PutUint64(b[8:], uint64(old.MaxText))
	binary.LittleEndian.PutUint64(b[16:], uint64(old.Skipped))
	binary.LittleEndian.PutUint64(b[24:], uint64(old.Truncated))
	binary.LittleEndian.PutUint64(b[32:], uint64(old.ScopeDropped))
	if got, ok := decodeContentPolicy(b); !ok || got != old {
		t.Fatalf("40-byte policy = %+v, %v; want %+v", got, ok, old)
	}
	if got, ok := decodeContentPolicy(b[:32]); !ok || got.ScopeDropped != 0 || got.ScopeDroppedBytes != 0 || got.BudgetBytes != 0 {
		t.Fatalf("32-byte policy = %+v, %v; want zero drop fields", got, ok)
	}
	full := contentBuildPolicy{MaxRaw: 8, MaxText: 4, Skipped: 2, Truncated: 1, ScopeDropped: 1, ScopeDroppedBytes: 44, BudgetBytes: 100}
	b56 := full.encode()[:56]
	if got, ok := decodeContentPolicy(b56); !ok || got != full || got.Binary != 0 {
		t.Fatalf("56-byte policy = %+v, %v; want %+v with zero binary", got, ok, full)
	}
	// A 64-byte CXPL (pre reserve) decodes with the reserve zero.
	b64 := full.encode()[:64]
	if got, ok := decodeContentPolicy(b64); !ok || got.Binary != full.Binary || got.ReserveBytes != 0 {
		t.Fatalf("64-byte policy = %+v, %v; want binary kept and zero reserve", got, ok)
	}
}

func TestContentIndexScopeIdentityRoundTrip(t *testing.T) {
	idx := newContentIndex()
	idx.ScopeHash = defaultContentScope().resolve("C:", []string{`C:\repo`}).fingerprint()
	got, err := contentIndexDecode(contentIndexEncode(idx))
	if err != nil {
		t.Fatal(err)
	}
	if got.ScopeHash != idx.ScopeHash {
		t.Fatal("scope identity did not survive encoding")
	}
	idx.ScopeHash = [32]byte{}
	idx.Sections[contentSectionScope] = []byte("bad")
	if _, err := contentIndexDecode(contentIndexEncode(idx)); err == nil {
		t.Fatal("malformed scope identity accepted")
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
	releaseContentIndexOnCleanup(t, got)
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
