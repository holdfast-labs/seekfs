package main

// Idle-aware content build governor.
//
// The service build used to pause a fixed contentBuildBatchPause every
// contentBuildBatchSize extracted docs (~30 s on a 76k-file build, ~4 min on
// a 650k-file one) whether the machine needed the politeness or not. The
// offline harnesses bypass the pause, so it was never measured — and on an
// idle machine it is pure overhead: the prefetch workers stay ahead of the
// consumer, so the pause only gates how fast the ordered stream drains.
//
// contentBuildPauseNow returns the pause for one batch boundary: zero when
// the machine/disk is idle and no foreground queries are running, otherwise
// the full contentBuildBatchPause. Every signal is a var so tests inject
// states deterministically; the production probes are one cheap syscall each
// (or cached), sampled at most once per 64-doc batch:
//
//   - contentUserActiveNow: a filename/content query ran within
//     contentBuildQueryQuietWindow (the service already tracks this).
//   - contentBuildQueriesActiveNow: a foreground search is in flight right
//     now (atomic count around the search commands, not just recent).
//   - contentOnBatteryNow: discharging (existing probe).
//   - contentInputActiveNow: keyboard/mouse input within
//     contentBuildInputQuietWindow. The installed service runs in session 0,
//     which never receives input, so the WTS session enumeration covers the
//     interactive sessions from any session and GetLastInputInfo is only the
//     fallback for the calling session elsewhere; with no valid source the
//     signal reads idle for decisions and reports unavailable in health
//     (contentGovernorHealthSnapshot) instead of silently assuming idleness.
//   - contentSystemBusyNow: other processes burn > contentBuildCPUBusyFraction
//     of the machine over the sample window (GetSystemTimes minus our own
//     GetProcessTimes, cached for contentBuildCPUSampleInterval).
//   - contentDiskBusyNow: the disks are busy (PhysicalDisk _Total % Idle
//     Time under contentDiskIdleBusyPct via PDH — device-busy time, not our
//     experienced latency, so cold opens and our own storm do not self-trip
//     it the way a latency average would).
//   - system drive: a build whose sidecar lands on the Windows system drive
//     always pauses (OS/pagefile/AV contention no cheap probe sees).
//
// SEEKFS_CONTENT_BUILD_PAUSE forces the policy ("never" skips every pause,
// "always" takes every pause); anything else (or unset) is auto.
// contentBuildPausesTaken counts pauses actually slept (plus Skipped via
// contentBuildPauseSkips) so harnesses can report what the governor did.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// contentBuildInputQuietWindow is how recently foreground input must have
// arrived for the build to consider someone at the machine.
var contentBuildInputQuietWindow = 60 * time.Second

// contentBuildCPUBusyFraction is the other-process CPU share over the sample
// window above which the machine reads as busy.
var contentBuildCPUBusyFraction = 0.5

// contentDiskIdleBusyPct is the PhysicalDisk _Total % Idle Time below which
// the disks read as busy. Our own small-read storm barely moves device-busy
// time (its milliseconds are AV/MFT CPU, not device time); bulk IO from
// another process pins it near zero.
var contentDiskIdleBusyPct = 30.0

// contentDiskBusyFromIdle applies the busy threshold to a sampled % Idle
// Time. Pure so tests pin the boundary without PDH.
func contentDiskBusyFromIdle(pct float64) bool {
	return pct < contentDiskIdleBusyPct
}

// contentBuildPauseModeOverride forces the governor ("never"/"always"/auto).
// A var so tests drive the policy without the environment.
var contentBuildPauseModeOverride = func() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_CONTENT_BUILD_PAUSE")))
}

// contentBuildQueriesActiveNow reports whether a foreground search command is
// in flight. A var so tests inject query load without a service.
var contentBuildQueriesActiveNow = func() bool {
	return serviceActiveQueries.Load() > 0
}

// contentInputActiveNow reports whether foreground keyboard/mouse input
// arrived within contentBuildInputQuietWindow. A var so tests inject
// presence without the OS probes. Unknown reads as idle for decisions;
// availability is reported via health (contentGovernorHealthSnapshot).
var contentInputActiveNow = contentSystemInputActive

// Input-source probes. The installed service runs in session 0, which never
// receives input, so GetLastInputInfo alone always reads idle there; the WTS
// session enumeration covers the interactive sessions from any session. Each
// is a var so tests inject sources deterministically.
var (
	contentCurrentSessionID = contentCurrentSessionIDProbe
	contentWTSLastInput     = contentWTSLastInputProbe
	contentGLIILastInput    = contentGLIILastInputProbe
)

// contentInputSampleInterval bounds how often the input sampler enumerates
// sessions: the quiet window is 60 s, so a 5 s sample is plenty, and WTS
// enumeration per 64-doc batch would be wasteful.
var contentInputSampleInterval = 5 * time.Second

