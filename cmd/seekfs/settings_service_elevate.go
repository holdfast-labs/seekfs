package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
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

// Test hook.
var settingsInstallService = installAppWindowsService

const appServiceReadyTimeout = 2 * time.Minute

// installAppWindowsService registers this executable as the seekfs Windows
// service for the desktop apps and starts it: LocalSystem, delayed automatic
// start, and no -db flags, so the service follows the configured drive list
// (drives added later in Settings survive a restart). A standalone sibling on
// the pipe is stopped first so the service can own the pipe. An existing
// seekfs service is reconfigured in place rather than deleted and recreated.
func installAppWindowsService(pipe string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if info, err := callService(pipe, serviceRequest{Command: "info"}); err == nil {
		if strings.EqualFold(info.ProcessMode, "windows-service") {
			return nil
		}
		if !strings.EqualFold(info.ProcessMode, "standalone") || !samePath(info.Executable, exe) {
			return fmt.Errorf("another seekfs process (%s) is using %s; stop it and try again", firstNonEmpty(info.Executable, "unknown"), pipe)
		}
		if err := stopServiceProcess(info.PID, pipe); err != nil {
			return err
		}
	}
	if err := configureAppWindowsService(exe, appServiceArgs(pipe)); err != nil {
		return err
	}
	if err := startWindowsService(); err != nil {
		return fmt.Errorf("start the seekfs service: %w", err)
	}
	deadline := time.Now().Add(appServiceReadyTimeout)
	for {
		resp, err := callServiceOnce(pipe, serviceRequest{Command: "info"})
		if err == nil && strings.EqualFold(resp.ProcessMode, "windows-service") {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("pipe answered as %q", resp.ProcessMode)
			}
			return fmt.Errorf("the seekfs service did not start: %w", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// appServiceArgs is the installed service command line for the apps. Unlike
// "seekfs install", it never pins -db paths.
func appServiceArgs(pipe string) []string {
	return []string{"service", "-pipe", pipe, "-sddl", defaultServiceSDDL}
}

func configureAppWindowsService(exe string, args []string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	const description = "Indexes file names (and optionally contents) for seekfs search."
	s, err := m.OpenService(serviceName)
	if err != nil {
		s, err = m.CreateService(serviceName, exe, mgr.Config{
			DisplayName:      "seekfs indexing service",
			Description:      description,
			StartType:        mgr.StartAutomatic,
			DelayedAutoStart: true,
		}, args...)
		if err != nil {
			return fmt.Errorf("install the seekfs service: %w", err)
		}
		s.Close()
		return nil
	}
	defer s.Close()
	if err := stopServiceIfExists(); err != nil {
		return err
	}
	cfg, err := s.Config()
	if err != nil {
		return err
	}
	// "seekfs install"/"launch" pin the index list on the command line, which
	// the new command line drops; keep those indexes in the config instead.
	if err := carryPinnedServiceDBs(appServiceConfigPath(exe), pinnedServiceDBs(cfg.BinaryPathName)); err != nil {
		return fmt.Errorf("keep the service's index list: %w", err)
	}
	cmdline := syscall.EscapeArg(exe)
	for _, arg := range args {
		cmdline += " " + syscall.EscapeArg(arg)
	}
	cfg.BinaryPathName = cmdline
	cfg.StartType = mgr.StartAutomatic
	cfg.DelayedAutoStart = true
	cfg.Description = description
	if err := s.UpdateConfig(cfg); err != nil {
		return fmt.Errorf("update the seekfs service: %w", err)
	}
	return nil
}

// appServiceConfigPath is the seekfs.toml the LocalSystem service reads. Its
// working directory and profile hold none, so its config search finds
// %ProgramData%\seekfs before the executable's folder.
func appServiceConfigPath(exe string) string {
	primary := defaultConfigPath()
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	if beside := filepath.Join(filepath.Dir(exe), "seekfs.toml"); exe != "" {
		if _, err := os.Stat(beside); err == nil {
			return beside
		}
	}
	return primary
}

// pinnedServiceDBs returns the -db paths on an installed service command line.
func pinnedServiceDBs(binaryPath string) []string {
	args, err := windows.DecomposeCommandLine(binaryPath)
	if err != nil {
		return nil
	}
	var dbs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case (arg == "-db" || arg == "--db") && i+1 < len(args):
			dbs = append(dbs, args[i+1])
			i++
		case strings.HasPrefix(arg, "-db="):
			dbs = append(dbs, strings.TrimPrefix(arg, "-db="))
		case strings.HasPrefix(arg, "--db="):
			dbs = append(dbs, strings.TrimPrefix(arg, "--db="))
		}
	}
	return dbs
}

// carryPinnedServiceDBs adds pinned index paths to the config's dbs list so
// a service that follows the config still loads them.
func carryPinnedServiceDBs(configPath string, pinned []string) error {
	if len(pinned) == 0 {
		return nil
	}
	var dbs []string
	if _, err := os.Stat(configPath); err == nil {
		cfg, err := loadConfig(configPath)
		if err != nil {
			return err
		}
		dbs = append(dbs, cfg.DBs...)
	}
	changed := false
	for _, db := range pinned {
		if !settingsContainsDB(dbs, db) {
			dbs = append(dbs, db)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return setConfigKey(configPath, "dbs", formatStringArray(dbs))
}
