package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

// randomPostingSet builds nkeys 3-byte gram keys with a varying number of
// ascending-docID postings, the shape the builder promises to match. tf
// varies in the input but v7 stores docIDs only, so comparisons use docIDs.
func randomPostingSet(seed int64, nkeys, maxPostings int) ([]string, map[string][]contentDocFreq) {
	rng := rand.New(rand.NewSource(seed))
	keys := make([]string, nkeys)
	postings := make(map[string][]contentDocFreq, nkeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("%c%c%c", 'a'+byte(i/(26*26)), 'a'+byte((i/26)%26), 'a'+byte(i%26))
		n := rng.Intn(maxPostings + 1)
		list := make([]contentDocFreq, 0, n)
		doc := uint32(0)
		for j := 0; j < n; j++ {
			doc += uint32(1 + rng.Intn(5))
			list = append(list, contentDocFreq{docID: doc, tf: uint32(1 + rng.Intn(5))})
		}
		postings[keys[i]] = list
	}
	return keys, postings
}

func postingDocIDs(list []contentDocFreq) []uint32 {
	out := make([]uint32, len(list))
	for i, p := range list {
		out[i] = p.docID
	}
	return out
}

func TestContentExternalEquivalence(t *testing.T) {
	keys, postings := randomPostingSet(1, 40, 9000)
	want := encodeContentPostingSection(postings)

	b, err := newContentExternalSectionBuilder(t.TempDir(), 4096)
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	defer b.Close()
	for _, k := range keys {
		for _, p := range postings[k] {
			b.Add(k, p.docID)
		}
	}
	got, err := b.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("builder output differs from encodeContentPostingSection: got %d bytes, want %d", len(got), len(want))
	}

	idx, err := decodeContentPostingIndex(got)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range keys {
		list, ok := idx.lookup(k)
		if len(postings[k]) == 0 {
			if ok {
				t.Fatalf("lookup(%q) = %d postings, want none", k, len(list))
			}
			continue
		}
		if !ok {
			t.Fatalf("lookup(%q) missing", k)
		}
		wantIDs, gotIDs := postingDocIDs(postings[k]), postingDocIDs(list)
		if !bytes.Equal(uint32SliceBytes(wantIDs), uint32SliceBytes(gotIDs)) {
			t.Fatalf("lookup(%q) docIDs differ", k)
		}
	}
}

