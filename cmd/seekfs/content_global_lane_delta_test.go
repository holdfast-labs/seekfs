package main

import (
	"os"
	"testing"
)

// globalLaneDeltaVolume builds a base content index with one directory and three
// matching files. Each subtest applies a content delta and/or a filename overlay
// change to one of them, then drives the global content lane.
func globalLaneDeltaVolume(t *testing.T) *serviceVolumeIndex {
	t.Helper()
	recs := []contentVolRecord{
		{frn: 1, parent: -1, parentFRN: 0, name: "alpha", mode: uint32(os.ModeDir), path: `C:\alpha`},
		{frn: 100, parent: 0, parentFRN: 1, name: "changed.txt", path: `C:\alpha\changed.txt`, content: "needle original"},
		{frn: 101, parent: 0, parentFRN: 1, name: "deleted.txt", path: `C:\alpha\deleted.txt`, content: "needle doomed"},
		{frn: 102, parent: 0, parentFRN: 1, name: "renamed.txt", path: `C:\alpha\renamed.txt`, content: "needle renamed"},
		{frn: 103, parent: 0, parentFRN: 1, name: "stops.txt", path: `C:\alpha\stops.txt`, content: "needle to be removed"},
	}
	return contentLaneScopeVolume(t, "C:", recs)
}

func globalLanePathCount(paths []string, want string) int {
	n := 0
	for _, p := range paths {
		if p == want {
			n++
		}
	}
	return n
}

// assertGlobalLaneSearchCount runs the lane search and count for q and checks
// the path multiplicity plus count == distinct rows.
func assertGlobalLaneSearchCount(t *testing.T, vol *serviceVolumeIndex, q string, wantPaths map[string]int) []Entry {
	t.Helper()
	matches, trace := contentGlobalLaneSearch([]*serviceVolumeIndex{vol}, q, 100, true)
	if trace.PlannerMode != "global-content-components" {
		t.Fatalf("%q: lane not engaged: %q", q, trace.PlannerMode)
	}
	paths := entryPathsInOrder(matches)
	for path, want := range wantPaths {
		if got := globalLanePathCount(paths, path); got != want {
			t.Fatalf("%q: %s appears %d times, want %d: %v", q, path, got, want, paths)
		}
	}
	n, countTrace := contentGlobalLaneCount([]*serviceVolumeIndex{vol}, q, true)
	if countTrace.PlannerMode != "global-content-components" {
		t.Fatalf("%q: count lane not engaged: %q", q, countTrace.PlannerMode)
	}
	if n != len(matches) {
		t.Fatalf("%q: count=%d but distinct search rows=%d: %v", q, n, len(matches), paths)
	}
	return matches
}

func TestGlobalLaneDeltaChangedStillMatching(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := globalLaneDeltaVolume(t)
	vol.applyUSNChanges([]usnChange{{FRN: 100, ParentFRN: 1, USN: 10, Reason: usnReasonDataOverwrite}})
	vol.content.deltaView().upsert(contentDeltaDoc{FRN: 100, Path: `C:\alpha\changed.txt`, Text: []byte("needle updated still matching")})
	assertGlobalLaneSearchCount(t, vol, "content:needle dir:alpha", map[string]int{`C:\alpha\changed.txt`: 1})
}

func TestGlobalLaneDeltaChangedNoLongerMatching(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := globalLaneDeltaVolume(t)
	vol.applyUSNChanges([]usnChange{{FRN: 103, ParentFRN: 1, USN: 11, Reason: usnReasonDataOverwrite}})
	vol.content.deltaView().upsert(contentDeltaDoc{FRN: 103, Path: `C:\alpha\stops.txt`, Text: []byte("nothing relevant remains")})
	assertGlobalLaneSearchCount(t, vol, "content:needle dir:alpha", map[string]int{`C:\alpha\stops.txt`: 0})
}

