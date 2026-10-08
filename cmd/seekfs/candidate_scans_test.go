package main

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

func plainTermPrefilterVolumes(t *testing.T) []*serviceVolumeIndex {
	t.Helper()
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	a := dottedPathBenchmarkIndex(2000)
	b := dottedPathBenchmarkIndex(500)
	b.Volume = "E:"
	va := newServiceVolumeIndex("prefilter-a.gsi", a)
	va.rebuildNameTrigramsLocked()
	vb := newServiceVolumeIndex("prefilter-b.gsi", b)
	vb.rebuildNameTrigramsLocked()
	return []*serviceVolumeIndex{va, vb}
}

func mustParsePrefilterQuery(t *testing.T, opts queryOptions) parsedQuery {
	t.Helper()
	pq, err := parseQuery(opts)
	if err != nil {
		t.Fatal(err)
	}
	return pq
}

func TestBoundedScanPrefilterPlainTerm(t *testing.T) {
	volumes := plainTermPrefilterVolumes(t)
	vol := volumes[0]

	t.Run("no-hit-term-proves-empty", func(t *testing.T) {
		pq := mustParsePrefilterQuery(t, queryOptions{Query: "zzzz-no-hit-seekfs", MatchPath: true, Limit: 20})
		var filter *boundedScanMembershipFilter
		exactEmpty, filterOK := vol.boundedScanPrefilter(pq, &filter)
		if !exactEmpty || !filterOK {
			t.Fatalf("prefilter = (%v, %v), want (true, true)", exactEmpty, filterOK)
		}
	})

	t.Run("multi-term-with-no-hit-proves-empty", func(t *testing.T) {
		pq := mustParsePrefilterQuery(t, queryOptions{Query: "trainingdata zzzz-no-hit-seekfs", MatchPath: true, Limit: 20})
		var filter *boundedScanMembershipFilter
		exactEmpty, filterOK := vol.boundedScanPrefilter(pq, &filter)
		if !exactEmpty || !filterOK {
			t.Fatalf("prefilter = (%v, %v), want (true, true)", exactEmpty, filterOK)
		}
	})

	t.Run("hit-term-builds-superset-filter", func(t *testing.T) {
		pq := mustParsePrefilterQuery(t, queryOptions{Query: "trainingdata", MatchPath: true, Limit: 20})
		var filter *boundedScanMembershipFilter
		exactEmpty, filterOK := vol.boundedScanPrefilter(pq, &filter)
		if exactEmpty || !filterOK || filter == nil {
			t.Fatalf("prefilter = (%v, %v, %v), want filter set", exactEmpty, filterOK, filter)
		}
		if len(filter.members) == 0 {
			t.Fatal("filter has no members for a term known to match")
		}
		for id := range filter.members {
			if !vol.index.compactPathContainsTerm(id, "trainingdata") {
				t.Fatalf("filter member %d does not contain the term", id)
			}
		}
	})

	t.Run("dotted-hit-term-builds-filter", func(t *testing.T) {
		pq := mustParsePrefilterQuery(t, queryOptions{Query: "sample-volume.nrrd", MatchPath: true, Limit: 20})
		var filter *boundedScanMembershipFilter
		exactEmpty, filterOK := vol.boundedScanPrefilter(pq, &filter)
		if exactEmpty || !filterOK || filter == nil || len(filter.members) == 0 {
			t.Fatalf("prefilter = (%v, %v), want filter set", exactEmpty, filterOK)
		}
	})

	t.Run("guards-decline", func(t *testing.T) {
		cases := []queryOptions{
			{Query: "zzzz-no-hit-seekfs", MatchPath: true, Limit: 20, CaseSensitive: true},
			{Query: "zzzz-no-hit-seekfs", MatchPath: true, Limit: 20, Fuzzy: true},
			{Query: "zzzz-no-hit-seekfs", MatchPath: false, Limit: 20},
			{Query: "ab", MatchPath: true, Limit: 20},
		}
		for _, opts := range cases {
			pq := mustParsePrefilterQuery(t, opts)
			var filter *boundedScanMembershipFilter
			if exactEmpty, filterOK := vol.boundedScanPrefilter(pq, &filter); exactEmpty || filterOK {
				t.Fatalf("query %+v: prefilter = (%v, %v), want decline", opts, exactEmpty, filterOK)
			}
		}
	})

	t.Run("overlay-additions-decline", func(t *testing.T) {
		vol.recentIDs = map[int]struct{}{1: {}}
		defer delete(vol.recentIDs, 1)
		pq := mustParsePrefilterQuery(t, queryOptions{Query: "zzzz-no-hit-seekfs", MatchPath: true, Limit: 20})
		var filter *boundedScanMembershipFilter
		if exactEmpty, filterOK := vol.boundedScanPrefilter(pq, &filter); exactEmpty || filterOK {
			t.Fatalf("prefilter with overlay = (%v, %v), want decline", exactEmpty, filterOK)
		}
	})
}

