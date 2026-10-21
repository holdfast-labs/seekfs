//go:build windows

package main

import (
	"os"
	"strings"
	"testing"
)

// TestVolumeLabel checks Explorer-style volume names. It uses the system
// drive, which exists on every machine (CI runners have no E:).
func TestVolumeLabel(t *testing.T) {
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	got := volumeLabel(drive + `\`)
	if got == "" {
		t.Fatalf("empty volume label for %s\\", drive)
	}
	if !strings.Contains(got, drive) {
		t.Fatalf("volume label %q missing drive letter", got)
	}
	t.Logf("volume label = %q", got)
	if volumeLabel("") != "" {
		t.Fatal("empty root must yield empty label")
	}
}
