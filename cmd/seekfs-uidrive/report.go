//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type report struct {
	Started       time.Time       `json:"started"`
	Finished      time.Time       `json:"finished"`
	DurationMS    float64         `json:"duration_ms"`
	Scenario      string          `json:"scenario"`
	OutDir        string          `json:"out_dir"`
	Build         buildReport     `json:"build"`
	Fixture       fixtureReport   `json:"fixture"`
	ServicePID    int             `json:"service_pid,omitempty"`
	AppPID        int             `json:"app_pid,omitempty"`
	ServicePipe   string          `json:"service_pipe,omitempty"`
	ServiceMode   string          `json:"service_mode,omitempty"`
	ServiceFlavor string          `json:"service_flavor,omitempty"`
	Startup       *startupReport  `json:"startup,omitempty"`
	Setup         *setupReport    `json:"setup,omitempty"`
	Search        *searchReport   `json:"search,omitempty"`
	Resize        *resizeReport   `json:"resize,omitempty"`
	Settings      *settingsReport `json:"settings,omitempty"`
	Findings      []string        `json:"findings,omitempty"`
}

type buildReport struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

type fixtureReport struct {
	Files           int `json:"files"`
	Dirs            int `json:"dirs"`
	PerEntryDelayMS int `json:"per_entry_delay_ms"`
}

type startupReport struct {
	AppStartToPipeMS   float64 `json:"app_start_to_pipe_ms"`
	PipeToFirstFrameMS float64 `json:"pipe_to_first_frame_ms"`
	PipeToHealthMS     float64 `json:"pipe_to_health_ms"`
	Health             string  `json:"health"`
	HealthMsg          string  `json:"health_msg"`
	Entries            int     `json:"entries"`
	NoVolumes          bool    `json:"no_volumes"`
	Rows               int     `json:"rows"`
	Screenshot         string  `json:"screenshot,omitempty"`
}

type setupReport struct {
	IndexedBefore bool    `json:"indexed_before"`
	SetupMS       float64 `json:"setup_ms"`
	State         string  `json:"state"`
	Entries       int     `json:"entries"`
	Error         string  `json:"error,omitempty"`
}

type searchQueryReport struct {
	Query     string  `json:"query"`
	LatencyMS float64 `json:"latency_ms"`
	Rows      int     `json:"rows"`
	Seq       int64   `json:"seq"`
}

type searchReport struct {
	Queries []searchQueryReport `json:"queries"`
}

type resizeReport struct {
	Steps            int               `json:"steps"`
	SetWindowMaxMS   float64           `json:"setwindow_max_ms"`
	SetWindowP50MS   float64           `json:"setwindow_p50_ms"`
	SetWindowBlocked bool              `json:"setwindow_blocked"`
	FrameP50MS       float64           `json:"frame_p50_ms"`
	FrameP95MS       float64           `json:"frame_p95_ms"`
	FrameMaxMS       float64           `json:"frame_max_ms"`
	FramesDuring     int               `json:"frames_during_sweep"`
	SlowFrames       int               `json:"slow_frames_gt_100ms"`
	SettleMS         float64           `json:"settle_ms"`
	AfterSettleMaxMS float64           `json:"after_settle_max_ms"`
	Slowest          []automationFrame `json:"slowest,omitempty"`
	Error            string            `json:"error,omitempty"`
}

