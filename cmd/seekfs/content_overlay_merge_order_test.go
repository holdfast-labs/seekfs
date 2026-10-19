package main

import (
	"path/filepath"
	"reflect"
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
