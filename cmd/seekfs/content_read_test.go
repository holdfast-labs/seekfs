package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func buildTestContentIndex(t *testing.T, files map[string]string) *contentIndex {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return idx
}

func contentPathsOf(hits []contentHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Path)
	}
	sort.Strings(out)
	return out
}

func TestContentBuildAndSearch(t *testing.T) {
	idx := buildTestContentIndex(t, map[string]string{
		"a.txt": "alpha needle beta",
		"b.txt": "nothing here",
		"c.md":  "Needle in markdown",
		"d.bin": "binary\x00needle",
	})
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := r.contentDocCount(); got != 3 {
		t.Fatalf("document count = %d; want 3 (binary skipped)", got)
	}

	hits := r.search("needle", 0)
	got := contentPathsOf(hits)
	want := []string{"a.txt", "c.md"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("search needle = %v; want %v", got, want)
	}

	if hits := r.search("NEEDLE", 0); len(hits) != 2 {
		t.Fatalf("case-insensitive search returned %d hits", len(hits))
	}
	if hits := r.search("zzznope", 0); len(hits) != 0 {
		t.Fatalf("no-hit search returned %v", contentPathsOf(hits))
	}
	if hits := r.search("alpha", 0); len(hits) != 1 || hits[0].Path != "a.txt" {
		t.Fatalf("alpha = %v", contentPathsOf(hits))
	}
	if hits := r.search("needle", 1); len(hits) != 1 {
		t.Fatalf("limit=1 returned %d hits", len(hits))
	}
}

// Offline `content -db` search: a byte-length-changing fold rune (İ 2->1 byte)
// before the match must not shift the reported Offset or the snippet window.
// The folded match offset is mapped back to the raw text for both, so the
// snippet still contains the match and Offset is its raw start.
func TestContentOfflineSnippetOffsetMapsFoldedToRaw(t *testing.T) {
	raw := strings.Repeat("İ", 300) + " needle " + strings.Repeat("x", 600)
	idx := buildTestContentIndex(t, map[string]string{"doc.txt": raw})
	path := filepath.Join(t.TempDir(), "content.gsx")
	if err := contentSaveFile(path, idx); err != nil {
		t.Fatal(err)
	}
	loaded, err := contentLoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(loaded)
	if err != nil {
		t.Fatal(err)
	}
	hits := r.search("needle", 0)
	if len(hits) != 1 {
		t.Fatalf("search returned %d hits; want 1", len(hits))
	}
	if want := strings.Index(raw, "needle"); hits[0].Offset != want {
		t.Fatalf("Offset = %d; want raw match start %d", hits[0].Offset, want)
	}
	if !strings.Contains(hits[0].Snippet, "needle") {
		t.Fatalf("snippet %q does not contain the match", hits[0].Snippet)
	}
}

func TestContentSearchRejectsNonContiguousGrams(t *testing.T) {
	// All query grams are present but not contiguously, so the trigram
	// candidate set includes the doc and verification must reject it.
	idx := buildTestContentIndex(t, map[string]string{
		"a.txt": "abc middle bcd",
	})
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("abcd", 0); len(hits) != 0 {
		t.Fatalf("false-positive candidate accepted: %v", contentPathsOf(hits))
	}
}

func TestContentSearchParityWithBruteForce(t *testing.T) {
	files := map[string]string{
		"a.txt":  "the packed trigram index",
		"b.txt":  "packed content and terms",
		"c.md":   "nothing to see",
		"sub/d":  "deeply packed values",
		"e.json": `{"needle":"x"}`,
	}
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}

	terms := []string{"packed", "index", "content", "needle", "to", "e", "zzz", "PACKED"}
	for _, term := range terms {
		got := contentPathsOf(r.search(term, 0))

		// Brute force: read every file back and substring-match.
		want := map[string]bool{}
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(dir, path)
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			if containsFold(string(body), term) {
				want[filepath.ToSlash(rel)] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		wantList := make([]string, 0, len(want))
		for p := range want {
			wantList = append(wantList, p)
		}
		sort.Strings(wantList)
		if len(got) != len(wantList) {
			t.Fatalf("parity for %q: got %v, want %v", term, got, wantList)
		}
		for i := range got {
			if got[i] != wantList[i] {
				t.Fatalf("parity for %q: got %v, want %v", term, got, wantList)
			}
		}
	}
}

func containsFold(haystack, needle string) bool {
	return containsLower(lowerASCII(haystack), lowerASCII(needle))
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func containsLower(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestContentIndexFileBuildRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("persisted needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "c.gsx")
	if err := contentSaveFile(path, idx); err != nil {
		t.Fatal(err)
	}
	loaded, err := contentLoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("needle", 0); len(hits) != 1 || hits[0].Path != "x.txt" {
		t.Fatalf("reloaded search = %v", contentPathsOf(hits))
	}
}