type settingsReport struct {
	OpenToDrivesMS     float64 `json:"open_to_drives_ms"`
	DriveCount         int     `json:"drive_count"`
	OffApplyMS         float64 `json:"off_apply_ms"`
	OffStateMS         float64 `json:"off_state_ms"`
	OnApplyMS          float64 `json:"on_apply_ms"`
	SavingClearedMS    float64 `json:"saving_cleared_ms"`
	IndexingSeenMS     float64 `json:"indexing_seen_ms"`
	ReadyMS            float64 `json:"ready_ms"`
	ReadyEntries       int64   `json:"ready_entries"`
	ServiceReadyMS     float64 `json:"service_ready_ms"`
	PanelRefresh       bool    `json:"panel_refresh_needed"`
	ContentApplyMS     float64 `json:"content_apply_ms"`
	ContentMode        string  `json:"content_mode"`
	OpenScreenshot     string  `json:"open_screenshot,omitempty"`
	IndexingScreenshot string  `json:"indexing_screenshot,omitempty"`
	DoneScreenshot     string  `json:"done_screenshot,omitempty"`
	Error              string  `json:"error,omitempty"`
	FinalDrive         string  `json:"final_drive,omitempty"`
	FinalNotice        string  `json:"final_notice,omitempty"`
	FinalErr           string  `json:"final_settings_err,omitempty"`
	ProgressSamples    int     `json:"progress_samples"`
	ProgressIncreases  int     `json:"progress_increases"`
	ProgressSeen       bool    `json:"progress_seen"`
	PanelLagMS         float64 `json:"panel_lag_ms"`
	ProgressScreenshot string  `json:"progress_screenshot,omitempty"`
}

func newReport(scenario, out string) *report {
	return &report{Started: time.Now(), Scenario: scenario, OutDir: out}
}

func (h *harness) writeReport() error {
	data, err := json.MarshalIndent(h.rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(h.out, "report.json"), data, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(h.out, "summary.md"), []byte(renderSummary(h.rep)), 0o600)
}

