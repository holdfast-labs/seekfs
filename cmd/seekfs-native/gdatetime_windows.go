//go:build windows

package main

import (
	"syscall"
	"time"
	"unsafe"
)

// gdatetime_windows.go: Explorer-style locale date/time formatting via
// GetDateFormatEx (short date) + GetTimeFormatEx (short time, no seconds),
// e.g. "10/11/2026 4:20 PM" for en-US.

var (
	pGetDateFormatEx = proc(modKernel32, "GetDateFormatEx")
	pGetTimeFormatEx = proc(modKernel32, "GetTimeFormatEx")
)

// formatExplorerTime formats t like Explorer's Date modified column
// (locale short date + short time, local zone).
func formatExplorerTime(t time.Time) string {
	st := systemTimeOf(t)
	var dateBuf [128]uint16
	r, _, _ := syscall.SyscallN(pGetDateFormatEx.Addr(),
		0, // LOCALE_NAME_USER_DEFAULT (NULL)
		uintptr(0x1), // DATE_SHORTDATE
		uintptr(unsafe.Pointer(&st)),
		0,
		uintptr(unsafe.Pointer(&dateBuf[0])), 128,
		0)
	if r == 0 {
		return t.Format("2006-01-02 15:04")
	}
	var timeBuf [128]uint16
	r, _, _ = syscall.SyscallN(pGetTimeFormatEx.Addr(),
		0, // LOCALE_NAME_USER_DEFAULT (NULL)
		uintptr(0x2), // TIME_NOSECONDS
		uintptr(unsafe.Pointer(&st)),
		0, // lpFormat NULL = locale default
		uintptr(unsafe.Pointer(&timeBuf[0])), 128)
	if r == 0 {
		return syscall.UTF16ToString(dateBuf[:])
	}
	return syscall.UTF16ToString(dateBuf[:]) + " " + syscall.UTF16ToString(timeBuf[:])
}

type systemTime struct {
	Year, Month, DayOfWeek, Day, Hour, Minute, Second, Millis uint16
}

// systemTimeOf converts to a SYSTEMTIME in local time (what Explorer
// shows). Calendar fields come straight from the time package — no manual
// civil math.
func systemTimeOf(t time.Time) systemTime {
	lt := t.In(time.Local)
	y, m, d := lt.Date()
	hh, mm, ss := lt.Clock()
	return systemTime{
		Year: uint16(y), Month: uint16(m), Day: uint16(d),
		DayOfWeek: uint16(lt.Weekday()),
		Hour:      uint16(hh), Minute: uint16(mm), Second: uint16(ss),
	}
}
