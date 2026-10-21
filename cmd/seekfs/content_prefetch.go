package main

import (
	"context"
	"os"
	"runtime"
	"strings"
	"time"
)

// contentBuildWorkers is how many files a build extracts concurrently.
// Extraction is IO-bound on small files (open, read, decode), so an idle
// build scales with the machine: NumCPU workers (capped), not a fixed small
// number. Results are still consumed in order, so selection, budget and abort
// behaviour are unchanged. The service build throttles to 1 worker while the
// user is active (contentBuildWorkerCount); the offline CLI keeps the static
// cap because it is a foreground command the user explicitly started. A var
// so tests can pin it.
var contentBuildWorkers = func() int {
	n := runtime.NumCPU()
	// GOMAXPROCS-aware: a capped process must not size the pool from CPUs it
	// cannot use — each worker holds an open file plus a decoded document, so
	// the pool is the memory bound on a low-memory box.
	if g := runtime.GOMAXPROCS(0); g >= 1 && g < n {
		n = g
	}
	if n < 1 {
		n = 1
	}
	if n > 16 {
		n = 16
	}
	return n
}()

// contentBuildQueryQuietWindow is how recently a query must have run for the
// service build to consider the user active and drop to one worker.
var contentBuildQueryQuietWindow = 30 * time.Second

// contentUserActiveNow and contentOnBatteryNow are the adaptive-policy probes.
// They are vars so tests can inject activity and power states deterministically.
var (
	contentUserActiveNow = func() bool {
		last := serviceLastQueryUnixNano.Load()
		if last == 0 {
			return false
		}
		return time.Since(time.Unix(0, last)) < contentBuildQueryQuietWindow
	}
	contentOnBatteryNow = contentSystemOnBattery
)

// contentLowerIOPriorityNow reports whether newly opened content handles
// should take the Low disk-priority hint: only while the user is actively
// querying or the machine is on battery. An idle build keeps normal priority:
// the Low hint measurably throttles reads even when the disk would otherwise
// be idle, and there is nothing to yield to. SEEKFS_CONTENT_IOPRIORITY forces
// the policy ("low" always lowers, anything else never does). The Windows
// opener consults it; other platforms open plainly.
func contentLowerIOPriorityNow() bool {
	if v := strings.TrimSpace(os.Getenv("SEEKFS_CONTENT_IOPRIORITY")); v != "" {
		return strings.EqualFold(v, "low")
	}
	return contentUserActiveNow() || contentOnBatteryNow()
}

// contentBuildWorkerCount is the desired extraction concurrency for the
// service-owned background build right now: the static cap, dropped to 1
// while the user is active — a recent or in-flight foreground query,
// foreground input (the governor input signal, UI heartbeat included), or a
// discharging battery. It is re-sampled continuously during the build (the
// adaptive prefetch below), never frozen at build start, so a quiet build
// runs full and only slows while load is actually present. Background CPU or
// disk pressure does NOT drop workers (the pause governor still yields the
// batch sleeps to those); only user-visible activity serializes extraction.
// The offline CLI keeps the static cap: it is a foreground command the user
// explicitly started. Ordering, budget and abort semantics are unchanged at
// any count because the prefetch stream is consumed in item order. The
// streaming assembler reuses the same count for its tokenize stage.
func contentBuildWorkerCount() int {
	if contentBuildWorkers <= 1 {
		return contentBuildWorkers
	}
	if contentUserActiveNow() || contentBuildQueriesActiveNow() || contentInputActiveNow() || contentOnBatteryNow() {
		return 1
	}
	return contentBuildWorkers
}

// contentBuildAdaptivePrefetch is the service build's extraction stream with
// a worker count that re-samples mid-build. The item slice is covered by
// consecutive fixed-size chunks (chunk = the governor batch, the same point
// that already re-samples every probe); each chunk runs as one
// contentPrefetchExtract stream with the desired count at chunk start, and
// adapt opens the next chunk at the new count. Delivery is strictly in item
// order across chunks — a worker change never reorders or repeats a
// document — and selection, budget and progress semantics are unchanged. No
// extra timers or goroutines run during the build: adapt is called from the
// existing batch-boundary branch. Only the service build adapts; the offline
// CLI and the fold create their prefetch once with their (static) count.
type contentBuildAdaptivePrefetch struct {
	ctx     context.Context
	items   []contentBuildItem
	extract func(context.Context, contentBuildItem) (contentBuildDoc, contentExtractResult, bool)
	pos     int
	left    int // undelivered items in the live stream
	nextFn  func() (contentExtracted, bool)
	stopFn  func()
	want    int
}

