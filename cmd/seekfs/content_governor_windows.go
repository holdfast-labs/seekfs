//go:build windows

package main

// Windows probes for the idle-aware build governor: foreground input recency
// (GetLastInputInfo), other-process CPU pressure (GetSystemTimes minus our own
// GetProcessTimes), and disk busyness (PDH PhysicalDisk _Total % Idle Time).
// Every probe fails open (idle): a broken reading must not throttle every
// build to full pauses forever. Sampled at most once per 64-doc batch, so
// each probe is a few syscalls per batch — noise next to the 64 opens.

import (
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
)

// lastInputInfo mirrors LASTINPUTINFO: the tick count of the last foreground
// input event.
type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// contentSystemInputActive reports whether keyboard/mouse input arrived within
// contentBuildInputQuietWindow. Unknown (a failed probe) reads as idle.
func contentSystemInputActive() bool {
	var info lastInputInfo
	info.cbSize = uint32(8)
	r1, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info)))
	if r1 == 0 {
		return false
	}
	now, _, _ := procGetTickCount64Gov.Call()
	if now == 0 {
		return false
	}
	// now is the 64-bit tick (GetTickCount64) while dwTime is its low 32
	// bits: compare in 32-bit unsigned arithmetic so the 49-day wrap is
	// exact at any uptime.
	idleMs := uint32(now) - info.dwTime
	return time.Duration(idleMs)*time.Millisecond < contentBuildInputQuietWindow
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
		return false
	}
	var create, exit, procKernel, procUser windows.Filetime
	proc, err := windows.GetCurrentProcess()
	if err != nil {
		return false
	}
	r1, _, _ = procGetProcessTimesGov.Call(
		uintptr(proc),
		uintptr(unsafe.Pointer(&create)),
		uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&procKernel)),
		uintptr(unsafe.Pointer(&procUser)))
	if r1 == 0 {
		return false
	}
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
