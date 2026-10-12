//go:build windows

package main

import (
	"strings"
	"testing"
	"time"
)

// TestFormatExplorerTime checks the locale short date+time rendering.
func TestFormatExplorerTime(t *testing.T) {
	got := formatExplorerTime(time.Date(2026, 10, 11, 16, 20, 0, 0, time.Local))
	if got == "" {
		t.Fatal("empty")
	}
	// Locale short date+time for en-US: "10/11/2026 4:20 PM" (no seconds).
	if !strings.Contains(got, "2026") {
		t.Fatalf("date missing in %q", got)
	}
	if strings.Count(got, ":") != 1 {
		t.Fatalf("want short time without seconds in %q", got)
	}
	t.Logf("explorer time = %q", got)
}
