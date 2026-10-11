package main

// Git ignore rules for content scope. Inside a git worktree the content
// builder indexes what git considers part of the tree: everything except
// paths ignored by the worktree's .gitignore files and .git/info/exclude.
// Name-based excludes ("vendor", "build", "node_modules") stay for paths
// outside any worktree, where there are no ignore rules to consult; inside a
// worktree they dropped tracked source (Chromium's top-level build/ and its
// vendored Rust crates).
//
// Matching follows git precedence: the deepest .gitignore wins, later lines
// win within a file, .git/info/exclude ranks below every .gitignore, and a
// path under an ignored directory stays ignored (a negation cannot re-include
// it). Patterns and paths are compared case-folded, matching git's default
// core.ignorecase on Windows. core.excludesFile is not consulted.

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// contentIgnorePattern is one parsed line; line orders precedence within its
// file (later lines win).
type contentIgnorePattern struct {
	line    int
	pattern gitignore.Pattern
}

// contentIgnoreFile holds one ignore file's patterns, split so a path only
// tests the patterns that can match it: a pattern whose first component is a
// literal anchors to that component, everything else floats.
type contentIgnoreFile struct {
	anchored map[string][]contentIgnorePattern
	floating []contentIgnorePattern
}

// contentGitIgnore answers ignore questions for a set of worktrees. Ignore
// files are read lazily, once per directory, and directory decisions are
// memoized, so a build pays one read attempt per directory it touches.
type contentGitIgnore struct {
	repos []string // normalized worktree roots, deepest first

	mu         sync.Mutex
	files      map[string]*contentIgnoreFile // normalized dir -> its .gitignore (nil: none)
	excludes   map[string]*contentIgnoreFile // repo root -> .git/info/exclude (nil: none)
	dirIgnored map[string]bool
}

func newContentGitIgnore(repos []string) *contentGitIgnore {
	roots := normalizeContentPaths(repos)
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	return &contentGitIgnore{
		repos:      roots,
		files:      make(map[string]*contentIgnoreFile),
		excludes:   make(map[string]*contentIgnoreFile),
		dirIgnored: make(map[string]bool),
	}
}

// addRepo registers a worktree discovered during a directory walk. The walk
// is single-goroutine; service scopes fix their worktrees at construction.
func (g *contentGitIgnore) addRepo(root string) {
	root = contentNormPath(root)
	for _, existing := range g.repos {
		if existing == root {
			return
		}
	}
	g.repos = append(g.repos, root)
	sort.Slice(g.repos, func(i, j int) bool { return len(g.repos[i]) > len(g.repos[j]) })
}

// repoFor returns the deepest worktree containing the normalized path p.
func (g *contentGitIgnore) repoFor(p string) (string, bool) {
	if g == nil {
		return "", false
	}
	for _, root := range g.repos {
		if contentPathUnder(p, root) {
			return root, true
		}
	}
	return "", false
}

