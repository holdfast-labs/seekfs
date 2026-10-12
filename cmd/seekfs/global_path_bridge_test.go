package main

import (
	"fmt"
	"slices"
	"sort"
	"testing"
)

// TestGlobalPathBridgeSingleTerm covers the -path slow lane: a selective
// single-term path query must use the global components lane (trigram bridge
// into budgeted subtree expansion) instead of a full scan. notes.txt matches
// only via dir-subtree expansion (its own name lacks the term), which proves
// the bridge is a superset union rather than a name-only lookup.
func TestGlobalPathBridgeSingleTerm(t *testing.T) {
	// The global components lane (where the bridge lives) runs only with
	// the global planner enabled, as the UI always sets it in production.
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	volumes := cheapPredicateTestVolumes(t)
	want := []string{
		`F:\other\run_pipeline.bat`,
		`F:\run_pipeline`,
		`F:\run_pipeline-cache`,
		`F:\run_pipeline\notes.txt`,
		`F:\scope\run_pipeline.bat`,
	}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "run_pipeline", MatchPath: true, Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	paths := pathsOf(got)
	sort.Strings(paths)
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if !slices.Contains(paths, `F:\run_pipeline\notes.txt`) {
		t.Fatalf("dir-only hit missing: expansion did not cover subtree contents")
	}
	if trace.PlannerMode != "global-components" ||
		(trace.Source != "global:components" && trace.Source != "global:component-top") {
		t.Fatalf("trace = %+v, want global components lane", trace)
	}
	if trace.Complete == nil || !*trace.Complete {
		t.Fatalf("trace complete = %v, want true", trace.Complete)
	}
	n, ok, err := countServiceVolumes(volumes, queryOptions{Query: "run_pipeline", MatchPath: true})
	if err != nil || !ok || n != len(want) {
		t.Fatalf("count = %d ok=%v err=%v, want %d true nil", n, ok, err, len(want))
	}
}

// TestBridgePathTermIDsWhiteBox pins the bridge contract directly: exact set
// for a selective term, fast-empty for a miss, decline for a short term.
func TestBridgePathTermIDsWhiteBox(t *testing.T) {
	vol := cheapPredicateTestVolumes(t)[0]
	ids, ok := vol.bridgePathTermIDs("run_pipeline")
	if !ok {
		t.Fatalf("bridge declined selective term")
	}
	paths := make([]string, 0, len(ids))
	for _, id := range ids {
		paths = append(paths, vol.index.reconstructCompactPath(id))
	}
	sort.Strings(paths)
	want := []string{
		`F:\other\run_pipeline.bat`,
		`F:\run_pipeline`,
		`F:\run_pipeline-cache`,
		`F:\run_pipeline\notes.txt`,
		`F:\scope\run_pipeline.bat`,
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("bridge paths = %v, want %v", paths, want)
	}
	if ids, ok := vol.bridgePathTermIDs("zzz-no-hit"); !ok || len(ids) != 0 {
		t.Fatalf("bridge miss = (%v, %v), want ([], true)", ids, ok)
	}
	if ids, ok := vol.bridgePathTermIDs("ab"); ok || ids != nil {
		t.Fatalf("bridge short term = (%v, %v), want decline", ids, ok)
	}
}

