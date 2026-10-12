//go:build windows

package main

import (
	"image/color"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// theme_windows.go: Windows system theme + accent color detection for the
// Gio UI. The UI polls systemTheme() and re-renders on change; all painting
// is GPU-rendered by Gio, so no per-control Win32 theming is needed.

// DWM attributes (see dwmapi.h).
const (
	dwmwaUseImmersiveDarkMode = 20
	dwmwaSystemBackdropType   = 38
	dwmsbtMainWindow          = 2 // Mica
)

// systemTheme reports whether Windows apps should use dark mode
// (AppsUseLightTheme == 0).
func systemTheme() (dark bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	if err != nil {
		return false
	}
	return v == 0
}

// systemAccent returns the user's accent color (DwmGetColorizationColor),
// falling back to a default blue.
func systemAccent() color.NRGBA {
	var dwmapi = syscall.NewLazyDLL("dwmapi.dll")
	p := dwmapi.NewProc("DwmGetColorizationColor")
	var clr uint32
	var opaque uint32
	r, _, _ := syscall.SyscallN(p.Addr(), uintptr(unsafe.Pointer(&clr)), uintptr(unsafe.Pointer(&opaque)))
	if r != 0 {
		return color.NRGBA{R: 0x00, G: 0x66, B: 0xCC, A: 0xFF}
	}
	// COLORREF 0x00BBGGRR.
	return color.NRGBA{R: uint8(clr), G: uint8(clr >> 8), B: uint8(clr >> 16), A: 0xFF}
}

// applyFrameTheme sets the immersive dark title bar and Mica backdrop on a
// top-level HWND. Values are ignored on systems that don't support them.
func applyFrameTheme(hwnd uintptr, dark bool) {
	useDark := uint32(0)
	if dark {
		useDark = 1
	}
	var dwmapi = syscall.NewLazyDLL("dwmapi.dll")
	p := dwmapi.NewProc("DwmSetWindowAttribute")
	if p.Find() != nil {
		return
	}
	_, _, _ = syscall.SyscallN(p.Addr(), hwnd,
		uintptr(dwmwaUseImmersiveDarkMode), uintptr(unsafe.Pointer(&useDark)), unsafe.Sizeof(useDark))
	mica := uint32(dwmsbtMainWindow)
	_, _, _ = syscall.SyscallN(p.Addr(), hwnd,
		uintptr(dwmwaSystemBackdropType), uintptr(unsafe.Pointer(&mica)), unsafe.Sizeof(mica))
}
