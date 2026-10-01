package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"seekfs/feature"
)

// This is a real child process, not a transport mock. stdout is protocol-only.
func TestFeatureCompanionProcess(t *testing.T) {
	if os.Getenv("SEEKFS_TEST_FEATURE_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	volumes := make(map[string]map[uint64]feature.Record)
	staged := make(map[string]map[uint64]feature.Record)
	committed := make(map[string]feature.Volume)
	scanner := feature.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req feature.Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			os.Exit(2)
		}
		resp := feature.Response{Version: feature.Version, ID: req.ID, OK: true, Complete: true}
		if mode == "wrong-version" {
			resp.Version++
		}
		if mode == "wrong-id" {
			resp.ID++
		}
		if mode == "bad-progress" {
			resp.Progress = &feature.Progress{Current: -1}
		}
		if mode == "malformed" {
			fmt.Println("not json")
			continue
		}
		if mode == "oversized" {
			fmt.Println(strings.Repeat("x", feature.MaxFrameBytes+1))
			continue
		}
		if mode == "stall" {
			time.Sleep(time.Hour)
		}
		id := ""
		if req.Volume != nil {
			id = req.Volume.ID
			if req.Volume.IndexID != "" {
				id = req.Volume.IndexID
			}
		}
		switch req.Op {
		case "volume_remove":
			delete(volumes, id)
			delete(staged, id)
			delete(committed, id)
		case "health":
			resp.Complete = mode != "incomplete"
			resp.Progress = &feature.Progress{Current: int64(len(volumes[id])), Total: int64(len(volumes[id])), Unit: "files"}
		case "hello":
			if mode != "background-only" {
				resp.Capabilities = []string{"query"}
			}
			if req.Version != feature.Version || req.Feature == "" || !filepath.IsAbs(req.DataDir) {
				os.Exit(3)
			}
		case "snapshot_begin":
			staged[id] = make(map[uint64]feature.Record)
		case "changes_begin":
			if req.Previous == nil || committed[id] != *req.Previous {
				resp.OK = false
				resp.Error = "changes cursor mismatch"
				break
			}
			staged[id] = make(map[uint64]feature.Record)
			for frn, rec := range volumes[id] {
				staged[id][frn] = rec
			}
		case "records":
			for _, rec := range req.Records {
				if rec.Deleted {
					delete(staged[id], rec.FRN)
				} else {
					staged[id][rec.FRN] = rec
				}
			}
		case "snapshot_commit", "changes_commit":
			volumes[id] = staged[id]
			committed[id] = *req.Volume
			delete(staged, id)
		case "query":
			if mode == "large-candidates" {
				resp.FRNs = make([]uint64, 70000)
				for i := range resp.FRNs {
					resp.FRNs[i] = uint64(i + 1)
				}
				break
			}
			if mode == "zero-frn" {
				resp.FRNs = []uint64{0}
				break
			}
			if mode == "too-many" {
				resp.FRNs = make([]uint64, feature.MaxQueryMatches+1)
				for i := range resp.FRNs {
					resp.FRNs[i] = 1
				}
				break
			}
			if req.Query == "slow-blue" {
				time.Sleep(50 * time.Millisecond)
				req.Query = "blue"
			}
			if req.Query == "invalid-query" {
				resp.OK = false
				resp.Error = "invalid query"
				break
			}
			if mode == "crash" {
				os.Exit(4)
			}
			if mode == "incomplete" {
				resp.Complete = false
			}
			for frn, rec := range volumes[id] {
				haystack, needle := rec.Name, req.Query
				if !req.CaseSensitive {
					haystack, needle = strings.ToLower(haystack), strings.ToLower(needle)
				}
				if needle == "all" || strings.Contains(haystack, needle) {
					resp.FRNs = append(resp.FRNs, frn)
				}
			}
		default:
			resp.OK = false
			resp.Error = "unknown operation"
		}
		if err := feature.Write(os.Stdout, resp); err != nil {
			os.Exit(5)
		}
	}
	os.Exit(0)
}

