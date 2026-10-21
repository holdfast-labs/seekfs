//go:build windows

package main

// Opening a content file with FILE_FLAG_OPEN_NO_RECALL makes a cloud
// placeholder (OneDrive and friends) fail or return without hydrating the file,
// closing the TOCTOU between the attribute check and the read (plan D5).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// contentFileFlagOpenNoRecall is FILE_FLAG_OPEN_NO_RECALL.
const contentFileFlagOpenNoRecall = 0x00100000

// contentIOPriorityHintLow is IoPriorityHintLow for FILE_IO_PRIORITY_HINT_INFO:
// background indexing yields disk priority to interactive I/O. (VeryLow is
// reserved for memory-manager paging; Low is the background level.)
const contentIOPriorityHintLow = 1

// contentLongPath prefixes an absolute path with the extended-length marker
// when it can exceed MAX_PATH. go's os package does this internally, but a raw
// windows.CreateFile call does not: without the prefix a valid file in a deep
// tree fails to open (ERROR_PATH_NOT_FOUND) even though the walker indexed it.
// Clean paths are required: the marker disables the normalizer, so "." and ".."
// must not survive. UNC paths use the \\?\UNC\ form.
func contentLongPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return path
	}
	if len(path) < 248 {
		return path
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		abs, err := filepath.Abs(clean)
		if err != nil {
			return path
		}
		clean = abs
	}
	if strings.HasPrefix(clean, `\\`) {
		return `\\?\UNC\` + clean[2:]
	}
	return `\\?\` + clean
}

func contentOpenNoRecall(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(contentLongPath(path))
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
	// Lower this handle's disk priority while foreground activity exists, so
	// background extraction (service builds, USN delta) is a good citizen
	// next to interactive I/O. An idle build keeps normal priority: the Low
	// hint measurably throttles reads even when the disk would otherwise be
	// idle, and there is nothing to yield to. The hint is per-handle, so
	// other openers of the file are unaffected; a failure here must not fail
	// the open (e.g. a filesystem that rejects the class).
	if contentLowerIOPriorityNow() {
		contentLowerHandleIOPriority(h)
	}
	return os.NewFile(uintptr(h), path), nil
}

// contentLowerHandleIOPriority sets FileIoPriorityHintInfo=Low on h. Best
// effort: filesystems may reject the information class.
func contentLowerHandleIOPriority(h windows.Handle) {
	hint := uint32(contentIOPriorityHintLow)
	_ = windows.SetFileInformationByHandle(h, windows.FileIoPriorityHintInfo, (*byte)(unsafe.Pointer(&hint)), uint32(unsafe.Sizeof(hint)))
}

// isContentSharingViolation reports whether err is a Windows file-locking
// sharing violation (ERROR_SHARING_VIOLATION, 32): another opener (AV
// indexer, a writer) holds the file without share rights. The verify path
// retries those once; any other error fails closed.
func isContentSharingViolation(err error) bool {
	var errno windows.Errno
	if errors.As(err, &errno) {
		return errno == windows.ERROR_SHARING_VIOLATION
	}
	return false
}
