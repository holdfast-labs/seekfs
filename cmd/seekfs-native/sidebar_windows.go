//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// sidebar_windows.go: the navigation TreeView — Favorites/Quick access,
// indexed volumes from the service info response, and saved searches.
// Selecting a scope node sets under:; selecting a saved search runs it.

func (a *app) createSidebar() {
	inst := instanceHandle()
	hwnd, _, _ := syscall.SyscallN(pCreateWindowExW.Addr(), uintptr(wsExClientEdge),
		uintptr(unsafe.Pointer(utf16("SysTreeView32"))), 0,
		uintptr(wsChild|wsVisible|wsClipSiblings|tvsHasButtons|tvsHasLines|tvsLinesAtRoot|tvsShowSelAlways|tvsFullRowSelect),
		0, 0, 10, 10,
		a.hwnd, uintptr(idSidebar), inst, 0)
	a.sidebar = hwnd
	if f := a.font; f != 0 {
		sendMessage(hwnd, 0x0030 /*WM_SETFONT*/, f, 1)
	}
	if a.iconSmall != 0 {
		sendMessage(hwnd, tvmSetImageList, 0 /*TVSIL_NORMAL*/, a.iconSmall)
	}
	subclassControl(hwnd)
	setControlHandler(hwnd, a.sidebarProc)
}

// sidebarProc handles keyboard (arrows move, Enter activates) and forwards.
func (a *app) sidebarProc(hwnd uintptr, msg uint32, w, l uintptr) (uintptr, bool) {
	h := controlHandlers[hwnd]
	if msg == wmKeyDown {
		switch int(w) {
		case vkReturn:
			a.activateSidebarSelection()
			return 0, true
		case vkRight, vkLeft:
			// Let the tree handle expand/collapse natively.
		}
		if w == uintptr('F') && keyDown(vkControlP) {
			_, _, _ = syscall.SyscallN(pSetFocus.Addr(), a.searchBox)
			return 0, true
		}
	}
	return defControlProc(h, hwnd, msg, w, l), true
}

