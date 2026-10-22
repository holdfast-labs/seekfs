//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// cmd/seekfs-uidrive builds tagged test binaries, drives the native app over
// its automation pipe, and writes report.json + summary.md. It is not part of
// the release build.

const defaultOutRoot = `E:\bench\gotmp`

func main() {
	out := flag.String("out", "", "output directory (default E:\\bench\\gotmp\\uidrive-<timestamp>)")
	timeout := flag.Duration("timeout", 5*time.Minute, "harness timeout")
	scenario := flag.String("scenario", "all", "all|startup|search|resize|settings|status")
	flag.Parse()

	if *out == "" {
		*out = filepath.Join(defaultOutRoot, "uidrive-"+time.Now().Format("20060102-150405"))
	}
	if err := runHarness(*out, *timeout, *scenario); err != nil {
		fmt.Fprintln(os.Stderr, "seekfs-uidrive:", err)
		os.Exit(1)
	}
}

type harness struct {
	out      string
	bin      string
	repo     string
	ctx      context.Context
	cancel   context.CancelFunc
	deadline time.Time

	svcPipe  string
	autoPipe string
	svcExe   string
	appExe   string
	toml     string
	fixture  string

	svcCmd      *exec.Cmd
	appCmd      *exec.Cmd
	appStart    time.Time
	appPipeAt   time.Time
	rep         *report
	auto        *autoClient
	content     bool
	contentSet  contentExpectedSet
	contentJSON string
}

func runHarness(out string, timeout time.Duration, scenario string) error {
	scenario = strings.ToLower(strings.TrimSpace(scenario))
	switch scenario {
	case "all", "startup", "search", "resize", "settings", "status", "content":
	default:
		return fmt.Errorf("unknown scenario %q", scenario)
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	repo, err := findRepoRoot()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	h := &harness{
		out:      absOut,
		bin:      filepath.Join(absOut, "bin"),
		repo:     repo,
		ctx:      ctx,
		cancel:   cancel,
		deadline: deadline,
		rep:      newReport(scenario, absOut),
		content:  scenario == "all" || scenario == "content",
	}
	h.svcPipe = fmt.Sprintf(`\\.\pipe\seekfs-uidrive-%d-1`, os.Getpid())
	h.autoPipe = fmt.Sprintf(`\\.\pipe\seekfs-uidrive-%d-2`, os.Getpid())
	h.svcExe = filepath.Join(h.bin, "seekfs.exe")
	h.appExe = filepath.Join(h.bin, "seekfs-native.exe")
	h.toml = filepath.Join(absOut, "seekfs.toml")
	h.fixture = filepath.Join(absOut, "fixture.json")

	for _, dir := range []string{h.out, h.bin, filepath.Join(h.out, "shots"), filepath.Join(h.out, "seekfsdir")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	defer h.cleanup()

	if err := h.writeFixtureFile(); err != nil {
		return err
	}
	if err := h.buildBinaries(); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if err := h.startService(); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if scenario == "status" {
		// statusScenario starts the app itself, after it starts a service-side
		// build, so the app comes up mid-build.
		h.rep.Status = h.statusScenario()
	} else {
		if err := h.startApp(); err != nil {
			return fmt.Errorf("app: %w", err)
		}

		runStartup := scenario == "all" || scenario == "startup"
		runSearch := scenario == "all" || scenario == "search"
		runResize := scenario == "all" || scenario == "resize"
		runSettings := scenario == "all" || scenario == "settings"
		runContent := scenario == "all" || scenario == "content"

		h.rep.Startup = h.startupScenario(runStartup)
		if runSearch || runSettings || runContent {
			h.rep.Setup = h.ensureFixtureDrive()
		}
		if runSearch {
			h.rep.Search = h.searchScenario()
		}
		if runResize {
			h.rep.Resize = h.resizeScenario()
		}
		if runSettings {
			h.rep.Settings = h.settingsScenario()
		}
		if runContent {
			h.rep.Content = h.contentScenario()
		}
	}
	h.rep.Finished = time.Now()
	h.rep.DurationMS = float64(h.rep.Finished.Sub(h.rep.Started)) / float64(time.Millisecond)
	h.rep.Findings = collectFindings(h.rep)
	if err := h.writeReport(); err != nil {
		return err
	}
	fmt.Println(absOut)
	return nil
}

func findRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err == nil {
		if root, ok := walkToGoMod(cwd); ok {
			return root, nil
		}
	}
	exe, _ := os.Executable()
	if root, ok := walkToGoMod(filepath.Dir(exe)); ok {
		return root, nil
	}
	return "", errors.New("go.mod not found; run seekfs-uidrive from the repository")
}

func walkToGoMod(dir string) (string, bool) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir || parent == "" {
			return "", false
		}
		dir = parent
	}
}

