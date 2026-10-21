package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// startup.go: service discovery and startup, mirroring cmd/seekfs/ui.go.
// The native app accepts an installed Windows service or a matching
// standalone sibling service. An unknown process that holds the pipe is
// refused, and a second service never starts while a verified one runs.

const defaultServiceSDDL = `D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)`

const nativeServiceBuildFlavor = "cli,service,lowmem"

const nativeServiceWaitTimeout = 15 * time.Second

// serviceIdentityKind is the classification of the process that answered
// the service info request.
type serviceIdentityKind int

const (
	serviceIdentityNone serviceIdentityKind = iota
	serviceIdentityInstalled
	serviceIdentityStandaloneSibling
	serviceIdentityForeign
	serviceIdentityStale
)

func (k serviceIdentityKind) String() string {
	switch k {
	case serviceIdentityInstalled:
		return "installed-service"
	case serviceIdentityStandaloneSibling:
		return "standalone-sibling"
	case serviceIdentityForeign:
		return "foreign"
	case serviceIdentityStale:
		return "stale"
	default:
		return "none"
	}
}

// serviceStartupInput is the observed state that decides startup.
type serviceStartupInput struct {
	PipeResponds  bool
	PipeTimeout   bool
	Identity      serviceIdentityKind
	SiblingExists bool
}

// serviceStartupAction is the decision for one observed state.
type serviceStartupAction int

const (
	serviceStartupConnect serviceStartupAction = iota
	serviceStartupSpawn
	serviceStartupRefuse
)

func (a serviceStartupAction) String() string {
	switch a {
	case serviceStartupSpawn:
		return "spawn"
	case serviceStartupRefuse:
		return "refuse"
	default:
		return "connect"
	}
}

type serviceStartupDecision struct {
	Action  serviceStartupAction
	Message string
}

// decideServiceStartup is the pure startup policy. A verified service is
// always connected to, so a second service never starts over it.
func decideServiceStartup(in serviceStartupInput) serviceStartupDecision {
	if in.PipeResponds {
		switch in.Identity {
		case serviceIdentityInstalled:
			return serviceStartupDecision{Action: serviceStartupConnect}
		case serviceIdentityStandaloneSibling:
			return serviceStartupDecision{Action: serviceStartupConnect}
		case serviceIdentityStale:
			return serviceStartupDecision{
				Action:  serviceStartupRefuse,
				Message: "A seekfs service with a stale identity is running on this pipe. Restart it or use another pipe.",
			}
		case serviceIdentityForeign:
			return serviceStartupDecision{
				Action:  serviceStartupRefuse,
				Message: "Another process is using the seekfs pipe. Stop it or choose a different pipe.",
			}
		default:
			return serviceStartupDecision{
				Action:  serviceStartupRefuse,
				Message: "The process on the seekfs pipe did not report a service identity. Restart it or choose a different pipe.",
			}
		}
	}
	if in.PipeTimeout {
		return serviceStartupDecision{
			Action:  serviceStartupRefuse,
			Message: "The seekfs service is busy and did not answer. Try again in a moment.",
		}
	}
	if in.SiblingExists {
		return serviceStartupDecision{
			Action:  serviceStartupSpawn,
			Message: "Starting seekfs service...",
		}
	}
	return serviceStartupDecision{
		Action:  serviceStartupRefuse,
		Message: "seekfs.exe was not found next to seekfs-native.exe. Install seekfs or start the service, then try again.",
	}
}

// serviceIdentityExpectation is the identity the native app requires from
// a standalone sibling service.
type serviceIdentityExpectation struct {
	Executable     string
	ExecutableHash string
	Version        string
	Commit         string
	Date           string
	BuildFlavor    string
	PipeName       string
}

// currentNativeServiceIdentity computes the expected sibling identity. The
// hash is taken from seekfs.exe beside the app, not from seekfs-native.exe.
func currentNativeServiceIdentity(appExecutable, pipeName string) serviceIdentityExpectation {
	sibling := siblingServiceExecutable(appExecutable)
	return serviceIdentityExpectation{
		Executable:     sibling,
		ExecutableHash: executableContentHash(sibling),
		Version:        version,
		Commit:         commit,
		Date:           date,
		BuildFlavor:    nativeServiceBuildFlavor,
		PipeName:       pipeName,
	}
}

func siblingServiceExecutable(appExecutable string) string {
	if strings.TrimSpace(appExecutable) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(appExecutable), "seekfs.exe")
}

