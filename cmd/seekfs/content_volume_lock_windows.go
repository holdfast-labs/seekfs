//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// acquireContentVolumeLock takes a non-blocking exclusive byte-range lock on
// gsx+".lock". It returns errContentVolumeLocked when another handle (a live
// service, or a concurrent CLI build) already holds it, so the caller fails
// fast instead of blocking. The file is opened with FILE_SHARE_DELETE so a test
// or cleanup can still unlink a directory that contains a held lock file.
func acquireContentVolumeLock(gsx string) (*contentVolumeLock, error) {
	path := contentVolumeLockPath(gsx)
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	ov := new(windows.Overlapped)
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ov,
	)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errContentVolumeLocked
		}
		return nil, err
	}
	return &contentVolumeLock{file: f}, nil
}

// release drops the lock and closes the handle. The lock file itself is left in
// place: removing it would race a concurrent acquirer.
func (l *contentVolumeLock) release() {
	if l == nil || l.file == nil {
		return
	}
	ov := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, ov)
	_ = l.file.Close()
	l.file = nil
}
