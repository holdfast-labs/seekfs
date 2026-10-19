package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// slowServiceContentVolume builds an all-matching content corpus whose disk
// verification reads take delay each, and returns the number of reads performed.
func slowServiceContentVolume(t *testing.T, files int, delay time.Duration) (*serviceVolumeIndex, *atomic.Int64) {
	t.Helper()
	fixture := make([]contentFixtureFile, files)
	for i := range fixture {
		fixture[i] = contentFixtureFile{frn: uint64(i + 2), name: fmt.Sprintf("file-%04d.txt", i), text: "needle in indexed text"}
	}
	vol := newContentQueryVolume(t, fixture)
	uncappedDiskVerify(t)
	prev := contentDiskReadForVerify
	var reads atomic.Int64
	contentDiskReadForVerify = func(path string, s contentExtractSettings) ([]byte, bool) {
		reads.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
		return prev(path, s)
	}
	t.Cleanup(func() { contentDiskReadForVerify = prev })
	return vol, &reads
}

// TestServiceCountIncompleteBeforeClientTimeout proves the service arms a
// content deadline from the client's own wait: a count that would outlive the
// client settles as an incomplete refusal before the exchange times out, so the
// caller never sees the 30 s "request timed out" error.
func TestServiceCountIncompleteBeforeClientTimeout(t *testing.T) {
	vol, _ := slowServiceContentVolume(t, 200, 30*time.Millisecond)
	pipe := startCLISilentTestService(t, []*serviceVolumeIndex{vol})

	const clientWait = time.Second
	prevHook := serviceClientTimeoutHook
	serviceClientTimeoutHook = func(serviceRequest) time.Duration { return clientWait }
	t.Cleanup(func() { serviceClientTimeoutHook = prevHook })

	start := time.Now()
	resp, err := callService(pipe, serviceRequest{
		Command:                 "search",
		Query:                   "content:needle",
		CountOnly:               true,
		Limit:                   1,
		ContentDiskVerifyBudget: 100000,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("count exchange err = %v; want an incomplete response before %v", err, clientWait)
	}
	if resp.OK {
		t.Fatalf("count OK with %d matches; want an incomplete refusal", resp.Count)
	}
	if !strings.Contains(resp.Message, "incomplete") {
		t.Fatalf("count message = %q; want the existing incomplete wording", resp.Message)
	}
	if !resp.BudgetExceeded {
		t.Fatal("count response did not mark budget_exceeded")
	}
	if elapsed >= clientWait {
		t.Fatalf("count took %v; want it settled before the %v client wait", elapsed, clientWait)
	}
}

// TestServiceExplicitBudgetWinsOverClientTimeout proves an explicit --budget
// smaller than the implicit client bound still selects the deadline: the
// response's absolute content deadline is the explicit one, not the client one.
func TestServiceExplicitBudgetWinsOverClientTimeout(t *testing.T) {
	vol, _ := slowServiceContentVolume(t, 200, 30*time.Millisecond)
	pipe := startCLISilentTestService(t, []*serviceVolumeIndex{vol})

	prevHook := serviceClientTimeoutHook
	serviceClientTimeoutHook = func(serviceRequest) time.Duration { return 5 * time.Second }
	t.Cleanup(func() { serviceClientTimeoutHook = prevHook })

	resp, err := callService(pipe, serviceRequest{
		Command:                 "search",
		Query:                   "content:needle",
		CountOnly:               true,
		Limit:                   1,
		BudgetMS:                40,
		ContentDiskVerifyBudget: 100000,
	})
	if err != nil {
		t.Fatalf("count exchange err = %v", err)
	}
	if resp.OK || !strings.Contains(resp.Message, "incomplete") {
		t.Fatalf("count response = (ok=%v, message=%q); want an incomplete refusal", resp.OK, resp.Message)
	}
	if resp.ContentBudgetDeadlineUnix == 0 {
		t.Fatal("count response dropped the content deadline")
	}
	age := time.Since(time.Unix(0, resp.ContentBudgetDeadlineUnix))
	if age < -time.Second {
		t.Fatalf("content deadline is %v in the future; explicit 40ms budget did not win", -age)
	}
}

// TestServiceCanceledCountStopsDiskReads proves a canceled request stops
// verifying: the tally reads only the candidates before the cancellation check
// and never drains the whole corpus.
func TestServiceCanceledCountStopsDiskReads(t *testing.T) {
	const files = 2000
	vol, reads := slowServiceContentVolume(t, files, 0)
	svc := &goSearchService{volumes: []*serviceVolumeIndex{vol}, indexes: []*Index{vol.index}}

	resp := callTestServiceCommand(t, svc, serviceRequest{
		Command:                 "search",
		Query:                   "content:needle",
		CountOnly:               true,
		Limit:                   1,
		ContentDiskVerifyBudget: 100000,
		CancelOverride:          func() bool { return reads.Load() >= 5 },
	})
	if resp.OK {
		t.Fatalf("canceled count answered %d matches; want a canceled refusal", resp.Count)
	}
	if !strings.Contains(resp.Message, errQueryCanceled.Error()) {
		t.Fatalf("canceled count message = %q; want %q", resp.Message, errQueryCanceled.Error())
	}
	if got := reads.Load(); got >= files {
		t.Fatalf("canceled count read %d/%d files; want verification to stop early", got, files)
	}
}

// TestServicePipeDisconnectErrOnlyDisconnects pins the cancel trigger: only
// the pipe-disconnect errors cancel a request; any other PeekNamedPipe failure
// keeps a live client's query running.
func TestServicePipeDisconnectErrOnlyDisconnects(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{syscall.Errno(109), true},
		{syscall.Errno(232), true},
		{syscall.Errno(233), true},
		{syscall.Errno(5), false},
		{syscall.Errno(6), false},
		{nil, false},
	} {
		if got := servicePipeDisconnectErr(c.err); got != c.want {
			t.Errorf("servicePipeDisconnectErr(%v) = %v; want %v", c.err, got, c.want)
		}
	}
}
