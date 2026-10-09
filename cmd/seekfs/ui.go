//go:build seekfs_ui && (production || dev)

package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	wails "github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const uiSearchDeadline = 15 * time.Second
const uiServiceTimeout = 20 * time.Second

//go:embed ui_frontend/*
var seekfsUIAssets embed.FS

type UIApp struct {
	ctx          context.Context
	pipeName     string
	dbs          []string
	defaultLimit int
	ready        bool
	readyMessage string
	searchSeq    atomic.Int64
	uiSeqBase    int64
	serviceStart atomic.Bool
	servicePID   atomic.Int64
	serviceSince atomic.Int64
}

type UISearchRequest struct {
	Query     string `json:"query"`
	MatchPath bool   `json:"match_path"`
	Under     string `json:"under,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Exists    bool   `json:"exists,omitempty"`
}

type UISearchResponse struct {
	OK        bool       `json:"ok"`
	Seq       int64      `json:"seq"`
	Query     string     `json:"query"`
	Count     int        `json:"count"`
	ElapsedMS int64      `json:"elapsed_ms"`
	Results   []UIResult `json:"results"`
	Message   string     `json:"message,omitempty"`
}

type UIResult struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Dir      string `json:"dir"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size,omitempty"`
	Modified string `json:"modified,omitempty"`
	Exists   bool   `json:"exists"`
}

type UIStatus struct {
	OK            bool     `json:"ok"`
	Message       string   `json:"message,omitempty"`
	Entries       int      `json:"entries,omitempty"`
	Loading       bool     `json:"loading,omitempty"`
	DBs           []dbInfo `json:"dbs,omitempty"`
	Health        string   `json:"health,omitempty"`
	HealthMessage string   `json:"health_message,omitempty"`
}

func cmdUI(args []string) error {
	flags := flag.NewFlagSet("ui", flag.ContinueOnError)
	configPath := flags.String("config", "", "optional seekfs.toml config path")
	pipeName := flags.String("pipe", defaultServicePipe, "service named pipe")
	limit := flags.Int("n", 200, "default UI result limit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if *limit <= 0 {
		*limit = 200
	}
	dbs := uiCandidateDBs(cfg)

	assets, err := fs.Sub(seekfsUIAssets, "ui_frontend")
	if err != nil {
		return err
	}
	app := &UIApp{pipeName: *pipeName, dbs: dbs, defaultLimit: *limit, readyMessage: "Starting service...", uiSeqBase: time.Now().UnixNano()}
	app.ensureServiceReady(dbs)
	return wails.Run(&options.App{
		Title:            "seekfs",
		Width:            1180,
		Height:           760,
		MinWidth:         860,
		MinHeight:        540,
		BackgroundColour: &options.RGBA{R: 11, G: 11, B: 14, A: 1},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.startup,
		Bind:             []interface{}{app},
	})
}

var cmdUIRun = cmdUI

func cmdDefault(args []string) error {
	return cmdUIRun(args)
}

func (a *UIApp) startup(ctx context.Context) {
	a.ctx = ctx
	warmUpShellContextMenus()
}

func (a *UIApp) Status() UIStatus {
	resp, err := callServiceWithTimeout(a.pipeName, serviceRequest{Command: "info"}, uiServiceTimeout)
	if err != nil {
		a.ensureServiceReady(a.dbs)
		if a.readyMessage != "" && a.readyMessage != err.Error() {
			return UIStatus{OK: false, Message: a.readyMessage}
		}
		a.ready = false
		a.readyMessage = err.Error()
		return UIStatus{OK: false, Message: err.Error()}
	}
	if resp.Loading {
		a.ready = false
		a.readyMessage = "Loading indexes..."
	} else if resp.OK && resp.Entries > 0 && len(resp.DBs) > 0 {
		a.ready = true
		a.readyMessage = ""
	} else {
		a.ready = false
		a.readyMessage = firstNonEmpty(resp.Message, "service has no search indexes loaded")
	}
	health, healthMessage := serviceHealthFromStatus(a.ready, a.readyMessage, resp)
	return UIStatus{
		OK:            a.ready,
		Message:       firstNonEmpty(a.readyMessage, resp.Message),
		Entries:       resp.Entries,
		Loading:       resp.Loading,
		DBs:           resp.DBs,
		Health:        health,
		HealthMessage: healthMessage,
	}
}

