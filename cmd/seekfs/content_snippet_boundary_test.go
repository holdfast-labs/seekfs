package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// contentSnippetWindow bounds a match at offset 0: it must include the match and
// must not claim truncation on the left.
func TestContentSnippetWindowAtOffsetZero(t *testing.T) {
	text := []byte("needle in a haystack")
	got := contentSnippetWindow(text, 0, len("needle"))
	if !strings.Contains(got, "needle") {
		t.Fatalf("offset-0 snippet %q missing the match", got)
	}
	if strings.HasPrefix(got, "...") {
		t.Fatalf("offset-0 snippet %q fabricated a leading ellipsis", got)
	}
}

// A match ending at EOF must not read past the document or fabricate a trailing
// ellipsis.
func TestContentSnippetWindowAtEOF(t *testing.T) {
	text := []byte("some context then needle")
	off := bytes.Index(text, []byte("needle"))
	got := contentSnippetWindow(text, off, len("needle"))
	if !strings.Contains(got, "needle") {
		t.Fatalf("EOF snippet %q missing the match", got)
	}
	if strings.HasSuffix(got, "...") {
		t.Fatalf("EOF snippet %q fabricated a trailing ellipsis", got)
	}
}

// An offset exactly at len(text) (and empty text) must not panic; there is no
// match start to window around, so the snippet is empty.
func TestContentSnippetWindowAtEndOffset(t *testing.T) {
	text := []byte("needle")
	if got := contentSnippetWindow(text, len(text), 0); got != "" {
		t.Fatalf("offset==len snippet = %q; want empty", got)
	}
	if got := contentSnippetWindow(nil, 0, 0); got != "" {
		t.Fatalf("empty text snippet = %q; want empty", got)
	}
}

// A multibyte rune at the window start is never split: even a caller offset
// inside the rune snaps back to its start and the result is valid UTF-8.
func TestContentSnippetWindowDoesNotSplitRune(t *testing.T) {
	text := []byte("€abc needle tail")
	got := contentSnippetWindow(text, 1, len("abc"))
	if !utf8.ValidString(got) {
		t.Fatalf("snippet %q is not valid UTF-8", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("snippet %q contains a broken rune", got)
	}
	if !strings.Contains(got, "€abc") {
		t.Fatalf("snippet %q lost the snapped-to rune", got)
	}
}

// A document longer than the window on both sides rolls over: the window stays
// at the cap and both sides are marked with an ellipsis.
func TestContentSnippetWindowRolloverPastCap(t *testing.T) {
	body := strings.Repeat("a", contentSnippetMaxRunes*2) + "needle" + strings.Repeat("b", contentSnippetMaxRunes*2)
	text := []byte(body)
	off := bytes.Index(text, []byte("needle"))
	got := contentSnippetWindow(text, off, len("needle"))
	if !strings.Contains(got, "needle") {
		t.Fatalf("rollover snippet %q missing the match", got)
	}
	if !strings.HasPrefix(got, "...") || !strings.HasSuffix(got, "...") {
		t.Fatalf("rollover snippet %q; want ellipses on both sides", got)
	}
	if n := utf8.RuneCountInString(got); n > contentSnippetMaxRunes+6 {
		t.Fatalf("rollover snippet length %d runes; want <= %d", n, contentSnippetMaxRunes+6)
	}
}

// m1: when a case-preserving source is available the snippet reflects the
// original case; without one it falls back to the normalized text, and an
// inexact mapping also falls back rather than fabricating.
func TestContentSnippetWindowSourcePreservesCase(t *testing.T) {
	source := []byte("Café Needle Here")
	normalized := []byte(strings.ToLower(string(source)))
	off := bytes.Index(normalized, []byte("needle"))
	if off < 0 {
		t.Fatal("fixture has no needle")
	}
	got := contentSnippetWindowSource(normalized, &contentSnippetSource{text: source}, off, len("needle"))
	if !strings.Contains(got, "Needle") {
		t.Fatalf("sourced snippet %q lost original case", got)
	}

	// No source: the normalized text is used verbatim.
	plain := contentSnippetWindowSource(normalized, nil, off, len("needle"))
	if !strings.Contains(plain, "needle") || strings.Contains(plain, "Needle") {
		t.Fatalf("fallback snippet %q; want lowercase normalized text", plain)
	}

	// Inexact mapping (offset inside a replacement) must fall back, not splice.
	inexact := &contentSnippetSource{text: source, fixups: contentLossyFixups{shifts: []contentShift{{at: 0, gained: 1}}}}
	fallback := contentSnippetWindowSource(normalized, inexact, 1, len("af"))
	if fallback != contentSnippetWindow(normalized, 1, len("af")) {
		t.Fatalf("inexact mapping did not fall back: %q", fallback)
	}
}

// Relevance ranks a term and a phrase match with bounded windows.
func TestContentServiceSnippetPhraseVsTerm(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.txt", "xx needle phrase yy"},
		{3, "b.txt", "no match"},
	})
	termMatches, err := contentServiceSearch(t, vol, "content:needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(termMatches) != 1 || !strings.Contains(termMatches[0].Snippet, "needle") {
		t.Fatalf("term snippet = %+v", termMatches)
	}
	phraseMatches, err := contentServiceSearch(t, vol, `content:"needle phrase"`, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(phraseMatches) != 1 || !strings.Contains(phraseMatches[0].Snippet, "needle phrase") {
		t.Fatalf("phrase snippet = %+v", phraseMatches)
	}
}

