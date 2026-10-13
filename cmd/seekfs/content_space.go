package main

// Disk safety for service content builds and folds. Scratch files (the text
// spool and posting spill runs) live next to the sidecar, on the same volume
// as the output, instead of in the process temp directory: the service runs
// as LocalSystem, whose temp directory is on the system drive, so a build for
// another volume could fill C: with gigabytes of spool. Builds also check
// that the volume keeps a free-space reserve while they write, and stop with
// a clear error instead of running the disk out of space.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// contentWorkPrefix names scratch directories created next to a sidecar, so a
// later build can remove ones left behind by a crash.
const contentWorkPrefix = ".seekfs-work-"

// contentDiskReserveBytes is the free space a content build leaves on the
// volume it writes to. A var so tests can raise it.
var contentDiskReserveBytes uint64 = 10 << 30

// contentFreeBytesFn reports free bytes on the volume holding a path; a var
// so tests can simulate a filling disk.
var contentFreeBytesFn = contentFreeDiskBytes

// contentDiskCheckEvery is how many bytes a build writes between free-space
// checks.
const contentDiskCheckEvery = 256 << 20

// contentDiskSpaceError reports a build stopped to keep the reserve free.
type contentDiskSpaceError struct {
	path    string
	free    uint64
	reserve uint64
}

func (e *contentDiskSpaceError) Error() string {
	where := filepath.VolumeName(e.path)
	if where == "" {
		where = e.path
	}
	return fmt.Sprintf("content index stopped: only %s free on %s (keeping %s free); free up space or move the index to a larger drive",
		contentFormatBytes(e.free), where, contentFormatBytes(e.reserve))
}

// contentCheckDiskReserve returns a *contentDiskSpaceError when the volume
// holding path has less than the reserve free. An unknown free size (for
// example on a non-Windows build) does not block the build.
func contentCheckDiskReserve(path string) error {
	free, err := contentFreeBytesFn(path)
	if err != nil {
		return nil
	}
	if free < contentDiskReserveBytes {
		return &contentDiskSpaceError{path: path, free: free, reserve: contentDiskReserveBytes}
	}
	return nil
}

// contentMkdirWork creates a fresh scratch directory next to the sidecar at
// gsx, after removing scratch directories a crashed build left there. The
// per-volume content lock means no other build owns them. It falls back to
// the process temp directory when gsx is empty or its directory is not
// writable.
func contentMkdirWork(gsx string) (string, error) {
	if gsx != "" {
		dir := filepath.Dir(gsx)
		if err := os.MkdirAll(dir, 0o755); err == nil {
			contentRemoveStaleWork(dir)
			if work, err := os.MkdirTemp(dir, contentWorkPrefix+"*"); err == nil {
				return work, nil
			}
		}
	}
	return os.MkdirTemp("", "seekfs-content-*")
}

func contentRemoveStaleWork(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), contentWorkPrefix) {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

func contentFormatBytes(n uint64) string {
	const gib = 1 << 30
	if n >= gib {
		return fmt.Sprintf("%.1f GB", float64(n)/gib)
	}
	return fmt.Sprintf("%d MB", n>>20)
}
