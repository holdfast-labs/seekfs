//go:build windows

package main

import (
	"image"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"gioui.org/gpu/headless"
	"gioui.org/layout"
)

// gpresent_gdi_windows.go: the GDI fallback presenter. Gio renders the frame
// ops headlessly (GPU, proven working) and the pixels are blitted to the
// window's client area with StretchDIBits (GDI, universally composited).
// Used when Gio's DXGI swapchain never composites (persistent
// DXGI_STATUS_OCCLUDED on machines with headless/capture displays).

var (
	gdi32mod              = syscall.NewLazyDLL("gdi32.dll")
	pStretchDIBits        = gdi32mod.NewProc("StretchDIBits")
	user32presentmod      = syscall.NewLazyDLL("user32.dll")
	pGetDCForPresent      = user32presentmod.NewProc("GetDC")
	pReleaseDCForPresent  = user32presentmod.NewProc("ReleaseDC")
)

// bitmapInfoV3 is a 40-byte BITMAPINFOHEADER for 32bpp BI_RGB DIBs.
type bitmapInfoV3 struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// gdiHeadless owns the headless renderer for GDI presentation.
type gdiHeadless struct {
	mu  sync.Mutex
	hw  *headless.Window
	w   int
	h   int
	img *image.RGBA
}

const (
	gdiSRCCOPY = 0x00CC0020
	gdiDIBRGB  = 0
)

// presentGDI renders gtx.Ops headlessly and blits to the window. Frame
// times are logged when SEEKFS_NATIVE_DEBUGLOG is set.
func (u *UI) presentGDI(gtx layout.Context) {
	start := time.Now()
	size := gtx.Constraints.Max
	if size.X <= 0 || size.Y <= 0 {
		return
	}
	u.mu.Lock()
	hwnd := u.hwnd
	u.mu.Unlock()
	if hwnd == 0 {
		return
	}
	gh := &u.gdiHead
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if gh.hw == nil || gh.w != size.X || gh.h != size.Y {
		if gh.hw != nil {
			gh.hw.Release()
			gh.hw = nil
		}
		hw, err := headless.NewWindow(size.X, size.Y)
		if err != nil {
			dbgLog("gdi headless: %v", err)
			return
		}
		gh.hw = hw
		gh.w, gh.h = size.X, size.Y
		gh.img = image.NewRGBA(image.Rectangle{Max: size})
	}
	// Headless Frame reads the ops (layout already recorded into them).
	if err := gh.hw.Frame(gtx.Ops); err != nil {
		dbgLog("gdi frame: %v", err)
		return
	}
	if err := gh.hw.Screenshot(gh.img); err != nil {
		dbgLog("gdi screenshot: %v", err)
		return
	}
	blitRGBA(hwnd, gh.img)
	if os.Getenv("SEEKFS_NATIVE_DEBUGLOG") != "" {
		dt := time.Since(start).Seconds() * 1000
		u.mu.Lock()
		u.frameTimes = append(u.frameTimes, dt)
		if len(u.frameTimes) > 600 {
			u.frameTimes = u.frameTimes[len(u.frameTimes)-600:]
		}
		n := len(u.frameTimes)
		sum := 0.0
		for _, v := range u.frameTimes {
			sum += v
		}
		u.mu.Unlock()
		if n%60 == 0 {
			dbgLog("gdi frame avg=%.2fms n=%d", sum/float64(n), n)
		}
	}
}

// blitRGBA paints img (premultiplied RGBA) onto hwnd's client area. GDI
// wants BGRA byte order, so R and B are swapped around the blit (swapped
// back afterwards to keep the cached image valid).
func blitRGBA(hwnd uintptr, img *image.RGBA) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	pix := img.Pix
	if w <= 0 || h <= 0 || len(pix) < w*h*4 {
		return
	}
	hdc, _, _ := syscall.SyscallN(pGetDCForPresent.Addr(), hwnd)
	if hdc == 0 {
		return
	}
	defer syscall.SyscallN(pReleaseDCForPresent.Addr(), hwnd, hdc)
	for i := 0; i+3 < len(pix); i += 4 {
		pix[i], pix[i+2] = pix[i+2], pix[i]
	}
	bi := bitmapInfoV3{Size: 40, Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
	syscall.SyscallN(pStretchDIBits.Addr(),
		hdc,
		0, 0, uintptr(w), uintptr(h),
		0, 0, uintptr(w), uintptr(h),
		uintptr(unsafe.Pointer(&pix[0])),
		uintptr(unsafe.Pointer(&bi)),
		uintptr(gdiDIBRGB),
		uintptr(gdiSRCCOPY))
	for i := 0; i+3 < len(pix); i += 4 {
		pix[i], pix[i+2] = pix[i+2], pix[i]
	}
}
