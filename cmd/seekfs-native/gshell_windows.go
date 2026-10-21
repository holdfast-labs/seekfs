//go:build windows

package main

import (
	"image"
	"image/color"
	"sync"
	"syscall"
	"unsafe"
)

// gshell_windows.go: the Windows shellBridge implementation. It reuses the
// proven Win32 integrations: the STA context-menu service thread,
// SHCreateDataObject drag-out, SHFileOperationW recycle, ShellExecute verbs,
// and rasterized SHGetImageList icons.

// winBridge owns the HWND-dependent shell operations.
type winBridge struct {
	mu    sync.Mutex
	hwnd  uintptr
	icons *iconStore
}

func newPlatformBridge() shellBridge {
	startContextMenuService()
	return &winBridge{icons: newIconStore()}
}

func (b *winBridge) SystemDark() bool          { return systemTheme() }
func (b *winBridge) SystemAccent() color.NRGBA { return systemAccent() }

func (b *winBridge) SetHWND(hwnd uintptr) {
	b.mu.Lock()
	b.hwnd = hwnd
	b.mu.Unlock()
}

func (b *winBridge) ApplyFrame(dark bool) {
	b.mu.Lock()
	hwnd := b.hwnd
	b.mu.Unlock()
	if hwnd != 0 {
		applyFrameTheme(hwnd, dark)
	}
}

func (b *winBridge) Open(path string) error       { return shellOpen(path) }
func (b *winBridge) Reveal(path string) error     { return shellReveal(path) }
func (b *winBridge) Properties(path string) error { return shellProperties(path) }

func (b *winBridge) Recycle(paths []string) error {
	b.mu.Lock()
	hwnd := b.hwnd
	b.mu.Unlock()
	return recyclePaths(hwnd, paths)
}

func (b *winBridge) CopyFiles(paths []string) error { return copyPathsToClipboard(paths) }

func (b *winBridge) ShowContextMenu(paths []string, x, y int32, done func(bool, error)) {
	b.mu.Lock()
	hwnd := b.hwnd
	b.mu.Unlock()
	go func() {
		shown, err := showMenuAt(hwnd, paths, x, y)
		done(shown, err)
	}()
}

func (b *winBridge) BeginDrag(paths []string) error {
	b.mu.Lock()
	hwnd := b.hwnd
	b.mu.Unlock()
	return doDragDropFiles(hwnd, paths)
}

func (b *winBridge) Icon(path string, isDir bool, size int) image.Image {
	return b.icons.image(path, isDir, size)
}

func (b *winBridge) TypeName(path string, isDir bool) string {
	if t := b.icons.typeName(path, isDir); t != "" {
		return t
	}
	if isDir {
		return "File folder"
	}
	return "File"
}

// showMenuAt shows the shell context menu synchronously at screen coords.
// It must run on the STA thread that owns the menu service; the service
// marshals via its request channel, so any goroutine may call this.
func showMenuAt(owner uintptr, paths []string, x, y int32) (bool, error) {
	startContextMenuService()
	resCh := make(chan ctxMenuResult, 1)
	ctxMenuSvc.req <- ctxMenuRequest{paths: paths, screenX: x, screenY: y, owner: owner, result: resCh}
	res := <-resCh
	return res.shown, res.err
}

// clientToScreen converts window-client pixels to screen pixels.
func clientToScreen(hwnd uintptr, x, y int32) (int32, int32) {
	pt := point{X: x, Y: y}
	r, _, _ := syscall.SyscallN(clientToScreenProc.Addr(), hwnd, uintptr(unsafe.Pointer(&pt)))
	if r == 0 {
		return x, y
	}
	return pt.X, pt.Y
}

var clientToScreenProc = proc(modUser32, "ClientToScreen")
