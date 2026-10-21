package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const r5OverlayGateSeed = 20260713

type r5OverlayGateQuery struct {
	name      string
	query     string
	matchPath bool
	under     string
}

type r5OverlayGateState struct {
	name    string
	vol     *serviceVolumeIndex
	logical map[uint64]CompactRecord
	// oracle is the clean-index reference volume for state.logical, built
	// once per state. The old helpers rebuilt it (plus a t.TempDir) on
	// every compare/count call — thousands of redundant builds of a
	// ~10-record index. Searches only read it through deterministic memo
	// caches, so reusing it across the sequential matrix compares the
	// same arms with the same assertions.
	oracle *serviceVolumeIndex
}

// r5GateState builds a state with its oracle volume in one place, so every
// construction site shares the build-once contract documented above.
func r5GateState(t *testing.T, name string, vol *serviceVolumeIndex, logical map[uint64]CompactRecord) r5OverlayGateState {
	t.Helper()
	return r5OverlayGateState{
		name:    name,
		vol:     vol,
		logical: logical,
		oracle:  newServiceVolumeIndex(filepath.Join(t.TempDir(), "oracle-"+name+".gsi"), r5OverlayGateFreshIndex(logical)),
	}
}

func TestR5ActiveOverlayOwnedInProcessGate(t *testing.T) {
	const seed = r5OverlayGateSeed
	t.Logf("R5 active-overlay gate seed=%d real_usn=blocked-no-unwatched-disposable-volume", seed)

	base := r5OverlayGateLogical()
	baseVol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "base.gsi"), r5OverlayGateFreshIndex(base))
	baseState := r5GateState(t, "clean-base", baseVol, cloneR5Logical(base))
	r5OverlayGateParitySmoke(t, baseState)

	activeVol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "active.gsi"), r5OverlayGateFreshIndex(base))
	activeLogical := cloneR5Logical(base)
	for i, changes := range r5OverlayGateChanges() {
		activeVol.applyUSNChanges(changes)
		r5OverlayGateApplyOracleChanges(activeLogical, changes)
		state := r5GateState(t, fmt.Sprintf("active-%02d", i+1), activeVol, cloneR5Logical(activeLogical))
		r5OverlayGateParitySmoke(t, state)
	}
	active := r5GateState(t, "active-overlay", activeVol, activeLogical)
	if snap := activeVol.snap.Load(); snap == nil || snap.watermark <= 1 {
		t.Fatalf("active overlay structural fixture has watermark=%v, want multiple additions/tombstones beyond limit 1", snap)
	}
	r5OverlayGateCancellationSmoke(t, active)

	compacted := compactOverlayIndex(activeVol)
	if compacted == nil || len(compacted.Records) == 0 {
		t.Fatal("flush-equivalent compaction returned no records")
	}
	flushed := r5GateState(t, "flush-equivalent",
		newServiceVolumeIndex(filepath.Join(t.TempDir(), "flushed.gsi"), compacted),
		cloneR5Logical(activeLogical))
	r5OverlayGateParitySmoke(t, flushed)

	walDir := t.TempDir()
	walDB := filepath.Join(walDir, "restart.gsi")
	walChanges := []usnChange{
		{FRN: 130, ParentFRN: 101, USN: 200, Reason: usnReasonFileCreate, Name: "wal-needle.txt"},
		{FRN: 131, ParentFRN: 101, USN: 201, Reason: usnReasonFileCreate, Name: "wal-hidden.txt", Attr: fileAttributeHidden},
	}
	if err := appendWAL(walDB, 201, walChanges); err != nil {
		t.Fatalf("append WAL: %v", err)
	}
	walLogical := cloneR5Logical(base)
	r5OverlayGateApplyOracleChanges(walLogical, walChanges)
	walReplay := newServiceVolumeIndex(walDB, r5OverlayGateFreshIndex(base))
	if err := walReplay.replayWAL(); err != nil {
		t.Fatalf("replay WAL: %v", err)
	}
	replayed := r5GateState(t, "wal-replay", walReplay, walLogical)
	r5OverlayGateParitySmoke(t, replayed)

	restarted := newServiceVolumeIndex(walDB, r5OverlayGateFreshIndex(base))
	if err := restarted.replayWAL(); err != nil {
		t.Fatalf("restart WAL replay: %v", err)
	}
	restartedState := r5GateState(t, "restart-wal-replay", restarted, cloneR5Logical(walLogical))
	r5OverlayGateParitySmoke(t, restartedState)
	if err := removeWAL(walDB); err != nil {
		t.Fatalf("remove owned WAL: %v", err)
	}

	for _, state := range []r5OverlayGateState{active, flushed, replayed, restartedState} {
		r5OverlayGateMatrix(t, state)
	}
}

