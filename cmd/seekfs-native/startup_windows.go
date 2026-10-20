//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const nativeCreateNoWindow = 0x08000000

// prepareNativeServiceCommand hides the spawned service console window.
func prepareNativeServiceCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: nativeCreateNoWindow,
	}
}