// newContentBuildAdaptivePrefetch opens the stream at the current desired
// worker count. workerCount() reports that count.
func newContentBuildAdaptivePrefetch(ctx context.Context, items []contentBuildItem, extract func(context.Context, contentBuildItem) (contentBuildDoc, contentExtractResult, bool)) *contentBuildAdaptivePrefetch {
	a := &contentBuildAdaptivePrefetch{ctx: ctx, items: items, extract: extract}
	a.want = contentBuildWorkerCount()
	a.startChunk()
	return a
}

// startChunk covers the next chunk with the recorded worker count. The
// previous stream, if any, is already fully delivered when this runs (chunks
// meet exactly at batch boundaries), so stopping it frees workers without
// stranding in-flight work — contentPrefetchExtract drains into its buffered
// slots on stop.
func (a *contentBuildAdaptivePrefetch) startChunk() {
	if a.stopFn != nil {
		a.stopFn()
		a.nextFn, a.stopFn = nil, nil
	}
	if a.pos >= len(a.items) {
		a.left = 0
		return
	}
	n := contentBuildBatchSize
	if n < 1 {
		n = 1
	}
	if n > len(a.items)-a.pos {
		n = len(a.items) - a.pos
	}
	w := a.want
	if w < 1 {
		w = 1
	}
	next, stop := contentPrefetchExtract(a.ctx, a.items[a.pos:a.pos+n], w, a.extract)
	a.nextFn, a.stopFn = next, stop
	a.left = n
}

// next delivers the next extraction in item order, opening the following
// chunk transparently at a boundary. It returns false once every item is
// delivered or the context is canceled.
func (a *contentBuildAdaptivePrefetch) next() (contentExtracted, bool) {
	if a.nextFn == nil {
		if a.pos >= len(a.items) {
			return contentExtracted{}, false
		}
		a.startChunk()
	}
	got, live := a.nextFn()
	if !live {
		return contentExtracted{}, false
	}
	a.left--
	a.pos++
	if a.left == 0 {
		a.stopFn()
		a.nextFn, a.stopFn = nil, nil
	}
	return got, true
}

// adapt re-samples the desired worker count. At a chunk boundary (where the
// build loop calls it) the next chunk opens immediately at the new count;
// mid-chunk it only records, and the following chunk picks it up.
func (a *contentBuildAdaptivePrefetch) adapt() {
	a.want = contentBuildWorkerCount()
	if a.nextFn == nil && a.pos < len(a.items) {
		a.startChunk()
	}
}

// workerCount reports the count the live (or next) chunk runs at.
func (a *contentBuildAdaptivePrefetch) workerCount() int {
	if a.want < 1 {
		return 1
	}
	return a.want
}

// stop cancels outstanding work; in-flight extractions finish into buffered
// slots, so no goroutine is left blocked.
func (a *contentBuildAdaptivePrefetch) stop() {
	if a.stopFn != nil {
		a.stopFn()
		a.nextFn, a.stopFn = nil, nil
	}
}

// contentExtracted is one prefetched extraction.
type contentExtracted struct {
	doc contentBuildDoc
	res contentExtractResult
	ok  bool
}

// contentPrefetchExtract extracts items on up to workers goroutines, at most
// 2*workers ahead of the consumer, and returns them in item order. next
// returns false once every item is delivered or the context is canceled.
// stop cancels outstanding work; in-flight extractions finish into buffered
// slots, so no goroutine is left blocked.
func contentPrefetchExtract(ctx context.Context, items []contentBuildItem, workers int, extract func(context.Context, contentBuildItem) (contentBuildDoc, contentExtractResult, bool)) (next func() (contentExtracted, bool), stop func()) {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	queue := make(chan chan contentExtracted, 2*workers)
	sem := make(chan struct{}, workers)
	go func() {
		defer close(queue)
		for _, item := range items {
			slot := make(chan contentExtracted, 1)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case queue <- slot:
			case <-ctx.Done():
				<-sem
				return
			}
			go func(item contentBuildItem) {
				defer func() { <-sem }()
				doc, res, ok := extract(ctx, item)
				slot <- contentExtracted{doc: doc, res: res, ok: ok}
			}(item)
		}
	}()
	next = func() (contentExtracted, bool) {
		slot, ok := <-queue
		if !ok {
			return contentExtracted{}, false
		}
		select {
		case r := <-slot:
			return r, true
		case <-ctx.Done():
			return contentExtracted{}, false
		}
	}
	return next, cancel
}
