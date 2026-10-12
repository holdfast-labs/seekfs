//go:build windows

package main

// Windows readability probe for the integrity pass: section payloads may alias
// a memory-mapped `.gsx` that a concurrent base swap unmaps, and touching an
// unmapped view raises a fault the Go runtime cannot recover (it is not a
// panic). Probing first with IsBadReadPtr keeps a torn-down mapping a clean
// verification error instead of a crash. The probe is best-effort by nature:
// it only narrows the race, which the service's task/indexMu discipline is
// what actually closes.

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procIsBadReadPtr = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsBadReadPtr")

// contentSectionReadable reports whether every page of payload can be read.
// Empty payloads are trivially readable.
func contentSectionReadable(payload []byte) bool {
	if len(payload) == 0 {
		return true
	}
	const page = 4096
	for off := 0; off < len(payload); off += page {
		end := off + page
		if end > len(payload) {
			end = len(payload)
		}
		if !contentRangeReadable(payload[off:end]) {
			return false
		}
	}
	return true
}

func contentRangeReadable(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	r, _, _ := procIsBadReadPtr.Call(
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(len(b)),
	)
	return r == 0
}