func featureTestService(t *testing.T, mode string) (*goSearchService, *serviceVolumeIndex, *companionFeature) {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	t.Setenv("SEEKFS_TEST_FEATURE_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 100, Records: []CompactRecord{
		{FRN: 10, ParentFRN: 1, Parent: -1, Name: "Blue.go", Size: 20},
		{FRN: 11, ParentFRN: 1, Parent: -1, Name: "blue.txt", Size: 10},
		{FRN: 12, ParentFRN: 1, Parent: -1, Name: "red.go", Size: 30},
	}}
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_c.gsi"), idx)
	s := &goSearchService{stop: make(chan struct{}), contentCfg: appConfig{SeekFSDir: t.TempDir(), Features: map[string]featureConfig{"tags": {Enabled: true, Command: exe, Args: []string{"-test.run=^TestFeatureCompanionProcess$", "--", mode}, Timeout: time.Second}}}}
	s.prepareFeatureVolume(vol)
	s.indexMu.Lock()
	s.indexes, s.volumes = []*Index{idx}, []*serviceVolumeIndex{vol}
	s.indexMu.Unlock()
	f := s.features.Load().companions["tags"]
	t.Cleanup(func() {
		s.signalServiceStop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		select {
		case <-f.stopped:
		case <-ctx.Done():
			t.Error("companion worker did not stop")
		}
	})
	return s, vol, f
}

func TestFeatureCompanionQueries(t *testing.T) {
	s, _, _ := featureTestService(t, "normal")
	for _, tc := range []struct {
		query string
		count int
	}{
		{"feature:tags:blue", 2}, {"feature:tags:blue ext:go", 1}, {"feature:tags:blue !ext:go", 1},
		{"feature:tags:blue|ext:go", 3}, {"!feature:tags:blue ext:go", 1}, {"feature:tags:all !feature:tags:red", 2},
		{"feature:tags:all feature:tags:blue", 2}, {"case:true feature:tags:Blue", 1}, {"feature:tags:all ext:go|feature:tags:red", 2},
	} {
		matches, _, err := s.searchFeatureQuery(queryOptions{Query: tc.query, Limit: 100}, false)
		if err != nil || len(matches) != tc.count {
			t.Errorf("search %q = %v, %v; want %d", tc.query, matches, err, tc.count)
		}
		_, count, err := s.searchFeatureQuery(queryOptions{Query: tc.query}, true)
		if err != nil || count != tc.count {
			t.Errorf("count %q = %d, %v; want %d", tc.query, count, err, tc.count)
		}
	}
	matches, _, err := s.searchFeatureQuery(queryOptions{Query: "feature:tags:all sort:size", Limit: 1}, false)
	if err != nil || len(matches) != 1 || matches[0].Name != "blue.txt" {
		t.Fatalf("sort/limit = %v, %v", matches, err)
	}
}

func TestFeatureFailureIsolation(t *testing.T) {
	for _, mode := range []string{"wrong-version", "wrong-id", "bad-progress", "malformed", "oversized", "stall", "crash", "incomplete", "zero-frn", "too-many"} {
		t.Run(mode, func(t *testing.T) {
			s, _, f := featureTestService(t, mode)
			timeout := 2 * time.Second
			if mode == "stall" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_, _, err := f.query(ctx, s.volumes[0], "all", false)
			if err == nil {
				t.Fatal("broken companion query succeeded")
			}
			matches, err := searchServiceVolumes(snapshotServiceVolumesForSearch(s.volumes), queryOptions{Query: "ext:go", Limit: 100}, false)
			if err != nil || len(matches) != 2 {
				t.Fatalf("feature failure broke filename search: %v, %v", matches, err)
			}
		})
	}
}

