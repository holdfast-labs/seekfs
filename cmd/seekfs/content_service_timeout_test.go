package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// contentHangExtractor blocks until its context is canceled, modeling a
// pathological file. A per-document timeout is the only thing that unblocks it,
// so it proves one bad file cannot stall the serial drain.
type contentHangExtractor struct{}

func (contentHangExtractor) Name() string         { return "test-hang" }
func (contentHangExtractor) Version() uint16      { return 1 }
func (contentHangExtractor) Class() uint16        { return contentClassText }
func (contentHangExtractor) Extensions() []string { return []string{".hang"} }
func (contentHangExtractor) Sniff([]byte) bool    { return false }

func (contentHangExtractor) Extract(ctx context.Context, _ io.ReaderAt, _ int64) (contentExtractResult, error) {
	<-ctx.Done()
	return contentExtractResult{}, ctx.Err()
}

// A document whose extraction hangs must time out and be skipped, and the drain
// must continue to the next queued file instead of stalling.
func TestContentExtractDocTimeoutDoesNotStallDrain(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	restoreTimeout := contentExtractDocTimeout
	contentExtractDocTimeout = 50 * time.Millisecond
	t.Cleanup(func() { contentExtractDocTimeout = restoreTimeout })

	restoreExtractors := contentExtractors
	contentExtractors = append(append([]contentExtractor(nil), contentExtractors...), contentHangExtractor{})
	t.Cleanup(func() { contentExtractors = restoreExtractors })

	dir := t.TempDir()
	hangPath := filepath.Join(dir, "a.hang")
	okPath := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(hangPath, []byte("hang"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(okPath, []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}

	state := newContentVolumeState("T:")
	coord := newContentCoordinator(state)
	coord.enableDrain()
	coord.queue[1] = struct{}{}
	coord.queue[2] = struct{}{}
	resolve := func(frn uint64) (string, bool) {
		switch frn {
		case 1:
			return hangPath, true
		case 2:
			return okPath, true
		}
		return "", false
	}

	done := make(chan int, 1)
	go func() { done <- coord.processQueue(resolve) }()
	select {
	case changed := <-done:
		if changed != 1 {
			t.Fatalf("processQueue changed=%d; want 1 (the good doc)", changed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("processQueue stalled on the hanging extractor")
	}
	if _, ok := state.deltaView().priorHash(2); !ok {
		t.Fatal("the good document was not extracted after the timeout")
	}
	if h := state.healthSnapshot(0); h.ExtractionErrors == 0 {
		t.Fatal("a timed-out extraction must be recorded as an extraction error")
	}
}

// A ctx-ignoring extractor (blocks without observing ctx) must still be cut off
// by the drain's boundary-enforced deadline, not wedge it: only the wrapping
// goroutine+select can stop a parser that never checks ctx.
func TestContentExtractCtxIgnoringDoesNotStallDrain(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	restoreTimeout := contentExtractDocTimeout
	contentExtractDocTimeout = 50 * time.Millisecond
	t.Cleanup(func() { contentExtractDocTimeout = restoreTimeout })

	restoreExtractors := contentExtractors
	contentExtractors = append(append([]contentExtractor(nil), contentExtractors...), contentSpinExtractor{})
	t.Cleanup(func() { contentExtractors = restoreExtractors })

	dir := t.TempDir()
	spinPath := filepath.Join(dir, "a.spin")
	okPath := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(spinPath, []byte("hang"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(okPath, []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}

	state := newContentVolumeState("T:")
	coord := newContentCoordinator(state)
	coord.enableDrain()
	coord.queue[1] = struct{}{}
	coord.queue[2] = struct{}{}
	resolve := func(frn uint64) (string, bool) {
		switch frn {
		case 1:
			return spinPath, true
		case 2:
			return okPath, true
		}
		return "", false
	}

	done := make(chan int, 1)
	go func() { done <- coord.processQueue(resolve) }()
	select {
	case changed := <-done:
		if changed != 1 {
			t.Fatalf("processQueue changed=%d; want 1 (the good doc)", changed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("processQueue wedged on a ctx-ignoring extractor")
	}
	if _, ok := state.deltaView().priorHash(2); !ok {
		t.Fatal("the good document was not extracted after the timeout")
	}
	if h := state.healthSnapshot(0); h.ExtractionErrors == 0 {
		t.Fatal("a timed-out extraction must be recorded as an extraction error")
	}
}

// A panicking extractor in the drain must be a per-document skip, not a service
// crash, and must be counted as an extraction error.
func TestContentExtractPanicIsolatedInDrain(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	restoreExtractors := contentExtractors
	contentExtractors = append(append([]contentExtractor(nil), contentExtractors...), contentPanicExtractor{})
	t.Cleanup(func() { contentExtractors = restoreExtractors })

	dir := t.TempDir()
	panicPath := filepath.Join(dir, "a.panic")
	okPath := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(panicPath, []byte("boom"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(okPath, []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}

	state := newContentVolumeState("T:")
	coord := newContentCoordinator(state)
	coord.enableDrain()
	coord.queue[1] = struct{}{}
	coord.queue[2] = struct{}{}
	resolve := func(frn uint64) (string, bool) {
		switch frn {
		case 1:
			return panicPath, true
		case 2:
			return okPath, true
		}
		return "", false
	}

	if changed := coord.processQueue(resolve); changed != 1 {
		t.Fatalf("processQueue changed=%d; want 1 (the good doc)", changed)
	}
	if _, ok := state.deltaView().priorHash(2); !ok {
		t.Fatal("the good document was not extracted after the panic")
	}
	if h := state.healthSnapshot(0); h.ExtractionErrors == 0 {
		t.Fatal("a panicking extraction must be recorded as an extraction error")
	}
}