// ignored reports whether the normalized path p is excluded by git. Paths
// outside every worktree are never ignored here; callers apply their own
// rules there. The .git directory itself is always ignored.
func (g *contentGitIgnore) ignored(p string, isDir bool) bool {
	root, ok := g.repoFor(p)
	if !ok || p == root {
		return false
	}
	comps := contentRelComponents(root, p)
	for _, c := range comps {
		if c == ".git" {
			return true
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(comps) > 1 && g.dirIgnoredLocked(root, comps[:len(comps)-1]) {
		return true
	}
	return g.matchLocked(root, comps, isDir)
}

// invalidate drops cached rules after an ignore file under p changed.
func (g *contentGitIgnore) invalidate() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.files = make(map[string]*contentIgnoreFile)
	g.excludes = make(map[string]*contentIgnoreFile)
	g.dirIgnored = make(map[string]bool)
	g.mu.Unlock()
}

func (g *contentGitIgnore) dirIgnoredLocked(root string, comps []string) bool {
	key := root + `\` + strings.Join(comps, `\`)
	if v, ok := g.dirIgnored[key]; ok {
		return v
	}
	res := len(comps) > 1 && g.dirIgnoredLocked(root, comps[:len(comps)-1])
	if !res {
		res = g.matchLocked(root, comps, true)
	}
	g.dirIgnored[key] = res
	return res
}

// matchLocked applies the ignore files from the path's own directory up to
// the worktree root, then .git/info/exclude; the first matching pattern
// decides.
func (g *contentGitIgnore) matchLocked(root string, comps []string, isDir bool) bool {
	for depth := len(comps) - 1; depth >= 0; depth-- {
		dir := root
		if depth > 0 {
			dir = root + `\` + strings.Join(comps[:depth], `\`)
		}
		file, ok := g.files[dir]
		if !ok {
			file = loadContentIgnoreFile(filepath.Join(dir, ".gitignore"), comps[:depth])
			g.files[dir] = file
		}
		if result := file.match(comps, depth, isDir); result != gitignore.NoMatch {
			return result == gitignore.Exclude
		}
	}
	exclude, ok := g.excludes[root]
	if !ok {
		exclude = loadContentIgnoreFile(filepath.Join(root, ".git", "info", "exclude"), nil)
		g.excludes[root] = exclude
	}
	return exclude.match(comps, 0, isDir) == gitignore.Exclude
}

// match evaluates the candidate patterns for comps, latest line first.
// depth is the number of leading components that belong to the file's
// directory, so comps[depth] is the first component a pattern sees.
func (f *contentIgnoreFile) match(comps []string, depth int, isDir bool) gitignore.MatchResult {
	if f == nil || depth >= len(comps) {
		return gitignore.NoMatch
	}
	anchored := f.anchored[comps[depth]]
	floating := f.floating
	for len(anchored) > 0 || len(floating) > 0 {
		var next contentIgnorePattern
		if len(floating) == 0 || (len(anchored) > 0 && anchored[len(anchored)-1].line > floating[len(floating)-1].line) {
			next, anchored = anchored[len(anchored)-1], anchored[:len(anchored)-1]
		} else {
			next, floating = floating[len(floating)-1], floating[:len(floating)-1]
		}
		if result := next.pattern.Match(comps, isDir); result != gitignore.NoMatch {
			return result
		}
	}
	return gitignore.NoMatch
}

func loadContentIgnoreFile(path string, domain []string) *contentIgnoreFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseContentIgnoreFile(string(data), domain)
}

func parseContentIgnoreFile(data string, domain []string) *contentIgnoreFile {
	f := &contentIgnoreFile{anchored: make(map[string][]contentIgnorePattern)}
	for i, raw := range strings.Split(data, "\n") {
		line := strings.ToLower(strings.TrimSuffix(raw, "\r"))
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := contentIgnorePattern{line: i, pattern: gitignore.ParsePattern(line, domain)}
		if key, ok := contentIgnoreAnchor(line); ok {
			f.anchored[key] = append(f.anchored[key], p)
		} else {
			f.floating = append(f.floating, p)
		}
	}
	if len(f.anchored) == 0 && len(f.floating) == 0 {
		return nil
	}
	return f
}

// contentIgnoreAnchor returns the literal first component of a pattern that
// go-git can only match against that component: patterns containing a slash
// are matched from the start of the path, so "/out/" or "third_party/foo"
// never match a path whose first component differs.
func contentIgnoreAnchor(line string) (string, bool) {
	line = strings.TrimPrefix(line, "!")
	if !strings.HasSuffix(line, `\ `) {
		line = strings.TrimRight(line, " ")
	}
	line = strings.TrimSuffix(line, "/")
	if !strings.Contains(line, "/") {
		return "", false
	}
	for _, comp := range strings.Split(line, "/") {
		if comp == "" {
			continue
		}
		if comp == "**" || strings.ContainsAny(comp, `*?[\`) {
			return "", false
		}
		return comp, true
	}
	return "", false
}

// contentRelComponents splits normalized path p into components below root.
func contentRelComponents(root, p string) []string {
	rel := strings.TrimPrefix(p[len(root):], `\`)
	return strings.FieldsFunc(rel, func(r rune) bool { return r == '\\' || r == '/' })
}

// contentIgnoreDigest fingerprints the ignore files of the given worktrees by
// path, size and modification time, so a sidecar built under different
// ignore rules is rebuilt. ignoreFiles maps a worktree root to the
// .gitignore paths found under it (from the filename index).
func contentIgnoreDigest(repos []string, ignoreFiles map[string][]string) [32]byte {
	h := sha256.New()
	var buf [16]byte
	add := func(path string) {
		h.Write([]byte(contentNormPath(path)))
		if info, err := os.Stat(path); err == nil {
			binary.LittleEndian.PutUint64(buf[:8], uint64(info.Size()))
			binary.LittleEndian.PutUint64(buf[8:], uint64(info.ModTime().UnixNano()))
			h.Write(buf[:])
		}
	}
	roots := normalizeContentPaths(repos)
	sort.Strings(roots)
	for _, root := range roots {
		h.Write([]byte{0})
		files := append([]string(nil), ignoreFiles[root]...)
		sort.Strings(files)
		for _, path := range files {
			add(path)
		}
		add(filepath.Join(root, ".git", "info", "exclude"))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// contentIsIgnoreFile reports whether a changed path can alter ignore rules.
func contentIsIgnoreFile(p string) bool {
	n := contentNormPath(p)
	return strings.HasSuffix(n, `\.gitignore`) || strings.HasSuffix(n, `\.git\info\exclude`)
}
