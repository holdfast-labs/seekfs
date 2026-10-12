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
// behaviour are unchanged. The service build drops to 1 worker while the user
// is actively querying or the machine is on battery
// (contentBuildWorkerCount); the offline CLI keeps the static cap because it
// is a foreground command the user explicitly started. A var so tests can pin
// it.
var contentBuildWorkers = func() int {
	n := runtime.NumCPU()
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

// contentBuildWorkerCount is the adaptive extraction concurrency for the
// service-owned background build: the static cap, dropped to 1 while the user
// is actively querying or the machine is on battery. The offline CLI keeps
// the static cap: it is a foreground command the user explicitly started.
// Ordering, budget and abort semantics are unchanged at any count because the
// prefetch stream is consumed in item order. The streaming assembler reuses
// the same count for its tokenize stage.
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
