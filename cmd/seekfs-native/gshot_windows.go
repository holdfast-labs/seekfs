//go:build windows

package main

import (
	"image"
	"image/png"
	"os"
	"sync"

	"gioui.org/gpu/headless"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
)

// gshot_windows.go: live-app diagnostic — re-render the current frame's UI
// headlessly and save it, proving whether on-screen frames carry content.
// Gated by SEEKFS_NATIVE_SAVESHOT=<png path>; saves once when rows arrive.

var saveshotOnce sync.Once

func (u *UI) maybeSaveShot(gtx layout.Context) {
	path := os.Getenv("SEEKFS_NATIVE_SAVESHOT")
	if path == "" {
		return
	}
	u.mu.Lock()
	n := len(u.rows)
	u.mu.Unlock()
	if n == 0 {
		return
	}
	saveshotOnce.Do(func() { u.saveShot(path, gtx) })
}

func (u *UI) saveShot(path string, src layout.Context) {
	size := src.Constraints.Max
	r := new(input.Router)
	ops2 := new(op.Ops)
	gtx := layout.Context{
		Ops:         ops2,
		Metric:      src.Metric,
		Constraints: layout.Exact(size),
		Source:      r.Source(),
	}
	u.layout(gtx)
	r.Frame(gtx.Ops)
	hw, err := headless.NewWindow(size.X, size.Y)
	if err != nil {
		dbgLog("saveshot headless: %v", err)
		return
	}
	defer hw.Release()
	if err := hw.Frame(gtx.Ops); err != nil {
		dbgLog("saveshot frame: %v", err)
		return
	}
	img := image.NewRGBA(image.Rectangle{Max: size})
	if err := hw.Screenshot(img); err != nil {
		dbgLog("saveshot screenshot: %v", err)
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	_ = png.Encode(f, img)
	dbgLog("saveshot saved %s", path)
}

var _ = unit.Dp(0)
