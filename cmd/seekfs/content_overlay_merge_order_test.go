package main

import (
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestContentOverlayUnrelatedKeepsPromotedOrder pins the ranking bug: an
// overlay publish that contributes no matching row must not change the
// single-volume content page, so symbol promotion (Foo.h first) survives.
func TestContentOverlayUnrelatedKeepsPromotedOrder(t *testing.T) {
	_, s := contentCursorSearchVolume(t, []contentFixtureFile{
		{2, "Foo.go", "Foo definition"},
		{3, "foo.h", "Foo header"},
		{4, "alpha.txt", "Foo alpha"},
		{5, "beta.txt", "Foo beta"},
	})
	const query = "content:Foo"
	want := contentCursorFull(t, s, query)

	s.volumes[0].applyUSNChanges([]usnChange{{
		FRN: 999, ParentFRN: 1, USN: 500, Reason: usnReasonFileCreate, Name: "zzz.log",
	}})
	got := contentCursorFull(t, s, query)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay page = %v; want promoted no-overlay page %v", got, want)
	}
	if len(got) < 2 || filepath.Base(got[0]) != "foo.h" {
		t.Fatalf("promoted page = %v; want foo.h first", got)
	}
}

func contentCursorBaseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i := range paths {
		out[i] = filepath.Base(paths[i])
	}
	return out
}

// TestContentOverlayMatchingRowInsertsAtRank pins the default-sort merge: a
// matching overlay row slots into the base rank order, a shadowed base row is
// hidden, and no row duplicates.
func TestContentOverlayMatchingRowInsertsAtRank(t *testing.T) {
	vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
		{10, "m10.txt", "needle"},
		{30, "m30.txt", "needle"},
		{50, "m50.txt", "needle"},
		{70, "m70.txt", "needle"},
	})
	vol.applyUSNChanges([]usnChange{
		{FRN: 999, ParentFRN: 1, USN: 900, Reason: usnReasonFileCreate, Name: "m20.txt"},
		{FRN: 30, ParentFRN: 1, USN: 901, Reason: usnReasonRenameNew, Name: "m35.txt"},
	})
	delta := vol.content.deltaView()
	delta.upsert(contentDeltaDoc{FRN: 999, Text: []byte("needle")})
	delta.upsert(contentDeltaDoc{FRN: 30, Text: []byte("needle")})

	got := contentCursorFull(t, s, "content:needle")
	want := []string{"m10.txt", "m20.txt", "m35.txt", "m50.txt", "m70.txt"}
	if names := contentCursorBaseNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("merged page = %v; want %v", names, want)
	}
	seen := make(map[string]struct{}, len(got))
	for _, path := range got {
		if _, ok := seen[path]; ok {
			t.Fatalf("duplicate path %q in %v", path, got)
		}
		seen[path] = struct{}{}
	}
	for _, path := range got {
		if filepath.Base(path) == "m30.txt" {
			t.Fatalf("shadowed base row appeared: %v", got)
		}
	}
}

// TestContentOverlayFrontierDropsLaterOverlayRow pins the incomplete-page
// frontier: with two verified base reads the frontier is m50.txt, so the
// matching overlay row before it (m20.txt) stays and the row after it
// (m60.txt) drops, while both verified base rows stay.
func TestContentOverlayFrontierDropsLaterOverlayRow(t *testing.T) {
	overlayPrefixSetup(t)
	vol := newContentRecordVolume(t, "C:", []contentVolRecord{
		{frn: 10, parent: -1, parentFRN: 1, name: "m10.txt", content: "needle"},
		{frn: 30, parent: -1, parentFRN: 1, name: "m30.txt", content: "needle"},
		{frn: 50, parent: -1, parentFRN: 1, name: "m50.txt", content: "needle"},
		{frn: 70, parent: -1, parentFRN: 1, name: "m70.txt", content: "needle"},
	})
	addContentOverlay(t, vol, 1, "m20.txt")
	addContentOverlay(t, vol, 2, "m60.txt")

	got, trace := overlayPrefixSearch(t, vol, 10, 2)
	if !trace.contentIncomplete() {
		t.Fatalf("capped query not incomplete: got=%v", got)
	}
	want := []string{"m10.txt", "m20.txt", "m30.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capped page = %v; want %v", got, want)
	}
}

