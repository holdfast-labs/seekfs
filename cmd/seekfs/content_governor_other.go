//go:build !windows

package main

import (
	"sync/atomic"
	"time"
)

// Non-Windows stubs for the idle-aware build governor probes: every signal
// reads as idle, so background builds never pause off-platform. The decision
// matrix (contentBuildBusyReason) stays testable everywhere.

// contentCurrentSessionIDProbe is a no-op off Windows.
func contentCurrentSessionIDProbe() (uint32, bool) { return 0, false }

// contentGLIILastInputProbe is a no-op off Windows: GetLastInputInfo is a
// Windows-only probe.
func contentGLIILastInputProbe() (time.Time, bool) { return time.Time{}, false }

// contentWTSLastInputProbe is a no-op off Windows: session enumeration is a
// Windows-only probe.
func contentWTSLastInputProbe() (time.Time, bool) { return time.Time{}, false }

// contentSystemCPUBusy is a no-op off Windows: CPU-pressure observation is a
// Windows-only probe (GetSystemTimes).
func contentSystemCPUBusy() bool { return false }

// contentSystemDiskBusy is a no-op off Windows: disk-idle observation is a
// Windows-only probe (PDH).
func contentSystemDiskBusy() bool { return false }

// contentDiskIdlePct is a no-op off Windows.
func contentDiskIdlePct() (float64, bool) { return 0, false }
