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
	// not read past the block table. Two postings keep the entry multi-block
	// (a single posting is a singleton entry with no block range to corrupt).
	data := encodeContentPostingSection(map[string][]contentDocFreq{"alp": {{docID: 1, tf: 1}, {docID: 9, tf: 1}}})
	idx, err := decodeContentPostingIndex(data)
	if err != nil {
		t.Fatal(err)
	}
	if idx.entries[0].single {
		t.Fatal("two-posting key must not be a singleton entry")
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

// v9 stores a single-document gram inline: the entry carries the docID and
// owns no blocks, and the streaming iterator serves it without touching the
// block tables.
func TestContentPostingSingletonEntry(t *testing.T) {
	postings := map[string][]contentDocFreq{
		"one": {{docID: 42, tf: 1}},
		"two": {{docID: 7, tf: 1}, {docID: 9, tf: 1}},
	}
	data := encodeContentPostingSection(postings)
	idx, err := decodeContentPostingIndex(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(idx.entries) != 2 {
		t.Fatalf("entries = %d; want 2", len(idx.entries))
	}
	if !idx.entries[0].single || idx.entries[0].singleDoc != 42 {
		t.Fatalf("one = %+v; want singleton doc 42", idx.entries[0])
	}
	if idx.entries[1].single {
		t.Fatal("two-posting key must not be a singleton")
	}
	if len(idx.blocks) != 1 {
		t.Fatalf("blocks = %d; want 1 (singleton owns none)", len(idx.blocks))
	}
	// The rebuilt firstBlock prefix sum must skip the singleton.
	if idx.entries[1].firstBlock != 0 || idx.entries[1].count != 2 {
		t.Fatalf("two = %+v; want firstBlock 0 count 2", idx.entries[1])
	}
	got, ok := idx.lookup("one")
	if !ok || len(got) != 1 || got[0].docID != 42 {
		t.Fatalf("one = %+v, %v", got, ok)
	}
	got, ok = idx.lookup("two")
	if !ok || len(got) != 2 || got[0].docID != 7 || got[1].docID != 9 {
		t.Fatalf("two = %+v, %v", got, ok)
	}
	// The singleton iterator stops after one posting.
	seen := 0
	if !idx.forEach("one", func(docID, tf uint32) bool {
		seen++
		return false
	}) {
		t.Fatal("forEach did not report the singleton as found")
	}
	if seen != 1 {
		t.Fatalf("singleton forEach ran %d times; want 1", seen)
	}
}

// Grams past the prune threshold are dropped from the section: dense keys
// cost more than they select. The in-memory encoder filters them like the
// streaming assembler does.
func TestContentPostingPrunesDenseKeys(t *testing.T) {
	old := contentGramPruneDF
	contentGramPruneDF = 3
	defer func() { contentGramPruneDF = old }()
	postings := map[string][]contentDocFreq{
		"aaa": {{docID: 1}, {docID: 2}, {docID: 3}, {docID: 4}, {docID: 5}},
		"bbb": {{docID: 1}, {docID: 2}, {docID: 3}},
		"ccc": {{docID: 7}},
	}
	idx, err := decodeContentPostingIndex(encodeContentPostingSection(postings))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := idx.lookup("aaa"); ok {
		t.Fatal("dense key must be pruned from the section")
	}
	if got, ok := idx.lookup("bbb"); !ok || len(got) != 3 {
		t.Fatalf("bbb = %+v, %v; want 3 postings", got, ok)
	}
	if got, ok := idx.lookup("ccc"); !ok || len(got) != 1 || got[0].docID != 7 {
		t.Fatalf("ccc = %+v, %v; want singleton 7", got, ok)
	}
	if len(idx.entries) != 2 {
		t.Fatalf("entries = %d; want 2 (bbb multi, ccc singleton)", len(idx.entries))
	}
}

// The CXGP pruned-gram set round-trips; corrupt framings fail the load.
func TestContentPrunedGramsCodec(t *testing.T) {
	set, err := decodeContentPrunedGrams(encodeContentPrunedGrams([]string{"the", "and", "aaa"}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, g := range []string{"the", "and", "aaa"} {
		if _, ok := set[contentGramU32Of(g)]; !ok {
			t.Fatalf("pruned set misses %q", g)
		}
	}
	if _, ok := set[contentGramU32Of("zzz")]; ok {
		t.Fatal("pruned set must not contain zzz")
	}
	if encodeContentPrunedGrams(nil) != nil {
		t.Fatal("empty prune set must encode as nil (section omitted)")
	}
	for _, bad := range [][]byte{{'a', 'b'}, {'a', 'b', 'c', 'd'}, {'b', 'b', 'b', 'a', 'a', 'a'}} {
		if _, err := decodeContentPrunedGrams(bad); err == nil {
			t.Fatalf("decoder accepted corrupt pruned set %q", bad)
		}
	}
}

// v9 drops the stored firstBlock/count/byteOffset fields; the decoder
// rebuilds them as prefix sums and rejects any section whose stored values
// or lengths disagree.
func TestContentPostingV9PrefixSumsValidated(t *testing.T) {
	mk := func() []byte {
		return encodeContentPostingSection(map[string][]contentDocFreq{
			"aaa": {{docID: 1}, {docID: 3}},
			"bbb": {{docID: 5}},
			"ccc": {{docID: 7}, {docID: 8}, {docID: 9}},
		})
	}
	mutate := func(t *testing.T, fn func(bad []byte)) {
		t.Helper()
		bad := append([]byte(nil), mk()...)
		fn(bad)
		if _, err := decodeContentPostingIndex(bad); err == nil {
			t.Fatal("decoder accepted a corrupt section")
		}
	}
	// Bad entry flags.
	mutate(t, func(bad []byte) { bad[16+3] = 7 })
	// Singleton reserved word nonzero.
	mutate(t, func(bad []byte) {
		// entries: aaa multi (12B), bbb singleton (12B): reserved at 16+12+8.
		bad[16+12+8] = 1
	})
	// Multi entry with zero blocks.
	mutate(t, func(bad []byte) {
		for i := 0; i < 4; i++ {
			bad[16+8+i] = 0
		}
	})
	// Truncated section still rejected.
	full := mk()
	if _, err := decodeContentPostingIndex(full[:len(full)-1]); err == nil {
		t.Fatal("decoder accepted a truncated section")
	}
	// A clean section rebuilds the dropped fields exactly.
	idx, err := decodeContentPostingIndex(full)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(idx.blocks) != 2 {
		t.Fatalf("blocks = %d; want 2 (aaa and ccc)", len(idx.blocks))
	}
	if idx.entries[2].firstBlock != 1 || idx.entries[0].count != 2 || idx.entries[2].count != 3 {
		t.Fatalf("rebuilt fields wrong: %+v", idx.entries)
	}
	if idx.blocks[0].byteOffset != 0 || idx.blocks[1].byteOffset != idx.blocks[0].byteLen {
		t.Fatalf("rebuilt offsets wrong: %+v", idx.blocks)
	}
}
