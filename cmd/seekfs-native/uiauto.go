//go:build uiautomation && windows

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/gpu/headless"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"golang.org/x/sys/windows"
)

// uiauto.go: the test-only UI automation endpoint. When
// SEEKFS_NATIVE_AUTOMATION names a pipe, the app serves line-delimited JSON
// commands on it. Every command runs on the Gio event-loop goroutine through
// the job queue below; the pipe goroutines only enqueue and wait.

type automationRequest struct {
	Cmd  string          `json:"cmd"`
	Name string          `json:"name,omitempty"`
	Arg  json.RawMessage `json:"arg,omitempty"`
	Path string          `json:"path,omitempty"`
}

type automationDrive struct {
	Volume   string              `json:"volume"`
	Indexed  bool                `json:"indexed"`
	State    string              `json:"state"`
	Entries  int64               `json:"entries"`
	Error    string              `json:"error"`
	Progress *automationProgress `json:"progress,omitempty"`
}

type automationProgress struct {
	Phase string `json:"phase"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

type automationState struct {
	Width               int               `json:"width"`
	Height              int               `json:"height"`
	SettingsOpen        bool              `json:"settings_open"`
	SettingsTab         string            `json:"settings_tab"`
	SettingsSaving      bool              `json:"settings_saving"`
	SettingsNotice      string            `json:"settings_notice"`
	SettingsErr         string            `json:"settings_err"`
	SettingsServiceMode string            `json:"settings_service_mode"`
	SettingsContentMode string            `json:"settings_content_mode"`
	Drives              []automationDrive `json:"drives"`
	Rows                int               `json:"rows"`
	Query               string            `json:"query"`
	Seq                 int64             `json:"seq"`
	Health              string            `json:"health"`
	HealthMsg           string            `json:"health_msg"`
	Entries             int               `json:"entries"`
	NoVolumes           bool              `json:"no_volumes"`
	SettingsApplyStep   string            `json:"settings_apply_step"`
}

type automationStats struct {
	Count   int     `json:"count"`
	P50     float64 `json:"p50_ms"`
	P95     float64 `json:"p95_ms"`
	Max     float64 `json:"max_ms"`
	Slow100 int     `json:"slow_over_100ms"`
}

type automationFrame struct {
	At string  `json:"at"`
	MS float64 `json:"ms"`
}

type automationActionTiming struct {
	Name    string  `json:"name"`
	At      string  `json:"at"`
	WaitMS  float64 `json:"wait_ms"`
	FrameMS float64 `json:"frame_ms"`
}

type automationMark struct {
	Name  string  `json:"name"`
	At    string  `json:"at"`
	Since float64 `json:"since_start_ms"`
}

type automationMetrics struct {
	Started      string                   `json:"started"`
	Frames       automationStats          `json:"frames"`
	Slowest      []automationFrame        `json:"slowest"`
	FirstFrameAt string                   `json:"first_frame_at,omitempty"`
	LastFrameAt  string                   `json:"last_frame_at,omitempty"`
	Actions      []automationActionTiming `json:"actions"`
	Marks        []automationMark         `json:"marks"`
}

type automationResponse struct {
	OK      bool               `json:"ok"`
	Error   string             `json:"error,omitempty"`
	State   *automationState   `json:"state,omitempty"`
	Metrics *automationMetrics `json:"metrics,omitempty"`
	HWND    uint64             `json:"hwnd,omitempty"`
	Path    string             `json:"path,omitempty"`
}

// automationSink is the UI-goroutine surface the dispatcher uses. The real
// app wraps *UI; tests use a fake.
type automationSink interface {
	AutomationState() automationState
	AutomationAction(name string, arg any) error
	AutomationScreenshot(path string) error
	AutomationHWND() uintptr
}

type automationFrameSample struct {
	at time.Time
	ms float64
}

type automationPendingAction struct {
	id    int64
	name  string
	start time.Time
}

var autoMetricsState = struct {
	mu      sync.Mutex
	start   time.Time
	frames  []automationFrameSample
	actions []automationActionTiming
	marks   []automationMark
	pending []automationPendingAction
	nextID  int64
}{start: time.Now()}

func autoNowString(t time.Time) string { return t.Format(time.RFC3339Nano) }

func autoMetricsReset() {
	autoMetricsState.mu.Lock()
	defer autoMetricsState.mu.Unlock()
	autoMetricsState.start = time.Now()
	autoMetricsState.frames = nil
	autoMetricsState.actions = nil
	autoMetricsState.marks = nil
	autoMetricsState.pending = nil
}

func autoMetricsRecordFrame(at time.Time, d time.Duration) {
	autoMetricsState.mu.Lock()
	defer autoMetricsState.mu.Unlock()
	autoMetricsState.frames = append(autoMetricsState.frames, automationFrameSample{at: at, ms: float64(d) / float64(time.Millisecond)})
	if len(autoMetricsState.frames) > 20000 {
		autoMetricsState.frames = append([]automationFrameSample(nil), autoMetricsState.frames[len(autoMetricsState.frames)-20000:]...)
	}
}

func autoMetricsBeginAction(name string) int64 {
	autoMetricsState.mu.Lock()
	defer autoMetricsState.mu.Unlock()
	autoMetricsState.nextID++
	id := autoMetricsState.nextID
	autoMetricsState.pending = append(autoMetricsState.pending, automationPendingAction{id: id, name: name, start: time.Now()})
	return id
}

func autoMetricsCompleteActions(at time.Time) {
	autoMetricsState.mu.Lock()
	defer autoMetricsState.mu.Unlock()
	for _, p := range autoMetricsState.pending {
		autoMetricsState.actions = append(autoMetricsState.actions, automationActionTiming{
			Name:    p.name,
			At:      autoNowString(p.start),
			WaitMS:  float64(at.Sub(p.start)) / float64(time.Millisecond),
			FrameMS: float64(at.Sub(p.start)) / float64(time.Millisecond),
		})
	}
	autoMetricsState.pending = nil
}

func autoMetricsMark(name string, at time.Time) {
	autoMetricsState.mu.Lock()
	defer autoMetricsState.mu.Unlock()
	autoMetricsState.marks = append(autoMetricsState.marks, automationMark{
		Name:  name,
		At:    autoNowString(at),
		Since: float64(at.Sub(autoMetricsState.start)) / float64(time.Millisecond),
	})
}

func autoMetricsSnapshot() automationMetrics {
	autoMetricsState.mu.Lock()
	defer autoMetricsState.mu.Unlock()
	frames := append([]automationFrameSample(nil), autoMetricsState.frames...)
	out := automationMetrics{
		Started: autoNowString(autoMetricsState.start),
		Actions: append([]automationActionTiming(nil), autoMetricsState.actions...),
		Marks:   append([]automationMark(nil), autoMetricsState.marks...),
	}
	out.Frames.Count = len(frames)
	if len(frames) > 0 {
		out.FirstFrameAt = autoNowString(frames[0].at)
		out.LastFrameAt = autoNowString(frames[len(frames)-1].at)
		values := make([]float64, len(frames))
		for i, f := range frames {
			values[i] = f.ms
		}
		sort.Float64s(values)
		out.Frames.P50 = values[(len(values)-1)*50/100]
		out.Frames.P95 = values[(len(values)-1)*95/100]
		out.Frames.Max = values[len(values)-1]
		for _, v := range values {
			if v > 100 {
				out.Frames.Slow100++
			}
		}
		ordered := append([]automationFrameSample(nil), frames...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].ms > ordered[j].ms })
		if len(ordered) > 10 {
			ordered = ordered[:10]
		}
		for _, f := range ordered {
			out.Slowest = append(out.Slowest, automationFrame{At: autoNowString(f.at), MS: f.ms})
		}
	}
	return out
}

// ---- command dispatch (always on the UI goroutine) ----

func decodeAutomationRequest(raw []byte) (automationRequest, error) {
	var req automationRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, err
	}
	if strings.TrimSpace(req.Cmd) == "" {
		return req, fmt.Errorf("cmd is required")
	}
	return req, nil
}

func automationArgAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

func automationArgString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func automationArgBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// runAutomationCommand decodes and executes one command. It must run on the
// UI goroutine because actions and state touch live UI widgets.
func runAutomationCommand(raw []byte, sink automationSink) []byte {
	req, err := decodeAutomationRequest(raw)
	if err != nil {
		return mustAutomationJSON(automationResponse{OK: false, Error: err.Error()})
	}
	arg := automationArgAny(req.Arg)
	switch req.Cmd {
	case "state":
		st := sink.AutomationState()
		return mustAutomationJSON(automationResponse{OK: true, State: &st})
	case "action":
		name := strings.TrimSpace(req.Name)
		if name == "" {
			return mustAutomationJSON(automationResponse{OK: false, Error: "action name is required"})
		}
		autoMetricsBeginAction(name)
		if err := sink.AutomationAction(name, arg); err != nil {
			return mustAutomationJSON(automationResponse{OK: false, Error: err.Error()})
		}
		return mustAutomationJSON(automationResponse{OK: true})
	case "screenshot":
		path := strings.TrimSpace(req.Path)
		if path == "" {
			return mustAutomationJSON(automationResponse{OK: false, Error: "path is required"})
		}
		if err := sink.AutomationScreenshot(path); err != nil {
			return mustAutomationJSON(automationResponse{OK: false, Error: err.Error()})
		}
		return mustAutomationJSON(automationResponse{OK: true, Path: path})
	case "metrics":
		m := autoMetricsSnapshot()
		return mustAutomationJSON(automationResponse{OK: true, Metrics: &m})
	case "metrics-reset":
		autoMetricsReset()
		return mustAutomationJSON(automationResponse{OK: true})
	case "hwnd":
		return mustAutomationJSON(automationResponse{OK: true, HWND: uint64(sink.AutomationHWND())})
	default:
		return mustAutomationJSON(automationResponse{OK: false, Error: "unknown cmd " + req.Cmd})
	}
}

func mustAutomationJSON(v automationResponse) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"ok":false,"error":"encode response"}`)
	}
	return data
}

