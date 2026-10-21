package main

import (
	"testing"
)

func resetPostingBlockCache(t *testing.T) {
	t.Helper()
	servicePostingBlockCache = postingBlockLRU{}
	t.Cleanup(func() { servicePostingBlockCache = postingBlockLRU{} })
}

func TestPostingBlockLRUClear(t *testing.T) {
	resetPostingBlockCache(t)
	key := postingBlockCacheKey{block: 7}
	ids := []uint32{1, 2, 3, 4}
	servicePostingBlockCache.add(key, ids)
	if got, ok := servicePostingBlockCache.get(key); !ok || len(got) != len(ids) {
		t.Fatalf("get after add = %v, %v; want hit with %d ids", got, ok, len(ids))
	}
	if n := servicePostingBlockCache.liveBytes(); n <= 0 {
		t.Fatalf("liveBytes = %d; want > 0", n)
	}
	servicePostingBlockCache.clear()
	if got, ok := servicePostingBlockCache.get(key); ok || got != nil {
		t.Fatalf("get after clear = %v, %v; want miss", got, ok)
	}
	if n := servicePostingBlockCache.liveBytes(); n != 0 {
		t.Fatalf("liveBytes after clear = %d; want 0", n)
	}
}

// TestIdleReleaseDropsCaches pins the footprint contract: the idle path
// drops every query-result cache (per-volume posting lists, path cache,
// global decoded blocks) while the mmapped base keeps serving. Caches are
// rebuilt on demand, so results are unchanged — only re-decode cost.
func TestIdleReleaseDropsCaches(t *testing.T) {
	resetPostingBlockCache(t)
	vol := buildOverlayLimitFixture(t, 5)

	vol.termMu.Lock()
	vol.termCache = map[string]postingCacheEntry{"needle": {ids: []int{1, 2, 3}, gen: 1}}
	vol.pathTermCache = map[string]postingCacheEntry{"x": {ids: []int{4}, gen: 1}}
	vol.termMu.Unlock()
	vol.pathCache[42] = `F:\needle-00.txt`
	servicePostingBlockCache.add(postingBlockCacheKey{block: 1}, []uint32{9, 9, 9})

	if info := vol.residentMemoryInfo(); info.CacheBytes <= 0 {
		t.Fatalf("CacheBytes = %d before release; want > 0", info.CacheBytes)
	}

	svc := &goSearchService{volumes: []*serviceVolumeIndex{vol}}
	serviceReleaseIdleCaches(svc)

	vol.termMu.Lock()
	termNil := vol.termCache == nil
	pathTermNil := vol.pathTermCache == nil
	vol.termMu.Unlock()
	if !termNil || !pathTermNil {
		t.Fatalf("posting caches survive idle release: term=%v pathterm=%v", !termNil, !pathTermNil)
	}
	if len(vol.pathCache) != 0 {
		t.Fatalf("pathCache len = %d after release; want 0", len(vol.pathCache))
	}
	if got, ok := servicePostingBlockCache.get(postingBlockCacheKey{block: 1}); ok || got != nil {
		t.Fatalf("block cache survives idle release")
	}
	if info := vol.residentMemoryInfo(); info.CacheBytes != 0 {
		t.Fatalf("CacheBytes = %d after release; want 0", info.CacheBytes)
	}
	if snap := runtimeMemorySnapshot(); snap.BlockCacheBytes != 0 {
		t.Fatalf("BlockCacheBytes = %d after release; want 0", snap.BlockCacheBytes)
	}

	// The base still serves after the release: same results, re-decoded.
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true, Limit: 5}, false)
	if err != nil {
		t.Fatalf("search after idle release: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("search after idle release returned %d; want 5", len(got))
	}

	// Nil-service release only clears the global block cache (no panic).
	servicePostingBlockCache.add(postingBlockCacheKey{block: 2}, []uint32{1})
	serviceReleaseIdleCaches(nil)
	if _, ok := servicePostingBlockCache.get(postingBlockCacheKey{block: 2}); ok {
		t.Fatalf("nil-service release left block cache populated")
	}
}