// refreshSidebar rebuilds the tree: Favorites, Volumes, Saved Searches.
func (a *app) refreshSidebar() {
	sendMessage(a.sidebar, tvmDeleteItem, 0, tviRoot)
	a.sidebarRoots = nil

	favRoot := a.insertSidebarNode(tviRoot, "Favorites", "", "", -1)
	a.sidebarRoots = append(a.sidebarRoots, sidebarNode{handle: favRoot, label: "Favorites"})
	for _, fav := range quickAccessPaths() {
		h := a.insertSidebarNode(favRoot, fav.label, fav.path, "", a.icons.iconIndex(fav.path, true))
		a.sidebarRoots = append(a.sidebarRoots, sidebarNode{handle: h, label: fav.label, under: fav.path})
	}
	sendMessage(a.sidebar, tvmExpand, tveExpand, favRoot)

	volRoot := a.insertSidebarNode(tviRoot, "Volumes", "", "", -1)
	a.sidebarRoots = append(a.sidebarRoots, sidebarNode{handle: volRoot, label: "Volumes"})
	a.mu.Lock()
	vols := append([]DBVolume(nil), a.volumes...)
	a.mu.Unlock()
	for _, v := range vols {
		label := v.Volume
		if label == "" {
			label = v.Path
		}
		if v.Entries > 0 {
			label += " (" + formatCount(v.Entries) + ")"
		}
		under := ""
		if v.Volume != "" {
			under = v.Volume + `\`
		}
		h := a.insertSidebarNode(volRoot, label, under, "", a.icons.iconIndex(v.Path, true))
		a.sidebarRoots = append(a.sidebarRoots, sidebarNode{handle: h, label: label, under: under})
	}
	sendMessage(a.sidebar, tvmExpand, tveExpand, volRoot)

	savedRoot := a.insertSidebarNode(tviRoot, "Saved searches", "", "", -1)
	a.sidebarRoots = append(a.sidebarRoots, sidebarNode{handle: savedRoot, label: "Saved searches"})
	for _, q := range a.settings.Saved {
		h := a.insertSidebarNode(savedRoot, q, "", q, -1)
		a.sidebarRoots = append(a.sidebarRoots, sidebarNode{handle: h, label: q, query: q})
	}
	sendMessage(a.sidebar, tvmExpand, tveExpand, savedRoot)
}

type quickFav struct{ label, path string }

// quickAccessPaths returns existing well-known folders for Favorites.
func quickAccessPaths() []quickFav {
	var out []quickFav
	home, _ := os.UserHomeDir()
	candidates := []quickFav{
		{"Desktop", filepath.Join(home, "Desktop")},
		{"Documents", filepath.Join(home, "Documents")},
		{"Downloads", filepath.Join(home, "Downloads")},
		{"Pictures", filepath.Join(home, "Pictures")},
		{"Music", filepath.Join(home, "Music")},
		{"Videos", filepath.Join(home, "Videos")},
	}
	for _, c := range candidates {
		if st, err := os.Stat(c.path); err == nil && st.IsDir() {
			out = append(out, c)
		}
	}
	return out
}

func (a *app) insertSidebarNode(parent uintptr, label, under, query string, icon int32) uintptr {
	text, _ := syscall.UTF16PtrFromString(label)
	item := tvInsertStruct{
		HParent:      parent,
		HInsertAfter: tviLast,
		Item: tvItem{
			Mask: tvifText | tvifParam,
			PszText: text,
			LParam: uintptr(len(a.sidebarRoots)),
		},
	}
	if icon >= 0 {
		item.Item.Mask |= tvifImage | tvifSelectedImage
		item.Item.IImage = int32(icon)
		item.Item.ISelectedImage = int32(icon)
	}
	_ = under
	_ = query
	return sendMessage(a.sidebar, tvmInsertItemW, 0, uintptr(unsafe.Pointer(&item)))
}

func (a *app) onSidebarNotify(hdr *nmhdr, l uintptr) uintptr {
	switch hdr.Code {
	case tvnSelChangedW:
		a.activateSidebarSelection()
		return 0
	case tvnGetDispInfoW:
		// We set text at insert time; nothing to do.
		return 0
	case nmDblClk:
		a.activateSidebarSelection()
		return 1
	case nmRClick:
		// Sidebar context menu: scope here / save search. Minimal: save.
		a.showSidebarMenu()
		return 1
	}
	return 0
}

func (a *app) sidebarSelected() *sidebarNode {
	sel := sendMessage(a.sidebar, tvmGetNextItem, tvgnCaret, 0)
	if sel == 0 {
		return nil
	}
	for i := range a.sidebarRoots {
		if a.sidebarRoots[i].handle == sel {
			return &a.sidebarRoots[i]
		}
	}
	return nil
}

func (a *app) activateSidebarSelection() {
	n := a.sidebarSelected()
	if n == nil {
		return
	}
	if n.query != "" {
		setWindowText(a.searchBox, n.query)
		a.query = n.query
		a.searcher.SearchNow(n.query, a.contents, a.under)
		return
	}
	if n.under != "" {
		setWindowText(a.scopeBox, n.under)
		a.under = n.under
		a.rerun()
	}
}

func (a *app) showSidebarMenu() {
	// Minimal native menu: Save current search.
	menu, _, _ := syscall.SyscallN(pCreatePopupMenu.Addr())
	if menu == 0 {
		return
	}
	defer syscall.SyscallN(pDestroyMenu.Addr(), menu)
	const cmdSave = 1
	_, _, _ = syscall.SyscallN(pAppendMenuW.Addr(), menu, mfString, cmdSave, uintptr(unsafe.Pointer(utf16("Save current search"))))
	x, y, ok := cursorScreenPos()
	if !ok {
		x, y = 100, 100
	}
	cmd, _, _ := syscall.SyscallN(pTrackPopupMenuEx.Addr(), menu,
		uintptr(tpmReturnCmd|tpmRightButton), uintptr(uint32(x)), uintptr(uint32(y)), a.hwnd, 0)
	if cmd == cmdSave {
		a.saveCurrentSearch()
	}
}

func (a *app) saveCurrentSearch() {
	q := getWindowText(a.searchBox)
	if q == "" {
		return
	}
	for _, s := range a.settings.Saved {
		if s == q {
			return
		}
	}
	a.settings.Saved = append(a.settings.Saved, q)
	a.refreshSidebar()
	a.saveSoon()
}
