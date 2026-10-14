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
//     contentBuildInputQuietWindow (GetLastInputInfo; someone is at the
//     machine even if they have not queried).
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
// presence without user32.
var contentInputActiveNow = contentSystemInputActive

// contentSystemBusyNow reports whether other processes are burning the
// machine. A var so tests inject load without sampling times.
var contentSystemBusyNow = contentSystemCPUBusy

// contentDiskBusyNow reports whether the disks are busy. A var so tests
// inject disk states without PDH.
var contentDiskBusyNow = contentSystemDiskBusy

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
