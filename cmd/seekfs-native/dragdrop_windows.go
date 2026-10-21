//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// dragdrop_windows.go: drag-and-drop OUT to Explorer and other apps.
// Uses SHCreateDataObject (pidl + file) so the drop target gets a full
// CF_HDROP/CFSTR_SHELLIDLIST data object, then DoDragDrop on the UI thread.
// IDataObject/IDropSource vtables for SHCreateDataObject's FORMATETC array.

type formatEtc struct {
	CfFormat uint16
	Ptd      uintptr
	DwAspect uint32
	Lindex   int32
	Tymed    uint32
}

var (
	pSHCreateDataObjectDrag = proc(modShell32, "SHCreateDataObject")
	pDoDragDropDrag         = proc(modOle32, "DoDragDrop")
)

// IID_IUnknown {00000000-0000-0000-C000-000000000046}
var iidIUnknown = syscall.GUID{
	Data1: 0x00000000, Data2: 0x0000, Data3: 0x0000,
	Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
}

// IID_IDataObject {0000010e-0000-0000-C000-000000000046}
var iidIDataObject = syscall.GUID{
	Data1: 0x0000010e, Data2: 0x0000, Data3: 0x0000,
	Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
}

// dropSource implements IDropSource with two methods after IUnknown:
// QueryContinueDrag(3), GiveFeedback(4). The COM object is a small
// heap-allocated struct with a vtable of callbacks.
type dropSource struct {
	vtbl *dropSourceVtbl
	refs int32
}

type dropSourceVtbl struct {
	queryInterface    uintptr
	addRef            uintptr
	release           uintptr
	queryContinueDrag uintptr
	giveFeedback      uintptr
}

const (
	drgContinue = 0
	drgDrop     = 1
	drgCancel   = 2

	mkLButton = 0x0001

	dropeffectNone = 0
	dropeffectCopy = 1
	dropeffectMove = 2
	dropeffectLink = 4
)

var globalDropSourceVtbl = &dropSourceVtbl{
	queryInterface:    syscall.NewCallback(dropSourceQueryInterface),
	addRef:            syscall.NewCallback(dropSourceAddRef),
	release:           syscall.NewCallback(dropSourceRelease),
	queryContinueDrag: syscall.NewCallback(dropSourceQueryContinueDrag),
	giveFeedback:      syscall.NewCallback(dropSourceGiveFeedback),
}

func dropSourceQueryInterface(this, riid, ppv uintptr) uintptr {
	want := (*syscall.GUID)(notifyPtr(riid))
	if *want == iidIDataObject || *want == iidIUnknown {
		*(*uintptr)(notifyPtr(ppv)) = this
		return 0 // S_OK
	}
	*(*uintptr)(notifyPtr(ppv)) = 0
	return 0x80004002 // E_NOINTERFACE
}

func dropSourceAddRef(this uintptr) uintptr {
	ds := (*dropSource)(notifyPtr(this))
	ds.refs++
	return uintptr(ds.refs)
}

func dropSourceRelease(this uintptr) uintptr {
	ds := (*dropSource)(notifyPtr(this))
	ds.refs--
	return uintptr(ds.refs)
}

func dropSourceQueryContinueDrag(this uintptr, escapePressed int32, keyState uint32) uintptr {
	if escapePressed != 0 {
		return drgCancel
	}
	if keyState&mkLButton == 0 {
		return drgDrop
	}
	return drgContinue
}

func dropSourceGiveFeedback(this uintptr, effect uint32) uintptr {
	return 0x00040102 // DRAGDROP_S_USEDEFAULTCURSORS
}

// pidlsForPaths parses paths to absolute PIDLs; caller frees with CoTaskMemFree.
func pidlsForPaths(paths []string) ([]unsafe.Pointer, error) {
	pidls := make([]unsafe.Pointer, 0, len(paths))
	for _, path := range paths {
		ptr, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			freePidls(pidls)
			return nil, err
		}
		var pidl unsafe.Pointer
		r, _, _ := syscall.SyscallN(pSHParseDisplayName.Addr(),
			uintptr(unsafe.Pointer(ptr)), 0, uintptr(unsafe.Pointer(&pidl)), 0, 0)
		if r != 0 || pidl == nil {
			freePidls(pidls)
			return nil, comHResultErr(r)
		}
		pidls = append(pidls, pidl)
	}
	return pidls, nil
}

