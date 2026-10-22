//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// scenarios.go: the four measured flows. Every wait is bounded by both its
// own timeout and the harness deadline.

func (h *harness) waitState(timeout time.Duration, pred func(automationState) bool) (automationState, time.Duration, error) {
	start := time.Now()
	deadline := time.Now().Add(timeout)
	for {
		if h.expired() {
			return automationState{}, time.Since(start), errors.New("harness deadline")
		}
		if time.Now().After(deadline) {
			return automationState{}, time.Since(start), errors.New("state wait timed out")
		}
		st, err := h.auto.State()
		if err == nil && pred(st) {
			return st, time.Since(start), nil
		}
		sleepCtx(h.ctx, 20*time.Millisecond)
	}
}

func (h *harness) startupScenario(run bool) *startupReport {
	if !run {
		return nil
	}
	rep := &startupReport{}
	if !h.appStart.IsZero() && !h.appPipeAt.IsZero() {
		rep.AppStartToPipeMS = float64(h.appPipeAt.Sub(h.appStart)) / float64(time.Millisecond)
	}
	st, err := h.auto.State()
	if err != nil {
		rep.HealthMsg = "state: " + err.Error()
		return rep
	}
	rep.Rows = st.Rows
	// Empty metrics mean no frame was laid out yet; keep polling state and
	// metrics until a frame is recorded.
	frameStart := time.Now()
	firstFrameAt := time.Time{}
	for {
		if h.expired() || time.Since(frameStart) > 20*time.Second {
			break
		}
		m, err := h.auto.Metrics()
		if err == nil && m.Frames.Count > 0 {
			if at, perr := time.Parse(time.RFC3339Nano, m.FirstFrameAt); perr == nil {
				firstFrameAt = at
			} else {
				firstFrameAt = time.Now()
			}
			break
		}
		sleepCtx(h.ctx, 10*time.Millisecond)
	}
	if !firstFrameAt.IsZero() && !h.appPipeAt.IsZero() {
		rep.PipeToFirstFrameMS = float64(firstFrameAt.Sub(h.appPipeAt)) / float64(time.Millisecond)
	}
	healthStart := time.Now()
	for {
		st, err = h.auto.State()
		if err == nil && st.Health != "" {
			break
		}
		if h.expired() || time.Since(healthStart) > 20*time.Second {
			break
		}
		sleepCtx(h.ctx, 20*time.Millisecond)
	}
	if !h.appPipeAt.IsZero() {
		rep.PipeToHealthMS = float64(time.Since(h.appPipeAt)) / float64(time.Millisecond)
	}
	rep.Health = st.Health
	rep.HealthMsg = st.HealthMsg
	rep.Entries = st.Entries
	rep.NoVolumes = st.NoVolumes
	rep.Rows = st.Rows
	rep.Screenshot = filepath.Join(h.out, "shots", "startup.png")
	_ = h.auto.Screenshot(rep.Screenshot)
	return rep
}

// ---- fixture drive setup (service-level, outside scenario timings) ----

type serviceVolumeView struct {
	Volume   string               `json:"volume"`
	Indexed  bool                 `json:"indexed"`
	State    string               `json:"state"`
	Entries  int                  `json:"entries"`
	Error    string               `json:"error"`
	Progress *serviceProgressView `json:"progress,omitempty"`
}

type serviceProgressView struct {
	Phase     string `json:"phase"`
	Done      int64  `json:"done"`
	Total     int64  `json:"total"`
	StartedAt string `json:"started_at"`
}

type serviceSettingsView struct {
	CanWrite    bool                `json:"can_write"`
	ServiceMode string              `json:"service_mode"`
	Volumes     []serviceVolumeView `json:"volumes"`
	Content     map[string]any      `json:"content"`
}