// TestContentOverlayFrontierInsidePromotedGroup pins the frontier cut when the
// page stops inside the symbol-promoted group: every unpromoted overlay row
// ranks after the unverified promoted frontier, even one whose name sorts
// before it, so it must drop instead of filling the page past a gap.
func TestContentOverlayFrontierInsidePromotedGroup(t *testing.T) {
	overlayPrefixSetup(t)
	vol := newContentRecordVolume(t, "C:", []contentVolRecord{
		{frn: 10, parent: -1, parentFRN: 1, name: "needle.h", content: "needle"},
		{frn: 30, parent: -1, parentFRN: 1, name: "needle.go", content: "needle"},
		{frn: 50, parent: -1, parentFRN: 1, name: "m50.txt", content: "needle"},
	})
	addContentOverlay(t, vol, 1, "a20.txt")

	full, _ := overlayPrefixSearch(t, vol, 10, 100)
	if want := []string{"needle.h", "needle.go", "a20.txt", "m50.txt"}; !reflect.DeepEqual(full, want) {
		t.Fatalf("uncapped page = %v; want %v", full, want)
	}
	got, trace := overlayPrefixSearch(t, vol, 10, 1)
	if !trace.contentIncomplete() {
		t.Fatalf("capped query not incomplete: got=%v", got)
	}
	if !isNamePrefix(got, full) {
		t.Fatalf("capped page = %v; want a prefix of %v", got, full)
	}
}

// TestContentCursorKeyReproducesMergedPageOrder derives the cursor page key
// independently, sorts the pre-merge rows by it, and checks the result is
// exactly the order mergeContentOverlayMatches emits.
func TestContentCursorKeyReproducesMergedPageOrder(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		vol, _ := contentCursorSearchVolume(t, []contentFixtureFile{
			{10, "m10.txt", "needle"},
			{30, "m30.txt", "needle"},
			{50, "m50.txt", "needle"},
			{70, "m70.txt", "needle"},
		})
		addContentOverlay(t, vol, 999, "m20.txt")
		addContentOverlay(t, vol, 998, "m60.txt")
		assertCursorKeyOrder(t, vol, "content:needle", []contentFixtureFile{
			{10, "m10.txt", "needle"},
			{30, "m30.txt", "needle"},
			{50, "m50.txt", "needle"},
			{70, "m70.txt", "needle"},
		})
	})
	t.Run("symbol", func(t *testing.T) {
		files := []contentFixtureFile{
			{2, "Foo.go", "Foo definition"},
			{3, "foo.h", "Foo header"},
			{4, "alpha.txt", "Foo alpha"},
			{5, "beta.txt", "Foo beta"},
		}
		vol, _ := contentCursorSearchVolume(t, files)
		addContentOverlay(t, vol, 999, "foo.cc")
		vol.content.deltaView().upsert(contentDeltaDoc{FRN: 999, Text: []byte("Foo")})
		assertCursorKeyOrder(t, vol, "content:Foo", files)
	})
}

func assertCursorKeyOrder(t *testing.T, vol *serviceVolumeIndex, query string, files []contentFixtureFile) {
	t.Helper()
	pq, err := parseQuery(queryOptions{Query: query, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	base := make([]Entry, len(files))
	for i, f := range files {
		path := `C:\` + f.name
		base[i] = Entry{
			Path:      path,
			Name:      f.name,
			LowerPath: strings.ToLower(path),
			LowerName: strings.ToLower(f.name),
			FRN:       f.frn,
		}
	}
	sort.SliceStable(base, func(i, j int) bool {
		a, b := vol.contentCursorBaseKey(base[i], pq), vol.contentCursorBaseKey(base[j], pq)
		if n := a.compare(b); n != 0 {
			return n < 0
		}
		return base[i].FRN < base[j].FRN
	})
	overlay := vol.overlayRankedMatches(vol.snap.Load(), pq, map[int]string{})
	merged := vol.mergeContentOverlayMatches(append([]Entry(nil), base...), overlay, pq, nil)

	type row struct {
		entry Entry
		key   contentCursorKey
	}
	rows := make([]row, 0, len(base)+len(overlay))
	for _, entry := range base {
		rows = append(rows, row{entry: entry, key: vol.contentCursorBaseKey(entry, pq)})
	}
	for _, item := range overlay {
		rows = append(rows, row{entry: item.entry, key: vol.contentCursorOverlayKey(item, pq)})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].key.compare(rows[j].key) < 0
	})
	if len(merged) != len(rows) {
		t.Fatalf("merged rows = %d; key rows = %d", len(merged), len(rows))
	}
	for i := range merged {
		if merged[i].Path != rows[i].entry.Path {
			t.Fatalf("row %d = %q; key order = %q (merged %v)", i, merged[i].Path, rows[i].entry.Path, contentCursorBaseNames(entryPaths(merged)))
		}
	}
}
