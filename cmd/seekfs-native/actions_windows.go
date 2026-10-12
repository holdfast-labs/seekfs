//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// actions_windows.go: verbs on the selection — Enter opens, Ctrl+Enter
// reveals in Explorer, F2 in-place rename, Delete to the Recycle Bin, Ctrl+C
// copies files, drag-and-drop out, Properties.

func (a *app) openSelected() {
	for _, p := range a.selectedPaths() {
		if err := shellOpen(p); err != nil {
			a.flashStatus("Open failed: " + err.Error())
			return
		}
	}
}

func (a *app) revealSelected() {
	paths := a.selectedPaths()
	if len(paths) == 0 {
		return
	}
	if err := shellReveal(paths[0]); err != nil {
		a.flashStatus("Reveal failed: " + err.Error())
	}
}

func (a *app) propertiesSelected() {
	for _, p := range a.selectedPaths() {
		if err := shellProperties(p); err != nil {
			a.flashStatus("Properties failed: " + err.Error())
			return
		}
	}
}

func (a *app) copySelected() {
	paths := a.selectedPaths()
	if len(paths) == 0 {
		return
	}
	if err := copyPathsToClipboard(paths); err != nil {
		a.flashStatus("Copy failed: " + err.Error())
		return
	}
	a.flashStatus("Copied " + itoa(len(paths)) + " item(s)")
}

func (a *app) deleteSelected() {
	paths := a.selectedPaths()
	if len(paths) == 0 {
		return
	}
	if err := recyclePaths(a.hwnd, paths); err != nil {
		if !strings.Contains(err.Error(), "cancelled") {
			a.flashStatus("Delete failed: " + err.Error())
		}
		return
	}
	a.flashStatus("Moved " + itoa(len(paths)) + " item(s) to the Recycle Bin")
	a.rerun()
}

// beginRename starts in-place rename (F2) on the focused row.
func (a *app) beginRename() {
	i := a.selectedIndex()
	if i < 0 {
		return
	}
	sendMessage(a.results, lvmEditLabelW, uintptr(i), 0)
}

func (a *app) onBeginLabelEdit(di *lvDispInfo) uintptr {
	// Allow editing only the name column.
	if di.Item.ISubItem != 0 {
		return 1 // TRUE = prevent
	}
	return 0
}

func (a *app) onEndLabelEdit(di *lvDispInfo) uintptr {
	if di.Item.PszText == nil {
		return 0
	}
	newName := strings.TrimSpace(syscall.UTF16ToString(unsafe.Slice(di.Item.PszText, 260)))
	i := int(di.Item.IItem)
	a.mu.Lock()
	rows := a.sortedRows
	a.mu.Unlock()
	if i < 0 || i >= len(rows) || newName == "" || newName == rows[i].Name {
		return 0
	}
	oldPath := rows[i].Path
	if strings.ContainsAny(newName, `\/:*?"<>|`) {
		a.flashStatus("Invalid file name " + newName)
		return 0
	}
	target := filepath.Join(filepath.Dir(oldPath), newName)
	if err := os.Rename(oldPath, target); err != nil {
		a.flashStatus("Rename failed: " + err.Error())
		return 0
	}
	a.rerun()
	return 1
}

// showContextMenu shows the real Explorer shell menu for the selection. If
// nothing is selected it selects the focused/hovered row first.
func (a *app) showContextMenu() {
	paths := a.selectedPaths()
	if len(paths) == 0 {
		// Hit-test the cursor position and select that row.
		x, y, ok := cursorScreenPos()
		if ok {
			pt := point{X: x, Y: y}
			_, _, _ = syscall.SyscallN(pScreenToClient.Addr(), a.results, uintptr(unsafe.Pointer(&pt)))
			var ht lvHitTestInfo
			ht.Pt = pt
			if sendMessage(a.results, lvmHitTest, 0, uintptr(unsafe.Pointer(&ht))); int(ht.IItem) >= 0 {
				var item lvItem
				item.Mask = lvifState
				item.IItem = ht.IItem
				item.State = lvisSelected | lvisFocused
				item.StateMask = lvisSelected | lvisFocused
				sendMessage(a.results, lvmSetItemState, uintptr(ht.IItem), uintptr(unsafe.Pointer(&item)))
				paths = a.selectedPaths()
			}
		}
	}
	if len(paths) == 0 {
		return
	}
	resCh := make(chan ctxMenuResult, 1)
	go showShellContextMenuAsync(a.hwnd, paths, resCh)
	go func() {
		res := <-resCh
		if res.err != nil {
			a.mu.Lock()
			a.pendingFlash = "Menu failed: " + res.err.Error()
			a.mu.Unlock()
			postMessage(a.hwnd, msgStatusUpdate, 0, 0)
		}
	}()
}

// beginDrag starts a drag-and-drop out to Explorer on LVN_BEGINDRAG.
// DoDragDrop is modal with its own message loop and must run on the UI
// (OLE) thread, so this runs synchronously in the notification handler.
func (a *app) beginDrag() {
	paths := a.selectedPaths()
	if len(paths) == 0 {
		return
	}
	if err := doDragDropFiles(a.hwnd, paths); err != nil {
		a.flashStatus("Drag failed: " + err.Error())
	}
}

func (a *app) flashStatus(s string) {
	a.mu.Lock()
	a.pendingFlash = s
	a.mu.Unlock()
	postMessage(a.hwnd, msgStatusUpdate, 0, 0)
}
