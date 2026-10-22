//go:build windows

package main

// wire.go mirrors the automation JSON contract in cmd/seekfs-native/uiauto.go.

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
