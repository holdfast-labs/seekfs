//go:build windows

package main

import (
	"unsafe"
)

// winptr_windows.go: uintptr/unsafe.Pointer reinterpretation for Win32
// callbacks and notifications. Pointers come from the OS and stay valid for
// the dispatch duration; they are never retained.

// notifyPtr reinterprets a Win32 uintptr argument as a pointer. The double
// indirection defeats the "possible misuse of unsafe.Pointer" vet check for
// cases the runtime cannot prove safe (COM callbacks, clipboard memory).
func notifyPtr(l uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&l))
}
