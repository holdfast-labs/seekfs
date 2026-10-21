package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestContentExtractionErrorReasonClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not exist", &os.PathError{Op: "open", Path: `C:\gone.txt`, Err: syscall.Errno(2)}, contentExtractReasonNotFound},
		{"path not found", &os.PathError{Op: "open", Path: `C:\gone\a.txt`, Err: syscall.Errno(3)}, contentExtractReasonNotFound},
		{"fs not exist", os.ErrNotExist, contentExtractReasonNotFound},
		{"access denied", &os.PathError{Op: "open", Path: `C:\locked\a.txt`, Err: syscall.Errno(5)}, contentExtractReasonAccessDenied},
		{"permission", os.ErrPermission, contentExtractReasonAccessDenied},
		{"sharing violation", &os.PathError{Op: "open", Path: `C:\busy\a.txt`, Err: syscall.Errno(32)}, contentExtractReasonSharingViolation},
		{"lock violation", &os.PathError{Op: "open", Path: `C:\busy\a.txt`, Err: syscall.Errno(33)}, contentExtractReasonSharingViolation},
		{"extractor", fmt.Errorf("%w: %w", errContentExtractFailure, errors.New("bad xref")), contentExtractReasonExtractor},
		{"panic", errContentExtractPanic, contentExtractReasonExtractor},
		{"timeout", context.DeadlineExceeded, contentExtractReasonTimeout},
		{"io", &os.PathError{Op: "read", Path: `C:\a.txt`, Err: syscall.Errno(1117)}, contentExtractReasonIO},
		{"other", errors.New("unexpected"), contentExtractReasonOther},
	}
	for _, tc := range cases {
		if got := contentExtractionErrorReason(tc.err); got != tc.want {
			t.Errorf("%s: reason = %q; want %q", tc.name, got, tc.want)
		}
	}
}

// Health must surface the per-reason split in JSON, and a vanished file must
// not move the error total.
func TestContentHealthReportsExtractionErrorReasons(t *testing.T) {
	state := newContentVolumeState("T:")
	state.noteExtractionError(`C:\gone.txt`, &os.PathError{Op: "open", Path: `C:\gone.txt`, Err: syscall.Errno(2)})
	state.noteExtractionError(`C:\denied.txt`, &os.PathError{Op: "open", Path: `C:\denied.txt`, Err: syscall.Errno(5)})
	state.noteExtractionError(`C:\busy.txt`, &os.PathError{Op: "open", Path: `C:\busy.txt`, Err: syscall.Errno(32)})
	state.noteExtractionError(`C:\bad.pdf`, fmt.Errorf("%w: %w", errContentExtractFailure, errors.New("bad xref")))

	h := state.healthSnapshot(0)
	if h.ExtractionErrors != 3 {
		t.Fatalf("ExtractionErrors = %d; want 3 (not_found is not an error)", h.ExtractionErrors)
	}
	if h.Vanished != 1 {
		t.Fatalf("Vanished = %d; want 1", h.Vanished)
	}
	for reason, want := range map[string]int{
		contentExtractReasonAccessDenied:     1,
		contentExtractReasonSharingViolation: 1,
		contentExtractReasonExtractor:        1,
	} {
		if got := h.ExtractErrorReasons[reason]; got != want {
			t.Errorf("reason %q = %d; want %d", reason, got, want)
		}
	}
	if _, ok := h.ExtractErrorReasons[contentExtractReasonNotFound]; ok {
		t.Fatal("not_found must not appear in extract_error_reasons")
	}

	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		ExtractionErrors    int            `json:"extract_errors"`
		ExtractErrorReasons map[string]int `json:"extract_error_reasons"`
		Vanished            int            `json:"extract_vanished"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.ExtractionErrors != 3 || raw.Vanished != 1 {
		t.Fatalf("health JSON totals = %+v; body %s", raw, b)
	}
	if raw.ExtractErrorReasons[contentExtractReasonSharingViolation] != 1 {
		t.Fatalf("health JSON has no per-reason map: %s", b)
	}
}

// A file that vanished between selection and extraction is a transient and
// must not keep the volume degraded or move the extraction-error total.
func TestVanishedExtractionIsNotAnError(t *testing.T) {
	state := contentTestState()
	coord := newContentCoordinator(state)
	coord.enableDrain()
	coord.queue[1] = struct{}{}
	missing := filepath.Join(t.TempDir(), "gone.txt")
	changed := coord.processQueue(func(uint64) (string, bool) { return missing, true })
	if changed != 0 {
		t.Fatalf("processQueue changed = %d; want 0", changed)
	}
	h := state.healthSnapshot(0)
	if h.ExtractionErrors != 0 {
		t.Fatalf("ExtractionErrors = %d; a vanished file must not count", h.ExtractionErrors)
	}
	if h.Vanished != 1 {
		t.Fatalf("Vanished = %d; want 1", h.Vanished)
	}
	if h.State == contentStateDegraded || h.State == contentStateStale {
		t.Fatalf("vanished file degraded the volume: %+v", h)
	}
}

// contentErrorExtractor always fails, modeling a broken parser. The drain must
// count it under the extractor reason, not under I/O.
type contentErrorExtractor struct{}

func (contentErrorExtractor) Name() string         { return "test-error" }
func (contentErrorExtractor) Version() uint16      { return 1 }
func (contentErrorExtractor) Class() uint16        { return contentClassText }
func (contentErrorExtractor) Extensions() []string { return []string{".errx"} }
func (contentErrorExtractor) Sniff([]byte) bool    { return false }

func (contentErrorExtractor) Extract(context.Context, io.ReaderAt, int64) (contentExtractResult, error) {
	return contentExtractResult{}, errors.New("extractor exploded")
}

func TestExtractorFailureIsClassifiedAsExtractor(t *testing.T) {
	restore := contentExtractors
	contentExtractors = append(append([]contentExtractor(nil), contentExtractors...), contentErrorExtractor{})
	t.Cleanup(func() { contentExtractors = restore })

	dir := t.TempDir()
	path := filepath.Join(dir, "broken.errx")
	if err := os.WriteFile(path, []byte("not really a document"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := contentTestState()
	coord := newContentCoordinator(state)
	coord.enableDrain()
	coord.queue[1] = struct{}{}
	if changed := coord.processQueue(func(uint64) (string, bool) { return path, true }); changed != 0 {
		t.Fatalf("processQueue changed = %d; want 0", changed)
	}
	h := state.healthSnapshot(0)
	if h.ExtractionErrors != 1 {
		t.Fatalf("ExtractionErrors = %d; want 1", h.ExtractionErrors)
	}
	if got := h.ExtractErrorReasons[contentExtractReasonExtractor]; got != 1 {
		t.Fatalf("extractor reason = %d; want 1 (%+v)", got, h.ExtractErrorReasons)
	}
}

func TestServiceLogPathAvoidsProgramData(t *testing.T) {
	path := serviceLogPath()
	programData := strings.TrimSpace(os.Getenv("ProgramData"))
	if programData != "" {
		rel, err := filepath.Rel(filepath.Clean(programData), filepath.Clean(path))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			t.Fatalf("service log path %q is under ProgramData %q", path, programData)
		}
	}
	serviceLog("service log path test")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("serviceLog did not write %q: %v", path, err)
	}
}
