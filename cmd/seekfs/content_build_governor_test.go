package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// stubGovernorSignals replaces every politeness probe with quiet stubs and
// pins the pause/skip counters to zero, restoring everything on cleanup.
// Tests then flip one signal at a time.
func stubGovernorSignals(t *testing.T) {
	t.Helper()
	restoreActive := contentUserActiveNow
	restoreBattery := contentOnBatteryNow
	restoreQueries := contentBuildQueriesActiveNow
	restoreInput := contentInputActiveNow
	restoreCPU := contentSystemBusyNow
	restoreDisk := contentDiskBusyNow
	restoreMode := contentBuildPauseModeOverride
	restoreDrive := contentSystemDriveLetter
	restorePause := contentBuildBatchPause
	restoreWindow := contentBuildQueryQuietWindow
	contentOnBatteryNow = func() bool { return false }
	// Query signals keep production-equivalent probes over reset state (see
	// below): the busy cases exercise the real recency/counter logic.
	contentUserActiveNow = func() bool {
		last := serviceLastQueryUnixNano.Load()
		if last == 0 {
			return false
		}
		return time.Since(time.Unix(0, last)) < contentBuildQueryQuietWindow
	}
	contentBuildQueriesActiveNow = func() bool { return serviceActiveQueries.Load() > 0 }
	contentInputActiveNow = func() bool { return false }
	contentSystemBusyNow = func() bool { return false }
	contentDiskBusyNow = func() bool { return false }
	contentBuildPauseModeOverride = func() string { return "" }
	contentSystemDriveLetter = func() string { return "Z:" }
	contentBuildBatchPause = 25 * time.Millisecond
	contentBuildPausesTaken.Store(0)
	contentBuildPauseSkips.Store(0)
	contentBuildPauseReasonsMu.Lock()
	contentBuildPauseReasons = make(map[string]int64)
	contentBuildPauseReasonsMu.Unlock()
	serviceLastQueryUnixNano.Store(0)
	serviceActiveQueries.Store(0)
	t.Cleanup(func() {
		contentUserActiveNow = restoreActive
		contentOnBatteryNow = restoreBattery
		contentBuildQueriesActiveNow = restoreQueries
		contentInputActiveNow = restoreInput
		contentSystemBusyNow = restoreCPU
		contentDiskBusyNow = restoreDisk
		contentBuildPauseModeOverride = restoreMode
		contentSystemDriveLetter = restoreDrive
		contentBuildBatchPause = restorePause
		contentBuildQueryQuietWindow = restoreWindow
		contentBuildPausesTaken.Store(0)
		contentBuildPauseSkips.Store(0)
		contentBuildPauseReasonsMu.Lock()
		contentBuildPauseReasons = make(map[string]int64)
		contentBuildPauseReasonsMu.Unlock()
		serviceLastQueryUnixNano.Store(0)
		serviceActiveQueries.Store(0)
	})
}

func TestContentBuildPauseNowSkipsWhenIdle(t *testing.T) {
	stubGovernorSignals(t)
	if got := contentBuildPauseNow(`E:\idx\vol.gsx`); got != 0 {
		t.Fatalf("idle pause = %v; want 0", got)
	}
	if n := contentBuildPauseSkips.Load(); n != 1 {
		t.Fatalf("skips = %d; want 1", n)
	}
	if n := contentBuildPausesTaken.Load(); n != 0 {
		t.Fatalf("taken = %d; want 0", n)
	}
}

func TestContentBuildPauseNowTakesWhenBusy(t *testing.T) {
	cases := []struct {
		name string
		busy func()
	}{
		{"recentQuery", func() { serviceNoteQueryActivity() }},
		{"inflightQuery", func() { serviceActiveQueryEnter() }},
		{"battery", func() { contentOnBatteryNow = func() bool { return true } }},
		{"input", func() { contentInputActiveNow = func() bool { return true } }},
		{"cpu", func() { contentSystemBusyNow = func() bool { return true } }},
		{"disk", func() { contentDiskBusyNow = func() bool { return true } }},
		{"systemDrive", func() { contentSystemDriveLetter = func() string { return "E:" } }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubGovernorSignals(t)
			tc.busy()
			if tc.name == "inflightQuery" {
				defer serviceActiveQueryLeave()
			}
			path := `E:\idx\vol.gsx`
			if tc.name == "systemDrive" {
				path = `E:\idx\vol.gsx`
			}
			if got := contentBuildPauseNow(path); got != contentBuildBatchPause {
				t.Fatalf("%s pause = %v; want %v", tc.name, got, contentBuildBatchPause)
			}
			if n := contentBuildPausesTaken.Load(); n != 1 {
				t.Fatalf("%s taken = %d; want 1", tc.name, n)
			}
		})
	}
}