func (h *harness) serviceSettings() (serviceSettingsView, error) {
	raw, err := serviceCall(h.svcPipe, map[string]any{"command": "settings-get"}, 5*time.Second)
	if err != nil {
		return serviceSettingsView{}, err
	}
	var resp struct {
		OK       bool                 `json:"ok"`
		Message  string               `json:"message"`
		Settings *serviceSettingsView `json:"settings"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return serviceSettingsView{}, err
	}
	if !resp.OK || resp.Settings == nil {
		return serviceSettingsView{}, fmt.Errorf("settings-get: %s", resp.Message)
	}
	return *resp.Settings, nil
}

func (h *harness) serviceSetDrive(volume string, indexed bool) error {
	payload := map[string]any{
		"command": "settings-set",
		"settings": map[string]any{
			"volumes": []map[string]any{{"volume": volume, "indexed": indexed}},
		},
	}
	raw, err := serviceCall(h.svcPipe, payload, 10*time.Second)
	if err != nil {
		return err
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("settings-set: %s", resp.Message)
	}
	return nil
}

func (h *harness) ensureFixtureDrive() *setupReport {
	rep := &setupReport{}
	start := time.Now()
	settings, err := h.serviceSettings()
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	find := func() *serviceVolumeView {
		for i := range settings.Volumes {
			if strings.EqualFold(settings.Volumes[i].Volume, "X:") {
				return &settings.Volumes[i]
			}
		}
		return nil
	}
	vol := find()
	if vol != nil && vol.Indexed && vol.State == "ready" {
		rep.IndexedBefore = true
		rep.State = vol.State
		rep.Entries = vol.Entries
		return rep
	}
	if vol == nil || !vol.Indexed {
		if err := h.serviceSetDrive("X:", true); err != nil {
			rep.Error = err.Error()
			return rep
		}
	}
	for {
		if h.expired() || time.Since(start) > 3*time.Minute {
			rep.Error = "fixture drive did not become ready"
			return rep
		}
		settings, err = h.serviceSettings()
		if err == nil {
			if vol = find(); vol != nil && vol.State == "ready" {
				rep.State = vol.State
				rep.Entries = vol.Entries
				break
			}
		}
		sleepCtx(h.ctx, 100*time.Millisecond)
	}
	rep.SetupMS = float64(time.Since(start)) / float64(time.Millisecond)
	return rep
}

// ---- search ----

func (h *harness) searchScenario() *searchReport {
	rep := &searchReport{}
	if err := h.auto.MetricsReset(); err != nil {
		rep.Queries = append(rep.Queries, searchQueryReport{Query: "metrics-reset", LatencyMS: 0, Rows: 0})
		return rep
	}
	for _, query := range []string{"needle", "report", "doc"} {
		before, err := h.auto.State()
		if err != nil {
			rep.Queries = append(rep.Queries, searchQueryReport{Query: query})
			continue
		}
		start := time.Now()
		if err := h.auto.Action("set-query", query); err != nil {
			rep.Queries = append(rep.Queries, searchQueryReport{Query: query})
			continue
		}
		st, elapsed, err := h.waitState(30*time.Second, func(st automationState) bool {
			return st.Seq > before.Seq && st.Rows > 0
		})
		if err != nil {
			st, _ = h.auto.State()
		}
		rep.Queries = append(rep.Queries, searchQueryReport{
			Query: query, LatencyMS: float64(time.Since(start)) / float64(time.Millisecond),
			Rows: st.Rows, Seq: st.Seq,
		})
		_ = elapsed
	}
	return rep
}

// ---- resize ----

var (
	user32Proc            = windows.NewLazySystemDLL("user32.dll")
	procSetWindowPos      = user32Proc.NewProc("SetWindowPos")
	procGetClientRectAuto = user32Proc.NewProc("GetClientRect")
)

const (
	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010
	swpNoMove     = 0x0002
)

func setWindowPos(hwnd uintptr, w, h int) (time.Duration, error) {
	start := time.Now()
	r, _, err := procSetWindowPos.Call(hwnd, 0, 0, 0, uintptr(w), uintptr(h), swpNoMove|swpNoZOrder|swpNoActivate)
	d := time.Since(start)
	if r == 0 {
		return d, err
	}
	return d, nil
}

func (h *harness) resizeScenario() *resizeReport {
	rep := &resizeReport{}
	hwnd, err := h.auto.HWND()
	if err != nil || hwnd == 0 {
		rep.Error = "no window handle"
		return rep
	}
	if err := h.auto.MetricsReset(); err != nil {
		rep.Error = err.Error()
		return rep
	}
	sweepStart := time.Now()
	var calls []float64
	steps := 40
	lastStep := sweepStart
	for i := 0; i < steps; i++ {
		w, hgt := 900, 600
		if i%2 == 1 {
			w, hgt = 1600, 1000
		}
		stepStart := time.Now()
		d, callErr := setWindowPos(hwnd, w, hgt)
		if callErr != nil {
			rep.Error = callErr.Error()
			return rep
		}
		calls = append(calls, float64(d)/float64(time.Millisecond))
		lastStep = time.Now()
		if remain := 16*time.Millisecond - time.Since(stepStart); remain > 0 {
			sleepCtx(h.ctx, remain)
		}
	}
	// Wait for a quiet gap: no frame for 300 ms. Frames after the last
	// step are what the settle measurement covers.
	quietStart := time.Now()
	lastFrameAt := time.Time{}
	sawFrameAfterStep := false
	var metrics automationMetrics
	for {
		if h.expired() || time.Since(quietStart) > 5*time.Second {
			break
		}
		m, err := h.auto.Metrics()
		if err == nil {
			metrics = m
			if at, perr := time.Parse(time.RFC3339Nano, m.LastFrameAt); perr == nil {
				lastFrameAt = at
				if at.After(lastStep) {
					sawFrameAfterStep = true
				}
			}
		}
		if sawFrameAfterStep && time.Since(lastFrameAt) >= 300*time.Millisecond {
			break
		}
		if !sawFrameAfterStep && time.Since(lastStep) >= 300*time.Millisecond {
			break
		}
		sleepCtx(h.ctx, 20*time.Millisecond)
	}
	if sawFrameAfterStep {
		rep.SettleMS = float64(lastFrameAt.Sub(lastStep)) / float64(time.Millisecond)
		if rep.SettleMS < 0 {
			rep.SettleMS = 0
		}
	}
	time.Sleep(50 * time.Millisecond)
	if m, err := h.auto.Metrics(); err == nil {
		metrics = m
	}
	rep.Steps = steps
	rep.SetWindowP50MS, _, rep.SetWindowMaxMS = stats(calls)
	rep.SetWindowBlocked = rep.SetWindowMaxMS > 100
	rep.FrameP50MS = metrics.Frames.P50
	rep.FrameP95MS = metrics.Frames.P95
	rep.FrameMaxMS = metrics.Frames.Max
	rep.FramesDuring = metrics.Frames.Count
	rep.SlowFrames = metrics.Frames.Slow100
	rep.Slowest = metrics.Slowest
	if err := h.auto.MetricsReset(); err == nil {
		sleepCtx(h.ctx, 500*time.Millisecond)
		if after, err := h.auto.Metrics(); err == nil {
			rep.AfterSettleMaxMS = after.Frames.Max
		}
	}
	return rep
}

// ---- settings ----

func (h *harness) settingsScenario() *settingsReport {
	rep := &settingsReport{}
	if err := h.auto.MetricsReset(); err != nil {
		rep.Error = err.Error()
		return rep
	}
	start := time.Now()
	if err := h.auto.Action("open-settings", "drives"); err != nil {
		rep.Error = err.Error()
		return rep
	}
	st, _, err := h.waitState(20*time.Second, func(st automationState) bool {
		return st.SettingsOpen && len(st.Drives) > 0
	})
	if err != nil {
		rep.Error = "drives did not list: " + err.Error()
		return rep
	}
	rep.OpenToDrivesMS = float64(time.Since(start)) / float64(time.Millisecond)
	rep.DriveCount = len(st.Drives)
	rep.OpenScreenshot = filepath.Join(h.out, "shots", "settings-open.png")
	_ = h.auto.Screenshot(rep.OpenScreenshot)
	// The panel offers to install the Windows service when no drive is\n	// indexed. Never let the harness take that path: it elevates.\n	_ = h.auto.Action("set-install-service", false)

	drive := findDrive(st, "X:")
	if drive == nil {
		rep.Error = "fixture drive X: missing"
		return rep
	}
	if drive.Indexed {
		offStart := time.Now()
		if err := h.auto.Action("toggle-drive", "X:"); err != nil {
			rep.Error = err.Error()
			return rep
		}
		if err := h.auto.Action("apply-settings", nil); err != nil {
			rep.Error = err.Error()
			return rep
		}
		_, _, err := h.waitState(30*time.Second, func(st automationState) bool {
			d := findDrive(st, "X:")
			return d != nil && !d.Indexed && !st.SettingsSaving
		})
		if err != nil {
			rep.Error = "drive did not turn off: " + err.Error()
			return rep
		}
		rep.OffApplyMS = float64(time.Since(offStart)) / float64(time.Millisecond)
		rep.OffStateMS = rep.OffApplyMS
	}

	_ = h.auto.Action("set-install-service", false)
	onStart := time.Now()

	if err := h.auto.Action("toggle-drive", "X:"); err != nil {
		rep.Error = err.Error()
		return rep
	}
	if err := h.auto.Action("apply-settings", nil); err != nil {
		rep.Error = err.Error()
		return rep
	}
	applyDone := false
	sawSaving := false
	indexSeen := false
	shotIndexing := false
	serviceReady := false
	nextServicePoll := time.Now()
	lastProgressDone := int64(-1)
	shotProgress := false
	progressSeen := false
	for {
		if h.expired() || time.Since(onStart) > 120*time.Second {
			if st, serr := h.auto.State(); serr == nil {
				if d := findDrive(st, "X:"); d != nil {
					rep.FinalDrive = d.State
				}
				rep.FinalNotice = st.SettingsNotice
				rep.FinalErr = st.SettingsErr
			}
			if rep.Error == "" {
				rep.Error = "drive did not reach ready"
			}
			rep.ProgressSeen = progressSeen
			break
		}
		st, err := h.auto.State()
		if err != nil {
			sleepCtx(h.ctx, 50*time.Millisecond)
			continue
		}
		d := findDrive(st, "X:")
		if !serviceReady && time.Now().After(nextServicePoll) {
			nextServicePoll = time.Now().Add(500 * time.Millisecond)
			if svc, serr := h.serviceSettings(); serr == nil {
				for i := range svc.Volumes {
					if strings.EqualFold(svc.Volumes[i].Volume, "X:") && svc.Volumes[i].State == "ready" {
						serviceReady = true
						rep.ServiceReadyMS = float64(time.Since(onStart)) / float64(time.Millisecond)
						if rep.ReadyEntries == 0 {
							rep.ReadyEntries = int64(svc.Volumes[i].Entries)
						}
					}
				}
			}
		}
		if st.SettingsSaving {
			sawSaving = true
		}
		if sawSaving && !st.SettingsSaving && !applyDone {
			applyDone = true
			rep.OnApplyMS = float64(time.Since(onStart)) / float64(time.Millisecond)
		}
		if d != nil {
			switch d.State {
			case "indexing":
				if !indexSeen {
					indexSeen = true
					rep.IndexingSeenMS = float64(time.Since(onStart)) / float64(time.Millisecond)
					if !applyDone {
						applyDone = true
						rep.OnApplyMS = rep.IndexingSeenMS
					}
				}
				if !shotIndexing {
					shotIndexing = true
					rep.IndexingScreenshot = filepath.Join(h.out, "shots", "settings-indexing.png")
					_ = h.auto.Screenshot(rep.IndexingScreenshot)
				}
				if d.Progress != nil {
					progressSeen = true
					rep.ProgressSamples++
					if lastProgressDone >= 0 && d.Progress.Done > lastProgressDone {
						rep.ProgressIncreases++
					}
					lastProgressDone = d.Progress.Done
					if !shotProgress && d.Progress.Done > 0 &&
						(d.Progress.Total <= 0 || d.Progress.Done*4 >= d.Progress.Total) {
						shotProgress = true
						rep.ProgressScreenshot = filepath.Join(h.out, "shots", "settings-progress.png")
						_ = h.auto.Screenshot(rep.ProgressScreenshot)
					}
				}
			case "ready":
				rep.ReadyMS = float64(time.Since(onStart)) / float64(time.Millisecond)
				if serviceReady {
					rep.PanelLagMS = rep.ReadyMS - rep.ServiceReadyMS
					if rep.PanelLagMS < 0 {
						rep.PanelLagMS = 0
					}
				}
				if d.Entries > 0 {
					rep.ReadyEntries = d.Entries
				}
				rep.DoneScreenshot = filepath.Join(h.out, "shots", "settings-done.png")
				_ = h.auto.Screenshot(rep.DoneScreenshot)
				rep.ProgressSeen = progressSeen
				goto content
			}
		}
		sleepCtx(h.ctx, 50*time.Millisecond)
	}
content:
	if err := h.auto.Action("open-settings", "content"); err != nil {
		if rep.Error == "" {
			rep.Error = err.Error()
		}
		return rep
	}
	st, _, err = h.waitState(10*time.Second, func(st automationState) bool {
		return st.SettingsTab == "content" && st.SettingsOpen
	})
	if err != nil && rep.Error == "" {
		rep.Error = "content tab: " + err.Error()
	}
	target := "explicit"
	if strings.EqualFold(st.SettingsContentMode, "explicit") {
		target = "auto"
	}
	rep.ContentMode = target
	_ = h.auto.Action("set-install-service", false)
	contentStart := time.Now()
	if err := h.auto.Action("set-content-mode", target); err != nil {
		if rep.Error == "" {
			rep.Error = err.Error()
		}
		return rep
	}
	if err := h.auto.Action("apply-settings", nil); err != nil {
		if rep.Error == "" {
			rep.Error = err.Error()
		}
		return rep
	}
	_, _, err = h.waitState(30*time.Second, func(st automationState) bool {
		return !st.SettingsSaving && st.SettingsErr == "" && st.SettingsNotice != "" && !strings.Contains(st.SettingsNotice, "Applying")
	})
	if err != nil && rep.Error == "" {
		rep.Error = "content apply: " + err.Error()
	}
	rep.ContentApplyMS = float64(time.Since(contentStart)) / float64(time.Millisecond)
	return rep
}

func findDrive(st automationState, volume string) *automationDrive {
	for i := range st.Drives {
		if strings.EqualFold(st.Drives[i].Volume, volume) {
			d := st.Drives[i]
			return &d
		}
	}
	return nil
}

func findServiceVolume(settings serviceSettingsView, volume string) *serviceVolumeView {
	for i := range settings.Volumes {
		if strings.EqualFold(settings.Volumes[i].Volume, volume) {
			v := settings.Volumes[i]
			return &v
		}
	}
	return nil
}

func millis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// statusScenario starts a service-side build and then starts the app while the
// build runs. It requires the app to reach ready health and list the volume in
// the sidebar without a restart.
func (h *harness) statusScenario() *statusReport {
	rep := &statusReport{}
	if err := h.serviceSetDrive("X:", false); err != nil {
		rep.Error = "drive off: " + err.Error()
		return rep
	}
	offDeadline := time.Now().Add(30 * time.Second)
	for {
		if h.expired() || time.Now().After(offDeadline) {
			rep.Error = "fixture drive did not turn off"
			return rep
		}
		settings, err := h.serviceSettings()
		if err == nil {
			if v := findServiceVolume(settings, "X:"); v == nil || !v.Indexed || v.State == "not_indexed" {
				break
			}
		}
		sleepCtx(h.ctx, 100*time.Millisecond)
	}

	buildStart := time.Now()
	if err := h.serviceSetDrive("X:", true); err != nil {
		rep.Error = "drive on: " + err.Error()
		return rep
	}
	if err := h.startApp(); err != nil {
		rep.Error = "app: " + err.Error()
		return rep
	}

	var first bool
	var serviceReadyAt time.Time
	var sidebarAt time.Time
	nextServicePoll := time.Now()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if h.expired() || time.Now().After(deadline) {
			rep.Error = "status did not reach ready"
			return rep
		}
		st, err := h.auto.State()
		if err == nil {
			if !first {
				first = true
				rep.InitialHealth = st.Health
				rep.InitialHealthMsg = st.HealthMsg
				rep.LoadingScreenshot = filepath.Join(h.out, "shots", "status-loading.png")
				_ = h.auto.Screenshot(rep.LoadingScreenshot)
			}
			if st.Health != "ok" {
				rep.SawNotReady = true
			}
			if sidebarAt.IsZero() && sidebarListsVolume(st, "X:") {
				sidebarAt = time.Now()
			}
			if serviceReadyAt.IsZero() && time.Now().After(nextServicePoll) {
				nextServicePoll = time.Now().Add(250 * time.Millisecond)
				if svc, serr := h.serviceSettings(); serr == nil {
					if v := findServiceVolume(svc, "X:"); v != nil && v.State == "ready" {
						serviceReadyAt = time.Now()
					}
				}
			}
			if st.Health == "ok" && sidebarListsVolume(st, "X:") && st.Entries > 0 {
				readyAt := time.Now()
				rep.SidebarX = true
				rep.Entries = st.Entries
				rep.BuildStartToReadyMS = millis(readyAt.Sub(buildStart))
				rep.AppStartToReadyMS = millis(readyAt.Sub(h.appStart))
				if !sidebarAt.IsZero() {
					rep.SidebarXMS = millis(sidebarAt.Sub(buildStart))
				}
				if !serviceReadyAt.IsZero() {
					rep.ServiceReadyMS = millis(serviceReadyAt.Sub(buildStart))
					rep.StatusLagMS = millis(readyAt.Sub(serviceReadyAt))
				}
				rep.ReadyScreenshot = filepath.Join(h.out, "shots", "status-ready.png")
				_ = h.auto.Screenshot(rep.ReadyScreenshot)
				return rep
			}
		}
		sleepCtx(h.ctx, 50*time.Millisecond)
	}
}

func sidebarListsVolume(st automationState, volume string) bool {
	for _, row := range st.Sidebar {
		if row.Kind == "scope" && strings.Contains(row.Label, volume) {
			return true
		}
	}
	return false
}
