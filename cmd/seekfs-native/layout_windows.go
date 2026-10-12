//go:build windows

package main

// layout_windows.go: DPI-aware control layout for the main window.
//
//   toolbar (top, 40px at 96dpi): back | fwd | search (fills) | scope (180px) |
//       Contents | view
//   body: sidebar (220px) | results (fills) | preview (320px when open)
//   status bar (bottom, 24px)

func (a *app) layout() {
	if a.hwnd == 0 {
		return
	}
	rc := getClientRect(a.hwnd)
	W, H := rc.width(), rc.height()
	if W <= 0 || H <= 0 {
		return
	}
	dpi := a.dpi

	toolbarH := scaleDPI(40, dpi)
	statusH := scaleDPI(26, dpi)
	sideW := scaleDPI(220, dpi)
	prevW := scaleDPI(330, dpi)
	if !a.previewOpen() {
		prevW = 0
	}
	gap := scaleDPI(8, dpi)
	btnW := scaleDPI(52, dpi)
	toggleW := scaleDPI(96, dpi)
	viewW := scaleDPI(88, dpi)
	scopeW := scaleDPI(170, dpi)

	// Status bar at the bottom.
	moveWindow(a.statusBar, 0, H-statusH, W, statusH, true)
	a.sizeStatusParts(W)

	// Toolbar.
	y := scaleDPI(6, dpi)
	bh := toolbarH - 2*y
	x := gap
	moveWindow(a.backBtn, x, y, btnW, bh, true)
	x += btnW + scaleDPI(4, dpi)
	moveWindow(a.fwdBtn, x, y, btnW, bh, true)
	x += btnW + gap
	right := W - gap
	moveWindow(a.viewBtn, right-viewW, y, viewW, bh, true)
	right -= viewW + scaleDPI(4, dpi)
	moveWindow(a.contentsBtn, right-toggleW, y, toggleW, bh, true)
	right -= toggleW + gap
	scopeX := right - scopeW
	moveWindow(a.scopeBox, scopeX, y, scopeW, bh, true)
	right = scopeX - gap
	moveWindow(a.searchBox, x, y, right-x, bh, true)

	// Body.
	bodyY := toolbarH
	bodyH := H - toolbarH - statusH
	moveWindow(a.sidebar, 0, bodyY, sideW, bodyH, true)
	rx := sideW
	rw := W - sideW - prevW
	if rw < 0 {
		rw = 0
	}
	moveWindow(a.results, rx, bodyY, rw, bodyH, true)
	if prevW > 0 {
		moveWindow(a.preview, rx+rw, bodyY, prevW, bodyH, true)
		a.layoutPreview(rx+rw, bodyY, prevW, bodyH)
	}
}

func (a *app) previewOpen() bool {
	if a.settings.PreviewOpen {
		return true
	}
	// Default open on wide windows until the user toggles.
	rc := getClientRect(a.hwnd)
	return rc.width() > scaleDPI(900, a.dpi)
}

func (a *app) togglePreview() {
	// Explicit toggle: if settings say open, close; else open.
	wide := getClientRect(a.hwnd).width() > scaleDPI(900, a.dpi)
	if a.settings.PreviewOpen || (!a.explicitPreview && wide) {
		a.settings.PreviewOpen = false
		a.explicitPreview = true
	} else {
		a.settings.PreviewOpen = true
		a.explicitPreview = true
	}
	a.layout()
	a.updatePreviewSelection()
}
