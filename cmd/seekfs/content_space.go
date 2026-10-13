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

// contentTotalBytesFn reports the capacity bytes of the volume holding a
// path; a var so tests can simulate disks of any size. Off Windows it
// errors, and the reserve falls back to contentDiskReserveBytes.
var contentTotalBytesFn = contentTotalDiskBytes

// contentScopeReserveFraction and contentScopeReserveMinBytes define the
// free-space reserve the default (non-explicit) scope budget protects on the
// volume holding the sidecar: the larger of 10% of the volume's capacity or
// 20 GiB. Sharding bounds build/fold memory per 64 MiB shard, so disk space
// is the binding constraint; the reserve keeps a content build from filling
// the volume.
const contentScopeReserveFraction = 0.1

const contentScopeReserveMinBytes = 20 << 30

// contentScopeEncodedMultiplier projects encoded sidecar bytes from extracted
// text bytes. Calibrated against the v7 encoding (measured ~0.34 of
// extracted text including tables on the 553 MB Chromium sample); 0.5 keeps
// headroom for denser corpora. The encode-time size-cap check stays the
// backstop: the projection only decides when selection stops.
const contentScopeEncodedMultiplier = 0.5

// contentScopeReserveFor returns the reserve the default scope budget
// protects on the volume holding anchorPath. Unknown capacity falls back to
// contentDiskReserveBytes (0 in tests, which disables the reserve there).
func contentScopeReserveFor(anchorPath string) uint64 {
	total, err := contentTotalBytesFn(anchorPath)
	if err != nil || total == 0 {
		return contentDiskReserveBytes
	}
	reserve := uint64(float64(total) * contentScopeReserveFraction)
	if reserve < contentScopeReserveMinBytes {
		reserve = contentScopeReserveMinBytes
	}
	return reserve
}

// contentScopeAllowance returns the extracted-text bytes selectable before
// the projected index would breach the reserve on anchorPath's volume:
// (free - reserve) / multiplier. Math.MaxInt64 means unlimited: unknown disk
// figures, or a volume already below its reserve (fail-open; see
// contentScopeFitsReserve). Callers recompute it as selection proceeds (free
// space moves as the spool grows) and keep extracting past it to tally exact
// dropped counts.
func contentScopeAllowance(anchorPath string) int64 {
	free, err := contentFreeBytesFn(anchorPath)
	if err != nil {
		return int64(^uint64(0) >> 1)
	}
	reserve := contentScopeReserveFor(anchorPath)
	if free <= reserve {
		return int64(^uint64(0) >> 1)
	}
	allowance := float64(free-reserve) / contentScopeEncodedMultiplier
	if allowance >= float64(int64(^uint64(0)>>1)) {
		return int64(^uint64(0) >> 1)
	}
	return int64(allowance)
}

// contentScopeFitsReserve reports whether selecting extracted bytes (total,
// including the candidate) still projects an index that leaves the reserve
// free on the volume holding anchorPath. Unknown disk figures never stop the
// build (fail-open): the write-time reserve guard is the backstop. When the
// volume is already below its reserve the check passes: the index is not
// what pushes the volume below, and refusing everything would disable
// content on full disks instead of bounding it.
func contentScopeFitsReserve(anchorPath string, extracted int64) bool {
	if extracted < 0 {
		return true
	}
	free, err := contentFreeBytesFn(anchorPath)
	if err != nil {
		return true
	}
	reserve := contentScopeReserveFor(anchorPath)
	if free <= reserve {
		return true
	}
	projected := uint64(float64(extracted) * contentScopeEncodedMultiplier)
	if projected >= free {
		return false
	}
	return free-projected >= reserve
}

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
