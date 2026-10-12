package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"

	"seekfs/feature"
)

type contentActivation struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (s *goSearchService) contentConfig() appConfig {
	cfg := s.contentCfg
	if settings := s.pluginSettings.Load(); settings != nil {
		cfg.Plugins = settings.Definitions
	}
	return cfg
}

func (s *goSearchService) setPluginContentSelection(defs map[string]pluginDefinition) {
	if d, ok := defs["content"]; ok {
		selected := d.Installed && d.Feature.Enabled
		servicePluginContentSelection.Store(&selected)
	} else {
		servicePluginContentSelection.Store(nil)
	}
}

func (s *goSearchService) contentStopped(vol *serviceVolumeIndex) bool {
	if registry := s.features.Load(); registry != nil && !registry.content {
		return true
	}
	if s.serviceStopping() {
		return true
	}
	if a := vol.contentActivation.Load(); a != nil {
		return a.ctx.Err() != nil
	}
	return false
}

func contentActivationDone(vol *serviceVolumeIndex) <-chan struct{} {
	if a := vol.contentActivation.Load(); a != nil {
		return a.ctx.Done()
	}
	return nil
}

func (s *goSearchService) beginContentTask(vol *serviceVolumeIndex) bool {
	vol.contentTasksMu.Lock()
	defer vol.contentTasksMu.Unlock()
	if s.contentStopped(vol) {
		return false
	}
	if vol.contentTasks == 0 {
		vol.contentTasksDone = make(chan struct{})
	}
	vol.contentTasks++
	return true
}

func endContentTask(vol *serviceVolumeIndex) {
	vol.contentTasksMu.Lock()
	defer vol.contentTasksMu.Unlock()
	vol.contentTasks--
	if vol.contentTasks == 0 {
		close(vol.contentTasksDone)
	}
}

// Cancel before waiting; retain the old state/mapping until queued builds,
// catch-up, drains and folds have all released it. Filename queries continue.
func (s *goSearchService) stopContentPlugin(volumes []*serviceVolumeIndex) {
	s.indexMu.Lock()
	for _, vol := range volumes {
		if a := vol.contentActivation.Load(); a != nil {
			a.cancel()
		}
		vol.stopContentDrain()
	}
	s.indexMu.Unlock()
	for _, vol := range volumes {
		vol.contentTasksMu.Lock()
		done := vol.contentTasksDone
		count := vol.contentTasks
		vol.contentTasksMu.Unlock()
		if count > 0 {
			<-done
		}
	}
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	for _, vol := range volumes {
		vol.mu.Lock()
		if vol.content != nil {
			vol.content.mu.Lock()
			idx := vol.content.idx
			vol.content.idx, vol.content.reader, vol.content.resolver = nil, nil, nil
			vol.content.mu.Unlock()
			if idx != nil {
				idx.Release()
			}
		}
		vol.content, vol.contentCoord = nil, nil
		vol.contentActivation.Store(nil)
		vol.mu.Unlock()
	}
	s.contentLocksMu.Lock()
	defer s.contentLocksMu.Unlock()
	for path, lock := range s.contentLocks {
		lock.release()
		delete(s.contentLocks, path)
	}
}

func (s *goSearchService) applyPluginDefinitions(defs map[string]pluginDefinition) {
	s.pluginUpdateMu.Lock()
	defer s.pluginUpdateMu.Unlock()
	s.initializeFeatures()
	old := s.features.Load()
	previous := s.contentConfig().Plugins
	s.pluginSettings.Store(&pluginSettings{Definitions: defs})
	cfg := s.contentConfig()
	configs := configuredFeatureMap(cfg, defs)
	content := featureContentEnabled(cfg)
	next := &serviceFeatures{content: content, configs: configs, companions: make(map[string]*companionFeature)}
	restartContent := old.content && (!content || !reflect.DeepEqual(previous["content"].Content, defs["content"].Content))
	for name, f := range old.companions {
		wanted, ok := configs[name]
		if ok && wanted.Enabled && reflect.DeepEqual(old.configs[name], wanted) {
			next.companions[name] = f
		} else {
			f.requestStop()
		}
	}
	// Withdraw disabled/changing modules immediately; publish only immutable maps.
	retained := make(map[string]*companionFeature, len(next.companions))
	for name, f := range next.companions {
		retained[name] = f
	}
	interim := &serviceFeatures{configs: configs, content: old.content && !restartContent, companions: retained}
	s.features.Store(interim)
	if restartContent {
		disabled := false
		servicePluginContentSelection.Store(&disabled)
	}
	s.indexMu.RLock()
	volumes := append([]*serviceVolumeIndex(nil), s.volumes...)
	s.indexMu.RUnlock()
	if restartContent {
		s.stopContentPlugin(volumes)
	}
	for name, f := range old.companions {
		if next.companions[name] != f {
			<-f.stopped
		}
	}
	if s.serviceStopping() {
		return
	}
	for name, c := range configs {
		if name != "content" && c.Enabled && next.companions[name] == nil {
			next.companions[name] = newCompanionFeature(s, name, c)
		}
	}
	s.setPluginContentSelection(defs)
	selected := configs["content"].Enabled
	serviceContentSelection.Store(&selected)
	s.features.Store(next)
	s.indexMu.Lock()
	volumes = append([]*serviceVolumeIndex(nil), s.volumes...)
	for _, vol := range volumes {
		s.prepareFeatureVolume(vol)
		if len(next.companions) == 0 {
			vol.mu.Lock()
			vol.featureFeed = nil
			vol.mu.Unlock()
		}
	}
	s.indexMu.Unlock()
	for name, f := range next.companions {
		if old.companions[name] != f {
			go f.run()
		}
	}
	for _, vol := range volumes {
		s.featuresVolumeReady(vol)
	}
}

