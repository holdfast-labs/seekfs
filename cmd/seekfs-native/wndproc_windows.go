//go:build windows

package main

import (
	"syscall"
)

// wndproc_windows.go: the main window procedure — layout on WM_SIZE,
// command routing, list/tree notifications, theme/DPI changes, shutdown.

func (a *app) wndProc(msg uint32, w, l uintptr) uintptr {
	_ = l
	switch msg {
	case wmCreate:
		return 0
	case wmSize:
		if w != sizeMinimized {
			a.layout()
		}
		return 0
	case wmClose:
		_, _, _ = syscall.SyscallN(pDestroyWindow.Addr(), a.hwnd)
		return 0
	case wmDestroy:
		a.saveNow()
		_, _, _ = syscall.SyscallN(pPostQuitMessage.Addr(), 0)
		return 0
	case wmCommand:
		return a.onCommand(w, l)
	case wmNotify:
		return a.onNotify(w, l, msg)
	case wmCtlColorEdit, wmCtlColorStatic, wmCtlColorListBox:
		return a.onCtlColor(w, uintptr(l), msg)
	case wmSettingChange, wmThemeChanged:
		dark := systemUsesDarkMode()
		if dark != a.dark {
			a.dark = dark
			a.applyTheme()
		}
		return 0
		case wmDpiChanged:
		a.dpi = int(hiword(w))
		a.onDPIChanged()
		// lParam points to the suggested rect; honor it.
		rc := notifyLPARAM[*rect](l)
		moveWindow(a.hwnd, rc.Left, rc.Top, rc.width(), rc.height(), true)
		return 0
	case msgSearchResult:
		a.deliverPendingResult()
		return 0
	case msgStatusUpdate:
		a.refreshStatus()
		return 0
	case msgVolumesUpdate:
		a.refreshSidebar()
		return 0
	}
	r, _, _ := syscall.SyscallN(pDefWindowProcW.Addr(), a.hwnd, uintptr(msg), w, l)
	return r
}

func loword(v uintptr) uint16 { return uint16(v & 0xffff) }
func hiword(v uintptr) uint16 { return uint16((v >> 16) & 0xffff) }

// onCommand routes button clicks, menu items and accelerator commands.
func (a *app) onCommand(w, l uintptr) uintptr {
	id := loword(w)
	code := hiword(w)
	switch int(id) {
	case idSearchBox:
		if code == enChange {
			a.onSearchTyped()
		}
		return 0
	case idBackBtn:
		a.goBack()
		return 0
	case idFwdBtn:
		a.goForward()
		return 0
	case idContentsBtn:
		if code == 0 /*BN_CLICKED*/ {
			a.toggleContents()
		}
		return 0
	case idViewBtn:
		a.cycleView()
		return 0
	case idGoBtn:
		a.refreshGoButton()
		a.onScopeGo()
		return 0
	}
	// Accelerator commands (see createAccelerators for IDs).
	switch int(id) {
	case accelFocusSearch:
		_, _, _ = syscall.SyscallN(pSetFocus.Addr(), a.searchBox)
		return 0
	case accelOpen:
		a.openSelected()
		return 0
	case accelReveal:
		a.revealSelected()
		return 0
	case accelRename:
		a.beginRename()
		return 0
	case accelDelete:
		a.deleteSelected()
		return 0
	case accelCopy:
		a.copySelected()
		return 0
	case accelSelectAll:
		a.selectAll()
		return 0
	case accelRefresh:
		a.rerun()
		return 0
	case accelPreview:
		a.togglePreview()
		return 0
	}
	return 0
}

// onNotify routes ListView/TreeView/header notifications.
func (a *app) onNotify(w, l uintptr, msg uint32) uintptr {
	_ = msg
	if l == 0 {
		return 0
	}
	hdr := (*nmhdr)(notifyPtr(l))
	switch hdr.HwndFrom {
	case a.results:
		return a.onResultsNotify(hdr, l)
	case a.sidebar:
		return a.onSidebarNotify(hdr, l)
	}
	// Header notifications come from the ListView's header child.
	if hdr.HwndFrom == a.resultsHeader() {
		return a.onHeaderNotify(hdr)
	}
	return 0
}

// onCtlColor paints edit/static controls with the palette so dark mode is
// complete even where the uxtheme "Explorer" theme leaves gaps.
// hdc is the message wParam, hwndCtl the lParam.
func (a *app) onCtlColor(hdc, hwndCtl uintptr, msg uint32) uintptr {
	var bg, fg uintptr
	switch hwndCtl {
	case a.searchBox, a.scopeBox:
		bg, fg = a.pal.editBg, a.pal.editText
	case a.statusBar:
		bg, fg = a.pal.statusBg, a.pal.statusText
	default:
		// Labels and buttons: match the window background.
		bg, fg = a.pal.windowBg, a.pal.text
	}
	return a.ctlColorBrush(bg, fg, hdc)
}

func (a *app) ctlColorBrush(bg, fg, hdc uintptr) uintptr {
	if hdc != 0 {
		_, _, _ = syscall.SyscallN(pSetBkColor.Addr(), hdc, bg)
		_, _, _ = syscall.SyscallN(pSetTextColor.Addr(), hdc, fg)
		_, _, _ = syscall.SyscallN(pSetBkMode.Addr(), hdc, opaque)
	}
	if b, ok := a.brushes[bg]; ok {
		return b
	}
	b, _, _ := syscall.SyscallN(pCreateSolidBrush.Addr(), bg)
	a.brushes[bg] = b
	return b
}
