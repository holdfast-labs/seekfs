//go:build !windows

package main

import (
	"os"
)

// currentProcessUserSID is unavailable off Windows: CLI fallback uses the
// legacy shared file there.
func currentProcessUserSID() string {
	return ""
}

// restrictSignalStoreFile tightens a per-user signals file off Windows:
// owner-only permissions, best-effort.
func restrictSignalStoreFile(path, sid string) {
	if path == "" || sid == "" || signalStoreDisableACLForTest {
		return
	}
	_ = os.Chmod(path, 0o600)
}
