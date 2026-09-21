package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// contentVolRecord describes one service-volume record plus its content text
// for the explicit ordering fixtures below.
type contentVolRecord struct {
	frn       uint64
	parent    int32
	parentFRN uint64
	name      string
	mode      uint32
	size      int64
	modUnix   int64
	path      string
	content   string
}

// newContentRecordVolume builds a service volume from explicit records (with
// parent links, so directory-qualified duplicate basenames are possible) and a
// matching FRN-keyed content index. Records with empty content get no doc.
func newContentRecordVolume(t *testing.T, volume string, recs []contentVolRecord) *serviceVolumeIndex {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	idx := &Index{Source: "usn", Volume: volume, Compact: true}
	build := make([]contentBuildDoc, 0, len(recs))
	for _, r := range recs {
		idx.Records = append(idx.Records, CompactRecord{
			FRN: r.frn, ParentFRN: r.parentFRN, Parent: r.parent,
			Name: r.name, Mode: r.mode, Size: r.size, ModUnix: r.modUnix,
		})
		if r.content != "" {
			path := r.path
			if path == "" {
				path = volume + `\` + r.name
			}
			build = append(build, contentBuildDoc{path: path, frn: r.frn, text: []byte(r.content)})
		}
	}
	contentIndexFRNs(idx)
	cidx, err := assembleContentIndex(build, t.TempDir())
	if err != nil {
		t.Fatalf("assemble content index: %v", err)
	}
	cidx.Origin = contentOriginUSN
	reader, err := openContentReader(cidx)
	if err != nil {
		t.Fatalf("open content reader: %v", err)
	}
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_"+strings.TrimSuffix(volume, ":")+".gsi"), idx)
	if vol.content == nil {
		t.Fatal("content state not initialized")
	}
	frns := make([]uint64, len(cidx.Docs))
	ids := make([]uint32, len(cidx.Docs))
	for i := range cidx.Docs {
		frns[i] = cidx.Docs[i].FRN
		ids[i] = uint32(contentRecordIndexForFRN(idx, cidx.Docs[i].FRN))
	}
	vol.content.setReady(cidx, reader, buildContentResolver(cidx.Docs, frns, ids))
	return vol
}

func entryPathsInOrder(entries []Entry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	return paths
}

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

	// Without sort:relevance the default name/path order is used (alpha.txt,
	// beta.txt, both.txt here), which still differs from the relevance order
	// above (both.txt first), proving the arm changes the order.
	def, err := contentServiceSearch(t, vol, "content:alpha|content:beta", false)
	if err != nil {
		t.Fatal(err)
	}
	wantDefault := []string{"alpha.txt", "beta.txt", "both.txt"}
	if got := entryNamesInOrder(def); !sameOrder(got, wantDefault) {
		t.Fatalf("default content order = %v; want %v", got, wantDefault)
	}
}

// sort:relevance is a content-only ordering: a filename-only query keeps the
// pre-content contract and rejects it as an unsupported sort, while a
// content-token query accepts it.
func TestContentServiceRelevanceRejectedForNonContent(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "beta.txt", "x"},
		{3, "alpha.txt", "y"},
	})
	if _, err := contentServiceSearch(t, vol, "ext:.txt sort:relevance", false); err == nil || !strings.Contains(err.Error(), "unsupported sort") {
		t.Fatalf("non-content sort:relevance = %v; want unsupported sort", err)
	}
	if _, err := contentServiceSearch(t, vol, "content:x sort:relevance", false); err != nil {
		t.Fatalf("content sort:relevance should be accepted: %v", err)
	}
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	if _, err := parseQuery(queryOptions{Query: "ext:.txt sort:relevance"}); err == nil || !strings.Contains(err.Error(), "unsupported sort") {
		t.Fatalf("flag-off non-content sort:relevance = %v; want unsupported sort", err)
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

// PB9: a content query with no explicit sort returns the same relative order as
// the equivalent filename query. The fixture's record order (zzz, aaa, mmm) is
// the candidate/posting order and deliberately differs from name order
// (aaa, mmm, zzz).
func TestContentServiceDefaultOrderMatchesFilename(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "zzz-amber.txt", "amber payload"},
		{3, "aaa-amber.txt", "amber payload"},
		{4, "mmm-amber.txt", "amber payload"},
	})

	content, err := contentServiceSearch(t, vol, "content:amber", false)
	if err != nil {
		t.Fatal(err)
	}
	filename, err := contentServiceSearch(t, vol, "amber", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aaa-amber.txt", "mmm-amber.txt", "zzz-amber.txt"}
	if got := entryNamesInOrder(content); !sameOrder(got, want) {
		t.Fatalf("content order = %v; want name order %v", got, want)
	}
	if got := entryNamesInOrder(filename); !sameOrder(got, want) {
		t.Fatalf("filename order = %v; want name order %v", got, want)
	}
	if !sameOrder(entryNamesInOrder(content), entryNamesInOrder(filename)) {
		t.Fatalf("content order %v != filename order %v", entryNamesInOrder(content), entryNamesInOrder(filename))
	}
	candidate := []string{"zzz-amber.txt", "aaa-amber.txt", "mmm-amber.txt"}
	if sameOrder(entryNamesInOrder(content), candidate) {
		t.Fatalf("content order still candidate/record order: %v", entryNamesInOrder(content))
	}
}

// PB9 bounded window: with more matches than the window, the returned top-N is
// the correct default-order top-N within the window and the result is visibly
// incomplete rather than a silent candidate-order truncation.
func TestContentServiceDefaultOrderWindowFullMarksIncomplete(t *testing.T) {
	// Record/candidate order is e,f,a,b,g,c,h,d; name order is a..h. The two
	// name-smallest (a,b) are records 2 and 3, inside the six-record window but
	// outside a two-result candidate page.
	files := []contentFixtureFile{
		{100, "e.txt", "needle here"},
		{101, "f.txt", "needle here"},
		{102, "a.txt", "needle here"},
		{103, "b.txt", "needle here"},
		{104, "g.txt", "needle here"},
		{105, "c.txt", "needle here"},
		{106, "h.txt", "needle here"},
		{107, "d.txt", "needle here"},
	}
	vol := newContentQueryVolume(t, files)

	trace := &searchTrace{}
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: "content:needle", Limit: 2, ContentCandidateBudget: 6, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := entryNamesInOrder(matches); !sameOrder(got, []string{"a.txt", "b.txt"}) {
		t.Fatalf("windowed default order = %v; want [a.txt b.txt]", got)
	}
	if !trace.ContentIncomplete {
		t.Fatal("window-full default order was not marked incomplete")
	}
	if trace.Complete == nil || *trace.Complete {
		t.Fatalf("window-full default order reported Complete=%v; want false", trace.Complete)
	}
	s := &goSearchService{volumes: []*serviceVolumeIndex{vol}}
	if h := s.searchContentHealth(trace); h == nil || h.State != contentStateDegraded || !h.Incomplete {
		t.Fatalf("content health = %+v; want degraded incomplete", h)
	}
}

// PB9 multi-volume: the merged content result is ordered by the same default
// comparator before the user limit is applied.
func TestContentServiceMultiVolumeDefaultOrder(t *testing.T) {
	volC := newContentQueryVolumeNamed(t, "C:", []contentFixtureFile{
		{2, "zeta.txt", "needle on C"},
		{3, "alpha.txt", "needle on C"},
	})
	volF := newContentQueryVolumeNamed(t, "F:", []contentFixtureFile{
		{2, "mike.txt", "needle on F"},
		{3, "beta.txt", "needle on F"},
	})
	matches, _, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{volC, volF}, "content:needle", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha.txt", "beta.txt", "mike.txt"}
	if got := entryNamesInOrder(matches); !sameOrder(got, want) {
		t.Fatalf("multi-volume default order = %v; want %v", got, want)
	}
}

// PB9 finding 1: duplicate basenames in different directories. Filename
// single-volume order breaks the basename tie by record id, while the shared
// name/path comparator would break it by path. A default content query must
// follow the filename (rank) order, not the path order.
func TestContentServiceDefaultOrderDuplicateBasenameTie(t *testing.T) {
	vol := newContentRecordVolume(t, "C:", []contentVolRecord{
		{frn: 1, parent: -1, parentFRN: 1, name: ".", mode: uint32(os.ModeDir)},
		{frn: 10, parent: 0, parentFRN: 1, name: "zdir", mode: uint32(os.ModeDir)},
		{frn: 11, parent: 0, parentFRN: 1, name: "adir", mode: uint32(os.ModeDir)},
		{frn: 20, parent: 1, parentFRN: 10, name: "notes.txt", size: 10, content: "needle here"},
		{frn: 21, parent: 2, parentFRN: 11, name: "notes.txt", size: 10, content: "needle here"},
	})

	content, err := contentServiceSearch(t, vol, "content:needle", false)
	if err != nil {
		t.Fatal(err)
	}
	filename, err := contentServiceSearch(t, vol, "notes", false)
	if err != nil {
		t.Fatal(err)
	}
	// Record order is znotes (rec 3) then anotes (rec 4); path order would be
	// adir before zdir, so the two orderings differ on this fixture.
	want := []string{`C:\zdir\notes.txt`, `C:\adir\notes.txt`}
	if got := entryPathsInOrder(content); !sameOrder(got, want) {
		t.Fatalf("content duplicate-basename order = %v; want filename rank order %v", got, want)
	}
	if got := entryPathsInOrder(filename); !sameOrder(got, want) {
		t.Fatalf("filename duplicate-basename order = %v; want %v", got, want)
	}
	if !sameOrder(entryPathsInOrder(content), entryPathsInOrder(filename)) {
		t.Fatalf("content order %v != filename order %v", entryPathsInOrder(content), entryPathsInOrder(filename))
	}
}

// PB9 finding 2: explicit sorts on a single-volume content query must match the
// filename single-volume order for the same sort column.
func TestContentServiceSingleVolumeExplicitSortParity(t *testing.T) {
	vol := newContentRecordVolume(t, "C:", []contentVolRecord{
		{frn: 1, parent: -1, parentFRN: 1, name: ".", mode: uint32(os.ModeDir)},
		{frn: 2, parent: 0, parentFRN: 1, name: "a-needle.txt", size: 30, modUnix: 300, content: "needle here"},
		{frn: 3, parent: 0, parentFRN: 1, name: "b-needle.txt", size: 10, modUnix: 100, content: "needle here"},
		{frn: 4, parent: 0, parentFRN: 1, name: "c-needle.txt", size: 20, modUnix: 200, content: "needle here"},
	})
	for _, tc := range []struct {
		sort string
		want []string
	}{
		{"size", []string{"b-needle.txt", "c-needle.txt", "a-needle.txt"}},
		{"modified", []string{"a-needle.txt", "c-needle.txt", "b-needle.txt"}},
		{"path", []string{"a-needle.txt", "b-needle.txt", "c-needle.txt"}},
	} {
		content, err := contentServiceSearch(t, vol, "content:needle sort:"+tc.sort, false)
		if err != nil {
			t.Fatalf("content sort:%s: %v", tc.sort, err)
		}
		filename, err := contentServiceSearch(t, vol, "needle sort:"+tc.sort, false)
		if err != nil {
			t.Fatalf("filename sort:%s: %v", tc.sort, err)
		}
		if got := entryNamesInOrder(content); !sameOrder(got, tc.want) {
			t.Fatalf("content sort:%s = %v; want %v", tc.sort, got, tc.want)
		}
		if !sameOrder(entryNamesInOrder(content), entryNamesInOrder(filename)) {
			t.Fatalf("content sort:%s %v != filename sort:%s %v", tc.sort, entryNamesInOrder(content), tc.sort, entryNamesInOrder(filename))
		}
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
