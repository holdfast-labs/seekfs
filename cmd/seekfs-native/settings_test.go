package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEEKFS_NATIVE_SETTINGS", filepath.Join(dir, "native.json"))
	s := defaultSettings()
	s.WindowW = 1400
	s.WindowH = 900
	s.ContentsMode = true
	s.View = ViewIcons.String()
	s.SortColumn = SortSize.String()
	s.SortAsc = false
	s.ColumnWidths = map[string]int{"name": 320}
	s.Saved = []string{"ext:go", "content:needle"}
	if err := saveSettings(s); err != nil {
		t.Fatal(err)
	}
	got := loadSettings()
	if got.WindowW != 1400 || got.WindowH != 900 || !got.ContentsMode {
		t.Fatalf("got = %+v", got)
	}
	if got.View != ViewIcons.String() || got.SortColumn != SortSize.String() || got.SortAsc {
		t.Fatalf("got = %+v", got)
	}
	if got.ColumnWidths["name"] != 320 || len(got.Saved) != 2 {
		t.Fatalf("got = %+v", got)
	}
}

func TestSettingsMissingFileGivesDefaults(t *testing.T) {
	t.Setenv("SEEKFS_NATIVE_SETTINGS", filepath.Join(t.TempDir(), "nope.json"))
	got := loadSettings()
	if got.WindowW != 1180 || got.SortColumn != "name" || got.View != "details" {
		t.Fatalf("got = %+v", got)
	}
}

func TestSettingsCorruptFileGivesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SEEKFS_NATIVE_SETTINGS", path)
	got := loadSettings()
	if got.WindowW != 1180 {
		t.Fatalf("got = %+v", got)
	}
}

func TestViewModeRoundTrip(t *testing.T) {
	for _, m := range []ViewMode{ViewDetails, ViewIcons, ViewGrouped} {
		if viewModeFromString(m.String()) != m {
			t.Fatalf("round trip failed for %v", m)
		}
	}
	if viewModeFromString("bogus") != ViewDetails {
		t.Fatal("unknown view must fall back to details")
	}
}
