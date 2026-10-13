package main

import (
	"encoding/binary"
	"testing"
)

// v7 stores docIDs only: tf in the input is ignored and reads back as 0.
// Keys are fixed 3-byte grams held inline in the entry.
func TestContentPostingCodecRoundTrip(t *testing.T) {
	postings := map[string][]contentDocFreq{
		"alp": {{docID: 1, tf: 3}, {docID: 5, tf: 1}},
		"bet": {{docID: 2, tf: 7}},
	}
	data := encodeContentPostingSection(postings)
	idx, err := decodeContentPostingIndex(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := idx.lookup("alp")
	if !ok || len(got) != 2 || got[0].docID != 1 || got[1].docID != 5 {
		t.Fatalf("alp = %+v, %v", got, ok)
	}
	if got[0].tf != 0 || got[1].tf != 0 {
		t.Fatalf("v7 stores no tf, got %+v", got)
	}
	got, ok = idx.lookup("bet")
	if !ok || len(got) != 1 || got[0].docID != 2 {
		t.Fatalf("bet = %+v, %v", got, ok)
	}
	if _, ok := idx.lookup("missing"); ok {
		t.Fatal("lookup of a missing key must fail")
	}
	if _, ok := idx.lookup("toolong"); ok {
		t.Fatal("lookup of a non-gram key must fail")
	}
	// Non-3-byte keys are not encoded.
	if len(idx.entries) != 2 {
		t.Fatalf("entries = %d; want 2 (only 3-byte keys encode)", len(idx.entries))
	}
}

func TestContentPostingCodecRejectsCorruptData(t *testing.T) {
	data := encodeContentPostingSection(map[string][]contentDocFreq{"alp": {{docID: 1, tf: 1}}})
	for _, cut := range []int{0, 5, 16, len(data) - 1} {
		if cut < 0 || cut > len(data) {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("truncated data at %d panicked: %v", cut, r)
				}
			}()
			idx, err := decodeContentPostingIndex(data[:cut])
			if err == nil && idx != nil {
				_, _ = idx.lookup("alp")
			}
		}()
	}
}

func TestContentPostingCodecRejectsKeyBlob(t *testing.T) {
	// A v6-style section (nonzero keyBlobLen) must fail: the version gate
	// should already have refused the file, and the decoder must not read
	// v6 bytes as v7 inline keys.
	data := encodeContentPostingSection(map[string][]contentDocFreq{"alp": {{docID: 1}}})
	bad := append([]byte(nil), data...)
	binary.LittleEndian.PutUint32(bad[4:], 3)
	if _, err := decodeContentPostingIndex(bad); err == nil {
		t.Fatal("decoder accepted a section with a key blob")
	}
}

func TestContentPostingLookupRejectsOutOfRangeEntry(t *testing.T) {
	// A dictionary entry whose block range is out of bounds must be refused,
	// not read past the block table.
	data := encodeContentPostingSection(map[string][]contentDocFreq{"alp": {{docID: 1, tf: 1}}})
	idx, err := decodeContentPostingIndex(data)
	if err != nil {
		t.Fatal(err)
	}
	idx.entries[0].firstBlock = 9999
	idx.entries[0].blockCount = 10
	if _, ok := idx.lookup("alp"); ok {
		t.Fatal("lookup accepted an out-of-range block entry")
	}
}