func renderSummary(r *report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# seekfs UI automation summary\n\n")
	fmt.Fprintf(&b, "- scenario: `%s`\n- out: `%s`\n- duration: %.1f ms\n", r.Scenario, r.OutDir, r.DurationMS)
	fmt.Fprintf(&b, "- build: `%s` commit `%s` (%s)\n", r.Build.Version, r.Build.Commit, r.Build.Date)
	fmt.Fprintf(&b, "- fixture: %d files, %d dirs, per-entry delay %d ms\n", r.Fixture.Files, r.Fixture.Dirs, r.Fixture.PerEntryDelayMS)
	fmt.Fprintf(&b, "- service: pid %d mode `%s` flavor `%s`\n", r.ServicePID, r.ServiceMode, r.ServiceFlavor)
	fmt.Fprintf(&b, "- app: pid %d\n\n", r.AppPID)

	if r.Startup != nil {
		fmt.Fprintf(&b, "## Startup\n\n")
		fmt.Fprintf(&b, "| metric | value |\n|---|---:|\n")
		fmt.Fprintf(&b, "| app start to automation pipe | %.1f ms |\n", r.Startup.AppStartToPipeMS)
		fmt.Fprintf(&b, "| pipe to first frame | %.1f ms |\n", r.Startup.PipeToFirstFrameMS)
		fmt.Fprintf(&b, "| pipe to health state | %.1f ms |\n", r.Startup.PipeToHealthMS)
		fmt.Fprintf(&b, "| health | %s |\n", mdCell(r.Startup.Health))
		fmt.Fprintf(&b, "| health message | %s |\n", mdCell(r.Startup.HealthMsg))
		fmt.Fprintf(&b, "| entries | %d |\n", r.Startup.Entries)
		fmt.Fprintf(&b, "| rows | %d |\n\n", r.Startup.Rows)
	}
	if r.Setup != nil {
		fmt.Fprintf(&b, "## Fixture drive setup\n\n")
		fmt.Fprintf(&b, "| metric | value |\n|---|---:|\n")
		fmt.Fprintf(&b, "| indexed before | %t |\n", r.Setup.IndexedBefore)
		fmt.Fprintf(&b, "| setup time | %.1f ms |\n", r.Setup.SetupMS)
		fmt.Fprintf(&b, "| state | %s |\n", mdCell(r.Setup.State))
		fmt.Fprintf(&b, "| entries | %d |\n", r.Setup.Entries)
		if r.Setup.Error != "" {
			fmt.Fprintf(&b, "| error | %s |\n", mdCell(r.Setup.Error))
		}
		fmt.Fprintln(&b)
	}
	if r.Search != nil {
		fmt.Fprintf(&b, "## Search\n\n")
		fmt.Fprintf(&b, "| query | action to rows | rows | seq |\n|---|---:|---:|---:|\n")
		for _, q := range r.Search.Queries {
			fmt.Fprintf(&b, "| %s | %.1f ms | %d | %d |\n", mdCell(q.Query), q.LatencyMS, q.Rows, q.Seq)
		}
		vals := make([]float64, 0, len(r.Search.Queries))
		for _, q := range r.Search.Queries {
			vals = append(vals, q.LatencyMS)
		}
		p50, p95, max := stats(vals)
		fmt.Fprintf(&b, "\nlatency p50 %.1f ms, p95 %.1f ms, max %.1f ms\n\n", p50, p95, max)
	}
	if r.Resize != nil {
		fmt.Fprintf(&b, "## Resize\n\n")
		fmt.Fprintf(&b, "| metric | value |\n|---|---:|\n")
		fmt.Fprintf(&b, "| SetWindowPos steps | %d |\n", r.Resize.Steps)
		fmt.Fprintf(&b, "| SetWindowPos p50 | %.2f ms |\n", r.Resize.SetWindowP50MS)
		fmt.Fprintf(&b, "| SetWindowPos max | %.2f ms |\n", r.Resize.SetWindowMaxMS)
		fmt.Fprintf(&b, "| WM_SIZE blocked (call > 100 ms) | %t |\n", r.Resize.SetWindowBlocked)
		fmt.Fprintf(&b, "| frames during sweep | %d |\n", r.Resize.FramesDuring)
		fmt.Fprintf(&b, "| frame p50 | %.1f ms |\n", r.Resize.FrameP50MS)
		fmt.Fprintf(&b, "| frame p95 | %.1f ms |\n", r.Resize.FrameP95MS)
		fmt.Fprintf(&b, "| frame max | %.1f ms |\n", r.Resize.FrameMaxMS)
		fmt.Fprintf(&b, "| frames over 100 ms | %d |\n", r.Resize.SlowFrames)
		fmt.Fprintf(&b, "| time to settle | %.1f ms |\n", r.Resize.SettleMS)
		fmt.Fprintf(&b, "| max frame after settle | %.1f ms |\n\n", r.Resize.AfterSettleMaxMS)
	}
	if r.Settings != nil {
		fmt.Fprintf(&b, "## Settings\n\n")
		fmt.Fprintf(&b, "| metric | value |\n|---|---:|\n")
		fmt.Fprintf(&b, "| open to drives listed | %.1f ms |\n", r.Settings.OpenToDrivesMS)
		fmt.Fprintf(&b, "| drives | %d |\n", r.Settings.DriveCount)
		fmt.Fprintf(&b, "| X: off apply (action to saving cleared) | %.1f ms |\n", r.Settings.OffApplyMS)
		fmt.Fprintf(&b, "| X: off observed | %.1f ms |\n", r.Settings.OffStateMS)
		fmt.Fprintf(&b, "| X: on apply (action to saving cleared) | %.1f ms |\n", r.Settings.OnApplyMS)
		fmt.Fprintf(&b, "| X: indexing observed | %.1f ms |\n", r.Settings.IndexingSeenMS)
		fmt.Fprintf(&b, "| X: service ready | %.1f ms |\n", r.Settings.ServiceReadyMS)
		fmt.Fprintf(&b, "| X: panel ready | %.1f ms |\n", r.Settings.ReadyMS)
		fmt.Fprintf(&b, "| panel needed reopen to refresh | %t |\n", r.Settings.PanelRefresh)
		fmt.Fprintf(&b, "| panel ready after service (lag) | %.1f ms |\n", r.Settings.PanelLagMS)
		fmt.Fprintf(&b, "| progress observed | %t (%d increases in %d samples) |\n", r.Settings.ProgressSeen, r.Settings.ProgressIncreases, r.Settings.ProgressSamples)
		fmt.Fprintf(&b, "| X: ready entries | %d |\n", r.Settings.ReadyEntries)
		fmt.Fprintf(&b, "| content mode %s apply | %.1f ms |\n", mdCell(r.Settings.ContentMode), r.Settings.ContentApplyMS)
		if r.Settings.OpenScreenshot != "" {
			fmt.Fprintf(&b, "| screenshots | `%s`, `%s`, `%s`, `%s` |\n",
				shortPath(r.Settings.OpenScreenshot), shortPath(r.Settings.IndexingScreenshot), shortPath(r.Settings.ProgressScreenshot), shortPath(r.Settings.DoneScreenshot))
		}
		if r.Settings.Error != "" {
			fmt.Fprintf(&b, "| error | %s |\n", mdCell(r.Settings.Error))
		}
		if r.Settings.FinalDrive != "" {
			fmt.Fprintf(&b, "| final drive state | %s |\n", mdCell(r.Settings.FinalDrive))
		}
		if r.Settings.FinalNotice != "" {
			fmt.Fprintf(&b, "| final notice | %s |\n", mdCell(r.Settings.FinalNotice))
		}
		if r.Settings.FinalErr != "" {
			fmt.Fprintf(&b, "| final settings error | %s |\n", mdCell(r.Settings.FinalErr))
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "## Findings\n\n")
	if len(r.Findings) == 0 {
		fmt.Fprintf(&b, "- none\n")
	} else {
		for _, f := range r.Findings {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	return b.String()
}

func shortPath(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}

func mdCell(s string) string {
	if s == "" {
		return "-"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

func stats(values []float64) (p50, p95, max float64) {
	if len(values) == 0 {
		return 0, 0, 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[(len(sorted)-1)*50/100], sorted[(len(sorted)-1)*95/100], sorted[len(sorted)-1]
}

func collectFindings(r *report) []string {
	var out []string
	if r.Startup != nil {
		if r.Startup.Health != "" && r.Startup.Health != "ok" && r.Startup.Health != "empty" {
			out = append(out, fmt.Sprintf("startup health was %q: %s", r.Startup.Health, r.Startup.HealthMsg))
		}
		if r.Startup.Health == "empty" && !strings.Contains(strings.ToLower(r.Startup.HealthMsg), "no drives") {
			out = append(out, fmt.Sprintf("startup empty health message was %q", r.Startup.HealthMsg))
		}
	}
	if r.Resize != nil {
		if r.Resize.SetWindowBlocked {
			out = append(out, fmt.Sprintf("SetWindowPos blocked the caller (max %.1f ms > 100 ms)", r.Resize.SetWindowMaxMS))
		}
		if r.Resize.SlowFrames > 0 {
			out = append(out, fmt.Sprintf("%d frame(s) over 100 ms during the resize sweep", r.Resize.SlowFrames))
		}
		if r.Resize.AfterSettleMaxMS > 100 {
			out = append(out, fmt.Sprintf("a frame after settle still took %.1f ms", r.Resize.AfterSettleMaxMS))
		}
	}
	if r.Search != nil {
		for _, q := range r.Search.Queries {
			if q.Rows == 0 {
				out = append(out, fmt.Sprintf("query %q returned no rows", q.Query))
			}
		}
	}
	if r.Settings != nil && r.Settings.Error != "" {
		out = append(out, "settings scenario: "+r.Settings.Error)
	}
	if r.Settings != nil && r.Settings.PanelRefresh {
		out = append(out, "settings panel did not refresh the drive after indexing; the harness reopened it")
	}
	if r.Settings != nil && r.Settings.Error == "" {
		if !r.Settings.ProgressSeen || r.Settings.ProgressIncreases < 3 {
			out = append(out, fmt.Sprintf("indexing progress was not observed increasing at least 3 times (%d increases in %d samples)", r.Settings.ProgressIncreases, r.Settings.ProgressSamples))
		}
		if r.Settings.ProgressScreenshot == "" {
			out = append(out, "no mid-build progress screenshot was captured")
		}
	}
	if r.Setup != nil && r.Setup.Error != "" {
		out = append(out, "fixture drive setup: "+r.Setup.Error)
	}
	return out
}
