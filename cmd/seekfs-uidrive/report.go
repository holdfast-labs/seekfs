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
	SlowDrag      *slowDragReport `json:"slow_drag,omitempty"`
	Settings      *settingsReport `json:"settings,omitempty"`
	Status        *statusReport   `json:"status,omitempty"`
	Content       *contentReport  `json:"content,omitempty"`
	Findings      []string        `json:"findings,omitempty"`
}

type buildReport struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

type fixtureReport struct {
	Files           int `json:"files"`
	ContentFiles    int `json:"content_files,omitempty"`
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

type slowDragReport struct {
	Steps         int     `json:"steps"`
	FrameP50MS    float64 `json:"frame_p50_ms"`
	FrameP95MS    float64 `json:"frame_p95_ms"`
	FrameMaxMS    float64 `json:"frame_max_ms"`
	FramesOver100 int     `json:"frames_over_100ms"`
	StepMaxMS     float64 `json:"step_max_ms"`
	Error         string  `json:"error,omitempty"`
}

type statusReport struct {
	InitialHealth       string  `json:"initial_health"`
	InitialHealthMsg    string  `json:"initial_health_msg"`
	SawNotReady         bool    `json:"saw_not_ready"`
	AppStartToReadyMS   float64 `json:"app_start_to_ready_ms"`
	BuildStartToReadyMS float64 `json:"build_start_to_ready_ms"`
	ServiceReadyMS      float64 `json:"service_ready_ms"`
	StatusLagMS         float64 `json:"status_lag_ms"`
	Entries             int     `json:"entries"`
	SidebarX            bool    `json:"sidebar_lists_x"`
	SidebarXMS          float64 `json:"sidebar_x_ms"`
	LoadingScreenshot   string  `json:"loading_screenshot,omitempty"`
	ReadyScreenshot     string  `json:"ready_screenshot,omitempty"`
	Error               string  `json:"error,omitempty"`
}

type contentQueryReport struct {
	ID             string   `json:"id"`
	Query          string   `json:"query"`
	Expected       []string `json:"expected"`
	Actual         []string `json:"actual,omitempty"`
	Missing        []string `json:"missing,omitempty"`
	Extra          []string `json:"extra,omitempty"`
	Pass           bool     `json:"pass"`
	SnippetPass    bool     `json:"snippet_pass"`
	SnippetText    string   `json:"snippet_text,omitempty"`
	TypedLatencyMS float64  `json:"typed_latency_ms,omitempty"`
	NowLatencyMS   float64  `json:"search_now_latency_ms,omitempty"`
	Rows           int      `json:"rows"`
	ServicePass    *bool    `json:"service_pass,omitempty"`
	ServiceActual  []string `json:"service_actual,omitempty"`
	Attribution    string   `json:"attribution,omitempty"`
	Notice         string   `json:"notice,omitempty"`
	SearchMessage  string   `json:"search_message,omitempty"`
}

type contentNegativeReport struct {
	ID          string   `json:"id"`
	Query       string   `json:"query"`
	Pass        bool     `json:"pass"`
	Rows        int      `json:"rows"`
	Actual      []string `json:"actual,omitempty"`
	ServicePass *bool    `json:"service_pass,omitempty"`
	Attribution string   `json:"attribution,omitempty"`
}

type contentFreshnessReport struct {
	Path        string  `json:"path"`
	QueryBefore string  `json:"query_before"`
	QueryAfter  string  `json:"query_after"`
	BeforePass  bool    `json:"before_pass"`
	AfterPass   bool    `json:"after_pass"`
	ReflectMS   float64 `json:"reflect_ms"`
	OldRows     int     `json:"old_rows"`
	NewRows     int     `json:"new_rows"`
	Pass        bool    `json:"pass"`
	Attribution string  `json:"attribution,omitempty"`
}

type contentLatencyReport struct {
	Typed     []float64 `json:"typed_ms,omitempty"`
	SearchNow []float64 `json:"search_now_ms,omitempty"`
}

type contentReport struct {
	ContentState        string                  `json:"content_state,omitempty"`
	ContentDocs         int                     `json:"content_docs,omitempty"`
	ContentBuildError   string                  `json:"content_build_error,omitempty"`
	ContentBuildDone    int                     `json:"content_build_done,omitempty"`
	ContentBuildTotal   int                     `json:"content_build_total,omitempty"`
	ContentPartial      bool                    `json:"content_partial,omitempty"`
	ContentIncomplete   bool                    `json:"content_incomplete,omitempty"`
	ContentStale        bool                    `json:"content_stale,omitempty"`
	ContentReconcile    bool                    `json:"content_scope_reconcile,omitempty"`
	ContentReconcileN   int                     `json:"content_scope_reconcile_pending,omitempty"`
	ContentExtractorRef bool                    `json:"content_extractor_refresh,omitempty"`
	ContentSidecarCap   bool                    `json:"content_sidecar_capped,omitempty"`
	ContentReadyMS      float64                 `json:"content_ready_ms"`
	BuildingSeen        bool                    `json:"building_seen"`
	BuildingNoticeSeen  bool                    `json:"building_notice_seen"`
	BuildingRows        int                     `json:"building_rows"`
	BuildingNotice      string                  `json:"building_notice,omitempty"`
	StatusText          string                  `json:"status_text,omitempty"`
	BuildingScreenshot  string                  `json:"building_screenshot,omitempty"`
	StatusScreenshot    string                  `json:"status_screenshot,omitempty"`
	SnippetsScreenshot  string                  `json:"snippets_screenshot,omitempty"`
	NoResultsScreenshot string                  `json:"no_results_screenshot,omitempty"`
	Queries             []contentQueryReport    `json:"queries"`
	Negatives           []contentNegativeReport `json:"negatives"`
	Freshness           *contentFreshnessReport `json:"freshness,omitempty"`
	Latency             contentLatencyReport    `json:"latency"`
	Error               string                  `json:"error,omitempty"`
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
	if r.SlowDrag != nil {
		d := r.SlowDrag
		fmt.Fprintf(&b, "## Slow drag\n\n")
		fmt.Fprintf(&b, "| metric | value |\n|---|---:|\n")
		fmt.Fprintf(&b, "| steps | %d |\n", d.Steps)
		fmt.Fprintf(&b, "| frame p50 | %.1f ms |\n", d.FrameP50MS)
		fmt.Fprintf(&b, "| frame p95 | %.1f ms |\n", d.FrameP95MS)
		fmt.Fprintf(&b, "| frame max | %.1f ms |\n", d.FrameMaxMS)
		fmt.Fprintf(&b, "| frames over 100 ms | %d |\n", d.FramesOver100)
		fmt.Fprintf(&b, "| SetWindowPos max | %.1f ms |\n", d.StepMaxMS)
		if d.Error != "" {
			fmt.Fprintf(&b, "| error | %s |\n", mdCell(d.Error))
		}
		fmt.Fprintln(&b)
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
	if r.Status != nil {
		fmt.Fprintf(&b, "## Status freshness\n\n")
		fmt.Fprintf(&b, "| metric | value |\n|---|---:|\n")
		fmt.Fprintf(&b, "| initial health | %s |\n", mdCell(r.Status.InitialHealth))
		fmt.Fprintf(&b, "| initial health message | %s |\n", mdCell(r.Status.InitialHealthMsg))
		fmt.Fprintf(&b, "| saw not-ready status | %t |\n", r.Status.SawNotReady)
		fmt.Fprintf(&b, "| app start to ready (no restart) | %.1f ms |\n", r.Status.AppStartToReadyMS)
		fmt.Fprintf(&b, "| build start to ready | %.1f ms |\n", r.Status.BuildStartToReadyMS)
		fmt.Fprintf(&b, "| service ready | %.1f ms |\n", r.Status.ServiceReadyMS)
		fmt.Fprintf(&b, "| status lag after service ready | %.1f ms |\n", r.Status.StatusLagMS)
		fmt.Fprintf(&b, "| entries | %d |\n", r.Status.Entries)
		fmt.Fprintf(&b, "| sidebar lists X: | %t |\n", r.Status.SidebarX)
		fmt.Fprintf(&b, "| sidebar listed X: at | %.1f ms |\n", r.Status.SidebarXMS)
		if r.Status.LoadingScreenshot != "" {
			fmt.Fprintf(&b, "| screenshots | `%s`, `%s` |\n", shortPath(r.Status.LoadingScreenshot), shortPath(r.Status.ReadyScreenshot))
		}
		if r.Status.Error != "" {
			fmt.Fprintf(&b, "| error | %s |\n", mdCell(r.Status.Error))
		}
		fmt.Fprintln(&b)
	}
	if r.Content != nil {
		c := r.Content
		fmt.Fprintf(&b, "## Content search\n\n")
		fmt.Fprintf(&b, "| metric | value |\n|---|---:|\n")
		fmt.Fprintf(&b, "| content state | %s |\n", mdCell(c.ContentState))
		fmt.Fprintf(&b, "| content docs | %d |\n", c.ContentDocs)
		fmt.Fprintf(&b, "| content flags partial/incomplete/stale/reconcile/extractor/cap | %t / %t / %t / %t (%d) / %t / %t |\n",
			c.ContentPartial, c.ContentIncomplete, c.ContentStale, c.ContentReconcile, c.ContentReconcileN, c.ContentExtractorRef, c.ContentSidecarCap)
		if c.ContentBuildError != "" {
			fmt.Fprintf(&b, "| content build error | %s |\n", mdCell(c.ContentBuildError))
		}
		fmt.Fprintf(&b, "| content ready | %.1f ms |\n", c.ContentReadyMS)
		fmt.Fprintf(&b, "| building state seen | %t |\n", c.BuildingSeen)
		fmt.Fprintf(&b, "| building notice shown | %t |\n", c.BuildingNoticeSeen)
		fmt.Fprintf(&b, "| rows while building | %d |\n", c.BuildingRows)
		fmt.Fprintf(&b, "| status bar while building | %s |\n", mdCell(c.StatusText))
		if c.Error != "" {
			fmt.Fprintf(&b, "| error | %s |\n", mdCell(c.Error))
		}
		if len(c.Latency.Typed) > 0 {
			p50, p95, max := stats(c.Latency.Typed)
			fmt.Fprintf(&b, "| typed latency p50/p95/max | %.1f / %.1f / %.1f ms |\n", p50, p95, max)
		}
		if len(c.Latency.SearchNow) > 0 {
			p50, p95, max := stats(c.Latency.SearchNow)
			fmt.Fprintf(&b, "| SearchNow latency p50/p95/max | %.1f / %.1f / %.1f ms |\n", p50, p95, max)
		}
		if c.SnippetsScreenshot != "" {
			fmt.Fprintf(&b, "| screenshots | `%s`, `%s`, `%s`, `%s` |\n", shortPath(c.SnippetsScreenshot), shortPath(c.BuildingScreenshot), shortPath(c.StatusScreenshot), shortPath(c.NoResultsScreenshot))
		}
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "| query | pass | rows | missing | extra | snippet | typed ms | now ms | attribution |\n|---|---:|---:|---|---|---:|---:|---:|---|\n")
		for _, q := range c.Queries {
			fmt.Fprintf(&b, "| %s | %t | %d | %s | %s | %t | %.1f | %.1f | %s |\n",
				mdCell(q.ID), q.Pass, q.Rows, mdCell(strings.Join(q.Missing, ", ")), mdCell(strings.Join(q.Extra, ", ")),
				q.SnippetPass, q.TypedLatencyMS, q.NowLatencyMS, mdCell(q.Attribution))
		}
		if len(c.Negatives) > 0 {
			fmt.Fprintf(&b, "\n| negative | pass | rows | attribution |\n|---|---:|---:|---|\n")
			for _, q := range c.Negatives {
				fmt.Fprintf(&b, "| %s | %t | %d | %s |\n", mdCell(q.ID), q.Pass, q.Rows, mdCell(q.Attribution))
			}
		}
		if c.Freshness != nil {
			f := c.Freshness
			fmt.Fprintf(&b, "\n| freshness | value |\n|---|---:|\n")
			fmt.Fprintf(&b, "| old query gone | %t |\n", f.BeforePass)
			fmt.Fprintf(&b, "| new query present | %t |\n", f.AfterPass)
			fmt.Fprintf(&b, "| reflect time | %.1f ms |\n", f.ReflectMS)
			if f.Attribution != "" {
				fmt.Fprintf(&b, "| attribution | %s |\n", mdCell(f.Attribution))
			}
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
	if r.SlowDrag != nil {
		if r.SlowDrag.Error != "" {
			out = append(out, "slow drag scenario: "+r.SlowDrag.Error)
		}
		if r.SlowDrag.FramesOver100 > 0 {
			out = append(out, fmt.Sprintf("%d frame(s) over 100 ms during the slow drag (max %.1f ms)", r.SlowDrag.FramesOver100, r.SlowDrag.FrameMaxMS))
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
	if r.Status != nil {
		if r.Status.Error != "" {
			out = append(out, "status scenario: "+r.Status.Error)
		}
		if !r.Status.SawNotReady {
			out = append(out, "status scenario: app never observed a not-ready state before ready")
		}
		if !r.Status.SidebarX {
			out = append(out, "status scenario: sidebar never listed the fixture drive without a restart")
		}
	}
	if r.Content != nil {
		c := r.Content
		if c.Error != "" {
			out = append(out, "content scenario: "+c.Error)
		}
		if c.BuildingSeen && !c.BuildingNoticeSeen {
			out = append(out, "content scenario: index was building but the results notice was not shown")
		}
		if c.BuildingSeen && strings.Contains(strings.ToLower(c.StatusText), "unavailable") {
			out = append(out, fmt.Sprintf("content scenario: status bar repeated the raw building error: %q", c.StatusText))
		}
		for _, q := range c.Queries {
			if !q.Pass {
				out = append(out, fmt.Sprintf("content query %q failed: missing=%v extra=%v; service=%v (attribution %s)", q.Query, q.Missing, q.Extra, q.ServiceActual, q.Attribution))
			}
			if !q.SnippetPass {
				out = append(out, fmt.Sprintf("content query %q snippet did not contain the expected text: %q", q.Query, q.SnippetText))
			}
		}
		for _, q := range c.Negatives {
			if !q.Pass {
				out = append(out, fmt.Sprintf("content negative %q returned %d rows: %v (attribution %s)", q.Query, q.Rows, q.Actual, q.Attribution))
			}
		}
		if c.Freshness != nil && !c.Freshness.Pass {
			out = append(out, fmt.Sprintf("content freshness failed: old_gone=%t new_present=%t (attribution %s)", c.Freshness.BeforePass, c.Freshness.AfterPass, c.Freshness.Attribution))
		}
	}
	return out
}
