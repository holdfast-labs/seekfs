//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// status_windows.go: the bottom status bar — result count, query time,
// content health, index health. Parts: [results ............] [time] [contents] [health].

func (a *app) createStatusBar() {
	inst := instanceHandle()
	hwnd, _, _ := syscall.SyscallN(pCreateWindowExW.Addr(), 0,
		uintptr(unsafe.Pointer(utf16("msctls_statusbar32"))), 0,
		uintptr(wsChild|wsVisible|wsClipSiblings),
		0, 0, 10, 10,
		a.hwnd, uintptr(idStatusBar), inst, 0)
	a.statusBar = hwnd
	if f := a.font; f != 0 {
		sendMessage(hwnd, 0x0030 /*WM_SETFONT*/, f, 1)
	}
}

func (a *app) sizeStatusParts(totalW int32) {
	// Right-aligned fixed parts; the first part fills the rest.
	timeW := scaleDPI(150, a.dpi)
	contentW := scaleDPI(220, a.dpi)
	healthW := scaleDPI(170, a.dpi)
	parts := []int32{
		totalW - timeW - contentW - healthW,
		totalW - contentW - healthW,
		totalW - healthW,
		totalW,
	}
	a.statusParts = parts
	ints := make([]int32, len(parts))
	copy(ints, parts)
	sendMessage(a.statusBar, sbSetParts, uintptr(len(ints)), uintptr(unsafe.Pointer(&ints[0])))
}

func (a *app) setStatusPart(i int, s string) {
	sendMessage(a.statusBar, sbSetText, uintptr(i), uintptr(unsafe.Pointer(utf16(s))))
}

// refreshStatus recomputes all four parts from current state.
func (a *app) refreshStatus() {
	a.mu.Lock()
	n := len(a.sortedRows)
	if n == 0 {
		n = len(a.rows)
	}
	msg := a.fuzzyMsg
	elapsed := a.elapsedMS
	searchMS := a.searchMS
	content := a.content
	contentMode := a.contents
	unavail := a.contentUnavailableMsg
	health, healthMsg := a.indexHealth, a.indexMsg
	entries := a.indexEntries
	flash := a.pendingFlash
	a.pendingFlash = ""
	a.mu.Unlock()

	// Part 0: result count + message (a transient flash wins).
	countText := fmt.Sprintf("%s result%s", formatCount(n), plural(n))
	if msg != "" {
		countText += " — " + msg
	}
	if unavail != "" {
		countText = unavail
	}
	if flash != "" {
		countText = flash
	}
	a.setStatusPart(0, countText)

	// Part 1: query time.
	if elapsed > 0 || searchMS > 0 {
		a.setStatusPart(1, fmt.Sprintf("%d ms (service %.0f ms)", elapsed, searchMS))
	} else {
		a.setStatusPart(1, "")
	}

	// Part 2: content health (only meaningful in contents mode, but always
	// show when the service reports a state).
	contentText := ""
	if contentMode || (content != nil && content.State != "") {
		if unavail != "" {
			contentText = "Contents unavailable"
		} else {
			contentText = contentStatusText(content)
		}
	}
	a.setStatusPart(2, contentText)

	// Part 3: index health.
	healthText := ""
	switch health {
	case serviceHealthOK:
		healthText = fmt.Sprintf("Index ready (%s entries)", formatCount(entries))
	case serviceHealthDegraded:
		healthText = "Index: " + firstNonEmpty(healthMsg, "degraded")
	case serviceHealthError:
		healthText = "Index: " + firstNonEmpty(healthMsg, "error")
	default:
		if healthMsg != "" {
			healthText = healthMsg
		} else {
			healthText = "Connecting…"
		}
	}
	a.setStatusPart(3, healthText)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// initialInfo runs the service handshake off the UI thread, then updates
// health + volumes and posts a status refresh.
func (a *app) initialInfo() {
	heartbeat("info-start")
	resp, err := a.client.Info()
	heartbeat("info-done")
	a.mu.Lock()
	if err != nil {
		a.indexHealth = serviceHealthError
		a.indexMsg = err.Error()
	} else if resp.Loading {
		a.indexHealth = serviceHealthDegraded
		a.indexMsg = "Loading indexes…"
		a.indexEntries = resp.Entries
	} else if resp.OK && resp.Entries > 0 {
		a.indexHealth = firstNonEmpty(resp.Health, serviceHealthOK)
		a.indexMsg = resp.HealthMessage
		a.indexEntries = resp.Entries
	} else {
		a.indexHealth = serviceHealthError
		a.indexMsg = firstNonEmpty(resp.Message, "service has no search indexes loaded")
	}
	a.volumes = dbVolumesFrom(resp)
	a.mu.Unlock()
	postMessage(a.hwnd, msgStatusUpdate, 0, 0)
	// Rebuild the sidebar volumes section on the UI thread.
	postMessage(a.hwnd, msgVolumesUpdate, 0, 0)
	if err == nil {
		warmUpShellContextMenus(nil)
	}
}

const msgVolumesUpdate = wmApp + 4
