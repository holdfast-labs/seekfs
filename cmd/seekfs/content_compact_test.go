package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Blocked text store round-trip: docs of wildly varying sizes (including
// multi-block, empty and block-boundary-straddling docs) come back byte
// identical, with logical offsets matching the doc-table convention.
func TestContentTextBlockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w, err := newContentTextBlockWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	var docs [][]byte
	var offs []uint64
	docs = append(docs, []byte("hello world"))
	docs = append(docs, []byte{})
	docs = append(docs, bytes.Repeat([]byte("abcdefgh"), 30000))           // ~240 KiB, spans blocks
	docs = append(docs, bytes.Repeat([]byte("x"), contentTextBlockTarget)) // exactly one block
	for i := 0; i < 500; i++ {
		docs = append(docs, []byte(fmt.Sprintf("small doc %d with some text to compress %s", i, "lorem ipsum ")))
	}
	docs = append(docs, []byte("Ünïcödé ✓ text with multibyte runes ✓✓✓"))
	for _, d := range docs {
		offs = append(offs, w.logicalSize())
		if err := w.Append(d); err != nil {
			t.Fatal(err)
		}
	}
	secPath := filepath.Join(dir, "cxtext.sec")
	if err := w.FinishFile(secPath); err != nil {
		t.Fatal(err)
	}
	sec, err := os.ReadFile(secPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := openContentTextStore(sec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := st.logicalSize(); got != offs[len(offs)-1]+uint64(len(docs[len(docs)-1])) {
		t.Fatalf("logical size = %d", got)
	}
	for i, d := range docs {
		got, ok := st.textFor(offs[i], uint32(len(d)))
		if !ok {
			t.Fatalf("doc %d: not ok", i)
		}
		if !bytes.Equal(got, d) {
			t.Fatalf("doc %d differs (%d vs %d bytes)", i, len(got), len(d))
		}
	}
	// A span crossing a block boundary stitches.
	if len(st.blocks) < 2 {
		t.Fatalf("want multiple blocks, got %d", len(st.blocks))
	}
	b0 := st.blocks[0]
	mid := b0.logicalStart + uint64(b0.rawLen) - 3
	got, ok := st.textFor(mid, 10)
	if !ok || len(got) != 10 {
		t.Fatalf("cross-block span = %d bytes, %v", len(got), ok)
	}
	// Out-of-range spans fail closed.
	if _, ok := st.textFor(st.logicalSize(), 1); ok {
		t.Fatal("read past end must fail")
	}
	if _, ok := st.textFor(st.logicalSize()+100, 5); ok {
		t.Fatal("read fully past end must fail")
	}
	// Truncations fail closed, never panic.
	for _, cut := range []int{0, 3, 8, len(sec) / 2, len(sec) - 1} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("truncation at %d panicked: %v", cut, r)
				}
			}()
			if st, err := openContentTextStore(sec[:cut]); err == nil {
				_, _ = st.textFor(0, 1)
			}
		}()
	}
}

func TestContentTextBlockEmpty(t *testing.T) {
	dir := t.TempDir()
	w, err := newContentTextBlockWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	secPath := filepath.Join(dir, "cxtext.sec")
	if err := w.FinishFile(secPath); err != nil {
		t.Fatal(err)
	}
	sec, err := os.ReadFile(secPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := openContentTextStore(sec)
	if err != nil {
		t.Fatalf("open empty: %v", err)
	}
	if st.logicalSize() != 0 {
		t.Fatalf("empty logical size = %d", st.logicalSize())
	}
	if _, ok := st.textFor(0, 1); ok {
		t.Fatal("empty store must not serve text")
	}
}

// The decoded text must be a copy: mutating it (as verifiers do when
// folding) must not corrupt the block cache for the next reader.
func TestContentTextBlockCopySemantics(t *testing.T) {
	dir := t.TempDir()
	w, err := newContentTextBlockWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("mutable mutable mutable")
	if err := w.Append(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(body); err != nil {
		t.Fatal(err)
	}
	secPath := filepath.Join(dir, "cxtext.sec")
	if err := w.FinishFile(secPath); err != nil {
		t.Fatal(err)
	}
	sec, err := os.ReadFile(secPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := openContentTextStore(sec)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := st.textFor(0, uint32(len(body)))
	if !ok {
		t.Fatal("not ok")
	}
	for i := range a {
		a[i] = 'Z'
	}
	b, ok := st.textFor(uint64(len(body)), uint32(len(body)))
	if !ok || !bytes.Equal(b, body) {
		t.Fatalf("cache corruption after caller mutation: %q", b)
	}
}

// Terms dictionary round-trip: keys, frequencies, ordering, lookup,
// range scans and corruption rejection.
func TestContentTermDictRoundTrip(t *testing.T) {
	terms := []string{"apple", "application", "apply", "banana", "band", "bandana", "zebra", "ßpecial", "a"}
	sort.Strings(terms)
	df := map[string]uint32{
		"apple": 5, "application": 2, "apply": 9, "banana": 1,
		"band": 7, "bandana": 3, "zebra": 4, "ßpecial": 6, "a": 8,
	}
	data := encodeContentTermDict(terms, df)
	d, err := decodeContentTermDict(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.keyCount() != len(terms) {
		t.Fatalf("keyCount = %d; want %d", d.keyCount(), len(terms))
	}
	for _, k := range terms {
		got, ok := d.count(k)
		if !ok || got != int(df[k]) {
			t.Fatalf("count(%q) = %d, %v; want %d", k, got, ok, df[k])
		}
	}
	for _, miss := range []string{"", "appl", "apples", "bandanas", "zzzz", "A"} {
		if _, ok := d.count(miss); ok {
			t.Fatalf("count(%q) unexpectedly found", miss)
		}
	}
	// Range scan visits every key exactly once, in order, with the right df.
	var seen []string
	d.scanRange(0, d.keyCount(), func(key []byte, freq int) bool {
		seen = append(seen, string(key))
		if freq != int(df[string(key)]) {
			t.Fatalf("scan df(%q) = %d; want %d", key, freq, df[string(key)])
		}
		return true
	})
	if !sort.StringsAreSorted(seen) || len(seen) != len(terms) {
		t.Fatalf("scan visited %d keys, sorted=%v", len(seen), sort.StringsAreSorted(seen))
	}
	for i, k := range terms {
		if seen[i] != k {
			t.Fatalf("scan[%d] = %q; want %q", i, seen[i], k)
		}
	}
	// Sub-range scan.
	seen = seen[:0]
	d.scanRange(2, 5, func(key []byte, freq int) bool {
		seen = append(seen, string(key))
		return true
	})
	if len(seen) != 3 || seen[0] != terms[2] || seen[2] != terms[4] {
		t.Fatalf("sub-range scan = %q", seen)
	}
	// Truncations fail closed, never panic.
	for _, cut := range []int{0, 7, 16, len(data) / 2, len(data) - 1} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("truncation at %d panicked: %v", cut, r)
				}
			}()
			_, _ = decodeContentTermDict(data[:cut])
		}()
	}
	// A bit flip in the blob fails decode. The second key's record starts
	// right after the first ("a" encodes as three bytes: shared=0,
	// suffixLen=1, 'a'); maxing its shared-prefix byte exceeds the
	// predecessor length and fails deterministically. (A flip confined to
	// the df tail is undetectable at decode by design — document
	// frequencies carry no checksum, and section-level CRC32C is the
	// integrity mechanism.)
	blobStart := 16
	rc := int(binary.LittleEndian.Uint32(data[8:]))
	off := 16
	for r := 0; r < rc; r++ {
		kl := int(binary.LittleEndian.Uint32(data[off+4:]))
		off += 8 + kl
	}
	blobStart = off
	bad := append([]byte(nil), data...)
	bad[blobStart+3] = 0x7f
	if _, err := decodeContentTermDict(bad); err == nil {
		t.Fatal("flipped blob decoded without error")
	}
}

