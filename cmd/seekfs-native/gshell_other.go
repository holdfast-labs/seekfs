//go:build !windows

package main

import (
	"errors"
	"image"
	"image/color"
)

type stubBridge struct{}

func newPlatformBridge() shellBridge { return &stubBridge{} }

func (s *stubBridge) SystemDark() bool               { return false }
func (s *stubBridge) SystemAccent() color.NRGBA      { return color.NRGBA{R: 0, G: 0x66, B: 0xCC, A: 0xFF} }
func (s *stubBridge) SetHWND(hwnd uintptr)           {}
func (s *stubBridge) ApplyFrame(dark bool)           {}
func (s *stubBridge) Open(path string) error         { return errors.New("open requires Windows") }
func (s *stubBridge) Reveal(path string) error       { return errors.New("reveal requires Windows") }
func (s *stubBridge) Properties(path string) error   { return errors.New("properties requires Windows") }
func (s *stubBridge) Recycle(paths []string) error   { return errors.New("recycle requires Windows") }
func (s *stubBridge) CopyFiles(paths []string) error { return errors.New("copy requires Windows") }
func (s *stubBridge) ShowContextMenu(paths []string, x, y int32, done func(bool, error)) {
	done(false, errors.New("shell menu requires Windows"))
}
func (s *stubBridge) BeginDrag(paths []string) error { return errors.New("drag requires Windows") }
func (s *stubBridge) Icon(path string, isDir bool, size int) image.Image {
	return nil
}
func (s *stubBridge) TypeName(path string, isDir bool) string {
	if isDir {
		return "File folder"
	}
	return "File"
}
func (s *stubBridge) PickFolder(title string) (string, error) {
	return "", errors.New("folder picker requires Windows")
}
