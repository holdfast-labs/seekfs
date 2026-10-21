package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestContentOverlayUnrelatedKeepsRegexPromotedOrder pins the same overlay
// merge rule for regex promotion that the symbol test pins for symbols. A
// matching overlay row whose name is unrelated to the pattern forces the
// merge/re-sort path; the regex-named base rows must keep their front position.
func TestContentOverlayUnrelatedKeepsRegexPromotedOrder(t *testing.T) {
	vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
		{2, "a10.txt", "Tab control Model"},
		{3, "a20.txt", "Tab and Model"},
		{4, "tab_strip_model.h", "class TabStripModel"},
		{5, "tab_strip_model.cc", "TabStripModel members"},
	})
	addCursorOverlayCreateText(t, vol, 999, "zzz_overlay.txt", "TabStripModel overlay")

	got := contentCursorFull(t, s, `content:/Tab\w+Model/`)
	if len(got) < 2 {
		t.Fatalf("results = %v; want the tab_strip_model rows first", got)
	}
	if filepath.Base(got[0]) != "tab_strip_model.h" || filepath.Base(got[1]) != "tab_strip_model.cc" {
		t.Fatalf("rows = %v; want tab_strip_model.h then tab_strip_model.cc first", contentCursorBaseNames(got))
	}
}

// TestContentCursorV2OverlayRegexPromotion pages a pure-regex query with
// matching overlay rows. Regex-named rows stay ahead across page boundaries and
// the concatenated pages equal one big page with no duplicate paths.
func TestContentCursorV2OverlayRegexPromotion(t *testing.T) {
	vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
		{2, "tab_strip_model.h", "TabStripModel definition"},
		{3, "tab_strip_model.cc", "TabStripModel members"},
		{4, "alpha.txt", "Tab control Model"},
		{5, "beta.txt", "Tab and Model"},
	})
	addCursorOverlayCreateText(t, vol, 999, "tab_strip_model_observer.h", "TabStripModel observer")
	addCursorOverlayCreateText(t, vol, 998, "zzz_overlay.txt", "TabStripModel overlay")

	const query = `content:/Tab\w+Model/`
	paged, cursors := contentCursorPagesNoRestart(t, s, query, 2)
	full := contentCursorFull(t, s, query)
	if !reflect.DeepEqual(paged, full) {
		t.Fatalf("paged %v != full %v", paged, full)
	}
	assertNoDuplicatePaths(t, paged)
	assertCursorPagesV2(t, query, 2, cursors)
	if len(full) < 4 {
		t.Fatalf("full result = %v; want four matching rows", full)
	}
	for i := 0; i < 3; i++ {
		if !strings.HasPrefix(filepath.Base(full[i]), "tab_strip_model") {
			t.Fatalf("regex rows not promoted across pages: %v", contentCursorBaseNames(full))
		}
	}
}
