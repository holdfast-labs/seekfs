//go:build windows

package main

import (
	"errors"
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
	gdi32mod             = syscall.NewLazyDLL("gdi32.dll")
	pStretchDIBits       = gdi32mod.NewProc("StretchDIBits")
	user32presentmod     = syscall.NewLazyDLL("user32.dll")
	pGetDCForPresent     = user32presentmod.NewProc("GetDC")
	pReleaseDCForPresent = user32presentmod.NewProc("ReleaseDC")
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

// gdiHeadless owns the headless renderer for GDI presentation. The headless
// window grows in buckets and is never shrunk, so a resize sweep does not
// recreate the GPU context or reallocate the readback buffers per step.
type gdiHeadless struct {
	mu   sync.Mutex
	hw   *headless.Window
	w    int
	h    int
	img  *image.RGBA
	bgra []byte
	// visible aliases the readback buffer at the current client size so the
	// per-frame sub-rectangle does not allocate.
	visible image.RGBA
}

const (
	gdiSRCCOPY = 0x00CC0020
	gdiDIBRGB  = 0
	// gdiBucket rounds each headless window axis up to a multiple of this
	// many pixels. Growth is rare; shrinking is never needed.
	gdiBucket = 256
)

// gdiTargetSize returns the headless window size to use for one visible size.
// It keeps cur when cur already covers want and otherwise grows each axis up
// to the next gdiBucket multiple. It never shrinks.
func gdiTargetSize(cur, want image.Point) image.Point {
	out := cur
	if want.X > out.X {
		out.X = (want.X + gdiBucket - 1) / gdiBucket * gdiBucket
	}
	if want.Y > out.Y {
		out.Y = (want.Y + gdiBucket - 1) / gdiBucket * gdiBucket
	}
	return out
}

type gdiMonitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
}

// gdiMonitorWorkArea returns the work area of the monitor nearest hwnd.
func gdiMonitorWorkArea(hwnd uintptr) image.Point {
	if hwnd == 0 {
		return image.Point{}
	}
	const monitorDefaultToNearest = 2
	mon, _, _ := pMonitorFromWindow.Call(hwnd, monitorDefaultToNearest)
	if mon == 0 {
		return image.Point{}
	}
	mi := gdiMonitorInfo{CbSize: uint32(unsafe.Sizeof(gdiMonitorInfo{}))}
	ok, _, _ := pGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))
	if ok == 0 {
		return image.Point{}
	}
	return image.Pt(int(mi.RcWork.width()), int(mi.RcWork.height()))
}

// gdiPresizeTarget sizes the first headless context to cover the monitor work
// area in one step. A drag inside that monitor then never crosses a bucket;
// gdiTargetSize still grows for multi-monitor moves or larger windows.
func gdiPresizeTarget(work, want image.Point) image.Point {
	base := image.Point{}
	if work.X > 0 && work.Y > 0 {
		base = gdiTargetSize(base, work)
	}
	return gdiTargetSize(base, want)
}

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
	var target image.Point
	if gh.hw == nil {
		if os.Getenv("SEEKFS_NATIVE_GDIPRESIZE") == "0" {
			target = gdiTargetSize(image.Point{}, size)
		} else {
			target = gdiPresizeTarget(gdiMonitorWorkArea(hwnd), size)
		}
	} else {
		target = gdiTargetSize(image.Point{X: gh.w, Y: gh.h}, size)
	}
	if gh.hw == nil || target != image.Pt(gh.w, gh.h) {
		if gh.hw != nil {
			gh.hw.Release()
			gh.hw = nil
		}
		hw, err := headless.NewWindow(target.X, target.Y)
		if err != nil {
			dbgLog("gdi headless: %v", err)
			return
		}
		gh.hw = hw
		gh.w, gh.h = target.X, target.Y
		gh.img = image.NewRGBA(image.Rectangle{Max: target})
		gh.bgra = make([]byte, target.X*target.Y*4)
	}
	// The ops were laid out at the visible size, which can be smaller than
	// the bucket-sized headless window. Render into the window, then read back
	// and blit only the visible sub-rectangle.
	if err := gh.hw.Frame(gtx.Ops); err != nil {
		dbgLog("gdi frame: %v", err)
		return
	}
	gh.visible = image.RGBA{Pix: gh.img.Pix, Stride: gh.img.Stride, Rect: image.Rectangle{Max: size}}
	if err := gh.hw.Screenshot(&gh.visible); err != nil {
		dbgLog("gdi screenshot: %v", err)
		return
	}
	blitRGBA(hwnd, &gh.visible, gh.bgra)
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

// convertRGBAtoBGRA copies src (premultiplied RGBA) into dst as tightly packed
// BGRA rows. src may be a sub-image with a stride larger than its visible
// width. It writes exactly dstW*dstH*4 bytes and leaves src unchanged.
func convertRGBAtoBGRA(dst []byte, src *image.RGBA) error {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	if len(dst) < w*h*4 {
		return errors.New("bgra buffer too small")
	}
	for y := 0; y < h; y++ {
		sp := src.Pix[y*src.Stride : y*src.Stride+w*4]
		dp := dst[y*w*4 : (y+1)*w*4]
		for i := 0; i < w*4; i += 4 {
			dp[i] = sp[i+2]
			dp[i+1] = sp[i+1]
			dp[i+2] = sp[i]
			dp[i+3] = sp[i+3]
		}
	}
	return nil
}

// blitRGBA paints img (premultiplied RGBA) onto hwnd's client area. GDI wants
// BGRA byte order, so the frame is converted once into the reusable bgra
// buffer and only the visible region is copied.
func blitRGBA(hwnd uintptr, img *image.RGBA, bgra []byte) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return
	}
	if err := convertRGBAtoBGRA(bgra, img); err != nil {
		dbgLog("gdi convert: %v", err)
		return
	}
	hdc, _, _ := syscall.SyscallN(pGetDCForPresent.Addr(), hwnd)
	if hdc == 0 {
		return
	}
	defer syscall.SyscallN(pReleaseDCForPresent.Addr(), hwnd, hdc)
	bi := bitmapInfoV3{Size: 40, Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
	syscall.SyscallN(pStretchDIBits.Addr(),
		hdc,
		0, 0, uintptr(w), uintptr(h),
		0, 0, uintptr(w), uintptr(h),
		uintptr(unsafe.Pointer(&bgra[0])),
		uintptr(unsafe.Pointer(&bi)),
		uintptr(gdiDIBRGB),
		uintptr(gdiSRCCOPY))
}
