package main

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"testing"
)

// TestOverlayHiddenTruncatedLaneParity pins the hiddenBlocksTruncation gates:
// with overlay-hidden base records present, top-N/limited lanes must decline
// so the page cannot underfill. Short (1-2 char) terms bypass the trigram and
// would otherwise be served by limit-truncated scans. The oracle is a fresh
// index built from the final logical records (no overlay), so any underfill
// or wrong result on the overlay volume fails loudly.
func TestOverlayHiddenTruncatedLaneParity(t *testing.T) {
	// aa-gone.txt sorts before every live hit: with Limit 1 a truncated
	// top-N lane returns only the tombstone, the verify loop drops it, and
	// the page underfills — unless the lane declines under hidden.
	base := map[uint64]CompactRecord{
		1: {FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		2: {FRN: 2, ParentFRN: 1, Parent: 0, Name: "aa-gone.txt", Size: 500},
		3: {FRN: 3, ParentFRN: 1, Parent: 0, Name: "aa-live.txt", Size: 600},
		4: {FRN: 4, ParentFRN: 1, Parent: 0, Name: "other.txt", Size: 700},
		5: {FRN: 5, ParentFRN: 1, Parent: 0, Name: "sub", Mode: uint32(os.ModeDir)},
		6: {FRN: 6, ParentFRN: 5, Parent: 4, Name: "aw-deep.txt", Size: 800},
	}
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "hidden-parity.gsi"), r5OverlayGateFreshIndex(base))
	changes := []usnChange{
		{FRN: 2, USN: 10, Reason: usnReasonFileDelete},
		{FRN: 7, ParentFRN: 1, USN: 11, Reason: usnReasonFileCreate, Name: "aw-three.txt"},
	}
	vol.applyUSNChanges(changes)
	if vol.snapshotHiddenBaseIDs().empty() {
		t.Fatal("fixture has no hidden ids; gates untested")
	}
	if !vol.hasActiveOverlay() {
		t.Fatal("fixture has no active overlay")
	}
	logical := cloneR5Logical(base)
	r5OverlayGateApplyOracleChanges(logical, changes)
	oracle := newServiceVolumeIndex(filepath.Join(t.TempDir(), "hidden-parity-oracle.gsi"), r5OverlayGateFreshIndex(logical))

	queries := []struct {
		queryOptions
		wantTotal int
	}{
		{queryOptions: queryOptions{Query: "aa", Limit: 20}, wantTotal: 1},
		{queryOptions: queryOptions{Query: "aa", Limit: 1}, wantTotal: 1},
		{queryOptions: queryOptions{Query: "aw", Limit: 20}, wantTotal: 2},
		{queryOptions: queryOptions{Query: "aw", Limit: 2}, wantTotal: 2},
		{queryOptions: queryOptions{Query: "aw", Limit: 1}, wantTotal: 2},
		{queryOptions: queryOptions{Query: "w", Limit: 2}, wantTotal: 2},
		{queryOptions: queryOptions{Query: "aw", MatchPath: true, Limit: 2}, wantTotal: 2},
		{queryOptions: queryOptions{Query: "aw", MatchPath: true, Limit: 1}, wantTotal: 2},
		{queryOptions: queryOptions{Query: "aa-gone", Limit: 5}, wantTotal: 0},
		{queryOptions: queryOptions{Query: "txt", Limit: 1}, wantTotal: 4},
		{queryOptions: queryOptions{Query: "other", Limit: 1}, wantTotal: 1},
		{queryOptions: queryOptions{Query: "zzz-no-hit", Limit: 5}, wantTotal: 0},
		{queryOptions: queryOptions{Query: "aw", MatchPath: true, Under: `F:\sub`, Limit: 5}, wantTotal: 1},
		{queryOptions: queryOptions{Query: "aw dir:sub", Limit: 5}, wantTotal: 1},
		{queryOptions: queryOptions{Query: "aw type:file", Limit: 1}, wantTotal: 2},
	}
	for _, q := range queries {
		q := q
		name := q.Query + map[bool]string{true: "+path", false: ""}[q.MatchPath]
		if q.Under != "" {
			name += "+under"
		}
		name += "/" + strconv.Itoa(q.Limit)
		t.Run(name, func(t *testing.T) {
			gotTrace, wantTrace := &searchTrace{}, &searchTrace{}
			gotOpts, wantOpts := q.queryOptions, q.queryOptions
			gotOpts.Trace, wantOpts.Trace = gotTrace, wantTrace
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, gotOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			want, err := searchServiceVolumes([]*serviceVolumeIndex{oracle}, wantOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			gotPaths, wantPaths := pathsOf(got), pathsOf(want)
			sort.Strings(gotPaths)
			sort.Strings(wantPaths)
			if !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("overlay paths = %v, oracle = %v (trace=%+v)", gotPaths, wantPaths, gotTrace)
			}
			gotComplete := gotTrace.Complete != nil && *gotTrace.Complete
			wantComplete := wantTrace.Complete != nil && *wantTrace.Complete
			if gotComplete != wantComplete {
				t.Fatalf("trace complete overlay=%v oracle=%v; want agreement", gotComplete, wantComplete)
			}
			// The overlay volume must always serve count (its line-129
			// verifying-scan fallback covers unplannable shapes). The
			// no-overlay oracle declines some (e.g. sub-3-char path terms
			// have no plan source and no fallback) — pre-existing gap,
			// unrelated to hidden lanes; assert it only where supported.
			gotCount, gotOK, err := countServiceVolumes([]*serviceVolumeIndex{vol}, gotOpts)
			if err != nil || !gotOK {
				t.Fatalf("overlay count ok=%v err=%v; want served", gotOK, err)
			}
			if gotCount != q.wantTotal {
				t.Fatalf("overlay count = %d, want %d", gotCount, q.wantTotal)
			}
			wantCount, wantOK, err := countServiceVolumes([]*serviceVolumeIndex{oracle}, wantOpts)
			if err != nil {
				t.Fatal(err)
			}
			if wantOK && wantCount != q.wantTotal {
				t.Fatalf("oracle count = %d, want %d", wantCount, q.wantTotal)
			}
		})
	}
}

