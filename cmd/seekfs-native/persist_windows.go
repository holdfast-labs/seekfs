//go:build windows

package main

import (
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// persist_windows.go: debounced settings persistence (window rect, columns,
// view, Contents toggle) plus save-on-exit.

var (
	saveMu    sync.Mutex
	saveTimer *time.Timer
)

// saveSoon schedules a settings write ~800ms out, coalescing bursts
// (column drags, typing in the scope box).
func (a *app) saveSoon() {
	saveMu.Lock()
	defer saveMu.Unlock()
	appRef := a
	if saveTimer != nil {
		saveTimer.Stop()
	}
	saveTimer = time.AfterFunc(800*time.Millisecond, func() { appRef.saveNow() })
}

// saveNow captures live window state and writes %APPDATA%\seekfs\native.json.
func (a *app) saveNow() {
	if a.hwnd == 0 {
		return
	}
	// Window rect (restored, not minimized/maximized).
	var wp windowPlacement
	wp.Length = windowPlacementSize()
	if getWindowPlacement(a.hwnd, &wp) {
		a.settings.WindowX = int(wp.RcNormal.Left)
		a.settings.WindowY = int(wp.RcNormal.Top)
		a.settings.WindowW = int(wp.RcNormal.width())
		a.settings.WindowH = int(wp.RcNormal.height())
		a.settings.Maximized = wp.ShowCmd == swShowMax
	}
	a.settings.SortColumn = a.sortKey.String()
	a.settings.SortAsc = a.sortAsc
	a.settings.View = a.view.String()
	a.settings.ContentsMode = a.contents
	a.settings.Under = a.under
	snapshot := a.settings
	widths := make(map[string]int, len(a.settings.ColumnWidths))
	for k, v := range a.settings.ColumnWidths {
		widths[k] = v
	}
	snapshot.ColumnWidths = widths
	go func() { _ = saveSettings(snapshot) }()
}

type windowPlacement struct {
	Length         uint32
	Flags          uint32
	ShowCmd        uint32
	PtMinPosition  point
	PtMaxPosition  point
	RcNormal       rect
}

func windowPlacementSize() uint32 { return 44 }

var (
	pGetWindowPlacement = proc(modUser32, "GetWindowPlacement")
)

func getWindowPlacement(hwnd uintptr, wp *windowPlacement) bool {
	r, _, _ := syscall.SyscallN(pGetWindowPlacement.Addr(), hwnd, uintptr(unsafe.Pointer(wp)))
	return r != 0
}
