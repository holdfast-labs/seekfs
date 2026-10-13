package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

// Bounded shutdown (BUG 1): every wait on the stop path must complete within
// a deadline even mid-persist or mid-build. These tests start the relevant
// loops — a stuck content task, a live drain loop, a blocked build, an
// in-flight persist, an integrity pass, restart catch-up — and assert the
// stop completes within a deadline instead of hanging STOP_PENDING forever.

// stubShutdownFast shrinks every shutdown budget so the tests prove
// boundedness without waiting out production deadlines.
func stubShutdownFast(t *testing.T) {
	t.Helper()
	restoreTimeout := serviceShutdownTimeout
	restoreContent := serviceShutdownContentWait
	restoreCompanion := serviceShutdownCompanionWait
	restoreLock := serviceShutdownLockWait
	restoreListener := serviceShutdownListenerWait
	restoreRemote := serviceShutdownRemoteWait
	serviceShutdownTimeout = 5 * time.Second
	serviceShutdownContentWait = 150 * time.Millisecond
	serviceShutdownCompanionWait = 150 * time.Millisecond
	serviceShutdownLockWait = 150 * time.Millisecond
	serviceShutdownListenerWait = 500 * time.Millisecond
	serviceShutdownRemoteWait = 150 * time.Millisecond
	t.Cleanup(func() {
		serviceShutdownTimeout = restoreTimeout
		serviceShutdownContentWait = restoreContent
		serviceShutdownCompanionWait = restoreCompanion
		serviceShutdownLockWait = restoreLock
		serviceShutdownListenerWait = restoreListener
		serviceShutdownRemoteWait = restoreRemote
	})
}

// shutdownTestVolumeWithRecords builds a minimal USN volume with content state
// on dir. Callers must set SEEKFS_CONTENT_SEARCH=1 first so
// newServiceVolumeIndex creates the content state; the scope is pinned to dir
// like contentBuildTestVolume does.
func shutdownTestVolumeWithRecords(t *testing.T, dir string, journal uint64, checkpoint int64, records []CompactRecord) (*goSearchService, *serviceVolumeIndex) {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SCOPE", "explicit")
	t.Setenv("SEEKFS_CONTENT_ROOTS", dir)
	s := contentTestService(t)
	idx := &Index{Source: "usn", Volume: dir, Compact: true, JournalID: journal, Checkpoint: checkpoint}
	idx.Records = append([]CompactRecord(nil), records...)
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_c.gsi"), idx)
	releaseVolumeContentOnCleanup(t, vol)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{idx}
	return s, vol
}

func shutdownTestVolume(t *testing.T) (*goSearchService, *serviceVolumeIndex) {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	return shutdownTestVolumeWithRecords(t, t.TempDir(), 5, 50,
		[]CompactRecord{{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: 8}})
}

func contentTasksNow(vol *serviceVolumeIndex) int {
	vol.contentTasksMu.Lock()
	defer vol.contentTasksMu.Unlock()
	return vol.contentTasks
}

// A content task that never finishes (a wedged fold/extraction) must not hold
// the stop past the content budget: shutdown proceeds and releases the base.
func TestShutdownCompletesWithStuckContentTask(t *testing.T) {
	stubShutdownFast(t)
	s, vol := shutdownTestVolume(t)
	if vol.content == nil || vol.contentCoord == nil {
		t.Fatal("precondition: volume must carry content state")
	}
	if !s.beginContentTask(vol) {
		t.Fatal("precondition: content task must begin")
	}
	// Never endContentTask: the wedged worker.
	coord := vol.contentCoord

	start := time.Now()
	s.shutdownService(nil)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("shutdown with a stuck content task took %s; must be bounded", elapsed)
	}
	if !s.serviceStopping() {
		t.Fatal("shutdown must signal the service stop")
	}
	if a := vol.contentActivation.Load(); a != nil && a.ctx.Err() == nil {
		t.Fatal("shutdown must cancel the content activation")
	}
	if coord.drainEnabledNow() {
		t.Fatal("shutdown must retire the drain")
	}
	if vol.content != nil || vol.contentCoord != nil {
		t.Fatal("shutdown must release the content base even when a task is stuck")
	}
	// Idempotent: a second stop returns immediately.
	start = time.Now()
	s.shutdownService(nil)
	if time.Since(start) > 5*time.Second {
		t.Fatal("repeated shutdown must return immediately")
	}
}

// A live drain loop ticking alongside shutdown must not deadlock it (the tick
// takes indexMu.RLock while the release takes indexMu.Lock) and must exit on
// the stop signal.
func TestShutdownWithLiveDrainLoop(t *testing.T) {
	stubShutdownFast(t)
	s, vol := shutdownTestVolume(t)
	vol.contentCoord.enableDrain()
	if !s.beginContentTask(vol) {
		t.Fatal("precondition: content task must begin")
	}
	drainDone := make(chan struct{})
	go func() { defer endContentTask(vol); s.contentDrainLoop(vol); close(drainDone) }()

	start := time.Now()
	s.shutdownService(nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("shutdown with a live drain loop took %s; must be bounded", elapsed)
	}
	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("drain loop did not exit after shutdown")
	}
}

