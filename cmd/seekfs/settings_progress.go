package main

import (
	"sync/atomic"
	"time"
)

// volumeBuildProgress tracks one settings-driven index build so settings-get
// can report a determinate progress bar. Every field is updated from the
// build goroutine and read from pipe handlers, so all mutable state is atomic.
type volumeBuildProgress struct {
	started time.Time
	phase   atomic.Value // string
	done    atomic.Int64
	total   atomic.Int64
}

func newVolumeBuildProgress() *volumeBuildProgress {
	p := &volumeBuildProgress{started: time.Now()}
	p.phase.Store("enumerating")
	return p
}

func (p *volumeBuildProgress) setPhase(phase string) {
	if p == nil || phase == "" {
		return
	}
	p.phase.Store(phase)
}

func (p *volumeBuildProgress) setCounts(done, total uint64) {
	if p == nil {
		return
	}
	p.done.Store(int64(done))
	p.total.Store(int64(total))
}

func (p *volumeBuildProgress) setDone(done uint64) {
	if p == nil {
		return
	}
	p.done.Store(int64(done))
}

func (p *volumeBuildProgress) phaseString() string {
	if p == nil {
		return ""
	}
	if v := p.phase.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// settingsVolumeProgress is the JSON projection in settings-get. It is only
// present while the volume's state is "indexing".
type settingsVolumeProgress struct {
	Phase     string `json:"phase"`
	Done      int64  `json:"done"`
	Total     int64  `json:"total"`
	StartedAt string `json:"started_at"`
}

func (p *volumeBuildProgress) settingsProgress() *settingsVolumeProgress {
	if p == nil {
		return nil
	}
	started := ""
	if !p.started.IsZero() {
		started = p.started.UTC().Format(time.RFC3339Nano)
	}
	return &settingsVolumeProgress{
		Phase:     p.phaseString(),
		Done:      p.done.Load(),
		Total:     p.total.Load(),
		StartedAt: started,
	}
}

// mftProgressTracker throttles the MFT enumeration callback so the hot parse
// loop pays one increment per record and one atomic callback per interval.
type mftProgressTracker struct {
	total    uint64
	interval uint64
	since    uint64
	onUpdate func(done, total uint64)
}

func newMFTProgressTracker(total uint64, onUpdate func(done, total uint64)) *mftProgressTracker {
	if onUpdate == nil {
		return nil
	}
	return &mftProgressTracker{total: total, interval: 4096, onUpdate: onUpdate}
}

func (t *mftProgressTracker) record(done uint64) {
	if t == nil {
		return
	}
	t.since++
	if t.since >= t.interval {
		t.since = 0
		t.onUpdate(done, t.total)
	}
}

func (t *mftProgressTracker) finish(done uint64) {
	if t == nil {
		return
	}
	t.onUpdate(done, t.total)
}
