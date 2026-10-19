package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func contentCursorSearchVolume(t *testing.T, files []contentFixtureFile) (*serviceVolumeIndex, *goSearchService) {
	t.Helper()
	vol := newContentQueryVolume(t, files)
	s := &goSearchService{volumes: []*serviceVolumeIndex{vol}, indexes: []*Index{vol.index}}
	return vol, s
}

func contentCursorPages(t *testing.T, s *goSearchService, query string, limit int) ([]string, []string) {
	t.Helper()
	var paths []string
	var cursors []string
	cursor := ""
	for page := 0; page < 32; page++ {
		r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: limit, Cursor: cursor})
		if !r.OK {
			t.Fatalf("%s page %d: %s", query, page, r.Message)
		}
		paths = append(paths, r.Results...)
		if r.NextCursor == "" {
			return paths, cursors
		}
		cursors = append(cursors, r.NextCursor)
		cursor = r.NextCursor
	}
	t.Fatalf("%s did not terminate", query)
	return nil, nil
}

func contentCursorFull(t *testing.T, s *goSearchService, query string) []string {
	t.Helper()
	r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: 1000})
	if !r.OK {
		t.Fatalf("%s: %s", query, r.Message)
	}
	return r.Results
}

func TestContentCursorV2PagesMatchFullStatic(t *testing.T) {
	_, s := contentCursorSearchVolume(t, []contentFixtureFile{
		{2, "Foo.go", "Foo definition"},
		{3, "foo.h", "Foo header"},
		{4, "alpha.txt", "Foo alpha"},
		{5, "beta.txt", "Foo beta"},
		{6, "gamma.txt", "Foo gamma"},
		{7, "delta.txt", "Foo delta"},
	})
	for _, query := range []string{"content:Foo", "content:Foo sort:size"} {
		t.Run(query, func(t *testing.T) {
			paged, cursors := contentCursorPages(t, s, query, 2)
			full := contentCursorFull(t, s, query)
			if !reflect.DeepEqual(paged, full) {
				t.Fatalf("paged %v != full %v", paged, full)
			}
			if len(cursors) == 0 {
				t.Fatal("no v2 cursor emitted")
			}
			for _, token := range cursors {
				_, v2, err := decodeAnySearchCursor(token, queryOptions{Query: query, Limit: 2})
				if err != nil || v2 == nil {
					t.Fatalf("cursor %q is not v2: %v", token, err)
				}
			}
			if query == "content:Foo" {
				if len(full) < 2 {
					t.Fatalf("full result = %v", full)
				}
				first, second := filepath.Base(full[0]), filepath.Base(full[1])
				if !(strings.EqualFold(first, "foo.go") && strings.EqualFold(second, "foo.h")) &&
					!(strings.EqualFold(first, "foo.h") && strings.EqualFold(second, "foo.go")) {
					t.Fatalf("symbol promotion order = %v, %v; want Foo.go/foo.h first", first, second)
				}
			}
		})
	}
}

func TestContentCursorV2ResumeVerifiesOnePage(t *testing.T) {
	files := make([]contentFixtureFile, 0, 20)
	for i := 0; i < 20; i++ {
		files = append(files, contentFixtureFile{uint64(100 + i), "file" + string(rune('a'+i)) + ".txt", "needle"})
	}
	_, s := contentCursorSearchVolume(t, files)
	one := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 2})
	two := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 2, Cursor: one.NextCursor})
	if one.NextCursor == "" || two.NextCursor == "" {
		t.Fatalf("missing cursors: page1=%q page2=%q", one.NextCursor, two.NextCursor)
	}
	opts := queryOptions{Query: "content:needle", Limit: 2, Trace: &searchTrace{}}
	_, token, err := decodeAnySearchCursor(two.NextCursor, opts)
	if err != nil || token == nil {
		t.Fatalf("page2 cursor not v2: %v", err)
	}
	opts.contentAfter = &contentCursorResume{FRN: token.ID}
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{s.volumes[0]}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("resumed page = %d rows; want 2", len(matches))
	}
	// Two verification reads plus the two post-limit snippet reads. A v1
	// offset resume (offset 4 + limit 2) would verify six candidates before
	// this page; this resume verifies only the page itself.
	if opts.Trace.DiskVerified > 4 {
		t.Fatalf("resumed page used %d disk reads; want 2 verifies + 2 snippets", opts.Trace.DiskVerified)
	}
	three := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 2, Cursor: two.NextCursor})
	if !three.OK || !reflect.DeepEqual(entryPaths(matches), three.Results) {
		t.Fatalf("direct resume %v != service page %v", entryPaths(matches), three.Results)
	}
}

