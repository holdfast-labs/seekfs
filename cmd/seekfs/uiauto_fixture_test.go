//go:build uiautomation

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func makeUIDriveFixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		"a.txt",
		"b.log",
		filepath.Join("sub", "c.md"),
		filepath.Join("sub", "deep", "d.go"),
		filepath.Join("other", "needle.txt"),
	}
	for _, name := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestUIDriveFixtureParse(t *testing.T) {
	root := makeUIDriveFixtureTree(t)
	raw := `{"volumes":[{"volume":"X:","label":"Fixture","fs":"NTFS","total_bytes":1000,"free_bytes":400,"root":"` + jsonEscapePath(root) + `","per_entry_delay_ms":2}]}`
	f, err := parseUIDriveFixture([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Volumes) != 1 {
		t.Fatalf("volumes = %d", len(f.Volumes))
	}
	v := f.Volumes[0]
	if v.Volume != "X:" || v.Label != "Fixture" || v.FS != "NTFS" || v.PerEntryDelayMS != 2 {
		t.Fatalf("volume = %+v", v)
	}
	infos := f.enumerateVolumes()
	if len(infos) != 1 || infos[0].Volume != "X:" || infos[0].TotalBytes != 1000 {
		t.Fatalf("enumeration = %+v", infos)
	}
	if _, err := parseUIDriveFixture([]byte(`{"volumes":[]}`)); err == nil {
		t.Fatal("empty fixture accepted")
	}
	if _, err := parseUIDriveFixture([]byte(`{"volumes":[{"volume":"X:"}]}`)); err == nil {
		t.Fatal("missing root accepted")
	}
}

func TestUIDriveFixtureBuildProducesIndex(t *testing.T) {
	root := makeUIDriveFixtureTree(t)
	f := &uiDriveFixture{Volumes: []uiDriveFixtureVolume{{
		Volume: "X:", Label: "Fixture", FS: "NTFS", TotalBytes: 1000, FreeBytes: 400, Root: root,
	}}}
	idx, err := buildUIDriveFixtureIndex(f, "X:")
	if err != nil {
		t.Fatal(err)
	}
	// root + 3 directories (sub, deep, other) + 5 files
	if got, want := idx.entryCount(), 9; got != want {
		t.Fatalf("entryCount = %d, want %d", got, want)
	}
	if idx.Volume != "X:" || idx.Source != "walk" || !idx.Compact {
		t.Fatalf("index metadata = %+v", idx)
	}
	names := map[string]int{}
	for _, rec := range idx.Records {
		names[rec.Name]++
	}
	for _, want := range []string{".", "sub", "deep", "other", "a.txt", "b.log", "c.md", "d.go", "needle.txt"} {
		if names[want] != 1 {
			t.Fatalf("record %q count = %d in %+v", want, names[want], names)
		}
	}
	if _, err := buildUIDriveFixtureIndex(f, "Z:"); err == nil {
		t.Fatal("unknown volume accepted")
	}
}

func TestUIDriveFixtureCapabilitiesAllowLocalMutate(t *testing.T) {
	previous := uiDriveFixtureData
	uiDriveFixtureData = &uiDriveFixture{}
	defer func() { uiDriveFixtureData = previous }()
	caps := fixtureCapabilities(serviceCapabilities{ReadOnly: true})
	if !caps.Mutate {
		t.Fatal("fixture local caller did not get Mutate")
	}
	remote := fixtureCapabilities(serviceCapabilities{ReadOnly: true, Remote: true})
	if remote.Mutate {
		t.Fatal("fixture remote caller got Mutate")
	}
}

func jsonEscapePath(path string) string {
	out := ""
	for _, r := range path {
		if r == '\\' {
			out += `\\`
			continue
		}
		out += string(r)
	}
	return out
}
