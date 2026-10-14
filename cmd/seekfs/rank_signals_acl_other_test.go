//go:build !windows

package main

import (
	"os"
	"testing"
)

func assertSignalStoreACL(t *testing.T, path, sid string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("per-user file mode = %o, want 600", perm)
	}
}
