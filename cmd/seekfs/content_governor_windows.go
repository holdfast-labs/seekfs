//go:build windows

package main

// Windows probes for the idle-aware build governor: foreground input recency
// (GetLastInputInfo), other-process CPU pressure (GetSystemTimes minus our own
// GetProcessTimes), and disk busyness (PDH PhysicalDisk _Total % Idle Time).
// Every probe fails open (idle): a broken reading must not throttle every
// build to full pauses forever. Sampled at most once per 64-doc batch, so
// each probe is a few syscalls per batch — noise next to the 64 opens.

import (
	"encoding/binary"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	moduser32ForGovernor   = windows.NewLazySystemDLL("user32.dll")
	procGetLastInputInfo   = moduser32ForGovernor.NewProc("GetLastInputInfo")
	modkernel32ForGovernor = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimesGov  = modkernel32ForGovernor.NewProc("GetSystemTimes")
	procGetProcessTimesGov = modkernel32ForGovernor.NewProc("GetProcessTimes")
	procGetTickCount64Gov  = modkernel32ForGovernor.NewProc("GetTickCount64")
	procProcessIDToSession = modkernel32ForGovernor.NewProc("ProcessIdToSessionId")
)

// Session-0 note: the installed service runs as LocalSystem in session 0,
// which never receives keyboard/mouse input, so GetLastInputInfo there always
// reads idle. The WTS probes below query the interactive sessions instead
// and work from any session; GetLastInputInfo stays as the fallback for the
// calling session when not running as a service.

var (
	modwtsapi32ForGovernor   = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSEnumerateSessions = modwtsapi32ForGovernor.NewProc("WTSEnumerateSessionsW")
	procWTSQuerySessionInfo  = modwtsapi32ForGovernor.NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory        = modwtsapi32ForGovernor.NewProc("WTSFreeMemory")
)

// wtsSessionInfoExClass is WTS_INFO_CLASS WTSSessionInfoEx.
const wtsSessionInfoExClass = 25

// Offsets into the WTSINFOEX Level-1 payload (WTS_INFOEX wrapper: Level @0,
// Data @8; LEVEL1: SessionId @8, LastInputTime FILETIME @184, CurrentTime
// FILETIME @192). Validated live against SessionId echo + CurrentTime skew
// (see the governor WTS probe); struct sizes: WinStationName 33 + UserName 21
// + Domain 16 wide chars, then 8-byte-aligned LARGE_INTEGERs from @160.
const (
	wtsInfoExLevelOff     = 0
	wtsInfoExDataOff      = 8
	wtsL1SessionIDOff     = 8
	wtsL1LastInputOff     = 184
	wtsL1CurrentOff       = 192
	wtsInfoExMinBytes     = 200
	wtsSessionEntryStride = 24
	wtsSessionEntryState  = 16
)

// lastInputInfo mirrors LASTINPUTINFO: the tick count of the last foreground
// input event in the calling session.
type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// contentCurrentSessionIDProbe reports our terminal-services session ID
// (0 for the service session) via ProcessIdToSessionId.
func contentCurrentSessionIDProbe() (uint32, bool) {
	var session uint32
	r1, _, _ := procProcessIDToSession.Call(
		uintptr(windows.GetCurrentProcessId()),
		uintptr(unsafe.Pointer(&session)))
	if r1 == 0 {
		return 0, false
	}
	return session, true
}

// contentGLIILastInputProbe wraps GetLastInputInfo as an absolute UTC time.
// It observes the calling session only: exact in an interactive session,
// permanently idle in session 0 (see above).
func contentGLIILastInputProbe() (time.Time, bool) {
	var info lastInputInfo
	info.cbSize = uint32(8)
	r1, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info)))
	if r1 == 0 {
		return time.Time{}, false
	}
	now, _, _ := procGetTickCount64Gov.Call()
	if now == 0 {
		return time.Time{}, false
	}
	// now is the 64-bit tick (GetTickCount64) while dwTime is its low 32
	// bits: compare in 32-bit unsigned arithmetic so the 49-day wrap is
	// exact at any uptime.
	idleMs := uint32(now) - info.dwTime
	return time.Now().UTC().Add(-time.Duration(idleMs) * time.Millisecond), true
}

