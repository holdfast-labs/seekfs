//go:build windows

package main

// Mapping the `.gsx` sidecar follows the record-index mmap path (mmap_windows.go
// mapIndexFile): open, map, expose as a []byte. Unlike mapIndexFile it closes
// the file handle as soon as the view exists — a live Windows section keeps the
// file's pages valid, and holding the handle open would block the atomic
// temp+rename that replaces the sidecar while the previous base is still mapped
// (Windows refuses to rename over a file with an open handle). The release
// unmaps the view and closes the mapping handle; the caller owns it and calls it
// exactly once. A failure returns a nil release so the caller can fall back to a
// heap read.

import (
	"os"

	"golang.org/x/sys/windows"
)

func contentMapFile(path string) (data []byte, release func(), err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if info.Size() <= 0 {
		_ = f.Close()
		return nil, nil, os.ErrInvalid
	}
	mapping, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	addr, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		_ = windows.CloseHandle(mapping)
		_ = f.Close()
		return nil, nil, err
	}
	// The section (mapping) now owns the file's data; drop the handle so the
	// sidecar can be replaced by an atomic rename while this view is still live.
	_ = f.Close()
	data = mappedViewBytes(addr, int(info.Size()))
	release = func() {
		_ = windows.UnmapViewOfFile(addr)
		_ = windows.CloseHandle(mapping)
		fireContentMappingReleaseHook()
	}
	return data, release, nil
}
