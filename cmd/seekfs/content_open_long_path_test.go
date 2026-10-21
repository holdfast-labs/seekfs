//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentLongPathExtendsDeepPaths(t *testing.T) {
	short := `C:\a\b.txt`
	if got := contentLongPath(short); got != short {
		t.Fatalf("short path changed: %q", got)
	}
	already := `\\?\C:\a\b.txt`
	if got := contentLongPath(already); got != already {
		t.Fatalf("prefixed path changed: %q", got)
	}
	deep := `C:\` + strings.Repeat(`deep\`, 80) + `a.txt`
	got := contentLongPath(deep)
	if !strings.HasPrefix(got, `\\?\C:\`) {
		t.Fatalf("deep path not extended: %q", got)
	}
	unc := `\\server\share\` + strings.Repeat(`deep\`, 80) + `a.txt`
	if got := contentLongPath(unc); !strings.HasPrefix(got, `\\?\UNC\server\share\`) {
		t.Fatalf("UNC path not extended: %q", got)
	}
}

// A valid text file in a tree past MAX_PATH must open and extract. A raw
// windows.CreateFile call needs the extended-length prefix; go's os helpers
// add it internally, so this is the regression that produced content
// extraction errors for indexed files in deep trees.
func TestContentSlurpOpensLongPath(t *testing.T) {
	root := t.TempDir()
	dir := root
	for i := 0; i < 6; i++ {
		dir = filepath.Join(dir, strings.Repeat("d", 48))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "deep.txt")
	if len(path) <= 260 {
		t.Fatalf("test path is only %d chars; want > 260", len(path))
	}
	const body = "deep needle text"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := contentSlurpNoRecall(path, 1<<20)
	if err != nil {
		t.Fatalf("contentSlurpNoRecall(%d-char path): %v", len(path), err)
	}
	defer s.close()
	if got := string(s.data); got != body {
		t.Fatalf("data = %q; want %q", got, body)
	}
}
