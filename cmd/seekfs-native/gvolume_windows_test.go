//go:build windows

package main

import (
	"strings"
	"testing"
)

// TestVolumeLabel checks Explorer-style volume names.
func TestVolumeLabel(t *testing.T) {
	got := volumeLabel(`E:\`)
	if got == "" {
		t.Fatal("empty volume label for E:\\")
	}
	if !strings.Contains(got, "E:") {
		t.Fatalf("volume label %q missing drive letter", got)
	}
	t.Logf("volume label = %q", got)
	if volumeLabel("") != "" {
		t.Fatal("empty root must yield empty label")
	}
}