// Each result's snippet is taken from its own volume's text, not another
// volume's.
func TestContentServiceSnippetMultiVolumeAssociation(t *testing.T) {
	volC := newContentQueryVolumeNamed(t, "C:", []contentFixtureFile{
		{2, "c.txt", "ccc needle ccc"},
	})
	volF := newContentQueryVolumeNamed(t, "F:", []contentFixtureFile{
		{2, "f.txt", "fff needle fff"},
	})
	matches, _, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{volC, volF}, "content:needle", 100)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, m := range matches {
		byName[m.Name] = m.Snippet
	}
	if !strings.Contains(byName["c.txt"], "ccc") {
		t.Fatalf("c.txt snippet %q does not come from C:", byName["c.txt"])
	}
	if !strings.Contains(byName["f.txt"], "fff") {
		t.Fatalf("f.txt snippet %q does not come from F:", byName["f.txt"])
	}
}

// M1: with limit < matches, sort:relevance must select the highest-scored
// matches, not the first candidate-order page. The high-score documents sit at
// the end of candidate order, so a truncated pre-rank page would miss them.
func TestContentRelevanceTopNWithLimit(t *testing.T) {
	var files []contentFixtureFile
	for i := 0; i < 12; i++ {
		files = append(files, contentFixtureFile{
			frn:  uint64(10 + i),
			name: "low" + string(rune('a'+i)) + ".txt",
			text: "alpha only",
		})
	}
	hiNames := []string{"hi00.md", "hi01.md", "hi02.md"}
	for i, name := range hiNames {
		files = append(files, contentFixtureFile{frn: uint64(200 + i), name: name, text: "alpha beta"})
	}
	vol := newContentQueryVolume(t, files)

	// Low docs satisfy the OR group by extension (0 content leaves); hi docs
	// satisfy it with content:beta, so they score 2 against the low docs' 1.
	matches, trace, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{vol},
		"content:alpha ext:.txt|content:beta sort:relevance", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hi00.md", "hi01.md", "hi02.md"}
	if got := namesOf(matches); !sameOrder(got, want) {
		t.Fatalf("top-N relevance = %v; want %v", got, want)
	}
	if trace.ContentIncomplete {
		t.Fatalf("window covered all %d matches; unexpected incomplete", len(files))
	}
}

// M1: the window rule is min(budget, max(limit*100, 4096)) and is
// overflow-safe.
func TestContentRelevanceWindowRule(t *testing.T) {
	budget := contentDefaultCandidateBudget
	if got := contentRelevanceWindow(0, budget); got != 0 {
		t.Fatalf("window(0) = %d; want 0", got)
	}
	if got := contentRelevanceWindow(1, budget); got != 4096 {
		t.Fatalf("window(1) = %d; want the 4096 floor", got)
	}
	if got := contentRelevanceWindow(100, budget); got != 10000 {
		t.Fatalf("window(100) = %d; want 10000", got)
	}
	if got := contentRelevanceWindow(int(^uint(0)>>1), budget); got != budget {
		t.Fatalf("window(maxint) = %d; want the budget %d", got, budget)
	}
	// A per-query budget below the 4096 floor caps the window.
	if got := contentRelevanceWindow(1, 3); got != 3 {
		t.Fatalf("window(1, budget=3) = %d; want 3", got)
	}
}

// A relevance window that fills up means more matches may exist: the result is
// marked incomplete (Complete=false), never presented as a complete top-N.
func TestContentServiceRelevanceWindowFullMarksIncomplete(t *testing.T) {
	var files []contentFixtureFile
	for i := 0; i < 8; i++ {
		files = append(files, contentFixtureFile{frn: uint64(100 + i), name: fmt.Sprintf("hit%02d.txt", i), text: "needle here"})
	}
	vol := newContentQueryVolume(t, files)

	trace := &searchTrace{}
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: "content:needle sort:relevance", Limit: 2, ContentCandidateBudget: 3, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("relevance returned %d matches; want the user limit 2", len(matches))
	}
	if !trace.ContentIncomplete {
		t.Fatal("window-full relevance result was not marked incomplete")
	}
	if trace.Complete == nil || *trace.Complete {
		t.Fatalf("window-full relevance result reported Complete=%v; want false", trace.Complete)
	}
}

