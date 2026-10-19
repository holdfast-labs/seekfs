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
	t.Run("delta does not restart", func(t *testing.T) {
		vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
			{2, "a.txt", "needle"}, {3, "b.txt", "needle"}, {4, "c.txt", "needle"}, {5, "d.txt", "needle"},
		})
		one := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 2})
		full := contentCursorFull(t, s, "content:needle")
		vol.content.deltaView().upsert(contentDeltaDoc{FRN: 99, Path: `C:\new.txt`, Text: []byte("needle")})
		two := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 2, Cursor: one.NextCursor})
		if !two.OK {
			t.Fatalf("resumed page failed: %s", two.Message)
		}
		if two.CursorRestarted {
			t.Fatalf("content delta restarted the cursor: %s", two.CursorRestartReason)
		}
		if !reflect.DeepEqual(two.Results, full[2:]) {
			t.Fatalf("resumed page %v != static continuation %v", two.Results, full[2:])
		}
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

func contentCursorPagesNoRestart(t *testing.T, s *goSearchService, query string, limit int) ([]string, []string) {
	t.Helper()
	var paths []string
	var cursors []string
	cursor := ""
	for page := 0; page < 64; page++ {
		r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: limit, Cursor: cursor})
		if !r.OK {
			t.Fatalf("%s page %d: %s", query, page, r.Message)
		}
		if r.CursorRestarted {
			t.Fatalf("%s page %d restarted: %s", query, page, r.CursorRestartReason)
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

func assertCursorPagesV2(t *testing.T, query string, limit int, cursors []string) {
	t.Helper()
	if len(cursors) == 0 {
		t.Fatal("no v2 cursor emitted")
	}
	for _, token := range cursors {
		if _, v2, err := decodeAnySearchCursor(token, queryOptions{Query: query, Limit: limit}); err != nil || v2 == nil {
			t.Fatalf("cursor %q is not v2: v2=%v err=%v", token, v2, err)
		}
	}
}

func assertNoDuplicatePaths(t *testing.T, paths []string) {
	t.Helper()
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if _, ok := seen[path]; ok {
			t.Fatalf("duplicate path %q in %v", path, paths)
		}
		seen[path] = struct{}{}
	}
}

func addCursorOverlayCreate(t *testing.T, vol *serviceVolumeIndex, frn uint64, name string) {
	t.Helper()
	addCursorOverlayCreateText(t, vol, frn, name, "needle")
}

func addCursorOverlayCreateText(t *testing.T, vol *serviceVolumeIndex, frn uint64, name, text string) {
	t.Helper()
	vol.applyUSNChanges([]usnChange{{
		FRN: frn, ParentFRN: 1, USN: int64(frn), Reason: usnReasonFileCreate, Name: name,
	}})
	vol.content.deltaView().upsert(contentDeltaDoc{FRN: frn, Text: []byte(text)})
}

func addCursorOverlayRename(t *testing.T, vol *serviceVolumeIndex, frn uint64, name string) {
	t.Helper()
	vol.applyUSNChanges([]usnChange{{
		FRN: frn, ParentFRN: 1, USN: int64(frn) + 1_000_000, Reason: usnReasonRenameNew, Name: name,
	}})
	vol.content.deltaView().upsert(contentDeltaDoc{FRN: frn, Text: []byte("needle")})
}

// TestContentCursorV2OverlayPagesMatchFull pages a content query on a volume
// with a populated name overlay (creates and renames interleaving by name).
// The concatenated pages must equal one big page, with no duplicate paths, all
// v2 cursors, and no restart.
func TestContentCursorV2OverlayPagesMatchFull(t *testing.T) {
	vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
		{10, "m10.txt", "needle"},
		{30, "m30.txt", "needle"},
		{50, "m50.txt", "needle"},
		{70, "m70.txt", "needle"},
		{90, "m90.txt", "needle"},
		{110, "m110.txt", "needle"},
	})
	addCursorOverlayCreate(t, vol, 999, "m20.txt")
	addCursorOverlayCreate(t, vol, 998, "m60.txt")
	addCursorOverlayRename(t, vol, 30, "m35.txt")
	addCursorOverlayRename(t, vol, 70, "m75.txt")

	const query = "content:needle"
	paged, cursors := contentCursorPagesNoRestart(t, s, query, 2)
	full := contentCursorFull(t, s, query)
	if !reflect.DeepEqual(paged, full) {
		t.Fatalf("paged %v != full %v", paged, full)
	}
	assertNoDuplicatePaths(t, paged)
	assertCursorPagesV2(t, query, 2, cursors)
}