func TestPlainTermPrefilterGlobalFallbackParity(t *testing.T) {
	volumes := plainTermPrefilterVolumes(t)
	queries := []queryOptions{
		{Query: "zzzz-no-hit-seekfs", MatchPath: true, Limit: 20},
		{Query: "trainingdata zzzz-no-hit-seekfs", MatchPath: true, Limit: 20},
		{Query: "sample-volume.nrrd", MatchPath: true, Limit: 20},
		{Query: "trainingdata Dataset", MatchPath: true, Limit: 20},
		{Query: "nrrd-cache", MatchPath: true, Limit: 20},
		{Query: "trainingdata ext:nrrd", MatchPath: true, Limit: 20},
		{Query: "Downloads zzzz-nohit", MatchPath: true, Limit: 20},
		{Query: "path:node_modules commonishzz", Limit: 20},
	}
	for _, opts := range queries {
		t.Run(strings.ReplaceAll(opts.Query, " ", "_"), func(t *testing.T) {
			want, err := r5ExhaustivePlannerOracle(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			trace := &searchTrace{}
			searchOpts := opts
			searchOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, searchOpts, false)
			if err != nil {
				t.Fatalf("search: %v trace=%+v", err, *trace)
			}
			if !sameOrderedStrings(pathsOf(got), pathsOf(want)) {
				t.Fatalf("search paths=%v want=%v trace=%+v", pathsOf(got), pathsOf(want), *trace)
			}
			wantCount, err := r5ExhaustivePlannerOracle(volumes, opts, true)
			if err != nil {
				t.Fatal(err)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			gotCount, _, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatalf("count: %v trace=%+v", err, *countTrace)
			}
			if gotCount != len(wantCount) {
				t.Fatalf("count=%d want=%d trace=%+v", gotCount, len(wantCount), *countTrace)
			}
		})
	}
}

func TestPlainTermPrefilterNoHitSkipsScan(t *testing.T) {
	volumes := plainTermPrefilterVolumes(t)
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{
		Query:     "zzzz-no-hit-seekfs",
		MatchPath: true,
		Limit:     20,
		Trace:     trace,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d results, want none", len(got))
	}
	if trace.Source != "global:bounded-scan" {
		t.Fatalf("source = %q, want global:bounded-scan", trace.Source)
	}
	if trace.Candidates != 0 {
		t.Fatalf("candidates = %d, want 0 (scan skipped)", trace.Candidates)
	}
}

// overlayPrefilterFixture builds a tiny two-file volume for active-overlay
// prefilter tests: "base-keep.txt" survives, and
// "base-doomed-unique-zzzz.txt" is tombstoned via a USN delete below.
func overlayPrefilterFixture(t *testing.T, volume string) (*serviceVolumeIndex, uint64) {
	t.Helper()
	idx := &Index{Source: "usn", Volume: volume, Compact: true}
	idx.Records = append(idx.Records, CompactRecord{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)})
	idx.Records = append(idx.Records, CompactRecord{FRN: 2, ParentFRN: 1, Parent: 0, Name: "workspace", Mode: uint32(os.ModeDir)})
	idx.Records = append(idx.Records, CompactRecord{FRN: 3, ParentFRN: 2, Parent: 1, Name: "base-keep.txt"})
	idx.Records = append(idx.Records, CompactRecord{FRN: 4, ParentFRN: 2, Parent: 1, Name: "base-doomed-unique-zzzz.txt"})
	buildOrders(idx)
	vol := newServiceVolumeIndex("overlay-prefilter-"+volume+".gsi", idx)
	vol.rebuildNameTrigramsLocked()
	return vol, 4
}

