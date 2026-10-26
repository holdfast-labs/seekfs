//go:build windows

package main

import (
	"bytes"
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

func TestGenerateContentFixtureAnswers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fixture")
	if _, err := generateFixtureTree(root, 200); err != nil {
		t.Fatal(err)
	}
	set, err := generateContentFixture(root)
	if err != nil {
		t.Fatal(err)
	}
	if set.Files < 3000 {
		t.Fatalf("content files = %d, want >= 3000", set.Files)
	}
	for _, q := range set.Queries {
		for _, rel := range q.Expected {
			path := filepath.Join(root, filepath.FromSlash(rel))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v", q.ID, err)
			}
			snippet := q.Snippet
			if snippet == "" {
				snippet = q.SnippetAlt
			}
			if snippet != "" && !strings.Contains(strings.ToLower(string(data)), strings.ToLower(snippet)) {
				t.Fatalf("%s: %s does not contain %q", q.ID, rel, snippet)
			}
		}
	}
	// Unique needles must appear in exactly the expected file (or no file).
	countNeedle := func(needle string) int {
		n := 0
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr == nil && strings.Contains(strings.ToLower(string(data)), needle) {
				n++
			}
			return nil
		})
		return n
	}
	if got := countNeedle("zzuniqueneedlealpha"); got != 1 {
		t.Fatalf("unique needle count = %d, want 1", got)
	}
	if got := countNeedle("cobalt lantern phrase"); got != 3 {
		t.Fatalf("phrase count = %d, want 3", got)
	}
	if got := countNeedle("binaryneedle"); got != 1 {
		t.Fatalf("binary needle count = %d, want 1", got)
	}
	binaryData, err := os.ReadFile(filepath.Join(root, "content", "binary.bin"))
	if err != nil || !bytes.Contains(binaryData[:min(8, len(binaryData))], []byte{0}) {
		t.Fatalf("binary fixture lacks NUL head: %v", err)
	}
	large, err := os.Stat(filepath.Join(root, "content", "large.txt"))
	if err != nil || large.Size() <= 1<<20 {
		t.Fatalf("large fixture size = %v err=%v, want > 1 MiB", large.Size(), err)
	}
	if _, err := os.Stat(filepath.Join(root, "repo", ".git", "HEAD")); err != nil {
		t.Fatalf("nested git repo missing: %v", err)
	}
}