// TestContentCursorV2OverlaySymbolPromotion pages a symbol query with a
// matching overlay row. Symbol-named rows stay ahead across page boundaries.
func TestContentCursorV2OverlaySymbolPromotion(t *testing.T) {
	vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
		{2, "Foo.go", "Foo definition"},
		{3, "foo.h", "Foo header"},
		{4, "alpha.txt", "Foo alpha"},
		{5, "beta.txt", "Foo beta"},
	})
	addCursorOverlayCreateText(t, vol, 999, "foo.cc", "Foo definition")

	const query = "content:Foo"
	paged, cursors := contentCursorPagesNoRestart(t, s, query, 2)
	full := contentCursorFull(t, s, query)
	if !reflect.DeepEqual(paged, full) {
		t.Fatalf("paged %v != full %v", paged, full)
	}
	assertNoDuplicatePaths(t, paged)
	assertCursorPagesV2(t, query, 2, cursors)
	if len(full) < 3 {
		t.Fatalf("full result = %v", full)
	}
	for i := 0; i < 3; i++ {
		name := filepath.Base(full[i])
		if !strings.EqualFold(name, "foo.h") && !strings.EqualFold(name, "foo.go") && !strings.EqualFold(name, "foo.cc") {
			t.Fatalf("symbol rows not promoted across pages: %v", full)
		}
	}
}

// TestContentCursorV2UnrelatedOverlayPublishKeepsPages publishes a
// non-matching overlay record between pages. It must not restart or change the
// page sequence.
func TestContentCursorV2UnrelatedOverlayPublishKeepsPages(t *testing.T) {
	vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
		{10, "m10.txt", "needle"},
		{30, "m30.txt", "needle"},
		{50, "m50.txt", "needle"},
		{70, "m70.txt", "needle"},
		{90, "m90.txt", "needle"},
	})
	addCursorOverlayCreate(t, vol, 999, "m20.txt")

	const query = "content:needle"
	var paged []string
	var cursors []string
	cursor := ""
	for page := 0; page < 16; page++ {
		r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: 2, Cursor: cursor})
		if !r.OK {
			t.Fatalf("page %d: %s", page, r.Message)
		}
		if r.CursorRestarted {
			t.Fatalf("page %d restarted: %s", page, r.CursorRestartReason)
		}
		paged = append(paged, r.Results...)
		if page == 0 {
			vol.applyUSNChanges([]usnChange{{
				FRN: 1000, ParentFRN: 1, USN: 2000, Reason: usnReasonFileCreate, Name: "zzz.log",
			}})
		}
		if r.NextCursor == "" {
			break
		}
		cursors = append(cursors, r.NextCursor)
		cursor = r.NextCursor
	}
	full := contentCursorFull(t, s, query)
	if !reflect.DeepEqual(paged, full) {
		t.Fatalf("paged %v != static full %v", paged, full)
	}
	assertNoDuplicatePaths(t, paged)
	assertCursorPagesV2(t, query, 2, cursors)
}

// TestContentCursorV2OverlayRowBeforeCursorIsSkipped adds a matching overlay
// row whose key is before the cursor between pages: it is not emitted later and
// no path duplicates.
func TestContentCursorV2OverlayRowBeforeCursorIsSkipped(t *testing.T) {
	vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
		{10, "m10.txt", "needle"},
		{30, "m30.txt", "needle"},
		{50, "m50.txt", "needle"},
		{70, "m70.txt", "needle"},
		{90, "m90.txt", "needle"},
	})
	addCursorOverlayCreate(t, vol, 999, "m20.txt")

	const query = "content:needle"
	one := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: 2})
	if !one.OK || one.NextCursor == "" {
		t.Fatalf("first page: %+v", one)
	}
	addCursorOverlayCreate(t, vol, 997, "m15.txt")
	paged := append([]string(nil), one.Results...)
	cursors := []string{one.NextCursor}
	cursor := one.NextCursor
	for page := 0; page < 16; page++ {
		r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: 2, Cursor: cursor})
		if !r.OK {
			t.Fatalf("page %d: %s", page, r.Message)
		}
		if r.CursorRestarted {
			t.Fatalf("page %d restarted: %s", page, r.CursorRestartReason)
		}
		paged = append(paged, r.Results...)
		if r.NextCursor == "" {
			break
		}
		cursors = append(cursors, r.NextCursor)
		cursor = r.NextCursor
	}
	for _, path := range paged {
		if filepath.Base(path) == "m15.txt" {
			t.Fatalf("overlay row before the cursor was emitted: %v", paged)
		}
	}
	assertNoDuplicatePaths(t, paged)
	assertCursorPagesV2(t, query, 2, cursors)
}

