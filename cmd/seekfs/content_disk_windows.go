//go:build windows

package main

// contentFreeDiskBytes reports the caller's free bytes on the volume holding
// path, for the disk-derived content scope budget (Problem 1). It reuses the
// existing freeDiskBytes helper, which resolves through non-existent parents
// to the enclosing volume.
func contentFreeDiskBytes(path string) (uint64, error) {
	return freeDiskBytes(path)
}

// contentTotalDiskBytes reports the total capacity bytes of the volume
// holding path, for the scope reserve (10% of capacity). It resolves through
// non-existent parents like freeDiskBytes so a sidecar path that does not
// exist yet still anchors to its volume.
func contentTotalDiskBytes(path string) (uint64, error) {
	return totalDiskBytes(path)
}

// contentTotalRamBytes reports the machine's total physical memory, for
// sizing the shared decompressed-block cache. An error (or 0) means unknown:
// the cache falls back to its fixed default.
func contentTotalRamBytes() (uint64, error) {
	return totalRamBytes()
}
