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

// contentScopeReserveFraction, contentScopeReserveMinBytes and
// contentScopeReserveMaxBytes define the free-space reserve a content build
// leaves on the volume holding the sidecar: 5% of the volume's capacity,
// clamped to [10 GiB, 20 GiB]. Sharding bounds build/fold memory per shard, so
// disk space is the binding constraint. The same reserve governs selection
// (how much text to index) and the write-time guard, so the two agree.
const contentScopeReserveFraction = 0.05

const contentScopeReserveMinBytes = 10 << 30

const contentScopeReserveMaxBytes = 20 << 30

// contentScopeSpoolMultiplier caps the spool's share of the build peak per
// selected extracted byte. The spool holds one zstd frame per document
// (SpeedFastest, see content_spool.go); measured 0.265 of extracted text on
// the 599 MB Chromium sample, so 0.35 keeps ~30% headroom for denser corpora.
// The split reframes the spool into per-group files (§6x): it is skipped when
// the staged spool exceeds this cap times the extracted bytes (a corpus that
// compresses pathologically), so the copy never doubles an already-large
// spool. Past the cap the write-time guard stays the backstop.
const contentScopeSpoolMultiplier = 0.35

// contentScopeEncodedMultiplier projects encoded sidecar bytes from extracted
// text bytes. Calibrated against the v7 encoding (measured ~0.34 of
// extracted text including tables on the 553 MB Chromium sample); 0.5 keeps
// headroom for denser corpora. The encode-time size-cap check stays the
// backstop: the projection only decides when selection stops. It also feeds
// the estimate's projected output size.
const contentScopeEncodedMultiplier = 0.5

// contentScopePeakMultiplier projects the peak bytes a service build holds on
// the sidecar's volume per selected extracted byte. Since §6x the spool is
// compressed (~0.27x) and reframed into per-group files that are deleted as
// their shards save, so assembly holds remaining spool plus saved output — a
// mix bounded by the larger share, not the sum — and the sum-like instant is
// the split itself (original plus reframed copy: twice the spool cap, 0.7).
// 0.8 covers that with margin; the in-flight shard scratch rides the
// separate contentScopeScratchSlack, single-file builds (at most one shard of
// extracted text) absorb their fixed excess there too, and a corpus past the
// spool cap skips the split and relies on the write-time guard. Measured peak
// on the 599 MB Chromium sample: 0.596 of extracted (was 1.521).
const contentScopePeakMultiplier = 0.8

// contentScopeScratchSlack is the headroom kept for the shards being
// assembled at once: their section files exist alongside the spool until
// each shard is saved and its scratch removed.
func contentScopeScratchSlack() uint64 {
	return uint64(contentShardBuildConcurrency) * uint64(contentShardTargetBytes)
}

// contentScopeReserveFor returns the reserve kept free on the volume holding
// anchorPath. Unknown capacity falls back to contentDiskReserveBytes (0 in
// tests, which disables the reserve there).
func contentScopeReserveFor(anchorPath string) uint64 {
	total, err := contentTotalBytesFn(anchorPath)
	if err != nil || total == 0 {
		return contentDiskReserveBytes
	}
	reserve := uint64(float64(total) * contentScopeReserveFraction)
	if reserve < contentScopeReserveMinBytes {
		reserve = contentScopeReserveMinBytes
	}
	if reserve > contentScopeReserveMaxBytes {
		reserve = contentScopeReserveMaxBytes
	}
	return reserve
}

// contentScopeHeadroom returns the bytes a build may occupy on anchorPath's
// volume at its peak: free space plus the compressed bytes this build has
// already staged in its spool (they are part of the peak, so counting them
// against free space as well would charge them twice), minus the reserve and
// the in-flight shard scratch. ok is false when free space is unknown.
func contentScopeHeadroom(anchorPath string, spooled int64) (headroom uint64, ok bool) {
	free, err := contentFreeBytesFn(anchorPath)
	if err != nil {
		return 0, false
	}
	if spooled > 0 {
		free += uint64(spooled)
	}
	need := contentScopeReserveFor(anchorPath) + contentScopeScratchSlack()
	if free <= need {
		return 0, true
	}
	return free - need, true
}

// contentScopeAllowance returns the extracted-text bytes selectable before
// the build's peak (spool plus output, contentScopePeakMultiplier) would
// breach the reserve on anchorPath's volume, given the bytes this build has
// already spooled: headroom / peak multiplier, and 0 at or below the reserve,
// so it never grows as free space shrinks. Unknown free space means unlimited
// (the write-time guard is the backstop). Callers recompute it as selection
// proceeds and keep extracting past it to tally exact dropped counts, which
// health reports.
func contentScopeAllowance(anchorPath string, spooled int64) int64 {
	headroom, ok := contentScopeHeadroom(anchorPath, spooled)
	if !ok {
		return int64(^uint64(0) >> 1)
	}
	allowance := float64(headroom) / contentScopePeakMultiplier
	if allowance >= float64(int64(^uint64(0)>>1)) {
		return int64(^uint64(0) >> 1)
	}
	return int64(allowance)
}

// contentScopeFitsReserve reports whether selecting extracted bytes (total,
// including the candidate) keeps the build's projected peak within the
// headroom above the reserve on the volume holding anchorPath, before
// anything is spooled: the estimate's view of contentScopeAllowance. Unknown
// free space never stops the selection (the write-time guard is the
// backstop).
func contentScopeFitsReserve(anchorPath string, extracted int64) bool {
	if extracted < 0 {
		return true
	}
	headroom, ok := contentScopeHeadroom(anchorPath, 0)
	if !ok {
		return true
	}
	return headroom > 0 && float64(extracted)*contentScopePeakMultiplier <= float64(headroom)
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
// holding path has less than its reserve free (the same reserve selection
// uses). An unknown free size (for example on a non-Windows build) does not
// block the build.
func contentCheckDiskReserve(path string) error {
	free, err := contentFreeBytesFn(path)
	if err != nil {
		return nil
	}
	reserve := contentScopeReserveFor(path)
	if free < reserve {
		return &contentDiskSpaceError{path: path, free: free, reserve: reserve}
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
