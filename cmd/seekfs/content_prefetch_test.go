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
