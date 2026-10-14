package main

// Tests for usage signals: frecency decay/persistence/bounds, query-history
// combos, and the open/recent service protocol ops.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testSignalStore(t *testing.T, now time.Time) *signalStore {
	t.Helper()
	dir := t.TempDir()
	s := loadSignalStore(filepath.Join(dir, signalStoreFileName))
	s.nowFunc = func() time.Time { return now }
	return s
}

func TestSignalDecayHalfLife(t *testing.T) {
	base := time.Now()
	s := testSignalStore(t, base)
	s.recordOpen(`T:\proj\a.go`, "")
	f := s.files[normalizeSignalPath(`T:\proj\a.go`)]
	if f == nil || f.Score != 1 {
		t.Fatalf("after one open score=1, got %+v", f)
	}
	// One half-life later a second open adds 1 to the decayed 0.5.
	s.nowFunc = func() time.Time { return base.AddDate(0, 0, signalFrecencyHalfLifeDays) }
	s.recordOpen(`T:\proj\a.go`, "")
	if got := s.files[normalizeSignalPath(`T:\proj\a.go`)].Score; got < 1.49 || got > 1.51 {
		t.Fatalf("decayed score after one half-life + reopen = 1.5, got %v", got)
	}
	// Pure decay query (no reopen): the 1.5 stored at +30d halves by +60d.
	if got := s.frecencyScore(`T:\proj\a.go`, base.AddDate(0, 0, 2*signalFrecencyHalfLifeDays)); got < 0.749 || got > 0.751 {
		t.Fatalf("score after one more half-life = 0.75, got %v", got)
	}
}

func TestSignalPersistenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, signalStoreFileName)
	s := loadSignalStore(path)
	s.recordOpen(`T:\proj\a.go`, "a.go")
	s.recordOpen(`T:\proj\a.go`, "a.go")
	s.recordOpen(`T:\proj\b.go`, "other")
	// Reload like a service restart.
	r := loadSignalStore(path)
	if got := r.frecencyScore(`T:\proj\a.go`, time.Now()); got < 1.9 {
		t.Fatalf("reloaded score for twice-opened file, got %v", got)
	}
	if got := r.comboCount("a.go", `T:\proj\a.go`); got != 2 {
		t.Fatalf("reloaded combo count = 2, got %d", got)
	}
	if got := r.comboCount("  A.GO  ", `T:\proj\a.go`); got != 2 {
		t.Fatalf("combo key is normalized (case/space), got %d", got)
	}
	if len(r.recent(10)) != 2 {
		t.Fatalf("recent lists both files, got %d", len(r.recent(10)))
	}
}

func TestSignalStoreBounds(t *testing.T) {
	s := testSignalStore(t, time.Now())
	for i := 0; i < signalStoreMaxFiles+500; i++ {
		p := fmt.Sprintf(`T:\proj\f%05d.go`, i)
		s.mu.Lock()
		s.files[normalizeSignalPath(p)] = &fileSignal{Opens: 1, Score: float64(i), LastOpenUnix: time.Now().UnixNano()}
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.compact()
	n := len(s.files)
	s.mu.Unlock()
	if n != signalStoreMaxFiles {
		t.Fatalf("files compacted to %d, got %d", signalStoreMaxFiles, n)
	}
	// The survivors are the highest scores.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.files {
		if f.Score < 500 {
			t.Fatalf("low-score entry survived compaction: %v", f.Score)
		}
	}
}

func TestSignalQueryBounds(t *testing.T) {
	s := testSignalStore(t, time.Now())
	for i := 0; i < signalStoreMaxQueries+10; i++ {
		q := fmt.Sprintf("query number %05d", i)
		s.mu.Lock()
		s.queries[normalizeSignalQuery(q)] = map[string]*queryPick{`T:\x.go`: {Count: 1}}
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.compact()
	n := len(s.queries)
	s.mu.Unlock()
	if n != signalStoreMaxQueries {
		t.Fatalf("queries compacted to %d, got %d", signalStoreMaxQueries, n)
	}
}

func TestSignalEmptyFastPath(t *testing.T) {
	var nilStore *signalStore
	if !nilStore.empty() {
		t.Fatal("nil store is empty")
	}
	s := testSignalStore(t, time.Now())
	if !s.empty() {
		t.Fatal("fresh store is empty")
	}
	s.recordOpen(`T:\proj\a.go`, "")
	if s.empty() {
		t.Fatal("store with an open is not empty")
	}
}

func callTestServiceCommand(t *testing.T, svc *goSearchService, req serviceRequest) serviceResponse {
	t.Helper()
	var buf bytes.Buffer
	caps := localServiceCapabilities(servicePrincipal{})
	svc.handleServiceCommand(&buf, servicePrincipal{}, caps, &req)
	var resp serviceResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("decode service response: %v\n%s", err, buf.String())
	}
	return resp
}

func TestSignalOpenRecentProtocol(t *testing.T) {
	t.Setenv("SEEKFS_DIR", t.TempDir())
	resetSharedSignalStoreForTest()
	defer resetSharedSignalStoreForTest()
	svc := &goSearchService{}
	openPath := filepath.Join(t.TempDir(), "opened.go")
	if err := writeTestFile(openPath, "package x"); err != nil {
		t.Skipf("cannot create test file: %v", err)
	}
	resp := callTestServiceCommand(t, svc, serviceRequest{Command: "open", Path: openPath, OpenQuery: "opened"})
	if !resp.OK {
		t.Fatalf("open failed: %s", resp.Message)
	}
	resp = callTestServiceCommand(t, svc, serviceRequest{Command: "open", Path: openPath})
	if !resp.OK {
		t.Fatalf("second open failed: %s", resp.Message)
	}
	resp = callTestServiceCommand(t, svc, serviceRequest{Command: "recent", Limit: 10})
	if !resp.OK || len(resp.Recent) != 1 {
		t.Fatalf("recent = %+v", resp)
	}
	row := resp.Recent[0]
	if row.Path != normalizeSignalPath(openPath) || row.Opens != 2 {
		t.Fatalf("recent row = %+v", row)
	}
	// Unknown command stays denied (deny-by-default allowlist).
	resp = callTestServiceCommand(t, svc, serviceRequest{Command: "open-evil"})
	if resp.OK {
		t.Fatal("unknown command must be denied")
	}
	// Open requires a path.
	resp = callTestServiceCommand(t, svc, serviceRequest{Command: "open"})
	if resp.OK {
		t.Fatal("open without a path must fail")
	}
	// Remote callers cannot use local-only ops.
	var buf bytes.Buffer
	svc.handleServiceCommand(&buf, servicePrincipal{}, serviceCapabilities{ReadOnly: true, Remote: true}, &serviceRequest{Command: "open", Path: openPath})
	var rresp serviceResponse
	if err := json.Unmarshal(buf.Bytes(), &rresp); err != nil || rresp.OK {
		t.Fatalf("remote open must be denied: %v %s", err, buf.String())
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
