//go:build windows

package main

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// shell_windows.go: native Explorer shell integration.
//
// - showShellContextMenu: the real IContextMenu via SHBindToParent +
//   SHCreateDefaultContextMenu, ported from
//   cmd/seekfs/ui_contextmenu_windows.go (same-folder full menu, desktop
//   merge for cross-folder selections).
// - shellOpen / shellReveal / shellProperties via ShellExecuteW.
// - recyclePaths via SHFileOperationW (FO_DELETE + FOF_ALLOWUNDO).
// - copyPathsToClipboard as CF_HDROP.
// - fileDropData builds the HDROP blob for drag-and-drop out.

// COM / shell constants.
const (
	coinitApartmentThreaded = 0x2
	coinitDisableOLE1DDE    = 0x4
	rpcEChangedMode         = 0x80010106

	cmfNormal  = 0x0
	cmfExplore = 0x4

	tpmCtxReturnCmd = 0x0100
	tpmCtxRightBtn  = 0x0002

	cmicMaskUnicode  = 0x4000
	cmicMaskPTInvoke = 0x20000000

	contextMenuIDFirst = 1
	contextMenuIDLast  = 0x7FFF

	iContextMenuQueryContextMenuSlot = 3
	iContextMenuInvokeCommandSlot    = 4
)

var (
	// IID_IContextMenu {000214e4-0000-0000-c000-000000000046}
	iidContextMenu = syscall.GUID{
		Data1: 0x000214e4,
		Data3: 0x0000, Data2: 0x0000,
		Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
	}
	// IID_IShellFolder {000214e6-0000-0000-c000-000000000046}
	iidShellFolder = syscall.GUID{
		Data1: 0x000214e6,
		Data3: 0x0000, Data2: 0x0000,
		Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
	}
)

type cminvokeCommandInfoEx struct {
	CbSize        uint32
	FMask         uint32
	Hwnd          uintptr
	LpVerb        uintptr
	LpParameters  uintptr
	LpDirectory   uintptr
	NShow         int32
	DwHotKey      uint32
	HIcon         uintptr
	LpTitle       uintptr
	LpVerbW       uintptr
	LpParametersW uintptr
	LpDirectoryW  uintptr
	LpTitleW      uintptr
	PtInvoke      point
	DwNoWait      uint32
}

func makeIntResourceW(id uint16) uintptr { return uintptr(id) }

// parentKeyOfPidl returns a string key for the parent folder of an absolute
// ITEMIDLIST (the prefix before the last element).
func parentKeyOfPidl(pidl unsafe.Pointer) string {
	p := pidl
	lastElem := pidl
	for {
		cb := *(*uint16)(p)
		if cb == 0 {
			break
		}
		lastElem = p
		p = unsafe.Add(p, uintptr(cb))
	}
	parent := unsafe.Slice((*byte)(pidl), int(uintptr(lastElem)-uintptr(pidl)))
	return string(parent)
}

type builtShellMenu struct {
	ctxMenu unsafe.Pointer
	hMenu   uintptr
}

func destroyBuiltMenu(b *builtShellMenu) {
	if b == nil {
		return
	}
	if b.hMenu != 0 {
		_, _, _ = syscall.SyscallN(pDestroyMenu.Addr(), b.hMenu)
	}
	if b.ctxMenu != nil {
		releaseCom(b.ctxMenu)
	}
}

