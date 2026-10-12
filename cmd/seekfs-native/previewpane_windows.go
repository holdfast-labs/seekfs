//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// previewpane_windows.go: the Quick Look-style preview pane UI — a container
// window hosting either a preview-handler child or a read-only text view,
// plus a header with the file name.

const previewHeaderH = 28 // 96-dpi

func (a *app) createPreview() {
	inst := instanceHandle()
	hwnd, _, _ := syscall.SyscallN(pCreateWindowExW.Addr(), uintptr(wsExClientEdge),
		uintptr(unsafe.Pointer(utf16("STATIC"))), 0,
		uintptr(wsChild|wsVisible|wsClipChildren),
		0, 0, 10, 10,
		a.hwnd, uintptr(idPreview), inst, 0)
	a.preview = hwnd

	// Header label with the file name.
	header, _, _ := syscall.SyscallN(pCreateWindowExW.Addr(), 0,
		uintptr(unsafe.Pointer(utf16("STATIC"))), 0,
		uintptr(wsChild|wsVisible|0x00000000 /*SS_LEFT*/),
		0, 0, 10, 10,
		hwnd, uintptr(idPreview+2), inst, 0)
	a.previewHeader = header
	if f := a.font; f != 0 {
		sendMessage(header, 0x0030 /*WM_SETFONT*/, f, 1)
	}

	// Read-only multi-line text view for the text fallback.
	edit, _, _ := syscall.SyscallN(pCreateWindowExW.Addr(), 0,
		uintptr(unsafe.Pointer(utf16("EDIT"))), 0,
		uintptr(wsChild|wsVisible|wsVScroll|0x0004 /*ES_MULTILINE*/|0x0040 /*ES_READONLY*/|0x0080 /*ES_AUTOVSCROLL*/),
		0, 0, 10, 10,
		hwnd, uintptr(idPreview+1), inst, 0)
	a.previewText = edit
	if f := a.font; f != 0 {
		sendMessage(edit, 0x0030 /*WM_SETFONT*/, f, 1)
	}
}

func (a *app) layoutPreview(x, y, w, h int32) {
	if a.preview == 0 {
		return
	}
	headerH := scaleDPI(previewHeaderH, a.dpi)
	pad := scaleDPI(6, a.dpi)
	moveWindow(a.previewHeader, pad, scaleDPI(4, a.dpi), w-2*pad, headerH-scaleDPI(4, a.dpi), true)
	moveWindow(a.previewText, 0, headerH, w, h-headerH, true)
	a.previewHost.resize(rect{Left: 0, Top: headerH, Right: w, Bottom: h})
}

// updatePreviewSelection shows the focused row: preview handler when
// available, text preview for text files, metadata card otherwise.
func (a *app) updatePreviewSelection() {
	rows := a.selectedRows()
	if len(rows) == 0 {
		a.mu.Lock()
		sorted := a.sortedRows
		a.mu.Unlock()
		focus := int(sendMessage(a.results, lvmGetSelectionMark, 0, 0))
		if focus >= 0 && focus < len(sorted) {
			rows = []ResultRow{sorted[focus]}
		}
	}
	if len(rows) == 0 {
		a.showPreviewEmpty()
		return
	}
	row := rows[0]
	if row.Path == a.previewPath {
		return
	}
	a.previewPath = row.Path
	a.previewHost.unload()
	setWindowText(a.previewHeader, row.Name)

	rc := getClientRect(a.preview)
	headerH := scaleDPI(previewHeaderH, a.dpi)
	hostRc := rect{Left: 0, Top: headerH, Right: rc.width(), Bottom: rc.height()}
	_, _, _ = syscall.SyscallN(pShowWindow.Addr(), a.previewText, 0 /*SW_HIDE*/)
	if a.previewHost.tryAttachHandler(a.preview, hostRc, row.Path) {
		return
	}
	// Fallback: text preview or metadata card.
	if text, ok := readTextPreview(row.Path); ok && textPreviewableExt(row.Path) {
		_, _, _ = syscall.SyscallN(pShowWindow.Addr(), a.previewText, swShow)
		setWindowText(a.previewText, text)
		a.previewText2 = text
		return
	}
	_, _, _ = syscall.SyscallN(pShowWindow.Addr(), a.previewText, swShow)
	setWindowText(a.previewText, previewMetadataCardFor(a, row))
	a.previewText2 = ""
}

func (a *app) showPreviewEmpty() {
	a.previewPath = ""
	a.previewHost.unload()
	setWindowText(a.previewHeader, "Preview")
	if a.previewText != 0 {
		_, _, _ = syscall.SyscallN(pShowWindow.Addr(), a.previewText, swShow)
		setWindowText(a.previewText, "Select a file to preview it here.")
	}
}

// previewTypeLabel returns the shell type name for the metadata card.
func (a *app) previewTypeLabel(r ResultRow) string {
	if r.IsDir {
		return "File folder"
	}
	if t := a.icons.typeName(r.Path, false); t != "" {
		return t
	}
	if r.Type != "" {
		return r.Type + " File"
	}
	return "File"
}

// previewMetadataCardFor renders name/path/size/date/type with the shell
// type name.
func previewMetadataCardFor(a *app, r ResultRow) string {
	out := r.Name + "\r\n" + r.Path + "\r\n\r\n"
	if !r.IsDir {
		out += a.previewTypeLabel(r) + "\r\n"
		if r.HasSize {
			out += formatByteSize(r.Size) + "\r\n"
		}
		if r.HasMod {
			out += formatSystemTime(r.Modified.Unix(), 0)
		}
	}
	if r.Snippet != "" {
		out += "\r\n\r\nMatch:\r\n" + r.Snippet
	}
	return out
}
