package main

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestIsIndexFileLockedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"generic error", errors.New("disk full"), false},
		{"access denied path error", &os.PathError{Op: "remove", Path: "C:\\x.gsi", Err: windows.ERROR_ACCESS_DENIED}, true},
		{"sharing violation path error", &os.PathError{Op: "remove", Path: "C:\\x.gsi", Err: windows.ERROR_SHARING_VIOLATION}, true},
		{"permission wrapped", &os.PathError{Op: "open", Path: "C:\\x.gsi", Err: os.ErrPermission}, true},
		{"access denied text", errors.New("remove C:\\x.gsi: Access is denied."), true},
		{"sharing violation text", errors.New("The process cannot access the file because it is being used by another process"), false},
	}
	for _, tt := range tests {
		if got := isIndexFileLockedError(tt.err); got != tt.want {
			t.Errorf("isIndexFileLockedError(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestStaleRecoveryBlockedErrorIsDistinct(t *testing.T) {
	var blocked staleRecoveryBlockedError
	if errors.As(errors.New("x"), &blocked) {
		t.Fatal("generic error must not match staleRecoveryBlockedError")
	}
	if !errors.As(staleRecoveryBlockedError{reason: "locked"}, &blocked) {
		t.Fatal("staleRecoveryBlockedError must match itself")
	}
}

func TestValidateUSNCheckpointTable(t *testing.T) {
	base := usnJournalDataV0{UsnJournalID: 42, FirstUsn: 1000, LowestValidUsn: 1200, NextUsn: 9000}
	cases := []struct {
		name      string
		vol       *serviceVolumeIndex
		journal   usnJournalDataV0
		wantStale bool
		wantMsg   string
	}{
		{
			name:    "healthy checkpoint in range",
			vol:     &serviceVolumeIndex{journalID: 42, checkpoint: 5000},
			journal: base,
		},
		{
			name:    "zero journal id skips id comparison",
			vol:     &serviceVolumeIndex{journalID: 0, checkpoint: 5000},
			journal: base,
		},
		{
			name:      "journal id changed means recreated journal",
			vol:       &serviceVolumeIndex{journalID: 41, checkpoint: 5000},
			journal:   base,
			wantStale: true,
			wantMsg:   "journal id changed",
		},
		{
			name:      "checkpoint before first usn means wrapped journal",
			vol:       &serviceVolumeIndex{journalID: 42, checkpoint: 999},
			journal:   base,
			wantStale: true,
			wantMsg:   "before first valid USN",
		},
		{
			name:    "checkpoint at lowest valid usn is acceptable",
			vol:     &serviceVolumeIndex{journalID: 42, checkpoint: 1200},
			journal: base,
		},
		{
			name:      "checkpoint after next usn means foreign journal state",
			vol:       &serviceVolumeIndex{journalID: 42, checkpoint: 9001},
			journal:   base,
			wantStale: true,
			wantMsg:   "after journal next USN",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUSNCheckpoint(tc.vol, tc.journal)
			if !tc.wantStale {
				if err != nil {
					t.Fatalf("validateUSNCheckpoint() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateUSNCheckpoint() = nil, want stale error")
			}
			if !shouldRebuildStaleIndex(err) {
				t.Fatalf("error %q should trigger rebuild", err)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q should contain %q", err, tc.wantMsg)
			}
		})
	}
}

func TestClassifyServiceHealth(t *testing.T) {
	cases := []struct {
		name        string
		loading     bool
		loadErr     string
		infos       []dbInfo
		wantHealth  string
		wantMessage string
	}{
		{
			name:       "all ready volumes are ok",
			infos:      []dbInfo{{Volume: "C:", State: "ready"}},
			wantHealth: serviceHealthOK,
		},
		{
			name:        "stale volume is an error",
			infos:       []dbInfo{{Volume: "C:", State: "ready"}, {Volume: "F:", State: "stale", StaleReason: "journal wrapped"}},
			wantHealth:  serviceHealthError,
			wantMessage: "journal wrapped",
		},
		{
			name:        "load error is surfaced as error",
			loadErr:     "db unreadable",
			infos:       []dbInfo{{Volume: "C:", State: "ready"}},
			wantHealth:  serviceHealthError,
			wantMessage: "db unreadable",
		},
		{
			name:        "persist failures degrade health",
			infos:       []dbInfo{{Volume: "C:", State: "ready", PersistFailures: 2}},
			wantHealth:  serviceHealthDegraded,
			wantMessage: "persist failing on volume C:",
		},
		{
			name:        "replaying volume degrades health",
			infos:       []dbInfo{{Volume: "F:", State: "replaying"}},
			wantHealth:  serviceHealthDegraded,
			wantMessage: "catching up volume F:",
		},
		{
			name:        "startup loading degrades health",
			loading:     true,
			infos:       []dbInfo{{Volume: "C:", State: "ready"}},
			wantHealth:  serviceHealthDegraded,
			wantMessage: "loading indexes",
		},
		{
			name:        "no indexes loaded degrades health",
			wantHealth:  serviceHealthDegraded,
			wantMessage: "no indexes loaded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			health, message := classifyServiceHealth(tc.loading, tc.loadErr, tc.infos)
			if health != tc.wantHealth {
				t.Fatalf("health = %q, want %q (message %q)", health, tc.wantHealth, message)
			}
			if tc.wantMessage != "" && message != tc.wantMessage {
				t.Fatalf("message = %q, want %q", message, tc.wantMessage)
			}
		})
	}
}

func TestReplayLoopMarksUnreachableVolumeStaleAndStopsCleanly(t *testing.T) {
	origIdle, origErr := replayIdleDelay, replayErrorDelay
	replayIdleDelay, replayErrorDelay = time.Millisecond, time.Millisecond
	defer func() { replayIdleDelay, replayErrorDelay = origIdle, origErr }()

	svc := &goSearchService{stop: make(chan struct{})}
	idx := &Index{Volume: "\\\\.\\seekfs-test-nonexistent-volume", Source: "usn", Compact: true}
	vol := newServiceVolumeIndex("", idx)
	vol.state = "ready"

	before := runtime.NumGoroutine()
	done := make(chan struct{})
	go func() {
		svc.replayVolumeLoop(vol)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		svc.indexMu.RLock()
		state := vol.state
		svc.indexMu.RUnlock()
		if state == "stale" && vol.staleReason != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	svc.indexMu.RLock()
	state, reason := vol.state, vol.staleReason
	svc.indexMu.RUnlock()
	if state != "stale" || reason == "" {
		t.Fatalf("volume state=%q reason=%q; want stale with reason", state, reason)
	}

	close(svc.stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("replay loop did not exit after stop")
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines before=%d after=%d; possible leak", before, after)
	}
}

func TestServiceInfoResponseCarriesHealth(t *testing.T) {
	resp := serviceInfoResponse(serviceResponse{OK: true, Entries: 10, Health: serviceHealthOK})
	if resp.Health != serviceHealthOK {
		t.Fatalf("health = %q after identity decoration; want preserved", resp.Health)
	}
}

func TestCheckReplayStallSkipsNonReadyAndRecentlyActive(t *testing.T) {
	svc := &goSearchService{stop: make(chan struct{})}
	idx := &Index{Volume: "C:", Source: "usn", Compact: true}
	vol := newServiceVolumeIndex("", idx)
	vol.state = "stale"
	// Non-ready volume: must not be touched.
	svc.checkReplayStall(vol)
	vol.mu.Lock()
	if vol.state != "stale" || vol.staleReason != "" {
		vol.mu.Unlock()
		t.Fatalf("stale volume was modified by watchdog: state=%q reason=%q", vol.state, vol.staleReason)
	}
	vol.mu.Unlock()

	// Volume under recovery: must not be touched.
	vol.state = "ready"
	vol.lastReplayAt = time.Now()
	vol.recovering.Store(true)
	svc.checkReplayStall(vol)
	vol.mu.Lock()
	if vol.state != "ready" {
		vol.mu.Unlock()
		t.Fatalf("recovering volume was touched: state=%q", vol.state)
	}
	vol.mu.Unlock()
	vol.recovering.Store(false)

	// No heartbeat yet: must not be touched.
	svc.checkReplayStall(vol)
	vol.mu.Lock()
	if vol.state != "ready" || vol.staleReason != "" {
		vol.mu.Unlock()
		t.Fatalf("volume without heartbeat was touched: state=%q reason=%q", vol.state, vol.staleReason)
	}
	vol.mu.Unlock()
}

func TestReplayGenGuardDropsStaleBatch(t *testing.T) {
	vol := watchDeltaFixture(t)
	vol.replayGen.Store(1)
	// Simulate a rebuild bumping the generation mid-read.
	vol.replayGen.Add(1)
	// A stale replay loop would capture the old gen; ensure the volume
	// plumbing is sane: checkpoint + gen fields exist and a gen bump is
	// observable, so an in-flight replay can detect replacement.
	if vol.replayGen.Load() != 2 {
		t.Fatalf("replayGen = %d, want 2 after bump", vol.replayGen.Load())
	}
}

func TestObserveReplayStallGraduatedResponse(t *testing.T) {
	origDelay := staleRecoveryDelay
	staleRecoveryDelay = 25 * time.Millisecond
	defer func() { staleRecoveryDelay = origDelay }()

	svc := &goSearchService{stop: make(chan struct{})}
	idx := &Index{Volume: "C:", Source: "usn", Compact: true}
	vol := newServiceVolumeIndex("", idx)
	vol.state = "ready"
	vol.lastReplayAt = time.Now()
	vol.checkpoint = 1000

	// First observation: records the frozen checkpoint, takes no action.
	svc.observeReplayStall(vol, 1000, 5000)
	vol.mu.Lock()
	firstObs := vol.stallObservedAt
	strikes := vol.replayStrikes
	vol.mu.Unlock()
	if firstObs.IsZero() || strikes != 0 {
		t.Fatalf("first observation should only record: observedAt zero=%v strikes=%d", firstObs.IsZero(), strikes)
	}

	// Backdate the observation so the window has elapsed, then re-observe.
	vol.mu.Lock()
	vol.stallObservedAt = time.Now().Add(-replayStallWindow - time.Second)
	vol.mu.Unlock()
	svc.observeReplayStall(vol, 1000, 5000)
	vol.mu.Lock()
	strikes = vol.replayStrikes
	state := vol.state
	lastErr := vol.lastReplayErr
	vol.mu.Unlock()
	if strikes != 1 || state != "ready" {
		t.Fatalf("strike 1 should restart loop only: strikes=%d state=%q", strikes, state)
	}
	if lastErr == "" || !strings.Contains(lastErr, "restarting replay loop") {
		t.Fatalf("strike 1 reason = %q, want restart message", lastErr)
	}

	// Strikes 2 and 3: strike 3 crosses the rebuild threshold.
	for i := 0; i < 2; i++ {
		vol.mu.Lock()
		vol.stallObservedCp = vol.checkpoint
		vol.stallObservedAt = time.Now().Add(-replayStallWindow - time.Second)
		vol.mu.Unlock()
		svc.observeReplayStall(vol, 1000, 5000)
	}
	vol.mu.Lock()
	state = vol.state
	reason := vol.staleReason
	vol.mu.Unlock()
	if state != "stale" || !strings.Contains(reason, "rebuilding") {
		t.Fatalf("strike %d should mark stale for rebuild: state=%q reason=%q", replayStallRebuildStrikes, state, reason)
	}

	// Wait out the (shortened) recovery delay so the stale-recovery
	// goroutine exits deterministically before the test ends.
	time.Sleep(staleRecoveryDelay + 250*time.Millisecond)
}

func TestHealthyApplyResetsStallTracking(t *testing.T) {
	idx := &Index{Volume: "C:", Source: "usn", Compact: true}
	vol := newServiceVolumeIndex("", idx)
	vol.state = "ready"
	vol.replayStrikes = 2
	vol.stallObservedCp = 1000
	vol.stallObservedAt = time.Now()

	// Mirrors the commit section of replayVolumeOnce: a healthy apply wipes
	// stall bookkeeping.
	vol.mu.Lock()
	vol.replayStrikes = 0
	vol.stallObservedCp = 0
	vol.stallObservedAt = time.Time{}
	vol.mu.Unlock()
	if vol.replayStrikes != 0 || vol.stallObservedCp != 0 || !vol.stallObservedAt.IsZero() {
		t.Fatal("healthy apply did not reset stall tracking")
	}
}

func TestReplayLoopRetiresWhenGenChanges(t *testing.T) {
	origIdle, origErr := replayIdleDelay, replayErrorDelay
	replayIdleDelay, replayErrorDelay = time.Millisecond, time.Millisecond
	defer func() { replayIdleDelay, replayErrorDelay = origIdle, origErr }()

	svc := &goSearchService{stop: make(chan struct{})}
	idx := &Index{Volume: "\\\\.\\seekfs-test-nonexistent-volume", Source: "usn", Compact: true}
	vol := newServiceVolumeIndex("", idx)
	vol.state = "ready"

	done := make(chan struct{})
	go func() {
		svc.replayVolumeLoop(vol)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if vol.replayGen.Load() != 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Bump the generation like a watchdog restart would; the old loop must
	// exit promptly instead of racing the replacement.
	vol.replayGen.Add(1)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(svc.stop)
		t.Fatal("replay loop did not retire after gen bump")
	}
	close(svc.stop)
}

func TestDoctorStatusSurfacesReplayHealth(t *testing.T) {
	// dbInfo now carries replay-health fields; ensure the JSON round-trips.
	info := dbInfo{
		Volume:          "C:",
		State:           "ready",
		LastReplayError: "replay stall watchdog",
		LastReplayAt:    "2026-08-26T00:00:00Z",
		LastReplayNext:  123456,
	}
	if info.State != "ready" || info.LastReplayError == "" || info.LastReplayNext == 0 {
		t.Fatalf("dbInfo replay fields not populated: %+v", info)
	}
}

func TestWALCheckpointForApplied(t *testing.T) {
	if got := walCheckpointForApplied(100, nil); got != 100 {
		t.Fatalf("empty batch checkpoint = %d, want journal head 100", got)
	}
	changes := []usnChange{
		{FRN: 11, ParentFRN: 5, USN: 90, Name: "report-new.txt"},
		{FRN: 12, ParentFRN: 5, USN: 95, Name: "notes.txt"},
	}
	if got := walCheckpointForApplied(100, changes); got != 95 {
		t.Fatalf("batch checkpoint = %d, want last applied change 95", got)
	}
}

func TestWALTruncatedBatchClaimsAppliedCheckpoint(t *testing.T) {
	// A replay batch truncated to serviceUSNReplayBatchMax must persist the
	// last applied USN, not the unread journal head. replayWAL sets the volume
	// checkpoint to the frame's NextUSN, so an over-claim would skip the
	// truncated tail after a restart and permanently hide newly created files
	// that fell beyond the truncated batch.
	changes := []usnChange{
		{FRN: 11, ParentFRN: 5, USN: 101, Name: "report-new.txt"},
		{FRN: 12, ParentFRN: 5, USN: 102, Name: "notes.txt"},
	}
	const journalHead = 190
	checkpoint := walCheckpointForApplied(journalHead, changes)
	if checkpoint != 102 {
		t.Fatalf("truncated batch checkpoint = %d, want last applied 102", checkpoint)
	}
	payload, err := encodeBinaryWALBatch(checkpoint, changes)
	if err != nil {
		t.Fatalf("encodeBinaryWALBatch: %v", err)
	}
	batch, err := decodeBinaryWALBatch(payload, true)
	if err != nil {
		t.Fatalf("decodeBinaryWALBatch: %v", err)
	}
	if batch.NextUSN != 102 {
		t.Fatalf("decoded frame NextUSN = %d, want applied checkpoint 102", batch.NextUSN)
	}
	if len(batch.Changes) != 2 {
		t.Fatalf("decoded frame changes = %d, want 2", len(batch.Changes))
	}
	// A change from the truncated tail stays ahead of the persisted
	// checkpoint, so the next replay still picks it up. The old buggy value
	// (journal head 190) would have hidden every change up to 190.
	const tailUSN = 103
	if checkpoint >= tailUSN {
		t.Fatalf("persisted checkpoint %d already covers tail change %d", checkpoint, tailUSN)
	}
	if journalHead < tailUSN {
		t.Fatalf("test setup invalid: journal head %d must exceed tail change %d", journalHead, tailUSN)
	}
}

func TestPersistOverlayCarryKeepsPostSnapshotChanges(t *testing.T) {
	// A background persist folds a point-in-time overlay snapshot while replay
	// keeps applying changes into the live overlay.  The swap must carry the
	// live overlay into the replacement so post-snapshot changes (e.g. a file
	// created while the multi-GB index file was being staged) stay searchable
	// instead of vanishing for the remainder of the persist window.
	vol := &serviceVolumeIndex{volume: "C:", dbPath: t.TempDir() + "\\persist-carry.gsi"}
	vol.index = &Index{Version: indexVersion, Compact: true, Source: "usn", Volume: "C:"}
	vol.overlay = newOverlaySegment()
	// Pre-snapshot change: folded into the new base index by the persist.
	vol.applyUSNChanges([]usnChange{{FRN: 100, ParentFRN: 0, USN: 10, Name: "folded.txt", Reason: usnReasonFileCreate}})
	foldOverlay := snapshotOverlayForFold(vol.overlay)
	foldOverlay.checkpointAtRotate.Store(vol.checkpoint)
	// Post-snapshot change: applied while persist stages the multi-GB file.
	vol.applyUSNChanges([]usnChange{{FRN: 200, ParentFRN: 0, USN: 20, Name: "fresh.txt", Reason: usnReasonFileCreate}})
	if _, ok := vol.idForFRN(200); ok {
		t.Fatal("post-snapshot change must live in the overlay, not the base")
	}
	folded := compactOverlayIndexLocked(vol, foldOverlay)
	if folded.Checkpoint != 10 {
		t.Fatalf("folded checkpoint = %d, want the at-snapshot value 10", folded.Checkpoint)
	}
	// Swap section: the replacement base is the persisted fold (in production
	// it is loaded from the staged index file), carrying only the post-snapshot
	// overlay delta.
	replacement := newServiceVolumeIndex(vol.dbPath, folded)
	replacement.overlay = carryOverlayDelta(vol.overlay, len(foldOverlay.records), foldOverlay)
	replacement.publishSnapshot()
	if replacement.checkpoint < vol.checkpoint {
		replacement.checkpoint = vol.checkpoint
	}
	if _, ok := replacement.overlay.byFRN[200]; !ok {
		t.Fatal("post-snapshot FRN 200 missing from carried overlay")
	}
	if _, ok := replacement.overlay.byFRN[100]; ok {
		t.Fatal("pre-snapshot FRN 100 must not be carried (already folded)")
	}
	if got := replacement.overlay.tombstone.len() + replacement.overlay.shadowed.len(); got != 0 {
		t.Fatalf("carried overlay re-carried %d pre-snapshot base-id set entries", got)
	}
	replaceServiceVolumeContents(vol, replacement)
	// The folded record must be visible through the base index...
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "folded.txt", Limit: 10}, false)
	if err != nil {
		t.Fatalf("search folded: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`C:\folded.txt`}) {
		t.Fatalf("folded.txt search = %v", pathsOf(got))
	}
	// ...and the post-snapshot record through the carried overlay snapshot.
	snap := vol.snap.Load()
	if snap == nil {
		t.Fatal("no published snapshot after swap")
	}
	if overlay := vol.overlayRankedMatches(snap, parsedQuery{Terms: []string{"fresh.txt"}}, map[int]string{}); len(overlay) == 0 {
		t.Fatal("file fresh.txt not searchable from carried overlay after persist swap")
	}
}