// TestContentCursorV2OverlayRowAfterCursorEmittedOnce adds a matching overlay
// row whose key is after the cursor between pages: it is emitted exactly once.
func TestContentCursorV2OverlayRowAfterCursorEmittedOnce(t *testing.T) {
	vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
		{10, "m10.txt", "needle"},
		{30, "m30.txt", "needle"},
		{50, "m50.txt", "needle"},
		{70, "m70.txt", "needle"},
		{90, "m90.txt", "needle"},
	})
	addCursorOverlayCreate(t, vol, 999, "m20.txt")

	const query = "content:needle"
	one := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: 2})
	if !one.OK || one.NextCursor == "" {
		t.Fatalf("first page: %+v", one)
	}
	addCursorOverlayCreate(t, vol, 996, "m25.txt")
	paged := append([]string(nil), one.Results...)
	cursors := []string{one.NextCursor}
	cursor := one.NextCursor
	for page := 0; page < 16; page++ {
		r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: 2, Cursor: cursor})
		if !r.OK {
			t.Fatalf("page %d: %s", page, r.Message)
		}
		if r.CursorRestarted {
			t.Fatalf("page %d restarted: %s", page, r.CursorRestartReason)
		}
		paged = append(paged, r.Results...)
		if r.NextCursor == "" {
			break
		}
		cursors = append(cursors, r.NextCursor)
		cursor = r.NextCursor
	}
	count := 0
	for _, path := range paged {
		if filepath.Base(path) == "m25.txt" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("overlay row after the cursor emitted %d times; want 1 (%v)", count, paged)
	}
	assertNoDuplicatePaths(t, paged)
	assertCursorPagesV2(t, query, 2, cursors)
}

// TestContentCursorV2ValveCappedOverlayPagesResumeWithoutGaps is the valve test
// with overlay hits: pages must concatenate to the uncapped merged order.
func TestContentCursorV2ValveCappedOverlayPagesResumeWithoutGaps(t *testing.T) {
	const files = 400
	const valve = 150
	vol := contentFilterV11Fixture(t, matchingBodies(files))
	addContentOverlay(t, vol, 900001, "f0000z.txt")
	addContentOverlay(t, vol, 900002, "f0150z.txt")
	addContentOverlay(t, vol, 900003, "f0300z.txt")
	const overlays = 3
	prevMS, prevValve := contentDefaultBudgetMS, contentDiskVerifyBudget
	contentDefaultBudgetMS = 0
	contentDiskVerifyBudget = valve
	pinContentDiskVerifyPageScale(t, 1)
	t.Cleanup(func() {
		contentDefaultBudgetMS = prevMS
		contentDiskVerifyBudget = prevValve
	})

	full, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: "content:needle", Limit: files + overlays, Trace: &searchTrace{}, ContentDiskVerifyBudget: 2 * files}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != files+overlays {
		t.Fatalf("uncapped hits=%d; want %d", len(full), files+overlays)
	}

	s := &goSearchService{volumes: []*serviceVolumeIndex{vol}, indexes: []*Index{vol.index}}
	var got []string
	cursor := ""
	for page := 0; page < 16; page++ {
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
	if len(got) != files+overlays {
		t.Fatalf("paged rows=%d; want all %d", len(got), files+overlays)
	}
	for i, e := range full {
		if got[i] != e.Path {
			t.Fatalf("row %d = %q; want %q (pages must concatenate to the uncapped order)", i, got[i], e.Path)
		}
	}
}
