//go:build windows

package main

import (
	"errors"
	"syscall"
	"unsafe"
)

// folderpicker_windows.go: SHBrowseForFolderW folder selection for the
// content roots and exclusions lists.

const (
	bifReturnOnlyFSDirs = 0x0001
	bifEditBox          = 0x0010
	bifNewDialogStyle   = 0x0040
	bifUseNewUI         = bifEditBox | bifNewDialogStyle
	maxPath             = 32768
)

type browseInfoW struct {
	HwndOwner      uintptr
	PidlRoot       uintptr
	PszDisplayName *uint16
	LpszTitle      *uint16
	UlFlags        uint32
	Lpfn           uintptr
	LParam         uintptr
	IImage         int32
}

var (
	pSHBrowseForFolderW   = proc(modShell32, "SHBrowseForFolderW")
	pSHGetPathFromIDListW = proc(modShell32, "SHGetPathFromIDListW")
)

func browseForFolder(owner uintptr, title string) (string, error) {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	display := make([]uint16, maxPath)
	bi := browseInfoW{
		HwndOwner:      owner,
		PszDisplayName: &display[0],
		LpszTitle:      titlePtr,
		UlFlags:        bifReturnOnlyFSDirs | bifUseNewUI,
	}
	pidl, _, _ := syscall.SyscallN(pSHBrowseForFolderW.Addr(), uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return "", errors.New("no folder selected")
	}
	defer syscall.SyscallN(pCoTaskMemFree.Addr(), pidl)
	buf := make([]uint16, maxPath)
	ok, _, _ := syscall.SyscallN(pSHGetPathFromIDListW.Addr(), pidl, uintptr(unsafe.Pointer(&buf[0])))
	if ok == 0 {
		return "", errors.New("cannot read the selected folder")
	}
	return syscall.UTF16ToString(buf), nil
}

func (b *winBridge) PickFolder(title string) (string, error) {
	b.mu.Lock()
	hwnd := b.hwnd
	b.mu.Unlock()
	return browseForFolder(hwnd, title)
}
