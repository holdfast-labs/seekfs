//go:build windows

package main

// Opening a content file with FILE_FLAG_OPEN_NO_RECALL makes a cloud
// placeholder (OneDrive and friends) fail or return without hydrating the file,
// closing the TOCTOU between the attribute check and the read (plan D5).

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// contentFileFlagOpenNoRecall is FILE_FLAG_OPEN_NO_RECALL.
const contentFileFlagOpenNoRecall = 0x00100000

// contentIOPriorityHintLow is IoPriorityHintLow for FILE_IO_PRIORITY_HINT_INFO:
// background indexing yields disk priority to interactive I/O. (VeryLow is
// reserved for memory-manager paging; Low is the background level.)
const contentIOPriorityHintLow = 1

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
	// Lower this handle's disk priority so background extraction (service
	// builds, USN delta) is a good citizen next to interactive I/O. The hint
	// is per-handle, so other openers of the file are unaffected; a failure
	// here must not fail the open (e.g. a filesystem that rejects the class).
	contentLowerHandleIOPriority(h)
	return os.NewFile(uintptr(h), path), nil
}

// contentLowerHandleIOPriority sets FileIoPriorityHintInfo=Low on h. Best
// effort: filesystems may reject the information class.
func contentLowerHandleIOPriority(h windows.Handle) {
	hint := uint32(contentIOPriorityHintLow)
	_ = windows.SetFileInformationByHandle(h, windows.FileIoPriorityHintInfo, (*byte)(unsafe.Pointer(&hint)), uint32(unsafe.Sizeof(hint)))
}
