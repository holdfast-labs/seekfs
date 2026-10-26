package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stubBuildSpeedProbes(t *testing.T) {
	t.Helper()
	restoreUser, restoreInput, restoreQuery, restoreBattery := contentUserActiveNow, contentInputActiveNow, contentBuildQueriesActiveNow, contentOnBatteryNow
	restoreWorkers := contentBuildWorkers
	previousMode := contentBuildSpeedCurrent.Load()
	t.Cleanup(func() {
		contentUserActiveNow, contentInputActiveNow, contentBuildQueriesActiveNow, contentOnBatteryNow = restoreUser, restoreInput, restoreQuery, restoreBattery
		contentBuildWorkers = restoreWorkers
		contentBuildSpeedCurrent.Store(previousMode)
		t.Setenv("SEEKFS_CONTENT_IOPRIORITY", "")
	})
}

func TestContentBuildSpeedWorkerPolicy(t *testing.T) {
	stubBuildSpeedProbes(t)
	contentBuildWorkers = 8
	t.Setenv("SEEKFS_CONTENT_IOPRIORITY", "")
	cases := []struct {
		name                          string
		mode                          string
		recent, input, inflight, batt bool
		want                          int
		ioLow                         bool
	}{
		{"balanced idle", contentBuildSpeedBalanced, false, false, false, false, 8, false},
		{"balanced recent query", contentBuildSpeedBalanced, true, false, false, false, 4, true},
		{"balanced input", contentBuildSpeedBalanced, false, true, false, false, 4, true},
		{"balanced battery", contentBuildSpeedBalanced, false, false, false, true, 2, true},
		{"balanced battery+active", contentBuildSpeedBalanced, true, true, false, true, 2, true},
		{"balanced inflight query", contentBuildSpeedBalanced, true, true, true, false, 1, true},
		{"fast idle", contentBuildSpeedFast, false, false, false, false, 8, false},
		{"fast active", contentBuildSpeedFast, true, true, false, false, 8, false},
		{"fast battery", contentBuildSpeedFast, false, false, false, true, 8, false},
		{"fast inflight query", contentBuildSpeedFast, false, false, true, false, 1, false},
		{"gentle idle", contentBuildSpeedGentle, false, false, false, false, 8, false},
		{"gentle recent", contentBuildSpeedGentle, true, false, false, false, 1, true},
		{"gentle input", contentBuildSpeedGentle, false, true, false, false, 1, true},
		{"gentle battery", contentBuildSpeedGentle, false, false, false, true, 1, true},
		{"gentle inflight", contentBuildSpeedGentle, false, false, true, false, 1, true},
	}
	for _, c := range cases {
		contentSetBuildSpeed(c.mode)
		contentUserActiveNow = func() bool { return c.recent }
		contentInputActiveNow = func() bool { return c.input }
		contentBuildQueriesActiveNow = func() bool { return c.inflight }
		contentOnBatteryNow = func() bool { return c.batt }
		if got := contentBuildWorkerCount(); got != c.want {
			t.Errorf("%s: workers=%d; want %d", c.name, got, c.want)
		}
		if got := contentLowerIOPriorityNow(); got != c.ioLow {
			t.Errorf("%s: low IO=%v; want %v", c.name, got, c.ioLow)
		}
	}

	// Small caps clamp: balanced never exceeds 4 active / 2 battery.
	contentBuildWorkers = 3
	contentSetBuildSpeed(contentBuildSpeedBalanced)
	contentUserActiveNow = func() bool { return true }
	contentInputActiveNow = func() bool { return false }
	contentBuildQueriesActiveNow = func() bool { return false }
	contentOnBatteryNow = func() bool { return false }
	if got := contentBuildWorkerCount(); got != 3 {
		t.Errorf("balanced active cap 3 = %d; want 3", got)
	}
	contentBuildWorkers = 1
	contentSetBuildSpeed(contentBuildSpeedBalanced)
	if got := contentBuildWorkerCount(); got != 1 {
		t.Errorf("cap 1 = %d; want 1", got)
	}
}

