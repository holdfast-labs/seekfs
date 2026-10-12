//go:build windows

package main

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// theme_windows.go: system theme tracking (dark/light follows Windows),
// DWM backdrop (Mica with fallback), immersive dark title bars, dark-mode
// common controls via uxtheme, and the system UI font.

// DWM attributes (see dwmapi.h).
const (
	dwmwaUseImmersiveDarkMode = 20
	dwmwaSystemBackdropType   = 38
	dwmsbtMainWindow          = 2 // Mica
)

// appTheme is a snapshot of the Windows personalization state.
type appTheme struct {
	dark bool
	dpi  int
}

func currentAppTheme(hwnd uintptr) appTheme {
	return appTheme{dark: systemUsesDarkMode(), dpi: windowDPI(hwnd)}
}

// systemUsesDarkMode reads AppsUseLightTheme: 0 (or missing with a dark
// system theme) means dark mode. Missing key defaults to light.
func systemUsesDarkMode() bool {
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

// applyWindowTheme applies the backdrop, title-bar mode and control themes
// for hwnd. controls lists child HWNDs that need SetWindowTheme
// ("DarkMode_Explorer" in dark mode, "Explorer" in light mode); the
// ListView/TreeView/Header/Edit pick up dark scrollbars, caret and selection
// colors from the theme, so custom drawing stays minimal.
func applyWindowTheme(hwnd uintptr, dark bool, controls []uintptr) {
	useDark := uint32(0)
	if dark {
		useDark = 1
	}
	// Immersive dark title bar (Win10 1903+; harmless elsewhere).
	if pDwmSetWindowAttribute.Addr() != 0 {
		_, _, _ = syscall.SyscallN(pDwmSetWindowAttribute.Addr(), hwnd,
			uintptr(dwmwaUseImmersiveDarkMode), uintptr(unsafe.Pointer(&useDark)), unsafe.Sizeof(useDark))
		// Mica backdrop where supported (Win11 22000+); ignored otherwise.
		mica := uint32(dwmsbtMainWindow)
		_, _, _ = syscall.SyscallN(pDwmSetWindowAttribute.Addr(), hwnd,
			uintptr(dwmwaSystemBackdropType), uintptr(unsafe.Pointer(&mica)), unsafe.Sizeof(mica))
	}
	for _, c := range controls {
		if pSetWindowTheme.Addr() == 0 {
			continue
		}
		if dark {
			_, _, _ = syscall.SyscallN(pSetWindowTheme.Addr(), c,
				uintptr(unsafe.Pointer(utf16("DarkMode_Explorer"))), 0)
		} else {
			_, _, _ = syscall.SyscallN(pSetWindowTheme.Addr(), c,
				uintptr(unsafe.Pointer(utf16("Explorer"))), 0)
		}
	}
}

// colorref builds a COLORREF (0x00BBGGRR).
func colorref(r, g, b uint32) uintptr { return uintptr(r | (g << 8) | (b << 16)) }

// palette holds the app's control colors per mode. The dark values match
// Windows 11 File Explorer's dark surfaces.
type palette struct {
	windowBg   uintptr // main window / list background
	text       uintptr
	grayText   uintptr
	headerBg   uintptr
	headerText uintptr
	editBg     uintptr
	editText   uintptr
	statusBg   uintptr
	statusText uintptr
	accent     uintptr
	// selection is drawn by the themed ListView itself.
}

func darkPalette() palette {
	return palette{
		windowBg:   colorref(0x20, 0x20, 0x20), // #202020 Explorer dark
		text:       colorref(0xFF, 0xFF, 0xFF),
		grayText:   colorref(0xA0, 0xA0, 0xA0),
		headerBg:   colorref(0x2B, 0x2B, 0x2B),
		headerText: colorref(0xFF, 0xFF, 0xFF),
		editBg:     colorref(0x2D, 0x2D, 0x2D),
		editText:   colorref(0xFF, 0xFF, 0xFF),
		statusBg:   colorref(0x20, 0x20, 0x20),
		statusText: colorref(0xCC, 0xCC, 0xCC),
		accent:     colorref(0x4C, 0xC2, 0xFF),
	}
}

func lightPalette() palette {
	return palette{
		windowBg:   colorref(0xF3, 0xF3, 0xF3), // Win11 light
		text:       colorref(0x00, 0x00, 0x00),
		grayText:   colorref(0x60, 0x60, 0x60),
		headerBg:   colorref(0xF9, 0xF9, 0xF9),
		headerText: colorref(0x00, 0x00, 0x00),
		editBg:     colorref(0xFF, 0xFF, 0xFF),
		editText:   colorref(0x00, 0x00, 0x00),
		statusBg:   colorref(0xF3, 0xF3, 0xF3),
		statusText: colorref(0x40, 0x40, 0x40),
		accent:     colorref(0x00, 0x66, 0xCC),
	}
}

func paletteFor(dark bool) palette {
	if dark {
		return darkPalette()
	}
	return lightPalette()
}

// ---- system font ----

// systemUIFont creates the system message-box font (Segoe UI Variable on
// Windows 11, Segoe UI on 10) at the right size for dpi. Callers own the
// HFONT and must DeleteObject it.
func systemUIFont(dpi int) uintptr {
	lf := messageLogFont()
	// NCM heights are in logical units at 96 DPI; scale to the window DPI.
	lf.Height = (lf.Height*int32(dpi) + 48) / 96
	if lf.Height == 0 {
		lf.Height = -scaleDPI(12, dpi)
	}
	h, _, _ := syscall.SyscallN(pCreateFontIndirectW.Addr(), uintptr(unsafe.Pointer(&lf)))
	return h
}

// messageLogFont reads lfMessageFont from NONCLIENTMETRICS via
// SystemParametersInfoW, falling back to Segoe UI 9pt.
func messageLogFont() logfont {
	const ncmSize = 512
	buf := make([]byte, ncmSize)
	*(*uint32)(unsafe.Pointer(&buf[0])) = 500 // pre-Vista size works everywhere
	r, _, _ := syscall.SyscallN(pSystemParametersInfoW.Addr(), 41 /*SPI_GETNONCLIENTMETRICS*/, uintptr(ncmSize), uintptr(unsafe.Pointer(&buf[0])), 0)
	if r != 0 {
		lf := *(*logfont)(unsafe.Pointer(&buf[408])) // lfMessageFont offset
		if lf.Height != 0 {
			return lf
		}
	}
	var lf logfont
	lf.Height = -12
	lf.Weight = fwNormal
	lf.CharSet = 1 // DEFAULT_CHARSET
	copy(lf.FaceName[:], utf16Slice("Segoe UI Variable Text"))
	if lf.FaceName[0] == 0 {
		copy(lf.FaceName[:], utf16Slice("Segoe UI"))
	}
	return lf
}

func utf16Slice(s string) []uint16 {
	u, _ := syscall.UTF16FromString(s)
	return u
}
