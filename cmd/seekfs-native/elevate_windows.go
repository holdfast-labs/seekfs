//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// elevate_windows.go: retry a settings write through
// "seekfs.exe settings set --json <file>" with the runas verb. The helper
// lives next to the UI executable.

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	seeMaskFlagNoUI       = 0x00000400
	swHide                = 0
)

type shellExecuteInfo struct {
	CbSize         uint32
	FMask          uint32
	Hwnd           uintptr
	LpVerb         *uint16
	LpFile         *uint16
	LpParameters   *uint16
	LpDirectory    *uint16
	NShow          int32
	HInstApp       uintptr
	LpIDList       uintptr
	LpClass        *uint16
	HkeyClass      uintptr
	DwHotKey       uint32
	HIconOrMonitor uintptr
	HProcess       uintptr
}

var pShellExecuteExW = proc(modShell32, "ShellExecuteExW")

// settingsHelperPath returns the CLI next to the running UI executable.
func settingsHelperPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), "seekfs.exe"), nil
}

// runElevatedSettings writes patch to a temp file and runs the elevated CLI
// helper against pipeName, first installing the Windows service when
// installService is set. It waits for the helper to exit and reports a
// non-zero status.
func runElevatedSettings(patch *serviceSettings, pipeName string, installService bool, onPhase func(string)) error {
	helper, err := settingsHelperPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(helper); err != nil {
		return fmt.Errorf("seekfs.exe not found next to the app: %w", err)
	}
	if patch == nil {
		patch = &serviceSettings{}
	}
	data, err := patch.marshal()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "seekfs-settings-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	defer os.Remove(name)
	// The elevated helper's stderr is not visible to the app, so it writes
	// its outcome (including a validation message) to a result file.
	resultName := name + ".result"
	defer os.Remove(resultName)
	// The elevated helper appends one JSON line per step so the Apply area
	// can say which step is slow.
	progressName := name + ".progress"
	defer os.Remove(progressName)

	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(helper)
	params, _ := syscall.UTF16PtrFromString(elevatedSettingsArgs(name, resultName, progressName, pipeName, installService))
	dir, _ := syscall.UTF16PtrFromString(filepath.Dir(helper))
	sei := shellExecuteInfo{
		CbSize:       uint32(unsafe.Sizeof(shellExecuteInfo{})),
		FMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		LpVerb:       verb,
		LpFile:       file,
		LpParameters: params,
		LpDirectory:  dir,
		NShow:        swHide,
	}
	r, _, errno := syscall.SyscallN(pShellExecuteExW.Addr(), uintptr(unsafe.Pointer(&sei)))
	if r == 0 {
		if errno == syscall.Errno(windows.ERROR_CANCELLED) {
			return errors.New("elevation was cancelled")
		}
		return fmt.Errorf("run elevated settings: %w", errno)
	}
	if sei.HProcess == 0 {
		return errors.New("elevated settings helper did not start")
	}
	defer windows.CloseHandle(windows.Handle(sei.HProcess))
	done := make(chan error, 1)
	go func() {
		if _, err := windows.WaitForSingleObject(windows.Handle(sei.HProcess), windows.INFINITE); err != nil {
			done <- err
			return
		}
		var code uint32
		if err := windows.GetExitCodeProcess(windows.Handle(sei.HProcess), &code); err != nil {
			done <- err
			return
		}
		if code != 0 {
			var result struct {
				Message string `json:"message"`
			}
			if data, err := os.ReadFile(resultName); err == nil && json.Unmarshal(data, &result) == nil && result.Message != "" {
				done <- errors.New(result.Message)
				return
			}
			done <- fmt.Errorf("elevated settings helper exited with code %d", code)
			return
		}
		done <- nil
	}()

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	var progressOffset int64
	for {
		select {
		case err := <-done:
			pollSettingsProgress(progressName, &progressOffset, onPhase)
			return err
		case <-ticker.C:
			pollSettingsProgress(progressName, &progressOffset, onPhase)
		}
	}
}

// pollSettingsProgress reads new JSON progress lines and reports each phase.
func pollSettingsProgress(path string, offset *int64, onPhase func(string)) {
	if onPhase == nil || path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(*offset, 0); err != nil {
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	lastNewline := bytes.LastIndexByte(data, '\n')
	if lastNewline < 0 {
		return
	}
	for _, raw := range bytes.Split(data[:lastNewline], []byte{'\n'}) {
		var line struct {
			Phase string `json:"phase"`
		}
		if json.Unmarshal(raw, &line) == nil && line.Phase != "" {
			onPhase(line.Phase)
		}
	}
	*offset += int64(lastNewline + 1)
}

// elevatedSettingsArgs is the helper command line. The pipe is passed so a
// service on a non-default pipe receives the write.
func elevatedSettingsArgs(patchPath, resultPath, progressPath, pipeName string, installService bool) string {
	args := "settings set --json " + quoteArg(patchPath) + " --result " + quoteArg(resultPath)
	if progressPath != "" {
		args += " --progress " + quoteArg(progressPath)
	}
	if pipeName != "" {
		args += " --pipe " + quoteArg(pipeName)
	}
	if installService {
		args += " --install-service"
	}
	return args
}

// quoteArg quotes a path for the Windows command line.
func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	needs := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	out := `"`
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out += `\`
		}
		out += string(s[i])
	}
	return out + `"`
}