// TestOverlayHiddenShortTermUsesBoundedScan proves the gates change routing
// (not just results): a short-term hit under hidden must come from the
// hidden-aware bounded scan, never the full compact-name-order scan.
func TestOverlayHiddenShortTermUsesBoundedScan(t *testing.T) {
	base := map[uint64]CompactRecord{
		1: {FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		2: {FRN: 2, ParentFRN: 1, Parent: 0, Name: "aa-gone.txt", Size: 500},
		3: {FRN: 3, ParentFRN: 1, Parent: 0, Name: "aa-live.txt", Size: 600},
		5: {FRN: 5, ParentFRN: 1, Parent: 0, Name: "sub", Mode: uint32(os.ModeDir)},
		6: {FRN: 6, ParentFRN: 5, Parent: 5, Name: "aw-deep.txt", Size: 800},
	}
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "hidden-lane.gsi"), r5OverlayGateFreshIndex(base))
	vol.applyUSNChanges([]usnChange{{FRN: 2, USN: 10, Reason: usnReasonFileDelete}})
	if vol.snapshotHiddenBaseIDs().empty() {
		t.Fatal("fixture has no hidden ids; lane assertion vacuous")
	}
	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "aa", Limit: 1, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(got); !slices.Equal(paths, []string{`F:\aa-live.txt`}) {
		t.Fatalf("paths = %v, want [F:\\aa-live.txt]", paths)
	}
	if trace.Source == "compact-name-order-scan" {
		t.Fatalf("trace source = compact-name-order-scan; want hidden-aware lane (bounded-scan)")
	}
}