// contentWTSLastInputProbe returns the most recent valid LastInputTime across
// all enumerated sessions. It works from session 0 (LocalSystem may query any
// session) and from interactive sessions alike. Sessions whose LastInputTime
// is zero (never reported input on this Windows build, or never received
// any) are skipped, never treated as recent.
func contentWTSLastInputProbe() (time.Time, bool) {
	var list unsafe.Pointer
	var count uintptr
	r1, _, _ := procWTSEnumerateSessions.Call(0, 0, 1,
		uintptr(unsafe.Pointer(&list)),
		uintptr(unsafe.Pointer(&count)))
	if r1 == 0 || list == nil || count == 0 {
		return time.Time{}, false
	}
	entries := unsafe.Slice((*byte)(list), count*wtsSessionEntryStride)
	var best time.Time
	found := false
	for i := uintptr(0); i < count; i++ {
		base := i * wtsSessionEntryStride
		session := binary.LittleEndian.Uint32(entries[base:])
		if t, ok := contentWTSSessionLastInput(session); ok && (!found || t.After(best)) {
			best, found = t, true
		}
	}
	_, _, _ = procWTSFreeMemory.Call(uintptr(list))
	if !found {
		return time.Time{}, false
	}
	return best, true
}

// contentWTSSessionLastInput queries one session's WTSSessionInfoEx and
// returns its LastInputTime. Zero times and layout mismatches (the echoed
// SessionId must match) read as unavailable, never as recent.
func contentWTSSessionLastInput(session uint32) (time.Time, bool) {
	var buf unsafe.Pointer
	var bytesRet uintptr
	r1, _, _ := procWTSQuerySessionInfo.Call(0, uintptr(session),
		uintptr(wtsSessionInfoExClass),
		uintptr(unsafe.Pointer(&buf)),
		uintptr(unsafe.Pointer(&bytesRet)))
	if r1 == 0 || buf == nil || bytesRet < wtsInfoExMinBytes {
		if buf != nil {
			_, _, _ = procWTSFreeMemory.Call(uintptr(buf))
		}
		return time.Time{}, false
	}
	raw := unsafe.Slice((*byte)(buf), bytesRet)
	ok := false
	var last time.Time
	if binary.LittleEndian.Uint32(raw[wtsInfoExLevelOff:]) == 1 &&
		binary.LittleEndian.Uint32(raw[wtsL1SessionIDOff:]) == session {
		low := binary.LittleEndian.Uint32(raw[wtsL1LastInputOff:])
		high := binary.LittleEndian.Uint32(raw[wtsL1LastInputOff+4:])
		if low != 0 || high != 0 {
			ft := windows.Filetime{LowDateTime: low, HighDateTime: high}
			last, ok = time.Unix(0, ft.Nanoseconds()).UTC(), true
		}
	}
	_, _, _ = procWTSFreeMemory.Call(uintptr(buf))
	return last, ok
}

// filetimeToU64 folds a Windows FILETIME to a 100 ns tick count.
func filetimeToU64(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// contentCPUSampleInterval bounds how often the CPU probe samples: deltas
// need a window, and a syscall pair per batch is waste next to 64 opens but
// still pointless — one sample per ~2 s is plenty for a governor.
var contentCPUSampleInterval = 2 * time.Second

var contentCPUSampleMu sync.Mutex
var contentCPUSampleAt time.Time
var contentCPUSampleIdle, contentCPUSampleKernel, contentCPUSampleUser uint64
var contentCPUSampleProcKernel, contentCPUSampleProcUser uint64

// contentSystemCPUBusy reports whether processes other than us burned more
// than contentBuildCPUBusyFraction of the machine since the last sample.
// Unknown (a failed probe, or the first sample with nothing to delta
// against) reads as idle. Our own footprint is subtracted: with 16 extract
// workers the build itself is the hottest process, and measuring ourselves
// would pin every build to full pauses.
func contentSystemCPUBusy() bool {
	var idle, kernel, user windows.Filetime
	r1, _, _ := procGetSystemTimesGov.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)))
	if r1 == 0 {
		contentCPULastOK.Store(false)
		return false
	}
	var create, exit, procKernel, procUser windows.Filetime
	proc := windows.CurrentProcess()
	r1, _, _ = procGetProcessTimesGov.Call(
		uintptr(proc),
		uintptr(unsafe.Pointer(&create)),
		uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&procKernel)),
		uintptr(unsafe.Pointer(&procUser)))
	if r1 == 0 {
		contentCPULastOK.Store(false)
		return false
	}
	contentCPULastOK.Store(true)
	now := time.Now()
	contentCPUSampleMu.Lock()
	defer contentCPUSampleMu.Unlock()
	prevAt := contentCPUSampleAt
	contentCPUSampleAt = now
	prevIdle, prevKernel, prevUser := contentCPUSampleIdle, contentCPUSampleKernel, contentCPUSampleUser
	prevPK, prevPU := contentCPUSampleProcKernel, contentCPUSampleProcUser
	contentCPUSampleIdle, contentCPUSampleKernel, contentCPUSampleUser =
		filetimeToU64(idle), filetimeToU64(kernel), filetimeToU64(user)
	contentCPUSampleProcKernel, contentCPUSampleProcUser =
		filetimeToU64(procKernel), filetimeToU64(procUser)
	if prevAt.IsZero() || now.Sub(prevAt) < contentCPUSampleInterval {
		return false
	}
	total := (contentCPUSampleIdle - prevIdle) +
		(contentCPUSampleKernel - prevKernel) + (contentCPUSampleUser - prevUser)
	if total == 0 {
		return false
	}
	procBusy := (contentCPUSampleProcKernel - prevPK) + (contentCPUSampleProcUser - prevPU)
	if procBusy > total {
		procBusy = total
	}
	other := total - procBusy - (contentCPUSampleIdle - prevIdle)
	return float64(other)/float64(total) > contentBuildCPUBusyFraction
}

