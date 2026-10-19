package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// settingsDocJSON is the exact response shape from docs/SETTINGS_API.md.
const settingsDocJSON = `{
  "ok": true,
  "settings": {
    "can_write": false,
    "volumes": [
      {"volume": "C:", "indexed": true, "fs": "NTFS", "label": "OS",
       "total_bytes": 1000204886016, "free_bytes": 120000000000,
       "entries": 1460000, "state": "ready"},
      {"volume": "F:", "indexed": false, "fs": "NTFS", "label": "Data",
       "total_bytes": 2000398934016, "free_bytes": 900000000000,
       "entries": 0, "state": "not_indexed"}
    ],
    "content": {
      "enabled": true,
      "mode": "auto",
      "roots": ["E:\\work"],
      "exclude": ["E:\\work\\scratch"],
      "exts": [],
      "git": true,
      "system_excludes": true,
      "budget_bytes": 0,
      "volumes": {"E:": {"mode": "explicit", "roots": ["E:\\src"]}}
    }
  }
}`

func TestSettingsProtocolDecodeDoc(t *testing.T) {
	var resp serviceResponse
	if err := json.Unmarshal([]byte(settingsDocJSON), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Settings == nil {
		t.Fatalf("response = %+v", resp)
	}
	s := resp.Settings
	if s.CanWrite {
		t.Fatal("can_write must decode as false")
	}
	if len(s.Volumes) != 2 {
		t.Fatalf("volumes = %d", len(s.Volumes))
	}
	c := s.Volumes[0]
	if c.Volume != "C:" || c.Indexed == nil || !*c.Indexed || c.FS != "NTFS" ||
		c.Label != "OS" || c.TotalBytes != 1000204886016 || c.FreeBytes != 120000000000 ||
		c.Entries != 1460000 || c.State != "ready" {
		t.Fatalf("C: decoded as %+v", c)
	}
	f := s.Volumes[1]
	if f.Indexed == nil || *f.Indexed || f.State != "not_indexed" {
		t.Fatalf("F: decoded as %+v", f)
	}
	if s.Content == nil {
		t.Fatal("content missing")
	}
	content := s.Content
	if content.Enabled == nil || !*content.Enabled {
		t.Fatal("content.enabled missing")
	}
	if content.Mode == nil || *content.Mode != "auto" {
		t.Fatalf("mode = %v", content.Mode)
	}
	if content.Roots == nil || len(*content.Roots) != 1 || (*content.Roots)[0] != `E:\work` {
		t.Fatalf("roots = %v", content.Roots)
	}
	if content.Exts == nil || len(*content.Exts) != 0 {
		t.Fatalf("exts = %v", content.Exts)
	}
	if content.Git == nil || !*content.Git || content.SystemExcludes == nil || !*content.SystemExcludes {
		t.Fatal("content toggles missing")
	}
	ov, ok := content.Volumes["E:"]
	if !ok || ov.Mode == nil || *ov.Mode != "explicit" || ov.Roots == nil || len(*ov.Roots) != 1 {
		t.Fatalf("E: override = %+v", ov)
	}
}

func TestSettingsPatchEncodesOnlyChangedFields(t *testing.T) {
	var resp serviceResponse
	if err := json.Unmarshal([]byte(settingsDocJSON), &resp); err != nil {
		t.Fatal(err)
	}
	m := newSettingsModel(*resp.Settings)
	if m.patch() != nil {
		t.Fatal("fresh model must produce no patch")
	}

	m.setContentMode("", contentModeExplicit)
	data, err := json.Marshal(m.patch())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"content":{"mode":"explicit"}}`
	if string(data) != want {
		t.Fatalf("patch = %s, want %s", data, want)
	}

	m2 := newSettingsModel(*resp.Settings)
	m2.setDriveIndexed("F:", true)
	data, err = json.Marshal(m2.patch())
	if err != nil {
		t.Fatal(err)
	}
	want = `{"volumes":[{"volume":"F:","indexed":true}]}`
	if string(data) != want {
		t.Fatalf("volume patch = %s, want %s", data, want)
	}

	m3 := newSettingsModel(*resp.Settings)
	m3.setBudget("", 2*gbBytes)
	data, err = json.Marshal(m3.patch())
	if err != nil {
		t.Fatal(err)
	}
	want = `{"content":{"budget_bytes":2147483648}}`
	if string(data) != want {
		t.Fatalf("budget patch = %s, want %s", data, want)
	}
}

func TestSettingsStagingUndo(t *testing.T) {
	m := newSettingsModel(serviceSettings{
		Volumes: []serviceVolume{{Volume: "C:", Indexed: boolPtr(true)}},
		Content: &serviceContent{Enabled: boolPtr(true), Mode: strPtr(contentModeAuto)},
	})
	if m.hasChanges() {
		t.Fatal("fresh model has changes")
	}
	m.setDriveIndexed("C:", false)
	if !m.hasChanges() {
		t.Fatal("indexed toggle did not stage")
	}
	if !m.undo() {
		t.Fatal("undo returned false")
	}
	if m.hasChanges() {
		t.Fatal("undo did not restore the snapshot")
	}
	if !m.drive("C:").Indexed {
		t.Fatal("undo did not restore indexed")
	}
	if m.undo() {
		t.Fatal("undo must stop at the base snapshot")
	}
}

func TestSettingsPerDriveOverride(t *testing.T) {
	m := newSettingsModel(serviceSettings{
		Volumes: []serviceVolume{{Volume: "E:", Indexed: boolPtr(true)}},
		Content: &serviceContent{
			Enabled: boolPtr(true),
			Mode:    strPtr(contentModeAuto),
			Roots:   &[]string{`E:\work`},
		},
	})
	if m.hasOverride("E:") {
		t.Fatal("no override expected")
	}
	m.ensureOverride("E:")
	m.setContentMode("E:", contentModeExplicit)
	m.addRoot("E:", `E:\src`)
	if !m.hasOverride("E:") {
		t.Fatal("override not created")
	}
	vals := m.contentValues("E:")
	if vals.Mode != contentModeExplicit || len(vals.Roots) != 2 ||
		vals.Roots[0] != `E:\work` || vals.Roots[1] != `E:\src` {
		t.Fatalf("values = %+v", vals)
	}
	// Global values stay untouched.
	if m.contentValues("").Mode != contentModeAuto {
		t.Fatal("global mode changed")
	}
	p := m.patch()
	if p == nil || p.Content == nil || p.Content.Volumes == nil {
		t.Fatal("override missing from patch")
	}
	ov := p.Content.Volumes["E:"]
	if ov.Mode == nil || *ov.Mode != contentModeExplicit {
		t.Fatalf("override mode = %v", ov.Mode)
	}
	if ov.Roots == nil || len(*ov.Roots) != 2 || (*ov.Roots)[1] != `E:\src` {
		t.Fatalf("override roots = %v", ov.Roots)
	}
	m.removeOverride("E:")
	if m.hasOverride("E:") {
		t.Fatal("override not removed")
	}
	if m.undo(); !m.hasOverride("E:") {
		t.Fatal("undo must restore the removed override")
	}
}

func TestSettingsBudgetParsing(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{"", 0, false},
		{"No limit", 0, false},
		{"2", 2 * gbBytes, false},
		{"2.5", 5 * gbBytes / 2, false},
		{"3GB", 3 * gbBytes, false},
		{" 1.25 g ", 5 * gbBytes / 4, false},
		{"0", 0, false},
		{"-1", 0, true},
		{"abc", 0, true},
		{"1e300", 0, true},
	}
	for _, c := range cases {
		got, err := parseBudgetGB(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("parseBudgetGB(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBudgetGB(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseBudgetGB(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	if formatBudgetGB(2*gbBytes) != "2" {
		t.Fatalf("formatBudgetGB(2GB) = %q", formatBudgetGB(2*gbBytes))
	}
	if formatBudgetGB(0) != "" {
		t.Fatalf("formatBudgetGB(0) = %q", formatBudgetGB(0))
	}
}

func TestSettingsExtensionParsing(t *testing.T) {
	got, err := parseExtList(" .GO, txt;md  go ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"go", "txt", "md"}
	if len(got) != len(want) {
		t.Fatalf("exts = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("exts = %q, want %q", got, want)
		}
	}
	if _, err := parseExtList("go, bad/ext"); err == nil {
		t.Fatal("invalid extension must report an error")
	}
	if exts, err := parseExtList(""); err != nil || len(exts) != 0 {
		t.Fatalf("empty list = %q, %v", exts, err)
	}
	if formatExtList([]string{"go", "md"}) != "go, md" {
		t.Fatalf("formatExtList = %q", formatExtList([]string{"go", "md"}))
	}
}

func TestSettingsElevationDecision(t *testing.T) {
	refusal := &serviceResponse{OK: false, Message: "settings-set requires an elevated caller"}
	if !elevationDecision(false, refusal) {
		t.Fatal("can_write=false must select the runas path")
	}
	if !elevationDecision(true, refusal) {
		t.Fatal("the documented refusal message must select the runas path")
	}
	if elevationDecision(true, &serviceResponse{OK: true}) {
		t.Fatal("success must not elevate")
	}
	if elevationDecision(true, &serviceResponse{OK: false, Message: "content.roots: path not found"}) {
		t.Fatal("validation failure must not elevate")
	}
	if elevationDecision(true, nil) {
		t.Fatal("nil response must not elevate")
	}
}

func TestSettingsSizeFormatting(t *testing.T) {
	if got := formatBytesCompact(0); got != "0 B" {
		t.Fatalf("0 = %q", got)
	}
	if got := formatBytesCompact(1024); got != "1.0 KB" {
		t.Fatalf("1024 = %q", got)
	}
	if got := formatBytesCompact(1 << 30); got != "1.0 GB" {
		t.Fatalf("1GB = %q", got)
	}
	if got := formatBytesCompact(500 * gbBytes); got != "500 GB" {
		t.Fatalf("500GB = %q", got)
	}
	if !strings.Contains(formatBytesCompact(3<<30), "GB") {
		t.Fatal("missing unit")
	}
}

func boolPtr(v bool) *bool    { return &v }
func strPtr(v string) *string { return &v }
