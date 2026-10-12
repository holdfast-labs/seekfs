//go:build windows

package main

import (
	"gioui.org/app"
)

// gview_windows.go: captures the HWND from Gio's ViewEvent for frame
// theming (Mica + dark title bar) and shell operations (menus, drag-drop).

func (u *UI) onViewEvent(e app.ViewEvent) {
	if ev, ok := e.(app.Win32ViewEvent); ok {
		if ev.Valid() {
			u.mu.Lock()
			u.hwnd = ev.HWND
			u.mu.Unlock()
			u.shell.SetHWND(ev.HWND)
			u.shell.ApplyFrame(u.dark)
		}
	}
}
