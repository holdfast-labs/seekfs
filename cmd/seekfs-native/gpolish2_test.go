//go:build windows

package main

import (
	"image"
	"strings"
	"testing"
)

func TestSidebarVolumeDisplayKeepsPercentage(t *testing.T) {
	long := strings.Repeat("Very Long Local Disk Label ", 8)
	label, suffix := sidebarVolumeDisplay(long, "indexing", 42, 0)
	if label != long {
		t.Fatalf("label changed: %q", label)
	}
	if suffix != "(indexing 42%)" {
		t.Fatalf("suffix = %q, want the percentage", suffix)
	}
	if _, suffix := sidebarVolumeDisplay("X:", "indexing", -1, 0); suffix != "(indexing\u2026)" {
		t.Fatalf("unknown progress suffix = %q", suffix)
	}
	if _, suffix := sidebarVolumeDisplay("X:", "error", 0, 0); suffix != "(error)" {
		t.Fatalf("error suffix = %q", suffix)
	}
	if _, suffix := sidebarVolumeDisplay("X:", "ready", 0, 12000); suffix != "(12,000)" {
		t.Fatalf("count suffix = %q", suffix)
	}
	if label, suffix := sidebarVolumeDisplay("X:", "ready", 0, 0); label != "X:" || suffix != "" {
		t.Fatalf("ready row = %q/%q", label, suffix)
	}
}

func TestGdiPresizeTarget(t *testing.T) {
	cases := []struct {
		name       string
		work, want image.Point
		wantTarget image.Point
	}{
		{"1080p work area", image.Pt(1920, 1040), image.Pt(900, 600), image.Pt(2048, 1280)},
		{"small work area", image.Pt(800, 600), image.Pt(900, 600), image.Pt(1024, 768)},
		{"multi monitor larger window", image.Pt(1280, 720), image.Pt(1900, 1100), image.Pt(2048, 1280)},
		{"unknown work area falls back", image.Point{}, image.Pt(1600, 1000), image.Pt(1792, 1024)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gdiPresizeTarget(tc.work, tc.want); got != tc.wantTarget {
				t.Fatalf("gdiPresizeTarget(%v, %v) = %v, want %v", tc.work, tc.want, got, tc.wantTarget)
			}
		})
	}
}