func TestFeatureMultiVolumeIsolation(t *testing.T) {
	s, _, _ := featureTestService(t, "normal")
	idx := &Index{Source: "usn", Volume: "D:", Compact: true, JournalID: 8, Checkpoint: 100, Records: []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "blue.go"}, {FRN: 11, ParentFRN: 1, Parent: -1, Name: "green.txt"}}}
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_d.gsi"), idx)
	s.prepareFeatureVolume(vol)
	s.indexMu.Lock()
	s.volumes = append(s.volumes, vol)
	s.indexes = append(s.indexes, idx)
	s.indexMu.Unlock()
	matches, _, err := s.searchFeatureQuery(queryOptions{Query: "feature:tags:blue", Limit: 100}, false)
	if err != nil || len(matches) != 3 {
		t.Fatalf("federated feature search = %v, %v", matches, err)
	}
	_, count, err := s.searchFeatureQuery(queryOptions{Query: "feature:tags:blue", Under: `D:\`}, true)
	if err != nil || count != 1 {
		t.Fatalf("volume-scoped feature count = %d, %v", count, err)
	}
	// A second database on C: must not overwrite the first C: companion table.
	other := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 200, Records: []CompactRecord{{FRN: 13, ParentFRN: 1, Parent: -1, Name: "blue.log"}}}
	contentIndexFRNs(other)
	otherVol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "other_c.gsi"), other)
	s.prepareFeatureVolume(otherVol)
	s.indexMu.Lock()
	s.volumes = append(s.volumes, otherVol)
	s.indexes = append(s.indexes, other)
	s.indexMu.Unlock()
	for i := 0; i < 2; i++ {
		matches, _, err := s.searchFeatureQuery(queryOptions{Query: "feature:tags:blue", Limit: 100}, false)
		if err != nil || len(matches) != 4 {
			t.Fatalf("same-volume index isolation = %v, %v", matches, err)
		}
	}
}

func TestFeatureAggregateCandidateBudgetRefusesCount(t *testing.T) {
	s, _, _ := featureTestService(t, "large-candidates")
	if _, _, err := s.searchFeatureQuery(queryOptions{Query: "feature:tags:one feature:tags:two"}, true); err == nil || !strings.Contains(err.Error(), "candidate budget") {
		t.Fatalf("oversized joint count = %v", err)
	}
}

func TestFeatureSnapshotInvalidation(t *testing.T) {
	s, vol, f := featureTestService(t, "normal")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := f.query(ctx, vol, "all", false); err != nil {
		t.Fatal(err)
	}
	before, err := s.captureFeatureVolume(vol)
	if err != nil {
		t.Fatal(err)
	}
	vol.featureFeed.observe(make([]usnChange, featureChangeWindow+1))
	after, err := s.captureFeatureVolume(vol)
	if err != nil {
		t.Fatal(err)
	}
	if _, incremental, err := s.featureChanges(vol, after, before.meta); err != nil || incremental {
		t.Fatalf("gap should require snapshot: %v, %v", incremental, err)
	}
	if _, _, err := f.query(ctx, vol, "all", false); err != nil {
		t.Fatalf("gap recovery: %v", err)
	}
	vol.featureFeed.observe([]usnChange{{FRN: 100, Attr: 0x10, Reason: usnReasonRenameNew}})
	if _, _, err := s.featureSnapshotPage(vol, before, 0); err == nil {
		t.Fatal("committed a stale snapshot after directory rename")
	}
}

func TestFeatureIncrementalAndRestartRecovery(t *testing.T) {
	s, vol, f := featureTestService(t, "normal")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, before, err := f.query(ctx, vol, "all", false)
	if err != nil {
		t.Fatal(err)
	}
	vol.mu.Lock()
	vol.applyUSNChanges([]usnChange{
		{FRN: 10, ParentFRN: 1, Name: "Blue.go", Reason: usnReasonFileDelete, USN: 110},
		{FRN: 13, ParentFRN: 1, Name: "green.go", Reason: usnReasonFileCreate, USN: 111},
	})
	vol.mu.Unlock()
	view, err := s.captureFeatureVolume(vol)
	if err != nil {
		t.Fatal(err)
	}
	records, incremental, err := s.featureChanges(vol, view, before.meta)
	if err != nil || !incremental || len(records) != 2 {
		t.Fatalf("changes = %v, %v, %v", records, incremental, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		set, _, err := f.query(ctx, vol, "all", false)
		if err != nil || len(set) != 3 {
			t.Fatalf("sync after change/restart: %v, %v", set, err)
		}
		if _, ok := set[10]; ok {
			t.Fatal("deleted FRN survived sync")
		}
		if _, ok := set[13]; !ok {
			t.Fatal("new FRN missing")
		}
		if attempt == 0 {
			if err := f.lock(ctx); err != nil {
				t.Fatal(err)
			}
			f.stopProcess()
			<-f.gate
		}
	}
}

func TestFeatureApplicationErrorsDoNotRestartCompanion(t *testing.T) {
	_, vol, f := featureTestService(t, "normal")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := f.query(ctx, vol, "all", false); err != nil {
		t.Fatal(err)
	}
	if err := f.lock(ctx); err != nil {
		t.Fatal(err)
	}
	pid := f.cmd.Process.Pid
	<-f.gate
	_, _, err := f.query(ctx, vol, "invalid-query", false)
	var operationError *featureOperationError
	if !errors.As(err, &operationError) {
		t.Fatalf("query error = %v", err)
	}
	if _, _, err := f.query(ctx, vol, "blue", false); err != nil {
		t.Fatal(err)
	}
	if err := f.lock(ctx); err != nil {
		t.Fatal(err)
	}
	currentPID := f.cmd.Process.Pid
	<-f.gate
	if currentPID != pid {
		t.Fatal("application-level error restarted the companion")
	}
}

func TestBackgroundOnlyFeatureDoesNotAdvertiseQueries(t *testing.T) {
	_, vol, f := featureTestService(t, "background-only")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := f.query(ctx, vol, "all", false); err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("background-only query: %v", err)
	}
}

func TestFeatureQuickCancellationKeepsWarmCompanion(t *testing.T) {
	_, vol, f := featureTestService(t, "normal")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := f.query(ctx, vol, "all", false); err != nil {
		t.Fatal(err)
	}
	if err := f.lock(ctx); err != nil {
		t.Fatal(err)
	}
	pid := f.cmd.Process.Pid
	<-f.gate
	short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if _, _, err := f.query(short, vol, "slow-blue", false); err == nil {
		t.Fatal("canceled query succeeded")
	}
	if set, _, err := f.query(ctx, vol, "blue", false); err != nil || len(set) != 2 {
		t.Fatalf("query after cancellation = %v, %v", set, err)
	}
	if err := f.lock(ctx); err != nil {
		t.Fatal(err)
	}
	currentPID := f.cmd.Process.Pid
	<-f.gate
	if currentPID != pid {
		t.Fatal("quick cancellation discarded the warmed companion")
	}
}

func TestFeatureLongPathsSplitIntoBoundedFrames(t *testing.T) {
	_, _, f := featureTestService(t, "normal")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.lock(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { <-f.gate }()
	if err := f.start(ctx); err != nil {
		t.Fatal(err)
	}
	meta := feature.Volume{ID: "long-path-test", JournalID: 7, Checkpoint: 100}
	if _, err := f.call(ctx, feature.Request{Op: "snapshot_begin", Volume: &meta}); err != nil {
		t.Fatal(err)
	}
	var records []feature.Record
	for i := 0; i < 25; i++ {
		records = append(records, feature.Record{FRN: uint64(1000 + i), Name: "long.go", Path: `C:\` + strings.Repeat(`dir\`, 20000) + "long.go"})
	}
	if err := f.sendRecords(ctx, &meta, records); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(ctx, feature.Request{Op: "snapshot_commit", Volume: &meta}); err != nil {
		t.Fatal(err)
	}
	resp, err := f.call(ctx, feature.Request{Op: "query", Volume: &meta, Query: "long"})
	if err != nil || len(resp.FRNs) != len(records) {
		t.Fatalf("long path transfer = %d records, %v", len(resp.FRNs), err)
	}
}

func TestFeatureSnapshotPagesAndDirectoryRename(t *testing.T) {
	s, vol, f := featureTestService(t, "normal")
	s.indexMu.Lock()
	vol.mu.Lock()
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 200, Records: []CompactRecord{
		{FRN: 5, ParentFRN: 5, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 100, ParentFRN: 5, Parent: 0, Name: "old", Mode: uint32(os.ModeDir)},
	}}
	for i := 0; i < feature.PageSize+2; i++ {
		idx.Records = append(idx.Records, CompactRecord{FRN: uint64(1000 + i), ParentFRN: 100, Parent: 1, Name: fmt.Sprintf("doc%d.go", i)})
	}
	contentIndexFRNs(idx)
	src := newServiceVolumeIndex(vol.dbPath, idx)
	replaceServiceVolumeContents(vol, src)
	s.indexes[0] = idx
	vol.applyUSNChanges([]usnChange{{FRN: 100, ParentFRN: 5, Name: "new", Attr: 0x10, Reason: usnReasonRenameNew, USN: 210}})
	vol.mu.Unlock()
	s.indexMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	set, _, err := f.query(ctx, vol, "doc", false)
	if err != nil || len(set) != feature.PageSize+2 {
		t.Fatalf("paged snapshot = %d, %v", len(set), err)
	}
	matches, _, err := s.searchFeatureQuery(queryOptions{Query: "feature:tags:doc", Limit: 1}, false)
	if err != nil || len(matches) != 1 || !strings.Contains(matches[0].Path, `\new\`) {
		t.Fatalf("renamed parent path = %v, %v", matches, err)
	}
}

func TestFeatureCommandDispatchAndMixedContent(t *testing.T) {
	s, vol, _ := featureTestService(t, "normal")
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	idx, err := assembleContentIndex([]contentBuildDoc{
		{frn: 10, path: `C:\Blue.go`, text: []byte("needle feature:tags:blue"), class: contentClassText, version: 1},
		{frn: 11, path: `C:\blue.txt`, text: []byte("other"), class: contentClassText, version: 1},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	s.indexMu.Lock()
	vol.content = newContentVolumeState(vol.volume)
	vol.content.setReady(idx, reader, buildContentResolver(idx.Docs, vol.index.Derived.FRNs, vol.index.Derived.FRNRecordIDs))
	s.indexMu.Unlock()
	for _, tc := range []struct {
		query string
		count int
	}{
		{"feature:tags:blue content:needle", 1}, {"feature:tags:all !content:needle", 2}, {"feature:tags:blue|content:needle", 2}, {`content:"feature:tags:blue"`, 1},
	} {
		var out bytes.Buffer
		s.serviceCommandSearch(&out, serviceCapabilities{}, &serviceRequest{Query: tc.query, Limit: 100})
		var resp serviceResponse
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.OK || resp.Count != tc.count || resp.Complete == nil || !*resp.Complete {
			t.Errorf("dispatch %q: %+v", tc.query, resp)
		}
	}
	var out bytes.Buffer
	s.serviceCommandSearch(&out, serviceCapabilities{Remote: true}, &serviceRequest{Query: "feature:tags:blue"})
	var resp serviceResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || !strings.Contains(resp.Message, "local-only") {
		t.Fatalf("remote feature query accepted: %+v", resp)
	}
}

func TestFeatureSnapshotRemainsConsistentAcrossReplay(t *testing.T) {
	s, vol, _ := featureTestService(t, "normal")
	v, err := s.captureFeatureVolume(vol)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.prepareFeatureView(vol, v); err != nil {
		t.Fatal(err)
	}
	vol.mu.Lock()
	vol.applyUSNChanges([]usnChange{{FRN: 13, ParentFRN: 1, Name: "later.go", Reason: usnReasonFileCreate, USN: 120}})
	vol.mu.Unlock()
	records, _, err := s.featureSnapshotPage(vol, v, 0)
	if err != nil || len(records) != 3 {
		t.Fatalf("original snapshot changed with replay: %v, %v", records, err)
	}
	for _, rec := range records {
		if rec.FRN == 13 {
			t.Fatal("new file leaked into old checkpoint snapshot")
		}
	}
	s.indexMu.RLock()
	vol.mu.Lock()
	current := s.featureVolumeCurrent(vol, v)
	vol.mu.Unlock()
	s.indexMu.RUnlock()
	if current {
		t.Fatal("old query answers considered current after replay")
	}
	latest, err := s.captureFeatureVolume(vol)
	if err != nil {
		t.Fatal(err)
	}
	changes, incremental, err := s.featureChanges(vol, latest, v.meta)
	if err != nil || !incremental || len(changes) != 1 || changes[0].FRN != 13 {
		t.Fatalf("post-snapshot replay: %v, %v, %v", changes, incremental, err)
	}
}

// The child host deliberately exits without normal shutdown. Closing its Job
// handle must terminate the companion, rather than leave it running orphaned.
func TestFeatureManagedHostProcess(t *testing.T) {
	if os.Getenv("SEEKFS_TEST_MANAGED_HOST") != "1" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	job, err := newFeatureProcessJob()
	if err != nil {
		os.Exit(3)
	}
	cmd := exec.Command(exe, "-test.run=^TestFeatureCompanionProcess$", "--", "stall")
	cmd.Env = append(os.Environ(), "SEEKFS_TEST_FEATURE_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		os.Exit(4)
	}
	if err := cmd.Start(); err != nil {
		os.Exit(5)
	}
	if err := job.attach(cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		os.Exit(6)
	}
	if err := feature.Write(stdin, feature.Request{Version: feature.Version, ID: 1, Op: "hello"}); err != nil {
		os.Exit(7)
	}
	fmt.Println(cmd.Process.Pid)
	os.Exit(0)
}

func TestFeatureJobCleansUpAfterHostExit(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestFeatureManagedHostProcess$")
	cmd.Env = append(os.Environ(), "SEEKFS_TEST_MANAGED_HOST=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("host output: %q", out)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return
	} // Already reaped.
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	state, err := windows.WaitForSingleObject(h, 5000)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(h, 1)
		t.Fatalf("companion survived host exit: state=%d err=%v", state, err)
	}
}