func entryPaths(entries []Entry) []string {
	out := make([]string, len(entries))
	for i := range entries {
		out[i] = entries[i].Path
	}
	return out
}

func TestContentCursorV2RestartsOnMutation(t *testing.T) {
	t.Run("delta", func(t *testing.T) {
		vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
			{2, "a.txt", "needle"}, {3, "b.txt", "needle"}, {4, "c.txt", "needle"}, {5, "d.txt", "needle"},
		})
		one := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 2})
		vol.content.deltaView().upsert(contentDeltaDoc{FRN: 99, Path: `C:\new.txt`, Text: []byte("needle")})
		two := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 2, Cursor: one.NextCursor})
		assertCursorRestart(t, two, one)
	})
	t.Run("name", func(t *testing.T) {
		vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
			{2, "a.txt", "needle"}, {3, "b.txt", "needle"}, {4, "c.txt", "needle"}, {5, "d.txt", "needle"},
		})
		one := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 2})
		vol.baseCheckpoint++
		two := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 2, Cursor: one.NextCursor})
		assertCursorRestart(t, two, one)
	})
}

func assertCursorRestart(t *testing.T, restarted, first serviceResponse) {
	t.Helper()
	if !restarted.OK {
		t.Fatalf("restarted page failed: %s", restarted.Message)
	}
	if !restarted.CursorRestarted {
		t.Fatalf("cursor_restarted = false; want true")
	}
	if restarted.CursorRestartReason == "" {
		t.Fatal("cursor_restart_reason is empty")
	}
	if !reflect.DeepEqual(restarted.Results, first.Results) {
		t.Fatalf("restart page %v != first page %v", restarted.Results, first.Results)
	}
}

func TestContentCursorV2OutOfScopeStaysV1(t *testing.T) {
	files := []contentFixtureFile{
		{2, "a.txt", "needle"}, {3, "b.txt", "needle"}, {4, "c.txt", "needle"}, {5, "d.txt", "needle"},
	}
	// Multi-volume: v1.
	volA, _ := contentCursorSearchVolume(t, files)
	volB := newContentQueryVolumeNamed(t, "D:", []contentFixtureFile{
		{102, "e.txt", "needle"}, {103, "f.txt", "needle"}, {104, "g.txt", "needle"}, {105, "h.txt", "needle"},
	})
	sMulti := &goSearchService{volumes: []*serviceVolumeIndex{volA, volB}, indexes: []*Index{volA.index, volB.index}}
	assertV1Paging(t, sMulti, "content:needle")

	// Relevance: v1.
	_, sRelevance := contentCursorSearchVolume(t, files)
	assertV1Paging(t, sRelevance, "content:needle sort:relevance")

	// Non-empty content delta: v1.
	volDelta, sDelta := contentCursorSearchVolume(t, files)
	volDelta.content.deltaView().upsert(contentDeltaDoc{FRN: 2, Path: `C:\a.txt`, Text: []byte("needle fresh")})
	assertV1Paging(t, sDelta, "content:needle")
}

