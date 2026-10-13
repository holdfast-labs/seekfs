package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A published sidecar is a file, and GetDiskFreeSpaceEx only accepts
// directories. The disk queries must anchor an existing file at its parent:
// an error reads as unknown free space, which silently disabled the reserve
// for reconcile and the single-file write guard (real E: index, 2026-10-13).
func TestDiskBytesAnchorExistingFileAtParent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sidecar.gsx")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if free, err := freeDiskBytes(file); err != nil || free == 0 {
		t.Fatalf("freeDiskBytes(existing file) = %d, %v; want the volume's free bytes", free, err)
	}
	if total, err := totalDiskBytes(file); err != nil || total == 0 {
		t.Fatalf("totalDiskBytes(existing file) = %d, %v; want the volume's capacity", total, err)
	}
	// A path that does not exist yet still walks up to its volume.
	if _, err := freeDiskBytes(filepath.Join(filepath.Dir(file), "missing", "x.gsx")); err != nil {
		t.Fatalf("freeDiskBytes(missing path) = %v; want it to resolve via the parent", err)
	}
}
