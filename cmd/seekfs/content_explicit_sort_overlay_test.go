package main

import (
	"reflect"
	"sort"
	"testing"
)

func explicitSortOverlayRun(t *testing.T, vols []*serviceVolumeIndex, opts queryOptions, lane bool) ([]Entry, *searchTrace) {
	t.Helper()
	if lane {
		t.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "1")
	} else {
		t.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "0")
	}
	trace := &searchTrace{}
	opts.Trace = trace
	matches, err := searchServiceVolumes(vols, opts, false)
	if err != nil {
		t.Fatalf("search lane=%v: %v", lane, err)
	}
	return matches, trace
}

func explicitSortComparatorOrder(entries []Entry, pq parsedQuery) []Entry {
	out := append([]Entry(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool {
		return compareSearchAllEntries(out[i], out[j], pq) < 0
	})
	return out
}

func assertEntryOrder(t *testing.T, label string, got, want []Entry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: count %d != %d", label, len(got), len(want))
	}
	for i := range want {
		if got[i].Path != want[i].Path {
			t.Fatalf("%s: element %d diverged: got=%q want=%q", label, i, got[i].Path, want[i].Path)
		}
	}
}

func TestContentExplicitSortOverlayComparatorOrder(t *testing.T) {
	uncappedDiskVerify(t)
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	recs := contentLaneScopeCorpus("C:", 7, 600)
	vol := contentLaneScopeVolume(t, "C:", recs)
	contentLaneScopeMutations(t, vol, recs, "add-match")
	for _, sortCol := range []string{"sort:extension", "sort:type"} {
		t.Run(sortCol, func(t *testing.T) {
			query := "content:download ext:.txt " + sortCol
			pq, err := parseQuery(queryOptions{Query: query, Limit: 5000})
			if err != nil {
				t.Fatal(err)
			}
			opts := queryOptions{Query: query, Limit: 5000}
			slow, _ := explicitSortOverlayRun(t, []*serviceVolumeIndex{vol}, opts, false)
			lane, _ := explicitSortOverlayRun(t, []*serviceVolumeIndex{vol}, opts, true)
			want := explicitSortComparatorOrder(slow, pq)
			assertEntryOrder(t, "slow vs comparator", slow, want)
			assertEntryOrder(t, "lane vs comparator", lane, want)
			assertEntryOrder(t, "lane vs slow", lane, slow)
		})
	}
}

func TestContentExplicitSortSizeOverlayComparatorOrder(t *testing.T) {
	uncappedDiskVerify(t)
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentRecordVolume(t, "C:", []contentVolRecord{
		{frn: 2, parent: -1, parentFRN: 1, name: "b.txt", size: 10, content: "needle"},
		{frn: 4, parent: -1, parentFRN: 1, name: "d.txt", size: 10, content: "needle"},
		{frn: 6, parent: -1, parentFRN: 1, name: "f.txt", size: 10, content: "needle"},
		{frn: 8, parent: -1, parentFRN: 1, name: "h.txt", size: 10, content: "needle"},
		{frn: 10, parent: -1, parentFRN: 1, name: "r1.txt", size: 10, content: "needle"},
		{frn: 12, parent: -1, parentFRN: 1, name: "r2.txt", size: 10, content: "needle"},
		{frn: 14, parent: -1, parentFRN: 1, name: "r3.txt", size: 10, content: "needle"},
		{frn: 16, parent: -1, parentFRN: 1, name: "r4.txt", size: 10, content: "needle"},
	})
	vol.rebuildNameTrigramsLocked()
	vol.applyUSNChanges([]usnChange{
		{FRN: 10, ParentFRN: 1, USN: 1000, Reason: usnReasonRenameNew, Name: "a.txt"},
		{FRN: 12, ParentFRN: 1, USN: 1001, Reason: usnReasonRenameNew, Name: "c.txt"},
		{FRN: 14, ParentFRN: 1, USN: 1002, Reason: usnReasonRenameNew, Name: "e.txt"},
		{FRN: 16, ParentFRN: 1, USN: 1003, Reason: usnReasonRenameNew, Name: "g.txt"},
	})
	delta := vol.content.deltaView()
	for _, frn := range []uint64{10, 12, 14, 16} {
		delta.upsert(contentDeltaDoc{FRN: frn, Text: []byte("needle")})
	}

	const query = "content:needle ext:.txt sort:size"
	pq, err := parseQuery(queryOptions{Query: query, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	opts := queryOptions{Query: query, Limit: 100}
	slow, _ := explicitSortOverlayRun(t, []*serviceVolumeIndex{vol}, opts, false)
	lane, _ := explicitSortOverlayRun(t, []*serviceVolumeIndex{vol}, opts, true)
	want := explicitSortComparatorOrder(slow, pq)
	assertEntryOrder(t, "slow vs comparator", slow, want)
	assertEntryOrder(t, "lane vs comparator", lane, want)
	assertEntryOrder(t, "lane vs slow", lane, slow)
	names := make([]string, len(want))
	for i := range want {
		names[i] = want[i].Name
	}
	if !reflect.DeepEqual(names, []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt", "f.txt", "g.txt", "h.txt"}) {
		t.Fatalf("comparator order = %v", names)
	}
}

func explicitSortCursorPages(t *testing.T, s *goSearchService, query string, limit int) []string {
	t.Helper()
	var paths []string
	cursor := ""
	for page := 0; page < 32; page++ {
		r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: limit, Cursor: cursor})
		if !r.OK {
			t.Fatalf("%s page %d: %s", query, page, r.Message)
		}
		if r.CursorRestarted {
			t.Fatalf("%s page %d restarted: %s", query, page, r.CursorRestartReason)
		}
		paths = append(paths, r.Results...)
		if r.NextCursor == "" {
			return paths
		}
		cursor = r.NextCursor
	}
	t.Fatalf("%s did not terminate", query)
	return nil
}

