package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeContentTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func contentIndexedPaths(t *testing.T, root string) []string {
	t.Helper()
	idx, err := buildContentIndexFromDir(context.Background(), root, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	paths := contentPathsOf(r.search("zqmarker", 0))
	sort.Strings(paths)
	return paths
}

func TestContentGitIgnoreRules(t *testing.T) {
	root := t.TempDir()
	writeContentTree(t, root, map[string]string{
		".git/HEAD":         "ref: refs/heads/main\n",
		".git/info/exclude": "secret.txt\n",
		".gitignore":        "# comment\nOUT/\n*.log\n!keep.log\n/build-cache\n!out/keep.txt\n",
		"sub/.gitignore":    "generated/\n",
	})
	g := newContentGitIgnore([]string{root})
	norm := func(rel string) string { return contentNormPath(filepath.Join(root, filepath.FromSlash(rel))) }
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"out/a.txt", false, true},              // directory pattern, case-folded
		{"out/keep.txt", false, true},           // cannot re-include under an ignored dir
		{"x.log", false, true},                  // floating glob
		{"keep.log", false, false},              // later negation wins
		{"build-cache/x.txt", false, true},      // anchored to the root
		{"sub/build-cache/x.txt", false, false}, // anchored patterns do not float
		{"sub/generated/y.go", false, true},     // nested .gitignore
		{"generated/y.go", false, false},        // nested rules stay in their dir
		{"secret.txt", false, true},             // .git/info/exclude
		{"vendor/a.rs", false, false},           // tracked-style names are not special
		{"build/b.py", false, false},
		{".git/config", false, true},
		{"sub", true, false},
	}
	for _, tc := range cases {
		if got := g.ignored(norm(tc.rel), tc.isDir); got != tc.want {
			t.Errorf("ignored(%s) = %v, want %v", tc.rel, got, tc.want)
		}
	}
	if g.ignored(contentNormPath(filepath.Join(t.TempDir(), "out", "a.txt")), false) {
		t.Error("a path outside every worktree must not be ignored")
	}
}

func TestContentRepoIndexesTrackedVendorAndBuildDirs(t *testing.T) {
	root := t.TempDir()
	writeContentTree(t, root, map[string]string{
		".git/HEAD":          "ref: refs/heads/main\n",
		".gitignore":         "node_modules/\nout/\n",
		"vendor/a.rs":        "zqmarker",
		"build/b.py":         "zqmarker",
		"dist/c.js":          "zqmarker",
		"node_modules/d.js":  "zqmarker",
		"out/e.txt":          "zqmarker",
		"src/f.go":           "zqmarker",
		"nested/.git/HEAD":   "ref: refs/heads/main\n",
		"nested/.gitignore":  "*.tmp\n",
		"nested/g.tmp":       "zqmarker",
		"nested/vendor/h.rs": "zqmarker",
	})
	got := contentIndexedPaths(t, root)
	want := []string{"build/b.py", "dist/c.js", "nested/vendor/h.rs", "src/f.go", "vendor/a.rs"}
	if !sameStringSet(got, want) {
		t.Fatalf("indexed %v\nwant %v", got, want)
	}
}

func TestContentOutsideRepoKeepsNameExcludes(t *testing.T) {
	root := t.TempDir()
	writeContentTree(t, root, map[string]string{
		"vendor/a.rs":       "zqmarker",
		"node_modules/b.js": "zqmarker",
		"src/c.go":          "zqmarker",
	})
	if got := contentIndexedPaths(t, root); !sameStringSet(got, []string{"src/c.go"}) {
		t.Fatalf("indexed %v, want only src/c.go", got)
	}
}

func TestContentIgnoredWalkRootStillIndexes(t *testing.T) {
	root := t.TempDir()
	writeContentTree(t, root, map[string]string{
		".git/HEAD":  "ref: refs/heads/main\n",
		".gitignore": "out/\n",
		"out/a.txt":  "zqmarker",
	})
	if got := contentIndexedPaths(t, filepath.Join(root, "out")); !sameStringSet(got, []string{"a.txt"}) {
		t.Fatalf("indexed %v, want a.txt from the explicitly requested ignored root", got)
	}
}

func TestContentScopeAllowsUsesGitIgnoreInsideRepos(t *testing.T) {
	repo := t.TempDir()
	other := t.TempDir()
	writeContentTree(t, repo, map[string]string{
		".git/HEAD":  "ref: refs/heads/main\n",
		".gitignore": "out/\n",
	})
	scope := contentScope{Mode: contentScopeExplicit, Roots: []string{contentNormPath(other)}, IncludeGit: true, SystemExcludes: true, BudgetBytes: 1 << 20}
	r := scope.resolve(other[:2], []string{contentNormPath(repo)})
	r.attachGitIgnore(nil)
	for _, tc := range []struct {
		path string
		want bool
	}{
		{filepath.Join(repo, "build", "a.go"), true},
		{filepath.Join(repo, "vendor", "b.go"), true},
		{filepath.Join(repo, "out", "c.go"), false},
		{filepath.Join(other, "build", "d.go"), false}, // outside worktrees: name excludes
		{filepath.Join(other, "src", "e.go"), true},
	} {
		if _, _, ok := r.allows(tc.path); ok != tc.want {
			t.Errorf("allows(%s) = %v, want %v", tc.path, ok, tc.want)
		}
	}
}

func TestContentScopeFingerprintTracksIgnoreFiles(t *testing.T) {
	repo := t.TempDir()
	writeContentTree(t, repo, map[string]string{".git/HEAD": "x", ".gitignore": "out/\n"})
	scope := contentScope{Mode: contentScopeExplicit, IncludeGit: true, BudgetBytes: 1 << 20}
	files := map[string][]string{contentNormPath(repo): {filepath.Join(repo, ".gitignore")}}
	r1 := scope.resolve(repo[:2], []string{contentNormPath(repo)})
	r1.attachGitIgnore(files)
	writeContentTree(t, repo, map[string]string{".gitignore": "out/\nmore/\n"})
	r2 := scope.resolve(repo[:2], []string{contentNormPath(repo)})
	r2.attachGitIgnore(files)
	if r1.fingerprint() == r2.fingerprint() {
		t.Fatal("editing .gitignore must change the scope fingerprint")
	}
}
