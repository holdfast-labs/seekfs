package main

import "testing"

func TestContentPostingCodecRoundTrip(t *testing.T) {
	postings := map[string][]contentDocFreq{
		"alpha": {{docID: 1, tf: 3}, {docID: 5, tf: 1}},
		"beta":  {{docID: 2, tf: 7}},
	}
	data := encodeContentPostingSection(postings)
	idx, err := decodeContentPostingIndex(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := idx.lookup("alpha")
	if !ok || len(got) != 2 || got[0].docID != 1 || got[0].tf != 3 || got[1].docID != 5 || got[1].tf != 1 {
		t.Fatalf("alpha = %+v, %v", got, ok)
	}
	got, ok = idx.lookup("beta")
	if !ok || len(got) != 1 || got[0].docID != 2 || got[0].tf != 7 {
		t.Fatalf("beta = %+v, %v", got, ok)
	}
	if _, ok := idx.lookup("missing"); ok {
		t.Fatal("lookup of a missing key must fail")
	}
}

func TestContentPostingCodecRejectsCorruptData(t *testing.T) {
	data := encodeContentPostingSection(map[string][]contentDocFreq{"alpha": {{docID: 1, tf: 1}}})
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
				_, _ = idx.lookup("alpha")
			}
		}()
	}
}

func TestContentPostingLookupRejectsOutOfRangeEntry(t *testing.T) {
	// A dictionary entry whose block range is out of bounds must be refused,
	// not read past the block table.
	data := encodeContentPostingSection(map[string][]contentDocFreq{"alpha": {{docID: 1, tf: 1}}})
	idx, err := decodeContentPostingIndex(data)
	if err != nil {
		t.Fatal(err)
	}
	idx.entries[0].firstBlock = 9999
	idx.entries[0].blockCount = 10
	if _, ok := idx.lookup("alpha"); ok {
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
		if got[i].docID != uint32(i*2) || got[i].tf != uint32(i%5+1) {
			t.Fatalf("posting %d = %+v", i, got[i])
		}
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
	idx, err := decodeContentPostingIndex(encodeContentPostingSection(map[string][]contentDocFreq{"broad": list}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	decoded := 0
	contentPostingDecodeHook = func() { decoded++ }
	defer func() { contentPostingDecodeHook = nil }()

	seen := 0
	if !idx.forEach("broad", func(docID, tf uint32) bool {
		if docID != uint32(seen) || tf != 1 {
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
	if got, ok := idx.lookup("broad"); !ok || len(got) != n {
		t.Fatalf("lookup = %d, %v; want %d", len(got), ok, n)
	}
}
