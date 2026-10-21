//go:build !windows

package main

// contentSystemOnBattery is a no-op off Windows: battery-aware throttling is
// a Windows service concern.
func contentSystemOnBattery() bool { return false }

// contentBatteryProbe is a no-op off Windows (never available).
func contentBatteryProbe() (bool, bool) { return false, false }
