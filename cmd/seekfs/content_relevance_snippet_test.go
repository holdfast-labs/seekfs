package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func entryNamesInOrder(entries []Entry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

func sameOrder(a, b []string) bool {
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

// sort:relevance orders by matched-leaf count, then earliest match offset, then
// the existing name/path order, and is deterministic. The default content order
// is candidate order, so the two differ on this fixture.
func TestContentServiceRelevanceOrder(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "both.txt", "alpha beta"},
		{3, "beta.txt", "xx beta"},
		{4, "alpha.txt", "xx alpha"},
		{5, "zz.txt", "nothing"},
	})
	matches, err := contentServiceSearch(t, vol, "content:alpha|content:beta sort:relevance", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"both.txt", "alpha.txt", "beta.txt"}
	if got := entryNamesInOrder(matches); !sameOrder(got, want) {
		t.Fatalf("sort:relevance order = %v; want %v", got, want)
	}

	again, err := contentServiceSearch(t, vol, "content:alpha|content:beta sort:relevance", false)
	if err != nil {
		t.Fatal(err)
	}
	if !sameOrder(entryNamesInOrder(again), want) {
		t.Fatalf("sort:relevance is not deterministic: %v then %v", entryNamesInOrder(matches), entryNamesInOrder(again))
	}

	// Without sort:relevance the candidate order is retained (both.txt,
	// beta.txt, alpha.txt here), proving the arm changes the order.
	def, err := contentServiceSearch(t, vol, "content:alpha|content:beta", false)
	if err != nil {
		t.Fatal(err)
	}
	wantDefault := []string{"both.txt", "beta.txt", "alpha.txt"}
	if got := entryNamesInOrder(def); !sameOrder(got, wantDefault) {
		t.Fatalf("default content order = %v; want %v", got, wantDefault)
	}
}

// A non-content query with sort:relevance parses and is left in the default
// (name/path) order, consistent with the other sort columns.
func TestContentServiceRelevanceIgnoredForNonContent(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "beta.txt", "x"},
		{3, "alpha.txt", "y"},
	})
	plain, err := contentServiceSearch(t, vol, "ext:.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := contentServiceSearch(t, vol, "ext:.txt sort:relevance", false)
	if err != nil {
		t.Fatal(err)
	}
	if !sameOrder(entryNamesInOrder(plain), entryNamesInOrder(rel)) {
		t.Fatalf("non-content sort:relevance changed the order: %v vs %v", entryNamesInOrder(plain), entryNamesInOrder(rel))
	}
}

// A content-query result carries a bounded snippet around the matched term; a
// regex-only match yields no snippet rather than a fabricated one.
func TestContentServiceSnippet(t *testing.T) {
	body := strings.Repeat("padding ", 40) + "needle in a haystack " + strings.Repeat("trailing ", 40)
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.txt", body},
		{3, "b.txt", "no match here"},
	})
	matches, err := contentServiceSearch(t, vol, "content:needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("content:needle = %v; want [a.txt]", entryNamesInOrder(matches))
	}
	snippet := matches[0].Snippet
	if !strings.Contains(snippet, "needle") {
		t.Fatalf("snippet %q does not contain the matched term", snippet)
	}
	if !strings.Contains(snippet, "padding") && !strings.Contains(snippet, "trailing") {
		t.Fatalf("snippet %q has no surrounding context", snippet)
	}
	if n := utf8.RuneCountInString(snippet); n > contentSnippetMaxRunes+6 {
		t.Fatalf("snippet length = %d runes; want <= %d", n, contentSnippetMaxRunes+6)
	}

	re, err := contentServiceSearch(t, vol, "content:/nee.*le/", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(re) != 1 {
		t.Fatalf("regex query = %v; want [a.txt]", entryNamesInOrder(re))
	}
	if re[0].Snippet != "" {
		t.Fatalf("regex-only match fabricated a snippet: %q", re[0].Snippet)
	}
}

// Snippet work is bounded by the result limit: more matches than the limit
// yields exactly the limit entries, each with a snippet.
func TestContentServiceSnippetsBoundedByLimit(t *testing.T) {
	var files []contentFixtureFile
	for i := 0; i < 8; i++ {
		files = append(files, contentFixtureFile{frn: uint64(100 + i), name: fmt.Sprintf("hit%02d.txt", i), text: "needle here"})
	}
	vol := newContentQueryVolume(t, files)
	matches, _, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:needle", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("content:needle limit=2 returned %d entries; want 2", len(matches))
	}
	withSnippet := 0
	for _, entry := range matches {
		if entry.Snippet != "" {
			withSnippet++
		}
	}
	if withSnippet != 2 {
		t.Fatalf("snippets for %d of %d limited results; want 2", withSnippet, len(matches))
	}
}