func (h *harness) writeFixtureFile() error {
	plan, err := generateFixtureTree(filepath.Join(h.out, "fixture"), 20000)
	if err != nil {
		return err
	}
	h.rep.Fixture = fixtureReport{Files: plan.Files, Dirs: plan.Dirs, PerEntryDelayMS: plan.PerEntryDelayMS}
	if h.content {
		set, cerr := generateContentFixture(plan.Root)
		if cerr != nil {
			return cerr
		}
		h.contentSet = set
		h.rep.Fixture.ContentFiles = set.Files
		h.contentJSON = filepath.Join(h.out, "content-expected.json")
		expected, merr := json.MarshalIndent(set, "", "  ")
		if merr != nil {
			return merr
		}
		if werr := os.WriteFile(h.contentJSON, expected, 0o600); werr != nil {
			return werr
		}
	}
	fixture := uiDriveFixture{
		Volumes: []uiDriveFixtureVolume{{
			Volume: "X:", Label: "Fixture", FS: "NTFS",
			TotalBytes: 512 << 30, FreeBytes: 256 << 30,
			Root: plan.Root, PerEntryDelayMS: plan.PerEntryDelayMS,
		}},
	}
	data, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(h.fixture, data, 0o600); err != nil {
		return err
	}
	return os.WriteFile(h.toml, []byte("# seekfs-uidrive fixture config\n"), 0o600)
}

func (h *harness) buildBinaries() error {
	stamp := buildStamp(h.repo)
	common := []string{"build", "-tags", "uiautomation", "-trimpath", "-p", "1"}
	svcLDFlags := fmt.Sprintf("-s -w -X main.version=%s -X main.commit=%s -X main.date=%s", stamp.version, stamp.commit, stamp.date)
	appLDFlags := "-H=windowsgui " + svcLDFlags
	builds := []struct {
		exe     string
		pkg     string
		ldflags string
	}{
		{h.svcExe, "./cmd/seekfs", svcLDFlags},
		{h.appExe, "./cmd/seekfs-native", appLDFlags},
	}
	for _, b := range builds {
		args := append(append([]string{}, common...), "-ldflags", b.ldflags, "-o", b.exe, b.pkg)
		cmd := exec.Command("go", args...)
		cmd.Dir = h.repo
		cmd.Env = buildEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			tail := tailString(string(out), 4000)
			_ = os.WriteFile(filepath.Join(h.out, "build.log"), out, 0o600)
			return fmt.Errorf("%s: %w\n%s", b.pkg, err, tail)
		}
	}
	h.rep.Build = buildReport{Version: stamp.version, Commit: stamp.commit, Date: stamp.date}
	return nil
}

type stampInfo struct{ version, commit, date string }

func buildStamp(repo string) stampInfo {
	commit := "uidrive"
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--short", "HEAD").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			commit = s
		}
	}
	return stampInfo{
		version: "uiauto-" + commit,
		commit:  commit,
		date:    time.Now().UTC().Format(time.RFC3339),
	}
}

func buildEnv() []string {
	env := os.Environ()
	set := func(key, value string) {
		prefix := key + "="
		for i, item := range env {
			if strings.HasPrefix(item, prefix) {
				env[i] = prefix + value
				return
			}
		}
		env = append(env, prefix+value)
	}
	set("GOCACHE", `E:\bench\gocache`)
	set("GOTMPDIR", `E:\bench\gotmp`)
	set("TEMP", `E:\bench\gotmp`)
	set("TMP", `E:\bench\gotmp`)
	set("GOMODCACHE", `E:\bench\gomodcache`)
	set("GOMAXPROCS", "2")
	return env
}

func (h *harness) childEnv(extra map[string]string) []string {
	env := os.Environ()
	set := func(key, value string) {
		prefix := key + "="
		for i, item := range env {
			if strings.HasPrefix(item, prefix) {
				env[i] = prefix + value
				return
			}
		}
		env = append(env, prefix+value)
	}
	set("SEEKFS_DIR", filepath.Join(h.out, "seekfsdir"))
	if h.content {
		set("SEEKFS_CONTENT_SEARCH", "1")
	} else {
		set("SEEKFS_CONTENT_SEARCH", "0")
	}
	set("GOMAXPROCS", "2")
	for k, v := range extra {
		set(k, v)
	}
	return env
}

func (h *harness) startService() error {
	logFile, err := os.Create(filepath.Join(h.out, "service.log"))
	if err != nil {
		return err
	}
	cmd := exec.Command(h.svcExe, "service", "-lowmem", "-pipe", h.svcPipe, "-config", h.toml)
	cmd.Dir = h.bin
	svcEnv := map[string]string{
		"SEEKFS_UIDRIVE_FIXTURE": h.fixture,
		"SEEKFS_MEMORY_MODE":     "lowmem",
		"SEEKFS_STARTUP_WORKERS": "1",
	}
	if h.content {
		svcEnv["SEEKFS_CONTENT_SCOPE"] = "explicit"
		svcEnv["SEEKFS_CONTENT_BUDGET_BYTES"] = "67108864"
		svcEnv["SEEKFS_CONTENT_ROOTS"] = filepath.Join(h.out, "fixture")
		svcEnv["SEEKFS_CONTENT_EXCLUDE"] = strings.Join([]string{
			filepath.Join(h.out, "fixture", "node_modules"),
		}, string(filepath.ListSeparator))
	}
	cmd.Env = h.childEnv(svcEnv)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return err
	}
	h.svcCmd = cmd
	h.rep.ServicePID = cmd.Process.Pid
	deadline := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) || h.expired() {
			return errors.New("service info did not answer")
		}
		resp, err := serviceCall(h.svcPipe, map[string]any{"command": "info"}, 2*time.Second)
		if err == nil {
			var info struct {
				OK      bool   `json:"ok"`
				Pipe    string `json:"pipe_name"`
				Mode    string `json:"process_mode"`
				Flavor  string `json:"build_flavor"`
				Message string `json:"message"`
			}
			if json.Unmarshal(resp, &info) == nil && info.Pipe != "" {
				h.rep.ServicePipe = info.Pipe
				h.rep.ServiceMode = info.Mode
				h.rep.ServiceFlavor = info.Flavor
				return nil
			}
		}
		sleepCtx(h.ctx, 100*time.Millisecond)
	}
}