func assertNoDuplicateExplicitPaths(t *testing.T, label string, paths []string) {
	t.Helper()
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if _, ok := seen[path]; ok {
			t.Fatalf("%s: duplicate path %q in %v", label, path, paths)
		}
		seen[path] = struct{}{}
	}
}

func TestContentCursorV2ExplicitSortOverlayPagesMatchFull(t *testing.T) {
	t.Run("extension", func(t *testing.T) {
		vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
			{2, "m10.txt", "needle"},
			{4, "m30.txt", "needle"},
			{6, "m50.txt", "needle"},
			{8, "m70.txt", "needle"},
		})
		addContentOverlay(t, vol, 3, "m20.txt")
		addContentOverlay(t, vol, 5, "m60.txt")
		const query = "content:needle ext:.txt sort:extension"
		paged := explicitSortCursorPages(t, s, query, 2)
		full := contentCursorFull(t, s, query)
		assertNoDuplicateExplicitPaths(t, query, paged)
		if !reflect.DeepEqual(paged, full) {
			t.Fatalf("paged %v != full %v", paged, full)
		}
		want := []string{"m10.txt", "m20.txt", "m30.txt", "m50.txt", "m60.txt", "m70.txt"}
		names := contentCursorBaseNames(full)
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("full order = %v; want %v", names, want)
		}
	})

	t.Run("size", func(t *testing.T) {
		vol, s := contentCursorSearchVolume(t, []contentFixtureFile{
			{2, "b.txt", "needle"},
			{4, "d.txt", "needle"},
			{6, "f.txt", "needle"},
			{8, "h.txt", "needle"},
			{10, "r1.txt", "needle"},
			{12, "r2.txt", "needle"},
			{14, "r3.txt", "needle"},
			{16, "r4.txt", "needle"},
		})
		vol.applyUSNChanges([]usnChange{
			{FRN: 10, ParentFRN: 1, USN: 1000, Reason: usnReasonRenameNew, Name: "a.txt"},
			{FRN: 12, ParentFRN: 1, USN: 1001, Reason: usnReasonRenameNew, Name: "c.txt"},
			{FRN: 14, ParentFRN: 1, USN: 1002, Reason: usnReasonRenameNew, Name: "e.txt"},
			{FRN: 16, ParentFRN: 1, USN: 1003, Reason: usnReasonRenameNew, Name: "g.txt"},
		})
		delta := vol.content.deltaView()
		for _, frn := range []uint64{10, 12, 14, 16} {
			delta.upsert(contentDeltaDoc{FRN: frn, Text: []byte("needle")})
		}
		const query = "content:needle ext:.txt sort:size"
		paged := explicitSortCursorPages(t, s, query, 2)
		full := contentCursorFull(t, s, query)
		assertNoDuplicateExplicitPaths(t, query, paged)
		if !reflect.DeepEqual(paged, full) {
			t.Fatalf("paged %v != full %v", paged, full)
		}
		want := []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt", "f.txt", "g.txt", "h.txt"}
		names := contentCursorBaseNames(full)
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("full order = %v; want %v", names, want)
		}
	})
}
