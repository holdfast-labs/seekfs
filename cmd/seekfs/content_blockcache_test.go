package main

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func TestContentBlockCacheCap(t *testing.T) {
	// Default derives from TotalRAM; only the bounds are asserted here so
	// the test holds on any machine.
	got := contentBlockCacheCap()
	if got < 32<<20 || got > 256<<20 {
		t.Fatalf("default cap = %d; want within [32 MiB, 256 MiB]", got)
	}
	t.Setenv("SEEKFS_CONTENT_BLOCK_CACHE_MB", "1")
	if got := contentBlockCacheCap(); got != contentBlockCacheMinMB<<20 {
		t.Fatalf("tiny override cap = %d; want the %d MiB floor", got, contentBlockCacheMinMB)
	}
	t.Setenv("SEEKFS_CONTENT_BLOCK_CACHE_MB", "10000")
	if got := contentBlockCacheCap(); got != contentBlockCacheMaxMB<<20 {
		t.Fatalf("huge override cap = %d; want the %d MiB ceiling", got, contentBlockCacheMaxMB)
	}
	t.Setenv("SEEKFS_CONTENT_BLOCK_CACHE_MB", "64")
	if got := contentBlockCacheCap(); got != 64<<20 {
		t.Fatalf("64 MiB override cap = %d", got)
	}
	t.Setenv("SEEKFS_CONTENT_BLOCK_CACHE_MB", "bogus")
	if got := contentBlockCacheCap(); got < 32<<20 || got > 256<<20 {
		t.Fatalf("bogus override cap = %d; want the default range", got)
	}
}

func testBlockCacheTiny() *contentBlockCache {
	c := newContentBlockCache()
	c.capBytes = 3 << 10 // three 1 KiB blocks
	return c
}

func TestContentBlockCacheHitMissEvict(t *testing.T) {
	c := testBlockCacheTiny()
	st := &contentTextStore{}
	mk := func(b byte) []byte { return bytes.Repeat([]byte{b}, 1024) }
	dst := make([]byte, 1024)
	if _, ok := c.copyBlock(st, 0, 0, dst); ok {
		t.Fatal("empty cache hit")
	}
	// Fill three blocks; all hit afterwards.
	for i, b := range []byte{'a', 'b', 'c'} {
		if n := c.installAndCopy(st, uint64(i*1024), 0, mk(b), dst); n != 1024 || dst[0] != b {
			t.Fatalf("install block %d: n=%d first=%q", i, n, dst[0])
		}
	}
	for i, b := range []byte{'a', 'b', 'c'} {
		dst := make([]byte, 1024)
		if n, ok := c.copyBlock(st, uint64(i*1024), 0, dst); !ok || n != 1024 || dst[0] != b {
			t.Fatalf("block %d: n=%d ok=%v first=%q", i, n, ok, dst[0])
		}
	}
	// A fourth block evicts the LRU ('a'): 'b' was promoted by the read
	// above... 'a' is now the back. Insert 'd', then 'a' must miss while
	// 'b', 'c', 'd' hit.
	if n := c.installAndCopy(st, 3*1024, 0, mk('d'), dst); n != 1024 {
		t.Fatalf("install block 3: n=%d", n)
	}
	if _, ok := c.copyBlock(st, 0, 0, dst); ok {
		t.Fatal("evicted block still hits")
	}
	for i, want := range []byte{'b', 'c', 'd'} {
		if n, ok := c.copyBlock(st, uint64((i+1)*1024), 0, dst); !ok || n != 1024 || dst[0] != want {
			t.Fatalf("block %d: n=%d ok=%v first=%q; want survive %q", i+1, n, ok, dst[0], want)
		}
	}
	// Partial span copies respect the offset.
	if n, ok := c.copyBlock(st, 1024, 100, dst[:50]); !ok || n != 50 || dst[0] != 'b' {
		t.Fatalf("partial copy: n=%d ok=%v", n, ok)
	}
	if n, ok := c.copyBlock(st, 1024, 2000, dst); ok || n != 0 {
		t.Fatalf("past-end copy: n=%d ok=%v; want miss", n, ok)
	}
}

// The key includes the store: two stores sharing one cache never see each
// other's blocks, so a composite's shards stay isolated by construction.
func TestContentBlockCacheIsolatedByStore(t *testing.T) {
	c := testBlockCacheTiny()
	a, b := &contentTextStore{}, &contentTextStore{}
	dst := make([]byte, 8)
	c.installAndCopy(a, 0, 0, bytes.Repeat([]byte{'x'}, 1024), dst)
	if _, ok := c.copyBlock(b, 0, 0, dst); ok {
		t.Fatal("store B hit store A's block")
	}
	if n, ok := c.copyBlock(a, 0, 0, dst); !ok || n != 8 || dst[0] != 'x' {
		t.Fatalf("store A: n=%d ok=%v", n, ok)
	}
}

