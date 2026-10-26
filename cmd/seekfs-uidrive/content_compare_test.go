//go:build windows

package main

import (
	"path/filepath"
	"testing"
)

func TestCompareResultPaths(t *testing.T) {
	root := `C:\fixture`
	exp := expectedResultPaths(root, []string{"content/a.txt", "content/b.txt"})
	got := []string{filepath.Join(root, "CONTENT", "a.txt"), filepath.Join(root, "content", "c.txt")}
	cmp := compareResultPaths(exp, got, root)
	if cmp.Pass || len(cmp.Missing) != 1 || len(cmp.Extra) != 1 {
		t.Fatalf("comparison = %+v, want one missing and one extra", cmp)
	}
	if cmp.Missing[0] != filepath.Join(root, "content", "b.txt") {
		t.Fatalf("missing = %v", cmp.Missing)
	}
	if !compareResultPaths(exp, exp, root).Pass {
		t.Fatal("identical sets must pass")
	}
}

func TestSnippetContains(t *testing.T) {
	if !snippetContains("a NEEDLE here", "needle", false) {
		t.Fatal("case-insensitive snippet check failed")
	}
	if snippetContains("a NEEDLE here", "needle", true) {
		t.Fatal("case-sensitive snippet check matched wrong case")
	}
	if !snippetContains("anything", "", true) {
		t.Fatal("empty needle must pass")
	}
}
