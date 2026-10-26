package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSettingsGetReportsBuildProgressAndClearsIt(t *testing.T) {
	preservePluginSelection(t)
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	useSyntheticSettingsVolumes(t)
	configPath := settingsTestConfig(t, "default_limit = 10\n")
	s := settingsTestService(t, configPath, nil)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	useSettingsIndexProgressStub(t, func(volume string, progress *volumeBuildProgress) (*Index, error) {
		progress.setPhase("building")
		progress.setCounts(120, 290)
		started <- struct{}{}
		<-release
		return settingsTestBuildIndex(volume), nil
	})

	add := callSettingsCommand(t, s, serviceCapabilities{ReadOnly: true, Mutate: true}, settingsPatchRequest(t, map[string]any{"volumes": []map[string]any{{"volume": "F:", "indexed": true}}}))
	if !add.OK {
		t.Fatalf("add volume = %+v", add)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("background build did not start")
	}

	entry := settingsVolumeEntry(t, s, "F:")
	if entry.State != "indexing" || entry.Progress == nil {
		t.Fatalf("indexing F: = %+v, want progress", entry)
	}
	if entry.Progress.Phase != "building" || entry.Progress.Done != 120 || entry.Progress.Total != 290 {
		t.Fatalf("progress = %+v", entry.Progress)
	}
	if entry.Progress.StartedAt == "" {
		t.Fatalf("progress started_at is empty: %+v", entry.Progress)
	}

	close(release)
	ready := waitSettingsVolumeState(t, s, "F:", "ready")
	if ready.Progress != nil {
		t.Fatalf("ready F: still reports progress: %+v", ready.Progress)
	}
}

func TestMFTProgressTrackerIsMonotonicAndThrottled(t *testing.T) {
	var got []uint64
	var totals []uint64
	tracker := newMFTProgressTracker(100000, func(done, total uint64) {
		got = append(got, done)
		totals = append(totals, total)
	})
	for i := uint64(1); i <= 20000; i++ {
		tracker.record(i)
	}
	tracker.finish(20000)
	if len(got) < 4 {
		t.Fatalf("progress callbacks = %d, want at least 4", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("progress not monotonic: %v", got)
		}
	}
	for _, total := range totals {
		if total != 100000 {
			t.Fatalf("total = %d, want 100000", total)
		}
	}
	if got[len(got)-1] != 20000 {
		t.Fatalf("final done = %d, want 20000", got[len(got)-1])
	}
}

func TestSettingsHelperWritesInstallProgressPhases(t *testing.T) {
	preservePluginSelection(t)
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	useSyntheticSettingsVolumes(t)
	useSettingsIndexStub(t, func(volume string) (*Index, error) {
		return settingsTestBuildIndex(volume), nil
	})
	configPath := settingsTestConfig(t, "default_limit = 10\n")
	s := settingsTestService(t, configPath, nil)
	pipeName := startSettingsTestService(t, s)

	previous := settingsInstallService
	t.Cleanup(func() { settingsInstallService = previous })
	settingsInstallService = func(_ string, progress *settingsProgress) error {
		return installServiceSteps{
			stopSibling: func() error { return nil },
			configure:   func() error { return nil },
			start:       func() error { return nil },
			wait:        func() error { return nil },
		}.run(progress)
	}

	patch := filepath.Join(t.TempDir(), "drive.json")
	if err := os.WriteFile(patch, []byte(`{"volumes":[{"volume":"F:","indexed":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	progressPath := filepath.Join(t.TempDir(), "progress.jsonl")
	_, _ = runCLICaptured(t, []string{"settings", "set", "--json", patch, "--pipe", pipeName, "--install-service", "--progress", progressPath})

	got := readSettingsProgressPhases(t, progressPath)
	want := []string{
		progressPhaseStart,
		progressPhaseInstallStop,
		progressPhaseInstallConfig,
		progressPhaseInstallStart,
		progressPhaseInstallWait,
		progressPhaseApply,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("phases = %v, want %v", got, want)
	}
}

func TestRestartServiceStepsWriteProgressPhases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.jsonl")
	progress := newSettingsProgress(path)
	err := restartServiceSteps{
		stop:  func() error { return nil },
		start: func() error { return nil },
		wait:  func() error { return nil },
	}.run(progress)
	if err != nil {
		t.Fatal(err)
	}
	got := readSettingsProgressPhases(t, path)
	want := []string{progressPhaseRestartStop, progressPhaseRestartStart, progressPhaseRestartWait}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("phases = %v, want %v", got, want)
	}
}

func readSettingsProgressPhases(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open progress file: %v", err)
	}
	defer f.Close()
	var phases []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line struct {
			Phase string `json:"phase"`
			At    string `json:"at"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("progress line %q: %v", scanner.Text(), err)
		}
		if line.Phase == "" || line.At == "" {
			t.Fatalf("progress line missing fields: %q", scanner.Text())
		}
		phases = append(phases, line.Phase)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return phases
}