// buildShellContextMenu parses paths and builds the Explorer context menu.
// Same-folder selections get the full SHCreateDefaultContextMenu menu;
// cross-folder selections use the desktop-folder merge.
func buildShellContextMenu(clean []string) (*builtShellMenu, error) {
	pidls := make([]unsafe.Pointer, 0, len(clean))
	defer func() {
		for _, pidl := range pidls {
			_, _, _ = syscall.SyscallN(pCoTaskMemFree.Addr(), uintptr(pidl))
		}
	}()
	for _, path := range clean {
		ptr, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
		}
		var pidl unsafe.Pointer
		r, _, _ := syscall.SyscallN(pSHParseDisplayName.Addr(),
			uintptr(unsafe.Pointer(ptr)), 0, uintptr(unsafe.Pointer(&pidl)), 0, 0)
		if r != 0 {
			return nil, fmt.Errorf("SHParseDisplayName(%q): %w", path, comHResultErr(r))
		}
		if pidl == nil {
			return nil, fmt.Errorf("SHParseDisplayName(%q) returned a nil pidl", path)
		}
		pidls = append(pidls, pidl)
	}

	type parentGroup struct {
		parent   unsafe.Pointer
		children []unsafe.Pointer
	}
	groupsByKey := make(map[string]*parentGroup)
	var groups []*parentGroup
	var keptParents []unsafe.Pointer
	for _, pidl := range pidls {
		var parent, child unsafe.Pointer
		r, _, _ := syscall.SyscallN(pSHBindToParent.Addr(),
			uintptr(pidl), uintptr(unsafe.Pointer(&iidShellFolder)),
			uintptr(unsafe.Pointer(&parent)), uintptr(unsafe.Pointer(&child)))
		if r != 0 {
			return nil, fmt.Errorf("SHBindToParent: %w", comHResultErr(r))
		}
		key := parentKeyOfPidl(pidl)
		g := groupsByKey[key]
		if g == nil {
			g = &parentGroup{parent: parent}
			groupsByKey[key] = g
			groups = append(groups, g)
			keptParents = append(keptParents, parent)
		} else {
			releaseCom(parent)
		}
		g.children = append(g.children, child)
	}
	defer func() {
		for _, p := range keptParents {
			releaseCom(p)
		}
	}()

	type defContextMenu struct {
		hwnd      uintptr
		pcmcb     uintptr
		pidlFolder uintptr
		psf       unsafe.Pointer
		cidl      uint32
		apidl     *unsafe.Pointer
		psfFilter unsafe.Pointer
		pidlItem  unsafe.Pointer
	}
	var dcm defContextMenu
	if len(groups) == 1 && len(groups[0].children) > 0 {
		g := groups[0]
		dcm = defContextMenu{psf: g.parent, cidl: uint32(len(g.children)), apidl: &g.children[0]}
	} else if len(pidls) > 0 {
		var desktop unsafe.Pointer
		r, _, _ := syscall.SyscallN(pSHGetDesktopFolder.Addr(), uintptr(unsafe.Pointer(&desktop)))
		if r != 0 {
			return nil, fmt.Errorf("SHGetDesktopFolder: %w", comHResultErr(r))
		}
		defer releaseCom(desktop)
		dcm = defContextMenu{psf: desktop, cidl: uint32(len(pidls)), apidl: &pidls[0]}
	}
	if dcm.psf == nil || dcm.cidl == 0 {
		return nil, fmt.Errorf("no items to build a context menu for")
	}
	// CoTaskMemFree is in ole32, not exposed via our proc list; use syscall DLL.
	var ctxMenu unsafe.Pointer
	r, _, _ := syscall.SyscallN(pSHCreateDefaultContextMenu.Addr(),
		uintptr(unsafe.Pointer(&dcm)),
		uintptr(unsafe.Pointer(&iidContextMenu)),
		uintptr(unsafe.Pointer(&ctxMenu)))
	if r != 0 {
		return nil, fmt.Errorf("SHCreateDefaultContextMenu: %w", comHResultErr(r))
	}
	if ctxMenu == nil {
		return nil, fmt.Errorf("SHCreateDefaultContextMenu returned a nil IContextMenu")
	}

	hMenu, _, _ := syscall.SyscallN(pCreatePopupMenu.Addr())
	if hMenu == 0 {
		releaseCom(ctxMenu)
		return nil, fmt.Errorf("CreatePopupMenu failed")
	}
	r = callVtbl(ctxMenu, iContextMenuQueryContextMenuSlot,
		hMenu, 0, contextMenuIDFirst, contextMenuIDLast, uintptr(cmfNormal|cmfExplore))
	if r == 0xFFFFFFFF {
		_, _, _ = syscall.SyscallN(pDestroyMenu.Addr(), hMenu)
		releaseCom(ctxMenu)
		return nil, fmt.Errorf("QueryContextMenu failed")
	}
	if r == 0 {
		_, _, _ = syscall.SyscallN(pDestroyMenu.Addr(), hMenu)
		releaseCom(ctxMenu)
		return nil, nil
	}
	return &builtShellMenu{ctxMenu: ctxMenu, hMenu: hMenu}, nil
}