func TestContentExternalMultiBlock(t *testing.T) {
	const n = 5000
	b, err := newContentExternalSectionBuilder(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	want := make([]uint32, 0, n)
	doc := uint32(0)
	for i := 0; i < n; i++ {
		doc += uint32(1 + i%3)
		b.Add("alp", doc)
		want = append(want, doc)
	}
	data, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := decodeContentPostingIndex(data)
	if err != nil {
		t.Fatal(err)
	}
	if idx.entryCount() != 1 || idx.entryAt(0).blockCount != 2 {
		t.Fatalf("blocks = %d, want 2 for one 5000-posting key", idx.entryAt(0).blockCount)
	}
	got, ok := idx.lookup("alp")
	if !ok || len(got) != n {
		t.Fatalf("lookup = %d postings, %v; want %d", len(got), ok, n)
	}
	if !bytes.Equal(uint32SliceBytes(want), uint32SliceBytes(postingDocIDs(got))) {
		t.Fatal("decoded docIDs differ from input")
	}
}

// With pruning active the streaming assembler drops the same dense keys
// the in-memory encoder drops, byte-identically, and reports exactly those
// keys as pruned.
func TestContentExternalPruneParity(t *testing.T) {
	old := contentGramPruneDF
	contentGramPruneDF = 100
	defer func() { contentGramPruneDF = old }()
	keys, postings := randomPostingSet(7, 60, 300)
	want := encodeContentPostingSection(postings)
	var wantPruned []string
	for _, k := range keys {
		if len(postings[k]) > contentGramPruneDF {
			wantPruned = append(wantPruned, k)
		}
	}
	sort.Strings(wantPruned)

	build := func(arena int) ([]byte, []string) {
		b, err := newContentExternalSectionBuilder(t.TempDir(), arena)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		for _, k := range keys {
			for _, p := range postings[k] {
				b.Add(k, p.docID)
			}
		}
		out, err := b.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return out, b.PrunedKeys()
	}
	first, firstPruned := build(2048)
	second, secondPruned := build(65536)
	if !bytes.Equal(first, second) {
		t.Fatal("pruned builds with different arenas produced different bytes")
	}
	if !bytes.Equal(first, want) {
		t.Fatal("pruned streaming build differs from in-memory encode")
	}
	if !equalStrings(firstPruned, wantPruned) || !equalStrings(secondPruned, wantPruned) {
		t.Fatalf("pruned keys = %q/%q; want %q", firstPruned, secondPruned, wantPruned)
	}
	if len(wantPruned) == 0 {
		t.Fatal("prune test corpus pruned nothing; want dense keys")
	}
	// The pruned set encodes and constrains the reader: dropped keys are
	// reported pruned, kept keys decode.
	sec := encodeContentPrunedGrams(firstPruned)
	set, err := decodeContentPrunedGrams(sec)
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != len(wantPruned) {
		t.Fatalf("pruned set size = %d; want %d", len(set), len(wantPruned))
	}
	idx, err := decodeContentPostingIndex(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range wantPruned {
		if _, ok := idx.lookup(k); ok {
			t.Fatalf("pruned key %q still indexed", k)
		}
		if _, ok := set[contentGramU32Of(k)]; !ok {
			t.Fatalf("pruned key %q missing from set", k)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestContentExternalEmpty(t *testing.T) {
	b, err := newContentExternalSectionBuilder(t.TempDir(), 64)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.Add("", 1) // empty key is ignored, same as encode
	b.Add("toolongkey", 2)
	if got, err := b.Finish(); err != nil || got != nil {
		t.Fatalf("Finish() = %v, %v; want nil, nil", got, err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestContentExternalDeterminism(t *testing.T) {
	keys, postings := randomPostingSet(99, 25, 3000)
	build := func(arena int) []byte {
		b, err := newContentExternalSectionBuilder(t.TempDir(), arena)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		for _, k := range keys {
			for _, p := range postings[k] {
				b.Add(k, p.docID)
			}
		}
		out, err := b.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := build(4096)
	second := build(8192)
	if !bytes.Equal(first, second) {
		t.Fatal("same set built with different arenas produced different bytes")
	}
	if !bytes.Equal(first, encodeContentPostingSection(postings)) {
		t.Fatal("determinism build differs from encodeContentPostingSection")
	}
}

func TestContentExternalBoundedBuffer(t *testing.T) {
	const (
		total  = 200000
		nkeys  = 200
		arena  = 64 << 10
		perKey = total / nkeys
	)
	dir := t.TempDir()
	b, err := newContentExternalSectionBuilder(dir, arena)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	rng := rand.New(rand.NewSource(7))
	for k := 0; k < nkeys; k++ {
		key := fmt.Sprintf("%c%c%c", 'a'+byte(k/(26*26)), 'a'+byte((k/26)%26), 'a'+byte(k%26))
		doc := uint32(0)
		for j := 0; j < perKey; j++ {
			doc += uint32(1 + rng.Intn(4))
			b.Add(key, doc)
		}
	}

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	entries, _ := os.ReadDir(dir)

	stop := make(chan struct{})
	var peak, peakObjs uint64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(100 * time.Microsecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak {
					peak = m.HeapAlloc
					peakObjs = m.HeapObjects
				}
			}
		}
	}()

	out, err := b.Finish()
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("runs=%d peakObjs=%d", len(entries), peakObjs)

	extra := int64(0)
	if peak > base.HeapAlloc {
		extra = int64(peak - base.HeapAlloc)
	}
	naive := int64(total) * 8 // contentDocFreq per posting, before map/slice/key overhead
	t.Logf("arena=%d output=%d baseHeap=%d peakHeap=%d extra=%d naivePostingBytes=%d", arena, len(out), base.HeapAlloc, peak, extra, naive)
	if extra >= naive {
		t.Fatalf("peak transient heap %d not below naive posting size %d", extra, naive)
	}
	if extra > int64(len(out))+int64(arena)+(2<<20) {
		t.Fatalf("peak transient heap %d exceeds output %d + arena %d + 2MiB", extra, len(out), arena)
	}
}