func freePidls(pidls []unsafe.Pointer) {
	for _, p := range pidls {
		_, _, _ = syscall.SyscallN(pCoTaskMemFree.Addr(), uintptr(p))
	}
}

// parentAndChild splits an absolute pidl via SHBindToParent.
func parentAndChild(pidl unsafe.Pointer) (parent, child unsafe.Pointer, err error) {
	r, _, _ := syscall.SyscallN(pSHBindToParent.Addr(),
		uintptr(pidl), uintptr(unsafe.Pointer(&iidShellFolder)),
		uintptr(unsafe.Pointer(&parent)), uintptr(unsafe.Pointer(&child)))
	if r != 0 {
		return nil, nil, comHResultErr(r)
	}
	return parent, child, nil
}

// doDragDropFiles starts a drag of paths with copy+move+link effects.
// DoDragDrop runs its own modal message loop; the caller must have COM
// initialized on its thread (the UI thread has OLE via runUI) and must call
// from the UI thread. Callers invoke it from the drag-start notification,
// which is already on the UI thread, so this runs synchronously.
func doDragDropFiles(hwnd uintptr, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	// Single parent fast path: SHCreateDataObject(pidlFolder, cidl, apidl).
	pidls, err := pidlsForPaths(paths)
	if err != nil {
		return err
	}
	defer freePidls(pidls)

	// Group by parent; use the first group if all share one, else desktop merge.
	type group struct {
		parent   unsafe.Pointer
		children []unsafe.Pointer
	}
	var groups []*group
	byKey := map[string]*group{}
	var parents []unsafe.Pointer
	for _, pidl := range pidls {
		parent, child, err := parentAndChild(pidl)
		if err != nil {
			for _, p := range parents {
				releaseCom(p)
			}
			return err
		}
		key := parentKeyOfPidl(pidl)
		g := byKey[key]
		if g == nil {
			g = &group{parent: parent}
			byKey[key] = g
			groups = append(groups, g)
			parents = append(parents, parent)
		} else {
			releaseCom(parent)
		}
		g.children = append(g.children, child)
	}
	defer func() {
		for _, p := range parents {
			releaseCom(p)
		}
	}()

	var folder unsafe.Pointer
	var cidl uint32
	var apidl *unsafe.Pointer
	var desktop unsafe.Pointer
	if len(groups) == 1 {
		folder = groups[0].parent
		cidl = uint32(len(groups[0].children))
		apidl = &groups[0].children[0]
	} else {
		r, _, _ := syscall.SyscallN(pSHGetDesktopFolder.Addr(), uintptr(unsafe.Pointer(&desktop)))
		if r != 0 {
			return comHResultErr(r)
		}
		defer releaseCom(desktop)
		folder = desktop
		cidl = uint32(len(pidls))
		apidl = &pidls[0]
	}

	var dataObj unsafe.Pointer
	hr, _, _ := syscall.SyscallN(pSHCreateDataObjectDrag.Addr(),
		uintptr(folder), uintptr(cidl), uintptr(unsafe.Pointer(apidl)),
		0, uintptr(unsafe.Pointer(&iidIDataObject)), uintptr(unsafe.Pointer(&dataObj)))
	if hr != 0 || dataObj == nil {
		return comHResultErr(hr)
	}
	defer releaseCom(dataObj)

	src := &dropSource{vtbl: globalDropSourceVtbl, refs: 1}
	var effect uint32
	hr, _, _ = syscall.SyscallN(pDoDragDropDrag.Addr(),
		uintptr(dataObj), uintptr(unsafe.Pointer(src)),
		uintptr(dropeffectCopy|dropeffectMove|dropeffectLink), uintptr(unsafe.Pointer(&effect)))
	_ = hr // DROPEFFECT / DRAGDROP_S_DROP / cancel codes are all fine
	return nil
}