// ---- UI sink ----

type uiAutomationSink struct{ u *UI }

func (s uiAutomationSink) AutomationState() automationState {
	u := s.u
	st := automationState{SettingsTab: "drives"}
	u.mu.Lock()
	st.Width = u.settings.WindowW
	st.Height = u.settings.WindowH
	st.Rows = len(u.rows)
	st.Query = u.query
	st.Seq = u.seq
	st.Health = u.health
	st.HealthMsg = u.healthMsg
	st.Entries = u.entries
	st.NoVolumes = u.noVolumes
	st.SettingsApplyStep = u.applyStep
	u.mu.Unlock()
	st.SettingsOpen = u.settingsOpen
	if p := u.gsettings; p != nil {
		st.SettingsTab = automationTabName(p.tab)
		st.SettingsSaving = p.saving
		st.SettingsNotice = p.notice
		st.SettingsErr = p.err
		if p.model != nil {
			st.SettingsServiceMode = p.model.serviceMode
			st.SettingsContentMode = p.model.contentValues("").Mode
			for _, v := range p.model.draft.Volumes {
				d := automationDrive{
					Volume: v.Volume, Indexed: v.Indexed, State: v.State,
					Entries: v.Entries, Error: v.Error,
				}
				if v.Progress != nil {
					d.Progress = &automationProgress{Phase: v.Progress.Phase, Done: v.Progress.Done, Total: v.Progress.Total}
				}
				st.Drives = append(st.Drives, d)
			}
		}
	}
	return st
}

