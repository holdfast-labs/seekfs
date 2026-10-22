package main

import (
	"strings"
	"time"
)

// gstatusrefresh.go: one background refresher keeps service status fresh. It
// polls info (never ensureService, so it cannot spawn a second service) on a
// state-dependent cadence, applies the result under the UI lock, and reruns the
// current query only when the loaded volume set changes or a load finishes.

const (
	statusPollUnhealthy = time.Second
	statusPollBuilding  = 2 * time.Second
	statusPollIdle      = 10 * time.Second
)

// statusSnapshot is one observation of the service info response.
type statusSnapshot struct {
	Reachable bool
	Health    string
	Message   string
	Entries   int
	NoVolumes bool
	Loading   bool
	Volumes   []DBVolume
	Content   *contentState
}

// statusChange records what one applied snapshot changed on the UI state.
type statusChange struct {
	Changed         bool
	VolumesChanged  bool
	LoadingFinished bool
}

// RerunQuery reports whether results may be stale because the index set moved.
func (c statusChange) RerunQuery() bool {
	return c.VolumesChanged || c.LoadingFinished
}

// statusFromInfo maps an info response to the UI status view. A first-run
// service (no volumes) is the neutral empty state, not an error.
func statusFromInfo(resp serviceResponse, err error) statusSnapshot {
	if err != nil {
		return statusSnapshot{Health: serviceHealthError, Message: err.Error()}
	}
	s := statusSnapshot{
		Reachable: true,
		Entries:   resp.Entries,
		Loading:   resp.Loading,
		Volumes:   dbVolumesFrom(resp),
		Content:   cloneContentState(resp.Content),
		NoVolumes: len(resp.DBs) == 0,
	}
	switch {
	case resp.Loading:
		s.Health = serviceHealthDegraded
		s.Message = "Loading indexes…"
	case len(resp.DBs) > 0:
		s.Health = firstNonEmpty(resp.Health, serviceHealthOK)
		s.Message = resp.HealthMessage
	case s.NoVolumes:
		s.Health = serviceHealthEmpty
		s.Message = "No drives indexed yet"
	case resp.OK:
		s.Health = firstNonEmpty(resp.Health, serviceHealthOK)
		s.Message = resp.HealthMessage
	default:
		s.Health = serviceHealthError
		s.Message = firstNonEmpty(resp.Message, "service has no search indexes loaded")
	}
	if !resp.OK && !resp.Loading && len(resp.DBs) == 0 {
		s.Health = serviceHealthError
		s.Message = firstNonEmpty(resp.Message, "service has no search indexes loaded")
	}
	return s
}

func cloneContentState(c *contentState) *contentState {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

func contentStatesEqual(a, b *contentState) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func dbVolumesEqual(a, b []DBVolume) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Volume != b[i].Volume ||
			a[i].Entries != b[i].Entries || a[i].State != b[i].State {
			return false
		}
		if !volumeProgressEqual(a[i].Progress, b[i].Progress) {
			return false
		}
	}
	return true
}

func volumeProgressEqual(a, b *serviceVolumeProgress) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// statusRefreshInterval selects the next poll delay: fast while the service is
// not healthy, moderate while a drive or content index builds, slow when idle.
func statusRefreshInterval(health string, reachable, building bool) time.Duration {
	if !reachable || health != serviceHealthOK {
		return statusPollUnhealthy
	}
	if building {
		return statusPollBuilding
	}
	return statusPollIdle
}

// statusRefreshLoop polls info forever. It never spawns a service; the initial
// handshake (and its one spawn attempt) stays in initialInfo.
func (u *UI) statusRefreshLoop() {
	timer := time.NewTimer(statusPollUnhealthy)
	defer timer.Stop()
	for {
		<-timer.C
		resp, err := u.client.Info()
		snap := statusFromInfo(resp, err)
		u.applyStatus(snap)
		u.mu.Lock()
		driveBuilding := u.settingsIndexing || u.loadingIndex
		u.mu.Unlock()
		building := driveBuilding || contentIndexBuilding(snap.Content)
		timer.Reset(statusRefreshInterval(snap.Health, err == nil, building))
	}
}