func assertV1Paging(t *testing.T, s *goSearchService, query string) {
	t.Helper()
	one := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: 2})
	if !one.OK || one.NextCursor == "" {
		t.Fatalf("%s first page: %+v", query, one)
	}
	if _, v2, err := decodeAnySearchCursor(one.NextCursor, queryOptions{Query: query, Limit: 2}); err != nil || v2 != nil {
		t.Fatalf("%s cursor is not v1: v2=%v err=%v", query, v2, err)
	}
	paged, cursors := contentCursorPages(t, s, query, 2)
	full := contentCursorFull(t, s, query)
	if !reflect.DeepEqual(paged, full) {
		t.Fatalf("%s paged %v != full %v", query, paged, full)
	}
	for _, token := range cursors {
		if _, v2, err := decodeAnySearchCursor(token, queryOptions{Query: query, Limit: 2}); err != nil || v2 != nil {
			t.Fatalf("%s cursor is not v1: v2=%v err=%v", query, v2, err)
		}
	}
}

func TestContentCursorV2RejectsBadTokens(t *testing.T) {
	_, s := contentCursorSearchVolume(t, []contentFixtureFile{
		{2, "a.txt", "needle"}, {3, "b.txt", "needle"}, {4, "c.txt", "needle"},
	})
	if r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 1, Cursor: "%%%"}); r.OK {
		t.Fatal("malformed token accepted")
	}
	oversized := strings.Repeat("A", searchCursorMaxBytes+1)
	if r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 1, Cursor: oversized}); r.OK {
		t.Fatal("oversized token accepted")
	}
	one := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 1})
	if r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:other", Limit: 1, Cursor: one.NextCursor}); r.OK || r.Message != "cursor does not match this query" {
		t.Fatalf("wrong-query token = ok:%v message:%q", r.OK, r.Message)
	}
}

// TestContentCursorV2ValveCappedPagesResumeWithoutGaps walks a v2 cursor
// under a per-request disk valve smaller than the result set. Each resume
// prices one page, so paging reaches every hit, and the concatenated pages
// equal the uncapped rank order with no duplicates or gaps.
func TestContentCursorV2ValveCappedPagesResumeWithoutGaps(t *testing.T) {
	const files = 400
	const valve = 150
	vol := contentFilterV11Fixture(t, matchingBodies(files))
	prevMS, prevValve := contentDefaultBudgetMS, contentDiskVerifyBudget
	contentDefaultBudgetMS = 0
	contentDiskVerifyBudget = valve
	pinContentDiskVerifyPageScale(t, 1)
	t.Cleanup(func() {
		contentDefaultBudgetMS = prevMS
		contentDiskVerifyBudget = prevValve
	})

	full, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: "content:needle", Limit: files, Trace: &searchTrace{}, ContentDiskVerifyBudget: 2 * files}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != files {
		t.Fatalf("uncapped hits=%d; want %d", len(full), files)
	}

	s := &goSearchService{volumes: []*serviceVolumeIndex{vol}, indexes: []*Index{vol.index}}
	var got []string
	cursor := ""
	for page := 0; page < 10; page++ {
		resp := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 100, Cursor: cursor})
		if !resp.OK {
			t.Fatalf("page %d: %s", page, resp.Message)
		}
		if resp.CursorRestarted {
			t.Fatalf("page %d restarted: %s", page, resp.CursorRestartReason)
		}
		got = append(got, resp.Results...)
		if resp.NextCursor == "" {
			break
		}
		if page > 0 && !strings.HasPrefix(resp.NextCursor, "eyJ2IjoyL") {
			t.Fatalf("page %d emitted a non-v2 cursor", page)
		}
		cursor = resp.NextCursor
	}
	if len(got) != files {
		t.Fatalf("paged rows=%d; want all %d (each v2 resume prices one page)", len(got), files)
	}
	for i, e := range full {
		if got[i] != e.Path {
			t.Fatalf("row %d = %q; want %q (pages must concatenate to the uncapped order)", i, got[i], e.Path)
		}
	}
}