var contentInputSampleMu sync.Mutex
var contentInputSampleAt time.Time
var contentInputSampleT time.Time
var contentInputSampleOK bool
var contentInputSampleSource string

// contentResetInputSample drops the cached input sample so tests sample
// fresh sources.
func contentResetInputSample() {
	contentInputSampleMu.Lock()
	defer contentInputSampleMu.Unlock()
	contentInputSampleAt = time.Time{}
	contentInputSampleT = time.Time{}
	contentInputSampleOK = false
	contentInputSampleSource = ""
}

// contentSampleInputTime returns the most recent valid last-input timestamp.
// In session 0 (the service) only WTS is consulted — GetLastInputInfo would
// read the inputless service session. Elsewhere both sources count and the
// most recent wins (a neighbor RDP session's typing counts as presence).
// ok is false when no source yields a time.
func contentSampleInputTime() (t time.Time, ok bool, source string) {
	var best time.Time
	var bOK bool
	var src string
	if sid, sidOK := contentCurrentSessionID(); !sidOK || sid != 0 {
		if lt, lok := contentGLIILastInput(); lok && (!bOK || lt.After(best)) {
			best, bOK, src = lt, true, "lastinput"
		}
	}
	if wt, wok := contentWTSLastInput(); wok && (!bOK || wt.After(best)) {
		best, bOK, src = wt, true, "wts"
	}
	return best, bOK, src
}

// contentCachedInputSample serves the sampler with a bounded cache.
func contentCachedInputSample() (time.Time, bool, string) {
	contentInputSampleMu.Lock()
	defer contentInputSampleMu.Unlock()
	now := time.Now()
	if contentInputSampleAt.IsZero() || now.Sub(contentInputSampleAt) >= contentInputSampleInterval {
		t, ok, src := contentSampleInputTime()
		contentInputSampleAt = now
		contentInputSampleT, contentInputSampleOK, contentInputSampleSource = t, ok, src
	}
	return contentInputSampleT, contentInputSampleOK, contentInputSampleSource
}

// contentSystemInputActive reports whether keyboard/mouse input arrived within
// contentBuildInputQuietWindow. Unknown (no source yields a time) reads as
// idle for decisions — availability is reported via health, never silently
// assumed.
func contentSystemInputActive() bool {
	last, ok, _ := contentCachedInputSample()
	if !ok {
		return false
	}
	return time.Since(last) < contentBuildInputQuietWindow
}

// contentSystemBusyNow reports whether other processes are burning the
// machine. A var so tests inject load without sampling times.
var contentSystemBusyNow = contentSystemCPUBusy

// contentDiskBusyNow reports whether the disks are busy. A var so tests
// inject disk states without PDH.
var contentDiskBusyNow = contentSystemDiskBusy

// Probe availability for health: each latch is true once its mechanism has
// produced a valid reading in this process (never on unsupported platforms).
// Decisions fail open independently of these; health reports them so a
// silently-dead probe (e.g. input in session 0 with no WTS time) is visible
// instead of masquerading as an idle machine.
var contentCPULastOK atomic.Bool
var contentDiskLastOK atomic.Bool
var contentBatteryLastOK atomic.Bool

// governorSignalHealth is one probe's live/unavailable status for `loaded`.
type governorSignalHealth struct {
	Live   bool   `json:"live"`
	Detail string `json:"detail,omitempty"`
}

// governorHealth reports which build-governor signals are live in this
// process. A signal that has never produced a valid reading reports
// live=false (e.g. input when neither WTS nor the calling session yields a
// time) so operators can tell "no user activity" apart from "cannot see user
// activity".
type governorHealth struct {
	Mode    string               `json:"mode"`
	Input   governorSignalHealth `json:"input"`
	CPU     governorSignalHealth `json:"cpu"`
	Disk    governorSignalHealth `json:"disk"`
	Battery governorSignalHealth `json:"battery"`
}