func TestContentBuildWorkerCountResampledMidBuild(t *testing.T) {
	stubBuildSpeedProbes(t)
	contentBuildWorkers = 8
	contentSetBuildSpeed(contentBuildSpeedBalanced)
	contentUserActiveNow = func() bool { return false }
	contentInputActiveNow = func() bool { return false }
	contentBuildQueriesActiveNow = func() bool { return false }
	contentOnBatteryNow = func() bool { return false }
	contentBuildBatchSize = 2
	t.Cleanup(func() { contentBuildBatchSize = 64 })
	items := []contentBuildItem{{frn: 1, path: "a.txt"}, {frn: 2, path: "b.txt"}, {frn: 3, path: "c.txt"}, {frn: 4, path: "d.txt"}}
	extract := func(ctx context.Context, item contentBuildItem) (contentBuildDoc, contentExtractResult, bool) {
		return contentBuildDoc{path: item.path, text: []byte("needle")}, contentExtractResult{}, true
	}
	a := newContentBuildAdaptivePrefetch(context.Background(), items, extract)
	defer a.stop()
	if got := a.workerCount(); got != 8 {
		t.Fatalf("initial workers=%d; want 8", got)
	}
	a.next()
	contentUserActiveNow = func() bool { return true }
	a.adapt()
	if got := a.workerCount(); got != 4 {
		t.Fatalf("active workers=%d; want 4", got)
	}
	contentOnBatteryNow = func() bool { return true }
	contentUserActiveNow = func() bool { return false }
	a.adapt()
	if got := a.workerCount(); got != 2 {
		t.Fatalf("battery workers=%d; want 2", got)
	}
	contentSetBuildSpeed(contentBuildSpeedFast)
	a.adapt()
	if got := a.workerCount(); got != 8 {
		t.Fatalf("fast workers=%d; want 8", got)
	}
}

func TestContentBuildSpeedConfigAndSettings(t *testing.T) {
	preservePluginSelection(t)
	defs, err := parsePluginDefinitions("[plugins.content]\ninstalled = true\nenabled = true\nbuild_speed = \"fast\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := defs["content"].Content.BuildSpeed; got != contentBuildSpeedFast {
		t.Fatalf("parsed build_speed=%q; want fast", got)
	}
	formatted := formatPluginDefinition("content", defs["content"])
	if !strings.Contains(formatted, "build_speed = \"fast\"") {
		t.Fatalf("formatted plugin missing build_speed: %s", formatted)
	}
	if _, err := parsePluginDefinitions("[plugins.content]\ninstalled = true\nbuild_speed = \"warp\"\n"); err == nil {
		t.Fatal("invalid build_speed parsed")
	}

	cfg := appConfig{Plugins: defs}
	if got := contentScopeForVolume(cfg, "C:").BuildSpeed; got != contentBuildSpeedFast {
		t.Fatalf("resolved build speed=%q; want fast", got)
	}
	info := settingsContentFromConfig(cfg)
	if info.BuildSpeed != contentBuildSpeedFast {
		t.Fatalf("settings projection build_speed=%q; want fast", info.BuildSpeed)
	}
	if err := validateSettingsContentPatch("content", &settingsContentPatch{BuildSpeed: stringPtr("gentle")}, nil); err != nil {
		t.Fatalf("valid build_speed rejected: %v", err)
	}
	if err := validateSettingsContentPatch("content", &settingsContentPatch{BuildSpeed: stringPtr("warp")}, nil); err == nil {
		t.Fatal("invalid build_speed accepted")
	}

	// settings-set writes build_speed into plugin.toml and settings-get reads it.
	configPath := settingsTestConfig(t, "default_limit = 10\n")
	pluginPath := filepath.Join(filepath.Dir(configPath), "plugin.toml")
	if err := os.WriteFile(pluginPath, []byte("[plugins.content]\ninstalled = true\nenabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := settingsTestService(t, configPath, nil)
	resp := callSettingsCommand(t, s, serviceCapabilities{ReadOnly: true, Mutate: true},
		settingsPatchRequest(t, map[string]any{"content": map[string]any{"build_speed": "gentle"}}))
	if !resp.OK {
		t.Fatalf("settings-set build_speed = %+v", resp)
	}
	waitSettingsPluginApplied(t, s)
	loaded, err := loadPluginDefinitions(pluginPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["content"].Content.BuildSpeed; got != contentBuildSpeedGentle {
		t.Fatalf("persisted build_speed=%q; want gentle", got)
	}
	getResp := callSettingsCommand(t, s, serviceCapabilities{ReadOnly: true}, serviceRequest{Command: "settings-get"})
	if !getResp.OK || getResp.Settings == nil || getResp.Settings.Content.BuildSpeed != contentBuildSpeedGentle {
		t.Fatalf("settings-get build_speed = %+v", getResp.Settings)
	}
}

func stringPtr(s string) *string { return &s }
