//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// shouldTranslateAccelerator reports whether the accelerator table may see
// this message. Edit boxes keep their own keys (Ctrl+C copy, Ctrl+A select
// all, Enter commit, Delete); only Ctrl+Enter passes through to reveal, plus
// non-character keys (F2/F5/Delete at list level are handled by the list's
// own LVN_KEYDOWN path anyway).
func (a *app) shouldTranslateAccelerator(m *msgT) bool {
	if m.Message != wmKeyDown && m.Message != 0x0104 /*WM_SYSKEYDOWN*/ {
		return true
	}
	if !isEditFocused() {
		return true
	}
	// In an edit box: allow only Ctrl+Enter (reveal).
	if m.WParam == vkReturn && keyDown(vkControlP) {
		return true
	}
	return false
}

// newAccelTable creates the accelerator table from the slice. ACCEL is 6
// bytes (BYTE fVirt, WORD key, WORD cmd) matching the accel struct layout.
func newAccelTable(accels []accel) uintptr {
	h, _, _ := syscall.SyscallN(pCreateAcceleratorTableW.Addr(),
		uintptr(unsafe.Pointer(&accels[0])), uintptr(len(accels)))
	return h
}

// theme_apply_windows.go: applying the theme to all controls, DPI changes,
// accelerators, and settings persistence.

// applyTheme re-reads the palette and repaints everything.
func (a *app) applyTheme() {
	a.pal = paletteFor(a.dark)
	applyWindowTheme(a.hwnd, a.dark, []uintptr{a.results, a.sidebar})
	// ListView colors.
	sendMessage(a.results, lvmSetBkColor, 0, a.pal.windowBg)
	sendMessage(a.results, lvmSetTextBkColor, 0, a.pal.windowBg)
	sendMessage(a.results, lvmSetTextColor, 0, a.pal.text)
	// TreeView colors.
	sendMessage(a.sidebar, tvmSetBkColor, 0, a.pal.windowBg)
	sendMessage(a.sidebar, tvmSetTextColor, 0, a.pal.text)
	// Drop cached brushes (COLORREF keys differ per mode anyway).
	for _, b := range a.brushes {
		_, _, _ = syscall.SyscallN(pDeleteObject.Addr(), b)
	}
	a.brushes = map[uintptr]uintptr{}
	invalidate(a.hwnd)
	invalidate(a.results)
	invalidate(a.sidebar)
	invalidate(a.preview)
	a.refreshStatus()
}

func (a *app) onDPIChanged() {
	a.dpi = windowDPI(a.hwnd)
	if a.font != 0 {
		_, _, _ = syscall.SyscallN(pDeleteObject.Addr(), a.font)
	}
	a.font = systemUIFont(a.dpi)
	for _, hwnd := range []uintptr{a.searchBox, a.scopeBox, a.backBtn, a.fwdBtn, a.contentsBtn, a.viewBtn, a.results, a.sidebar, a.statusBar, a.previewText} {
		if hwnd != 0 {
			sendMessage(hwnd, 0x0030 /*WM_SETFONT*/, a.font, 1)
		}
	}
	a.setupResultColumns()
	a.refreshRows()
	a.layout()
}

// createAccelerators builds the keyboard table:
// Ctrl+F/Ctrl+L focus search, Ctrl+Enter reveal, F2 rename, Delete recycle,
// Ctrl+C copy, Ctrl+A select all, F5 refresh, Alt+P preview.
// (Plain Enter is handled by the results list itself.)
func (a *app) createAccelerators() {
	const fVirtKey = 0x01
	const fControl = 0x08
	const fAlt = 0x10
	accels := []accel{
		{FVirt: fControl | fVirtKey, Key: uint16('F'), Cmd: accelFocusSearch},
		{FVirt: fControl | fVirtKey, Key: uint16('L'), Cmd: accelFocusSearch},
		{FVirt: fControl | fVirtKey, Key: vkReturn, Cmd: accelReveal},
		{FVirt: fVirtKey, Key: vkF2, Cmd: accelRename},
		{FVirt: fVirtKey, Key: vkDelete, Cmd: accelDelete},
		{FVirt: fControl | fVirtKey, Key: uint16('C'), Cmd: accelCopy},
		{FVirt: fControl | fVirtKey, Key: uint16('A'), Cmd: accelSelectAll},
		{FVirt: fVirtKey, Key: vkF5, Cmd: accelRefresh},
		{FVirt: fAlt | fVirtKey, Key: uint16('P'), Cmd: accelPreview},
	}
	if len(accels) == 0 {
		return
	}
	// ACCEL is 6 bytes (BYTE fVirt, WORD key, WORD cmd); the accel struct
	// matches that layout exactly. Import the unsafe package for the slice
	// pointer below.
	a.accelTable = newAccelTable(accels)
}
