package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsPatchAddsVolume(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"volumes":[{"volume":"D:","indexed":true}]}`, true},
		{`{"volumes":[{"volume":"D:","indexed":false},{"volume":"E:","indexed":true}]}`, true},
		{`{"volumes":[{"volume":"D:","indexed":false}]}`, false},
		{`{"volumes":[{"volume":"D:"}]}`, false},
		{`{"content":{"mode":"auto"}}`, false},
		{`not json`, false},
	} {
		if got := settingsPatchAddsVolume([]byte(tc.raw)); got != tc.want {
			t.Errorf("settingsPatchAddsVolume(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestSettingsServiceNeedsElevatedRestart(t *testing.T) {
	exe := `C:\seekfs\seekfs.exe`
	sibling := serviceResponse{PID: 42, ProcessMode: "standalone", Executable: exe}
	if !settingsServiceNeedsElevatedRestart(sibling, true, exe) {
		t.Fatal("unelevated standalone sibling must be restarted elevated")
	}
	elevated := sibling
	elevated.Elevated = true
	installed := sibling
	installed.ProcessMode = "windows-service"
	foreign := sibling
	foreign.Executable = `C:\other\seekfs.exe`
	noPID := sibling
	noPID.PID = 0
	for name, info := range map[string]serviceResponse{
		"already elevated": elevated,
		"windows service":  installed,
		"other binary":     foreign,
		"no pid":           noPID,
	} {
		if settingsServiceNeedsElevatedRestart(info, true, exe) {
			t.Errorf("%s: restart requested", name)
		}
	}
	if settingsServiceNeedsElevatedRestart(sibling, false, exe) {
		t.Error("an unelevated helper cannot start an elevated service")
	}
}

func TestSettingsVolumeErrorMessageNamesMissingElevation(t *testing.T) {
	const reason = "Access is denied."
	if got := settingsVolumeErrorMessage(reason, true); got != reason {
		t.Fatalf("elevated message = %q", got)
	}
	got := settingsVolumeErrorMessage(reason, false)
	if !strings.Contains(got, "not running as administrator") || !strings.Contains(got, reason) {
		t.Fatalf("unelevated message = %q", got)
	}
	if got := settingsVolumeErrorMessage("", false); got != "" {
		t.Fatalf("empty reason = %q", got)
	}
}

// A loaded volume that went stale (an unelevated service cannot open the
// volume for USN catch-up) reports its reason, not just "error".
func TestSettingsVolumeListReportsStaleReason(t *testing.T) {
	prev := serviceProcessElevated
	t.Cleanup(func() { serviceProcessElevated = prev })
	serviceProcessElevated = func() bool { return false }
	vol := &serviceVolumeIndex{volume: "D:", index: &Index{}, state: "stale", staleReason: "Access is denied."}
	list := settingsVolumeList(nil, []*serviceVolumeIndex{vol}, false, nil)
	if len(list) != 1 || list[0].State != "error" {
		t.Fatalf("list = %+v", list)
	}
	if !strings.Contains(list[0].Error, "Access is denied.") || !strings.Contains(list[0].Error, "not running as administrator") {
		t.Fatalf("error = %q", list[0].Error)
	}
}

// TestSettingsCLISetRestartsUnelevatedSiblingForNewDrive drives "settings set"
// against an unelevated standalone service started from this executable: a
// write that adds a drive restarts the service elevated first; other writes
// do not.
func TestSettingsCLISetRestartsUnelevatedSiblingForNewDrive(t *testing.T) {
	preservePluginSelection(t)
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	useSyntheticSettingsVolumes(t)
	useSettingsIndexStub(t, func(volume string) (*Index, error) {
		return nil, errors.New("stub build")
	})
	configPath := settingsTestConfig(t, "default_limit = 10\n")
	s := settingsTestService(t, configPath, nil)
	s.processMode = "standalone"
	pipeName := startSettingsTestService(t, s)

	prevService, prevHelper, prevRestart := serviceProcessElevated, settingsHelperElevated, settingsRestartService
	t.Cleanup(func() {
		serviceProcessElevated, settingsHelperElevated, settingsRestartService = prevService, prevHelper, prevRestart
	})
	serviceProcessElevated = func() bool { return false }
	settingsHelperElevated = func() bool { return true }
	var restarts []serviceResponse
	settingsRestartService = func(info serviceResponse, pipe string) error {
		if pipe != pipeName {
			t.Errorf("restart pipe = %q, want %q", pipe, pipeName)
		}
		restarts = append(restarts, info)
		return nil
	}

	dir := t.TempDir()
	writePatch := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	content := writePatch("content.json", `{"content":{"mode":"auto"}}`)
	_, _ = runCLICaptured(t, []string{"settings", "set", "--json", content, "--pipe", pipeName})
	if len(restarts) != 0 {
		t.Fatalf("content-only write restarted the service %d time(s)", len(restarts))
	}

	drive := writePatch("drive.json", `{"volumes":[{"volume":"F:","indexed":true}]}`)
	_, _ = runCLICaptured(t, []string{"settings", "set", "--json", drive, "--pipe", pipeName})
	if len(restarts) != 1 {
		t.Fatalf("drive add restarted the service %d time(s), want 1", len(restarts))
	}
	if restarts[0].PID != os.Getpid() || restarts[0].Elevated {
		t.Fatalf("restart info = pid %d elevated %v", restarts[0].PID, restarts[0].Elevated)
	}

	restarts = nil
	serviceProcessElevated = func() bool { return true }
	_, _ = runCLICaptured(t, []string{"settings", "set", "--json", drive, "--pipe", pipeName})
	if len(restarts) != 0 {
		t.Fatal("an elevated service was restarted")
	}
}

func TestStopServiceProcessRefusesSelf(t *testing.T) {
	if err := stopServiceProcess(os.Getpid(), `\\.\pipe\unused`); err == nil {
		t.Fatal("stopServiceProcess stopped the current process")
	}
}