func TestContentBlockCacheDrop(t *testing.T) {
	c := testBlockCacheTiny()
	st := &contentTextStore{}
	dst := make([]byte, 1024)
	c.installAndCopy(st, 0, 0, bytes.Repeat([]byte{'q'}, 1024), dst)
	if c.usedBytes != 1024 {
		t.Fatalf("used = %d; want 1024", c.usedBytes)
	}
	c.drop()
	if c.usedBytes != 0 {
		t.Fatalf("used after drop = %d", c.usedBytes)
	}
	if _, ok := c.copyBlock(st, 0, 0, dst); ok {
		t.Fatal("dropped cache hits")
	}
}

// Concurrent fetchers must never observe a torn copy: every byte comes from
// one stable block generation.
func TestContentBlockCacheConcurrent(t *testing.T) {
	c := newContentBlockCache()
	st := &contentTextStore{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			dst := make([]byte, 1024)
			for i := 0; i < 50; i++ {
				start := uint64((w*50 + i) * 1024)
				data := bytes.Repeat([]byte{byte('a' + w)}, 1024)
				c.installAndCopy(st, start, 0, data, dst)
				got := make([]byte, 1024)
				if n, ok := c.copyBlock(st, start, 0, got); ok && n == 1024 {
					for _, b := range got {
						if b != byte('a'+w) && !clearedForReuse(b, w) {
							// Any byte is either this worker's fill or a
							// recycled buffer's stale content only if the
							// copy raced an evict+reuse — which the lock
							// discipline forbids. Fail loudly.
							t.Errorf("torn copy: worker %d saw %q", w, b)
							return
						}
					}
				}
			}
		}(w)
	}
	wg.Wait()
}

// clearedForReuse is a placeholder that always fails the torn-copy check:
// with copy-under-lock there is no benign torn state, so any foreign byte
// is a bug. (Kept as a named predicate for readability.)
func clearedForReuse(b byte, w int) bool { return false }

func TestContentBlockCacheStats(t *testing.T) {
	c := testBlockCacheTiny()
	st := &contentTextStore{}
	dst := make([]byte, 1024)
	if got := c.stats(); got != (contentBlockCacheStats{}) {
		t.Fatalf("fresh stats = %+v; want zero", got)
	}
	if _, ok := c.copyBlock(st, 0, 0, dst); ok {
		t.Fatal("empty cache hit")
	}
	// A miss (copyBlock) leaves the counters alone: only decodes count.
	if got := c.stats(); got.Hits != 0 || got.Misses != 0 || got.DecodedBytes != 0 {
		t.Fatalf("after miss stats = %+v; want zero", got)
	}
	c.installAndCopy(st, 0, 0, bytes.Repeat([]byte{'q'}, 1024), dst)
	c.installAndCopy(st, 1024, 0, bytes.Repeat([]byte{'w'}, 2048), dst[:0])
	if _, ok := c.copyBlock(st, 0, 0, dst); !ok {
		t.Fatal("installed block misses")
	}
	if _, ok := c.copyBlock(st, 0, 0, dst); !ok {
		t.Fatal("installed block misses on repeat")
	}
	got := c.stats()
	if got.Hits != 2 || got.Misses != 2 || got.DecodedBytes != 1024+2048 {
		t.Fatalf("stats = %+v; want hits=2 misses=2 decoded=3072", got)
	}
	c.resetStats()
	if got := c.stats(); got != (contentBlockCacheStats{}) {
		t.Fatalf("after reset stats = %+v; want zero", got)
	}
	// A duplicate install still decoded, so it still counts as a miss.
	c.installAndCopy(st, 0, 0, bytes.Repeat([]byte{'z'}, 1024), dst)
	if got := c.stats(); got.Misses != 1 || got.DecodedBytes != 1024 {
		t.Fatalf("after dup install stats = %+v; want misses=1 decoded=1024", got)
	}
	var nilCache *contentBlockCache
	if got := nilCache.stats(); got != (contentBlockCacheStats{}) {
		t.Fatalf("nil stats = %+v; want zero", got)
	}
	nilCache.resetStats() // must not panic
}

func TestContentBlockCacheEndToEnd(t *testing.T) {
	// A reader built over real sections serves identical bytes through the
	// shared cache on repeat reads (second read is a cache hit).
	docs := make([]contentBuildDoc, 0, 40)
	for i := 0; i < 40; i++ {
		docs = append(docs, contentBuildDoc{
			path: fmt.Sprintf("f%02d.txt", i),
			frn:  uint64(1000 + i),
			text: []byte(fmt.Sprintf("needle doc %d lorem ipsum dolor sit amet", i)),
		})
	}
	idx, err := assembleContentIndex(docs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if r.cache == nil || r.store == nil || r.store.cache != r.cache {
		t.Fatal("reader must own a shared cache attached to its store")
	}
	first := r.search("needle", 0)
	if len(first) != 40 {
		t.Fatalf("hits = %d; want 40", len(first))
	}
	// All 40 small docs share few blocks; the second search must not grow
	// cache usage (every block already resident).
	before := r.cache.usedBytes
	second := r.search("needle", 0)
	if len(second) != len(first) {
		t.Fatal("repeat search differs")
	}
	if r.cache.usedBytes != before {
		t.Fatalf("cache grew on repeat: %d -> %d", before, r.cache.usedBytes)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("hit %d differs between searches", i)
		}
	}
}