func (h *harness) startApp() error {
	logFile, err := os.Create(filepath.Join(h.out, "app.log"))
	if err != nil {
		return err
	}
	cmd := exec.Command(h.appExe, "-pipe", h.svcPipe)
	cmd.Dir = h.bin
	cmd.Env = h.childEnv(map[string]string{
		"SEEKFS_NATIVE_AUTOMATION": h.autoPipe,
		"SEEKFS_NATIVE_SETTINGS":   filepath.Join(h.out, "native.json"),
		"SEEKFS_NATIVE_DEBUGLOG":   filepath.Join(h.out, "native-debug.log"),
	})
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	h.appStart = time.Now()
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return err
	}
	h.appCmd = cmd
	h.rep.AppPID = cmd.Process.Pid
	h.auto = &autoClient{pipe: h.autoPipe, timeout: 20 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) || h.expired() {
			return errors.New("automation pipe did not answer")
		}
		if _, err := h.auto.State(); err == nil {
			h.appPipeAt = time.Now()
			return nil
		}
		sleepCtx(h.ctx, 100*time.Millisecond)
	}
}

func (h *harness) cleanup() {
	if h.appCmd != nil && h.appCmd.Process != nil {
		_ = h.appCmd.Process.Kill()
		done := make(chan struct{})
		go func() { _ = h.appCmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	if h.svcCmd != nil && h.svcCmd.Process != nil {
		_ = h.svcCmd.Process.Kill()
		done := make(chan struct{})
		go func() { _ = h.svcCmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
}

func (h *harness) expired() bool { return time.Now().After(h.deadline) }

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func tailString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

// ---- pipe clients ----

func pipeRoundTrip(pipe string, payload []byte, timeout time.Duration) ([]byte, error) {
	ptr, err := windows.UTF16PtrFromString(pipe)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(ptr, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), pipe)
	defer file.Close()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		if _, err := file.Write(append(payload, '\n')); err != nil {
			done <- result{err: err}
			return
		}
		line, err := bufio.NewReader(file).ReadBytes('\n')
		done <- result{data: line, err: err}
	}()
	select {
	case r := <-done:
		return r.data, r.err
	case <-time.After(timeout):
		_ = file.Close()
		return nil, fmt.Errorf("pipe %s timed out after %s", pipe, timeout)
	}
}

func serviceCall(pipe string, req map[string]any, timeout time.Duration) ([]byte, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return pipeRoundTrip(pipe, payload, timeout)
}

type autoClient struct {
	pipe    string
	timeout time.Duration
}

func (c *autoClient) Do(req map[string]any, out any) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	raw, err := pipeRoundTrip(c.pipe, payload, c.timeout)
	if err != nil {
		return err
	}
	var envelope struct {
		OK    bool            `json:"ok"`
		Error string          `json:"error"`
		State json.RawMessage `json:"state"`
		Raw   json.RawMessage `json:"metrics"`
		HWND  uint64          `json:"hwnd"`
		Path  string          `json:"path"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if !envelope.OK {
		return errors.New(envelope.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *autoClient) State() (automationState, error) {
	var resp struct {
		State automationState `json:"state"`
	}
	err := c.Do(map[string]any{"cmd": "state"}, &resp)
	return resp.State, err
}

func (c *autoClient) Action(name string, arg any) error {
	req := map[string]any{"cmd": "action", "name": name}
	if arg != nil {
		req["arg"] = arg
	}
	return c.Do(req, nil)
}

func (c *autoClient) Metrics() (automationMetrics, error) {
	var resp struct {
		Metrics automationMetrics `json:"metrics"`
	}
	err := c.Do(map[string]any{"cmd": "metrics"}, &resp)
	return resp.Metrics, err
}

func (c *autoClient) MetricsReset() error {
	return c.Do(map[string]any{"cmd": "metrics-reset"}, nil)
}

func (c *autoClient) HWND() (uintptr, error) {
	var resp struct {
		HWND uint64 `json:"hwnd"`
	}
	if err := c.Do(map[string]any{"cmd": "hwnd"}, &resp); err != nil {
		return 0, err
	}
	return uintptr(resp.HWND), nil
}

func (c *autoClient) Screenshot(path string) error {
	return c.Do(map[string]any{"cmd": "screenshot", "path": path}, nil)
}
