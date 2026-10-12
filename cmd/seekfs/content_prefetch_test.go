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
	restoreWorkers := contentBuildWorkers
	restoreActive := contentUserActiveNow
	restoreBattery := contentOnBatteryNow
	t.Cleanup(func() {
		contentBuildWorkers = restoreWorkers
		contentUserActiveNow = restoreActive
		contentOnBatteryNow = restoreBattery
	})
	contentBuildWorkers = 4
	quiet, active := false, true
	mains, battery := false, true
	contentUserActiveNow = func() bool { return quiet }
	contentOnBatteryNow = func() bool { return mains }
	if got := contentBuildWorkerCount(); got != 4 {
		t.Fatalf("quiet+mains workers = %d; want the static cap 4", got)
	}
	quiet = active
	if got := contentBuildWorkerCount(); got != 1 {
		t.Fatalf("active-query workers = %d; want 1", got)
	}
	quiet = !active
	contentOnBatteryNow = func() bool { return battery }
	if got := contentBuildWorkerCount(); got != 1 {
		t.Fatalf("battery workers = %d; want 1", got)
	}
	// A static cap of 1 stays 1 whatever the probes say.
	contentBuildWorkers = 1
	if got := contentBuildWorkerCount(); got != 1 {
		t.Fatalf("single-worker cap = %d; want 1", got)
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