// serviceHealthFromStatus maps the service info response onto the UI health
// dot: green (ok), yellow (degraded/catching up), red (stale or error).  When
// the UI itself could not reach the service the dot is red with the transport
// error.
func serviceHealthFromStatus(ready bool, readyMessage string, resp serviceResponse) (string, string) {
	if !ready {
		if resp.Health == serviceHealthError || resp.Health == serviceHealthDegraded {
			return resp.Health, firstNonEmpty(resp.HealthMessage, resp.Message, readyMessage)
		}
		return "error", firstNonEmpty(resp.Message, readyMessage, "service is not ready")
	}
	switch resp.Health {
	case serviceHealthOK, serviceHealthDegraded, serviceHealthError:
		return resp.Health, resp.HealthMessage
	}
	return serviceHealthOK, ""
}

func (a *UIApp) Search(req UISearchRequest) (UISearchResponse, error) {
	return a.search(req, 0), nil
}

func (a *UIApp) SearchAsync(req UISearchRequest, seq int64) error {
	a.searchSeq.Store(seq)
	go func() {
		resp := a.search(req, seq)
		if a.ctx == nil || seq != a.searchSeq.Load() {
			return
		}
		wruntime.EventsEmit(a.ctx, "seekfs:search-results", resp)
	}()
	return nil
}

func (a *UIApp) search(req UISearchRequest, seq int64) UISearchResponse {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return UISearchResponse{OK: true, Seq: seq, Query: query, Results: []UIResult{}}
	}
	if incompleteUIQuery(query) {
		return UISearchResponse{OK: true, Seq: seq, Query: query, Results: []UIResult{}, Message: "Keep typing"}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = a.defaultLimit
	}
	if !a.ready {
		if status := a.Status(); !status.OK {
			return UISearchResponse{OK: false, Seq: seq, Query: query, Message: firstNonEmpty(status.Message, "service is not ready")}
		}
	}
	start := time.Now()
	resp, err := a.searchServiceUI(query, req, limit, seq)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		a.ready = false
		a.readyMessage = err.Error()
		return UISearchResponse{OK: false, Seq: seq, Query: query, ElapsedMS: elapsed, Message: err.Error()}
	}
	if !resp.OK {
		return UISearchResponse{OK: false, Seq: seq, Query: query, ElapsedMS: elapsed, Message: resp.Message}
	}
	results := uiResultsFromServiceResponse(resp)
	message := ""
	if resp.Fuzzy {
		message = "showing close matches"
	}
	return UISearchResponse{
		OK:        true,
		Seq:       seq,
		Query:     query,
		Count:     len(results),
		ElapsedMS: elapsed,
		Message:   message,
		Results:   results,
	}
}

func (a *UIApp) ensureServiceReady(dbs []string) {
	resp, err := a.pollServiceInfoReady()
	if err == nil && !a.serviceResponseIsFresh(resp) {
		if serviceResponseIsVerifiedStandalone(resp) {
			if stopErr := a.stopStaleStandaloneService(resp.PID); stopErr == nil {
				a.serviceStart.Store(false)
				err = errors.New("stale service stopped")
			} else {
				a.ready = false
				a.readyMessage = "Stale seekfs service is running. Restart seekfs or stop PID " + strconv.Itoa(resp.PID)
				return
			}
		} else {
			a.ready = false
			a.readyMessage = "Stale seekfs service identity is ambiguous. Restart the verified seekfs service or change the UI pipe."
			return
		}
	}
	if err == nil && resp.OK && resp.Entries > 0 && !resp.Loading {
		a.ready = true
		a.readyMessage = ""
		return
	}
	if err == nil && resp.Loading {
		a.ready = false
		a.readyMessage = "Loading indexes..."
		return
	}
	if err == nil && resp.OK && resp.Entries == 0 && len(dbs) > 0 && runtime.GOOS == "windows" {
		if startErr := startWindowsService(); startErr == nil {
			a.readyMessage = "Loading indexes..."
			return
		}
	}
	if len(dbs) == 0 {
		a.ready = false
		a.readyMessage = "No seekfs indexes found. Run: seekfs index-volumes -launch"
		return
	}
	if a.serviceStart.Load() && time.Since(time.Unix(0, a.serviceSince.Load())) < 3*time.Minute {
		a.ready = false
		a.readyMessage = "Loading indexes..."
		return
	}
	if err := a.startStandaloneService(dbs); err == nil {
		a.ready = false
		a.readyMessage = "Loading indexes..."
		return
	}
	if err := startWindowsService(); err == nil {
		a.ready = false
		a.readyMessage = "Loading indexes..."
		return
	}
	a.ready = false
	a.readyMessage = "Service is not ready. Run elevated: seekfs launch " + uiDBArgs(dbs)
}

