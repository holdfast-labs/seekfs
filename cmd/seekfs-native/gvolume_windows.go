//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// gvolume_windows.go: friendly volume names ("Local Disk (E:)") via
// GetVolumeInformationW.

var pGetVolumeInformationW = proc(modKernel32, "GetVolumeInformationW")

// volumeLabel returns "Label (X:)" for a volume root like "E:\", or "" when
// the name can't be read.
func volumeLabel(root string) string {
	if root == "" {
		return ""
	}
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return ""
	}
	var name [261]uint16 // MAX_PATH+1
	r, _, _ := syscall.SyscallN(pGetVolumeInformationW.Addr(),
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&name[0])), 261,
		0, 0, 0, 0, 0)
	if r == 0 {
		return ""
	}
	label := syscall.UTF16ToString(name[:])
	if label == "" {
		label = "Local Disk"
	}
	vol := root
	if len(vol) >= 2 && vol[1] == ':' {
		vol = vol[:2]
	}
	return label + " (" + vol + ")"
}
