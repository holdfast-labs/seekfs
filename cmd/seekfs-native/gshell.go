package main

import (
	"image"
	"image/color"
)

// gshell.go: the bridge between the portable Gio UI and OS shell
// integrations. The Windows implementation lives in gshell_windows.go and
// reuses the proven Win32 code (context menu service, SHGetImageList icons,
// SHCreateDataObject drag-out, Recycle Bin, ShellExecute verbs). Other
// platforms get a stub.

// shellBridge is implemented per-OS.
type shellBridge interface {
	// SystemDark reports the OS app theme.
	SystemDark() bool
	// SystemAccent returns the OS accent color.
	SystemAccent() color.NRGBA
	// SetHWND gives the bridge the top-level window handle (from Gio's
	// ViewEvent) for frame theming, menus and drag-drop.
	SetHWND(hwnd uintptr)
	// ApplyFrame requests Mica + dark title bar for the current mode.
	ApplyFrame(dark bool)
	// Verbs.
	Open(path string) error
	Reveal(path string) error
	Properties(path string) error
	Recycle(paths []string) error
	CopyFiles(paths []string) error
	// ShowContextMenu shows the native shell menu for paths at the screen
	// position, reporting completion on done (always called, possibly with
	// an error; runs on a background goroutine).
	ShowContextMenu(paths []string, x, y int32, done func(shown bool, err error))
	// BeginDrag starts an OS drag of paths. It runs DoDragDrop's modal loop
	// on the caller's (UI) thread.
	BeginDrag(paths []string) error
	// Icon returns the shell icon raster for a file or folder at the given
	// pixel size (16, 32 or 48). May return nil.
	Icon(path string, isDir bool, size int) image.Image
	// TypeName returns the shell type name ("Text Document", "File folder").
	TypeName(path string, isDir bool) string
}

// newShellBridge builds the platform bridge.
func newShellBridge() shellBridge { return newPlatformBridge() }