// pollServiceInfoReady tries the service info handshake several times before
// giving up.  A live service can be momentarily unresponsive (e.g. mid
// persist-swap while it holds the global lock); treating one slow reply as a
// dead service spawns a SECOND service over the same pipe and index files,
// and the two processes then fight over the .gsi files and interleave WAL
// frames.  Timeout errors (server alive but slow) get the full retry budget;
// fast connect failures (no listener) give up after two attempts.
func (a *UIApp) pollServiceInfoReady() (serviceResponse, error) {
	var resp serviceResponse
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		resp, err = callServiceWithTimeout(a.pipeName, serviceRequest{Command: "info"}, 500*time.Millisecond)
		if err == nil {
			return resp, nil
		}
		if !isServiceTimeoutError(err) && attempt >= 1 {
			return resp, err
		}
	}
	return resp, err
}

type serviceIdentityExpectation struct {
	Executable     string
	ExecutableHash string
	Version        string
	Commit         string
	Date           string
	BuildFlavor    string
	PipeName       string
}

func currentUIServiceIdentity(pipeName string) (serviceIdentityExpectation, error) {
	exe, err := os.Executable()
	if err != nil {
		return serviceIdentityExpectation{}, err
	}
	return serviceIdentityExpectation{
		Executable:     exe,
		ExecutableHash: executableContentHash(exe),
		Version:        version,
		Commit:         commit,
		Date:           date,
		BuildFlavor:    serviceBuildFlavorForMemoryMode("lowmem"),
		PipeName:       pipeName,
	}, nil
}

func (a *UIApp) serviceResponseIsFresh(resp serviceResponse) bool {
	expected, err := currentUIServiceIdentity(a.pipeName)
	if err != nil {
		return false
	}
	return serviceIdentityMatchesExpected(resp, expected)
}

func serviceIdentityMatchesExpected(resp serviceResponse, expected serviceIdentityExpectation) bool {
	if resp.PID <= 0 {
		return false
	}
	if resp.Executable == "" || expected.Executable == "" {
		return false
	}
	respExe, err := filepath.Abs(resp.Executable)
	if err == nil {
		resp.Executable = respExe
	}
	expectedExe, err := filepath.Abs(expected.Executable)
	if err == nil {
		expected.Executable = expectedExe
	}
	if !strings.EqualFold(filepath.Clean(resp.Executable), filepath.Clean(expected.Executable)) {
		return false
	}
	if resp.ExecutableHash == "" || expected.ExecutableHash == "" || resp.ExecutableHash != expected.ExecutableHash {
		return false
	}
	if resp.BuildFlavor == "" || expected.BuildFlavor == "" || resp.BuildFlavor != expected.BuildFlavor {
		return false
	}
	if resp.PipeName == "" || expected.PipeName == "" || !strings.EqualFold(resp.PipeName, expected.PipeName) {
		return false
	}
	return serviceBuildIdentityMatches(resp, expected)
}

func serviceBuildIdentityMatches(resp serviceResponse, expected serviceIdentityExpectation) bool {
	if resp.Version == "" || resp.Commit == "" || resp.Date == "" {
		return false
	}
	if expected.Version == "" || expected.Commit == "" || expected.Date == "" {
		return false
	}
	if isUnknownBuildIdentity(resp.Version, resp.Commit, resp.Date) || isUnknownBuildIdentity(expected.Version, expected.Commit, expected.Date) {
		return true
	}
	return resp.Version == expected.Version && resp.Commit == expected.Commit && resp.Date == expected.Date
}

