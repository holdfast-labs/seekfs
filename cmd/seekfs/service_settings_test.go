package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func settingsTestVolumes() []settingsVolumeInfo {
	return []settingsVolumeInfo{
		{Volume: "C:", FS: "NTFS", Label: "OS", TotalBytes: 1000, FreeBytes: 400},
		{Volume: "F:", FS: "NTFS", Label: "Data", TotalBytes: 2000, FreeBytes: 900},
	}
}

func useSyntheticSettingsVolumes(t *testing.T) {
	t.Helper()
	previous := settingsVolumeEnumeration
	settingsVolumeEnumeration = settingsTestVolumes
	t.Cleanup(func() { settingsVolumeEnumeration = previous })
}

func settingsTestConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "seekfs.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func settingsTestService(t *testing.T, configPath string, volumes []*serviceVolumeIndex) *goSearchService {
	t.Helper()
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	indexes := make([]*Index, 0, len(volumes))
	for _, vol := range volumes {
		indexes = append(indexes, vol.index)
	}
	return &goSearchService{
		stop:       make(chan struct{}),
		configPath: configPath,
		contentCfg: cfg,
		indexes:    indexes,
		volumes:    volumes,
	}
}

func startSettingsTestService(t *testing.T, s *goSearchService) string {
	t.Helper()
	pipeName := fmt.Sprintf(`\\.\pipe\seekfs-settings-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	s.pipeName = pipeName
	s.sddl = standaloneTestPipeSDDL
	if s.stop == nil {
		s.stop = make(chan struct{})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.servePipeListener()
	}()
	t.Cleanup(func() {
		s.signalServiceStop()
		if handle, err := openPipeClientWithTimeout(pipeName, 100*time.Millisecond); err == nil {
			_ = os.NewFile(uintptr(handle), pipeName).Close()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("settings test service listener did not stop")
		}
	})
	return pipeName
}

func callSettingsCommand(t *testing.T, s *goSearchService, caps serviceCapabilities, req serviceRequest) serviceResponse {
	t.Helper()
	var out bytes.Buffer
	s.handleServiceCommand(&out, servicePrincipal{Elevated: caps.Mutate}, caps, &req)
	var resp serviceResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode settings response: %v (%s)", err, out.String())
	}
	return resp
}

func settingsPatchRequest(t *testing.T, patch any) serviceRequest {
	t.Helper()
	raw, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	return serviceRequest{Command: "settings-set", Settings: raw}
}

func waitSettingsPluginApplied(t *testing.T, s *goSearchService) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.pluginApplying.Load() {
		if time.Now().After(deadline) {
			t.Fatal("plugin apply did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSettingsGetReturnsVolumesAndContent(t *testing.T) {
	preservePluginSelection(t)
	useSyntheticSettingsVolumes(t)
	configPath := settingsTestConfig(t, `[features.content]
enabled = true

[content]
mode = "auto"
roots = ["C:\work"]
exclude = ["C:\work\scratch"]
exts = []
git = true
system_excludes = true

[content."C:"]
mode = "explicit"
roots = ["C:\src"]
`)
	vol := syntheticServiceVolumeIndexForCacheTests()
	s := settingsTestService(t, configPath, []*serviceVolumeIndex{vol})
	pipeName := startSettingsTestService(t, s)

	resp, err := callService(pipeName, serviceRequest{Command: "settings-get"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Settings == nil {
		t.Fatalf("settings-get = %+v", resp)
	}
	settings := resp.Settings
	if settings.CanWrite {
		t.Fatal("settings-get reported can_write for a read-only caller")
	}
	byVolume := make(map[string]settingsVolume, len(settings.Volumes))
	for _, volume := range settings.Volumes {
		byVolume[volume.Volume] = volume
	}
	if !byVolume["C:"].Indexed || byVolume["C:"].State != "ready" {
		t.Fatalf("C: = %+v, want indexed ready", byVolume["C:"])
	}
	if byVolume["F:"].Indexed || byVolume["F:"].State != "not_indexed" {
		t.Fatalf("F: = %+v, want not indexed", byVolume["F:"])
	}
	content := settings.Content
	if !content.Enabled || content.Mode != "auto" {
		t.Fatalf("content = %+v", content)
	}
	if len(content.Roots) != 1 || content.Roots[0] != `C:\work` {
		t.Fatalf("content roots = %v", content.Roots)
	}
	override, ok := content.Volumes["C:"]
	if !ok || override.Mode == nil || *override.Mode != "explicit" {
		t.Fatalf("per-volume override = %+v", override)
	}
	if override.Roots == nil || len(*override.Roots) != 1 || (*override.Roots)[0] != `C:\src` {
		t.Fatalf("per-volume roots = %+v", override.Roots)
	}
}

func TestSettingsSetContentRoundTripAndLiveReload(t *testing.T) {
	preservePluginSelection(t)
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	configPath := settingsTestConfig(t, "default_limit = 10\n")
	pluginPath := filepath.Join(filepath.Dir(configPath), "plugin.toml")
	if err := os.WriteFile(pluginPath, []byte(`[plugins.content]
installed = true
enabled = true
scope = "auto"
roots = ["C:\\work"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := settingsTestService(t, configPath, nil)
	s.initializeFeatures()
	if !contentSearchEnabled() {
		t.Fatal("content was not enabled at start")
	}

	patch := map[string]any{
		"content": map[string]any{
			"enabled":         false,
			"mode":            "explicit",
			"roots":           []string{`C:\src`},
			"exclude":         []string{`C:\src\tmp`},
			"exts":            []string{".go"},
			"git":             false,
			"system_excludes": false,
			"budget_bytes":    int64(1048576),
		},
	}
	resp := callSettingsCommand(t, s, serviceCapabilities{ReadOnly: true, Mutate: true}, settingsPatchRequest(t, patch))
	if !resp.OK {
		t.Fatalf("settings-set = %+v", resp)
	}
	waitSettingsPluginApplied(t, s)

	defs, err := loadPluginDefinitions(pluginPath)
	if err != nil {
		t.Fatal(err)
	}
	content := defs["content"]
	if content.Feature.Enabled {
		t.Fatal("content enabled after enabled=false")
	}
	if content.Content.Mode != "explicit" || len(content.Content.Roots) != 1 || content.Content.Roots[0] != `C:\src` {
		t.Fatalf("content config = %+v", content.Content)
	}
	if content.Content.Git == nil || *content.Content.Git || content.Content.SystemExcludes == nil || *content.Content.SystemExcludes {
		t.Fatalf("content booleans = %+v", content.Content)
	}
	if content.Content.BudgetBytes != 1048576 || len(content.Content.Exts) != 1 || content.Content.Exts[0] != ".go" {
		t.Fatalf("content budget/exts = %+v", content.Content)
	}
	if contentSearchEnabled() {
		t.Fatal("live content selection stayed enabled")
	}

	restore := callSettingsCommand(t, s, serviceCapabilities{ReadOnly: true, Mutate: true}, settingsPatchRequest(t, map[string]any{"content": map[string]any{"enabled": true}}))
	if !restore.OK {
		t.Fatalf("restore = %+v", restore)
	}
	waitSettingsPluginApplied(t, s)
	if !contentSearchEnabled() {
		t.Fatal("content did not come back after enabled=true")
	}
}

func TestSettingsSetValidationWritesNothing(t *testing.T) {
	preservePluginSelection(t)
	useSyntheticSettingsVolumes(t)
	configPath := settingsTestConfig(t, `[content]
mode = "auto"
roots = ["C:\work"]
`)
	pluginPath := filepath.Join(filepath.Dir(configPath), "plugin.toml")
	if err := os.WriteFile(pluginPath, []byte("[plugins.content]\ninstalled = true\nenabled = true\nscope = \"auto\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := settingsTestService(t, configPath, nil)
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	beforePlugin, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		patch map[string]any
		field string
	}{
		{"relative root", map[string]any{"content": map[string]any{"roots": []string{"relative"}}}, "content.roots"},
		{"unknown volume", map[string]any{"content": map[string]any{"volumes": map[string]any{"Z:": map[string]any{"mode": "explicit"}}}}, "content.volumes.Z:"},
		{"bad mode", map[string]any{"content": map[string]any{"mode": "sideways"}}, "content.mode"},
		{"negative budget", map[string]any{"content": map[string]any{"budget_bytes": -1}}, "content.budget_bytes"},
	}
	for _, tt := range tests {
		resp := callSettingsCommand(t, s, serviceCapabilities{ReadOnly: true, Mutate: true}, settingsPatchRequest(t, tt.patch))
		if resp.OK || !strings.Contains(resp.Message, tt.field) {
			t.Fatalf("%s: response = %+v, want failure naming %s", tt.name, resp, tt.field)
		}
		afterConfig, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		afterPlugin, err := os.ReadFile(pluginPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(beforeConfig, afterConfig) || !bytes.Equal(beforePlugin, afterPlugin) {
			t.Fatalf("%s: validation failure wrote files", tt.name)
		}
	}
}

func TestSettingsSetRequiresElevationAndRemoteIsDenied(t *testing.T) {
	preservePluginSelection(t)
	useSyntheticSettingsVolumes(t)
	configPath := settingsTestConfig(t, "default_limit = 10\n")
	s := settingsTestService(t, configPath, nil)

	resp := callSettingsCommand(t, s, serviceCapabilities{ReadOnly: true}, settingsPatchRequest(t, map[string]any{"content": map[string]any{"mode": "auto"}}))
	if resp.OK || resp.Message != "settings-set requires an elevated caller" {
		t.Fatalf("non-elevated settings-set = %+v", resp)
	}
	for _, req := range []serviceRequest{
		{Command: "settings-get"},
		settingsPatchRequest(t, map[string]any{"content": map[string]any{"mode": "auto"}}),
	} {
		resp := callSettingsCommand(t, s, serviceCapabilities{ReadOnly: true, Mutate: true, Remote: true}, req)
		if resp.OK || resp.Settings != nil {
			t.Fatalf("remote %s = %+v, want refusal without settings", req.Command, resp)
		}
	}
}

func TestSettingsSetAddsAndRemovesVolume(t *testing.T) {
	preservePluginSelection(t)
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	useSyntheticSettingsVolumes(t)
	dbC := filepath.Join(t.TempDir(), "c.gsi")
	configPath := settingsTestConfig(t, fmt.Sprintf("volumes = %s\ndbs = %s\n", formatStringArray([]string{"C:"}), formatStringArray([]string{dbC})))
	volC := syntheticServiceVolumeIndexForCacheTests()
	volC.dbPath = dbC
	s := settingsTestService(t, configPath, []*serviceVolumeIndex{volC})

	var indexed []string
	previous := settingsIndexVolume
	settingsIndexVolume = func(s *goSearchService, volume, db string) (*Index, error) {
		indexed = append(indexed, volume)
		return &Index{Source: "usn", Volume: volume, Compact: true, Records: []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: "."}}}, nil
	}
	t.Cleanup(func() { settingsIndexVolume = previous })

	add := callSettingsCommand(t, s, serviceCapabilities{ReadOnly: true, Mutate: true}, settingsPatchRequest(t, map[string]any{"volumes": []map[string]any{{"volume": "F:", "indexed": true}}}))
	if !add.OK || add.Settings == nil {
		t.Fatalf("add volume = %+v", add)
	}
	if len(indexed) != 1 || indexed[0] != "F:" {
		t.Fatalf("index-usn path volumes = %v, want F:", indexed)
	}
	if !settingsVolumeIndexed(add.Settings.Volumes, "F:") {
		t.Fatalf("F: not indexed after add: %+v", add.Settings.Volumes)
	}
	if !settingsHasVolume(s, "F:") {
		t.Fatal("service volume list missing F:")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !settingsContainsVolume(cfg.Volumes, "F:") || len(cfg.DBs) != 2 {
		t.Fatalf("config after add = volumes %v dbs %v", cfg.Volumes, cfg.DBs)
	}

	remove := callSettingsCommand(t, s, serviceCapabilities{ReadOnly: true, Mutate: true}, settingsPatchRequest(t, map[string]any{"volumes": []map[string]any{{"volume": "F:", "indexed": false}}}))
	if !remove.OK || remove.Settings == nil {
		t.Fatalf("remove volume = %+v", remove)
	}
	if settingsVolumeIndexed(remove.Settings.Volumes, "F:") {
		t.Fatalf("F: still indexed after remove: %+v", remove.Settings.Volumes)
	}
	if settingsHasVolume(s, "F:") {
		t.Fatal("service volume list still has F:")
	}
	cfg, err = loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if settingsContainsVolume(cfg.Volumes, "F:") || len(cfg.DBs) != 1 {
		t.Fatalf("config after remove = volumes %v dbs %v", cfg.Volumes, cfg.DBs)
	}
}

func settingsVolumeIndexed(volumes []settingsVolume, volume string) bool {
	for _, entry := range volumes {
		if entry.Volume == volume {
			return entry.Indexed
		}
	}
	return false
}

func settingsHasVolume(s *goSearchService, volume string) bool {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	for _, vol := range s.volumes {
		if vol != nil && normalizeSettingsVolume(vol.volume) == volume {
			return true
		}
	}
	return false
}

func TestSettingsCLIGetJSON(t *testing.T) {
	preservePluginSelection(t)
	useSyntheticSettingsVolumes(t)
	configPath := settingsTestConfig(t, `[content]
mode = "auto"
roots = ["C:\work"]
`)
	vol := syntheticServiceVolumeIndexForCacheTests()
	s := settingsTestService(t, configPath, []*serviceVolumeIndex{vol})
	pipeName := startSettingsTestService(t, s)

	out, err := runCLICaptured(t, []string{"settings", "get", "--json", "--pipe", pipeName})
	if err != nil {
		t.Fatalf("settings get: %v (%s)", err, out)
	}
	var settings serviceSettings
	if err := json.Unmarshal([]byte(out), &settings); err != nil {
		t.Fatalf("decode settings CLI output: %v (%s)", err, out)
	}
	if len(settings.Volumes) == 0 || settings.Content.Mode != "auto" {
		t.Fatalf("CLI settings = %+v", settings)
	}
	if !strings.Contains(out, `"volumes"`) || !strings.Contains(out, `"content"`) {
		t.Fatalf("CLI output = %s", out)
	}
}