// TestBridgePathTermIDsDeclinesBroad pins the budget guard: a term matching
// one dir that owns more than the expansion cap must decline (not hang
// materializing it), even though the name trigram itself is selective.
func TestBridgePathTermIDsDeclinesBroad(t *testing.T) {
	records := []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "bigdir-zz9", Mode: modeFromAttrs(fileAttributeDir)},
	}
	for i := 0; i < serviceComponentTrigramExpansionMaxIDs+1000; i++ {
		records = append(records, CompactRecord{
			FRN: uint64(3 + i), ParentFRN: 2, Parent: 1,
			Name: fmt.Sprintf("note-%05d.txt", i), Mode: modeFromAttrs(fileAttributeArchive),
		})
	}
	idx := &Index{Source: "usn", Volume: "F:", Roots: []string{`F:\`}, Compact: true, Records: records}
	buildOrders(idx)
	vol := newServiceVolumeIndex("f-bridge-broad.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	if ids, ok := vol.bridgePathTermIDs("bigdir-zz9"); ok || ids != nil {
		t.Fatalf("bridge broad = (%d ids, %v), want decline", len(ids), ok)
	}
	// The declined query must still answer (via fallback lanes) without
	// hanging: the dir itself is a top hit.
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "bigdir-zz9", MatchPath: true, Limit: 5, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Name != "bigdir-zz9" {
		t.Fatalf("broad fallback paths = %v, want bigdir-zz9 first", pathsOf(got))
	}
}

// TestGlobalPathBridgeLowMem covers the low-memory index shape (child ranges
// without SUBT intervals, as the production service runs): terms with no
// dir-hits are served without intervals, while a matching dir forces a
// decline (expansion would fall back to a full-volume scan iterator).
func TestGlobalPathBridgeLowMem(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	records := []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "scope", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 3, ParentFRN: 2, Parent: 1, Name: "run_pipeline.bat", Mode: modeFromAttrs(fileAttributeArchive)},
		{FRN: 4, ParentFRN: 1, Parent: 0, Name: "rundir-zz9", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 5, ParentFRN: 4, Parent: 3, Name: "notes.txt", Mode: modeFromAttrs(fileAttributeArchive)},
	}
	idx := &Index{Source: "usn", Volume: "F:", Roots: []string{`F:\`}, Compact: true, Records: records}
	buildOrders(idx)
	vol := newServiceVolumeIndex("f-bridge-lowmem.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	volumes := []*serviceVolumeIndex{vol}

	// No dir contains "pipeline" in this fixture's dir names... except via
	// substring: scope/rundir-zz9 don't contain it, so expansion is vacuous.
	// Use a name-only term instead: "notes" hits notes.txt by name.
	if ids, ok := vol.bridgePathTermIDs("notes"); !ok {
		t.Fatalf("lowmem bridge declined name-only term")
	} else if len(ids) != 1 {
		t.Fatalf("lowmem bridge name-only = %d ids, want 1", len(ids))
	}
	// rundir-zz9 contains "zz9" and owns a subtree but lowmem has no SUBT
	// intervals: must decline rather than scan-expand.
	if len(vol.subtreeOrder) != 0 {
		t.Skip("lowmem fixture unexpectedly built subtree intervals")
	}
	if ids, ok := vol.bridgePathTermIDs("zz9"); ok || ids != nil {
		t.Fatalf("lowmem bridge with dir-hits = (%d ids, %v), want decline without intervals", len(ids), ok)
	}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "notes", MatchPath: true, Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(got); !slices.Equal(paths, []string{`F:\rundir-zz9\notes.txt`}) {
		t.Fatalf("lowmem -path notes = %v, want single hit", paths)
	}
}

// TestGlobalPathBridgeMultiVolume pins cross-volume coverage: hits from both
// volumes must appear (no volume is dropped by the per-volume fallback).
func TestGlobalPathBridgeMultiVolume(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	fvol := cheapPredicateTestVolumes(t)[0]
	crecords := []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "run_pipeline.bat", Mode: modeFromAttrs(fileAttributeArchive)},
	}
	cidx := &Index{Source: "usn", Volume: "C:", Roots: []string{`C:\`}, Compact: true, Records: crecords}
	buildOrders(cidx)
	selective := buildSelectiveNameTrigramIndex(cidx, 1)
	cidx.Derived.NameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(selective, nil), cidx.compactRecordCount())
	extra := optionalSelfNameGramIndex(cidx, selective)
	cidx.Derived.SelfNameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(extra, nil), cidx.compactRecordCount())
	cvol := newServiceVolumeIndex("c-bridge-multi.gsi", cidx)
	cvol.rebuildNameTrigramsLocked()
	volumes := []*serviceVolumeIndex{cvol, fvol}

	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "run_pipeline", MatchPath: true, Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	paths := pathsOf(got)
	if !slices.Contains(paths, `C:\run_pipeline.bat`) || !slices.Contains(paths, `F:\scope\run_pipeline.bat`) {
		t.Fatalf("multi-volume -path = %v, want hits from both volumes", paths)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
	}
}