func serviceResponseIsVerifiedStandalone(resp serviceResponse) bool {
	return strings.EqualFold(resp.ProcessMode, "standalone")
}

func isUnknownBuildIdentity(v, c, d string) bool {
	return v == "dev" || c == "unknown" || d == "unknown"
}

func (a *UIApp) stopStaleStandaloneService(pid int) error {
	if pid <= 0 || pid == os.Getpid() {
		return errors.New("invalid stale service pid")
	}
	proc, err := os.FindProcess(pid)
	if err == nil {
		_ = proc.Kill()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := callServiceWithTimeout(a.pipeName, serviceRequest{Command: "ping"}, 100*time.Millisecond); err != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	cmd := exec.Command("taskkill.exe", "/PID", strconv.Itoa(pid), "/F")
	if err := cmd.Run(); err != nil {
		return err
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := callServiceWithTimeout(a.pipeName, serviceRequest{Command: "ping"}, 100*time.Millisecond); err != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("stale service still responds")
}

func (a *UIApp) startStandaloneService(dbs []string) error {
	if a == nil || !a.serviceStart.CompareAndSwap(false, true) {
		return errors.New("service start already attempted")
	}
	exe, err := os.Executable()
	if err != nil {
		a.serviceStart.Store(false)
		return err
	}
	cmd := exec.Command(exe, uiServiceArgs(a.pipeName, dbs)...)
	configureUIServiceEnvironment(cmd)
	prepareUIServiceCommand(cmd)
	if err := cmd.Start(); err != nil {
		a.serviceStart.Store(false)
		return err
	}
	a.servicePID.Store(int64(cmd.Process.Pid))
	a.serviceSince.Store(time.Now().UnixNano())
	return nil
}

func uiCandidateDBs(cfg appConfig) []string {
	if len(cfg.DBs) > 0 {
		return append([]string(nil), cfg.DBs...)
	}
	indexDir := defaultIndexDir()
	dbs := make([]string, 0, len(defaultIndexVolumes()))
	for _, volume := range defaultIndexVolumes() {
		db := defaultVolumeDB(indexDir, normalizeVolume(volume))
		if _, err := os.Stat(db); err == nil {
			dbs = append(dbs, db)
		}
	}
	return dbs
}

func uiDBArgs(dbs []string) string {
	parts := make([]string, 0, len(dbs)*2)
	for _, db := range dbs {
		parts = append(parts, "-db", db)
	}
	return strings.Join(parts, " ")
}

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

func (a *UIApp) searchServiceUI(query string, req UISearchRequest, limit int, seq int64) (serviceResponse, error) {
	serviceQuery, matchPath := normalizeUIQueryForService(query, req.MatchPath)
	opts := queryOptions{
		Query:      serviceQuery,
		MatchPath:  matchPath,
		Under:      strings.TrimSpace(req.Under),
		Limit:      limit,
		Exists:     req.Exists,
		RequestSeq: a.serviceUISeq(seq),
	}
	return a.callSearchServiceWithRetry(opts, seq)
}

func (a *UIApp) callSearchServiceWithRetry(opts queryOptions, seq int64) (serviceResponse, error) {
	if seq > 0 && seq != a.searchSeq.Load() {
		return serviceResponse{OK: false, Message: "query superseded"}, nil
	}
	opts.DeadlineUnix = time.Now().Add(uiSearchDeadline).UnixNano()
	return callServiceWithTimeout(a.pipeName, serviceRequestFromOptions(opts, false), uiServiceTimeout)
}

func (a *UIApp) serviceUISeq(seq int64) int64 {
	if seq <= 0 {
		return 0
	}
	base := a.uiSeqBase
	if base == 0 {
		base = time.Now().UnixNano()
		a.uiSeqBase = base
	}
	return base + seq
}

func isServiceTimeoutError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "request timed out")
}

func uiResultsFromServiceResponse(resp serviceResponse) []UIResult {
	if len(resp.Rows) > 0 {
		out := make([]UIResult, 0, len(resp.Rows))
		for _, row := range resp.Rows {
			out = append(out, uiResultFromJSON(row))
		}
		return out
	}
	out := make([]UIResult, 0, len(resp.Results))
	for _, path := range resp.Results {
		out = append(out, uiResultFromPathOnly(path))
	}
	return out
}

