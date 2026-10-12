//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// toolbar_windows.go: the top toolbar — back/forward buttons, the search box
// (instant as-you-type), the Contents toggle, the view switcher, and the
// scope ("under:") box.

const (
	idScopeBox = 107

	accelFocusSearch = 1001
	accelOpen        = 1002
	accelReveal      = 1003
	accelRename      = 1004
	accelDelete      = 1005
	accelCopy        = 1006
	accelSelectAll   = 1007
	accelRefresh     = 1008
	accelPreview     = 1009
)

func (a *app) createToolbar() {
	inst := instanceHandle()
	mkPlainBtn := func(id int, text string) uintptr {
		hwnd, _, _ := syscall.SyscallN(pCreateWindowExW.Addr(), 0,
			uintptr(unsafe.Pointer(utf16("BUTTON"))),
			uintptr(unsafe.Pointer(utf16(text))),
			uintptr(wsChild|wsVisible|wsTabStop),
			0, 0, 10, 10,
			a.hwnd, uintptr(id), inst, 0)
		if f := a.font; f != 0 {
			sendMessage(hwnd, 0x0030 /*WM_SETFONT*/, f, 1)
		}
		return hwnd
	}
	a.backBtn = mkPlainBtn(idBackBtn, "Back")
	a.fwdBtn = mkPlainBtn(idFwdBtn, "Fwd")
	a.contentsBtn = mkPlainBtn(idContentsBtn, "Contents")
	a.viewBtn = mkPlainBtn(idViewBtn, "Details")
	setWindowText(a.viewBtn, viewButtonLabel(a.view))

	// Scope box (under:).
	scope, _, _ := syscall.SyscallN(pCreateWindowExW.Addr(), uintptr(wsExClientEdge),
		uintptr(unsafe.Pointer(utf16("EDIT"))),
		uintptr(unsafe.Pointer(utf16(a.under))),
		uintptr(wsChild|wsVisible|wsTabStop|0x00000080 /*ES_AUTOHSCROLL*/|ecLeft),
		0, 0, 10, 10,
		a.hwnd, uintptr(idScopeBox), inst, 0)
	a.scopeBox = scope
	if f := a.font; f != 0 {
		sendMessage(scope, 0x0030, f, 1)
	}
	sendMessage(scope, emSetCueBanner, 1, uintptr(unsafe.Pointer(utf16("Folder scope"))))
	subclassControl(scope)
	setControlHandler(scope, a.scopeProc)

	// Search box.
	search, _, _ := syscall.SyscallN(pCreateWindowExW.Addr(), uintptr(wsExClientEdge),
		uintptr(unsafe.Pointer(utf16("EDIT"))),
		0,
		uintptr(wsChild|wsVisible|wsTabStop|0x00000080 /*ES_AUTOHSCROLL*/|ecLeft),
		0, 0, 10, 10,
		a.hwnd, uintptr(idSearchBox), inst, 0)
	a.searchBox = search
	if f := a.font; f != 0 {
		sendMessage(search, 0x0030, f, 1)
	}
	sendMessage(search, emSetCueBanner, 1, uintptr(unsafe.Pointer(utf16("Search"))))
	subclassControl(search)
	setControlHandler(search, a.searchBoxProc)

	a.refreshContentsButton()
	a.refreshGoButton()
}

// searchBoxProc handles Enter/Escape/arrows before the edit control sees
// them, then falls through to the original proc.
func (a *app) searchBoxProc(hwnd uintptr, msg uint32, w, l uintptr) (uintptr, bool) {
	h := controlHandlers[hwnd]
	switch msg {
	case wmKeyDown:
		switch int(w) {
		case vkReturn:
			a.searchNow()
			return 0, true
		case vkEscape:
			setWindowText(hwnd, "")
			return 0, true
		case vkDown:
			a.focusResultsMove(1)
			return 0, true
		case vkUp:
			a.focusResultsMove(-1)
			return 0, true
		}
	case wmChar:
		// Ctrl+A select-all inside the search box (edit controls with
		// ES_MULTILINE off beep otherwise on some layouts).
		if w == 1 { // Ctrl+A
			sendMessage(hwnd, 0x00B1 /*EM_SETSEL*/, 0, ^uintptr(0))
			return 0, true
		}
	}
	return defControlProc(h, hwnd, msg, w, l), true
}

