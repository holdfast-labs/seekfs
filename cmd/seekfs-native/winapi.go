//go:build windows

package main

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// winapi.go: raw Win32/COM entry points, message/struct layouts and small
// wrappers. Everything goes through SyscallN; HWND and friends are uintptr.

// ---- basic types ----

type point struct{ X, Y int32 }
type rect struct{ Left, Top, Right, Bottom int32 }

func (r rect) width() int32  { return r.Right - r.Left }
func (r rect) height() int32 { return r.Bottom - r.Top }

type wndClassEx struct {
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

type nmhdr struct {
	HwndFrom uintptr
	IdFrom   uintptr
	Code     int32
	_        [4]byte
}

type accel struct {
	FVirt uint16
	Key   uint16
	Cmd   uint16
}

type initCommonControlsEx struct {
	DwSize uint32
	DwICC  uint32
}

// ---- dlls ----

var (
	modUser32   = syscall.NewLazyDLL("user32.dll")
	modGdi32    = syscall.NewLazyDLL("gdi32.dll")
	modComctl32 = syscall.NewLazyDLL("comctl32.dll")
	modShell32  = syscall.NewLazyDLL("shell32.dll")
	modOle32    = syscall.NewLazyDLL("ole32.dll")
	modUxTheme  = syscall.NewLazyDLL("uxtheme.dll")
	modDwmapi   = syscall.NewLazyDLL("dwmapi.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")
)

func proc(dll *syscall.LazyDLL, name string) *syscall.LazyProc { return dll.NewProc(name) }

var (
	pRegisterClassExW        = proc(modUser32, "RegisterClassExW")
	pCreateWindowExW         = proc(modUser32, "CreateWindowExW")
	pDefWindowProcW          = proc(modUser32, "DefWindowProcW")
	pDestroyWindow           = proc(modUser32, "DestroyWindow")
	pShowWindow              = proc(modUser32, "ShowWindow")
	pUpdateWindow            = proc(modUser32, "UpdateWindow")
	pGetMessageW             = proc(modUser32, "GetMessageW")
	pTranslateMessage        = proc(modUser32, "TranslateMessage")
	pDispatchMessageW        = proc(modUser32, "DispatchMessageW")
	pPostMessageW            = proc(modUser32, "PostMessageW")
	pSendMessageW            = proc(modUser32, "SendMessageW")
	pSetWindowTextW          = proc(modUser32, "SetWindowTextW")
	pGetWindowTextW          = proc(modUser32, "GetWindowTextW")
	pGetWindowTextLengthW    = proc(modUser32, "GetWindowTextLengthW")
	pSetWindowLongPtrW       = proc(modUser32, "SetWindowLongPtrW")
	pGetWindowLongPtrW       = proc(modUser32, "GetWindowLongPtrW")
	pSetWindowPos            = proc(modUser32, "SetWindowPos")
	pMoveWindow              = proc(modUser32, "MoveWindow")
	pGetClientRect           = proc(modUser32, "GetClientRect")
	pGetWindowRect           = proc(modUser32, "GetWindowRect")
	pScreenToClient          = proc(modUser32, "ScreenToClient")
	pInvalidateRect          = proc(modUser32, "InvalidateRect")
	pSetFocus                = proc(modUser32, "SetFocus")
	pGetFocus                = proc(modUser32, "GetFocus")
	pGetCursorPos            = proc(modUser32, "GetCursorPos")
	pLoadCursorW             = proc(modUser32, "LoadCursorW")
	pGetDC                   = proc(modUser32, "GetDC")
	pReleaseDC               = proc(modUser32, "ReleaseDC")
	pGetSystemMetrics        = proc(modUser32, "GetSystemMetrics")
	pSystemParametersInfoW   = proc(modUser32, "SystemParametersInfoW")
	pGetDpiForWindow         = proc(modUser32, "GetDpiForWindow")
	pPostQuitMessage         = proc(modUser32, "PostQuitMessage")
	pCreateAcceleratorTableW = proc(modUser32, "CreateAcceleratorTableW")
	pTranslateAcceleratorW   = proc(modUser32, "TranslateAcceleratorW")
	pDestroyAcceleratorTable = proc(modUser32, "DestroyAcceleratorTable")
	pFillRect                = proc(modUser32, "FillRect")
	pSetCapture              = proc(modUser32, "SetCapture")
	pReleaseCapture          = proc(modUser32, "ReleaseCapture")
	pGetForegroundWindow     = proc(modUser32, "GetForegroundWindow")
	pSetForegroundWindow     = proc(modUser32, "SetForegroundWindow")
	pOpenClipboard           = proc(modUser32, "OpenClipboard")
	pEmptyClipboard          = proc(modUser32, "EmptyClipboard")
	pSetClipboardData        = proc(modUser32, "SetClipboardData")
	pCloseClipboard          = proc(modUser32, "CloseClipboard")
	pCreatePopupMenu         = proc(modUser32, "CreatePopupMenu")
	pTrackPopupMenuEx        = proc(modUser32, "TrackPopupMenuEx")
	pDrawTextW               = proc(modUser32, "DrawTextW")
	pAppendMenuW             = proc(modUser32, "AppendMenuW")
	pDestroyMenu             = proc(modUser32, "DestroyMenu")
	pGetKeyState             = proc(modUser32, "GetKeyState")
	pMonitorFromWindow       = proc(modUser32, "MonitorFromWindow")
	pGetMonitorInfoW         = proc(modUser32, "GetMonitorInfoW")
	pIsWindow                = proc(modUser32, "IsWindow")
	pEnableWindow            = proc(modUser32, "EnableWindow")
	pGetSysColor             = proc(modUser32, "GetSysColor")
	pGetClassNameW           = proc(modUser32, "GetClassNameW")

	pCreateFontIndirectW = proc(modGdi32, "CreateFontIndirectW")
	pSelectObject        = proc(modGdi32, "SelectObject")
	pDeleteObject        = proc(modGdi32, "DeleteObject")
	pCreateSolidBrush    = proc(modGdi32, "CreateSolidBrush")
	pSetBkColor          = proc(modGdi32, "SetBkColor")
	pSetTextColor        = proc(modGdi32, "SetTextColor")
	pSetBkMode           = proc(modGdi32, "SetBkMode")
	pGetTextMetricsW     = proc(modGdi32, "GetTextMetricsW")
	pCreateCompatibleDC  = proc(modGdi32, "CreateCompatibleDC")
	pDeleteDC            = proc(modGdi32, "DeleteDC")
	pGetDeviceCaps       = proc(modGdi32, "GetDeviceCaps")
	pCreateDIBSection    = proc(modGdi32, "CreateDIBSection")
	pDrawIconEx          = proc(modUser32, "DrawIconEx")
	pDestroyIcon         = proc(modUser32, "DestroyIcon")

	pInitCommonControlsEx = proc(modComctl32, "InitCommonControlsEx")
	pImageListDraw        = proc(modComctl32, "ImageList_Draw")
	pImageListCreate      = proc(modComctl32, "ImageList_Create")
	pImageListDestroy     = proc(modComctl32, "ImageList_Destroy")

	pSHGetFileInfoW             = proc(modShell32, "SHGetFileInfoW")
	pSHParseDisplayName         = proc(modShell32, "SHParseDisplayName")
	pSHBindToParent             = proc(modShell32, "SHBindToParent")
	pSHGetDesktopFolder         = proc(modShell32, "SHGetDesktopFolder")
	pSHCreateDefaultContextMenu = proc(modShell32, "SHCreateDefaultContextMenu")
	pSHCreateDataObject         = proc(modShell32, "SHCreateDataObject")
	pSHGetImageList             = proc(modShell32, "SHGetImageList")
	pSHFileOperationW           = proc(modShell32, "SHFileOperationW")
	pShellExecuteW              = proc(modShell32, "ShellExecuteW")

	pOleInitialize   = proc(modOle32, "OleInitialize")
	pOleUninitialize = proc(modOle32, "OleUninitialize")
	pDoDragDrop      = proc(modOle32, "DoDragDrop")

	pSetWindowTheme = proc(modUxTheme, "SetWindowTheme")

	pDwmSetWindowAttribute = proc(modDwmapi, "DwmSetWindowAttribute")

	pGlobalAlloc      = proc(modKernel32, "GlobalAlloc")
	pGlobalLock       = proc(modKernel32, "GlobalLock")
	pGlobalUnlock     = proc(modKernel32, "GlobalUnlock")
	pGetModuleHandleW = proc(modKernel32, "GetModuleHandleW")
)

// ---- small wrappers ----

func utf16(s string) *uint16 {
	ptr, _ := syscall.UTF16PtrFromString(s)
	return ptr
}

func sendMessage(hwnd uintptr, msg uint32, w, l uintptr) uintptr {
	r, _, _ := syscall.SyscallN(pSendMessageW.Addr(), hwnd, uintptr(msg), w, l)
	return r
}

func postMessage(hwnd uintptr, msg uint32, w, l uintptr) {
	_, _, _ = syscall.SyscallN(pPostMessageW.Addr(), hwnd, uintptr(msg), w, l)
}

func getWindowLong(hwnd uintptr, index int32) uintptr {
	r, _, _ := syscall.SyscallN(pGetWindowLongPtrW.Addr(), hwnd, uintptr(int32(index)), 0)
	return r
}

func setWindowLong(hwnd uintptr, index int32, val uintptr) uintptr {
	r, _, _ := syscall.SyscallN(pSetWindowLongPtrW.Addr(), hwnd, uintptr(int32(index)), val)
	return r
}

func getClientRect(hwnd uintptr) rect {
	var r rect
	_, _, _ = syscall.SyscallN(pGetClientRect.Addr(), hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

func getWindowRect(hwnd uintptr) rect {
	var r rect
	_, _, _ = syscall.SyscallN(pGetWindowRect.Addr(), hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

func moveWindow(hwnd uintptr, x, y, w, h int32, repaint bool) {
	rp := uintptr(0)
	if repaint {
		rp = 1
	}
	_, _, _ = syscall.SyscallN(pMoveWindow.Addr(), hwnd, uintptr(x), uintptr(y), uintptr(w), uintptr(h), rp)
}

func invalidate(hwnd uintptr) {
	_, _, _ = syscall.SyscallN(pInvalidateRect.Addr(), hwnd, 0, 1)
}

func getSysColor(idx uintptr) uintptr {
	r, _, _ := syscall.SyscallN(pGetSysColor.Addr(), idx)
	// COLORREF 0x00BBGGRR.
	return r
}

// focusedControlClass returns the class name of the focused window.
func focusedControlClass() string {
	focus, _, _ := syscall.SyscallN(pGetFocus.Addr())
	if focus == 0 {
		return ""
	}
	buf := make([]uint16, 64)
	n, _, _ := syscall.SyscallN(pGetClassNameW.Addr(), focus, uintptr(unsafe.Pointer(&buf[0])), 64)
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func isEditFocused() bool {
	return focusedControlClass() == "Edit"
}

func setWindowText(hwnd uintptr, s string) {
	_, _, _ = syscall.SyscallN(pSetWindowTextW.Addr(), hwnd, uintptr(unsafe.Pointer(utf16(s))))
}

func getWindowText(hwnd uintptr) string {
	n, _, _ := syscall.SyscallN(pGetWindowTextLengthW.Addr(), hwnd)
	buf := make([]uint16, n+1)
	_, _, _ = syscall.SyscallN(pGetWindowTextW.Addr(), hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(n+1))
	return syscall.UTF16ToString(buf)
}

func instanceHandle() uintptr {
	r, _, _ := syscall.SyscallN(pGetModuleHandleW.Addr(), 0)
	return r
}

// COM helpers (shared with shell integration files).
func callVtbl(iface unsafe.Pointer, slot uint32, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(iface)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(iface))
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

func releaseCom(iface unsafe.Pointer) {
	if iface == nil {
		return
	}
	vtbl := *(*unsafe.Pointer)(iface)
	release := *(*uintptr)(unsafe.Add(vtbl, 2*unsafe.Sizeof(uintptr(0))))
	syscall.SyscallN(release, uintptr(iface))
}

func comHResultErr(hr uintptr) error { return syscall.Errno(uint32(hr)) }

// ---- constants ----

const (
	wsOverlappedWindow = 0x00CF0000
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsVScroll          = 0x00200000
	wsBorder           = 0x00800000
	wsClipChildren     = 0x02000000
	wsClipSiblings     = 0x04000000
	wsTabStop          = 0x00010000
	wsExClientEdge     = 0x00000200
	wsExComposited     = 0x02000000

	swShow        = 5
	swShowDefault = 10
	swShowMax     = 3

	wmCreate          = 0x0001
	wmDestroy         = 0x0002
	wmSize            = 0x0005
	wmSetFocus        = 0x0007
	wmKillFocus       = 0x0008
	wmClose           = 0x0010
	wmCommand         = 0x0111
	wmNotify          = 0x004E
	wmKeyDown         = 0x0100
	wmChar            = 0x0102
	wmCtlColorEdit    = 0x0133
	wmCtlColorStatic  = 0x0138
	wmCtlColorListBox = 0x0134
	wmHScroll         = 0x0114
	wmVScroll         = 0x0115
	wmLButtonDown     = 0x0201
	wmLButtonUp       = 0x0202
	wmMouseMove       = 0x0200
	wmRButtonDown     = 0x0204
	wmRButtonUp       = 0x0205
	wmDpiChanged      = 0x02E0
	wmSettingChange   = 0x001A
	wmThemeChanged    = 0x031A

	sizeRestored  = 0
	sizeMinimized = 1
	sizeMaximized = 2

	gwlStyle     = -16
	gwlExStyle   = -20
	gwlWndProc   = -4
	gwlpUserData = -21

	mfString       = 0x0000
	mfSeparator    = 0x0800
	mfsChecked     = 0x0008
	tpmReturnCmd   = 0x0100
	tpmRightButton = 0x0002

	dtLeft        = 0x0000
	dtSingleLine  = 0x0020
	dtVCenter     = 0x0004
	dtNoPrefix    = 0x0800
	dtEndEllipsis = 0x00008000
	dtCalcRect    = 0x00004000
	dtNoClip      = 0x00000100
	dtCenter      = 0x00000001

	transparent = 1
	opaque      = 2

	// GetSysColor indices.
	sysColorWindow        = 5
	sysColorWindowText    = 8
	sysColorHighlight     = 13
	sysColorHighlightText = 14
	sysColorBtnFace       = 15
	sysColorGrayText      = 17
	sysColorHotLight      = 26

	idcArrow = 32512

	cfHdrop       = 15
	cfUnicodetext = 13

	gmemMoveable = 0x0002

	ecmFirst       = 0x1500
	emSetCueBanner = ecmFirst + 1

	vkReturn = 0x0D
	vkEscape = 0x1B
	vkDelete = 0x2E
	vkF2     = 0x71
	vkF5     = 0x74
	vkDown   = 0x28
	vkUp     = 0x26
	vkLeft   = 0x25
	vkRight  = 0x27
	vkPrior  = 0x21
	vkNext   = 0x22
	vkHome   = 0x24
	vkEnd    = 0x23
	vkTab    = 0x09

	sbSetParts = 0x0400 + 4
	sbSetText  = 0x0400 + 1

	wmUser = 0x0400
	wmApp  = 0x8000

	msgSearchResult = wmApp + 1
	msgStatusUpdate = wmApp + 2
	msgPreviewReady = wmApp + 3
)

// logfont for CreateFontIndirectW.
type logfont struct {
	Height         int32
	Width          int32
	Escapement     int32
	Orientation    int32
	Weight         int32
	Italic         uint8
	Underline      uint8
	StrikeOut      uint8
	CharSet        uint8
	OutPrecision   uint8
	ClipPrecision  uint8
	Quality        uint8
	PitchAndFamily uint8
	FaceName       [32]uint16
}

const fwNormal = 400

// windowDPI returns the DPI for hwnd, falling back to the screen DC.
func windowDPI(hwnd uintptr) int {
	if pGetDpiForWindow.Addr() != 0 {
		if r, _, _ := syscall.SyscallN(pGetDpiForWindow.Addr(), hwnd); r != 0 {
			return int(r)
		}
	}
	hdc, _, _ := syscall.SyscallN(pGetDC.Addr(), 0)
	if hdc == 0 {
		return 96
	}
	defer syscall.SyscallN(pReleaseDC.Addr(), 0, hdc)
	r, _, _ := syscall.SyscallN(pGetDeviceCaps.Addr(), hdc, 88) // LOGPIXELSX
	if r == 0 {
		return 96
	}
	return int(r)
}

// scaleDPI scales 96-DPI design units to the current DPI.
func scaleDPI(v int32, dpi int) int32 {
	if dpi <= 0 {
		dpi = 96
	}
	return (v*int32(dpi) + 48) / 96
}

var _ = windows.S_OK
