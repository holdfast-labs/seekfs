package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// settingsmodel.go: the pure-Go settings model behind the Settings panel.
// It mirrors docs/SETTINGS_API.md, stages edits against the last snapshot,
// and encodes only the fields that changed.

const (
	contentModeAuto     = "auto"
	contentModeExplicit = "explicit"
	contentModeOff      = "off"

	contentSpeedBalanced = "balanced"
	contentSpeedFast     = "fast"
	contentSpeedGentle   = "gentle"

	gbBytes = int64(1) << 30

	serviceModeWindowsService = "windows-service"
)

// serviceSettings is the "settings" object in settings-get/settings-set.
type serviceSettings struct {
	CanWrite bool `json:"can_write,omitempty"`
	// ServiceMode is read-only: "windows-service" when the installed
	// service answers, "standalone" for a sibling service.
	ServiceMode string          `json:"service_mode,omitempty"`
	Volumes     []serviceVolume `json:"volumes,omitempty"`
	Content     *serviceContent `json:"content,omitempty"`
}

type serviceVolume struct {
	Volume     string                 `json:"volume"`
	Indexed    *bool                  `json:"indexed,omitempty"`
	FS         string                 `json:"fs,omitempty"`
	Label      string                 `json:"label,omitempty"`
	TotalBytes int64                  `json:"total_bytes,omitempty"`
	FreeBytes  int64                  `json:"free_bytes,omitempty"`
	Entries    int64                  `json:"entries,omitempty"`
	State      string                 `json:"state,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Progress   *serviceVolumeProgress `json:"progress,omitempty"`
}

type serviceVolumeProgress struct {
	Phase     string `json:"phase"`
	Done      int64  `json:"done"`
	Total     int64  `json:"total"`
	StartedAt string `json:"started_at"`
}

// serviceContent mirrors [content] plus per-drive overrides. Every field is a
// pointer so a staged change can leave untouched keys out of the request.
type serviceContent struct {
	Enabled        *bool                           `json:"enabled,omitempty"`
	Mode           *string                         `json:"mode,omitempty"`
	Roots          *[]string                       `json:"roots,omitempty"`
	Exclude        *[]string                       `json:"exclude,omitempty"`
	Exts           *[]string                       `json:"exts,omitempty"`
	Git            *bool                           `json:"git,omitempty"`
	SystemExcludes *bool                           `json:"system_excludes,omitempty"`
	BudgetBytes    *int64                          `json:"budget_bytes,omitempty"`
	BuildSpeed     *string                         `json:"build_speed,omitempty"`
	Volumes        map[string]serviceContentVolume `json:"volumes,omitempty"`
}

type serviceContentVolume struct {
	Mode           *string   `json:"mode,omitempty"`
	Roots          *[]string `json:"roots,omitempty"`
	Exclude        *[]string `json:"exclude,omitempty"`
	Exts           *[]string `json:"exts,omitempty"`
	Git            *bool     `json:"git,omitempty"`
	SystemExcludes *bool     `json:"system_excludes,omitempty"`
	BudgetBytes    *int64    `json:"budget_bytes,omitempty"`
}

type volumeDraft struct {
	Volume     string
	Indexed    bool
	FS         string
	Label      string
	State      string
	TotalBytes int64
	FreeBytes  int64
	Entries    int64
	Error      string
	Progress   *serviceVolumeProgress
}

type contentDraft struct {
	Enabled        bool
	Mode           string
	Roots          []string
	Exclude        []string
	Exts           []string
	Git            bool
	SystemExcludes bool
	BudgetBytes    int64
	BuildSpeed     string
	Volumes        map[string]*contentVolumeDraft
}

// contentVolumeDraft keeps the decoded values plus a set mask. The mask
// records which keys existed in the service snapshot or received an edit, so
// the patch can stay narrow.
type contentVolumeDraft struct {
	Mode           string
	Roots          []string
	Exclude        []string
	Exts           []string
	Git            bool
	SystemExcludes bool
	BudgetBytes    int64

	HasMode           bool
	HasRoots          bool
	HasExclude        bool
	HasExts           bool
	HasGit            bool
	HasSystemExcludes bool
	HasBudget         bool
}

type settingsDraft struct {
	Volumes []volumeDraft
	Content contentDraft
}

// contentValues is the effective display value for "All drives" or one drive.
type contentValues struct {
	Enabled        bool
	Mode           string
	Roots          []string
	Exclude        []string
	Exts           []string
	Git            bool
	SystemExcludes bool
	BudgetBytes    int64
	BuildSpeed     string
	HasOverride    bool
}

type settingsModel struct {
	canWrite    bool
	serviceMode string
	base        settingsDraft
	draft       settingsDraft
	history     []settingsDraft
}

func newSettingsModel(s serviceSettings) *settingsModel {
	m := &settingsModel{canWrite: s.CanWrite, serviceMode: s.ServiceMode}
	m.base = draftFromService(s)
	m.draft = m.base.clone()
	return m
}

func draftFromService(s serviceSettings) settingsDraft {
	d := settingsDraft{Content: contentDraft{
		Mode:       contentModeAuto,
		BuildSpeed: contentSpeedBalanced,
		Volumes:    map[string]*contentVolumeDraft{},
	}}
	for _, v := range s.Volumes {
		indexed := false
		if v.Indexed != nil {
			indexed = *v.Indexed
		}
		d.Volumes = append(d.Volumes, volumeDraft{
			Volume:     v.Volume,
			Indexed:    indexed,
			FS:         v.FS,
			Label:      v.Label,
			State:      v.State,
			TotalBytes: v.TotalBytes,
			FreeBytes:  v.FreeBytes,
			Entries:    v.Entries,
			Error:      v.Error,
			Progress:   v.Progress,
		})
	}
	if c := s.Content; c != nil {
		if c.Enabled != nil {
			d.Content.Enabled = *c.Enabled
		}
		if c.Mode != nil && *c.Mode != "" {
			d.Content.Mode = *c.Mode
		}
		if c.Roots != nil {
			d.Content.Roots = append([]string(nil), (*c.Roots)...)
		}
		if c.Exclude != nil {
			d.Content.Exclude = append([]string(nil), (*c.Exclude)...)
		}
		if c.Exts != nil {
			d.Content.Exts = append([]string(nil), (*c.Exts)...)
		}
		if c.Git != nil {
			d.Content.Git = *c.Git
		}
		if c.SystemExcludes != nil {
			d.Content.SystemExcludes = *c.SystemExcludes
		}
		if c.BudgetBytes != nil {
			d.Content.BudgetBytes = *c.BudgetBytes
		}
		if c.BuildSpeed != nil && *c.BuildSpeed != "" {
			d.Content.BuildSpeed = *c.BuildSpeed
		}
		for drive, ov := range c.Volumes {
			d.Content.Volumes[drive] = contentVolumeFromService(ov)
		}
	}
	return d
}

func contentVolumeFromService(ov serviceContentVolume) *contentVolumeDraft {
	d := &contentVolumeDraft{}
	if ov.Mode != nil {
		d.Mode, d.HasMode = *ov.Mode, true
	}
	if ov.Roots != nil {
		d.Roots, d.HasRoots = append([]string(nil), (*ov.Roots)...), true
	}
	if ov.Exclude != nil {
		d.Exclude, d.HasExclude = append([]string(nil), (*ov.Exclude)...), true
	}
	if ov.Exts != nil {
		d.Exts, d.HasExts = append([]string(nil), (*ov.Exts)...), true
	}
	if ov.Git != nil {
		d.Git, d.HasGit = *ov.Git, true
	}
	if ov.SystemExcludes != nil {
		d.SystemExcludes, d.HasSystemExcludes = *ov.SystemExcludes, true
	}
	if ov.BudgetBytes != nil {
		d.BudgetBytes, d.HasBudget = *ov.BudgetBytes, true
	}
	return d
}

// mergeServerState refreshes server-side volume state, entries and progress
// while keeping the user's staged (unapplied) edits. Content edits are kept
// as staged too: only the snapshot used to compute the patch is updated.
func (m *settingsModel) mergeServerState(s serviceSettings) {
	if m == nil {
		return
	}
	m.canWrite = s.CanWrite
	m.serviceMode = s.ServiceMode
	next := draftFromService(s)
	merged := make([]volumeDraft, 0, len(next.Volumes))
	for _, nv := range next.Volumes {
		for i := range m.draft.Volumes {
			if m.draft.Volumes[i].Volume == nv.Volume {
				nv.Indexed = m.draft.Volumes[i].Indexed
				break
			}
		}
		merged = append(merged, nv)
	}
	m.base = next
	m.draft.Volumes = merged
}

func hasIndexingVolume(s *serviceSettings) bool {
	if s == nil {
		return false
	}
	for _, v := range s.Volumes {
		if v.State == "indexing" {
			return true
		}
	}
	return false
}

func (d settingsDraft) clone() settingsDraft {
	out := settingsDraft{
		Volumes: append([]volumeDraft(nil), d.Volumes...),
		Content: contentDraft{
			Enabled:        d.Content.Enabled,
			Mode:           d.Content.Mode,
			Roots:          append([]string(nil), d.Content.Roots...),
			Exclude:        append([]string(nil), d.Content.Exclude...),
			Exts:           append([]string(nil), d.Content.Exts...),
			Git:            d.Content.Git,
			SystemExcludes: d.Content.SystemExcludes,
			BudgetBytes:    d.Content.BudgetBytes,
			BuildSpeed:     d.Content.BuildSpeed,
			Volumes:        map[string]*contentVolumeDraft{},
		},
	}
	for drive, ov := range d.Content.Volumes {
		cp := *ov
		cp.Roots = append([]string(nil), ov.Roots...)
		cp.Exclude = append([]string(nil), ov.Exclude...)
		cp.Exts = append([]string(nil), ov.Exts...)
		out.Content.Volumes[drive] = &cp
	}
	return out
}

func (d contentVolumeDraft) clone() *contentVolumeDraft {
	cp := d
	cp.Roots = append([]string(nil), d.Roots...)
	cp.Exclude = append([]string(nil), d.Exclude...)
	cp.Exts = append([]string(nil), d.Exts...)
	return &cp
}

// snapshot records the state before a staged change. Undo restores it.
func (m *settingsModel) snapshot() {
	m.history = append(m.history, m.draft.clone())
	if len(m.history) > 64 {
		m.history = m.history[len(m.history)-64:]
	}
}

func (m *settingsModel) undo() bool {
	n := len(m.history)
	if n == 0 {
		return false
	}
	m.draft = m.history[n-1]
	m.history = m.history[:n-1]
	return true
}

func (m *settingsModel) reset() {
	m.draft = m.base.clone()
	m.history = nil
}

func (m *settingsModel) hasChanges() bool { return len(m.patch().allFields()) > 0 }

// setDriveIndexed stages an indexed/unindexed change for one volume.
// serviceInstalled reports that the installed Windows service answers.
func (m *settingsModel) serviceInstalled() bool {
	return m.serviceMode == serviceModeWindowsService
}

// offerServiceInstallByDefault pre-checks "Install the seekfs service" on
// first run: no installed service and no indexed drive yet.
func (m *settingsModel) offerServiceInstallByDefault() bool {
	if m.serviceInstalled() {
		return false
	}
	for _, v := range m.base.Volumes {
		if v.Indexed {
			return false
		}
	}
	return true
}

func (m *settingsModel) setDriveIndexed(volume string, indexed bool) {
	for i := range m.draft.Volumes {
		if m.draft.Volumes[i].Volume != volume {
			continue
		}
		if m.draft.Volumes[i].Indexed == indexed {
			return
		}
		m.snapshot()
		m.draft.Volumes[i].Indexed = indexed
		return
	}
}

func (m *settingsModel) drive(volume string) *volumeDraft {
	for i := range m.draft.Volumes {
		if m.draft.Volumes[i].Volume == volume {
			return &m.draft.Volumes[i]
		}
	}
	return nil
}

func (m *settingsModel) contentValues(drive string) contentValues {
	g := m.draft.Content
	v := contentValues{
		Enabled:        g.Enabled,
		Mode:           g.Mode,
		Roots:          append([]string(nil), g.Roots...),
		Exclude:        append([]string(nil), g.Exclude...),
		Exts:           append([]string(nil), g.Exts...),
		Git:            g.Git,
		SystemExcludes: g.SystemExcludes,
		BudgetBytes:    g.BudgetBytes,
		BuildSpeed:     g.BuildSpeed,
	}
	if drive == "" {
		return v
	}
	ov := g.Volumes[drive]
	if ov == nil {
		return v
	}
	v.HasOverride = true
	if ov.HasMode {
		v.Mode = ov.Mode
	}
	if ov.HasRoots {
		v.Roots = append([]string(nil), ov.Roots...)
	}
	if ov.HasExclude {
		v.Exclude = append([]string(nil), ov.Exclude...)
	}
	if ov.HasExts {
		v.Exts = append([]string(nil), ov.Exts...)
	}
	if ov.HasGit {
		v.Git = ov.Git
	}
	if ov.HasSystemExcludes {
		v.SystemExcludes = ov.SystemExcludes
	}
	if ov.HasBudget {
		v.BudgetBytes = ov.BudgetBytes
	}
	return v
}

func (m *settingsModel) hasOverride(drive string) bool {
	if drive == "" {
		return false
	}
	return m.draft.Content.Volumes[drive] != nil
}

// ensureOverride stages a per-drive override seeded from the global values.
// Editing methods also call it when a drive is selected.
func (m *settingsModel) ensureOverride(drive string) *contentVolumeDraft {
	if drive == "" {
		return nil
	}
	if ov := m.draft.Content.Volumes[drive]; ov != nil {
		return ov
	}
	m.snapshot()
	return m.ensureOverrideNoSnapshot(drive)
}

func (m *settingsModel) ensureOverrideNoSnapshot(drive string) *contentVolumeDraft {
	if ov := m.draft.Content.Volumes[drive]; ov != nil {
		return ov
	}
	ov := &contentVolumeDraft{
		Mode:           m.draft.Content.Mode,
		Roots:          append([]string(nil), m.draft.Content.Roots...),
		Exclude:        append([]string(nil), m.draft.Content.Exclude...),
		Exts:           append([]string(nil), m.draft.Content.Exts...),
		Git:            m.draft.Content.Git,
		SystemExcludes: m.draft.Content.SystemExcludes,
		BudgetBytes:    m.draft.Content.BudgetBytes,
	}
	m.draft.Content.Volumes[drive] = ov
	return ov
}

func (m *settingsModel) removeOverride(drive string) {
	if _, ok := m.draft.Content.Volumes[drive]; !ok {
		return
	}
	m.snapshot()
	delete(m.draft.Content.Volumes, drive)
}

// targetContent returns the draft the next edit applies to. For drive "" the
// global content block is edited; otherwise a per-drive override is created.
func (m *settingsModel) targetContent(drive string, allowCreate bool) *contentVolumeDraft {
	if drive == "" {
		return nil
	}
	if ov := m.draft.Content.Volumes[drive]; ov != nil {
		return ov
	}
	if !allowCreate {
		return nil
	}
	return m.ensureOverrideNoSnapshot(drive)
}

func (m *settingsModel) setContentEnabled(on bool) {
	if m.draft.Content.Enabled == on {
		return
	}
	m.snapshot()
	m.draft.Content.Enabled = on
}

func (m *settingsModel) setContentMode(drive, mode string) {
	if drive == "" {
		if m.draft.Content.Mode == mode {
			return
		}
		m.snapshot()
		m.draft.Content.Mode = mode
		return
	}
	m.snapshot()
	ov := m.targetContent(drive, true)
	ov.Mode, ov.HasMode = mode, true
}

func (m *settingsModel) addRoot(drive, path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if drive == "" {
		m.snapshot()
		m.draft.Content.Roots = appendUnique(m.draft.Content.Roots, path)
		return
	}
	m.snapshot()
	ov := m.targetContent(drive, true)
	ov.Roots, ov.HasRoots = appendUnique(ov.Roots, path), true
}

func (m *settingsModel) removeRoot(drive string, index int) {
	if drive == "" {
		if index < 0 || index >= len(m.draft.Content.Roots) {
			return
		}
		m.snapshot()
		m.draft.Content.Roots = removeAt(m.draft.Content.Roots, index)
		return
	}
	ov := m.targetContent(drive, false)
	if ov == nil || index < 0 || index >= len(ov.Roots) {
		return
	}
	m.snapshot()
	ov.Roots, ov.HasRoots = removeAt(ov.Roots, index), true
}

func (m *settingsModel) addExclude(drive, path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if drive == "" {
		m.snapshot()
		m.draft.Content.Exclude = appendUnique(m.draft.Content.Exclude, path)
		return
	}
	m.snapshot()
	ov := m.targetContent(drive, true)
	ov.Exclude, ov.HasExclude = appendUnique(ov.Exclude, path), true
}

func (m *settingsModel) removeExclude(drive string, index int) {
	if drive == "" {
		if index < 0 || index >= len(m.draft.Content.Exclude) {
			return
		}
		m.snapshot()
		m.draft.Content.Exclude = removeAt(m.draft.Content.Exclude, index)
		return
	}
	ov := m.targetContent(drive, false)
	if ov == nil || index < 0 || index >= len(ov.Exclude) {
		return
	}
	m.snapshot()
	ov.Exclude, ov.HasExclude = removeAt(ov.Exclude, index), true
}

func (m *settingsModel) setGit(drive string, on bool) {
	if drive == "" {
		if m.draft.Content.Git == on {
			return
		}
		m.snapshot()
		m.draft.Content.Git = on
		return
	}
	m.snapshot()
	ov := m.targetContent(drive, true)
	ov.Git, ov.HasGit = on, true
}

func (m *settingsModel) setSystemExcludes(drive string, on bool) {
	if drive == "" {
		if m.draft.Content.SystemExcludes == on {
			return
		}
		m.snapshot()
		m.draft.Content.SystemExcludes = on
		return
	}
	m.snapshot()
	ov := m.targetContent(drive, true)
	ov.SystemExcludes, ov.HasSystemExcludes = on, true
}

// setBuildSpeed stages the global indexing-speed policy. There is no
// per-drive override: the policy governs the process-wide worker pool.
func (m *settingsModel) setBuildSpeed(speed string) {
	switch speed {
	case contentSpeedBalanced, contentSpeedFast, contentSpeedGentle:
	default:
		return
	}
	if m.draft.Content.BuildSpeed == speed {
		return
	}
	m.snapshot()
	m.draft.Content.BuildSpeed = speed
}

func (m *settingsModel) setBudget(drive string, bytes int64) {
	if drive == "" {
		if m.draft.Content.BudgetBytes == bytes {
			return
		}
		m.snapshot()
		m.draft.Content.BudgetBytes = bytes
		return
	}
	m.snapshot()
	ov := m.targetContent(drive, true)
	ov.BudgetBytes, ov.HasBudget = bytes, true
}

func (m *settingsModel) setExts(drive string, exts []string) {
	exts = append([]string(nil), exts...)
	if drive == "" {
		if equalStrings(m.draft.Content.Exts, exts) {
			return
		}
		m.snapshot()
		m.draft.Content.Exts = exts
		return
	}
	m.snapshot()
	ov := m.targetContent(drive, true)
	ov.Exts, ov.HasExts = exts, true
}

func appendUnique(list []string, v string) []string {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return list
		}
	}
	return append(list, v)
}

func removeAt(list []string, i int) []string {
	out := make([]string, 0, len(list)-1)
	out = append(out, list[:i]...)
	return append(out, list[i+1:]...)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// patch builds the settings-set body. It returns nil when nothing changed.
func (m *settingsModel) patch() *serviceSettings {
	p := &serviceSettings{}
	for i := range m.draft.Volumes {
		dv := &m.draft.Volumes[i]
		bv := m.baseVolume(dv.Volume)
		if bv == nil {
			indexed := dv.Indexed
			p.Volumes = append(p.Volumes, serviceVolume{Volume: dv.Volume, Indexed: &indexed})
			continue
		}
		if bv.Indexed != dv.Indexed {
			indexed := dv.Indexed
			p.Volumes = append(p.Volumes, serviceVolume{Volume: dv.Volume, Indexed: &indexed})
		}
	}

	b, d := m.base.Content, m.draft.Content
	c := &serviceContent{}
	changed := false
	if d.Enabled != b.Enabled {
		v := d.Enabled
		c.Enabled, changed = &v, true
	}
	if d.Mode != b.Mode {
		v := d.Mode
		c.Mode, changed = &v, true
	}
	if !equalStrings(d.Roots, b.Roots) {
		v := append([]string(nil), d.Roots...)
		c.Roots, changed = &v, true
	}
	if !equalStrings(d.Exclude, b.Exclude) {
		v := append([]string(nil), d.Exclude...)
		c.Exclude, changed = &v, true
	}
	if !equalStrings(d.Exts, b.Exts) {
		v := append([]string(nil), d.Exts...)
		c.Exts, changed = &v, true
	}
	if d.Git != b.Git {
		v := d.Git
		c.Git, changed = &v, true
	}
	if d.SystemExcludes != b.SystemExcludes {
		v := d.SystemExcludes
		c.SystemExcludes, changed = &v, true
	}
	if d.BudgetBytes != b.BudgetBytes {
		v := d.BudgetBytes
		c.BudgetBytes, changed = &v, true
	}
	if d.BuildSpeed != b.BuildSpeed {
		v := d.BuildSpeed
		c.BuildSpeed, changed = &v, true
	}

	for drive, dv := range d.Volumes {
		bv := b.Volumes[drive]
		if pv := patchVolume(dv, bv); pv != nil {
			if c.Volumes == nil {
				c.Volumes = map[string]serviceContentVolume{}
			}
			c.Volumes[drive] = *pv
			changed = true
		}
	}
	// A removed override is neutralized by writing the global values, which
	// leaves the effective settings identical to "All drives".
	for drive := range b.Volumes {
		if _, ok := d.Volumes[drive]; ok {
			continue
		}
		global := serviceContentVolumeOf(b)
		if c.Volumes == nil {
			c.Volumes = map[string]serviceContentVolume{}
		}
		c.Volumes[drive] = global
		changed = true
	}

	if changed {
		p.Content = c
	}
	if len(p.Volumes) == 0 && !changed {
		return nil
	}
	return p
}

func (m *settingsModel) baseVolume(volume string) *volumeDraft {
	for i := range m.base.Volumes {
		if m.base.Volumes[i].Volume == volume {
			return &m.base.Volumes[i]
		}
	}
	return nil
}

func patchVolume(dv, bv *contentVolumeDraft) *serviceContentVolume {
	ov := &serviceContentVolume{}
	any := false
	if dv.HasMode && (bv == nil || !bv.HasMode || dv.Mode != bv.Mode) {
		v := dv.Mode
		ov.Mode, any = &v, true
	}
	if dv.HasRoots && (bv == nil || !bv.HasRoots || !equalStrings(dv.Roots, bv.Roots)) {
		v := append([]string(nil), dv.Roots...)
		ov.Roots, any = &v, true
	}
	if dv.HasExclude && (bv == nil || !bv.HasExclude || !equalStrings(dv.Exclude, bv.Exclude)) {
		v := append([]string(nil), dv.Exclude...)
		ov.Exclude, any = &v, true
	}
	if dv.HasExts && (bv == nil || !bv.HasExts || !equalStrings(dv.Exts, bv.Exts)) {
		v := append([]string(nil), dv.Exts...)
		ov.Exts, any = &v, true
	}
	if dv.HasGit && (bv == nil || !bv.HasGit || dv.Git != bv.Git) {
		v := dv.Git
		ov.Git, any = &v, true
	}
	if dv.HasSystemExcludes && (bv == nil || !bv.HasSystemExcludes || dv.SystemExcludes != bv.SystemExcludes) {
		v := dv.SystemExcludes
		ov.SystemExcludes, any = &v, true
	}
	if dv.HasBudget && (bv == nil || !bv.HasBudget || dv.BudgetBytes != bv.BudgetBytes) {
		v := dv.BudgetBytes
		ov.BudgetBytes, any = &v, true
	}
	if !any {
		return nil
	}
	return ov
}

func serviceContentVolumeOf(c contentDraft) serviceContentVolume {
	mode := c.Mode
	roots := append([]string(nil), c.Roots...)
	exclude := append([]string(nil), c.Exclude...)
	exts := append([]string(nil), c.Exts...)
	git := c.Git
	sys := c.SystemExcludes
	budget := c.BudgetBytes
	return serviceContentVolume{
		Mode: &mode, Roots: &roots, Exclude: &exclude, Exts: &exts,
		Git: &git, SystemExcludes: &sys, BudgetBytes: &budget,
	}
}

// allFields returns a non-nil marker list for hasChanges. The contents are
// not used; the length reports whether the patch carries work.
func (p *serviceSettings) allFields() []struct{} {
	if p == nil {
		return nil
	}
	n := len(p.Volumes)
	if p.Content != nil {
		n++
	}
	return make([]struct{}, n)
}

func (p *serviceSettings) marshal() ([]byte, error) {
	if p == nil {
		return []byte("null"), nil
	}
	return json.Marshal(p)
}

// parseBudgetGB converts the custom size field to bytes. An empty value or
// "no limit" means no explicit budget.
func parseBudgetGB(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "no limit" || s == "unlimited" {
		return 0, nil
	}
	s = strings.TrimSuffix(s, "gb")
	s = strings.TrimSuffix(s, "g")
	s = strings.TrimSpace(s)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		//lint:ignore ST1005 shown verbatim as an inline message in the Settings window
		return 0, errors.New("Enter a size in GB, or choose No limit")
	}
	if f > float64(math.MaxInt64)/float64(gbBytes) {
		//lint:ignore ST1005 shown verbatim as an inline message in the Settings window
		return 0, errors.New("Size limit is too large")
	}
	return int64(math.Round(f * float64(gbBytes))), nil
}

func formatBudgetGB(bytes int64) string {
	if bytes <= 0 {
		return ""
	}
	gb := float64(bytes) / float64(gbBytes)
	if math.Abs(gb-math.Round(gb)) < 0.0001 {
		return strconv.FormatInt(int64(math.Round(gb)), 10)
	}
	return strconv.FormatFloat(gb, 'f', -1, 64)
}

// parseExtList splits a comma/space separated allowlist. Entries may be
// written with a leading dot. Empty input means sniff text automatically.
func parseExtList(s string) ([]string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})
	var out []string
	seen := map[string]bool{}
	for _, f := range fields {
		f = strings.ToLower(strings.TrimSpace(f))
		f = strings.TrimPrefix(f, ".")
		if f == "" {
			continue
		}
		for _, r := range f {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' && r != '-' && r != '_' {
				//lint:ignore ST1005 shown verbatim as an inline message in the Settings window
				return nil, fmt.Errorf("Invalid extension %q", f)
			}
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out, nil
}

func formatExtList(exts []string) string {
	return strings.Join(exts, ", ")
}

func formatCompactCount(n int64) string {
	if n < 0 {
		n = 0
	}
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	units := []string{"K", "M", "B", "T"}
	v := float64(n)
	i := -1
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	text := strconv.FormatFloat(v, 'f', 1, 64)
	text = strings.TrimSuffix(text, ".0")
	return text + units[i]
}

func indexingPercent(v *serviceVolumeProgress) int {
	if v == nil || v.Total <= 0 {
		return -1
	}
	pct := int(v.Done * 100 / v.Total)
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

func formatIndexingProgress(v *serviceVolumeProgress) string {
	pct := indexingPercent(v)
	if pct >= 0 {
		return fmt.Sprintf("Indexing… %d%% (%s of %s files)", pct, formatCompactCount(v.Done), formatCompactCount(v.Total))
	}
	if v != nil && v.Done > 0 {
		return "Indexing… " + formatCompactCount(v.Done) + " files"
	}
	return "Indexing…"
}

func formatIndexingStatus(volume string, v *serviceVolumeProgress) string {
	if pct := indexingPercent(v); pct >= 0 {
		return fmt.Sprintf("Indexing %s %d%%", volume, pct)
	}
	return "Indexing " + volume
}

func formatBytesCompact(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatInt(n, 10) + " B"
	}
	if v >= 100 {
		return strconv.FormatFloat(v, 'f', 0, 64) + " " + units[i]
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + units[i]
}
