//go:build !windows

package main

// contentSystemOnBattery is a no-op off Windows: battery-aware throttling is
// a Windows service concern.
func contentSystemOnBattery() bool { return false }