type pluginStatus struct {
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Description string            `json:"description,omitempty"`
	Installed   bool              `json:"installed"`
	Enabled     bool              `json:"enabled"`
	State       string            `json:"state"`
	Error       string            `json:"error,omitempty"`
	Progress    *feature.Progress `json:"progress,omitempty"`
}

func (s *goSearchService) pluginStatuses() []pluginStatus {
	f := s.features.Load()
	if f == nil {
		return []pluginStatus{{Name: "content", Kind: "builtin", Description: "Scoped file content search", State: "available"}}
	}
	settings := s.pluginSettings.Load()
	defs := make(map[string]pluginDefinition)
	if settings != nil {
		for name, d := range settings.Definitions {
			defs[name] = d
		}
	}
	for name, c := range f.configs {
		if _, ok := defs[name]; !ok {
			defs[name] = pluginDefinition{Installed: c.Enabled || c.Command != "", Feature: c}
		}
	}
	var out []pluginStatus
	for _, name := range pluginNames(defs) {
		d := defs[name]
		p := pluginStatus{Name: name, Kind: "companion", Installed: d.Installed, Enabled: d.Installed && d.Feature.Enabled, State: "available"}
		if name == "content" {
			p.Kind, p.Description = "builtin", "Scoped file content search"
			if f.content {
				p.Installed, p.Enabled = true, true
			}
			if p.Enabled {
				p.State = "unavailable"
				s.indexMu.RLock()
				loading := s.loading
				var healths []contentHealth
				for _, vol := range s.volumes {
					if vol.index != nil && vol.index.Source == "usn" && !contentScopeForVolume(s.contentConfig(), vol.volume).disabled() && vol.content != nil {
						depth := 0
						if vol.contentCoord != nil {
							depth = vol.contentCoord.queued()
						}
						h := vol.content.healthSnapshot(depth)
						if h.State == contentStateReady && (vol.content.catchUpPendingNow() || depth > 0) {
							h.State = contentStateIndexing
						}
						healths = append(healths, h)
					}
				}
				s.indexMu.RUnlock()
				if len(healths) > 0 {
					h := aggregateContentHealth(healths)
					p.State = h.State
					p.Error = h.BuildError
					if p.Error == "" {
						p.Error = h.FoldError
					}
					p.Progress = &feature.Progress{Current: int64(h.BuildDone), Total: int64(h.BuildTotal), Unit: "files"}
					if h.Incomplete && p.State == contentStateReady {
						p.State = contentStateDegraded
					}
				} else if loading {
					p.State = "loading"
				} else {
					p.Error = "no eligible indexed NTFS volume; check loaded volumes and content scope"
				}
			}
		} else if companion := f.companions[name]; companion != nil {
			h := companion.health()
			p.State, p.Error, p.Progress = h.State, h.Error, h.Progress
		}
		if p.Installed && !p.Enabled {
			p.State = "disabled"
		}
		if s.pluginApplying.Load() {
			p.State = "applying"
			p.Error = ""
		}
		out = append(out, p)
	}
	s.pluginErrorMu.Lock()
	lastError := s.pluginError
	s.pluginErrorMu.Unlock()
	if lastError != "" {
		for i := range out {
			out[i].Error = lastError
			out[i].State = "error"
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *goSearchService) serviceCommandPlugins(w io.Writer, req *serviceRequest) {
	s.initializeFeatures()
	path := s.pluginFilePath()
	if req.Command == "plugin-reload" {
		if s.serviceStopping() {
			_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "service stopping"})
			return
		}
		if req.PluginConfig != "" && !samePath(req.PluginConfig, path) {
			_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "plugin file does not match this service's plugin.toml", PluginConfig: path, PluginAPI: 1})
			return
		}
		defs, err := loadPluginDefinitions(path)
		if err != nil {
			_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: err.Error(), PluginConfig: path, PluginAPI: 1})
			return
		}
		s.pluginErrorMu.Lock()
		s.pluginError = ""
		s.pluginErrorMu.Unlock()
		s.pluginQueueMu.Lock()
		s.pluginPending = defs
		if !s.pluginApplying.Load() {
			s.pluginApplying.Store(true)
			go s.runPluginUpdates()
		}
		s.pluginQueueMu.Unlock()
	}
	_ = json.NewEncoder(w).Encode(serviceResponse{OK: true, PluginConfig: path, PluginAPI: 1, Plugins: s.pluginStatuses()})
}

func (s *goSearchService) runPluginUpdates() {
	defer func() {
		if r := recover(); r != nil {
			s.pluginErrorMu.Lock()
			s.pluginError = fmt.Sprint(r)
			s.pluginErrorMu.Unlock()
			s.pluginQueueMu.Lock()
			s.pluginApplying.Store(false)
			s.pluginPending = nil
			s.pluginQueueMu.Unlock()
		}
	}()
	for {
		s.pluginQueueMu.Lock()
		defs := s.pluginPending
		s.pluginPending = nil
		if defs == nil {
			s.pluginApplying.Store(false)
			s.pluginQueueMu.Unlock()
			return
		}
		s.pluginQueueMu.Unlock()
		s.applyPluginDefinitions(defs)
	}
}

func (s *goSearchService) pluginFilePath() string {
	if s.contentCfg.PluginPath != "" {
		return s.contentCfg.PluginPath
	}
	return filepath.Join(filepath.Dir(defaultConfigPath()), "plugin.toml")
}
