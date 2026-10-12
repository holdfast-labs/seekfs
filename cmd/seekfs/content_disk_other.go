//go:build !windows

package main

import "errors"

// contentFreeDiskBytes is a no-op off Windows: the derived budget falls back
// to the legacy half-of-cap default.
func contentFreeDiskBytes(_ string) (uint64, error) {
	return 0, errors.New("content disk budget is windows-only")
}
