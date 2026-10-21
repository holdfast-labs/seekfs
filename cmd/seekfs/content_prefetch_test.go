package main

import (
	"context"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"
)

func TestContentPrefetchExtractKeepsOrderAndBoundsWork(t *testing.T) {
	items := make([]contentBuildItem, 200)
	for i := range items {
		items[i] = contentBuildItem{frn: uint64(i)}
	}
	var inFlight, peak atomic.Int32
	extract := func(_ context.Context, item contentBuildItem) (contentBuildDoc, contentExtractResult, bool) {
		n := inFlight.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(time.Duration(rand.Intn(300)) * time.Microsecond)
		inFlight.Add(-1)
		return contentBuildDoc{frn: item.frn}, contentExtractResult{}, item.frn%3 != 0
	}
	next, stop := contentPrefetchExtract(context.Background(), items, 4, extract)
	defer stop()
	for i := range items {
		got, live := next()
		if !live || got.doc.frn != uint64(i) || got.ok != (i%3 != 0) {
			t.Fatalf("item %d: got frn=%d ok=%v live=%v", i, got.doc.frn, got.ok, live)
		}
	}
	if _, live := next(); live {
		t.Fatal("next after the last item must report done")
	}
	if p := peak.Load(); p > 4 || p < 2 {
		t.Fatalf("peak concurrent extractions = %d; want 2..4", p)
	}
}

func TestContentBuildWorkerCountAdaptive(t *testing.T) {
	stubGovernorSignals(t)
	restoreWorkers := contentBuildWorkers
	restoreBatch := contentBuildBatchSize
	t.Cleanup(func() {
		contentBuildWorkers = restoreWorkers
		contentBuildBatchSize = restoreBatch
	})
	contentBuildWorkers = 4
	if got := contentBuildWorkerCount(); got != 4 {
		t.Fatalf("quiet+mains workers = %d; want the static cap 4", got)
	}
	// Each user-activity signal alone drops the build to one worker.
	serviceNoteQueryActivity()
	if got := contentBuildWorkerCount(); got != 1 {
		t.Fatalf("recent-query workers = %d; want 1", got)
	}
	serviceLastQueryUnixNano.Store(0)
	serviceActiveQueryEnter()
	defer serviceActiveQueryLeave()
	if got := contentBuildWorkerCount(); got != 1 {
		t.Fatalf("in-flight-query workers = %d; want 1", got)
	}
	serviceActiveQueryLeave()
	contentInputActiveNow = func() bool { return true }
	if got := contentBuildWorkerCount(); got != 1 {
		t.Fatalf("input workers = %d; want 1", got)
	}
	contentInputActiveNow = func() bool { return false }
	contentOnBatteryNow = func() bool { return true }
	if got := contentBuildWorkerCount(); got != 1 {
		t.Fatalf("battery workers = %d; want 1", got)
	}
	contentOnBatteryNow = func() bool { return false }
	if got := contentBuildWorkerCount(); got != 4 {
		t.Fatalf("quiet-again workers = %d; want the static cap 4", got)
	}
	// Background CPU/disk pressure slows pauses but must not serialize
	// extraction: workers stay at the cap.
	contentSystemBusyNow = func() bool { return true }
	contentDiskBusyNow = func() bool { return true }
	if got := contentBuildWorkerCount(); got != 4 {
		t.Fatalf("cpu+disk workers = %d; want the static cap 4", got)
	}
	// A static cap of 1 stays 1 whatever the probes say.
	contentBuildWorkers = 1
	if got := contentBuildWorkerCount(); got != 1 {
		t.Fatalf("single-worker cap = %d; want 1", got)
	}
}

