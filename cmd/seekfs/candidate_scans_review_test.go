package main

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestPlainTermPrefilterDeclinesVolumePrefixTerms(t *testing.T) {
	idx := dottedPathBenchmarkIndex(32)
	idx.Volume = `\\archive-server\share-root`
	vol := newServiceVolumeIndex("unc-prefix.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	pq := mustParsePrefilterQuery(t, queryOptions{Query: "server", MatchPath: true, Limit: 20})

	if !idx.compactPathContainsTerm(0, "server") {
		t.Fatal("fixture path should contain the term in the UNC volume prefix")
	}
	var filter *boundedScanMembershipFilter
	if exactEmpty, filterOK := vol.boundedScanPrefilter(pq, &filter); exactEmpty || filterOK {
		t.Fatalf("prefilter = (%v, %v), want decline for a term in the UNC prefix", exactEmpty, filterOK)
	}

	got, ok := vol.boundedScanCandidatesFiltered(pq, filter)
	if !ok || len(got) == 0 {
		t.Fatalf("bounded scan returned %v candidates, ok=%v; expected UNC-prefix matches", len(got), ok)
	}
}

func TestPlainTermPathFilterDeclinesRootPrefixTerms(t *testing.T) {
	idx := dottedPathBenchmarkIndex(32)
	idx.Roots = []string{`C:\archive-root\workspace`}
	vol := newServiceVolumeIndex("root-prefix.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	pq := mustParsePrefilterQuery(t, queryOptions{Query: "archive", MatchPath: true, Limit: 20})

	var filter *boundedScanMembershipFilter
	if exactEmpty, filterOK := vol.boundedScanPrefilter(pq, &filter); exactEmpty || filterOK {
		t.Fatalf("prefilter = (%v, %v), want decline for a term in the configured root", exactEmpty, filterOK)
	}
	got, ok := vol.boundedScanCandidatesFiltered(pq, filter)
	if !ok || len(got) == 0 {
		t.Fatalf("bounded scan returned %v candidates, ok=%v; expected root-prefix matches", len(got), ok)
	}
}

func TestBoundedScanHiddenTopParallelMatchesSerial(t *testing.T) {
	const recordCount = 50_000
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Records: make([]CompactRecord, 0, recordCount+1)}
	idx.Records = append(idx.Records, CompactRecord{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)})
	for i := 0; i < recordCount; i++ {
		name := fmt.Sprintf("a-unrelated-%06d.txt", i)
		if i >= recordCount-1000 {
			name = fmt.Sprintf("z-needle-%06d.txt", i)
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN: uint64(i + 2), ParentFRN: 1, Parent: 0, Name: name,
			Size: 1, ModUnix: time.Unix(int64(i), 0).UnixNano(),
		})
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("bounded-parallel.gsi", idx)
	pq := mustParsePrefilterQuery(t, queryOptions{Query: "needle", MatchPath: true, Limit: 37})
	order := vol.orderForQuery(pq)
	firstMatch := -1
	for pos, rawID := range order {
		if idx.compactPathContainsTerm(int(rawID), "needle") {
			firstMatch = pos
			break
		}
	}
	if firstMatch < 4*serviceTrigramParallelVerifyMinIDs {
		t.Fatalf("first match position = %d, want after serial prefix %d", firstMatch, 4*serviceTrigramParallelVerifyMinIDs)
	}

	// Hide several of the first matching IDs so both paths must keep scanning
	// through the late-hit region before filling the requested top-N result.
	hiddenIDs := make([]int32, 0, 7)
	for pos := firstMatch; pos < len(order) && len(hiddenIDs) < cap(hiddenIDs); pos++ {
		id := int(order[pos])
		if idx.compactPathContainsTerm(id, "needle") {
			hiddenIDs = append(hiddenIDs, int32(id))
		}
	}
	hidden := hiddenBaseIDs{tombstone: hiddenIDs}

	oldProcs := runtime.GOMAXPROCS(1)
	serial, serialOK := vol.boundedScanHiddenTop(pq, hidden, pq.Limit, nil)
	runtime.GOMAXPROCS(4)
	parallel, parallelOK := vol.boundedScanHiddenTop(pq, hidden, pq.Limit, nil)
	runtime.GOMAXPROCS(oldProcs)
	if !serialOK || !parallelOK {
		t.Fatalf("scan success = serial:%v parallel:%v", serialOK, parallelOK)
	}
	if len(serial) != pq.Limit || len(parallel) != pq.Limit {
		t.Fatalf("result lengths = serial:%d parallel:%d, want %d", len(serial), len(parallel), pq.Limit)
	}
	if !sameIntSlices(serial, parallel) {
		t.Fatalf("parallel IDs differ from serial: serial=%v parallel=%v", serial, parallel)
	}
}
