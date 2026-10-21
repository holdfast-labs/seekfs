package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings persists window position/size, column widths, view mode and the
// Contents toggle in %APPDATA%\seekfs\native.json.

type ViewMode int

const (
	ViewDetails ViewMode = iota
	ViewIcons
	ViewGrouped
)

func (m ViewMode) String() string {
	switch m {
	case ViewIcons:
		return "icons"
	case ViewGrouped:
		return "grouped"
	default:
		return "details"
	}
}

func viewModeFromString(s string) ViewMode {
	switch s {
	case "icons":
		return ViewIcons
	case "grouped":
		return ViewGrouped
	default:
		return ViewDetails
	}
}

type Settings struct {
	WindowX      int            `json:"window_x,omitempty"`
	WindowY      int            `json:"window_y,omitempty"`
	WindowW      int            `json:"window_w,omitempty"`
	WindowH      int            `json:"window_h,omitempty"`
	Maximized    bool           `json:"maximized,omitempty"`
	ColumnWidths map[string]int `json:"column_widths,omitempty"`
	SortColumn   string         `json:"sort_column,omitempty"`
	SortAsc      bool           `json:"sort_asc,omitempty"`
	View         string         `json:"view,omitempty"`
	ContentsMode bool           `json:"contents_mode,omitempty"`
	PreviewOpen  bool           `json:"preview_open,omitempty"`
	PreviewW     int            `json:"preview_w,omitempty"`
	Under        string         `json:"under,omitempty"`
	Saved        []string       `json:"saved_searches,omitempty"`
}

func defaultSettings() Settings {
	return Settings{
		WindowW:      1180,
		WindowH:      760,
		SortColumn:   SortName.String(),
		SortAsc:      true,
		View:         ViewDetails.String(),
		ColumnWidths: map[string]int{},
	}
}

func settingsPath() string {
	if v := os.Getenv("SEEKFS_NATIVE_SETTINGS"); v != "" {
		return v
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "seekfs", "native.json")
}

func loadSettings() Settings {
	s := defaultSettings()
	data, err := os.ReadFile(settingsPath())
	if err != nil {
		return s
	}
	var loaded Settings
	if err := json.Unmarshal(data, &loaded); err != nil {
		return s
	}
	if loaded.WindowW > 0 {
		s.WindowW = loaded.WindowW
	}
	if loaded.WindowH > 0 {
		s.WindowH = loaded.WindowH
	}
	s.WindowX = loaded.WindowX
	s.WindowY = loaded.WindowY
	s.Maximized = loaded.Maximized
	if loaded.ColumnWidths != nil {
		s.ColumnWidths = loaded.ColumnWidths
	}
	if loaded.SortColumn != "" {
		s.SortColumn = sortKeyFromString(loaded.SortColumn).String()
	}
	s.SortAsc = loaded.SortAsc
	if loaded.View == "" {
		loaded.View = ViewDetails.String()
	}
	s.View = viewModeFromString(loaded.View).String()
	s.ContentsMode = loaded.ContentsMode
	s.PreviewOpen = loaded.PreviewOpen
	s.Under = loaded.Under
	s.Saved = append([]string(nil), loaded.Saved...)
	return s
}

func saveSettings(s Settings) error {
	if s.WindowW <= 0 {
		s.WindowW = defaultSettings().WindowW
	}
	if s.WindowH <= 0 {
		s.WindowH = defaultSettings().WindowH
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := settingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
