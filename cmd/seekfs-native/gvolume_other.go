//go:build !windows

package main

// volumeLabel falls back to the bare volume on non-Windows.
func volumeLabel(root string) string { return root }