// The adaptive prefetch must scale the live extraction stream down while
// queries or input are active and back up when they go quiet — delivering
// every document exactly once and in item order throughout. A fake load
// signal drives the governor probes; the batch size pins the chunking, so
// no sleeps are involved.
func TestContentBuildAdaptivePrefetchScalesDownAndUp(t *testing.T) {
	stubGovernorSignals(t)
	restoreWorkers := contentBuildWorkers
	restoreBatch := contentBuildBatchSize
	t.Cleanup(func() {
		contentBuildWorkers = restoreWorkers
		contentBuildBatchSize = restoreBatch
	})
	contentBuildWorkers = 4
	contentBuildBatchSize = 4
	const n = 24
	items := make([]contentBuildItem, n)
	for i := range items {
		items[i] = contentBuildItem{frn: uint64(i + 1)}
	}
	extract := func(_ context.Context, item contentBuildItem) (contentBuildDoc, contentExtractResult, bool) {
		return contentBuildDoc{frn: item.frn}, contentExtractResult{}, true
	}
	a := newContentBuildAdaptivePrefetch(context.Background(), items, extract)
	defer a.stop()
	if got := a.workerCount(); got != 4 {
		t.Fatalf("initial workers = %d; want 4", got)
	}
	// Drain the first chunk quiet, then load starts: the next adapt must
	// drop the stream to 1 worker.
	for i := 0; i < 4; i++ {
		got, live := a.next()
		if !live || got.doc.frn != uint64(i+1) {
			t.Fatalf("chunk item %d: frn=%d live=%v; want frn=%d", i, got.doc.frn, live, i+1)
		}
	}
	serviceNoteQueryActivity()
	a.adapt()
	if got := a.workerCount(); got != 1 {
		t.Fatalf("loaded workers = %d; want 1", got)
	}
	// Drain loaded chunks: order must hold through the 4->1 transition.
	for i := 4; i < 12; i++ {
		got, live := a.next()
		if !live || got.doc.frn != uint64(i+1) {
			t.Fatalf("loaded item %d: frn=%d live=%v; want frn=%d", i, got.doc.frn, live, i+1)
		}
		a.adapt()
		if got := a.workerCount(); got != 1 {
			t.Fatalf("loaded workers at item %d = %d; want 1", i, got)
		}
	}
	// Load clears mid-build: the stream scales back to the cap and still
	// delivers the tail exactly once, in order.
	serviceLastQueryUnixNano.Store(0)
	a.adapt()
	if got := a.workerCount(); got != 4 {
		t.Fatalf("recovered workers = %d; want 4", got)
	}
	for i := 12; i < n; i++ {
		got, live := a.next()
		if !live || got.doc.frn != uint64(i+1) {
			t.Fatalf("tail item %d: frn=%d live=%v; want frn=%d", i, got.doc.frn, live, i+1)
		}
	}
	if _, live := a.next(); live {
		t.Fatal("next after the last item must report done")
	}
}

func TestContentUserActiveNowReadsQueryActivity(t *testing.T) {
	restoreWindow := contentBuildQueryQuietWindow
	contentBuildQueryQuietWindow = time.Minute
	t.Cleanup(func() { contentBuildQueryQuietWindow = restoreWindow })
	serviceLastQueryUnixNano.Store(0)
	if contentUserActiveNow() {
		t.Fatal("no recorded query must read as idle")
	}
	serviceNoteQueryActivity()
	if !contentUserActiveNow() {
		t.Fatal("a just-recorded query must read as active")
	}
	serviceLastQueryUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	if contentUserActiveNow() {
		t.Fatal("an hour-old query must read as idle")
	}
	serviceLastQueryUnixNano.Store(0)
}

func TestContentPrefetchExtractStopsOnCancel(t *testing.T) {
	items := make([]contentBuildItem, 1000)
	var calls atomic.Int32
	extract := func(context.Context, contentBuildItem) (contentBuildDoc, contentExtractResult, bool) {
		calls.Add(1)
		return contentBuildDoc{}, contentExtractResult{}, true
	}
	next, stop := contentPrefetchExtract(context.Background(), items, 2, extract)
	next()
	stop()
	for {
		if _, live := next(); !live {
			break
		}
	}
	time.Sleep(10 * time.Millisecond)
	if c := calls.Load(); c > 10 {
		t.Fatalf("extract ran %d times after stop; want only the small read-ahead window", c)
	}
}
