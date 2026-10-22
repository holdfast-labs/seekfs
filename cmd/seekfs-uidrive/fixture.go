//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// fixture.go: deterministic fixture tree and the fixture JSON the service
// test mode reads. File names carry the scenario query tokens.

type fixturePlan struct {
	Root            string
	Files           int
	Dirs            int
	PerEntryDelayMS int
}

type uiDriveFixture struct {
	Volumes []uiDriveFixtureVolume `json:"volumes"`
}

type uiDriveFixtureVolume struct {
	Volume          string `json:"volume"`
	Label           string `json:"label,omitempty"`
	FS              string `json:"fs,omitempty"`
	TotalBytes      uint64 `json:"total_bytes,omitempty"`
	FreeBytes       uint64 `json:"free_bytes,omitempty"`
	Root            string `json:"root"`
	PerEntryDelayMS int    `json:"per_entry_delay_ms,omitempty"`
}

// generateFixtureTree writes files small files in nested directories. It
// returns the plan used by the fixture JSON.
func generateFixtureTree(root string, files int) (fixturePlan, error) {
	plan := fixturePlan{Root: root, PerEntryDelayMS: 1}
	if files < 0 {
		files = 0
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return plan, err
	}
	dirs := files / 200
	if dirs < 1 {
		dirs = 1
	}
	if dirs > 100 {
		dirs = 100
	}
	for d := 0; d < dirs; d++ {
		dir := filepath.Join(root, fmt.Sprintf("group-%03d", d), fmt.Sprintf("nested-%02d", d%7))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return plan, err
		}
	}
	perDir := files
	if dirs > 0 {
		perDir = (files + dirs - 1) / dirs
	}
	written := 0
	for d := 0; d < dirs && written < files; d++ {
		dir := filepath.Join(root, fmt.Sprintf("group-%03d", d), fmt.Sprintf("nested-%02d", d%7))
		for i := 0; i < perDir && written < files; i++ {
			global := d*perDir + i
			var name string
			switch global % 3 {
			case 0:
				name = fmt.Sprintf("needle_%05d.txt", global)
			case 1:
				name = fmt.Sprintf("alpha_report_%05d.log", global)
			default:
				name = fmt.Sprintf("doc_%05d.md", global)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture\n"), 0o600); err != nil {
				return plan, err
			}
			plan.Files++
			written++
		}
	}
	plan.Dirs = dirs*2 + 1
	return plan, nil
}
