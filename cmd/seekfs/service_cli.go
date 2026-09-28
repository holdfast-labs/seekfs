package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

func cmdService(args []string) error {
	var dbs stringList
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	sddl := fs.String("sddl", defaultServiceSDDL, "pipe security descriptor SDDL")
	lowMemory := fs.Bool("lowmem", false, "run service in low-memory mmap mode")
	remoteAddr := fs.String("remote-addr", "", "loopback address for the Mode L transport (e.g. 127.0.0.1:0); empty disables it (default)")
	fs.Var(&dbs, "db", "index database path to load for service search; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lowMemory {
		_ = os.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *remoteAddr == "" && cfg.RemoteAddr != "" {
		*remoteAddr = cfg.RemoteAddr
	}
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	processMode := "standalone"
	if isService {
		processMode = "windows-service"
	}
	handler := &goSearchService{pipeName: *pipeName, sddl: *sddl, processMode: processMode, stop: make(chan struct{}), dbs: dbs, remoteAddr: *remoteAddr, contentCfg: cfg}
	if isService {
		return svc.Run(serviceName, handler)
	}
	return handler.runStandalone()
}

func cmdInstallService(args []string) error {
	var dbs stringList
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	sddl := fs.String("sddl", defaultServiceSDDL, "pipe security descriptor SDDL")
	fs.Var(&dbs, "db", "index database path to load for service search; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", serviceName)
	}
	serviceArgs := []string{"service", "-pipe", *pipeName, "-sddl", *sddl}
	for _, db := range dbs {
		serviceArgs = append(serviceArgs, "-db", db)
	}
	s, err = m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: "seekfs indexing service",
		StartType:   mgr.StartManual,
	}, serviceArgs...)
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Println("installed service", serviceName)
	return nil
}

func cmdSetupService(args []string) error {
	var dbs stringList
	fs := flag.NewFlagSet("setup-service", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	sddl := fs.String("sddl", defaultServiceSDDL, "pipe security descriptor SDDL")
	noStart := fs.Bool("no-start", false, "install but do not start the service")
	fs.Var(&dbs, "db", "index database path to load for service search; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if err := stopServiceIfExists(); err != nil {
		return err
	}
	if err := deleteServiceIfExists(); err != nil {
		return err
	}
	installArgs := []string{"-pipe", *pipeName, "-sddl", *sddl}
	for _, db := range dbs {
		installArgs = append(installArgs, "-db", db)
	}
	if err := cmdInstallService(installArgs); err != nil {
		return err
	}
	if !*noStart {
		if err := startWindowsService(); err != nil {
			return err
		}
		fmt.Println("started service", serviceName)
	}
	return nil
}

func cmdLaunch(args []string) error {
	var dbs stringList
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	sddl := fs.String("sddl", defaultServiceSDDL, "pipe security descriptor SDDL")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	timeout := fs.Duration("timeout", 3*time.Minute, "maximum time to wait for service health")
	fs.Var(&dbs, "db", "index database path to load for service search; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if len(dbs) == 0 {
		return errors.New("launch requires at least one -db or configured dbs")
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	setupArgs := []string{"-pipe", *pipeName, "-sddl", *sddl}
	for _, db := range dbs {
		setupArgs = append(setupArgs, "-db", db)
	}
	if err := cmdSetupService(setupArgs); err != nil {
		return err
	}
	resp := waitForDoctor(*pipeName, *timeout)
	if *jsonOut {
		if err := writeJSON(os.Stdout, resp); err != nil {
			return err
		}
		if !resp.OK {
			return errors.New(resp.Message)
		}
		return nil
	}
	fmt.Printf("installed: %t\nrunning: %t\npipe_reachable: %t\nentries: %d\nquery_ok: %t\n", resp.Installed, resp.Running, resp.PipeReachable, resp.Entries, resp.QueryOK)
	for _, db := range resp.DBs {
		fresh := db.State == "ready" && db.LastReplayError == ""
		state := db.State
		if state == "ready" && db.LastReplayError != "" {
			state = "replay-error"
		}
		line := fmt.Sprintf("  volume %s: state=%s entries=%d", db.Volume, state, db.Entries)
		if !fresh {
			reason := db.StaleReason
			if reason == "" {
				reason = db.LastReplayError
			}
			if reason == "" {
				reason = "unknown"
			}
			line += " [" + reason + "]"
		}
		fmt.Println(line)
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	return nil
}

func cmdControlService(args []string, action string) error {
	fs := flag.NewFlagSet(action+"-service", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch action {
	case "start":
		return startWindowsService()
	case "stop":
		return stopWindowsService()
	case "restart":
		if err := stopServiceIfExists(); err != nil {
			return err
		}
		return startWindowsService()
	default:
		return errors.New("unknown service action")
	}
}

func cmdUninstallService(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Delete(); err != nil {
		return err
	}
	fmt.Println("uninstalled service", serviceName)
	return nil
}

func startWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Start()
}

func stopWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	status, err := s.Control(svc.Stop)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for status.State != svc.Stopped && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		status, err = s.Query()
		if err != nil {
			return err
		}
	}
	return nil
}

func stopServiceIfExists() error {
	err := stopWindowsService()
	if err == nil || strings.Contains(strings.ToLower(err.Error()), "does not exist") {
		return nil
	}
	return nil
}

func deleteServiceIfExists() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return nil
	}
	defer s.Close()
	return s.Delete()
}