func TestContentBuildPauseReasonsAttribute(t *testing.T) {
	stubGovernorSignals(t)
	if got := contentBuildBusyReason(`E:\idx\vol.gsx`); got != "" {
		t.Fatalf("idle reason = %q; want empty", got)
	}
	contentBuildPauseNow(`E:\idx\vol.gsx`)
	contentInputActiveNow = func() bool { return true }
	contentBuildPauseNow(`E:\idx\vol.gsx`)
	contentBuildPauseNow(`E:\idx\vol.gsx`)
	counts := contentBuildPauseReasonCounts()
	if counts["idle"] != 1 || counts["input"] != 2 {
		t.Fatalf("reason counts = %v; want idle=1 input=2", counts)
	}
	if got := contentBuildBusyReason(`E:\idx\vol.gsx`); got != "input" {
		t.Fatalf("busy reason = %q; want input", got)
	}
}

func TestContentBuildPauseModeOverride(t *testing.T) {
	stubGovernorSignals(t)
	// "never" skips even under load; "always" pauses even when idle.
	contentBuildQueriesActiveNow = func() bool { return true }
	contentBuildPauseModeOverride = func() string { return "never" }
	if got := contentBuildPauseNow(`E:\idx\vol.gsx`); got != 0 {
		t.Fatalf("never+busy pause = %v; want 0", got)
	}
	contentBuildQueriesActiveNow = func() bool { return false }
	contentBuildPauseModeOverride = func() string { return "always" }
	if got := contentBuildPauseNow(`E:\idx\vol.gsx`); got != contentBuildBatchPause {
		t.Fatalf("always+idle pause = %v; want %v", got, contentBuildBatchPause)
	}
	// Zero configured pause never sleeps, whatever the signals say.
	contentBuildBatchPause = 0
	if got := contentBuildPauseNow(`E:\idx\vol.gsx`); got != 0 {
		t.Fatalf("zero-pause pause = %v; want 0", got)
	}
}

func TestContentBuildOnSystemDrive(t *testing.T) {
	stubGovernorSignals(t)
	contentSystemDriveLetter = func() string { return "C:" }
	if !contentBuildOnSystemDrive(`C:\idx\vol.gsx`) {
		t.Fatal("C: sidecar on C: system drive must read as system")
	}
	if !contentBuildOnSystemDrive(`c:\idx\vol.gsx`) {
		t.Fatal("drive match must be case-insensitive")
	}
	if contentBuildOnSystemDrive(`E:\idx\vol.gsx`) {
		t.Fatal("E: sidecar on C: system drive must not read as system")
	}
	if contentBuildOnSystemDrive(``) {
		t.Fatal("empty path must not read as system")
	}
	contentSystemDriveLetter = func() string { return "" }
	if contentBuildOnSystemDrive(`C:\idx\vol.gsx`) {
		t.Fatal("unknown system drive must fail open (non-system)")
	}
}

// The PDH disk probe itself needs a real counter, so unit tests cover the
// decision wiring (injected contentDiskBusyNow, exercised in the busy
// matrix) plus the threshold boundary through the exported calibration
// helper. The live % Idle values are logged by the local harness.
func TestContentDiskBusyThreshold(t *testing.T) {
	stubGovernorSignals(t)
	contentDiskIdleBusyPct = 30.0
	for _, tc := range []struct {
		idle float64
		busy bool
	}{
		{95.0, false},
		{30.0, false}, // boundary: busy only strictly below
		{29.9, true},
		{0.0, true},
	} {
		if got := contentDiskBusyFromIdle(tc.idle); got != tc.busy {
			t.Fatalf("idle=%.1f busy=%v; want %v", tc.idle, got, tc.busy)
		}
	}
}

func TestServiceActiveQueriesCounter(t *testing.T) {
	serviceActiveQueries.Store(0)
	t.Cleanup(func() { serviceActiveQueries.Store(0) })
	if contentBuildQueriesActiveNow() {
		t.Fatal("no in-flight query must read as idle")
	}
	serviceActiveQueryEnter()
	serviceActiveQueryEnter()
	if !contentBuildQueriesActiveNow() {
		t.Fatal("two in-flight queries must read as active")
	}
	serviceActiveQueryLeave()
	if !contentBuildQueriesActiveNow() {
		t.Fatal("one remaining in-flight query must read as active")
	}
	serviceActiveQueryLeave()
	if contentBuildQueriesActiveNow() {
		t.Fatal("drained counter must read as idle")
	}
}

