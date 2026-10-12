package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func preservePluginSelection(t *testing.T) {
	t.Helper()
	previous := servicePluginContentSelection.Load()
	old := serviceContentSelection.Load()
	t.Cleanup(func() { servicePluginContentSelection.Store(previous); serviceContentSelection.Store(old) })
}

func TestPluginFileIsSeparateAndPreservesOtherSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin.toml")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	original := "# user header\n[plugins.other]\ninstalled = true\nenabled = false\ncommand = " + strconvQuote(exe) + "\n# retained note\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := updatePluginFile(path, "content", func(pluginDefinition, bool) (pluginDefinition, error) {
		return pluginDefinition{Installed: true, Feature: featureConfig{Enabled: true}, Content: contentScopeConfig{Mode: "explicit", Roots: []string{`C:\work`}, BudgetBytes: 1024}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), original) {
		t.Fatalf("unrelated config was changed: %s", data)
	}
	defs, err := loadPluginDefinitions(path)
	if err != nil {
		t.Fatal(err)
	}
	if !defs["content"].Installed || defs["content"].Content.Mode != "explicit" || defs["content"].Content.BudgetBytes != 1024 {
		t.Fatalf("plugin config = %+v", defs)
	}
	if err := updatePluginFile(path, "content", func(pluginDefinition, bool) (pluginDefinition, error) { return pluginDefinition{Installed: false}, nil }); err != nil {
		t.Fatal(err)
	}
	defs, err = loadPluginDefinitions(path)
	if err != nil {
		t.Fatal(err)
	}
	if defs["content"].Installed || configuredFeatureMap(appConfig{Features: map[string]featureConfig{"content": {Enabled: true}}}, defs)["content"].Enabled {
		t.Fatal("removal did not override legacy configuration")
	}
	before, _ := os.ReadFile(path)
	if err := updatePluginFile(path, "content", func(pluginDefinition, bool) (pluginDefinition, error) {
		return pluginDefinition{Installed: true, Feature: featureConfig{Enabled: true}, Content: contentScopeConfig{Mode: "invalid"}}, nil
	}); err == nil {
		t.Fatal("invalid settings saved")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed edit damaged config")
	}
	for _, text := range []string{"[plugins.content]\nenabled = maybe", "[plugins.tags]\ncommand = 'relative.exe'", "[plugins.content]\nroots = ['relative']", "[plugins.content]\nenabled=true\nenabled=false"} {
		if _, err := parsePluginDefinitions(text); err == nil {
			t.Errorf("accepted invalid file %q", text)
		}
	}
}