// TestPlainTermPrefilterWithActiveOverlay proves the bounded-scan prefilter
// composes with the USN overlay: a term matching only an overlay-created
// record must still hit (base proof must not suppress the overlay merge),
// a term matching only a tombstoned base record must stay empty, and a term
// matching nothing anywhere must stay empty with count zero.
func TestPlainTermPrefilterWithActiveOverlay(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	volA, _ := overlayPrefilterFixture(t, "C:")
	volB, doomedFRN := overlayPrefilterFixture(t, "E:")
	volA.applyUSNChanges([]usnChange{{FRN: 500, ParentFRN: 2, USN: 10, Reason: usnReasonFileCreate, Name: "overlay-only-zzzz-term.txt"}})
	// Tombstone the doomed base record on both volumes: the term then
	// matches no live record anywhere.
	volA.applyUSNChanges([]usnChange{{FRN: doomedFRN, USN: 11, Reason: usnReasonFileDelete}})
	volB.applyUSNChanges([]usnChange{{FRN: doomedFRN, USN: 11, Reason: usnReasonFileDelete}})
	volumes := []*serviceVolumeIndex{volA, volB}

	t.Run("overlay-only-term-hits", func(t *testing.T) {
		got, err := searchServiceVolumes(volumes, queryOptions{Query: "overlay-only-zzzz-term", MatchPath: true, Limit: 20}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !strings.Contains(got[0].Path, "overlay-only-zzzz-term.txt") {
			t.Fatalf("got %v, want the overlay-created record", pathsOf(got))
		}
		n, _, err := countServiceVolumes(volumes, queryOptions{Query: "overlay-only-zzzz-term", MatchPath: true})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("count = %d, want 1", n)
		}
	})

	t.Run("tombstoned-term-stays-empty", func(t *testing.T) {
		got, err := searchServiceVolumes(volumes, queryOptions{Query: "base-doomed-unique-zzzz", MatchPath: true, Limit: 20}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("got %v, want none (tombstoned)", pathsOf(got))
		}
		n, _, err := countServiceVolumes(volumes, queryOptions{Query: "base-doomed-unique-zzzz", MatchPath: true})
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("count = %d, want 0", n)
		}
	})

	t.Run("nowhere-term-stays-empty", func(t *testing.T) {
		got, err := searchServiceVolumes(volumes, queryOptions{Query: "never-existed-zzzz", MatchPath: true, Limit: 20}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("got %v, want none", pathsOf(got))
		}
		n, _, err := countServiceVolumes(volumes, queryOptions{Query: "never-existed-zzzz", MatchPath: true})
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("count = %d, want 0", n)
		}
	})

	t.Run("unaffected-base-term-hits", func(t *testing.T) {
		got, err := searchServiceVolumes(volumes, queryOptions{Query: "base-keep", MatchPath: true, Limit: 20}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("got %v, want both volumes' base-keep.txt", pathsOf(got))
		}
	})

	t.Run("single-sided-tombstone-keeps-other-volume", func(t *testing.T) {
		t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
		volC, _ := overlayPrefilterFixture(t, "C:")
		volD, doomed := overlayPrefilterFixture(t, "E:")
		volD.applyUSNChanges([]usnChange{{FRN: doomed, USN: 11, Reason: usnReasonFileDelete}})
		got, err := searchServiceVolumes([]*serviceVolumeIndex{volC, volD}, queryOptions{Query: "base-doomed-unique-zzzz", MatchPath: true, Limit: 20}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !strings.Contains(got[0].Path, `C:\`) {
			t.Fatalf("got %v, want only the untombstoned C: copy", pathsOf(got))
		}
	})
}

// TestTypedExtLaneParity proves type:file/dir + ext: queries served by the
// ext lane match the exhaustive oracle for both search and count. The
// dottedPathBenchmarkIndex fixture contains a dotted directory name
// ("ai.opencode.desktop") so type:dir ext:desktop exercises a non-empty
// directory-typed result; type:file ext:json exercises the top-N truncation
// path where dirs must not displace file matches.
func TestTypedExtLaneParity(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	a := dottedPathBenchmarkIndex(2000)
	b := dottedPathBenchmarkIndex(2000)
	b.Volume = "E:"
	va := newServiceVolumeIndex("typed-ext-a.gsi", a)
	va.rebuildNameTrigramsLocked()
	vb := newServiceVolumeIndex("typed-ext-b.gsi", b)
	vb.rebuildNameTrigramsLocked()
	volumes := []*serviceVolumeIndex{va, vb}
	queries := []queryOptions{
		{Query: "type:file ext:json", MatchPath: true, Limit: 20},
		{Query: "type:file ext:nrrd", MatchPath: true, Limit: 20},
		{Query: "type:file ext:txt", MatchPath: true, Limit: 100},
		{Query: "type:dir ext:desktop", MatchPath: true, Limit: 20},
		{Query: "type:dir ext:json", MatchPath: true, Limit: 20},
		{Query: "type:file ext:md", MatchPath: true, Limit: 5},
	}
	for _, opts := range queries {
		t.Run(strings.ReplaceAll(opts.Query, " ", "_"), func(t *testing.T) {
			want, err := r5ExhaustivePlannerOracle(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			trace := &searchTrace{}
			tracedOpts := opts
			tracedOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, tracedOpts, false)
			if err != nil {
				t.Fatalf("search: %v trace=%+v", err, *trace)
			}
			if !sameOrderedStrings(pathsOf(got), pathsOf(want)) {
				t.Fatalf("search paths=%v want=%v trace=%+v", pathsOf(got), pathsOf(want), *trace)
			}
			if trace.PlannerMode != "global-ext" {
				t.Fatalf("planner = %q, want global-ext (typed ext lane)", trace.PlannerMode)
			}
			wantCount, err := r5ExhaustivePlannerOracle(volumes, opts, true)
			if err != nil {
				t.Fatal(err)
			}
			n, _, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if n != len(wantCount) {
				t.Fatalf("count=%d want=%d", n, len(wantCount))
			}
		})
	}
}

// path with concurrent broad queries: every run must produce the identical
// result set, matching the exhaustive oracle. The fixture is sized so the
// ext-driven queries drain well past the parallel threshold (asserted via
// trace candidates). This guards the drain + partition + merge restructure
// of collectGlobalVerifiedTopN (data races would surface as flaky
// mismatches; run with -race in CI).
func TestParallelGlobalVerifyDeterministic(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	a := dottedPathBenchmarkIndex(12000)
	b := dottedPathBenchmarkIndex(12000)
	b.Volume = "E:"
	va := newServiceVolumeIndex("parallel-a.gsi", a)
	va.rebuildNameTrigramsLocked()
	vb := newServiceVolumeIndex("parallel-b.gsi", b)
	vb.rebuildNameTrigramsLocked()
	volumes := []*serviceVolumeIndex{va, vb}
	queries := []queryOptions{
		{Query: "plain txt", MatchPath: true, Limit: 20},
		{Query: "trainingdata Dataset", MatchPath: true, Limit: 20},
		{Query: "scan nrrd", MatchPath: true, Limit: 20},
		{Query: "backup bak", MatchPath: true, Limit: 100},
	}
	want := make([][]string, len(queries))
	sawParallel := false
	for i, opts := range queries {
		oracle, err := r5ExhaustivePlannerOracle(volumes, opts, false)
		if err != nil {
			t.Fatal(err)
		}
		want[i] = pathsOf(oracle)
		trace := &searchTrace{}
		tracedOpts := opts
		tracedOpts.Trace = trace
		if _, err := searchServiceVolumes(volumes, tracedOpts, false); err != nil {
			t.Fatal(err)
		}
		if trace.ComponentRecordsVerified >= 2*serviceTrigramParallelVerifyMinIDs {
			sawParallel = true
		}
		wantCount, err := r5ExhaustivePlannerOracle(volumes, opts, true)
		if err != nil {
			t.Fatal(err)
		}
		n, _, err := countServiceVolumes(volumes, opts)
		if err != nil {
			t.Fatal(err)
		}
		if n != len(wantCount) {
			t.Fatalf("query %q count=%d want=%d", opts.Query, n, len(wantCount))
		}
	}
	if !sawParallel {
		t.Fatal("no query drained past the parallel threshold; test does not exercise the parallel path")
	}
	const rounds = 8
	const workers = 8
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		errs := make([]error, len(queries)*workers)
		for i, opts := range queries {
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(i, w int) {
					defer wg.Done()
					got, err := searchServiceVolumes(volumes, opts, false)
					if err != nil {
						errs[i*workers+w] = err
						return
					}
					if !sameOrderedStrings(pathsOf(got), want[i]) {
						errs[i*workers+w] = errors.New("result mismatch vs oracle: " + opts.Query)
					}
				}(i, w)
			}
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d query worker %d: %v", r, i, err)
			}
		}
	}
}
