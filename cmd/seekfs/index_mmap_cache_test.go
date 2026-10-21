package main

import (
	"slices"
	"testing"
)

// A posting block must never be served from a cache entry that belongs to a
// different section. The cache key used to be (data pointer, length, block);
// after an mmap is unmapped (or a heap section is freed) the same address and
// length can describe a new section, and the stale block then answers the
// query. TestShortTermRankedLaneFuzz hit exactly this: ok=true with zero rows
// because the driver grammar's block came from a previous iteration.
func TestPostingBlockCacheRejectsReusedSectionAddress(t *testing.T) {
	servicePostingBlockCache = postingBlockLRU{}
	t.Cleanup(func() { servicePostingBlockCache = postingBlockLRU{} })

	first := encodeStringPostingSection(map[string][]uint32{"key": {1, 2, 3}}, nil)
	second := encodeStringPostingSection(map[string][]uint32{"key": {4, 5, 6}}, nil)
	if len(first) != len(second) {
		t.Fatalf("fixture lengths differ: %d != %d", len(first), len(second))
	}

	// One backing array represents one mapped address. Decoding it caches block
	// 0 under the section identity.
	data := make([]byte, len(first))
	copy(data, first)
	oldSection := decodePostingSection(data)
	if got := oldSection.stringPosting("key"); !slices.Equal(got, []uint32{1, 2, 3}) {
		t.Fatalf("first section posting = %v, want [1 2 3]", got)
	}

	// Reuse the same address for a different section of the same length.
	copy(data, second)
	newSection := decodePostingSection(data)
	if got := newSection.stringPosting("key"); !slices.Equal(got, []uint32{4, 5, 6}) {
		t.Fatalf("reused-address posting = %v, want [4 5 6] (stale cache hit)", got)
	}
}