// showShellContextMenu shows the native Explorer context menu for paths at
// the screen position. owner must be a window on the calling (STA) thread.
// Returns shown=true once displayed (even if dismissed).
func showShellContextMenu(owner uintptr, paths []string, screenX, screenY int32) (bool, error) {
	var clean []string
	for _, p := range paths {
		if t := strings.TrimSpace(p); t != "" {
			clean = append(clean, t)
		}
	}
	if len(clean) == 0 {
		return false, nil
	}
	built, err := buildShellContextMenu(clean)
	if err != nil {
		return false, err
	}
	if built == nil {
		return false, nil
	}
	defer destroyBuiltMenu(built)

	if owner != 0 {
		_, _, _ = syscall.SyscallN(pSetForegroundWindow.Addr(), owner)
		_, _, _ = syscall.SyscallN(pPostMessageW.Addr(), owner, 0, 0, 0) // WM_NULL catch-up
	}
	cmd, _, _ := syscall.SyscallN(pTrackPopupMenuEx.Addr(),
		built.hMenu,
		uintptr(tpmCtxReturnCmd|tpmCtxRightBtn),
		uintptr(uint32(screenX)), uintptr(uint32(screenY)),
		owner, 0)
	if cmd == 0 {
		return true, nil // dismissed
	}
	verb := uint16(cmd - contextMenuIDFirst)
	var info cminvokeCommandInfoEx
	info.CbSize = uint32(unsafe.Sizeof(info))
	info.FMask = cmicMaskUnicode | cmicMaskPTInvoke
	info.Hwnd = owner
	info.LpVerb = makeIntResourceW(verb)
	info.LpVerbW = makeIntResourceW(verb)
	info.PtInvoke.X = screenX
	info.PtInvoke.Y = screenY
	info.NShow = 1
	r := callVtbl(built.ctxMenu, iContextMenuInvokeCommandSlot, uintptr(unsafe.Pointer(&info)))
	if r != 0 {
		return true, fmt.Errorf("InvokeCommand: %w", comHResultErr(r))
	}
	return true, nil
}

// pCoTaskMemFree: ole32 CoTaskMemFree.
var pCoTaskMemFree = proc(modOle32, "CoTaskMemFree")

// ---- verbs ----

func shellOpen(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path required")
	}
	r, _, _ := syscall.SyscallN(pShellExecuteW.Addr(), 0,
		0, uintptr(unsafe.Pointer(utf16(path))), 0, 0, uintptr(1))
	if r <= 32 {
		return fmt.Errorf("ShellExecuteW failed with code %d", r)
	}
	return nil
}

func shellReveal(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path required")
	}
	r, _, _ := syscall.SyscallN(pShellExecuteW.Addr(), 0,
		uintptr(unsafe.Pointer(utf16("open"))),
		uintptr(unsafe.Pointer(utf16("explorer.exe"))),
		uintptr(unsafe.Pointer(utf16(`/select,"`+path+`"`))),
		0, uintptr(1))
	if r <= 32 {
		return fmt.Errorf("ShellExecuteW failed with code %d", r)
	}
	return nil
}

func shellProperties(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path required")
	}
	r, _, _ := syscall.SyscallN(pShellExecuteW.Addr(), 0,
		uintptr(unsafe.Pointer(utf16("properties"))),
		uintptr(unsafe.Pointer(utf16(path))), 0, 0, uintptr(1))
	if r <= 32 {
		return fmt.Errorf("ShellExecuteW failed with code %d", r)
	}
	return nil
}

// shFileOp mirrors SHFILEOPSTRUCTW for SHFileOperationW.
type shFileOp struct {
	Hwnd                  uintptr
	WFunc                 uint32
	PFrom                 *uint16
	PTo                   *uint16
	FFlags                uint16
	FAnyOperationsAborted uint16
	HNameMappings         uintptr
	LpszProgressTitle     *uint16
}