// A service-owned build blocked mid-snapshot must abort on shutdown: no
// sidecar is published and the stop completes within the deadline.
func TestShutdownAbortsBlockedBuild(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubShutdownFast(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("blocked build needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(13)
	const cp = int64(20)
	s, vol := shutdownTestVolumeWithRecords(t, dir, journal, cp,
		[]CompactRecord{{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 20}})

	entered := make(chan struct{})
	var enterOnce sync.Once
	restoreHook := contentBuildSnapshotBatchHook
	contentBuildSnapshotBatchHook = func(*goSearchService, *serviceVolumeIndex) {
		enterOnce.Do(func() { close(entered) })
		<-s.stop // wedged snapshot batch: only the stop releases it
	}
	t.Cleanup(func() { contentBuildSnapshotBatchHook = restoreHook })

	s.ensureContentBuild(vol)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("build did not reach the wedged snapshot batch")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !vol.contentBuildBusy.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !vol.contentBuildBusy.Load() {
		t.Fatal("precondition: build must hold the build slot")
	}

	start := time.Now()
	s.shutdownService(nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("shutdown with a blocked build took %s; must be bounded", elapsed)
	}
	deadline = time.Now().Add(5 * time.Second)
	for (vol.contentBuildBusy.Load() || contentTasksNow(vol) > 0) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if vol.contentBuildBusy.Load() {
		t.Fatal("blocked build did not release the build slot after shutdown")
	}
	if n := contentTasksNow(vol); n != 0 {
		t.Fatalf("content tasks = %d after shutdown; want 0", n)
	}
	if _, err := os.Stat(contentIndexPathForDB(vol.dbPath)); !os.IsNotExist(err) {
		t.Fatalf("aborted build must not publish a sidecar (err=%v)", err)
	}
	// Shutdown releases the base; the aborted build must never have made it
	// ready (a published sidecar would have shown up above).
	if vol.content != nil {
		if got := vol.content.stateOf(); got == contentStateReady {
			t.Fatalf("aborted build left state = %q; want not-ready", got)
		}
	}
}

// Shutdown must not run the final blocking persist (the WAL keeps durability);
// the persist loop exits on the stop signal with the volume still dirty.
func TestShutdownSkipsFinalPersist(t *testing.T) {
	stubShutdownFast(t)
	s := contentTestService(t)
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 5, Checkpoint: 50}
	idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: 8}}
	contentIndexFRNs(idx)
	dbPath := filepath.Join(t.TempDir(), "seekfs_c.gsi")
	vol := newServiceVolumeIndex(dbPath, idx)
	vol.dirty = true
	vol.lastPersist = time.Now()
	s.volumes = []*serviceVolumeIndex{vol}

	exited := make(chan struct{})
	go func() { s.persistVolumeLoop(vol); close(exited) }()

	start := time.Now()
	s.shutdownService(nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("shutdown took %s; must not wait on persist", elapsed)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("persist loop did not exit on shutdown")
	}
	if !vol.dirty {
		t.Fatal("shutdown must skip the final persist (volume must stay dirty; the WAL keeps durability)")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("shutdown must not write the index file (err=%v)", err)
	}
}

// The post-attach integrity pass hashing large sections must abort on
// shutdown rather than hold the stop, and must not schedule a rebuild for the
// abort.
func TestShutdownAbortsIntegrityPass(t *testing.T) {
	stubShutdownFast(t)
	_, loaded := contentIntegrityTestFile(t)
	s, vol := shutdownTestVolume(t)

	restoreHook := contentVerifySectionHook
	entered := make(chan struct{})
	var enterOnce sync.Once
	contentVerifySectionHook = func(uint32) {
		enterOnce.Do(func() { close(entered) })
		<-s.stop // wedged hash: only the stop releases it
	}
	t.Cleanup(func() { contentVerifySectionHook = restoreHook })

	if !s.beginContentTask(vol) {
		t.Fatal("precondition: content task must begin")
	}
	passDone := make(chan struct{})
	go func() { defer endContentTask(vol); s.verifyContentAfterAttach(vol, loaded); close(passDone) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("integrity pass did not start")
	}

	start := time.Now()
	s.shutdownService(nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("shutdown with an in-flight integrity pass took %s; must be bounded", elapsed)
	}
	select {
	case <-passDone:
	case <-time.After(5 * time.Second):
		t.Fatal("integrity pass did not abort after shutdown")
	}
	if vol.contentBuildBusy.Load() {
		t.Fatal("an aborted integrity pass must not schedule a rebuild")
	}
}

