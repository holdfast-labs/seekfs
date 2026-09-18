//go:build windows

package main

// Opening a content file with FILE_FLAG_OPEN_NO_RECALL makes a cloud
// placeholder (OneDrive and friends) fail or return without hydrating the file,
// closing the TOCTOU between the attribute check and the read (plan D5).

import (
	"os"

	"golang.org/x/sys/windows"
)

// contentFileFlagOpenNoRecall is FILE_FLAG_OPEN_NO_RECALL.
const contentFileFlagOpenNoRecall = 0x00100000

func contentOpenNoRecall(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|contentFileFlagOpenNoRecall,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
