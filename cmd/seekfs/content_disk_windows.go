//go:build windows

package main

// contentFreeDiskBytes reports the caller's free bytes on the volume holding
// path, for the disk-derived content scope budget (Problem 1). It reuses the
// existing freeDiskBytes helper, which resolves through non-existent parents
// to the enclosing volume.
func contentFreeDiskBytes(path string) (uint64, error) {
	return freeDiskBytes(path)
}
