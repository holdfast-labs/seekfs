package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestContentParallelCapFrontierStopsBeforeLaterHits creates a deterministic
// out-of-order cap race: candidate 1 pauses while later candidates consume the
// remaining read slots and produce matches. The visible result must stop at
// candidate 1's unverified frontier rather than emitting later hits past it.
func TestContentParallelCapFrontierStopsBeforeLaterHits(t *testing.T) {
	files := make([]contentFixtureFile, 600)
	for i := range files {
		files[i] = contentFixtureFile{frn: uint64(i + 2), name: fmt.Sprintf("f%03d.txt", i), text: "needle in indexed text"}
	}
	vol := newContentQueryVolume(t, files)

	previousRead, previousHook := contentDiskReadForVerify, contentVerifyParallelTestHook
	pq, err := parseQuery(queryOptions{Query: "content:needle", Limit: 100, Trace: &searchTrace{DiskVerifyBudget: 8}})
	if err != nil {
		t.Fatal(err)
	}
	trace := &searchTrace{DiskVerifyBudget: 8}
	pq.Trace = trace
	ids, ok, capped := vol.contentCandidatesBounded(pq, 0)
	if !ok || capped || len(ids) != len(files) {
		t.Fatalf("candidate superset ok=%v capped=%v len=%d; want all %d candidates", ok, capped, len(ids), len(files))
	}
	sortCandidateIDs(ids, pq, vol.index, vol.rankForQuery(pq))
	pq.hidden = vol.snapshotHiddenBaseIDs()
	matchPath := make(map[string]bool, len(ids))
	for rank, id := range ids {
		name := vol.index.compactNameAt(id)
		matchPath[strings.ToLower(name)] = rank == 0 || rank%2 == 0
	}
	readCount := 0
	var firstRead string
	contentDiskReadForVerify = func(path string, _ contentExtractSettings) ([]byte, bool) {
		readCount++
		if firstRead == "" {
			firstRead = path
		}
		if matchPath[strings.ToLower(filepath.Base(path))] {
			return []byte("needle"), true
		}
		return []byte("not a match"), true
	}
	t.Cleanup(func() {
		contentDiskReadForVerify = previousRead
		contentVerifyParallelTestHook = previousHook
	})

	oldProcs := runtime.GOMAXPROCS(8)
	t.Cleanup(func() { runtime.GOMAXPROCS(oldProcs) })

	seqOneBlocked := make(chan struct{})
	seqZeroDone := make(chan struct{})
	releaseSeqOne := make(chan struct{})
	var zeroOnce, releaseOnce sync.Once
	contentVerifyParallelTestHook = func(seq, _ int, phase string, _ int64, _, capped bool) {
		if seq == 0 && phase == "merge-done" {
			zeroOnce.Do(func() { close(seqZeroDone) })
		}
		if seq > 0 && phase == "merge-start" {
			<-seqZeroDone
			if seq == 1 {
				close(seqOneBlocked)
				<-releaseSeqOne
			}
		}
		if phase == "merge-done" && capped {
			releaseOnce.Do(func() { close(releaseSeqOne) })
		}
	}

	trace.initDiskBudget()
	matches, err := verifyCompactContentParallel(vol.index, pq, ids, make(map[int]string), 100, false, pq.hidden, vol, newContentLeafMatcher(pq))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-seqOneBlocked:
	default:
		t.Fatal("candidate 1 did not block at the deterministic frontier")
	}
	if len(matches) != 1 || matches[0].Name != "f000.txt" {
		t.Fatalf("parallel capped output=%v reads=%d firstRead=%q; want only the verified rank-0 hit before candidate 1's frontier", enrichmentPaths(matches), readCount, firstRead)
	}
	trace.publishDiskBudget()
	if !trace.DiskCapped || !trace.contentIncomplete() || trace.DiskVerified != 8 {
		t.Fatalf("cap trace capped=%v incomplete=%v reads=%d; want exact 8-read cap and incomplete", trace.DiskCapped, trace.contentIncomplete(), trace.DiskVerified)
	}

	// Exercise the other parallel ordered producer (the bounded candidate scan)
	// with the same false-positive distribution and an equivalent frontier race.
	scanTrace := &searchTrace{DiskVerifyBudget: 8}
	scanPQ := pq
	scanPQ.Trace = scanTrace
	scanOrder := vol.orderForQuery(scanPQ)
	for rank, id := range scanOrder {
		matchPath[strings.ToLower(vol.index.compactNameAt(int(id)))] = rank == 0 || rank%2 == 0
	}
	scanZeroDone := make(chan struct{})
	scanOneBlocked := make(chan struct{})
	releaseScanOne := make(chan struct{})
	var scanZeroOnce, scanReleaseOnce sync.Once
	contentVerifyParallelTestHook = func(seq, _ int, phase string, _ int64, _, capped bool) {
		if seq == 0 && phase == "scan-done" {
			scanZeroOnce.Do(func() { close(scanZeroDone) })
		}
		if seq > 0 && phase == "scan-start" {
			<-scanZeroDone
			if seq == 1 {
				close(scanOneBlocked)
				<-releaseScanOne
			}
		}
		if phase == "scan-done" && capped {
			scanReleaseOnce.Do(func() { close(releaseScanOne) })
		}
	}
	scanTrace.initDiskBudget()
	scanIDs, ok := vol.boundedScanCandidatesParallel(scanPQ, scanOrder, vol.index.compactRecordCount(), 100, nil, newContentLeafMatcher(scanPQ))
	if !ok {
		t.Fatal("parallel bounded scan declined its content query")
	}
	select {
	case <-scanOneBlocked:
	default:
		t.Fatal("bounded scan candidate 1 did not block at the deterministic frontier")
	}
	if len(scanIDs) != 1 || scanIDs[0] != int(scanOrder[0]) {
		t.Fatalf("parallel bounded-scan IDs=%v; want only rank-0 ID %d", scanIDs, scanOrder[0])
	}
	scanTrace.publishDiskBudget()
	if !scanTrace.DiskCapped || !scanTrace.contentIncomplete() || scanTrace.DiskVerified != 8 {
		t.Fatalf("bounded-scan cap trace capped=%v incomplete=%v reads=%d; want exact 8-read cap and incomplete", scanTrace.DiskCapped, scanTrace.contentIncomplete(), scanTrace.DiskVerified)
	}
}