func TestContentPostingCodecMultipleBlocks(t *testing.T) {
	const n = contentPostingBlockSize*2 + 7
	list := make([]contentDocFreq, n)
	for i := range list {
		list[i] = contentDocFreq{docID: uint32(i * 2), tf: uint32(i%5 + 1)}
	}
	idx, err := decodeContentPostingIndex(encodeContentPostingSection(map[string][]contentDocFreq{"big": list}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := idx.lookup("big")
	if !ok || len(got) != n {
		t.Fatalf("got %d postings, %v; want %d", len(got), ok, n)
	}
	for i := range got {
		if got[i].docID != uint32(i*2) {
			t.Fatalf("posting %d = %+v", i, got[i])
		}
	}
}

// A dense full block must encode as a bitmap and decode identically: 4096
// consecutive docIDs fit in ~520 bytes instead of ~4 KiB of deltas.
func TestContentPostingDenseBlockBitmap(t *testing.T) {
	const n = contentPostingBlockSize
	list := make([]contentDocFreq, n)
	for i := range list {
		list[i] = contentDocFreq{docID: uint32(i), tf: 1}
	}
	data := encodeContentPostingSection(map[string][]contentDocFreq{"den": list})
	idx, err := decodeContentPostingIndex(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(idx.blocks) != 1 {
		t.Fatalf("blocks = %d; want 1", len(idx.blocks))
	}
	payload := idx.blockBlob[idx.blocks[0].byteOffset : idx.blocks[0].byteOffset+idx.blocks[0].byteLen]
	if payload[0] != contentPostingBlockBitmap {
		t.Fatalf("dense block kind = %d; want bitmap (%d)", payload[0], contentPostingBlockBitmap)
	}
	if len(payload) > 600 {
		t.Fatalf("bitmap block = %d bytes; want ~520", len(payload))
	}
	got, ok := idx.lookup("den")
	if !ok || len(got) != n {
		t.Fatalf("got %d postings, %v; want %d", len(got), ok, n)
	}
	for i := range got {
		if got[i].docID != uint32(i) {
			t.Fatalf("posting %d = %+v", i, got[i])
		}
	}
}

// A sparse full block (huge span) must stay a delta list: a bitmap would be
// larger than the list.
func TestContentPostingSparseBlockStaysList(t *testing.T) {
	const n = contentPostingBlockSize
	list := make([]contentDocFreq, n)
	for i := range list {
		list[i] = contentDocFreq{docID: uint32(i * 1000), tf: 1}
	}
	data := encodeContentPostingSection(map[string][]contentDocFreq{"spr": list})
	idx, err := decodeContentPostingIndex(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	payload := idx.blockBlob[idx.blocks[0].byteOffset : idx.blocks[0].byteOffset+idx.blocks[0].byteLen]
	if payload[0] != contentPostingBlockList {
		t.Fatal("sparse block must stay a delta list")
	}
	got, ok := idx.lookup("spr")
	if !ok || len(got) != n || got[n-1].docID != uint32((n-1)*1000) {
		t.Fatalf("got %d postings, %v", len(got), ok)
	}
}

// A broad key must be paged, not decoded in full: forEach stops once the
// callback says so, and the decode hook sees only the postings actually visited.
func TestContentPostingForEachStopsEarly(t *testing.T) {
	const n = contentPostingBlockSize*3 + 11
	list := make([]contentDocFreq, n)
	for i := range list {
		list[i] = contentDocFreq{docID: uint32(i), tf: 1}
	}
	idx, err := decodeContentPostingIndex(encodeContentPostingSection(map[string][]contentDocFreq{"brd": list}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	decoded := 0
	contentPostingDecodeHook = func() { decoded++ }
	defer func() { contentPostingDecodeHook = nil }()

	seen := 0
	if !idx.forEach("brd", func(docID, tf uint32) bool {
		if docID != uint32(seen) || tf != 0 {
			t.Fatalf("posting %d = (%d, %d)", seen, docID, tf)
		}
		seen++
		return seen < 5
	}) {
		t.Fatal("forEach did not report the key as found")
	}
	if seen != 5 {
		t.Fatalf("callback ran %d times; want 5", seen)
	}
	if decoded != 5 {
		t.Fatalf("decoded %d postings; want 5 (decode did not stop early)", decoded)
	}

	// lookup still materializes the whole list, proving the key is much longer.
	contentPostingDecodeHook = nil
	if got, ok := idx.lookup("brd"); !ok || len(got) != n {
		t.Fatalf("lookup = %d, %v; want %d", len(got), ok, n)
	}
}
