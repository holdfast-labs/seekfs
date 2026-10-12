package main

import (
	"context"
	"runtime"
	"time"
)

// contentBuildWorkers is how many files a build extracts concurrently.
// Extraction (open, read, decode) dominated a serial Chromium build at about
// 8 ms per file; results are still consumed in order, so selection, budget
// and abort behaviour are unchanged. A var so tests can pin it.
var contentBuildWorkers = func() int {
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
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

// contentBuildWorkerCount is the adaptive extraction concurrency for the
// service-owned background build: the static cap, dropped to 1 while the user
// is actively querying or the machine is on battery. The offline CLI keeps
// the static cap: it is a foreground command the user explicitly started.
// Ordering, budget and abort semantics are unchanged at any count because the
// prefetch stream is consumed in item order.
func contentBuildWorkerCount() int {
	if contentBuildWorkers <= 1 {
		return contentBuildWorkers
	}
	if contentUserActiveNow() || contentOnBatteryNow() {
		return 1
	}
	return contentBuildWorkers
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