func uiResultFromJSON(row jsonResult) UIResult {
	result := UIResult{
		Path:     row.Path,
		Name:     row.Name,
		Dir:      filepath.Dir(row.Path),
		IsDir:    row.IsDir,
		Modified: row.Modified,
		Exists:   true,
	}
	if result.Name == "" {
		result.Name = filepath.Base(row.Path)
	}
	if row.Size != nil {
		result.Size = *row.Size
	}
	return result
}

func callServiceWithTimeout(pipeName string, req serviceRequest, timeout time.Duration) (serviceResponse, error) {
	handle, err := openPipeClientWithTimeout(pipeName, timeout)
	if err != nil {
		return serviceResponse{}, err
	}
	file := os.NewFile(uintptr(handle), pipeName)
	defer file.Close()
	return exchangeServiceJSON(file, req, timeout)
}

func normalizeUIQueryForService(query string, matchPath bool) (string, bool) {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return query, matchPath
	}
	if !matchPath && shouldUIUseLoosePathMatching(fields) {
		matchPath = true
	}
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		converted, nextMatchPath := normalizeEverythingTokenForService(field, matchPath)
		matchPath = nextMatchPath
		out = append(out, converted...)
	}
	return strings.Join(out, " "), matchPath
}

func shouldUIUseLoosePathMatching(fields []string) bool {
	for _, field := range fields {
		if strings.HasPrefix(field, "!") || strings.HasPrefix(field, "-") {
			continue
		}
		raw := strings.TrimLeft(field, "!-")
		if raw == "" {
			continue
		}
		if isVolumeQueryTerm(raw) {
			continue
		}
		if strings.ContainsAny(raw, `\/`) {
			return true
		}
		key, _, hasPrefix := strings.Cut(raw, ":")
		if hasPrefix {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "path", "fullpath", "full-path", "full_path", "fullpathname", "full-path-name", "location":
				return true
			}
			continue
		}
	}
	return false
}

func incompleteUIQuery(query string) bool {
	fields := strings.Fields(strings.TrimSpace(query))
	if len(fields) == 0 {
		return false
	}
	last := fields[len(fields)-1]
	return last == "." || strings.HasSuffix(last, ":")
}

func normalizeEverythingTokenForService(field string, matchPath bool) ([]string, bool) {
	neg := ""
	raw := field
	if len(raw) > 1 && (raw[0] == '!' || raw[0] == '-') {
		neg = raw[:1]
		raw = raw[1:]
	}
	key, value, ok := strings.Cut(raw, ":")
	if !ok {
		return []string{field}, matchPath
	}
	key = strings.ToLower(strings.TrimSpace(key))
	value = strings.TrimSpace(value)
	if value == "" {
		switch key {
		case "file", "files":
			return []string{neg + "type:file"}, matchPath
		case "folder", "folders", "directory", "directories", "dir":
			return []string{neg + "type:dir"}, matchPath
		}
		return []string{field}, matchPath
	}

	switch key {
	case "path", "fullpath", "full-path", "full_path", "fullpathname", "full-path-name":
		if _, ok := dottedExtensionTerm(strings.ToLower(value)); ok {
			return []string{neg + "path:" + value}, true
		}
		return []string{neg + value}, true
	case "pathpart", "path-part", "path_part", "location":
		return []string{neg + "dir:" + value}, matchPath
	case "ext", "extension":
		return []string{neg + "ext:" + strings.TrimPrefix(value, ".")}, matchPath
	case "file", "files":
		return append([]string{neg + "type:file"}, normalizePrefixedEverythingValue(neg, value)...), matchPath
	case "folder", "folders", "directory", "directories":
		return append([]string{neg + "type:dir"}, normalizePrefixedEverythingValue(neg, value)...), matchPath
	case "name", "basename", "filename", "file-name":
		return []string{neg + value}, matchPath
	case "sz":
		return []string{neg + "size:" + value}, matchPath
	case "date", "date-modified", "datemodified", "modified":
		return []string{neg + "dm:" + value}, matchPath
	case "case-sensitive":
		return []string{"case:" + value}, matchPath
	default:
		return []string{field}, matchPath
	}
}

