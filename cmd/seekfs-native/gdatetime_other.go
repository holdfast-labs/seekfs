//go:build !windows

package main

import (
	"time"
)

// formatExplorerTime falls back to ISO-ish format off Windows.
func formatExplorerTime(t time.Time) string {
	return t.Format("2006-01-02 15:04")
}
