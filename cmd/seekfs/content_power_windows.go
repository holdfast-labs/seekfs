//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// systemPowerStatus mirrors SYSTEM_POWER_STATUS (12 bytes): whether mains
// power is online and whether a battery exists and is discharging.
type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	Reserved1           byte
	BatteryLifeTime     int32
	BatteryFullLifeTime int32
}

var (
	modkernel32ForPower      = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemPowerStatus = modkernel32ForPower.NewProc("GetSystemPowerStatus")
)

// contentBatteryProbe reports whether the machine is discharging a battery,
// plus whether the probe itself worked. Unknown (a failed probe) reads as
// mains power for decisions (fail open); health reports the availability.
func contentBatteryProbe() (onBattery, ok bool) {
	var st systemPowerStatus
	r1, _, _ := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&st)))
	if r1 == 0 {
		contentBatteryLastOK.Store(false)
		return false, false
	}
	contentBatteryLastOK.Store(true)
	// BatteryFlag 128 means no system battery; anything else with mains
	// offline is a discharging (or unknown-state) battery.
	return st.ACLineStatus == 0 && st.BatteryFlag != 128, true
}

// contentSystemOnBattery reports whether the machine is discharging a battery.
// Unknown (a failed probe) reads as mains power: the probe must fail open so
// a broken reading cannot throttle every build to one worker forever.
func contentSystemOnBattery() bool {
	onBattery, _ := contentBatteryProbe()
	return onBattery
}
