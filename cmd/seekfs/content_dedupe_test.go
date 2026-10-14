package main

// Tests for content-hash dedupe (§6z): identical files stored once, every
// path listed with correct snippets and counts.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// dedupeTestCorpus builds an in-memory index over docs with duplicates:
// a.txt and sub/b.txt share content, c.txt is unique, empty.txt is empty
// (declined, never a doc).
func dedupeTestCorpus(t *testing.T) *contentIndex {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shared := "the shared needle body\nsecond line\n"
	write("a.txt", shared)
	write("sub/b.txt", shared)
	write("c.txt", "a unique needle document\n")
	write("empty.txt", "")
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestContentDedupeStoresSharedTextOnce(t *testing.T) {
	idx := dedupeTestCorpus(t)
	if len(idx.Docs) != 3 {
		t.Fatalf("docs = %d; want 3 (a, b, c; empty declined)", len(idx.Docs))
	}
	if len(idx.Aliases) != 1 {
		t.Fatalf("aliases = %v; want exactly one pair", idx.Aliases)
	}
	alias, primary := idx.Aliases[0].Alias, idx.Aliases[0].Primary
	if alias == primary {
		t.Fatal("alias names itself")
	}
	a, p := idx.Docs[alias], idx.Docs[primary]
	if a.TextOff != p.TextOff || a.TextLen != p.TextLen {
		t.Fatalf("alias text %d+%d != primary %d+%d", a.TextOff, a.TextLen, p.TextOff, p.TextLen)
	}
	if a.FRN == p.FRN {
		t.Fatal("alias must keep its own FRN (path-hash key)")
	}
	if idx.Inclusion.DedupeFiles != 1 {
		t.Fatalf("dedupe files = %d; want 1", idx.Inclusion.DedupeFiles)
	}
	if idx.Inclusion.DedupeBytes != int64(a.TextLen) {
		t.Fatalf("dedupe bytes = %d; want %d", idx.Inclusion.DedupeBytes, a.TextLen)
	}
}

func TestContentDedupeListsEveryPath(t *testing.T) {
	idx := dedupeTestCorpus(t)
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	hits := r.search("shared needle", 0)
	var paths []string
	for _, h := range hits {
		paths = append(paths, h.Path)
		if h.Snippet == "" {
			t.Errorf("hit %q has no snippet", h.Path)
		}
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"a.txt", "sub/b.txt"}) {
		t.Fatalf("hits = %v; want both duplicate paths", paths)
	}
	// Candidates include the alias even though it owns no postings.
	cands := r.candidates("shared", 0)
	if len(cands) != 2 {
		t.Fatalf("candidates = %v; want primary + alias", cands)
	}
	// The unique document still resolves alone.
	if hits := r.search("unique needle", 0); len(hits) != 1 {
		t.Fatalf("unique hits = %v; want 1", contentPathsOf(hits))
	}
}

func TestContentDedupeRoundTrip(t *testing.T) {
	src := dedupeTestCorpus(t)
	data := contentIndexEncode(src)
	back, err := contentIndexDecode(data)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Release()
	if len(back.Aliases) != 1 || back.Aliases[0] != src.Aliases[0] {
		t.Fatalf("round-trip aliases = %v; want %v", back.Aliases, src.Aliases)
	}
	if back.Inclusion.DedupeFiles != 1 || back.Inclusion.DedupeBytes != src.Inclusion.DedupeBytes {
		t.Fatalf("round-trip inclusion = %+v", back.Inclusion)
	}
	r, err := openContentReader(back)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("shared needle", 0); len(hits) != 2 {
		t.Fatalf("reloaded hits = %d; want 2", len(hits))
	}
}

func TestContentDedupeRejectsCorruptAliasSection(t *testing.T) {
	src := dedupeTestCorpus(t)
	_ = contentIndexEncode(src)
	// A chained alias (primary that is itself an alias) fails the load.
	chained := []contentAliasPair{{Alias: 1, Primary: 0}, {Alias: 2, Primary: 1}}
	if _, err := decodeContentAliases(encodeContentAliases(chained), 3); err == nil {
		t.Fatal("chained aliases decoded without error")
	}
	// Out-of-range, self-alias, unsorted and trailing bytes all fail.
	for name, pairs := range map[string][]contentAliasPair{
		"out of range": {{Alias: 9, Primary: 0}},
		"self":         {{Alias: 1, Primary: 1}},
	} {
		if _, err := decodeContentAliases(encodeContentAliases(pairs), 3); err == nil {
			t.Errorf("%s aliases decoded without error", name)
		}
	}
	unsorted := []byte{2, 2, 0, 1, 0}
	if _, err := decodeContentAliases(unsorted, 3); err == nil {
		t.Fatal("unsorted aliases decoded without error")
	}
	trailing := append(encodeContentAliases([]contentAliasPair{{Alias: 1, Primary: 0}}), 0x00)
	if _, err := decodeContentAliases(trailing, 3); err == nil {
		t.Fatal("trailing bytes decoded without error")
	}
	// Empty section is no aliases.
	if pairs, err := decodeContentAliases(nil, 3); err != nil || len(pairs) != 0 {
		t.Fatalf("empty section = %v,%v; want no aliases", pairs, err)
	}
}

func TestContentDedupeEmptyNeverAliases(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"e1.txt", "e2.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Aliases) != 0 {
		t.Fatalf("aliases = %v; empty files decline, never alias", idx.Aliases)
	}
}

func TestContentInclusionStatsRoundTrip(t *testing.T) {
	in := contentInclusionStats{
		Skipped:          map[string]int64{"lockfile": 3, "cache": 40},
		SkippedBytes:     map[string]int64{"lockfile": 300, "cache": 4000},
		Truncated:        2,
		TruncatedBytes:   200,
		DedupeFiles:      5,
		DedupeBytes:      5000,
		WorktreesSkipped: 2,
	}
	back, err := decodeContentInclusionStats(encodeContentInclusionStats(in))
	if err != nil {
		t.Fatal(err)
	}
	if back.Truncated != 2 || back.TruncatedBytes != 200 || back.DedupeFiles != 5 ||
		back.DedupeBytes != 5000 || back.WorktreesSkipped != 2 ||
		back.Skipped["lockfile"] != 3 || back.SkippedBytes["cache"] != 4000 {
		t.Fatalf("round-trip = %+v; want %+v", back, in)
	}
	if _, err := decodeContentInclusionStats([]byte{1}); err == nil {
		t.Fatal("truncated section decoded without error")
	}
}