// m3: relevance scores an OR group by the alternative that satisfied it; a
// content match from a failing alternative must not inflate the score.
func TestContentRelevanceJointOrDoesNotInflate(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{300, "a.txt", "zz beta alpha"},
		{301, "b.md", "beta"},
	})
	pq := parsedQuery{
		SortColumn: "relevance",
		OrGroups: [][]parsedQuery{{
			{Content: []contentLeaf{{Kind: contentLeafTerm, Text: "beta", LeafID: 0}}, Exts: []string{"md"}},
			{Content: []contentLeaf{{Kind: contentLeafTerm, Text: "alpha", LeafID: 1}}},
		}},
	}
	m := newContentLeafMatcher(pq)
	entryA := Entry{FRN: 300, Path: `C:\a.txt`, Name: "a.txt"}
	entryB := Entry{FRN: 301, Path: `C:\b.md`, Name: "b.md"}

	relA := contentRelevanceOf(vol, entryA, pq, m)
	relB := contentRelevanceOf(vol, entryB, pq, m)
	if relA.score != 1 {
		t.Fatalf("inflated score for a.txt = %d; want 1 (beta's alternative failed ext:.md)", relA.score)
	}
	if relB.score != 1 {
		t.Fatalf("b.md score = %d; want 1", relB.score)
	}
	if relA.score > relB.score {
		t.Fatalf("inflated score reordered results: a=%d b=%d", relA.score, relB.score)
	}
}

// M2: the offline JSON keeps results as path strings and adds a parallel
// snippets array; plain stdout keeps the path line unless --snippet is given.
func TestContentCLIJSONSnippetShape(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.txt"), []byte("Needle in a haystack"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), root, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "content.gsx")
	if err := contentSaveFile(db, idx); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() error { return cmdContent([]string{"-db", db, "--json", "needle"}) })
	var decoded struct {
		Results  []string `json:"results"`
		Snippets []string `json:"snippets"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode JSON %q: %v", out, err)
	}
	if len(decoded.Results) != 1 || !strings.HasSuffix(decoded.Results[0], "doc.txt") {
		t.Fatalf("results = %v; want paths-only [doc.txt]", decoded.Results)
	}
	if strings.Contains(decoded.Results[0], "needle") {
		t.Fatalf("results leaked snippet text: %q", decoded.Results[0])
	}
	if len(decoded.Snippets) != len(decoded.Results) {
		t.Fatalf("snippets %v not aligned to results %v", decoded.Snippets, decoded.Results)
	}
	if len(decoded.Snippets) != 1 || !strings.Contains(decoded.Snippets[0], "Needle") {
		t.Fatalf("snippets = %v; want the matched window with original case", decoded.Snippets)
	}

	plain := captureStdout(t, func() error { return cmdContent([]string{"-db", db, "needle"}) })
	if strings.Contains(plain, "\t") || strings.Contains(plain, "haystack") {
		t.Fatalf("default stdout leaked a snippet: %q", plain)
	}

	withSnippet := captureStdout(t, func() error { return cmdContent([]string{"-db", db, "--snippet", "needle"}) })
	if !strings.Contains(withSnippet, "doc.txt\t") || !strings.Contains(withSnippet, "Needle") {
		t.Fatalf("--snippet stdout = %q; want path and the case-preserved match", withSnippet)
	}
}

// The offline JSON keeps the original `results` key (an array of path strings)
// alongside the additive `snippets` key; older consumers are unaffected.
func TestContentCLIJSONKeepsResultsAndSnippetsKeys(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.txt"), []byte("needle body"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), root, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "content.gsx")
	if err := contentSaveFile(db, idx); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() error { return cmdContent([]string{"-db", db, "--json", "needle"}) })
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("decode JSON %q: %v", out, err)
	}
	if _, ok := raw["results"]; !ok {
		t.Fatalf("JSON lost the original results key: %q", out)
	}
	if _, ok := raw["snippets"]; !ok {
		t.Fatalf("JSON missing the additive snippets key: %q", out)
	}
	var results []string
	if err := json.Unmarshal(raw["results"], &results); err != nil {
		t.Fatalf("results is not an array of strings: %v", err)
	}
	if len(results) != 1 || !strings.HasSuffix(results[0], "doc.txt") || strings.Contains(results[0], "needle") {
		t.Fatalf("results = %q; want a single path string", results)
	}
}

// m4: the command boundary strips local-only content text from rows.
func TestServiceResponseRedactsContentText(t *testing.T) {
	resp := serviceResponse{Rows: []jsonResult{
		{Path: "C:\\a.txt", Snippet: "secret matched text"},
		{Path: "C:\\b.txt", Snippet: ""},
	}}
	resp.redactContentText()
	for _, row := range resp.Rows {
		if row.Snippet != "" {
			t.Fatalf("row %q kept snippet %q", row.Path, row.Snippet)
		}
	}
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	saved := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	out, readErr := io.ReadAll(r)
	_ = r.Close()
	os.Stdout = saved
	if runErr != nil {
		t.Fatalf("command error: %v", runErr)
	}
	if readErr != nil {
		t.Fatalf("read stdout: %v", readErr)
	}
	return string(out)
}