func TestGlobalLaneDeltaDeleted(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := globalLaneDeltaVolume(t)
	vol.applyUSNChanges([]usnChange{{FRN: 101, ParentFRN: 1, USN: 12, Reason: usnReasonFileDelete}})
	assertGlobalLaneSearchCount(t, vol, "content:needle dir:alpha", map[string]int{`C:\alpha\deleted.txt`: 0})
}

func TestGlobalLaneDeltaRenamed(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := globalLaneDeltaVolume(t)
	vol.applyUSNChanges([]usnChange{
		{FRN: 102, ParentFRN: 1, USN: 13, Reason: usnReasonRenameOld, Name: "renamed.txt"},
		{FRN: 102, ParentFRN: 1, USN: 14, Reason: usnReasonRenameNew, Name: "new-name.txt"},
	})
	vol.content.deltaView().upsert(contentDeltaDoc{FRN: 102, Path: `C:\alpha\new-name.txt`, Text: []byte("needle renamed still")})
	assertGlobalLaneSearchCount(t, vol, "content:needle dir:alpha", map[string]int{
		`C:\alpha\new-name.txt`: 1,
		`C:\alpha\renamed.txt`:  0,
	})
}

// A base index written by an older fold can carry a stale and a replacement
// record with the same FRN. The lane must return the file once under the
// replacement (newer) row, and the count must match.
func TestGlobalLaneDeltaDuplicateFRNBase(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	recs := []contentVolRecord{
		{frn: 1, parent: -1, parentFRN: 0, name: "alpha", mode: uint32(os.ModeDir), path: `C:\alpha`},
		{frn: 100, parent: 0, parentFRN: 1, name: "changed.txt", size: 10, modUnix: 100, path: `C:\alpha\changed.txt`, content: "needle stale base"},
		{frn: 100, parent: 0, parentFRN: 1, name: "changed.txt", size: 20, modUnix: 200, path: `C:\alpha\changed.txt`, content: "needle replacement base"},
	}
	vol := contentLaneScopeVolume(t, "C:", recs)
	vol.content.deltaView().upsert(contentDeltaDoc{FRN: 100, Path: `C:\alpha\changed.txt`, Text: []byte("needle delta still matches")})
	matches := assertGlobalLaneSearchCount(t, vol, "content:needle dir:alpha", map[string]int{`C:\alpha\changed.txt`: 1})
	if got := matches[0].Size; got != 20 {
		t.Fatalf("duplicate FRN kept size %d, want the replacement size 20", got)
	}
}

// The global name lane shares the FRN identity rule: a duplicate base record
// must not produce two rows for one path.
func TestGlobalNameLaneDuplicateFRNBase(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	recs := []contentVolRecord{
		{frn: 1, parent: -1, parentFRN: 0, name: "alpha", mode: uint32(os.ModeDir), path: `C:\alpha`},
		{frn: 100, parent: 0, parentFRN: 1, name: "changed.txt", size: 10, modUnix: 100, path: `C:\alpha\changed.txt`, content: "needle stale base"},
		{frn: 100, parent: 0, parentFRN: 1, name: "changed.txt", size: 20, modUnix: 200, path: `C:\alpha\changed.txt`, content: "needle replacement base"},
	}
	vol := contentLaneScopeVolume(t, "C:", recs)
	matches, trace := contentGlobalLaneSearch([]*serviceVolumeIndex{vol}, "changed.txt", 100, true)
	if trace.PlannerMode != "global-name" {
		t.Fatalf("name lane not engaged: %q", trace.PlannerMode)
	}
	paths := entryPathsInOrder(matches)
	if got := globalLanePathCount(paths, `C:\alpha\changed.txt`); got != 1 {
		t.Fatalf("duplicate base FRN appears %d times in the name lane: %v", got, paths)
	}
	if matches[0].Size != 20 {
		t.Fatalf("name lane kept size %d, want 20", matches[0].Size)
	}
}
