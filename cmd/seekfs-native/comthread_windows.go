//go:build windows

package main

import (
	"syscall"
)

// comthread_windows.go: tiny wrappers for COM apartment setup on dedicated
// threads (context-menu service, preview handlers).

func oleInitialize() error {
	r, _, _ := syscall.SyscallN(pOleInitialize.Addr(), 0)
	if r == 0 || r == 1 /*S_FALSE*/ {
		return nil
	}
	return syscall.Errno(uint32(r))
}

func oleUninitialize() {
	_, _, _ = syscall.SyscallN(pOleUninitialize.Addr())
}
