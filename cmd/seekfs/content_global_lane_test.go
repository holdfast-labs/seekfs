package main

// WP7 phase-1 differential: the compound content lane (filename-driven, content
// verified inline) must return the same entries and order as the per-volume
// content path for every in-scope query shape. The lane is a cost optimization;
// correctness is by construction (the filename selector and the content leaves
// are both requirements), and this test proves it against the content path.

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

// contentGlobalLaneSearch runs one query with the lane forced on or off and
// returns the entries (nil on error) and the trace.
func contentGlobalLaneSearch(vol *serviceVolumeIndex, query string, limit int, enabled bool) ([]Entry, *searchTrace) {
	if enabled {
		os.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "1")
	} else {
		os.Unsetenv("SEEKFS_CONTENT_GLOBAL_LANE")
	}
	trace := &searchTrace{}
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: query, Limit: limit, Trace: trace}, false)
	if err != nil {
		return nil, trace
	}
	return matches, trace
}

func TestContentGlobalLaneMatchesContentPath(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentRecordVolume(t, "C:", contentGlobalLaneRecords("C:"))
	engaged := []string{
		"content:needle ext:.go",
		"content:pelican ext:.txt",
		`content:"quick brown fox" ext:.txt`,
		"content:needle ext:.go sort:size",
		"content:needle ext:.go sort:path",
		"content:needle ext:.go size:>1",
	}
	// Shapes outside phase-1 scope must decline to the content path and still
	// return the content path's result.
	declined := []string{
		"content:needle",
		"content:needle ext:.go sort:relevance",
		"content:needle|dir:alpha",
	}
	for _, wantEngaged := range []bool{true, false} {
		queries := declined
		if wantEngaged {
			queries = engaged
		}
		for _, q := range queries {
			for _, limit := range []int{5, 100} {
				t.Run(fmt.Sprintf("engaged=%v/%s/limit=%d", wantEngaged, q, limit), func(t *testing.T) {
					want, _ := contentGlobalLaneSearch(vol, q, limit, false)
					got, laneTrace := contentGlobalLaneSearch(vol, q, limit, true)
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

// A content query with no filename selector stays on the content path; the lane
// must decline it rather than answer it from a match-all filename iterator.
func TestContentGlobalLaneDeclinesWithoutFilenameRoot(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	t.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "1")
	vol := newContentRecordVolume(t, "C:", contentGlobalLaneRecords("C:"))
	trace := &searchTrace{}
	if _, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:needle", Limit: 10, Trace: trace}, false); err != nil {
		t.Fatalf("content query: %v", err)
	}
	if trace.PlannerMode == "global-content-components" {
		t.Fatal("lane engaged a content query with no filename selector")
	}
}
