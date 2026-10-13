//go:build !windows

package main

import "errors"

// contentFreeDiskBytes is a no-op off Windows: the derived budget falls back
// to the legacy half-of-cap default.
func contentFreeDiskBytes(_ string) (uint64, error) {
	return 0, errors.New("content disk budget is windows-only")
}

// contentTotalDiskBytes is a no-op off Windows: the scope reserve falls back
// to the fixed write-guard reserve.
func contentTotalDiskBytes(_ string) (uint64, error) {
	return 0, errors.New("content disk budget is windows-only")
}

// contentTotalRamBytes is a no-op off Windows: the block cache falls back
// to its fixed default.
func contentTotalRamBytes() (uint64, error) {
	return 0, errors.New("content ram size is windows-only")
}