// PDH disk-idle probe. PhysicalDisk(_Total)\% Idle Time is device-busy time,
// not our experienced latency: cold AV-scanned opens and our own small-read
// storm barely move it (their milliseconds are CPU, not device time), while
// bulk IO from another process pins it near zero. That is exactly the "disk
// queue pressure" the governor yields to, with no per-volume instance
// mapping (_Total covers the subsystem).
//
// The query handle is opened once per process and never closed; setup failure
// is cached so a missing counter degrades to one failed setup, not a per-batch
// one. Every probe failure reads as idle (fail open).

var (
	modpdhForGovernor             = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQuery              = modpdhForGovernor.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounter      = modpdhForGovernor.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData       = modpdhForGovernor.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterVal = modpdhForGovernor.NewProc("PdhGetFormattedCounterValue")
)

// pdhFmtCounterValue mirrors PDH_FMT_COUNTERVALUE for PDH_FMT_DOUBLE: a
// status plus an 8-byte-aligned union holding the double at offset 8.
type pdhFmtCounterValue struct {
	status uint32
	_      [4]byte
	value  float64
}

// pdhFormatDouble selects the double variant for PdhGetFormattedCounterValue.
const pdhFormatDouble = 0x200

var contentDiskPDHMu sync.Mutex
var contentDiskPDHTried, contentDiskPDHReady bool
var contentDiskPDHQuery, contentDiskPDHCounter uintptr

// contentDiskIdlePct returns the _Total % Idle Time (0–100). ok is false when
// PDH is unavailable or the sample is not valid yet (including the first
// collect after opening the query).
func contentDiskIdlePct() (pct float64, ok bool) {
	contentDiskPDHMu.Lock()
	defer contentDiskPDHMu.Unlock()
	defer func() { contentDiskLastOK.Store(ok) }()
	if !contentDiskPDHTried {
		contentDiskPDHTried = true
		var query, counter uintptr
		r1, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&query)))
		if r1 != 0 || query == 0 {
			return 0, false
		}
		path, err := windows.UTF16PtrFromString(`\PhysicalDisk(_Total)\% Idle Time`)
		if err != nil {
			return 0, false
		}
		r1, _, _ = procPdhAddEnglishCounter.Call(query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&counter)))
		if r1 != 0 || counter == 0 {
			return 0, false
		}
		// The first collect primes the counter; its value is invalid by
		// design, so seed it here and report unavailable once.
		if r1, _, _ := procPdhCollectQueryData.Call(query); r1 != 0 {
			return 0, false
		}
		contentDiskPDHQuery, contentDiskPDHCounter = query, counter
		contentDiskPDHReady = true
		return 0, false
	}
	if !contentDiskPDHReady {
		return 0, false
	}
	if r1, _, _ := procPdhCollectQueryData.Call(contentDiskPDHQuery); r1 != 0 {
		return 0, false
	}
	var val pdhFmtCounterValue
	var typ uint32
	r1, _, _ := procPdhGetFormattedCounterVal.Call(contentDiskPDHCounter, uintptr(pdhFormatDouble), uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&val)))
	if r1 != 0 || val.status != 0 {
		return 0, false
	}
	if val.value < 0 || val.value > 100 {
		return 0, false
	}
	return val.value, true
}

// contentSystemDiskBusy reports whether _Total % Idle Time sits under
// contentDiskIdleBusyPct. Unavailable reads as idle.
func contentSystemDiskBusy() bool {
	pct, ok := contentDiskIdlePct()
	if !ok {
		return false
	}
	return contentDiskBusyFromIdle(pct)
}
