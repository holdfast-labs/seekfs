package main

import (
	"strings"

	"golang.org/x/sys/windows"
)

type settingsVolumeInfo struct {
	Volume     string
	FS         string
	Label      string
	TotalBytes uint64
	FreeBytes  uint64
}

// enumerateSettingsVolumes lists local fixed NTFS volumes. The settings API
// adds any indexed volume that is not covered here, so non-fixed or non-NTFS
// indexes still appear in the response.
func enumerateSettingsVolumes() []settingsVolumeInfo {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	out := make([]settingsVolumeInfo, 0, 8)
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		volume := string(rune('A'+i)) + ":"
		root := volume + `\`
		rootPtr, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		if windows.GetDriveType(rootPtr) != windows.DRIVE_FIXED {
			continue
		}
		var label [windows.MAX_PATH + 1]uint16
		var fs [windows.MAX_PATH + 1]uint16
		if err := windows.GetVolumeInformation(rootPtr, &label[0], uint32(len(label)), nil, nil, nil, &fs[0], uint32(len(fs))); err != nil {
			continue
		}
		filesystem := windows.UTF16ToString(fs[:])
		if !strings.EqualFold(filesystem, "NTFS") {
			continue
		}
		var total, free uint64
		if err := windows.GetDiskFreeSpaceEx(rootPtr, nil, &total, &free); err != nil {
			continue
		}
		out = append(out, settingsVolumeInfo{
			Volume:     volume,
			FS:         filesystem,
			Label:      windows.UTF16ToString(label[:]),
			TotalBytes: total,
			FreeBytes:  free,
		})
	}
	return out
}