// refreshStatusOnce applies one info poll now. Tests use it; the settings poll
// also calls it when an indexing build ends.
func (u *UI) refreshStatusOnce() statusChange {
	resp, err := u.client.Info()
	return u.applyStatus(statusFromInfo(resp, err))
}

// applyStatus updates health, entries, sidebar volumes and content telemetry.
// It invalidates only when something changed.
func (u *UI) applyStatus(s statusSnapshot) statusChange {
	u.mu.Lock()
	prevHealth, prevMsg := u.health, u.healthMsg
	prevEntries, prevNoVolumes := u.entries, u.noVolumes
	prevLoading := u.loadingIndex
	prevContent := u.contentHealth
	prevVolumes := u.loadedVolumes

	healthChanged := s.Health != prevHealth || s.Message != prevMsg
	volumesChanged := false
	contentChanged := false
	if s.Reachable {
		healthChanged = healthChanged || s.Entries != prevEntries || s.NoVolumes != prevNoVolumes
		volumesChanged = !dbVolumesEqual(prevVolumes, s.Volumes)
		contentChanged = !contentStatesEqual(prevContent, s.Content)
		u.health, u.healthMsg = s.Health, s.Message
		u.entries = s.Entries
		u.noVolumes = s.NoVolumes
		u.loadingIndex = s.Loading
		u.contentHealth = s.Content
		if volumesChanged {
			u.loadedVolumes = append([]DBVolume(nil), s.Volumes...)
		}
	} else {
		// Keep the last loaded set while the service is unreachable so the
		// sidebar does not flash "No indexes loaded".
		u.health, u.healthMsg = s.Health, s.Message
	}
	u.mu.Unlock()

	changed := healthChanged || contentChanged || volumesChanged
	if volumesChanged {
		u.refreshSidebarFromState()
	}
	if changed {
		u.invalidate()
	}
	change := statusChange{
		Changed:         changed,
		VolumesChanged:  volumesChanged,
		LoadingFinished: s.Reachable && prevLoading && !s.Loading,
	}
	if change.RerunQuery() {
		u.rerunCurrentQuery()
	}
	return change
}

// rerunCurrentQuery re-issues the typed query from a background goroutine
// without touching Gio widget state.
func (u *UI) rerunCurrentQuery() {
	u.mu.Lock()
	text, contents, under := u.query, u.contents, u.under
	u.mu.Unlock()
	if strings.TrimSpace(text) == "" {
		return
	}
	u.searcher.SearchNow(text, contents, under)
}

// refreshSidebarFromState merges loaded volumes with build-only rows from the
// settings snapshot and rebuilds the sidebar only when the display list moves.
func (u *UI) refreshSidebarFromState() {
	u.mu.Lock()
	loaded := append([]DBVolume(nil), u.loadedVolumes...)
	snap := u.settingsSnapshot
	current := append([]DBVolume(nil), u.volumes...)
	u.mu.Unlock()
	merged := mergeSidebarVolumes(loaded, snap)
	if dbVolumesEqual(current, merged) {
		return
	}
	u.rebuildSideRows(merged)
}

// mergeSidebarVolumes adds drives that are indexing (or failed) to the loaded
// list so the sidebar names them during a build instead of saying
// "No indexes loaded".
func mergeSidebarVolumes(loaded []DBVolume, snap *serviceSettings) []DBVolume {
	out := append([]DBVolume(nil), loaded...)
	if snap == nil {
		return out
	}
	index := func(volume string) int {
		for i := range out {
			if strings.EqualFold(out[i].Volume, volume) {
				return i
			}
		}
		return -1
	}
	for _, v := range snap.Volumes {
		if v.Volume == "" {
			continue
		}
		if v.State != "indexing" && v.State != "error" && v.State != "ready" {
			continue
		}
		i := index(v.Volume)
		if i >= 0 {
			out[i].State = v.State
			out[i].Entries = int(v.Entries)
			out[i].Progress = v.Progress
			continue
		}
		out = append(out, DBVolume{
			Volume:   v.Volume,
			Entries:  int(v.Entries),
			State:    v.State,
			Progress: v.Progress,
		})
	}
	return out
}
