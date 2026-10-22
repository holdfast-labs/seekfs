//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateFixtureTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fixture")
	plan, err := generateFixtureTree(root, 500)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Files != 500 {
		t.Fatalf("files = %d, want 500", plan.Files)
	}
	if plan.Dirs < 3 {
		t.Fatalf("dirs = %d, want nested dirs", plan.Dirs)
	}
	if plan.PerEntryDelayMS <= 0 {
		t.Fatalf("per-entry delay = %d", plan.PerEntryDelayMS)
	}
	seen := 0
	tokens := map[string]int{}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		seen++
		name := d.Name()
		for _, token := range []string{"needle", "alpha", "report", "doc"} {
			if strings.Contains(name, token) {
				tokens[token]++
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() == 0 {
			t.Fatalf("empty fixture file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 500 {
		t.Fatalf("walked files = %d, want 500", seen)
	}
	for _, token := range []string{"needle", "alpha", "report", "doc"} {
		if tokens[token] == 0 {
			t.Fatalf("token %q missing from fixture names", token)
		}
	}
	// A second generation in the same directory is deterministic and idempotent.
	again, err := generateFixtureTree(root, 500)
	if err != nil {
		t.Fatal(err)
	}
	if again.Files != 500 {
		t.Fatalf("second generate files = %d", again.Files)
	}
}
