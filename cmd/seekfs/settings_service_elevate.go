package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// settings_service_elevate.go: the desktop apps start a standalone sibling
// service from their own (unelevated) process, but a drive build reads the raw
// volume and needs an elevated service. "settings set" is what the apps run
// through UAC, so when a write adds a drive and the target is that unelevated
// sibling, the elevated helper restarts it elevated with the same spawn spec
// before sending the write.

// uiServiceArgs is the argv the desktop apps use to start the sibling service.
// cmd/seekfs-native mirrors it in serviceSpawnSpecFor.
func uiServiceArgs(pipeName string, dbs []string) []string {
	args := []string{"service", "-lowmem", "-pipe", pipeName, "-sddl", defaultServiceSDDL}
	for _, db := range dbs {
		args = append(args, "-db", db)
	}
	return args
}

func configureUIServiceEnvironment(cmd *exec.Cmd) {
	env := os.Environ()
	env = upsertEnv(env, "SEEKFS_MEMORY_MODE", "lowmem")
	env = upsertEnv(env, "SEEKFS_STARTUP_WORKERS", "1")
	// The UI is the primary interface for the v9 engine; always run the
	// service against memory-mapped indexes with the global planner so v9
	// volumes stay at low RSS regardless of how the UI was launched.
	env = upsertEnv(env, "SEEKFS_GLOBAL_PLANNER", "1")
	cmd.Env = env
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// settingsPatchAddsVolume reports whether a settings-set payload turns
// indexing on for any volume.
func settingsPatchAddsVolume(raw []byte) bool {
	var patch struct {
		Volumes []struct {
			Indexed *bool `json:"indexed"`
		} `json:"volumes"`
	}
	if json.Unmarshal(raw, &patch) != nil {
		return false
	}
	for _, v := range patch.Volumes {
		if v.Indexed != nil && *v.Indexed {
			return true
		}
	}
	return false
}

// settingsServiceNeedsElevatedRestart reports whether the elevated helper
// should restart the service it talks to: an unelevated standalone service
// started from this same executable (the desktop apps' sibling). The Windows
// service and services from other binaries are left alone.
func settingsServiceNeedsElevatedRestart(info serviceResponse, helperElevated bool, helperExe string) bool {
	return helperElevated &&
		!info.Elevated &&
		strings.EqualFold(info.ProcessMode, "standalone") &&
		info.PID > 0 &&
		info.Executable != "" && helperExe != "" &&
		samePath(info.Executable, helperExe)
}

// Test hooks.
var (
	settingsHelperElevated = currentProcessElevated
	settingsRestartService = restartSiblingServiceElevated
)

// ensureSettingsServiceElevated restarts the unelevated sibling service
// elevated when raw is a write that adds a drive. Any other case is a no-op.
func ensureSettingsServiceElevated(pipe string, raw []byte) error {
	if !settingsPatchAddsVolume(raw) {
		return nil
	}
	info, err := callService(pipe, serviceRequest{Command: "info"})
	if err != nil {
		// Let settings-set report the connection failure.
		return nil
	}
	exe, _ := os.Executable()
	if !settingsServiceNeedsElevatedRestart(info, settingsHelperElevated(), exe) {
		return nil
	}
	if err := settingsRestartService(info, pipe); err != nil {
		return fmt.Errorf("restart the seekfs service as administrator to index drives: %w", err)
	}
	return nil
}

const siblingServiceReadyTimeout = 60 * time.Second

// restartSiblingServiceElevated stops the unelevated sibling service and
// starts it again from this elevated process, so the new service inherits
// the elevated token. It waits until the new service answers and has
// finished loading.
func restartSiblingServiceElevated(info serviceResponse, pipe string) error {
	if err := stopServiceProcess(info.PID, pipe); err != nil {
		return err
	}
	cmd := exec.Command(info.Executable, uiServiceArgs(pipe, nil)...)
	cmd.Dir = filepath.Dir(info.Executable)
	configureUIServiceEnvironment(cmd)
	prepareUIServiceCommand(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(siblingServiceReadyTimeout)
	for {
		resp, err := callServiceOnce(pipe, serviceRequest{Command: "info"})
		if err == nil && resp.Elevated && !resp.Loading {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = errors.New("service did not finish loading")
			}
			return fmt.Errorf("restarted service did not become ready: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// stopServiceProcess terminates pid and waits until the pipe stops answering.
func stopServiceProcess(pid int, pipe string) error {
	if pid == os.Getpid() {
		return errors.New("refusing to stop the current process")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Kill(); err != nil {
		return fmt.Errorf("stop service pid %d: %w", pid, err)
	}
	_ = proc.Release()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := callServiceOnce(pipe, serviceRequest{Command: "ping"}); err != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("stopped service still answers on its pipe")
}