// classifyServiceIdentity grades a service info response against the
// expected sibling identity.
func classifyServiceIdentity(resp serviceResponse, expected serviceIdentityExpectation) serviceIdentityKind {
	if resp.PID <= 0 {
		return serviceIdentityForeign
	}
	if resp.PipeName != "" && expected.PipeName != "" &&
		!strings.EqualFold(resp.PipeName, expected.PipeName) {
		return serviceIdentityStale
	}
	switch strings.ToLower(strings.TrimSpace(resp.ProcessMode)) {
	case "windows-service":
		if resp.BuildFlavor == "" || expected.BuildFlavor == "" ||
			resp.BuildFlavor != expected.BuildFlavor {
			return serviceIdentityStale
		}
		if !serviceBuildIdentityMatches(resp, expected) {
			return serviceIdentityStale
		}
		return serviceIdentityInstalled
	case "standalone":
		if resp.Executable == "" || expected.Executable == "" {
			return serviceIdentityStale
		}
		if !samePath(resp.Executable, expected.Executable) {
			return serviceIdentityForeign
		}
		if resp.ExecutableHash == "" || expected.ExecutableHash == "" ||
			resp.ExecutableHash != expected.ExecutableHash {
			return serviceIdentityStale
		}
		if resp.BuildFlavor == "" || expected.BuildFlavor == "" ||
			resp.BuildFlavor != expected.BuildFlavor {
			return serviceIdentityStale
		}
		if !serviceBuildIdentityMatches(resp, expected) {
			return serviceIdentityStale
		}
		return serviceIdentityStandaloneSibling
	default:
		return serviceIdentityForeign
	}
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	if errA == nil {
		a = absA
	}
	absB, errB := filepath.Abs(b)
	if errB == nil {
		b = absB
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func serviceBuildIdentityMatches(resp serviceResponse, expected serviceIdentityExpectation) bool {
	if resp.Version == "" || resp.Commit == "" || resp.Date == "" {
		return false
	}
	if expected.Version == "" || expected.Commit == "" || expected.Date == "" {
		return false
	}
	if isUnknownBuildIdentity(resp.Version, resp.Commit, resp.Date) ||
		isUnknownBuildIdentity(expected.Version, expected.Commit, expected.Date) {
		return true
	}
	return resp.Version == expected.Version && resp.Commit == expected.Commit && resp.Date == expected.Date
}

func isUnknownBuildIdentity(v, c, d string) bool {
	return v == "dev" || c == "unknown" || d == "unknown"
}

func executableContentHash(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// serviceSpawnSpec is the exact command used to start the sibling service.
type serviceSpawnSpec struct {
	Executable string
	Args       []string
	Dir        string
}

// serviceSpawnSpecFor builds the same argv as uiServiceArgs in cmd/seekfs.
func serviceSpawnSpecFor(appExecutable, pipeName string) serviceSpawnSpec {
	exe := siblingServiceExecutable(appExecutable)
	return serviceSpawnSpec{
		Executable: exe,
		Args: []string{
			"service",
			"-lowmem",
			"-pipe", pipeName,
			"-sddl", defaultServiceSDDL,
		},
		Dir: filepath.Dir(exe),
	}
}

// newNativeServiceCommand builds the hidden service process. It does not
// start it; tests inspect argv, working directory and window flags.
func newNativeServiceCommand(spec serviceSpawnSpec) *exec.Cmd {
	cmd := exec.Command(spec.Executable, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = nativeServiceEnvironment()
	prepareNativeServiceCommand(cmd)
	return cmd
}

func nativeServiceEnvironment() []string {
	env := os.Environ()
	env = upsertNativeEnv(env, "SEEKFS_MEMORY_MODE", "lowmem")
	env = upsertNativeEnv(env, "SEEKFS_STARTUP_WORKERS", "1")
	env = upsertNativeEnv(env, "SEEKFS_GLOBAL_PLANNER", "1")
	return env
}

func upsertNativeEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// nativeServiceSpawnHook lets tests observe the spawn without a process.
var nativeServiceSpawnHook func(serviceSpawnSpec) error

// startNativeService starts the sibling service and leaves it running. Like
// seekfs-ui, the app does not stop the service on exit.
func startNativeService(spec serviceSpawnSpec) error {
	if nativeServiceSpawnHook != nil {
		return nativeServiceSpawnHook(spec)
	}
	cmd := newNativeServiceCommand(spec)
	if err := cmd.Start(); err != nil {
		return err
	}
	return nil
}

// ensureService connects to a verified service or starts the sibling.
func (u *UI) ensureService() (serviceResponse, error) {
	appExe, _ := os.Executable()
	expected := currentNativeServiceIdentity(appExe, u.client.PipeName)
	spec := serviceSpawnSpecFor(appExe, u.client.PipeName)

	resp, err := u.client.Info()
	if err == nil {
		kind := classifyServiceIdentity(resp, expected)
		decision := decideServiceStartup(serviceStartupInput{
			PipeResponds: true,
			Identity:     kind,
		})
		if decision.Action == serviceStartupConnect {
			return resp, nil
		}
		return resp, errors.New(decision.Message)
	}
	if err != nil && isTimeoutError(err) {
		decision := decideServiceStartup(serviceStartupInput{
			PipeTimeout: true,
		})
		return serviceResponse{}, errors.New(decision.Message)
	}

	_, statErr := os.Stat(spec.Executable)
	decision := decideServiceStartup(serviceStartupInput{
		PipeResponds:  err == nil,
		Identity:      serviceIdentityNone,
		SiblingExists: statErr == nil && appExe != "",
	})
	switch decision.Action {
	case serviceStartupConnect:
		return resp, nil
	case serviceStartupSpawn:
		if spawnErr := startNativeService(spec); spawnErr != nil {
			return serviceResponse{}, fmt.Errorf("start seekfs service: %w", spawnErr)
		}
		return u.waitForService()
	default:
		return serviceResponse{}, errors.New(decision.Message)
	}
}

// waitForService polls info after a spawn until the service answers.
func (u *UI) waitForService() (serviceResponse, error) {
	deadline := time.Now().Add(nativeServiceWaitTimeout)
	var lastErr error
	for {
		resp, err := u.client.Info()
		if err == nil && resp.OK {
			return resp, nil
		}
		if err != nil {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = errors.New("seekfs service did not become ready")
			}
			return serviceResponse{}, fmt.Errorf("seekfs service did not become ready: %w", lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
