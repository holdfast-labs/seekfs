package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// randomPostingSet builds nkeys keys with a varying number of ascending-docID
// postings and tf in 1..5, the shape the builder promises to match.
func randomPostingSet(seed int64, nkeys, maxPostings int) ([]string, map[string][]contentDocFreq) {
	rng := rand.New(rand.NewSource(seed))
	keys := make([]string, nkeys)
	postings := make(map[string][]contentDocFreq, nkeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%02d-%s", i, strings.Repeat("x", i%7))
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
			b.Add(k, p.docID, p.tf)
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
		if !reflect.DeepEqual(list, postings[k]) {
			t.Fatalf("lookup(%q) = %v, want %v", k, list, postings[k])
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

	want := make([]contentDocFreq, 0, n)
	doc := uint32(0)
	for i := 0; i < n; i++ {
		doc += uint32(1 + i%3)
		tf := uint32(1 + i%5)
		b.Add("alpha", doc, tf)
		want = append(want, contentDocFreq{docID: doc, tf: tf})
	}
	data, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := decodeContentPostingIndex(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.entries) != 1 || idx.entries[0].blockCount != 5 {
		t.Fatalf("blocks = %+v, want 5 for one key", idx.entries)
	}
	got, ok := idx.lookup("alpha")
	if !ok || len(got) != n {
		t.Fatalf("lookup = %d postings, %v; want %d", len(got), ok, n)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("decoded postings differ from input")
	}
}

func TestContentExternalEmpty(t *testing.T) {
	b, err := newContentExternalSectionBuilder(t.TempDir(), 64)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.Add("", 1, 1) // empty key is ignored, same as encode
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
				b.Add(k, p.docID, p.tf)
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
		key := fmt.Sprintf("term-%04d", k)
		doc := uint32(0)
		for j := 0; j < perKey; j++ {
			doc += uint32(1 + rng.Intn(4))
			b.Add(key, doc, uint32(1+rng.Intn(3)))
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
