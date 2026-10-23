package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func speedPtr(s string) *string { return &s }

func TestSettingsBuildSpeedDefaultAndDecode(t *testing.T) {
	m := newSettingsModel(serviceSettings{})
	if got := m.contentValues("").BuildSpeed; got != contentSpeedBalanced {
		t.Fatalf("default build speed=%q; want balanced", got)
	}
	m = newSettingsModel(serviceSettings{Content: &serviceContent{BuildSpeed: speedPtr(contentSpeedFast)}})
	if got := m.contentValues("").BuildSpeed; got != contentSpeedFast {
		t.Fatalf("decoded build speed=%q; want fast", got)
	}
}

func TestSettingsBuildSpeedPatch(t *testing.T) {
	m := newSettingsModel(serviceSettings{Content: &serviceContent{BuildSpeed: speedPtr(contentSpeedBalanced)}})
	if m.hasChanges() {
		t.Fatal("fresh model reports changes")
	}
	m.setBuildSpeed(contentSpeedGentle)
	if got := m.contentValues("").BuildSpeed; got != contentSpeedGentle {
		t.Fatalf("staged build speed=%q; want gentle", got)
	}
	patch := m.patch()
	if patch == nil || patch.Content == nil || patch.Content.BuildSpeed == nil || *patch.Content.BuildSpeed != contentSpeedGentle {
		t.Fatalf("patch = %+v; want content.build_speed=gentle", patch)
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"build_speed":"gentle"`) {
		t.Fatalf("patch JSON = %s; want build_speed", raw)
	}
	// Switching back to the baseline clears the staged change.
	m.setBuildSpeed(contentSpeedBalanced)
	if m.hasChanges() {
		t.Fatal("setting the baseline value left a change staged")
	}
	// Unknown values are ignored.
	m.setBuildSpeed("warp")
	if got := m.contentValues("").BuildSpeed; got != contentSpeedBalanced {
		t.Fatalf("unknown speed changed draft to %q", got)
	}
}
