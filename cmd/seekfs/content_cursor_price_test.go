package main

import (
	"fmt"
	"reflect"
	"testing"
)

// TestContentCursorV2OverlayResumePricesOnePage pages a populated name overlay
// with a hidden base hit. Each v2 resume must price its base verification at
// one page: DiskVerified stays within limit verifies plus one snippet read per
// emitted row and a small fixed slack. Before the fix a resume fell back to the
// completeness window and verified every remaining base candidate.
func TestContentCursorV2OverlayResumePricesOnePage(t *testing.T) {
	const files = 200
	const limit = 10
	fixture := make([]contentFixtureFile, 0, files)
	for i := 0; i < files; i++ {
		fixture = append(fixture, contentFixtureFile{
			uint64(1000 + i), fmt.Sprintf("b%03d.txt", i), "needle",
		})
	}
	vol, s := contentCursorSearchVolume(t, fixture)
	addCursorOverlayCreate(t, vol, 900010, "b010z.txt")
	addCursorOverlayCreate(t, vol, 900050, "b050z.txt")
	addCursorOverlayCreate(t, vol, 900150, "b150z.txt")
	addCursorOverlayCreateText(t, vol, 900200, "unrelated1.txt", "other")
	addCursorOverlayCreateText(t, vol, 900201, "unrelated2.txt", "other")
	// b100.txt (FRN 1100) is tombstoned: a hidden base hit that must be
	// skipped without a verification read.
	vol.applyUSNChanges([]usnChange{{FRN: 1100, USN: 5000, Reason: usnReasonFileDelete}})

	const query = "content:needle"
	pages := make([]serviceResponse, 0, 4)
	var cursors []string
	cursor := ""
	var concatenated []string
	for page := 0; page < 4; page++ {
		r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: limit, Cursor: cursor})
		if !r.OK {
			t.Fatalf("page %d: %s", page+1, r.Message)
		}
		if r.CursorRestarted {
			t.Fatalf("page %d restarted: %s", page+1, r.CursorRestartReason)
		}
		if len(r.Results) != limit {
			t.Fatalf("page %d rows=%d; want %d", page+1, len(r.Results), limit)
		}
		pages = append(pages, r)
		concatenated = append(concatenated, r.Results...)
		if r.NextCursor == "" {
			t.Fatalf("page %d has no next cursor", page+1)
		}
		cursors = append(cursors, r.NextCursor)
		cursor = r.NextCursor
	}

	// Finish the walk and prove the pages concatenate to the one-big-page order
	// with no duplicates, using the populated-overlay paging helpers.
	for {
		r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: limit, Cursor: cursor})
		if !r.OK {
			t.Fatalf("continuation page: %s", r.Message)
		}
		if r.CursorRestarted {
			t.Fatalf("continuation page restarted: %s", r.CursorRestartReason)
		}
		concatenated = append(concatenated, r.Results...)
		if r.NextCursor == "" {
			break
		}
		cursors = append(cursors, r.NextCursor)
		cursor = r.NextCursor
	}
	full := contentCursorFull(t, s, query)
	if len(full) < files-1+3 {
		t.Fatalf("full rows=%d; want at least %d", len(full), files-1+3)
	}
	if !reflect.DeepEqual(concatenated, full) {
		t.Fatalf("concatenated pages != full:\n pages=%v\n full =%v", concatenated, full)
	}
	assertNoDuplicatePaths(t, concatenated)
	assertCursorPagesV2(t, query, limit, cursors)

	for page := 1; page < len(pages); page++ {
		opts := queryOptions{Query: query, Limit: limit, Trace: &searchTrace{}}
		_, token, err := decodeAnySearchCursor(pages[page-1].NextCursor, opts)
		if err != nil || token == nil {
			t.Fatalf("page %d cursor is not v2: %v", page+1, err)
		}
		resumeFRN := token.BK
		if token.K == nil {
			resumeFRN = token.ID
		}
		opts.contentAfter = &contentCursorResume{FRN: resumeFRN, Found: resumeFRN == 0, Key: token.K}
		matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
		if err != nil {
			t.Fatalf("page %d direct resume: %v", page+1, err)
		}
		if !reflect.DeepEqual(entryPaths(matches), pages[page].Results) {
			t.Fatalf("page %d direct resume %v != service page %v", page+1, entryPaths(matches), pages[page].Results)
		}
		bound := limit + len(matches) + 2
		t.Logf("page %d: DiskVerified=%d rows=%d bound=%d", page+1, opts.Trace.DiskVerified, len(matches), bound)
		if opts.Trace.DiskVerified > bound {
			t.Fatalf("page %d used %d disk reads; want at most %d (limit %d + %d snippets + 2 slack)",
				page+1, opts.Trace.DiskVerified, bound, limit, len(matches))
		}
	}
}