// A service build must publish byte-identical output whether the governor
// pauses every batch, never pauses, or adapts on an idle machine: the pause
// is a sleep, never a selection/extraction/assembly input. 70 tiny files
// cross one batch boundary so the pause branch actually executes.
func TestContentServiceBuildIdenticalAcrossGovernorModes(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	restoreRun := contentBuildRun
	restorePause := contentBuildBatchPause
	restoreSize := contentBuildBatchSize
	restoreHook := contentBuildSnapshotHook
	restoreBatchHook := contentBuildSnapshotBatchHook
	restoreIndexing := contentBuildIndexingHook
	restoreBackoff := contentBuildRetryBackoff
	restoreMode := contentBuildPauseModeOverride
	restoreActive := contentUserActiveNow
	restoreBattery := contentOnBatteryNow
	restoreQueries := contentBuildQueriesActiveNow
	restoreInput := contentInputActiveNow
	restoreCPU := contentSystemBusyNow
	restoreDisk := contentDiskBusyNow
	restoreDrive := contentSystemDriveLetter
	t.Cleanup(func() {
		contentBuildRun = restoreRun
		contentBuildBatchPause = restorePause
		contentBuildBatchSize = restoreSize
		contentBuildSnapshotHook = restoreHook
		contentBuildSnapshotBatchHook = restoreBatchHook
		contentBuildIndexingHook = restoreIndexing
		contentBuildRetryBackoff = restoreBackoff
		contentBuildPauseModeOverride = restoreMode
		contentUserActiveNow = restoreActive
		contentOnBatteryNow = restoreBattery
		contentBuildQueriesActiveNow = restoreQueries
		contentInputActiveNow = restoreInput
		contentSystemBusyNow = restoreCPU
		contentDiskBusyNow = restoreDisk
		contentSystemDriveLetter = restoreDrive
		serviceLastQueryUnixNano.Store(0)
	})
	contentBuildRun = func(fn func()) { fn() }
	contentBuildBatchPause = time.Millisecond
	contentBuildSnapshotHook = func(*goSearchService, *serviceVolumeIndex) {}
	contentBuildSnapshotBatchHook = func(*goSearchService, *serviceVolumeIndex) {}
	contentBuildIndexingHook = func(*contentVolumeState) {}
	contentBuildRetryBackoff = 0
	contentUserActiveNow = func() bool { return false }
	contentOnBatteryNow = func() bool { return false }
	contentBuildQueriesActiveNow = func() bool { return false }
	contentInputActiveNow = func() bool { return false }
	contentSystemBusyNow = func() bool { return false }
	contentDiskBusyNow = func() bool { return false }
	contentSystemDriveLetter = func() string { return "Z:" }
	serviceLastQueryUnixNano.Store(0)

	const n = 70
	dir := t.TempDir()
	var records []CompactRecord
	for i := 0; i < n; i++ {
		name := "f" + strconv.Itoa(i) + ".txt"
		body := "governor identity doc " + strconv.Itoa(i) + " needle\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		records = append(records, CompactRecord{FRN: uint64(i + 1), ParentFRN: uint64(i + 1), Parent: -1, Name: name, Size: int64(len(body))})
	}
	const journal = uint64(0x60E)
	const cp = int64(600)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})

	build := func(mode string) (data []byte, taken, skipped int64) {
		contentBuildPauseModeOverride = func() string { return mode }
		contentBuildPausesTaken.Store(0)
		contentBuildPauseSkips.Store(0)
		vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), records)
		s := contentTestService(t)
		s.ensureContentBuild(vol)
		if got := vol.content.stateOf(); got != contentStateReady {
			t.Fatalf("mode %q state = %q; want ready", mode, got)
		}
		data, err := os.ReadFile(gsx)
		if err != nil {
			t.Fatalf("mode %q read sidecar: %v", mode, err)
		}
		taken, skipped = contentBuildPausesTaken.Load(), contentBuildPauseSkips.Load()
		t.Logf("mode %q bytes=%d taken=%d skipped=%d", mode, len(data), taken, skipped)
		return data, taken, skipped
	}
	never, neverTaken, _ := build("never")
	always, alwaysTaken, _ := build("always")
	auto, autoTaken, autoSkipped := build("")
	if neverTaken != 0 {
		t.Fatalf("never took %d pauses; want 0", neverTaken)
	}
	if alwaysTaken == 0 {
		t.Fatal("always took no pause; the 70-doc build must cross a batch boundary")
	}
	if autoTaken != 0 || autoSkipped == 0 {
		t.Fatalf("idle auto took=%d skipped=%d; want 0 taken and >=1 skipped", autoTaken, autoSkipped)
	}
	if string(never) == "" || string(always) == "" || string(auto) == "" {
		t.Fatal("empty sidecar in at least one mode")
	}
	if len(never) != len(always) || len(never) != len(auto) {
		t.Fatalf("sidecar sizes differ: never=%d always=%d auto=%d", len(never), len(always), len(auto))
	}
	for i := range never {
		if never[i] != always[i] || never[i] != auto[i] {
			t.Fatalf("sidecar byte %d differs across governor modes", i)
		}
	}
}

