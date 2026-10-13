package main

// Regression tests for the silent-empty CLI bug: service-side failures
// (unreachable pipe, rejected query shapes, refused content counts) used to
// be swallowed by cmdSearch, which returned nil with nothing printed — exit 0
// with no stdout and no stderr. Scripts and agents read that as "no matches".
// Every test below fails (run returns nil) without the cmdSearch fix.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// startCLISilentTestService serves vols on a unique test-only pipe and stops
// the listener on test cleanup. It mirrors the standalone-service test
// pattern without touching the live service.
func startCLISilentTestService(t *testing.T, vols []*serviceVolumeIndex) string {
	t.Helper()
	idxs := make([]*Index, 0, len(vols))
	for _, vol := range vols {
		idxs = append(idxs, vol.index)
	}
	pipeName := fmt.Sprintf(`\\.\pipe\seekfs-clisilent-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	svc := &goSearchService{
		pipeName: pipeName,
		sddl:     standaloneTestPipeSDDL,
		stop:     make(chan struct{}),
		indexes:  idxs,
		volumes:  vols,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.servePipeListener()
	}()
	var stopOnce sync.Once
	t.Cleanup(func() {
		stopOnce.Do(func() { close(svc.stop) })
		if handle, err := openPipeClientWithTimeout(pipeName, 100*time.Millisecond); err == nil {
			_ = os.NewFile(uintptr(handle), pipeName).Close()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("test service listener did not stop")
		}
	})
	return pipeName
}

// runCLICaptured runs the CLI entry point with os.Stdout captured, returning
// whatever the command printed plus the error run() returned. (main() turns a
// non-nil error into stderr text plus a nonzero exit; a nil error here with
// empty output is the silent-success bug.)
func runCLICaptured(t *testing.T, args []string) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := run(args)
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return string(out), runErr
}

// An unsupported filter shape (per `seekfs syntax`, `case:` takes no value;
// `case:Makefile` is rejected, not treated as a literal) must surface as an
// error, not as silent empty success.
func TestCLISearchSurfacesUnsupportedFilterError(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	pipe := startCLISilentTestService(t, []*serviceVolumeIndex{vol})
	for _, args := range [][]string{
		{"search", "-service", "-pipe", pipe, "-n", "1", "case:Makefile"},
		{"search", "-service", "-pipe", pipe, "--json", "case:Makefile"},
		{"count", "-service", "-pipe", pipe, "case:Makefile"},
	} {
		out, err := runCLICaptured(t, args)
		if err == nil {
			t.Fatalf("run(%q) = nil error with output %q; want the unsupported-filter error", args, out)
		}
		if !strings.Contains(err.Error(), "unsupported filter") {
			t.Fatalf("run(%q) err = %v; want unsupported-filter error", args, err)
		}
		if strings.TrimSpace(out) != "" {
			t.Fatalf("run(%q) printed %q alongside the error; want no stdout", args, out)
		}
	}
}

// An unreachable service pipe must surface as an error, not as silent empty
// success.
func TestCLISearchSurfacesUnreachablePipeError(t *testing.T) {
	dead := fmt.Sprintf(`\\.\pipe\seekfs-clisilent-dead-%d-%d`, os.Getpid(), time.Now().UnixNano())
	for _, args := range [][]string{
		{"search", "-service", "-pipe", dead, "main.cc"},
		{"search", "-service", "-pipe", dead, "--json", "main.cc"},
		{"count", "-service", "-pipe", dead, "main.cc"},
	} {
		out, err := runCLICaptured(t, args)
		if err == nil {
			t.Fatalf("run(%q) = nil error with output %q; want the pipe error", args, out)
		}
		if strings.TrimSpace(out) != "" {
			t.Fatalf("run(%q) printed %q alongside the error; want no stdout", args, out)
		}
	}
}

// A degraded (truncated) content index answers searches with complete=false
// but refuses exact counts: the refusal must surface as an error while the
// degraded search still prints its results.
func TestCLICountSurfacesIncompleteContentRefusal(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "notes.txt", "alpha needle beta"},
	})
	vol.content.markCatchUpIncomplete()
	pipe := startCLISilentTestService(t, []*serviceVolumeIndex{vol})

	out, err := runCLICaptured(t, []string{"count", "-service", "-pipe", pipe, "content:needle"})
	if err == nil {
		t.Fatalf("count run = nil error with output %q; want the incomplete-content refusal", out)
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("count run err = %v; want incomplete-content refusal", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("count run printed %q alongside the error; want no stdout", out)
	}

	// Control: the same query as a search still succeeds (degraded) and
	// reports itself incomplete instead of erroring.
	searchOut, searchErr := runCLICaptured(t, []string{"search", "-service", "-pipe", pipe, "-n", "10", "--json", "content:needle"})
	if searchErr != nil {
		t.Fatalf("degraded content search err = %v; want success with complete=false", searchErr)
	}
	if !strings.Contains(searchOut, `"complete": false`) {
		t.Fatalf("degraded content search output = %q; want \"complete\": false", searchOut)
	}
}

// A messageless service rejection must still produce a readable CLI error,
// never a blank stderr line.
func TestServiceResponseErrorNeverEmpty(t *testing.T) {
	if err := serviceResponseError(serviceResponse{}); err == nil || strings.TrimSpace(err.Error()) == "" {
		t.Fatalf("serviceResponseError(empty) = %v; want a non-empty error", err)
	}
	const msg = "content count is incomplete: candidate budget exceeded"
	if err := serviceResponseError(serviceResponse{Message: msg}); err == nil || err.Error() != msg {
		t.Fatalf("serviceResponseError(message) = %v; want %q", err, msg)
	}
}