func r5OverlayGateLogical() map[uint64]CompactRecord {
	return map[uint64]CompactRecord{
		100: {FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		101: {FRN: 101, ParentFRN: 100, Parent: 0, Name: "workspace", Mode: uint32(os.ModeDir)},
		102: {FRN: 102, ParentFRN: 101, Parent: 1, Name: "base-needle.txt", Size: 1024, ModUnix: 10},
		103: {FRN: 103, ParentFRN: 101, Parent: 1, Name: "shadow.txt", Size: 2048, ModUnix: 20},
		104: {FRN: 104, ParentFRN: 101, Parent: 1, Name: "old-dir", Mode: uint32(os.ModeDir)},
		105: {FRN: 105, ParentFRN: 104, Parent: 3, Name: "old-needle.raw", Size: 4096, ModUnix: 30},
		106: {FRN: 106, ParentFRN: 101, Parent: 1, Name: "same-a.raw", Size: 4096, ModUnix: 30},
		107: {FRN: 107, ParentFRN: 101, Parent: 1, Name: "same-b.raw", Size: 4096, ModUnix: 30},
		108: {FRN: 108, ParentFRN: 101, Parent: 1, Name: "base-hidden.dat", Mode: modeFromAttrs(fileAttributeHidden), Size: 512, ModUnix: 40},
	}
}

func r5OverlayGateChanges() [][]usnChange {
	return [][]usnChange{
		{{FRN: 110, ParentFRN: 101, USN: 100, Reason: usnReasonFileCreate, Name: "active-needle.txt"}},
		{{FRN: 110, ParentFRN: 101, USN: 101, Reason: usnReasonRenameOld, Name: "active-needle.txt"}, {FRN: 110, ParentFRN: 101, USN: 102, Reason: usnReasonRenameNew, Name: "renamed-needle.txt"}},
		{
			{FRN: 111, ParentFRN: 100, USN: 103, Reason: usnReasonFileCreate, Name: "parent-a", Attr: fileAttributeDir},
			{FRN: 112, ParentFRN: 100, USN: 104, Reason: usnReasonFileCreate, Name: "parent-b", Attr: fileAttributeDir},
			{FRN: 113, ParentFRN: 111, USN: 105, Reason: usnReasonFileCreate, Name: "moved-dir", Attr: fileAttributeDir},
			{FRN: 114, ParentFRN: 113, USN: 106, Reason: usnReasonFileCreate, Name: "moved-needle.raw"},
			{FRN: 113, ParentFRN: 111, USN: 107, Reason: usnReasonRenameOld, Name: "moved-dir"},
			{FRN: 113, ParentFRN: 112, USN: 108, Reason: usnReasonRenameNew, Name: "moved-dir"},
		},
		{
			{FRN: 115, ParentFRN: 101, USN: 109, Reason: usnReasonFileCreate, Name: "doomed-dir", Attr: fileAttributeDir},
			{FRN: 116, ParentFRN: 115, USN: 110, Reason: usnReasonFileCreate, Name: "doomed-needle.txt"},
			{FRN: 115, USN: 111, Reason: usnReasonFileDelete},
		},
		{
			{FRN: 103, USN: 112, Reason: usnReasonFileDelete},
			{FRN: 117, ParentFRN: 101, USN: 113, Reason: usnReasonFileCreate, Name: "shadow.txt"},
		},
		{{FRN: 118, ParentFRN: 101, USN: 114, Reason: usnReasonFileCreate, Name: "overlay-hidden.txt", Attr: fileAttributeHidden}},
	}
}

func r5OverlayGateQueries() []r5OverlayGateQuery {
	return []r5OverlayGateQuery{
		{name: "filename", query: "needle"},
		{name: "component", query: "path:workspace needle"},
		{name: "scalar", query: "size:>=0"},
		{name: "or", query: "needle|raw", matchPath: true},
		{name: "not", query: "needle !raw", matchPath: true},
		{name: "attrib", query: "attrib:H"},
		{name: "shadow", query: "shadow"},
		{name: "no-hit", query: "zzzz-r5-no-hit"},
		{name: "under", query: "needle", matchPath: true, under: `F:\workspace`},
	}
}

func r5OverlayGateSorts() []string {
	return []string{"", "sort:path", "sort:size", "sort:modified", "sort:extension", "sort:type"}
}

func r5OverlayGateParitySmoke(t *testing.T, state r5OverlayGateState) {
	t.Helper()
	queries := []r5OverlayGateQuery{
		{name: "smoke-filename", query: "needle"},
		{name: "smoke-component", query: "path:workspace needle"},
		{name: "smoke-or", query: "needle|raw", matchPath: true},
		{name: "smoke-not", query: "needle !raw", matchPath: true},
		{name: "smoke-scalar", query: "size:>=0"},
		{name: "smoke-attrib", query: "attrib:H"},
	}
	for _, query := range queries {
		if _, _, _, _, err := r5OverlayGateCompare(t, state, query, "sort:path", 20, 1); err != nil {
			t.Fatalf("%s parity smoke %s: %v", state.name, query.name, err)
		}
	}
}

func r5OverlayGateCancellationSmoke(t *testing.T, state r5OverlayGateState) {
	t.Helper()
	opts := queryOptions{Query: "needle|raw", MatchPath: true, Limit: 20, Cancel: func() bool { return true }}
	if _, err := searchServiceVolumes([]*serviceVolumeIndex{state.vol}, opts, false); !errors.Is(err, errQueryCanceled) {
		t.Fatalf("%s search cancellation = %v, want %v", state.name, err, errQueryCanceled)
	}
	other := r5OverlayGateFreshIndex(map[uint64]CompactRecord{
		200: {FRN: 200, ParentFRN: 200, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
	})
	other.Volume = "C:"
	if _, _, err := countServiceVolumes([]*serviceVolumeIndex{state.vol, newServiceVolumeIndex(filepath.Join(t.TempDir(), "cancel.gsi"), other)}, opts); !errors.Is(err, errQueryCanceled) {
		t.Fatalf("%s count cancellation = %v, want %v", state.name, err, errQueryCanceled)
	}
}

func r5OverlayGateMatrix(t *testing.T, state r5OverlayGateState) {
	t.Helper()
	queries := r5OverlayGateQueries()
	sorts := r5OverlayGateSorts()
	limits := []int{1, 20, 100}
	caseCount := 0
	var searchSamples, countSamples []float64
	var searchHeap, countHeap uint64
	worstSearch, worstCount := "", ""
	worstSearchMS, worstCountMS := 0.0, 0.0
	searchDigest, countDigest := sha256.New(), sha256.New()
	for _, query := range queries {
		for _, sortColumn := range sorts {
			for _, limit := range limits {
				caseCount++
				for sample := 0; sample < 3; sample++ {
					searchMS, count, gotHash, wantHash, err := r5OverlayGateCompare(t, state, query, sortColumn, limit, 1)
					if err != nil {
						t.Fatalf("%s matrix %s %s limit=%d sample=%d: %v", state.name, query.name, sortColumn, limit, sample, err)
					}
					_ = count
					if gotHash != wantHash {
						t.Fatalf("%s matrix %s result hash %s != oracle %s", state.name, query.name, gotHash, wantHash)
					}
					_, _ = searchDigest.Write([]byte(fmt.Sprintf("%s|%s|%d|%s\x00", query.name, sortColumn, limit, gotHash)))
					searchSamples = append(searchSamples, searchMS)
					if searchMS > worstSearchMS {
						worstSearchMS, worstSearch = searchMS, query.name+" "+sortColumn
					}

					var mem runtime.MemStats
					runtime.ReadMemStats(&mem)
					searchHeap = maxUint64(searchHeap, mem.HeapInuse)

					countMS, countHash, oracleCountHash, err := r5OverlayGateCount(t, state, query, sortColumn, limit)
					if err != nil {
						t.Fatalf("%s matrix count %s %s limit=%d sample=%d: %v", state.name, query.name, sortColumn, limit, sample, err)
					}
					if countHash != oracleCountHash {
						t.Fatalf("%s matrix %s count hash %s != oracle %s", state.name, query.name, countHash, oracleCountHash)
					}
					_, _ = countDigest.Write([]byte(fmt.Sprintf("%s|%s|%d|%s\x00", query.name, sortColumn, limit, countHash)))
					countSamples = append(countSamples, countMS)
					if countMS > worstCountMS {
						worstCountMS, worstCount = countMS, query.name+" "+sortColumn
					}
					runtime.ReadMemStats(&mem)
					countHeap = maxUint64(countHeap, mem.HeapInuse)
				}
			}
		}
	}
	searchStats := r5OverlayGateLatencyStats(searchSamples)
	countStats := r5OverlayGateLatencyStats(countSamples)
	t.Logf("R5 active-overlay state=%s seed=%d cases=%d search_hash=%s count_hash=%s search_p50=%.3f search_p95=%.3f search_p99=%.3f search_max=%.3f count_p50=%.3f count_p95=%.3f count_p99=%.3f count_max=%.3f worst_search=%s worst_count=%s go_heap_inuse_max=%d", state.name, r5OverlayGateSeed, caseCount, hex.EncodeToString(searchDigest.Sum(nil)), hex.EncodeToString(countDigest.Sum(nil)), searchStats.p50, searchStats.p95, searchStats.p99, searchStats.max, countStats.p50, countStats.p95, countStats.p99, countStats.max, worstSearch, worstCount, maxUint64(searchHeap, countHeap))
	if searchStats.p95 > 100 || searchStats.p99 > 200 || countStats.p95 > 100 || countStats.p99 > 200 {
		t.Fatalf("%s active-overlay latency gate breached: search=%v count=%v", state.name, searchStats, countStats)
	}
}

func r5OverlayGateCompare(t *testing.T, state r5OverlayGateState, query r5OverlayGateQuery, sortColumn string, limit, repetitions int) (float64, int, string, string, error) {
	t.Helper()
	oracleVol := state.oracle
	queryText := query.query
	if sortColumn != "" {
		queryText += " " + sortColumn
	}
	makeOpts := func(trace *searchTrace) queryOptions {
		return queryOptions{Query: queryText, MatchPath: query.matchPath, Under: query.under, Limit: limit, Trace: trace}
	}
	var elapsed time.Duration
	var gotHash, wantHash string
	gotCount := 0
	for i := 0; i < repetitions; i++ {
		gotTrace, wantTrace := &searchTrace{}, &searchTrace{}
		start := time.Now()
		got, err := searchServiceVolumes([]*serviceVolumeIndex{state.vol}, makeOpts(gotTrace), false)
		elapsed = time.Since(start)
		if err != nil {
			return 0, 0, "", "", err
		}
		if state.vol.hasActiveOverlay() && gotTrace.PlannerMode == "service-single-volume" {
			snap := state.vol.snap.Load()
			pq, parseErr := parseQuery(makeOpts(&searchTrace{}))
			if parseErr != nil {
				return 0, 0, "", "", parseErr
			}
			expectedWindow := normalizedLimit(limit, false) + state.vol.overlayLiveMatchCount(snap, pq)
			if gotTrace.OverlayBaseWindow != expectedWindow {
				return 0, 0, "", "", fmt.Errorf("overlay base window=%d want correctness bound %d", gotTrace.OverlayBaseWindow, expectedWindow)
			}
		} else if gotTrace.OverlayBaseWindow != 0 {
			return 0, 0, "", "", fmt.Errorf("inactive overlay base window=%d", gotTrace.OverlayBaseWindow)
		}
		want, err := searchServiceVolumes([]*serviceVolumeIndex{oracleVol}, makeOpts(wantTrace), false)
		if err != nil {
			return 0, 0, "", "", err
		}
		gotHash = r5OverlayGateHashPaths(pathsOf(got))
		wantHash = r5OverlayGateHashPaths(pathsOf(want))
		gotCount = len(got)
		if gotHash != wantHash || r5OverlayGateComplete(gotTrace) != r5OverlayGateComplete(wantTrace) {
			return 0, 0, gotHash, wantHash, fmt.Errorf("search parity complete=%v oracle=%v got=%v want=%v got_trace=%+v want_trace=%+v", r5OverlayGateComplete(gotTrace), r5OverlayGateComplete(wantTrace), pathsOf(got), pathsOf(want), *gotTrace, *wantTrace)
		}
	}
	return float64(elapsed.Microseconds()) / 1000, gotCount, gotHash, wantHash, nil
}

func r5OverlayGateCount(t *testing.T, state r5OverlayGateState, query r5OverlayGateQuery, sortColumn string, limit int) (float64, string, string, error) {
	t.Helper()
	oracleVol := state.oracle
	queryText := query.query
	if sortColumn != "" {
		queryText += " " + sortColumn
	}
	makeOpts := func(trace *searchTrace) queryOptions {
		return queryOptions{Query: queryText, MatchPath: query.matchPath, Under: query.under, Limit: limit, Trace: trace}
	}
	gotTrace, wantTrace := &searchTrace{}, &searchTrace{}
	start := time.Now()
	got, gotOK, err := countServiceVolumes([]*serviceVolumeIndex{state.vol}, makeOpts(gotTrace))
	if err != nil {
		return 0, "", "", err
	}
	want, wantOK, err := countServiceVolumes([]*serviceVolumeIndex{oracleVol}, makeOpts(wantTrace))
	if err != nil {
		return 0, "", "", err
	}
	elapsed := time.Since(start)
	if !gotOK || !wantOK || got != want || r5OverlayGateComplete(gotTrace) != r5OverlayGateComplete(wantTrace) {
		return 0, "", "", fmt.Errorf("count parity got=(%d,%v,%v) want=(%d,%v,%v)", got, gotOK, r5OverlayGateComplete(gotTrace), want, wantOK, r5OverlayGateComplete(wantTrace))
	}
	return float64(elapsed.Microseconds()) / 1000, r5OverlayGateHashCount(got), r5OverlayGateHashCount(want), nil
}

type r5OverlayGateLatency struct {
	count float64
	p50   float64
	p95   float64
	p99   float64
	max   float64
}

func r5OverlayGateLatencyStats(values []float64) r5OverlayGateLatency {
	if len(values) == 0 {
		return r5OverlayGateLatency{}
	}
	ordered := append([]float64(nil), values...)
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j] < ordered[j-1]; j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	pick := func(p float64) float64 {
		index := int(float64(len(ordered))*p+0.999999) - 1
		if index < 0 {
			index = 0
		}
		if index >= len(ordered) {
			index = len(ordered) - 1
		}
		return ordered[index]
	}
	return r5OverlayGateLatency{count: float64(len(ordered)), p50: pick(.50), p95: pick(.95), p99: pick(.99), max: ordered[len(ordered)-1]}
}

func r5OverlayGateComplete(trace *searchTrace) bool {
	return trace != nil && trace.completePtr() != nil && *trace.completePtr()
}

func r5OverlayGateHashPaths(paths []string) string {
	h := sha256.New()
	for _, path := range paths {
		_, _ = h.Write([]byte(path))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func r5OverlayGateHashCount(count int) string {
	return r5OverlayGateHashPaths([]string{fmt.Sprintf("%d", count)})
}

func cloneR5Logical(in map[uint64]CompactRecord) map[uint64]CompactRecord {
	out := make(map[uint64]CompactRecord, len(in))
	for frn, rec := range in {
		out[frn] = rec
	}
	return out
}

func r5OverlayGateApplyOracleChanges(logical map[uint64]CompactRecord, changes []usnChange) {
	for _, change := range changes {
		rec := logical[change.FRN]
		switch {
		case change.Reason&usnReasonFileDelete != 0:
			markLogicalDeleted(logical, change.FRN)
		case change.Reason&usnReasonRenameNew != 0 || change.Reason&usnReasonFileCreate != 0:
			rec.FRN = change.FRN
			rec.ParentFRN = change.ParentFRN
			rec.Name = change.Name
			rec.Mode = modeFromAttrs(change.Attr)
			rec.Deleted = false
			// USN changes do not carry size/mtime. The active overlay stores
			// the same conservative zero values until a later refresh.
			rec.Size = 0
			rec.ModUnix = 0
			logical[change.FRN] = rec
		}
	}
}

func r5OverlayGateFreshIndex(logical map[uint64]CompactRecord) *Index {
	idx := freshIndexFromLogicalRecords("F:", cloneR5Logical(logical))
	idx.CompactAttrs = true
	return idx
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
