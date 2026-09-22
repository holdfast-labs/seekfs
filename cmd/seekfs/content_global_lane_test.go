package main

// WP7 differential: the compound content lane (filename-driven, content verified
// inline) must return the same entries, order and count as the per-volume
// content path for every in-scope query shape, single and multi volume. The lane
// is a cost optimization; correctness is by construction (the filename selector
// and the content leaves are both requirements), and this test proves it against
// the content path.

import (
	"fmt"
	"os"
	"testing"
)

func contentGlobalLaneRecords(volume string) []contentVolRecord {
	dirs := []string{"alpha", "beta"}
	exts := []string{".txt", ".md", ".go"}
	recs := make([]contentVolRecord, 0, 60)
	for i := 0; i < 60; i++ {
		name := fmt.Sprintf("doc-%03d%s", i, exts[i%3])
		var body string
		switch {
		case i%5 == 0:
			body = "the quick brown fox needle pelican synced the remote cache"
		case i%3 == 0:
			body = "needle in a haystack"
		default:
			body = "nothing to see here"
		}
		recs = append(recs, contentVolRecord{
			frn:       uint64(1000 + i),
			parent:    -1,
			parentFRN: 1,
			name:      name,
			size:      int64(i) + 1,
			modUnix:   int64(1_600_000_000 + i),
			path:      volume + `\` + dirs[i%2] + `\` + name,
			content:   body,
		})
	}
	return recs
}

// contentGlobalLaneSearch runs one search with the lane forced on or off and
// returns the entries (nil on error) and the trace.
func contentGlobalLaneSearch(vols []*serviceVolumeIndex, query string, limit int, enabled bool) ([]Entry, *searchTrace) {
	if enabled {
		os.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "1")
	} else {
		os.Unsetenv("SEEKFS_CONTENT_GLOBAL_LANE")
	}
	trace := &searchTrace{}
	matches, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: limit, Trace: trace}, false)
	if err != nil {
		return nil, trace
	}
	return matches, trace
}

func contentGlobalLaneCount(vols []*serviceVolumeIndex, query string, enabled bool) (int, *searchTrace) {
	if enabled {
		os.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "1")
	} else {
		os.Unsetenv("SEEKFS_CONTENT_GLOBAL_LANE")
	}
	trace := &searchTrace{}
	n, ok, err := countServiceVolumes(vols, queryOptions{Query: query, Trace: trace})
	if err != nil || !ok {
		return -1, trace
	}
	return n, trace
}

func contentGlobalLaneScopes(t *testing.T) []struct {
	name string
	vols []*serviceVolumeIndex
} {
	t.Helper()
	volC := newContentRecordVolume(t, "C:", contentGlobalLaneRecords("C:"))
	volF := newContentRecordVolume(t, "F:", contentGlobalLaneRecords("F:"))
	return []struct {
		name string
		vols []*serviceVolumeIndex
	}{
		{"single", []*serviceVolumeIndex{volC}},
		{"multi", []*serviceVolumeIndex{volC, volF}},
	}
}

func TestContentGlobalLaneMatchesContentPath(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	engaged := []string{
		"content:needle ext:.go",
		"content:pelican ext:.txt",
		`content:"quick brown fox" ext:.txt`,
		"content:needle ext:.go sort:size",
		"content:needle ext:.go sort:path",
		"content:needle ext:.go size:>1",
	}
	// Shapes outside the lane's scope must decline to the content path and still
	// return the content path's result.
	declined := []string{
		"content:needle",
		"content:needle ext:.go sort:relevance",
		"content:needle|dir:alpha",
	}
	for _, scope := range contentGlobalLaneScopes(t) {
		for _, wantEngaged := range []bool{true, false} {
			queries := declined
			if wantEngaged {
				queries = engaged
			}
			for _, q := range queries {
				for _, limit := range []int{5, 100} {
					t.Run(fmt.Sprintf("%s/engaged=%v/%s/limit=%d", scope.name, wantEngaged, q, limit), func(t *testing.T) {
						want, _ := contentGlobalLaneSearch(scope.vols, q, limit, false)
						got, laneTrace := contentGlobalLaneSearch(scope.vols, q, limit, true)
						didEngage := laneTrace.PlannerMode == "global-content-components"
						if didEngage != wantEngaged {
							t.Fatalf("%q: engaged=%v want %v (planner=%q)", q, didEngage, wantEngaged, laneTrace.PlannerMode)
						}
						if len(got) != len(want) {
							t.Fatalf("%q: count diverged lane=%d content=%d", q, len(got), len(want))
						}
						for i := range want {
							if got[i].Path != want[i].Path {
								t.Fatalf("%q: element %d lane=%s content=%s", q, i, got[i].Path, want[i].Path)
							}
						}
					})
				}
			}
		}
	}
}

func TestContentGlobalLaneCountMatchesContentPath(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	engaged := []string{
		"content:needle ext:.go",
		"content:pelican ext:.txt",
		"content:needle ext:.go size:>1",
	}
	declined := []string{"content:needle", "content:needle|dir:alpha"}
	for _, scope := range contentGlobalLaneScopes(t) {
		for _, q := range engaged {
			t.Run(scope.name+"/engaged/"+q, func(t *testing.T) {
				want, _ := contentGlobalLaneCount(scope.vols, q, false)
				got, trace := contentGlobalLaneCount(scope.vols, q, true)
				if trace.PlannerMode != "global-content-components" {
					t.Fatalf("%q: count lane did not engage (planner=%q)", q, trace.PlannerMode)
				}
				if got != want {
					t.Fatalf("%q: count lane=%d content=%d", q, got, want)
				}
				matches, _ := contentGlobalLaneSearch(scope.vols, q, 1000, true)
				if got != len(matches) {
					t.Fatalf("%q: count %d != len(search) %d", q, got, len(matches))
				}
			})
		}
		for _, q := range declined {
			t.Run(scope.name+"/declined/"+q, func(t *testing.T) {
				want, _ := contentGlobalLaneCount(scope.vols, q, false)
				got, trace := contentGlobalLaneCount(scope.vols, q, true)
				if trace.PlannerMode == "global-content-components" {
					t.Fatalf("%q: count lane engaged a declined shape", q)
				}
				if got != want {
					t.Fatalf("%q: count diverged lane=%d content=%d", q, got, want)
				}
			})
		}
	}
}

// A content query with no filename selector stays on the content path; the lane
// must decline it rather than answer it from a match-all filename iterator.
func TestContentGlobalLaneDeclinesWithoutFilenameRoot(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	t.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "1")
	for _, scope := range contentGlobalLaneScopes(t) {
		trace := &searchTrace{}
		if _, err := searchServiceVolumes(scope.vols, queryOptions{Query: "content:needle", Limit: 10, Trace: trace}, false); err != nil {
			t.Fatalf("content query: %v", err)
		}
		if trace.PlannerMode == "global-content-components" {
			t.Fatalf("%s: lane engaged a content query with no filename selector", scope.name)
		}
	}
}

// A truncated catch-up leaves the volume usable but incomplete; the lane must
// decline so the content path still marks the search incomplete and refuses the
// count (errContentIncomplete).
func TestContentGlobalLaneDeclinesWhenContentIncomplete(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentRecordVolume(t, "C:", contentGlobalLaneRecords("C:"))
	vol.content.markCatchUpIncomplete()
	const q = "content:needle ext:.go"

	want, _ := contentGlobalLaneSearch([]*serviceVolumeIndex{vol}, q, 100, false)
	got, searchTrace := contentGlobalLaneSearch([]*serviceVolumeIndex{vol}, q, 100, true)
	if searchTrace.PlannerMode == "global-content-components" {
		t.Fatal("search lane engaged an incomplete content volume")
	}
	if len(got) != len(want) {
		t.Fatalf("%q: search diverged lane=%d content=%d", q, len(got), len(want))
	}

	wantCount, _ := contentGlobalLaneCount([]*serviceVolumeIndex{vol}, q, false)
	gotCount, countTrace := contentGlobalLaneCount([]*serviceVolumeIndex{vol}, q, true)
	if countTrace.PlannerMode == "global-content-components" {
		t.Fatal("count lane engaged an incomplete content volume")
	}
	if gotCount != wantCount {
		t.Fatalf("%q: count diverged lane=%d content=%d", q, gotCount, wantCount)
	}
	if wantCount != -1 {
		t.Fatalf("%q: incomplete content count was answered (%d); want refused (-1)", q, wantCount)
	}
}

// M8: a volume without usable content must not block a content query. The lane
// answers from the usable volume, matching the content path, and surfaces the
// skipped volume as partial.
func TestContentGlobalLaneMultiVolumeSkipsUnusable(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	volC := newContentRecordVolume(t, "C:", contentGlobalLaneRecords("C:"))
	volF := newContentRecordVolume(t, "F:", contentGlobalLaneRecords("F:"))
	volF.content = nil // F: has no usable content
	vols := []*serviceVolumeIndex{volC, volF}
	const q = "content:needle ext:.go"

	want, _ := contentGlobalLaneSearch(vols, q, 100, false)
	got, laneTrace := contentGlobalLaneSearch(vols, q, 100, true)
	if laneTrace.PlannerMode != "global-content-components" {
		t.Fatalf("lane did not engage (planner=%q)", laneTrace.PlannerMode)
	}
	if len(got) != len(want) {
		t.Fatalf("count diverged lane=%d content=%d", len(got), len(want))
	}
	for i := range want {
		if got[i].Path != want[i].Path {
			t.Fatalf("element %d lane=%s content=%s", i, got[i].Path, want[i].Path)
		}
	}
	if !laneTrace.ContentPartial {
		t.Fatal("skipped volume not surfaced as partial")
	}
}