// scopeProc commits the scope on Enter.
func (a *app) scopeProc(hwnd uintptr, msg uint32, w, l uintptr) (uintptr, bool) {
	h := controlHandlers[hwnd]
	if msg == wmKeyDown && int(w) == vkReturn {
		a.under = getWindowText(hwnd)
		a.rerun()
		_, _, _ = syscall.SyscallN(pSetFocus.Addr(), a.searchBox)
		return 0, true
	}
	return defControlProc(h, hwnd, msg, w, l), true
}

// onSearchTyped fires on every keystroke: debounced live search.
func (a *app) onSearchTyped() {
	text := getWindowText(a.searchBox)
	a.query = text
	a.searcher.Query(text, a.contents, a.under)
	a.updateNavButtons()
}

// searchNow bypasses the debounce (Enter pressed).
func (a *app) searchNow() {
	text := getWindowText(a.searchBox)
	a.query = text
	a.searcher.SearchNow(text, a.contents, a.under)
}

// rerun re-issues the current query (F5, scope change, view/sort change does
// not need it since sorting is client-side, but a refresh does).
func (a *app) rerun() {
	text := getWindowText(a.searchBox)
	a.query = text
	a.searcher.SearchNow(text, a.contents, a.under)
}

func (a *app) toggleContents() {
	a.contents = !a.contents
	a.settings.ContentsMode = a.contents
	a.refreshContentsButton()
	a.rerun()
}

func (a *app) refreshContentsButton() {
	// BS_AUTOCHECKBOX renders a real theme checkbox with the label.
	style := getWindowLong(a.contentsBtn, gwlStyle)
	const bsMask = uintptr(0xF)
	const bsAutoCheckbox = uintptr(0x3)
	style = (style &^ bsMask) | bsAutoCheckbox
	setWindowLong(a.contentsBtn, gwlStyle, style)
	if a.contents {
		sendMessage(a.contentsBtn, bmSetCheck, bstChecked, 0)
	} else {
		sendMessage(a.contentsBtn, bmSetCheck, bstUnchecked, 0)
	}
	setWindowText(a.contentsBtn, "Contents")
	invalidate(a.contentsBtn)
}

func (a *app) cycleView() {
	switch a.view {
	case ViewDetails:
		a.view = ViewIcons
	case ViewIcons:
		a.view = ViewGrouped
	default:
		a.view = ViewDetails
	}
	a.settings.View = a.view.String()
	setWindowText(a.viewBtn, viewButtonLabel(a.view))
	a.applyViewMode()
}

func viewButtonLabel(m ViewMode) string {
	switch m {
	case ViewIcons:
		return "Icons"
	case ViewGrouped:
		return "Grouped"
	default:
		return "Details"
	}
}

func (a *app) refreshGoButton() {
	_ = 0
}

func (a *app) onScopeGo() {
	a.under = getWindowText(a.scopeBox)
	a.rerun()
}

func (a *app) goBack() {
	e, ok := a.history.Back()
	if !ok {
		return
	}
	a.restoreHistory(e)
}

func (a *app) goForward() {
	e, ok := a.history.Forward()
	if !ok {
		return
	}
	a.restoreHistory(e)
}

func (a *app) restoreHistory(e HistoryEntry) {
	setWindowText(a.searchBox, e.Query)
	setWindowText(a.scopeBox, e.Under)
	a.query = e.Query
	a.under = e.Under
	if e.ContentsMode != a.contents {
		a.contents = e.ContentsMode
		a.refreshContentsButton()
	}
	a.searcher.SearchNow(e.Query, e.ContentsMode, e.Under)
	a.updateNavButtons()
}

func (a *app) updateNavButtons() {
	sendMessage(a.backBtn, 0x00F4 /*BM_SETSTATE*/, 0, 0)
	_, _, _ = syscall.SyscallN(pEnableWindow.Addr(), a.backBtn, boolToUintptr(a.history.CanBack()))
	_, _, _ = syscall.SyscallN(pEnableWindow.Addr(), a.fwdBtn, boolToUintptr(a.history.CanForward()))
}

func boolToUintptr(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}