func cmdServiceIndexUSN(args []string) error {
	fs := flag.NewFlagSet("service-index-usn", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	db := fs.String("db", defaultDB(), "index database path")
	volume := fs.String("volume", "C:", "NTFS volume")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	req := serviceRequest{Command: "index-usn", Volume: *volume, DB: *db}
	resp, err := callService(*pipeName, req)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	fmt.Printf("%s %d entries\n", resp.Message, resp.Entries)
	return nil
}

func cmdServiceSimple(args []string, command string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	volume := fs.String("volume", "C:", "NTFS volume")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	resp, err := callService(*pipeName, serviceRequest{Command: command, Volume: *volume})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	if *jsonOut {
		return writeJSON(os.Stdout, resp)
	}
	fmt.Println(resp.Message)
	return nil
}

func cmdServiceInfo(args []string) error {
	fs := flag.NewFlagSet("loaded", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	resp, err := callService(*pipeName, serviceRequest{Command: "info"})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	if *jsonOut {
		return writeJSON(os.Stdout, resp)
	}
	fmt.Printf("entries: %d\n", resp.Entries)
	for _, db := range resp.DBs {
		fmt.Printf("%s entries=%d source=%s volume=%s built_at=%s checkpoint=%d\n", db.Path, db.Entries, db.Source, db.Volume, db.BuiltAt, db.Checkpoint)
	}
	return nil
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	resp := probeDoctor(*pipeName)
	if *jsonOut {
		if err := writeJSON(os.Stdout, resp); err != nil {
			return err
		}
		if !resp.OK {
			return errors.New(resp.Message)
		}
		return nil
	}
	fmt.Printf("installed: %t\nrunning: %t\npipe_reachable: %t\nentries: %d\nquery_ok: %t\n", resp.Installed, resp.Running, resp.PipeReachable, resp.Entries, resp.QueryOK)
	for _, db := range resp.DBs {
		fresh := db.State == "ready" && db.LastReplayError == ""
		state := db.State
		if state == "ready" && db.LastReplayError != "" {
			state = "replay-error"
		}
		line := fmt.Sprintf("  volume %s: state=%s entries=%d", db.Volume, state, db.Entries)
		if !fresh {
			reason := db.StaleReason
			if reason == "" {
				reason = db.LastReplayError
			}
			if reason == "" {
				reason = "unknown"
			}
			line += " [" + reason + "]"
		}
		fmt.Println(line)
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	return nil
}

func cmdDefaults(args []string) error {
	fs := flag.NewFlagSet("defaults", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	type dbDefault struct {
		Volume string `json:"volume"`
		DB     string `json:"db"`
		Exists bool   `json:"exists"`
	}
	indexDir := defaultIndexDir()
	volumes := defaultIndexVolumes()
	dbs := make([]dbDefault, 0, len(volumes))
	for _, volume := range volumes {
		db := defaultVolumeDB(indexDir, normalizeVolume(volume))
		_, err := os.Stat(db)
		dbs = append(dbs, dbDefault{Volume: normalizeVolume(volume), DB: db, Exists: err == nil})
	}
	resp := struct {
		OK          bool        `json:"ok"`
		ConfigPath  string      `json:"config_path"`
		IndexDir    string      `json:"index_dir"`
		ServicePipe string      `json:"service_pipe"`
		Volumes     []string    `json:"volumes"`
		DBs         []dbDefault `json:"dbs"`
	}{OK: true, ConfigPath: defaultConfigPath(), IndexDir: indexDir, ServicePipe: defaultServicePipe, Volumes: volumes, DBs: dbs}
	cfg, _ := loadConfig("")
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	if *jsonOut {
		return writeJSON(os.Stdout, resp)
	}
	fmt.Printf("config: %s\nindex_dir: %s\nservice_pipe: %s\n", resp.ConfigPath, resp.IndexDir, resp.ServicePipe)
	for _, db := range dbs {
		fmt.Printf("%s -> %s exists=%t\n", db.Volume, db.DB, db.Exists)
	}
	return nil
}

func cmdConfig(args []string) error {
	if len(args) == 0 {
		return cmdConfig([]string{"show"})
	}
	switch args[0] {
	case "path":
		fmt.Println(defaultConfigPath())
		return nil
	case "show":
		path := defaultConfigPath()
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Print(string(data))
		return nil
	case "get":
		if len(args) < 2 {
			return errors.New("config get requires a key")
		}
		cfg, err := loadConfig(defaultConfigPath())
		if err != nil {
			return err
		}
		return printConfigKey(cfg, args[1])
	case "set":
		if len(args) < 3 {
			return errors.New(`config set requires a key and value, for example: seekfs config set output_format json`)
		}
		key := args[1]
		value := strings.TrimSpace(strings.Join(args[2:], " "))
		value = strings.TrimSpace(strings.TrimPrefix(value, "="))
		return setConfigKey(defaultConfigPath(), key, value)
	default:
		return errors.New("unknown config command; use path, show, get, or set")
	}
}

func printConfigKey(cfg appConfig, key string) error {
	switch key {
	case "dbs", "db_paths":
		fmt.Println(formatStringArray(cfg.DBs))
	case "volumes":
		fmt.Println(formatStringArray(cfg.Volumes))
	case "service_pipe":
		fmt.Println(cfg.ServicePipe)
	case "default_limit":
		fmt.Println(cfg.DefaultLimit)
	case "output_format":
		fmt.Println(cfg.OutputFormat)
	case "content_scope":
		fmt.Println(cfg.Content.Mode)
	case "content_roots":
		fmt.Println(formatStringArray(cfg.Content.Roots))
	case "content_exclude":
		fmt.Println(formatStringArray(cfg.Content.Exclude))
	case "content_budget_bytes":
		fmt.Println(cfg.Content.BudgetBytes)
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	return nil
}

func setConfigKey(path, key, value string) error {
	allowed := map[string]bool{"dbs": true, "db_paths": true, "volumes": true, "service_pipe": true, "default_limit": true, "output_format": true, "seekfs_dir": true, "content_scope": true, "content_roots": true, "content_exclude": true, "content_budget_bytes": true}
	if !allowed[key] {
		return fmt.Errorf("unknown config key %q", key)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lines := []string{}
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	}
	formatted := formatConfigValue(key, value)
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+" ") || strings.HasPrefix(trimmed, key+"=") {
			lines[i] = key + " = " + formatted
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, key+" = "+formatted)
	}
	out := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	return os.WriteFile(path, []byte(out), 0o644)
}

func formatConfigValue(key, value string) string {
	value = strings.TrimSpace(value)
	if key == "dbs" || key == "db_paths" || key == "volumes" {
		if strings.HasPrefix(value, "[") {
			return formatStringArray(parseTOMLStringArray(value))
		}
		parts := strings.Split(value, ",")
		items := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.Trim(strings.TrimSpace(part), `"'`)
			if part != "" {
				items = append(items, strconvQuote(part))
			}
		}
		return "[" + strings.Join(items, ", ") + "]"
	}
	if key == "default_limit" {
		return value
	}
	return strconvQuote(strings.Trim(value, `"'`))
}

func formatStringArray(values []string) string {
	items := make([]string, len(values))
	for i, value := range values {
		items[i] = strconvQuote(value)
	}
	return "[" + strings.Join(items, ", ") + "]"
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func probeDoctor(pipeName string) doctorResponse {
	resp := doctorResponse{ServiceName: serviceName}
	if status, err := queryWindowsService(); err == nil {
		resp.Installed = true
		resp.Running = status.State == svc.Running
	} else {
		resp.ServiceError = err.Error()
		if installed, running := serviceStatusBySC(serviceName); installed {
			resp.Installed = true
			resp.Running = running
		}
	}
	info, err := callService(pipeName, serviceRequest{Command: "info"})
	if err == nil && info.OK {
		resp.PipeReachable = true
		resp.Entries = info.Entries
		resp.Loading = info.Loading
		resp.DBs = info.DBs
		resp.Runtime = info.Runtime
	}
	if err == nil && info.Loading {
		resp.PipeReachable = true
		resp.Loading = true
		resp.DBs = info.DBs
		resp.Runtime = info.Runtime
		resp.Message = "seekfs service is loading indexes"
	}
	searchResp, searchErr := callService(pipeName, serviceRequestFromOptions(queryOptions{Query: "ext:go", MatchPath: true, Limit: 1}, false))
	if searchErr == nil && searchResp.OK {
		resp.QueryOK = true
	}
	if resp.Running && resp.PipeReachable {
		resp.ServiceError = ""
	}
	resp.OK = resp.PipeReachable && resp.Entries > 0 && resp.QueryOK
	if !resp.OK && resp.Message == "" {
		if resp.PipeReachable && !resp.QueryOK {
			resp.Message = "seekfs service pipe is reachable but search is not healthy"
		} else if !resp.PipeReachable && resp.ServiceError != "" && strings.Contains(strings.ToLower(resp.ServiceError), "access is denied") {
			resp.Message = "seekfs service pipe denied access; run seekfs launch/setup-service from an elevated shell to refresh the service ACL"
		} else {
			resp.Message = "seekfs service is not fully healthy"
		}
	}
	return resp
}

func serviceStatusBySC(name string) (bool, bool) {
	cmd := exec.Command("sc.exe", "query", name)
	out, err := cmd.Output()
	if err != nil {
		return false, false
	}
	text := strings.ToUpper(string(out))
	return true, strings.Contains(text, "RUNNING")
}

func waitForDoctor(pipeName string, timeout time.Duration) doctorResponse {
	deadline := time.Now().Add(timeout)
	var resp doctorResponse
	for {
		resp = probeDoctor(pipeName)
		if resp.OK || time.Now().After(deadline) {
			return resp
		}
	}
}

func queryWindowsService() (svc.Status, error) {
	m, err := mgr.Connect()
	if err != nil {
		return svc.Status{}, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return svc.Status{}, err
	}
	defer s.Close()
	return s.Query()
}

func callService(pipeName string, req serviceRequest) (serviceResponse, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := callServiceOnce(pipeName, req)
		if err == nil {
			return resp, nil
		}
		if !isTransientPipeError(err) || time.Now().After(deadline) {
			if strings.Contains(strings.ToLower(err.Error()), "access is denied") {
				return serviceResponse{}, fmt.Errorf("access denied opening seekfs service pipe %s; run seekfs launch/setup-service from an elevated shell to refresh the service ACL: %w", pipeName, err)
			}
			return serviceResponse{}, err
		}
		time.Sleep(75 * time.Millisecond)
	}
}

func callServiceOnce(pipeName string, req serviceRequest) (serviceResponse, error) {
	handle, err := openPipeClient(pipeName)
	if err != nil {
		return serviceResponse{}, err
	}
	file := os.NewFile(uintptr(handle), pipeName)
	defer file.Close()
	return exchangeServiceJSON(file, req, serviceCallTimeout(req))
}

func isTransientPipeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, windows.ERROR_PIPE_BUSY) ||
		errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) ||
		errors.Is(err, windows.ERROR_BROKEN_PIPE) ||
		errors.Is(err, windows.ERROR_NO_DATA) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "pipe is being closed") ||
		strings.Contains(text, "broken pipe") ||
		strings.Contains(text, "no process is on the other end")
}