// contentGovernorHealthSnapshot samples the governor probes' availability.
// A latch that never fired triggers one live attempt (cheap; `loaded` is
// infrequent) so a fresh process reports working probes as live instead of
// unknown. The attempts' only side effects are resetting the CPU delta
// window (fail-open for one interval) and shifting a PDH interval.
func contentGovernorHealthSnapshot() *governorHealth {
	h := &governorHealth{Mode: contentBuildPauseModeOverride()}
	if h.Mode == "" {
		h.Mode = "auto"
	}
	if _, ok, src := contentCachedInputSample(); ok {
		h.Input = governorSignalHealth{Live: true, Detail: src}
	} else {
		h.Input = governorSignalHealth{Live: false, Detail: "unavailable"}
	}
	if !contentCPULastOK.Load() {
		contentSystemCPUBusy()
	}
	if contentCPULastOK.Load() {
		h.CPU = governorSignalHealth{Live: true, Detail: "system-times"}
	} else {
		h.CPU = governorSignalHealth{Live: false, Detail: "unavailable"}
	}
	if !contentDiskLastOK.Load() {
		contentDiskIdlePct()
	}
	if contentDiskLastOK.Load() {
		h.Disk = governorSignalHealth{Live: true, Detail: "pdh"}
	} else {
		h.Disk = governorSignalHealth{Live: false, Detail: "unavailable"}
	}
	if !contentBatteryLastOK.Load() {
		contentBatteryProbe()
	}
	if contentBatteryLastOK.Load() {
		h.Battery = governorSignalHealth{Live: true, Detail: "power-status"}
	} else {
		h.Battery = governorSignalHealth{Live: false, Detail: "unavailable"}
	}
	return h
}

// contentBuildPausesTaken and contentBuildPauseSkips count batch boundaries
// that slept vs skipped, for harness reporting and tests.
var contentBuildPausesTaken atomic.Int64
var contentBuildPauseSkips atomic.Int64

// contentBuildPauseReasons counts pause decisions by the signal that fired
// ("idle" for skips), so harnesses can attribute what the governor saw.
var contentBuildPauseReasonsMu sync.Mutex
var contentBuildPauseReasons = make(map[string]int64)

func contentBuildNoteReason(reason string) {
	contentBuildPauseReasonsMu.Lock()
	contentBuildPauseReasons[reason]++
	contentBuildPauseReasonsMu.Unlock()
}

// contentBuildPauseReasonCounts snapshots the per-signal decision counts.
func contentBuildPauseReasonCounts() map[string]int64 {
	contentBuildPauseReasonsMu.Lock()
	defer contentBuildPauseReasonsMu.Unlock()
	out := make(map[string]int64, len(contentBuildPauseReasons))
	for k, v := range contentBuildPauseReasons {
		out[k] = v
	}
	return out
}

// contentBuildBusyReason names the first tripping politeness signal, or ""
// when the build may skip the pause. The order is cheapest-first: atomic
// loads, then the cached/one-syscall probes, then the path check.
func contentBuildBusyReason(sidecarPath string) string {
	switch contentBuildPauseModeOverride() {
	case "never":
		return ""
	case "always":
		return "always"
	}
	if contentUserActiveNow() {
		return "query-recent"
	}
	if contentBuildQueriesActiveNow() {
		return "query-inflight"
	}
	if contentOnBatteryNow() {
		return "battery"
	}
	if contentInputActiveNow() {
		return "input"
	}
	if contentSystemBusyNow() {
		return "cpu"
	}
	if contentDiskBusyNow() {
		return "disk"
	}
	if contentBuildOnSystemDrive(sidecarPath) {
		return "system-drive"
	}
	return ""
}

// contentBuildBusyNow folds every politeness signal: true means take the
// pause. sidecarPath anchors the system-drive check to the volume the build
// writes to.
func contentBuildBusyNow(sidecarPath string) bool {
	return contentBuildBusyReason(sidecarPath) != ""
}

// contentBuildPauseNow is the pause for one batch boundary: zero on an idle
// machine (or when tests zero contentBuildBatchPause), else the configured
// pause. It counts the decision for harness reporting.
func contentBuildPauseNow(sidecarPath string) time.Duration {
	if contentBuildBatchPause <= 0 {
		return 0
	}
	if reason := contentBuildBusyReason(sidecarPath); reason != "" {
		contentBuildPausesTaken.Add(1)
		contentBuildNoteReason(reason)
		return contentBuildBatchPause
	}
	contentBuildPauseSkips.Add(1)
	contentBuildNoteReason("idle")
	return 0
}

// contentSystemDriveLetter reports the Windows system drive ("C:") for the
// sidecar-volume check. A var so tests pin the drive.
var contentSystemDriveLetter = func() string {
	d := strings.ToUpper(strings.TrimSpace(os.Getenv("SystemDrive")))
	if len(d) >= 2 && d[1] == ':' {
		return d[:2]
	}
	return ""
}

// contentBuildOnSystemDrive reports whether sidecarPath lands on the system
// drive. Unknown/empty drive reads as non-system (fail open: a broken reading
// must not force every build to pause forever).
func contentBuildOnSystemDrive(sidecarPath string) bool {
	sys := contentSystemDriveLetter()
	if sys == "" || sidecarPath == "" {
		return false
	}
	vol := strings.ToUpper(filepath.VolumeName(sidecarPath))
	if vol == "" {
		return false
	}
	return vol == sys
}