func automationTabName(tab int) string {
	switch tab {
	case settingsTabContent:
		return "content"
	case settingsTabSearch:
		return "search"
	default:
		return "drives"
	}
}

func automationTabIndex(name string) int {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "content", "content-search":
		return settingsTabContent
	case "search":
		return settingsTabSearch
	default:
		return settingsTabDrives
	}
}

func (s uiAutomationSink) AutomationAction(name string, arg any) error {
	u := s.u
	switch name {
	case "open-settings":
		u.openSettingsTab(automationTabIndex(automationArgString(arg)))
	case "close-settings":
		u.closeSettings()
	case "toggle-drive":
		volume := automationArgString(arg)
		p := u.gsettings
		if p == nil || p.model == nil {
			return fmt.Errorf("settings model is not loaded")
		}
		d := p.model.drive(volume)
		if d == nil {
			return fmt.Errorf("unknown drive %q", volume)
		}
		p.model.setDriveIndexed(volume, !d.Indexed)
	case "set-install-service":
		p := u.gsettings
		if p == nil {
			return fmt.Errorf("settings panel is not open")
		}
		p.installService = automationArgBool(arg)
	case "apply-settings":
		p := u.gsettings
		if p == nil {
			return fmt.Errorf("settings panel is not open")
		}
		if p.installService && (p.model == nil || !p.model.serviceInstalled()) {
			return fmt.Errorf("apply-settings refuses the service install: automation never elevates")
		}
		u.applySettings()
	case "set-query":
		text := automationArgString(arg)
		u.searchEd.SetText(text)
		u.typedQuery(text)
		u.commitQuery()
	case "set-content-mode":
		mode := automationArgString(arg)
		p := u.gsettings
		if p == nil || p.model == nil {
			return fmt.Errorf("settings model is not loaded")
		}
		if mode != contentModeAuto && mode != contentModeExplicit && mode != contentModeOff {
			return fmt.Errorf("unknown content mode %q", mode)
		}
		p.model.setContentMode(p.drive, mode)
	default:
		return fmt.Errorf("unknown action %q", name)
	}
	return nil
}

