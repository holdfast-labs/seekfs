//go:build windows

package main

import (
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// ctxmenu_windows.go: the Explorer shell context menu on a dedicated STA
// thread with a prebuilt-menu cache, ported from
// cmd/seekfs/ui_contextmenu_windows.go. QueryContextMenu is slow (~300ms)
// because it loads every registered shell-extension DLL, so menus are
// prebuilt on selection change/hover and cached; right-click on a cached
// selection shows instantly. All COM building, showing and cache access
// happen on this one thread.

// HWND_MESSAGE is (HWND)-3, the parent for message-only windows.
const hwndMessage = ^uintptr(2)

var menuOwnerClassName = "seekfs-native-menu-owner"

type menuOwnerClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  uintptr
	LpszClassName uintptr
	HIconSm       uintptr
}

var (
	pRegisterClassExWMenu = proc(modUser32, "RegisterClassExW")
	pCreateWindowExWMenu  = proc(modUser32, "CreateWindowExW")
	pDefWindowProcWMenu   = proc(modUser32, "DefWindowProcW")
)

var menuOwnerWndProc = syscall.NewCallback(func(hwnd, msg, w, l uintptr) uintptr {
	r, _, _ := syscall.SyscallN(pDefWindowProcWMenu.Addr(), hwnd, msg, w, l)
	return r
})

// createMenuOwnerWindow registers a window class and creates a message-only
// owner window for the shell menu's modal loop on the calling thread.
func createMenuOwnerWindow() uintptr {
	className, _ := syscall.UTF16PtrFromString(menuOwnerClassName)
	inst := instanceHandle()
	wc := menuOwnerClassEx{
		CbSize:        uint32(unsafe.Sizeof(menuOwnerClassEx{})),
		LpfnWndProc:   menuOwnerWndProc,
		HInstance:     inst,
		LpszClassName: uintptr(unsafe.Pointer(className)),
	}
	_, _, _ = syscall.SyscallN(pRegisterClassExWMenu.Addr(), uintptr(unsafe.Pointer(&wc)))
	hwnd, _, _ := syscall.SyscallN(pCreateWindowExWMenu.Addr(),
		0,
		uintptr(unsafe.Pointer(className)),
		0, 0x0,
		0, 0, 0, 0,
		hwndMessage, 0, inst, 0)
	return hwnd
}

type ctxMenuRequest struct {
	prebuild bool
	paths    []string
	screenX  int32
	screenY  int32
	owner    uintptr
	result   chan ctxMenuResult
}

type ctxMenuResult struct {
	shown bool
	err   error
}

type contextMenuService struct {
	req   chan ctxMenuRequest
	cache map[string]*builtShellMenu
}

var ctxMenuSvc *contextMenuService

// startContextMenuService launches the persistent menu thread once.
func startContextMenuService() {
	if ctxMenuSvc != nil {
		return
	}
	svc := &contextMenuService{req: make(chan ctxMenuRequest, 16), cache: make(map[string]*builtShellMenu)}
	ctxMenuSvc = svc
	go svc.run()
}

func (s *contextMenuService) run() {
	// COM STA requires thread affinity for the service lifetime.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := oleInitialize(); err != nil {
		if errno, ok := err.(syscall.Errno); !ok || errno != rpcEChangedMode {
			return
		}
	}
	defer oleUninitialize()

	owner := createMenuOwnerWindow()
	for req := range s.req {
		if req.prebuild {
			s.prebuild(req.paths)
			continue
		}
		shown, err := s.show(owner, req)
		if req.result != nil {
			req.result <- ctxMenuResult{shown: shown, err: err}
		}
	}
}

func pathsCacheKey(paths []string) string { return strings.Join(paths, "\x00") }

func (s *contextMenuService) prebuild(paths []string) {
	key := pathsCacheKey(paths)
	if old, ok := s.cache[key]; ok {
		destroyBuiltMenu(old)
		delete(s.cache, key)
	}
	built, err := buildShellContextMenu(paths)
	if err != nil || built == nil {
		return
	}
	// Bound the cache: keep only the newest few entries.
	if len(s.cache) >= 4 {
		for k, v := range s.cache {
			destroyBuiltMenu(v)
			delete(s.cache, k)
			break
		}
	}
	s.cache[key] = built
}

func (s *contextMenuService) show(owner uintptr, req ctxMenuRequest) (bool, error) {
	key := pathsCacheKey(req.paths)
	built, ok := s.cache[key]
	if ok {
		delete(s.cache, key)
	}
	if !ok {
		var err error
		built, err = buildShellContextMenu(req.paths)
		if err != nil {
			return false, err
		}
		if built == nil {
			return false, nil
		}
	}
	defer destroyBuiltMenu(built)

	if owner == 0 {
		owner = req.owner
	}
	return trackBuiltMenu(owner, built, req.screenX, req.screenY)
}

// trackBuiltMenu runs the modal TrackPopupMenuEx loop and invokes the command.
func trackBuiltMenu(owner uintptr, built *builtShellMenu, screenX, screenY int32) (bool, error) {
	if owner != 0 {
		_, _, _ = syscall.SyscallN(pSetForegroundWindow.Addr(), owner)
		_, _, _ = syscall.SyscallN(pPostMessageW.Addr(), owner, 0, 0, 0) // WM_NULL catch-up
	}
	cmd, _, _ := syscall.SyscallN(pTrackPopupMenuEx.Addr(),
		built.hMenu,
		uintptr(tpmReturnCmd|tpmRightButton),
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
		return true, comHResultErr(r)
	}
	return true, nil
}

// showShellContextMenuAsync shows the menu via the service thread. The caller
// must be on the UI thread; the result arrives on resCh. owner is the main
// window (used only when the service has no message-only owner).
func showShellContextMenuAsync(owner uintptr, paths []string, resCh chan ctxMenuResult) {
	var clean []string
	for _, p := range paths {
		if t := strings.TrimSpace(p); t != "" {
			clean = append(clean, t)
		}
	}
	if len(clean) == 0 {
		resCh <- ctxMenuResult{}
		return
	}
	startContextMenuService()
	x, y, ok := cursorScreenPos()
	if !ok {
		x, y = 100, 100
	}
	ctxMenuSvc.req <- ctxMenuRequest{paths: clean, screenX: x, screenY: y, owner: owner, result: resCh}
}

// prebuildShellContextMenu warms the cache for a selection. Non-blocking.
func prebuildShellContextMenu(paths []string) {
	var clean []string
	for _, p := range paths {
		if t := strings.TrimSpace(p); t != "" {
			clean = append(clean, t)
		}
	}
	if len(clean) == 0 {
		return
	}
	startContextMenuService()
	select {
	case ctxMenuSvc.req <- ctxMenuRequest{prebuild: true, paths: clean}:
	default:
	}
}

// warmUpShellContextMenus pre-loads shell-extension DLLs at startup.
func warmUpShellContextMenus(paths []string) {
	if len(paths) == 0 {
		return
	}
	prebuildShellContextMenu(paths)
}
