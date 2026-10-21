package main

import (
	"fmt"
	"os"
	"reflect"
	"testing"
)

// extSortedFixture builds a single-volume usn index with many same-extension
// records: duplicate names across directories (tie coverage), mixed
// sizes/mtimes, a directory carrying an extension-like name, a deleted
// record, and non-matching records.
func extSortedFixture(t *testing.T) (*Index, *serviceVolumeIndex) {
	t.Helper()
	idx := &Index{Source: "usn", Volume: "C:", Compact: true}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32, size int64, mod int64, deleted bool) {
		idx.Records = append(idx.Records, CompactRecord{
			FRN: frn, ParentFRN: parentFRN, Parent: parent,
			Name: name, Mode: mode, Size: size, ModUnix: mod, Deleted: deleted,
		})
	}
	add(1, 1, -1, ".", uint32(os.ModeDir), 0, 1, false)
	add(2, 1, 0, "docs", uint32(os.ModeDir), 0, 2, false)
	add(3, 1, 0, "adir.txt", uint32(os.ModeDir), 0, 3, false)
	for i := 0; i < 30; i++ {
		parent := int32(0)
		if i%3 == 0 {
			parent = 1
		}
		add(uint64(10+i), 1, parent, fmt.Sprintf("f-%02d.txt", i), 0, int64(1000-i*7), int64(100+i), false)
	}
	add(100, 1, 0, "dup.txt", 0, 42, 50, false)
	add(101, 2, 1, "dup.txt", 0, 43, 51, false)
	add(102, 1, 0, "UP.TXT", 0, 44, 52, false)
	add(103, 1, 0, "gone.txt", 0, 45, 53, true)
	add(104, 1, 0, "other.md", 0, 46, 54, false)
	add(105, 1, 0, "noext", 0, 47, 55, false)
	buildOrders(idx)
	vol := newServiceVolumeIndex("ext-sorted-topn.gsi", idx)
	return idx, vol
}

// TestGlobalExtSortedTopNMatchesDirectSearch proves the record-level top-N
// fast path for sorted ext: queries returns exactly what the exhaustive
// direct search returns, across sort columns, filters, and limits, and that
// the fast path (not the materialize-all fallback) serves the fixture.
func TestGlobalExtSortedTopNMatchesDirectSearch(t *testing.T) {
	idx, vol := extSortedFixture(t)
	volumes := []*serviceVolumeIndex{vol}
	queries := []string{
		"ext:txt sort:extension",
		"ext:txt sort:size",
		"ext:txt sort:modified",
		"ext:txt sort:type",
		"ext:txt sort:path",
		"ext:txt type:file sort:extension",
		"ext:txt type:dir sort:extension",
		// NOTE: ext:+glob: together is not an ext-only query
		// (globalExtPostingFilters declines it to the planned lane), so it
		// stays out of this lane-specific test.
		"ext:zzz sort:extension",
	}
	limits := []int{1, 3, 7, 50}
	for _, q := range queries {
		for _, limit := range limits {
			opts := queryOptions{Query: q, Limit: limit, Trace: &searchTrace{}}
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatalf("query %q limit %d: %v", q, limit, err)
			}
			if opts.Trace.PlannerMode != "global-ext" || opts.Trace.Source != "global:ext:txt" && opts.Trace.Source != "global:ext:zzz" {
				t.Fatalf("query %q limit %d: planner=%q source=%q, want global-ext lane",
					q, limit, opts.Trace.PlannerMode, opts.Trace.Source)
			}
			want, err := search(idx, queryOptions{Query: q, Limit: limit}, false)
			if err != nil {
				t.Fatalf("query %q limit %d direct: %v", q, limit, err)
			}
			if !reflect.DeepEqual(pathsOf(got), pathsOf(want)) {
				t.Fatalf("query %q limit %d:\n got=%v\nwant=%v", q, limit, pathsOf(got), pathsOf(want))
			}
			// The fast path itself must claim the query and agree.
			pq, err := parseQuery(opts)
			if err != nil {
				t.Fatal(err)
			}
			filters, ok := globalExtPostingFilters(pq)
			if !ok || len(filters) != 1 {
				t.Fatalf("query %q: no single ext filter", q)
			}
			snapshot := newGlobalQuerySnapshot(volumes, &searchTrace{})
			fast, ok := globalExtSortedTopN(snapshot, pq, filters[0], limit, &searchTrace{})
			if !ok {
				t.Fatalf("query %q limit %d: fast path declined the fixture", q, limit)
			}
			if !reflect.DeepEqual(pathsOf(fast), pathsOf(want)) {
				t.Fatalf("query %q limit %d fast-path:\n got=%v\nwant=%v", q, limit, pathsOf(fast), pathsOf(want))
			}
		}
	}
}