func (s uiAutomationSink) AutomationScreenshot(path string) error {
	return s.u.autoScreenshot(path)
}

func (s uiAutomationSink) AutomationHWND() uintptr {
	s.u.mu.Lock()
	defer s.u.mu.Unlock()
	return s.u.hwnd
}

// ---- frame hook ----

func uiautoFrameBegin(u *UI, e app.FrameEvent) {
	autoFrameMu.Lock()
	autoFrameStart = time.Now()
	autoFrameSize = e.Size
	autoFrameMetric = e.Metric
	autoFrameMu.Unlock()
	if srv := autoServer; srv != nil {
		srv.drain(u)
	}
}

func uiautoFrameEnd(u *UI) {
	autoFrameMu.Lock()
	start := autoFrameStart
	autoFrameMu.Unlock()
	if start.IsZero() {
		return
	}
	now := time.Now()
	autoMetricsRecordFrame(now, now.Sub(start))
	autoMetricsCompleteActions(now)
}

func uiautoMark(name string) {
	autoMetricsMark(name, time.Now())
}

var (
	autoFrameMu     sync.Mutex
	autoFrameStart  time.Time
	autoFrameSize   image.Point
	autoFrameMetric unit.Metric
)

// ---- UI screenshot ----

func (u *UI) autoScreenshot(path string) error {
	autoFrameMu.Lock()
	size := autoFrameSize
	metric := autoFrameMetric
	autoFrameMu.Unlock()
	if size.X <= 0 || size.Y <= 0 {
		size = image.Pt(1280, 800)
	}
	if metric.PxPerDp <= 0 {
		metric = unit.Metric{PxPerDp: float32(dpScale), PxPerSp: float32(dpScale)}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	r := new(input.Router)
	ops := new(op.Ops)
	gtx := layout.Context{
		Ops:         ops,
		Metric:      metric,
		Constraints: layout.Exact(size),
		Source:      r.Source(),
	}
	u.layout(gtx)
	r.Frame(gtx.Ops)
	hw, err := headless.NewWindow(size.X, size.Y)
	if err != nil {
		return err
	}
	defer hw.Release()
	if err := hw.Frame(gtx.Ops); err != nil {
		return err
	}
	img := image.NewRGBA(image.Rectangle{Max: size})
	if err := hw.Screenshot(img); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// ---- named-pipe server ----

type automationJob struct {
	raw  []byte
	resp chan []byte
}

type automationServer struct {
	jobs chan automationJob
	stop chan struct{}
	once sync.Once
}

var autoServer *automationServer

func startUIAutomation(u *UI) error {
	name := strings.TrimSpace(os.Getenv("SEEKFS_NATIVE_AUTOMATION"))
	if name == "" {
		return nil
	}
	srv := &automationServer{jobs: make(chan automationJob, 64), stop: make(chan struct{})}
	autoServer = srv
	autoUI.set(u)
	go srv.serve(name)
	return nil
}

func stopUIAutomation() {
	if autoServer != nil {
		autoServer.once.Do(func() { close(autoServer.stop) })
	}
}

func (s *automationServer) submit(raw []byte) []byte {
	job := automationJob{raw: raw, resp: make(chan []byte, 1)}
	select {
	case s.jobs <- job:
	case <-s.stop:
		return mustAutomationJSON(automationResponse{OK: false, Error: "automation server stopping"})
	}
	select {
	case out := <-job.resp:
		return out
	case <-s.stop:
		return mustAutomationJSON(automationResponse{OK: false, Error: "automation server stopping"})
	}
}

func (s *automationServer) drain(u *UI) {
	sink := uiAutomationSink{u: u}
	for {
		select {
		case job := <-s.jobs:
			job.resp <- runAutomationCommand(job.raw, sink)
		default:
			return
		}
	}
}

func (s *automationServer) serve(name string) {
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		dbgLog("automation pipe name: %v", err)
		return
	}
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		handle, err := windows.CreateNamedPipe(
			ptr,
			windows.PIPE_ACCESS_DUPLEX,
			windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
			windows.PIPE_UNLIMITED_INSTANCES,
			64*1024, 64*1024, 0, nil,
		)
		if err != nil {
			dbgLog("automation create pipe: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if err := windows.ConnectNamedPipe(handle, nil); err != nil && err != windows.ERROR_PIPE_CONNECTED {
			windows.CloseHandle(handle)
			continue
		}
		go s.handleConn(handle)
	}
}

func (s *automationServer) handleConn(handle windows.Handle) {
	file := os.NewFile(uintptr(handle), "seekfs-native-automation")
	if file == nil {
		windows.CloseHandle(handle)
		return
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	writer := bufio.NewWriter(file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) == 0 && err != nil {
			return
		}
		if len(strings.TrimSpace(string(line))) > 0 {
			// Ask the UI goroutine for a frame before waiting so queued work
			// runs even while the app is otherwise idle.
			if u := currentAutomationUI(); u != nil && u.invalidate != nil {
				u.invalidate()
			}
			out := s.submit([]byte(strings.TrimSpace(string(line))))
			if _, werr := writer.Write(append(out, '\n')); werr != nil {
				return
			}
			if ferr := writer.Flush(); ferr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

type atomicUI struct {
	mu sync.Mutex
	u  *UI
}

func (a *atomicUI) set(u *UI) { a.mu.Lock(); a.u = u; a.mu.Unlock() }
func (a *atomicUI) get() *UI  { a.mu.Lock(); defer a.mu.Unlock(); return a.u }

var autoUI atomicUI

func currentAutomationUI() *UI { return autoUI.get() }