func TestContentTermDictEmpty(t *testing.T) {
	if data := encodeContentTermDict(nil, nil); data != nil {
		t.Fatalf("empty dict encoded to %d bytes", len(data))
	}
}

// The external dict builder must produce exactly the dictionary the
// in-memory encoder would: same keys, same document frequencies.
func TestContentExternalDictEquivalence(t *testing.T) {
	// docTerms[doc] = its distinct terms; df counts docs per term.
	docTerms := [][]string{
		{"apple", "banana", "cherry"},
		{"apple", "date"},
		{"banana", "apple", "elderberry"},
		{"fig"},
	}
	wantDF := map[string]uint32{}
	for _, terms := range docTerms {
		for _, term := range terms {
			wantDF[term]++
		}
	}
	var wantKeys []string
	for k := range wantDF {
		wantKeys = append(wantKeys, k)
	}
	sort.Strings(wantKeys)
	want := encodeContentTermDict(wantKeys, wantDF)

	b, err := newContentExternalDictBuilder(t.TempDir(), 64) // tiny arena forces spills
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, terms := range docTerms {
		for _, term := range terms {
			b.Add(term)
		}
	}
	path := filepath.Join(t.TempDir(), "cxterm.sec")
	if err := b.FinishFile(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("dict builder differs from encoder: got %d bytes, want %d", len(got), len(want))
	}
	d, err := decodeContentTermDict(got)
	if err != nil {
		t.Fatal(err)
	}
	for k, wantN := range wantDF {
		if gotN, ok := d.count(k); !ok || gotN != int(wantN) {
			t.Fatalf("count(%q) = %d, %v; want %d", k, gotN, ok, wantN)
		}
	}
}

func TestContentExternalDictEmpty(t *testing.T) {
	b, err := newContentExternalDictBuilder(t.TempDir(), 64)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.Add("")
	path := filepath.Join(t.TempDir(), "cxterm.sec")
	if err := b.FinishFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("empty dict builder must create no file")
	}
}

// A large dictionary (past several restart intervals, odd key lengths,
// shared prefixes) round-trips through front-coding exactly.
func TestContentTermDictScale(t *testing.T) {
	var keys []string
	df := map[string]uint32{}
	for i := 0; i < 5000; i++ {
		k := fmt.Sprintf("prefix-%04d-suffix-%d", i%500, i)
		keys = append(keys, k)
		df[k] = uint32(i%97 + 1)
	}
	sort.Strings(keys)
	data := encodeContentTermDict(keys, df)
	t.Logf("5000-key dict = %d bytes (%.1f B/key)", len(data), float64(len(data))/5000)
	d, err := decodeContentTermDict(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if got, ok := d.count(k); !ok || got != int(df[k]) {
			t.Fatalf("count(%q) = %d, %v", k, got, ok)
		}
	}
	n := 0
	d.scanRange(0, d.keyCount(), func(key []byte, freq int) bool {
		n++
		return true
	})
	if n != len(keys) {
		t.Fatalf("scan visited %d keys; want %d", n, len(keys))
	}
}