func normalizePrefixedEverythingValue(neg, value string) []string {
	if key, rest, ok := strings.Cut(value, ":"); ok {
		switch strings.ToLower(key) {
		case "regex", "glob", "ext", "extension", "size", "sz", "dm", "date", "date-modified", "datemodified", "modified", "dir", "pathpart", "path-part", "location":
			converted, _ := normalizeEverythingTokenForService(key+":"+rest, false)
			for i := range converted {
				if neg != "" && !strings.HasPrefix(converted[i], neg) {
					converted[i] = neg + converted[i]
				}
			}
			return converted
		}
	}
	return []string{neg + value}
}

func (a *UIApp) Open(path string) error {
	return shellOpen("", path, "", "")
}

func (a *UIApp) Reveal(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("path required")
	}
	return shellOpen("", "explorer.exe", "/select,\""+path+"\"", "")
}

func (a *UIApp) Properties(path string) error {
	return shellOpen("properties", path, "", "")
}

func (a *UIApp) CopyPaths(paths []string) error {
	if a.ctx == nil {
		return errors.New("UI is not ready")
	}
	clean := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			clean = append(clean, path)
		}
	}
	return wruntime.ClipboardSetText(a.ctx, strings.Join(clean, "\r\n"))
}

func (a *UIApp) Rename(path, newName string) error {
	path = strings.TrimSpace(path)
	newName = strings.TrimSpace(newName)
	if path == "" || newName == "" {
		return errors.New("path and new name required")
	}
	if strings.ContainsAny(newName, `\/:*?"<>|`) {
		return fmt.Errorf("invalid file name %q", newName)
	}
	target := filepath.Join(filepath.Dir(path), newName)
	return os.Rename(path, target)
}

func (a *UIApp) DeleteToRecycleBin(paths []string) error {
	if len(paths) == 0 {
		return errors.New("no paths selected")
	}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if err := recyclePath(path); err != nil {
			return err
		}
	}
	return nil
}

// PrebuildShellContextMenu builds the native Explorer shell context menu for
// the given paths in the background and caches it, so a subsequent
// ShowShellContextMenu call on the same selection shows instantly. Called from
// the frontend on row hover so the right-click is pre-warmed.
func (a *UIApp) PrebuildShellContextMenu(paths []string) {
	prebuildShellContextMenu(paths)
}

// ShowShellContextMenu shows the native Explorer shell context menu for the
// given paths at the given client coordinates, using the shell menu when
// available and falling back to the HTML menu (returned shown=false) otherwise.
func (a *UIApp) ShowShellContextMenu(paths []string, clientX, clientY int) (bool, error) {
	clean := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			clean = append(clean, path)
		}
	}
	if len(clean) == 0 {
		return false, nil
	}
	hwnd := foregroundWindowHandle()
	screenX, screenY := shellMenuScreenPosition(hwnd, clientX, clientY)
	uiDebugLogf("ShowShellContextMenu paths=%d hwnd=%d client=%d,%d screen=%d,%d", len(clean), hwnd, clientX, clientY, screenX, screenY)
	shown, err := showShellContextMenu(hwnd, clean, screenX, screenY)
	uiDebugLogf("ShowShellContextMenu result shown=%v err=%v", shown, err)
	return shown, err
}

// uiDebugLogf appends a line to a temporary debug log so UI integration
// problems can be diagnosed without attaching a console.
func uiDebugLogf(format string, args ...interface{}) {
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "seekfs-ui-debug.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
}

func uiResultFromPathOnly(path string) UIResult {
	return UIResult{
		Path: path,
		Name: filepath.Base(path),
		Dir:  filepath.Dir(path),
	}
}

func recyclePath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	method := "DeleteFile"
	if info.IsDir() {
		method = "DeleteDirectory"
	}
	script := fmt.Sprintf(
		`Add-Type -AssemblyName Microsoft.VisualBasic; [Microsoft.VisualBasic.FileIO.FileSystem]::%s($args[0], 'OnlyErrorDialogs', 'SendToRecycleBin')`,
		method,
	)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("recycle %q: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
