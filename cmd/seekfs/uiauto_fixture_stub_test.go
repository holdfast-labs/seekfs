//go:build !uiautomation

package main

import "testing"

func TestUIDriveFixtureDisabledInNormalBuild(t *testing.T) {
	if uiDriveFixtureActive() {
		t.Fatal("normal build reports an active UI-drive fixture")
	}
	previous := settingsVolumeEnumeration
	settingsVolumeEnumeration = func() []settingsVolumeInfo {
		return []settingsVolumeInfo{{Volume: "C:", FS: "NTFS"}}
	}
	defer func() { settingsVolumeEnumeration = previous }()
	// A fixture JSON file on disk must not change anything without the tag.
	t.Setenv("SEEKFS_UIDRIVE_FIXTURE", "does-not-matter.json")
	installUIAutoFixtureService()
	if uiDriveFixtureActive() {
		t.Fatal("normal build loaded a UI-drive fixture")
	}
	caps := fixtureCapabilities(serviceCapabilities{ReadOnly: true})
	if caps.Mutate {
		t.Fatal("normal build elevated a local caller")
	}
}