// Restart catch-up blocked in a journal read must abort on shutdown.
func TestShutdownAbortsCatchUp(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubShutdownFast(t)
	const journal = uint64(0xABC)
	const cp = int64(120)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	s, vol := shutdownTestVolumeWithRecords(t, t.TempDir(), journal, cp,
		[]CompactRecord{{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: 8}})

	base := newContentIndex()
	base.JournalID = journal
	base.CheckpointUSN = 50 // behind vol.checkpoint (120): forces the read loop

	entered := make(chan struct{})
	var enterOnce sync.Once
	stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
		enterOnce.Do(func() { close(entered) })
		<-s.stop // wedged journal read: only the stop releases it
		return startUSN, nil, errors.New("stopped")
	})

	if !s.beginContentTask(vol) {
		t.Fatal("precondition: content task must begin")
	}
	catchDone := make(chan struct{})
	go func() { defer endContentTask(vol); s.contentCatchUpAfterAttach(vol, base); close(catchDone) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("catch-up did not reach the journal read")
	}

	start := time.Now()
	s.shutdownService(nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("shutdown with in-flight catch-up took %s; must be bounded", elapsed)
	}
	select {
	case <-catchDone:
	case <-time.After(5 * time.Second):
		t.Fatal("catch-up did not abort after shutdown")
	}
}

// waitContentTasksBounded: drained tasks return true immediately, a stuck task
// returns false at the budget instead of hanging.
func TestWaitContentTasksBounded(t *testing.T) {
	stubShutdownFast(t)
	s, vol := shutdownTestVolume(t)
	if !s.waitContentTasksBounded([]*serviceVolumeIndex{vol}, time.Second) {
		t.Fatal("no tasks must drain immediately")
	}
	if !s.beginContentTask(vol) {
		t.Fatal("precondition: content task must begin")
	}
	start := time.Now()
	if s.waitContentTasksBounded([]*serviceVolumeIndex{vol}, 100*time.Millisecond) {
		endContentTask(vol)
		t.Fatal("a stuck task must not report drained")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("bounded wait took %s; must stop at the budget", elapsed)
	}
	endContentTask(vol)
	if !s.waitContentTasksBounded([]*serviceVolumeIndex{vol}, time.Second) {
		t.Fatal("ended tasks must drain")
	}
}

// The SCM stop path must report STOP_PENDING with a wait hint and increasing
// checkpoints while the bounded shutdown runs, then return stopped.
func TestExecuteReportsStopPendingWithWaitHint(t *testing.T) {
	stubShutdownFast(t)
	pipeName := `\\.\pipe\seekfs-test-stop-` + fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	s := &goSearchService{
		pipeName:    pipeName,
		processMode: "windows-service",
		stop:        make(chan struct{}),
	}
	r := make(chan svc.ChangeRequest)
	changes := make(chan svc.Status, 64)
	type executeResult struct {
		more bool
		code uint32
	}
	ret := make(chan executeResult, 1)
	go func() {
		more, code := s.Execute(nil, r, changes)
		ret <- executeResult{more, code}
	}()

	mustStatus := func(want svc.State) svc.Status {
		t.Helper()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case st := <-changes:
				if st.State == want {
					return st
				}
			case <-timer.C:
				t.Fatalf("timed out waiting for service state %v", want)
			}
		}
	}
	mustStatus(svc.StartPending)
	running := mustStatus(svc.Running)

	// Interrogate is echoed while running.
	r <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: running}
	if got := mustStatus(svc.Running); got.State != svc.Running {
		t.Fatalf("interrogate echo = %v; want Running", got.State)
	}

	r <- svc.ChangeRequest{Cmd: svc.Stop, CurrentStatus: running}
	// Collect STOP_PENDING updates until Execute returns.
	var checkpoints []uint32
	var waitHint uint32
	timeout := time.After(15 * time.Second)
collect:
	for {
		select {
		case st := <-changes:
			if st.State != svc.StopPending {
				t.Fatalf("during stop, status = %v; want StopPending", st.State)
			}
			checkpoints = append(checkpoints, st.CheckPoint)
			waitHint = st.WaitHint
		case res := <-ret:
			if res.more || res.code != 0 {
				t.Fatalf("Execute = (%v, %d); want (false, 0)", res.more, res.code)
			}
			break collect
		case <-timeout:
			t.Fatal("Execute did not return after Stop")
		}
	}
	if waitHint == 0 {
		t.Fatal("STOP_PENDING must carry a wait hint")
	}
	for i := 1; i < len(checkpoints); i++ {
		if checkpoints[i] <= checkpoints[i-1] {
			t.Fatalf("STOP_PENDING checkpoints must increase: %v", checkpoints)
		}
	}
	if len(checkpoints) < 2 {
		t.Fatalf("want progress checkpoints during stop, got %v", checkpoints)
	}
}
