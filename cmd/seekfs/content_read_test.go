package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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
	releaseContentIndexOnCleanup(t, loaded)
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
	releaseContentIndexOnCleanup(t, loaded)
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

// contentReaderFromGrams builds a queryable reader whose gram section is the
// given posting lists, with docs documents and no text.
func contentReaderFromGrams(t *testing.T, docs int, grams map[string][]contentDocFreq) *contentReader {
	t.Helper()
	idx := &contentIndex{Docs: make([]contentDoc, docs), Sections: map[uint32][]byte{}}
	if len(grams) > 0 {
		idx.Sections[contentSectionGrams] = encodeContentPostingSection(grams)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return r
}

// refContentCandidates is the pre-streaming algorithm: decode every trigram list
// in full and intersect, so the streaming result can be compared against it.
func refContentCandidates(r *contentReader, term string) []uint32 {
	if len(term) < 3 || r.grams == nil {
		all := make([]uint32, len(r.idx.Docs))
		for i := range all {
			all[i] = uint32(i)
		}
		return all
	}
	var current []uint32
	for i := 0; i+3 <= len(term); i++ {
		postings, ok := r.grams.lookup(term[i : i+3])
		if !ok || len(postings) == 0 {
			return nil
		}
		ids := make([]uint32, len(postings))
		for j := range postings {
			ids[j] = postings[j].docID
		}
		if i == 0 {
			current = ids
		} else {
			current = refIntersectSortedDocIDs(current, ids)
		}
		if len(current) == 0 {
			return nil
		}
	}
	return current
}

func refIntersectSortedDocIDs(a, b []uint32) []uint32 {
	out := a[:0:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

// The streamed trigram intersection must equal the full-decode intersection
// exactly, including when the caller's budget truncates it.
func TestContentCandidatesStreamingMatchesFullDecode(t *testing.T) {
	grams := map[string][]contentDocFreq{}
	add := func(key string, ids ...uint32) {
		list := make([]contentDocFreq, len(ids))
		for i, id := range ids {
			list[i] = contentDocFreq{docID: id, tf: 1}
		}
		grams[key] = list
	}
	var evens, bcd, cde []uint32
	for i := uint32(0); i < 20; i += 2 {
		evens = append(evens, i)
	}
	for i := uint32(5); i < 20; i++ {
		bcd = append(bcd, i)
	}
	for i := uint32(10); i < 20; i++ {
		cde = append(cde, i)
	}
	add("abc", evens...)
	add("bcd", bcd...)
	add("cde", cde...)
	r := contentReaderFromGrams(t, 20, grams)

	// abc ∩ bcd ∩ cde = even docIDs at or above 10.
	full := refContentCandidates(r, "abcde")
	want := []uint32{10, 12, 14, 16, 18}
	if !slices.Equal(full, want) {
		t.Fatalf("reference intersection = %v; want %v", full, want)
	}
	if got := r.candidates("abcde", 0); !slices.Equal(got, full) {
		t.Fatalf("candidates budget=0 = %v; want %v", got, full)
	}
	if got := r.candidates("abcde", 10); !slices.Equal(got, full) {
		t.Fatalf("under-budget candidates = %v; want %v", got, full)
	}
	for _, budget := range []int{1, 2, 3} {
		if got := r.candidates("abcde", budget); !slices.Equal(got, full[:budget]) {
			t.Fatalf("over-budget candidates(%d) = %v; want %v", budget, got, full[:budget])
		}
	}
	// A single-trigram term caps at the budget, matching the full list prefix.
	if got := r.candidates("abc", 3); !slices.Equal(got, evens[:3]) {
		t.Fatalf("single-gram candidates = %v; want %v", got, evens[:3])
	}
	// A missing gram is a real empty candidate set, not an error.
	if got := r.candidates("zzz", 5); got != nil {
		t.Fatalf("missing-gram candidates = %v; want nil", got)
	}
}

// A broad key with a small budget must yield the capped set without decoding the
// whole posting list.
func TestContentCandidatesBroadKeyStopsEarly(t *testing.T) {
	const n = contentPostingBlockSize*3 + 7
	list := make([]contentDocFreq, n)
	for i := range list {
		list[i] = contentDocFreq{docID: uint32(i), tf: 1}
	}
	r := contentReaderFromGrams(t, n, map[string][]contentDocFreq{"brd": list})

	decoded := 0
	contentPostingDecodeHook = func() { decoded++ }
	defer func() { contentPostingDecodeHook = nil }()

	const budget = 5
	got := r.candidates("brd", budget)
	if len(got) != budget || got[0] != 0 || got[budget-1] != budget-1 {
		t.Fatalf("candidates = %v; want the first %d docIDs", got, budget)
	}
	if decoded > budget+1 {
		t.Fatalf("decoded %d postings for budget %d; want at most %d", decoded, budget, budget+1)
	}
	if decoded >= n {
		t.Fatalf("decoded the whole %d-posting list despite the budget", n)
	}
}
