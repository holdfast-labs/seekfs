package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSettingsDecodeProgress(t *testing.T) {
	const doc = `{"ok":true,"settings":{"volumes":[
		{"volume":"X:","indexed":true,"state":"indexing","entries":0,
		 "progress":{"phase":"building","done":1200000,"total":2900000,
		 "started_at":"2026-10-22T10:00:00Z"}}]}}`
	var resp serviceResponse
	if err := json.Unmarshal([]byte(doc), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Settings == nil || len(resp.Settings.Volumes) != 1 {
		t.Fatalf("settings = %+v", resp.Settings)
	}
	p := resp.Settings.Volumes[0].Progress
	if p == nil || p.Phase != "building" || p.Done != 1200000 || p.Total != 2900000 || p.StartedAt == "" {
		t.Fatalf("progress = %+v", p)
	}
	m := newSettingsModel(*resp.Settings)
	if v := m.drive("X:"); v == nil || v.Progress == nil || v.Progress.Done != 1200000 {
		t.Fatalf("model drive = %+v", v)
	}
}

func TestMergeServerStateKeepsStagedDraftEdits(t *testing.T) {
	base := serviceSettings{
		Volumes: []serviceVolume{{Volume: "C:", Indexed: boolPtr(true), State: "ready", Entries: 10}},
		Content: &serviceContent{Mode: strPtr(contentModeAuto)},
	}
	m := newSettingsModel(base)
	m.setDriveIndexed("C:", false)
	m.setContentMode("", contentModeExplicit)

	next := serviceSettings{
		Volumes: []serviceVolume{{Volume: "C:", Indexed: boolPtr(true), State: "ready", Entries: 99}},
		Content: &serviceContent{Mode: strPtr(contentModeAuto)},
	}
	m.mergeServerState(next)

	d := m.drive("C:")
	if d == nil || d.Indexed {
		t.Fatalf("staged off edit was lost: %+v", d)
	}
	if d.State != "ready" || d.Entries != 99 {
		t.Fatalf("server state not merged: %+v", d)
	}
	if m.draft.Content.Mode != contentModeExplicit {
		t.Fatalf("staged content edit was lost: %q", m.draft.Content.Mode)
	}
	patch := m.patch()
	if patch == nil || len(patch.Volumes) != 1 || patch.Volumes[0].Indexed == nil || *patch.Volumes[0].Indexed {
		t.Fatalf("patch after merge = %+v", patch)
	}
	if patch.Content == nil || patch.Content.Mode == nil || *patch.Content.Mode != contentModeExplicit {
		t.Fatalf("content patch after merge = %+v", patch.Content)
	}
}

func TestIndexingProgressLabelFormatting(t *testing.T) {
	got := formatIndexingProgress(&serviceVolumeProgress{Done: 1200000, Total: 2900000})
	if !strings.Contains(got, "41%") || !strings.Contains(got, "1.2M of 2.9M files") {
		t.Fatalf("label = %q", got)
	}
	unknown := formatIndexingProgress(&serviceVolumeProgress{Done: 4200, Total: 0})
	if !strings.Contains(unknown, "4.2K files") || strings.Contains(unknown, "%") {
		t.Fatalf("unknown-total label = %q", unknown)
	}
	if status := formatIndexingStatus("C:", &serviceVolumeProgress{Done: 42, Total: 100}); status != "Indexing C: 42%" {
		t.Fatalf("status = %q", status)
	}
	if status := formatIndexingStatus("C:", &serviceVolumeProgress{Done: 42, Total: 0}); status != "Indexing C:" {
		t.Fatalf("status without total = %q", status)
	}
}

func TestFirstRunHealthIsNeutral(t *testing.T) {
	text, tone := healthStatusText(serviceHealthEmpty, "No drives indexed yet", 0)
	if tone != healthToneMuted || text != "No drives indexed yet" {
		t.Fatalf("empty health = %q tone %d", text, tone)
	}
	_, warn := healthStatusText(serviceHealthDegraded, "loading", 0)
	if warn != healthToneWarning {
		t.Fatalf("degraded tone = %d", warn)
	}
	_, bad := healthStatusText(serviceHealthError, "boom", 0)
	if bad != healthToneError {
		t.Fatalf("error tone = %d", bad)
	}
}
