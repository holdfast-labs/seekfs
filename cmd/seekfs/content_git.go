package main

// Git repository detection for the content scope, driven purely by index
// records (names + modes) so the hot path never touches the filesystem. A
// `.git` entry is either a directory (normal repo, the whole subtree is the
// repo metadata) or a file (worktree/submodule pointer whose `gitdir:` line
// names the real git dir). Nested markers are independent roots.

import (
	"path/filepath"
	"strings"
)

// contentGitMarkerName reports whether a record name is the git marker entry.
// Case-insensitive; exactly ".git" (not ".gitignore", ".gitattributes").
func contentGitMarkerName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), ".git")
}

// contentGitMarker is a .git entry discovered in the index.
type contentGitMarker struct {
	Path string // normalized path of the .git entry (contentNormPath)
	Dir  bool   // true when .git is a directory (subtree) vs a file (pointer)
	Root string // normalized worktree root: contentGitRepoRoot(Path)
}

// contentNewGitMarker builds a marker for a .git entry at path. isDir is true
// when the entry is a directory.
func contentNewGitMarker(path string, isDir bool) contentGitMarker {
	p := contentNormPath(path)
	return contentGitMarker{Path: p, Dir: isDir, Root: contentGitRepoRoot(p)}
}

// contentGitRepoRoot returns the worktree root for a .git entry path: the
// parent directory of the marker. Returns "" when path has no parent.
func contentGitRepoRoot(markerPath string) string {
	p := contentNormPath(markerPath)
	if p == "" {
		return ""
	}
	dir := filepath.Dir(p)
	if dir == "." || dir == p {
		return ""
	}
	return dir
}

// contentGitExcludedPaths returns the paths a build must skip for a repo
// marker. A .git directory excludes its whole subtree (its own path); a .git
// file (worktree/submodule pointer) excludes the file itself. Both are the
// marker Path, so this returns []string{marker.Path} — but keep it a function
// so callers do not depend on that detail, and return nil for a zero marker.
func contentGitExcludedPaths(m contentGitMarker) []string {
	if m.Path == "" {
		return nil
	}
	return []string{m.Path}
}

// contentParseGitDirPointer parses a `.git` *file*'s contents for a
// "gitdir: <path>" line (git worktree/submodule pointer). Returns the raw
// (un-normalized) target and whether a pointer was found. Leading/trailing
// whitespace and a trailing CR are tolerated; the line may be anywhere.
func contentParseGitDirPointer(content []byte) (string, bool) {
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		key, rest, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "gitdir") {
			continue
		}
		target := strings.TrimSpace(rest)
		if target == "" {
			return "", false
		}
		return target, true
	}
	return "", false
}