func serviceCallTimeout(req serviceRequest) time.Duration {
	switch req.Command {
	case "search", "status", "info":
		return serviceQueryTimeout
	default:
		return 0
	}
}

func exchangeServiceJSON(conn io.ReadWriteCloser, req serviceRequest, timeout time.Duration) (serviceResponse, error) {
	if timeout <= 0 {
		return exchangeServiceJSONBlocking(conn, req)
	}
	type result struct {
		resp serviceResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := exchangeServiceJSONBlocking(conn, req)
		done <- result{resp: resp, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.resp, res.err
	case <-timer.C:
		_ = conn.Close()
		return serviceResponse{}, fmt.Errorf("seekfs service %q request timed out after %s", req.Command, timeout)
	}
}

func exchangeServiceJSONBlocking(conn io.ReadWriteCloser, req serviceRequest) (serviceResponse, error) {
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return serviceResponse{}, err
	}
	var resp serviceResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return serviceResponse{}, err
	}
	return resp, nil
}

func openPipeClient(pipeName string) (windows.Handle, error) {
	return openPipeClientWithTimeout(pipeName, 5*time.Second)
}

func openPipeClientWithTimeout(pipeName string, timeout time.Duration) (windows.Handle, error) {
	ptr, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		return 0, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		handle, err := windows.CreateFile(
			ptr,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err == nil {
			return handle, nil
		}
		if err != windows.ERROR_PIPE_BUSY || time.Now().After(deadline) {
			return 0, err
		}
		sleep := 50 * time.Millisecond
		if remaining := time.Until(deadline); remaining < sleep {
			sleep = remaining
		}
		if sleep <= 0 {
			return 0, err
		}
		time.Sleep(sleep)
	}
}

func (s *goSearchService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	if s.processMode == "" {
		s.processMode = "windows-service"
	}
	applyServiceRuntimeMemoryTuning()
	changes <- svc.Status{State: svc.StartPending}
	done := make(chan struct{})
	go func() {
		s.servePrivileged()
		close(done)
	}()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serviceLog("startup index load panic: %v\n%s", r, string(debug.Stack()))
			}
		}()
		if err := s.loadConfiguredIndexes(); err != nil {
			serviceLog("startup index load error: %v", err)
		}
	}()
	for req := range r {
		switch req.Cmd {
		case svc.Interrogate:
			changes <- req.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			s.signalServiceStop()
			<-done
			return false, 0
		}
	}
	return false, 0
}
