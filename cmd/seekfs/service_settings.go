package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/windows"
)

// serviceSettings is the settings payload shared by settings-get and
// settings-set. The desktop app reads it to render the indexing controls.
type serviceSettings struct {
	CanWrite bool `json:"can_write"`
	// ServiceMode is the service's process mode: "windows-service" for the
	// installed service, "standalone" otherwise.
	ServiceMode string              `json:"service_mode,omitempty"`
	Volumes     []settingsVolume    `json:"volumes"`
	Content     settingsContentInfo `json:"content"`
}

type settingsVolume struct {
	Volume     string `json:"volume"`
	Indexed    bool   `json:"indexed"`
	FS         string `json:"fs,omitempty"`
	Label      string `json:"label,omitempty"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
	Entries    int    `json:"entries"`
	State      string `json:"state"`
	Error      string `json:"error,omitempty"`
	// Progress is present only while State is "indexing".
	Progress *settingsVolumeProgress `json:"progress,omitempty"`
}

type settingsContentInfo struct {
	Enabled        bool                               `json:"enabled"`
	Mode           string                             `json:"mode"`
	Roots          []string                           `json:"roots"`
	Exclude        []string                           `json:"exclude"`
	Exts           []string                           `json:"exts"`
	Git            bool                               `json:"git"`
	SystemExcludes bool                               `json:"system_excludes"`
	BudgetBytes    int64                              `json:"budget_bytes"`
	Volumes        map[string]settingsContentOverride `json:"volumes,omitempty"`
}

// settingsContentOverride holds only the keys that are set, both for the
// settings-get projection and for a settings-set patch.
type settingsContentOverride struct {
	Mode           *string   `json:"mode,omitempty"`
	Roots          *[]string `json:"roots,omitempty"`
	Exclude        *[]string `json:"exclude,omitempty"`
	Exts           *[]string `json:"exts,omitempty"`
	Git            *bool     `json:"git,omitempty"`
	SystemExcludes *bool     `json:"system_excludes,omitempty"`
	BudgetBytes    *int64    `json:"budget_bytes,omitempty"`
}

type settingsPatch struct {
	Volumes []settingsVolumePatch `json:"volumes,omitempty"`
	Content *settingsContentPatch `json:"content,omitempty"`
}

type settingsVolumePatch struct {
	Volume  string `json:"volume"`
	Indexed *bool  `json:"indexed,omitempty"`
}

type settingsContentPatch struct {
	Enabled        *bool                              `json:"enabled,omitempty"`
	Mode           *string                            `json:"mode,omitempty"`
	Roots          *[]string                          `json:"roots,omitempty"`
	Exclude        *[]string                          `json:"exclude,omitempty"`
	Exts           *[]string                          `json:"exts,omitempty"`
	Git            *bool                              `json:"git,omitempty"`
	SystemExcludes *bool                              `json:"system_excludes,omitempty"`
	BudgetBytes    *int64                             `json:"budget_bytes,omitempty"`
	Volumes        map[string]settingsContentOverride `json:"volumes,omitempty"`
}

// settingsVolumeEnumeration is the local fixed-volume probe. Tests replace it
// with synthetic volumes so no real disk is touched.
var settingsVolumeEnumeration = enumerateSettingsVolumes

// serviceIndexUSNVolume is the low-level index build seam shared by the
// index-usn path. Tests replace it with a small synthetic Index instead of
// touching a real USN journal. progress is nil outside settings-driven builds.
var serviceIndexUSNVolume = func(volume string, progress *volumeBuildProgress) (*Index, error) {
	return indexUSNVolume(volume, progress)
}

func (s *goSearchService) settingsConfigPath() string {
	if s != nil && s.configPath != "" {
		return s.configPath
	}
	if s != nil && s.contentCfg.PluginPath != "" {
		return filepath.Join(filepath.Dir(s.contentCfg.PluginPath), "seekfs.toml")
	}
	return defaultConfigPath()
}

func (s *goSearchService) serviceCommandSettingsGet(w io.Writer, caps serviceCapabilities) {
	if caps.Remote {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "settings-get is not available to remote callers"})
		return
	}
	s.initializeFeatures()
	settings := s.currentServiceSettings(caps)
	_ = json.NewEncoder(w).Encode(serviceResponse{OK: true, Settings: &settings})
}

func (s *goSearchService) serviceCommandSettingsSet(w io.Writer, caps serviceCapabilities, req *serviceRequest) {
	if caps.Remote {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "settings-set is not available to remote callers"})
		return
	}
	var patch settingsPatch
	if len(req.Settings) > 0 {
		if err := json.Unmarshal(req.Settings, &patch); err != nil {
			_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "settings: " + err.Error()})
			return
		}
	}
	s.initializeFeatures()
	if err := s.validateSettingsPatch(&patch); err != nil {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: err.Error()})
		return
	}
	if err := s.applySettingsVolumes(patch.Volumes); err != nil {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: err.Error()})
		return
	}
	if patch.Content != nil {
		if err := s.applySettingsContent(patch.Content); err != nil {
			_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: err.Error()})
			return
		}
	}
	settings := s.currentServiceSettings(caps)
	_ = json.NewEncoder(w).Encode(serviceResponse{OK: true, Settings: &settings})
}

func (s *goSearchService) currentServiceSettings(caps serviceCapabilities) serviceSettings {
	s.indexMu.RLock()
	volumes := append([]*serviceVolumeIndex(nil), s.volumes...)
	loading := s.loading
	s.indexMu.RUnlock()

	cfg := s.contentConfig()
	settings := serviceSettings{
		CanWrite:    caps.Mutate,
		ServiceMode: s.processMode,
		Volumes:     settingsVolumeList(settingsVolumeEnumeration(), volumes, loading, s.settingsVolumeStateOverrides()),
		Content:     settingsContentFromConfig(cfg),
	}
	return settings
}

type settingsVolumeStateOverride struct {
	indexed  bool
	state    string
	err      string
	progress *volumeBuildProgress
}

func (s *goSearchService) settingsVolumeStateOverrides() map[string]settingsVolumeStateOverride {
	s.settingsVolumeMu.Lock()
	defer s.settingsVolumeMu.Unlock()
	out := make(map[string]settingsVolumeStateOverride, len(s.settingsVolumeTasks)+len(s.settingsVolumeErrs))
	for volume, task := range s.settingsVolumeTasks {
		out[volume] = settingsVolumeStateOverride{indexed: true, state: "indexing", progress: task.progress}
	}
	for volume, message := range s.settingsVolumeErrs {
		if _, pending := out[volume]; pending {
			continue
		}
		out[volume] = settingsVolumeStateOverride{state: "error", err: message}
	}
	return out
}

func settingsVolumeList(infos []settingsVolumeInfo, volumes []*serviceVolumeIndex, loading bool, overrides map[string]settingsVolumeStateOverride) []settingsVolume {
	byVolume := make(map[string]*settingsVolume, len(infos)+len(volumes))
	for _, info := range infos {
		volume := normalizeSettingsVolume(info.Volume)
		if volume == "" {
			continue
		}
		byVolume[volume] = &settingsVolume{
			Volume:     volume,
			FS:         info.FS,
			Label:      info.Label,
			TotalBytes: info.TotalBytes,
			FreeBytes:  info.FreeBytes,
			State:      "not_indexed",
		}
	}
	for _, vol := range volumes {
		if vol == nil {
			continue
		}
		volume := normalizeSettingsVolume(vol.volume)
		if volume == "" {
			continue
		}
		entry := byVolume[volume]
		if entry == nil {
			entry = &settingsVolume{Volume: volume}
			byVolume[volume] = entry
		}
		entry.Indexed = true
		entry.Entries = 0
		if vol.index != nil {
			entry.Entries = vol.index.entryCount()
		}
		entry.State = settingsVolumeState(vol, loading)
		if entry.State == "error" {
			entry.Error = settingsVolumeErrorMessage(vol.staleReason, serviceProcessElevated())
		}
	}
	for volume, override := range overrides {
		entry := byVolume[volume]
		if entry == nil {
			entry = &settingsVolume{Volume: volume}
			byVolume[volume] = entry
		}
		entry.Indexed = override.indexed
		entry.State = override.state
		entry.Error = override.err
		if override.state == "indexing" {
			entry.Progress = override.progress.settingsProgress()
		}
		if override.state != "ready" {
			entry.Entries = 0
		}
	}
	out := make([]settingsVolume, 0, len(byVolume))
	for _, entry := range byVolume {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Volume < out[j].Volume })
	return out
}

func settingsVolumeState(vol *serviceVolumeIndex, loading bool) string {
	if vol == nil {
		return "not_indexed"
	}
	if vol.index == nil {
		return "not_indexed"
	}
	switch vol.state {
	case "ready", "":
		if loading {
			return "indexing"
		}
		return "ready"
	case "replaying", "indexing":
		return "indexing"
	case "stale", "error":
		return "error"
	default:
		return "ready"
	}
}

func settingsContentFromConfig(cfg appConfig) settingsContentInfo {
	global := cfg.Content
	if d, ok := cfg.Plugins["content"]; ok && d.Installed {
		global = mergeContentScopeConfig(global, d.Content)
	}
	info := settingsContentInfo{
		Enabled:        featureContentEnabled(cfg),
		Mode:           strings.TrimSpace(global.Mode),
		Roots:          append([]string{}, global.Roots...),
		Exclude:        append([]string{}, global.Exclude...),
		Exts:           append([]string{}, global.Exts...),
		Git:            boolValue(global.Git, true),
		SystemExcludes: boolValue(global.SystemExcludes, true),
		BudgetBytes:    global.BudgetBytes,
	}
	if info.Mode == "" {
		info.Mode = string(contentScopeAuto)
	}
	if len(cfg.ContentByVolume) > 0 {
		info.Volumes = make(map[string]settingsContentOverride, len(cfg.ContentByVolume))
		for volume, override := range cfg.ContentByVolume {
			info.Volumes[normalizeSettingsVolume(volume)] = settingsOverrideFromConfig(override)
		}
	}
	return info
}

func mergeContentScopeConfig(base, over contentScopeConfig) contentScopeConfig {
	if strings.TrimSpace(over.Mode) != "" {
		base.Mode = over.Mode
	}
	if over.Roots != nil {
		base.Roots = append([]string{}, over.Roots...)
	}
	if over.Exclude != nil {
		base.Exclude = append(append([]string{}, base.Exclude...), over.Exclude...)
	}
	if over.Exts != nil {
		base.Exts = append([]string{}, over.Exts...)
	}
	if over.Git != nil {
		value := *over.Git
		base.Git = &value
	}
	if over.SystemExcludes != nil {
		value := *over.SystemExcludes
		base.SystemExcludes = &value
	}
	if over.BudgetBytes > 0 {
		base.BudgetBytes = over.BudgetBytes
	}
	if over.Worktrees != "" {
		base.Worktrees = over.Worktrees
	}
	if over.Skip != nil {
		base.Skip = append([]string{}, over.Skip...)
	}
	return base
}

func settingsOverrideFromConfig(sc contentScopeConfig) settingsContentOverride {
	out := settingsContentOverride{}
	if strings.TrimSpace(sc.Mode) != "" {
		mode := sc.Mode
		out.Mode = &mode
	}
	if sc.Roots != nil {
		roots := append([]string{}, sc.Roots...)
		out.Roots = &roots
	}
	if sc.Exclude != nil {
		exclude := append([]string{}, sc.Exclude...)
		out.Exclude = &exclude
	}
	if sc.Exts != nil {
		exts := append([]string{}, sc.Exts...)
		out.Exts = &exts
	}
	if sc.Git != nil {
		value := *sc.Git
		out.Git = &value
	}
	if sc.SystemExcludes != nil {
		value := *sc.SystemExcludes
		out.SystemExcludes = &value
	}
	if sc.BudgetBytes > 0 {
		budget := sc.BudgetBytes
		out.BudgetBytes = &budget
	}
	return out
}

func boolValue(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func normalizeSettingsVolume(volume string) string {
	volume = strings.TrimSpace(volume)
	if volume == "" {
		return ""
	}
	return normalizeVolume(volume)
}

func (s *goSearchService) validateSettingsPatch(patch *settingsPatch) error {
	s.indexMu.RLock()
	volumes := append([]*serviceVolumeIndex(nil), s.volumes...)
	s.indexMu.RUnlock()
	known := make(map[string]bool)
	for _, info := range settingsVolumeEnumeration() {
		if volume := normalizeSettingsVolume(info.Volume); volume != "" {
			known[volume] = true
		}
	}
	for _, vol := range volumes {
		if vol != nil {
			if volume := normalizeSettingsVolume(vol.volume); volume != "" {
				known[volume] = true
			}
		}
	}
	for _, entry := range patch.Volumes {
		volume := normalizeSettingsVolume(entry.Volume)
		if volume == "" {
			return fmt.Errorf("volumes: volume is required")
		}
		if !known[volume] {
			return fmt.Errorf("volumes: unknown volume %s", volume)
		}
	}
	if patch.Content == nil {
		return nil
	}
	if err := validateSettingsContentPatch("content", patch.Content, known); err != nil {
		return err
	}
	for volume, override := range patch.Content.Volumes {
		normalized := normalizeSettingsVolume(volume)
		if normalized == "" || !known[normalized] {
			return fmt.Errorf("content.volumes.%s: unknown volume", volume)
		}
		if err := validateSettingsContentOverride("content.volumes."+normalized, override, known); err != nil {
			return err
		}
	}
	return nil
}

func validateSettingsContentPatch(field string, patch *settingsContentPatch, known map[string]bool) error {
	if patch.Mode != nil {
		if err := validateSettingsMode(field+".mode", *patch.Mode); err != nil {
			return err
		}
	}
	if patch.Roots != nil {
		if err := validateSettingsRoots(field+".roots", *patch.Roots, known); err != nil {
			return err
		}
	}
	if patch.BudgetBytes != nil && *patch.BudgetBytes < 0 {
		return fmt.Errorf("%s.budget_bytes: must be >= 0", field)
	}
	return nil
}

func validateSettingsContentOverride(field string, override settingsContentOverride, known map[string]bool) error {
	if override.Mode != nil {
		if err := validateSettingsMode(field+".mode", *override.Mode); err != nil {
			return err
		}
	}
	if override.Roots != nil {
		if err := validateSettingsRoots(field+".roots", *override.Roots, known); err != nil {
			return err
		}
	}
	if override.BudgetBytes != nil && *override.BudgetBytes < 0 {
		return fmt.Errorf("%s.budget_bytes: must be >= 0", field)
	}
	return nil
}

func validateSettingsMode(field, mode string) error {
	if _, ok := parseContentScopeMode(mode); !ok {
		return fmt.Errorf("%s: invalid mode %q (want auto, explicit, or off)", field, mode)
	}
	return nil
}

func validateSettingsRoots(field string, roots []string, known map[string]bool) error {
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("%s: root must be absolute: %s", field, root)
		}
		volume := normalizeSettingsVolume(filepath.VolumeName(root))
		if volume == "" || !known[volume] {
			return fmt.Errorf("%s: unknown volume for root %s", field, root)
		}
	}
	return nil
}

func (s *goSearchService) applySettingsVolumes(entries []settingsVolumePatch) error {
	for _, entry := range entries {
		if entry.Indexed == nil {
			continue
		}
		volume := normalizeSettingsVolume(entry.Volume)
		if *entry.Indexed {
			if err := s.addSettingsVolume(volume); err != nil {
				return err
			}
			continue
		}
		if err := s.removeSettingsVolume(volume); err != nil {
			return err
		}
	}
	return nil
}

func (s *goSearchService) addSettingsVolume(volume string) error {
	s.indexMu.RLock()
	loaded := false
	for _, vol := range s.volumes {
		if vol != nil && strings.EqualFold(normalizeSettingsVolume(vol.volume), volume) {
			loaded = true
			break
		}
	}
	db := s.settingsDBForVolumeLocked(volume)
	s.indexMu.RUnlock()
	if loaded {
		s.settingsVolumeMu.Lock()
		delete(s.settingsVolumeErrs, volume)
		s.settingsVolumeMu.Unlock()
		return nil
	}

	s.settingsVolumeMu.Lock()
	if s.settingsVolumeTasks == nil {
		s.settingsVolumeTasks = make(map[string]*settingsVolumeTask)
	}
	if _, pending := s.settingsVolumeTasks[volume]; pending {
		s.settingsVolumeMu.Unlock()
		return nil
	}
	task := &settingsVolumeTask{
		volume:   volume,
		db:       db,
		cancel:   make(chan struct{}),
		finished: make(chan struct{}),
		progress: newVolumeBuildProgress(),
	}
	s.settingsVolumeTasks[volume] = task
	delete(s.settingsVolumeErrs, volume)
	s.settingsVolumeMu.Unlock()

	go s.runSettingsVolumeBuild(task)
	go func() {
		if s.stop == nil {
			return
		}
		select {
		case <-s.stop:
			s.cancelSettingsVolumeBuild(task)
		case <-task.finished:
		}
	}()
	return nil
}

func (s *goSearchService) runSettingsVolumeBuild(task *settingsVolumeTask) {
	select {
	case <-task.cancel:
		s.finishSettingsVolumeTask(task)
		return
	default:
	}

	// Whether the build succeeds or fails, the in-memory Index and its MFT
	// tables are unreachable once it returns. Drop them and give the pages back
	// to the OS so a 2 GB build peak does not stay resident in lowmem mode.
	defer releaseBuildMemoryAfterSettings(task.volume)

	idx, err := s.buildIndexUSNVolume(task.volume, task.db, task.progress)

	s.settingsVolumeMu.Lock()
	current := s.settingsVolumeTasks[task.volume] == task
	canceled := task.stopped()
	if !current || canceled {
		delete(s.settingsVolumeTasks, task.volume)
		s.settingsVolumeMu.Unlock()
		if idx != nil {
			// The build installed the volume after the remove; drop it.
			s.removeSettingsVolumeMemory(task.volume)
		}
		s.finishSettingsVolumeTask(task)
		return
	}
	delete(s.settingsVolumeTasks, task.volume)
	if err != nil {
		if s.settingsVolumeErrs == nil {
			s.settingsVolumeErrs = make(map[string]string)
		}
		s.settingsVolumeErrs[task.volume] = settingsVolumeErrorMessage(err.Error(), serviceProcessElevated())
		s.settingsVolumeMu.Unlock()
		s.finishSettingsVolumeTask(task)
		return
	}
	s.settingsVolumeMu.Unlock()

	s.indexMu.Lock()
	if !settingsContainsDB(s.dbs, task.db) {
		s.dbs = append(s.dbs, task.db)
	}
	s.indexMu.Unlock()
	if writeErr := s.writeSettingsVolumeConfig([]string{task.volume}, nil, nil); writeErr != nil {
		serviceLog("settings volume config volume=%s err=%v", task.volume, writeErr)
	}
	s.finishSettingsVolumeTask(task)
}

// serviceProcessElevated is a test hook for currentProcessElevated.
var serviceProcessElevated = currentProcessElevated

// settingsVolumeErrorMessage is the per-drive message the apps show. An
// unelevated service cannot open the raw volume, so say that instead of a
// bare "Access is denied".
func settingsVolumeErrorMessage(reason string, elevated bool) string {
	if elevated || reason == "" {
		return reason
	}
	return "The seekfs service is not running as administrator, so it cannot read this drive (" + reason + ")"
}

// finishSettingsVolumeTask releases the service-stop watcher.
func (s *goSearchService) finishSettingsVolumeTask(task *settingsVolumeTask) {
	select {
	case <-task.finished:
	default:
		close(task.finished)
	}
}

func (s *goSearchService) cancelSettingsVolumeBuild(task *settingsVolumeTask) {
	s.settingsVolumeMu.Lock()
	if s.settingsVolumeTasks[task.volume] == task {
		delete(s.settingsVolumeTasks, task.volume)
	}
	s.settingsVolumeMu.Unlock()
	task.cancelNow()
}

func (s *goSearchService) settingsDBForVolumeLocked(volume string) string {
	for _, vol := range s.volumes {
		if vol != nil && strings.EqualFold(normalizeSettingsVolume(vol.volume), volume) {
			return vol.dbPath
		}
	}
	return defaultVolumeDB(defaultIndexDir(), volume)
}

func settingsContainsDB(dbs []string, db string) bool {
	for _, existing := range dbs {
		if samePath(existing, db) {
			return true
		}
	}
	return false
}

func (s *goSearchService) removeSettingsVolume(volume string) error {
	s.settingsVolumeMu.Lock()
	if task := s.settingsVolumeTasks[volume]; task != nil {
		delete(s.settingsVolumeTasks, volume)
		task.cancelNow()
	}
	delete(s.settingsVolumeErrs, volume)
	s.settingsVolumeMu.Unlock()

	dbPath, removed := s.removeSettingsVolumeMemory(volume)
	if !removed {
		// A pending build was only canceled: its config was never written.
		return nil
	}
	return s.writeSettingsVolumeConfig(nil, []string{volume}, []string{dbPath})
}

func (s *goSearchService) removeSettingsVolumeMemory(volume string) (string, bool) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	removeAt := -1
	dbPath := ""
	for i, vol := range s.volumes {
		if vol != nil && strings.EqualFold(normalizeSettingsVolume(vol.volume), volume) {
			removeAt = i
			dbPath = vol.dbPath
			if activation := vol.contentActivation.Load(); activation != nil && activation.cancel != nil {
				activation.cancel()
			}
			if err := closeIndexMMapRecords(vol.index); err != nil {
				serviceLog("settings remove close mmap volume=%s err=%v", volume, err)
			}
			break
		}
	}
	if removeAt < 0 {
		return "", false
	}
	s.volumes = append(s.volumes[:removeAt], s.volumes[removeAt+1:]...)
	if removeAt < len(s.indexes) {
		s.indexes = append(s.indexes[:removeAt], s.indexes[removeAt+1:]...)
	}
	if dbPath != "" {
		for i, db := range s.dbs {
			if samePath(db, dbPath) {
				s.dbs = append(s.dbs[:i], s.dbs[i+1:]...)
				break
			}
		}
	}
	return dbPath, true
}

func (s *goSearchService) writeSettingsVolumeConfig(add, remove, removeDBs []string) error {
	path := s.settingsConfigPath()
	cfg := s.contentCfg
	if _, err := os.Stat(path); err == nil {
		loaded, loadErr := loadConfig(path)
		if loadErr != nil {
			return loadErr
		}
		cfg = loaded
	}
	volumes := append([]string(nil), cfg.Volumes...)
	dbs := append([]string(nil), cfg.DBs...)
	for _, volume := range add {
		if !settingsContainsVolume(volumes, volume) {
			volumes = append(volumes, volume)
		}
		db := defaultVolumeDB(defaultIndexDir(), volume)
		if !settingsContainsDB(dbs, db) {
			dbs = append(dbs, db)
		}
	}
	for _, volume := range remove {
		volumes = settingsRemoveVolume(volumes, volume)
	}
	for _, db := range removeDBs {
		dbs = settingsRemoveDB(dbs, db)
	}
	if err := setConfigKey(path, "volumes", formatStringArray(volumes)); err != nil {
		return err
	}
	if err := setConfigKey(path, "dbs", formatStringArray(dbs)); err != nil {
		return err
	}
	return nil
}

func settingsContainsVolume(volumes []string, volume string) bool {
	for _, existing := range volumes {
		if strings.EqualFold(normalizeSettingsVolume(existing), volume) {
			return true
		}
	}
	return false
}

func settingsRemoveVolume(volumes []string, volume string) []string {
	out := volumes[:0]
	for _, existing := range volumes {
		if strings.EqualFold(normalizeSettingsVolume(existing), volume) {
			continue
		}
		out = append(out, existing)
	}
	return out
}

func settingsRemoveDB(dbs []string, db string) []string {
	if db == "" {
		return dbs
	}
	out := dbs[:0]
	for _, existing := range dbs {
		if samePath(existing, db) {
			continue
		}
		out = append(out, existing)
	}
	return out
}

func (s *goSearchService) applySettingsContent(patch *settingsContentPatch) error {
	pluginPath := s.pluginFilePath()
	hasScope := patch.Mode != nil || patch.Roots != nil || patch.Exclude != nil ||
		patch.Exts != nil || patch.Git != nil || patch.SystemExcludes != nil || patch.BudgetBytes != nil

	defs, err := loadPluginDefinitions(pluginPath)
	if err != nil {
		return err
	}
	_, hasContentPlugin := defs["content"]

	if hasScope {
		if err := updatePluginFile(pluginPath, "content", func(d pluginDefinition, exists bool) (pluginDefinition, error) {
			if !exists {
				d.Installed = true
				d.Feature.Enabled = true
			}
			if patch.Mode != nil {
				d.Content.Mode = *patch.Mode
			}
			if patch.Roots != nil {
				d.Content.Roots = append([]string{}, (*patch.Roots)...)
			}
			if patch.Exclude != nil {
				d.Content.Exclude = append([]string{}, (*patch.Exclude)...)
			}
			if patch.Exts != nil {
				d.Content.Exts = append([]string{}, (*patch.Exts)...)
			}
			if patch.Git != nil {
				value := *patch.Git
				d.Content.Git = &value
			}
			if patch.SystemExcludes != nil {
				value := *patch.SystemExcludes
				d.Content.SystemExcludes = &value
			}
			if patch.BudgetBytes != nil {
				d.Content.BudgetBytes = *patch.BudgetBytes
			}
			if patch.Enabled != nil {
				d.Feature.Enabled = *patch.Enabled
			}
			return d, nil
		}); err != nil {
			return err
		}
	} else if patch.Enabled != nil {
		if hasContentPlugin {
			if err := updatePluginFile(pluginPath, "content", func(d pluginDefinition, _ bool) (pluginDefinition, error) {
				d.Feature.Enabled = *patch.Enabled
				return d, nil
			}); err != nil {
				return err
			}
		} else if err := s.writeSettingsContentEnabled(*patch.Enabled); err != nil {
			return err
		}
	}

	perVolumeChanged := false
	if len(patch.Volumes) > 0 {
		if err := s.writeSettingsContentOverrides(patch.Volumes); err != nil {
			return err
		}
		perVolumeChanged = true
	}

	defs, err = loadPluginDefinitions(pluginPath)
	if err != nil {
		return err
	}
	if perVolumeChanged {
		s.contentForceRestart.Store(true)
	}
	if err := s.queueSettingsPluginReload(defs); err != nil {
		return err
	}
	return nil
}

func (s *goSearchService) writeSettingsContentEnabled(enabled bool) error {
	path := s.settingsConfigPath()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	replacement := fmt.Sprintf("[features.content]\nenabled = %t\n", enabled)
	text := replaceSettingsSection(string(data), "[features.content]", replacement)
	if err := writeFileAtomic(path, []byte(text)); err != nil {
		return err
	}
	if s.contentCfg.Features == nil {
		s.contentCfg.Features = make(map[string]featureConfig)
	}
	feature := s.contentCfg.Features["content"]
	feature.Enabled = enabled
	s.contentCfg.Features["content"] = feature
	return nil
}

func (s *goSearchService) writeSettingsContentOverrides(overrides map[string]settingsContentOverride) error {
	path := s.settingsConfigPath()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(data)
	if s.contentCfg.ContentByVolume == nil {
		s.contentCfg.ContentByVolume = make(map[string]contentScopeConfig)
	}
	for volume, override := range overrides {
		normalized := normalizeSettingsVolume(volume)
		merged := s.contentCfg.ContentByVolume[normalized]
		applySettingsContentOverride(&merged, override)
		s.contentCfg.ContentByVolume[normalized] = merged
		header := `[content."` + normalized + `"]`
		text = replaceSettingsSection(text, header, formatSettingsContentSection(header, merged))
	}
	return writeFileAtomic(path, []byte(text))
}

// writeFileAtomic writes data to a temp file in the target directory, fsyncs
// it, then replaces the target with a write-through rename.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	from, err := windows.UTF16PtrFromString(tmpName)
	if err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func applySettingsContentOverride(sc *contentScopeConfig, override settingsContentOverride) {
	if override.Mode != nil {
		sc.Mode = *override.Mode
	}
	if override.Roots != nil {
		sc.Roots = append([]string{}, (*override.Roots)...)
	}
	if override.Exclude != nil {
		sc.Exclude = append([]string{}, (*override.Exclude)...)
	}
	if override.Exts != nil {
		sc.Exts = append([]string{}, (*override.Exts)...)
	}
	if override.Git != nil {
		value := *override.Git
		sc.Git = &value
	}
	if override.SystemExcludes != nil {
		value := *override.SystemExcludes
		sc.SystemExcludes = &value
	}
	if override.BudgetBytes != nil {
		sc.BudgetBytes = *override.BudgetBytes
	}
}

func formatSettingsContentSection(header string, sc contentScopeConfig) string {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	if sc.Mode != "" {
		fmt.Fprintf(&b, "mode = %s\n", strconvQuote(sc.Mode))
	}
	if sc.Roots != nil {
		fmt.Fprintf(&b, "roots = %s\n", formatStringArray(sc.Roots))
	}
	if sc.Exclude != nil {
		fmt.Fprintf(&b, "exclude = %s\n", formatStringArray(sc.Exclude))
	}
	if sc.Exts != nil {
		fmt.Fprintf(&b, "exts = %s\n", formatStringArray(sc.Exts))
	}
	if sc.Git != nil {
		fmt.Fprintf(&b, "git = %t\n", *sc.Git)
	}
	if sc.SystemExcludes != nil {
		fmt.Fprintf(&b, "system_excludes = %t\n", *sc.SystemExcludes)
	}
	if sc.BudgetBytes > 0 {
		fmt.Fprintf(&b, "budget_bytes = %d\n", sc.BudgetBytes)
	}
	if sc.Worktrees != "" {
		fmt.Fprintf(&b, "worktrees = %s\n", strconvQuote(sc.Worktrees))
	}
	if sc.Skip != nil {
		fmt.Fprintf(&b, "skip = %s\n", formatStringArray(sc.Skip))
	}
	return b.String()
}

// replaceSettingsSection replaces one TOML table (by exact header line) or
// appends it when absent. It splices the original text, so every unrelated
// byte (other keys, comments, line endings) stays unchanged.
func replaceSettingsSection(text, header, replacement string) string {
	newline := "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}
	middle := strings.TrimRight(strings.ReplaceAll(replacement, "\r\n", "\n"), "\n")
	if newline != "\n" {
		middle = strings.ReplaceAll(middle, "\n", newline)
	}
	middle += newline

	start, end, lastContentEnd := -1, -1, -1
	for offset := 0; offset <= len(text); {
		lineEnd := strings.IndexByte(text[offset:], '\n')
		next := len(text)
		line := text[offset:]
		if lineEnd >= 0 {
			line = text[offset : offset+lineEnd]
			next = offset + lineEnd + 1
		}
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		isHeader := strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")
		if isHeader {
			if start >= 0 {
				end = lastContentEnd
				break
			}
			if trimmed == header {
				start = offset
				lastContentEnd = next
			}
		} else if start >= 0 && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			lastContentEnd = next
		}
		if lineEnd < 0 {
			break
		}
		offset = next
	}
	if start < 0 {
		base := text
		if base != "" {
			if !strings.HasSuffix(base, "\n") {
				base += newline
			}
			if !strings.HasSuffix(base, newline+newline) {
				base += newline
			}
		}
		return base + middle
	}
	if end < 0 {
		end = lastContentEnd
		if end < start {
			end = len(text)
		}
	}
	return text[:start] + middle + text[end:]
}

func (s *goSearchService) queueSettingsPluginReload(defs map[string]pluginDefinition) error {
	if s.serviceStopping() {
		return fmt.Errorf("service stopping")
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
	return nil
}

func cmdSettings(args []string) error {
	if len(args) == 0 {
		return errors.New("settings requires get or set")
	}
	switch args[0] {
	case "get":
		return runSettingsGet(args[1:], os.Stdout)
	case "set":
		return runSettingsSet(args[1:], os.Stdout)
	case "help", "-h", "--help":
		fmt.Fprintln(os.Stdout, "seekfs settings get [--json]\nseekfs settings set --json <file> [--install-service]")
		return nil
	default:
		return fmt.Errorf("unknown settings command %q; use get or set", args[0])
	}
}

func runSettingsGet(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("settings get", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "seekfs.toml path")
	pipe := fs.String("pipe", defaultServicePipe, "resident service pipe")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected settings get arguments")
	}
	if err := settingsApplyPipeConfig(fs, configPath, pipe); err != nil {
		return err
	}
	resp, err := callService(*pipe, serviceRequest{Command: "settings-get"})
	if err != nil {
		return err
	}
	if !resp.OK {
		return serviceResponseError(resp)
	}
	if resp.Settings == nil {
		return errors.New("service returned no settings")
	}
	if *jsonOut {
		return json.NewEncoder(out).Encode(resp.Settings)
	}
	printSettings(out, *resp.Settings)
	return nil
}

func runSettingsSet(args []string, out io.Writer) (err error) {
	var resp serviceResponse
	fs := flag.NewFlagSet("settings set", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "seekfs.toml path")
	pipe := fs.String("pipe", defaultServicePipe, "resident service pipe")
	jsonPath := fs.String("json", "", "settings JSON file")
	resultPath := fs.String("result", "", "write the service response JSON here (for the elevated app helper)")
	progressPath := fs.String("progress", "", "append JSON progress lines here (for the elevated app helper)")
	installService := fs.Bool("install-service", false, "first install and start seekfs as an automatic Windows service on --pipe (elevated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *resultPath != "" {
		// The desktop app runs this command elevated and cannot read its
		// stderr, so record the outcome (including a validation message).
		defer func() {
			_ = writeSettingsResult(*resultPath, resp, err)
		}()
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected settings set arguments")
	}
	if *jsonPath == "" {
		return errors.New("settings set requires --json <file>")
	}
	if err := settingsApplyPipeConfig(fs, configPath, pipe); err != nil {
		return err
	}
	data, err := os.ReadFile(*jsonPath)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("settings JSON: %w", err)
	}
	progress := newSettingsProgress(*progressPath)
	progress.report(progressPhaseStart)
	if *installService {
		if err := settingsInstallService(*pipe, progress); err != nil {
			return err
		}
	} else if err := ensureSettingsServiceElevated(*pipe, raw, progress); err != nil {
		return err
	}
	progress.report(progressPhaseApply)
	resp, err = callService(*pipe, serviceRequest{Command: "settings-set", Settings: raw})
	if err != nil {
		return err
	}
	if !resp.OK {
		return serviceResponseError(resp)
	}
	if resp.Settings == nil {
		return errors.New("service returned no settings")
	}
	return json.NewEncoder(out).Encode(resp.Settings)
}

// writeSettingsResult records a settings set outcome as {ok, message, settings}.
func writeSettingsResult(path string, resp serviceResponse, err error) error {
	out := struct {
		OK       bool             `json:"ok"`
		Message  string           `json:"message,omitempty"`
		Settings *serviceSettings `json:"settings,omitempty"`
	}{OK: err == nil, Settings: resp.Settings}
	if err != nil {
		out.Message = err.Error()
	}
	data, mErr := json.Marshal(out)
	if mErr != nil {
		return mErr
	}
	return os.WriteFile(path, data, 0o600)
}

func settingsApplyPipeConfig(fs *flag.FlagSet, configPath, pipe *string) error {
	changed := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { changed[f.Name] = true })
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if !changed["pipe"] && cfg.ServicePipe != "" {
		*pipe = cfg.ServicePipe
	}
	return nil
}

func printSettings(out io.Writer, settings serviceSettings) {
	for _, volume := range settings.Volumes {
		state := volume.State
		if state == "" {
			state = "not_indexed"
		}
		fmt.Fprintf(out, "%s indexed=%t state=%s fs=%s entries=%d\n", volume.Volume, volume.Indexed, state, volume.FS, volume.Entries)
	}
	content := settings.Content
	fmt.Fprintf(out, "content enabled=%t mode=%s roots=%s exclude=%s budget_bytes=%d\n", content.Enabled, content.Mode, formatStringArray(content.Roots), formatStringArray(content.Exclude), content.BudgetBytes)
}