func TestPluginCLIOfflineSetupAndConfig(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "seekfs.toml")
	path := filepath.Join(root, "plugin.toml")
	original := "default_limit = 25\n# keep seekfs configuration intact\n"
	if err := os.WriteFile(config, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	oldCall := pluginServiceCall
	pluginServiceCall = func(string, serviceRequest) (serviceResponse, error) { return serviceResponse{}, errors.New("offline") }
	t.Cleanup(func() { pluginServiceCall = oldCall })
	var out bytes.Buffer
	if err := runPluginCommand([]string{"add", "content", "--config", config, "--root", root, "--git=false", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var result pluginCommandResponse
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Service || result.Config != path || len(result.Plugins) != 1 || !result.Plugins[0].Installed {
		t.Fatalf("add response = %+v", result)
	}
	data, _ := os.ReadFile(config)
	if string(data) != original {
		t.Fatal("plugin command changed seekfs.toml")
	}
	defs, err := loadPluginDefinitions(path)
	if err != nil {
		t.Fatal(err)
	}
	if defs["content"].Content.Mode != "explicit" || defs["content"].Content.Git == nil || *defs["content"].Content.Git {
		t.Fatalf("content CLI settings = %+v", defs["content"])
	}
	before, _ := os.ReadFile(path)
	out.Reset()
	if err := runPluginCommand([]string{"config", "content", "--config", config}, &out); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("viewing config edited it")
	}
	out.Reset()
	if err := runPluginCommand([]string{"remove", "content", "--config", config}, &out); err != nil {
		t.Fatal(err)
	}
	defs, err = loadPluginDefinitions(path)
	if err != nil || defs["content"].Installed {
		t.Fatalf("remove = %+v, %v", defs, err)
	}
}

func TestPluginCLIAddBuildsContentWithoutServiceRestart(t *testing.T) {
	preservePluginSelection(t)
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello plugin needle"), 0600); err != nil {
		t.Fatal(err)
	}
	const journal = 7
	const checkpoint = 100
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: checkpoint})
	vol, _ := contentBuildTestVolume(t, dir, journal, checkpoint, []CompactRecord{{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 19}})
	root := t.TempDir()
	config := filepath.Join(root, "seekfs.toml")
	path := filepath.Join(root, "plugin.toml")
	if err := os.WriteFile(config, []byte("default_limit = 10\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &goSearchService{stop: make(chan struct{}), contentCfg: appConfig{PluginPath: path}, indexes: []*Index{vol.index}, volumes: []*serviceVolumeIndex{vol}}
	s.initializeFeatures()
	t.Cleanup(func() { s.signalServiceStop() })
	oldCall := pluginServiceCall
	pluginServiceCall = func(_ string, req serviceRequest) (serviceResponse, error) {
		var b bytes.Buffer
		s.handleServiceCommand(&b, servicePrincipal{Elevated: true}, serviceCapabilities{ReadOnly: true, Mutate: true}, &req)
		var resp serviceResponse
		err := json.Unmarshal(b.Bytes(), &resp)
		return resp, err
	}
	t.Cleanup(func() { pluginServiceCall = oldCall })
	var out bytes.Buffer
	if err := runPluginCommand([]string{"add", "content", "--config", config, "--root", dir, "--wait", "--timeout", "5s", "--json"}, &out); err != nil {
		t.Fatalf("plugin add: %v\n%s", err, &out)
	}
	if !contentSearchEnabled() || vol.content == nil || vol.content.stateOf() != contentStateReady {
		t.Fatal("plugin add did not activate content")
	}
	matches, err := searchServiceVolumes(snapshotServiceVolumesForSearch(s.volumes), queryOptions{Query: "content:needle", Limit: 10}, false)
	if err != nil || len(matches) != 1 {
		t.Fatalf("content query after add = %v, %v", matches, err)
	}
	out.Reset()
	if err := runPluginCommand([]string{"remove", "content", "--config", config, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.pluginApplying.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.pluginApplying.Load() {
		t.Fatal("removal never finished")
	}
	if contentSearchEnabled() || vol.content != nil {
		t.Fatal("content still active after removal")
	}
	matches, err = searchServiceVolumes(snapshotServiceVolumesForSearch(s.volumes), queryOptions{Query: "note.txt", Limit: 10}, false)
	if err != nil || len(matches) != 1 {
		t.Fatalf("removal broke filename search: %v, %v", matches, err)
	}
	out.Reset()
	if err := runPluginCommand([]string{"add", "content", "--config", config, "--root", dir, "--wait", "--timeout", "5s"}, &out); err != nil {
		t.Fatalf("re-add: %v\n%s", err, &out)
	}
}

func TestPluginReloadPermissionsAndPinnedPath(t *testing.T) {
	preservePluginSelection(t)
	s := &goSearchService{stop: make(chan struct{}), contentCfg: appConfig{PluginPath: filepath.Join(t.TempDir(), "plugin.toml")}}
	defer s.signalServiceStop()
	for _, caps := range []serviceCapabilities{{ReadOnly: true}, {ReadOnly: true, Mutate: true, Remote: true}} {
		var out bytes.Buffer
		s.handleServiceCommand(&out, servicePrincipal{}, caps, &serviceRequest{Command: "plugin-reload"})
		var resp serviceResponse
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.OK {
			t.Fatal("unprivileged/remote plugin reload allowed")
		}
	}
	var out bytes.Buffer
	s.handleServiceCommand(&out, servicePrincipal{Elevated: true}, serviceCapabilities{ReadOnly: true, Mutate: true}, &serviceRequest{Command: "plugin-reload", PluginConfig: filepath.Join(t.TempDir(), "foreign.toml")})
	var resp serviceResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || !strings.Contains(resp.Message, "does not match") {
		t.Fatalf("foreign plugin file accepted: %+v", resp)
	}
}

func TestPluginRemovalCancelsQueuedContentBuild(t *testing.T) {
	preservePluginSelection(t)
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	dir := t.TempDir()
	vol, _ := contentBuildTestVolume(t, dir, 7, 100, []CompactRecord{{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 10}})
	s := &goSearchService{stop: make(chan struct{}), indexes: []*Index{vol.index}, volumes: []*serviceVolumeIndex{vol}}
	defer s.signalServiceStop()
	oldRun := contentBuildRun
	var queued func()
	contentBuildRun = func(fn func()) { queued = fn }
	t.Cleanup(func() { contentBuildRun = oldRun })
	s.applyPluginDefinitions(map[string]pluginDefinition{"content": {Installed: true, Feature: featureConfig{Enabled: true}}})
	if queued == nil || !vol.contentBuildBusy.Load() {
		t.Fatal("content build was not queued")
	}
	finished := make(chan struct{})
	go func() {
		s.applyPluginDefinitions(map[string]pluginDefinition{"content": {Installed: false}})
		close(finished)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for contentSearchEnabled() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if contentSearchEnabled() {
		t.Fatal("removal did not withdraw content")
	}
	queued()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("removal hung on canceled queued build")
	}
	if vol.content != nil || vol.contentBuildBusy.Load() {
		t.Fatal("queued build survived removal")
	}
}

func TestPluginCompanionStopsAndRestartsLive(t *testing.T) {
	preservePluginSelection(t)
	s, vol, f := featureTestService(t, "normal")
	if _, _, err := f.query(context.Background(), vol, "all", false); err != nil {
		t.Fatal(err)
	}
	s.applyPluginDefinitions(map[string]pluginDefinition{"tags": {Installed: false}})
	select {
	case <-f.stopped:
	default:
		t.Fatal("companion did not stop")
	}
	if vol.featureFeed != nil {
		t.Fatal("removed companion retained change feed")
	}
	s.applyPluginDefinitions(map[string]pluginDefinition{"tags": {Installed: true, Feature: s.contentCfg.Features["tags"]}})
	current := s.features.Load().companions["tags"]
	if current == nil || current == f {
		t.Fatal("companion was not recreated")
	}
	t.Cleanup(func() {
		s.signalServiceStop()
		select {
		case <-current.stopped:
		case <-time.After(5 * time.Second):
			t.Error("replacement companion did not stop")
		}
	})
	if set, _, err := current.query(context.Background(), vol, "blue", false); err != nil || len(set) != 2 {
		t.Fatalf("restarted query = %v, %v", set, err)
	}
}

func TestPluginReloadQueuesLatestConfiguration(t *testing.T) {
	preservePluginSelection(t)
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	path := filepath.Join(t.TempDir(), "plugin.toml")
	s := &goSearchService{stop: make(chan struct{}), contentCfg: appConfig{PluginPath: path}}
	defer s.signalServiceStop()
	s.initializeFeatures()
	s.pluginUpdateMu.Lock()
	if err := updatePluginFile(path, "content", func(pluginDefinition, bool) (pluginDefinition, error) {
		return pluginDefinition{Installed: true, Feature: featureConfig{Enabled: true}}, nil
	}); err != nil {
		s.pluginUpdateMu.Unlock()
		t.Fatal(err)
	}
	var out bytes.Buffer
	s.serviceCommandPlugins(&out, &serviceRequest{Command: "plugin-reload", PluginConfig: path})
	if err := updatePluginFile(path, "content", func(pluginDefinition, bool) (pluginDefinition, error) { return pluginDefinition{Installed: false}, nil }); err != nil {
		s.pluginUpdateMu.Unlock()
		t.Fatal(err)
	}
	out.Reset()
	s.serviceCommandPlugins(&out, &serviceRequest{Command: "plugin-reload", PluginConfig: path})
	var resp serviceResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		s.pluginUpdateMu.Unlock()
		t.Fatal(err)
	}
	if !resp.OK {
		s.pluginUpdateMu.Unlock()
		t.Fatalf("queued reload rejected: %+v", resp)
	}
	s.pluginUpdateMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for s.pluginApplying.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.pluginApplying.Load() || s.features.Load().content || s.pluginSettings.Load().Definitions["content"].Installed {
		t.Fatal("latest queued settings were not applied")
	}
}