const (
	foDelete       = 0x3
	fofAllowUndo   = 0x40
	fofNoConfirm   = 0x10
	fofSilent      = 0x04
	fofNoErrorUI   = 0x400
	fofWantNukeWarn = 0x4000
)

// recyclePaths moves paths to the Recycle Bin (undoable, with Explorer UI).
func recyclePaths(hwnd uintptr, paths []string) error {
	var clean []string
	for _, p := range paths {
		if t := strings.TrimSpace(p); t != "" {
			clean = append(clean, t)
		}
	}
	if len(clean) == 0 {
		return fmt.Errorf("no paths selected")
	}
	// Double-NUL-terminated list.
	var buf []uint16
	for _, p := range clean {
		u, _ := syscall.UTF16FromString(p)
		buf = append(buf, u...)
	}
	buf = append(buf, 0)
	op := shFileOp{
		Hwnd:   hwnd,
		WFunc:  foDelete,
		PFrom:  &buf[0],
		FFlags: fofAllowUndo,
	}
	r, _, _ := syscall.SyscallN(pSHFileOperationW.Addr(), uintptr(unsafe.Pointer(&op)))
	if r != 0 {
		return fmt.Errorf("SHFileOperationW failed with code %d", r)
	}
	if op.FAnyOperationsAborted != 0 {
		return fmt.Errorf("delete was cancelled")
	}
	return nil
}

// dropFiles mirrors DROPFILES for CF_HDROP.
type dropFiles struct {
	Size    uint32
	Pt      point
	NC      int32
	Wide    int32
}

// fileDropData builds a CF_HDROP global-memory blob for paths.
func fileDropData(paths []string) []byte {
	var units []uint16
	for _, p := range paths {
		u, _ := syscall.UTF16FromString(p)
		units = append(units, u...)
	}
	units = append(units, 0)
	headerSize := int(unsafe.Sizeof(dropFiles{}))
	buf := make([]byte, headerSize+len(units)*2)
	h := (*dropFiles)(unsafe.Pointer(&buf[0]))
	h.Size = uint32(headerSize)
	h.Wide = 1
	copy(buf[headerSize:], unsafe.Slice((*byte)(unsafe.Pointer(&units[0])), len(units)*2))
	return buf
}

// globalBytes maps a GlobalLock pointer for the copy duration. The memory
// is owned by the clipboard after SetClipboardData.
func globalBytes(ptr uintptr, n int) []byte {
	return unsafe.Slice((*byte)(notifyPtr(ptr)), n)
}

// copyPathsToClipboard places CF_HDROP file paths on the clipboard.
func copyPathsToClipboard(paths []string) error {
	data := fileDropData(paths)
	hMem, _, _ := syscall.SyscallN(pGlobalAlloc.Addr(), gmemMoveable, uintptr(len(data)))
	if hMem == 0 {
		return fmt.Errorf("GlobalAlloc failed")
	}
	ptr, _, _ := syscall.SyscallN(pGlobalLock.Addr(), hMem)
	if ptr == 0 {
		return fmt.Errorf("GlobalLock failed")
	}
	copy(globalBytes(ptr, len(data)), data)
	_, _, _ = syscall.SyscallN(pGlobalUnlock.Addr(), hMem)
	r, _, _ := syscall.SyscallN(pOpenClipboard.Addr(), 0)
	if r == 0 {
		return fmt.Errorf("OpenClipboard failed")
	}
	defer syscall.SyscallN(pCloseClipboard.Addr())
	_, _, _ = syscall.SyscallN(pEmptyClipboard.Addr())
	r, _, _ = syscall.SyscallN(pSetClipboardData.Addr(), cfHdrop, hMem)
	if r == 0 {
		return fmt.Errorf("SetClipboardData failed")
	}
	return nil // clipboard owns hMem now
}

// cursorScreenPos returns the cursor position in screen coordinates.
func cursorScreenPos() (int32, int32, bool) {
	var pt point
	r, _, _ := syscall.SyscallN(pGetCursorPos.Addr(), uintptr(unsafe.Pointer(&pt)))
	return pt.X, pt.Y, r != 0
}
