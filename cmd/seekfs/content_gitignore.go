package main

// Git ignore rules for content scope. Inside a git worktree the content
// builder indexes what git considers part of the tree: everything except
// paths ignored by the worktree's .gitignore files, .git/info/exclude, and
// git's global excludes file (core.excludesFile). Name-based excludes
// ("vendor", "build", "node_modules") stay for paths outside any worktree,
// where there are no ignore rules to consult; inside a worktree they dropped
// tracked source (Chromium's top-level build/ and its vendored Rust crates).
//
// Matching follows git precedence: the deepest .gitignore wins, later lines
// win within a file, .git/info/exclude ranks below every .gitignore, and the
// global excludes file ranks below .git/info/exclude. A path under an ignored
// directory stays ignored (a negation cannot re-include it). Patterns and
// paths are compared case-folded, matching git's default core.ignorecase on
// Windows.

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
// memoized, so a build pays one read attempt per directory it touches. The
// global excludes path is resolved per repo (also lazily, then snapshotted)
// because it lives outside the volume: the digest hashes it and the drain
// watches its stat for edits the USN stream can never report.
type contentGitIgnore struct {
	repos []string // normalized worktree roots, deepest first

	mu          sync.Mutex
	files       map[string]*contentIgnoreFile // normalized dir -> its .gitignore (nil: none)
	excludes    map[string]*contentIgnoreFile // repo root -> .git/info/exclude (nil: none)
	global      map[string]*contentIgnoreFile // repo root -> global excludes (nil: none)
	globalPaths map[string]string             // repo root -> resolved global excludes path ("": none)
	globalStats map[string]contentIgnoreStat  // repo root -> stat snapshot at resolve time
	dirIgnored  map[string]bool
}

// contentIgnoreStat is the size/mtime snapshot of an ignore file outside the
// volume, for edit detection (the USN stream never reports those files).
type contentIgnoreStat struct {
	size  int64
	mtime int64
	ok    bool
}

// contentIgnoreFileStat snapshots path's size and mtime; a missing file reads
// as the zero stat, so creation and deletion are changes too.
func contentIgnoreFileStat(path string) contentIgnoreStat {
	info, err := os.Stat(path)
	if err != nil {
		return contentIgnoreStat{}
	}
	return contentIgnoreStat{size: info.Size(), mtime: info.ModTime().UnixNano(), ok: true}
}

func newContentGitIgnore(repos []string) *contentGitIgnore {
	roots := normalizeContentPaths(repos)
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	return &contentGitIgnore{
		repos:       roots,
		files:       make(map[string]*contentIgnoreFile),
		excludes:    make(map[string]*contentIgnoreFile),
		global:      make(map[string]*contentIgnoreFile),
		globalPaths: make(map[string]string),
		globalStats: make(map[string]contentIgnoreStat),
		dirIgnored:  make(map[string]bool),
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

// invalidate drops cached rules after an ignore file changed, including the
// global excludes snapshots (re-resolved lazily, so the next digest and the
// next change check see the new rules).
func (g *contentGitIgnore) invalidate() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.files = make(map[string]*contentIgnoreFile)
	g.excludes = make(map[string]*contentIgnoreFile)
	g.global = make(map[string]*contentIgnoreFile)
	g.globalPaths = make(map[string]string)
	g.globalStats = make(map[string]contentIgnoreStat)
	g.dirIgnored = make(map[string]bool)
	g.mu.Unlock()
}

// resolveGlobals resolves every repo's global excludes path (snapshotting
// stats), for the scope digest. Caller must not hold g.mu (it locks).
func (g *contentGitIgnore) resolveGlobals() map[string]string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]string, len(g.repos))
	for _, root := range g.repos {
		if p := g.globalForLocked(root); p != "" {
			out[root] = p
		}
	}
	return out
}

// globalChanged reports whether any repo's global excludes file was created,
// deleted, or modified since its path was resolved. The file lives outside
// the indexed volume, so the USN stream never reports it; the drain loop
// polls this instead. Caller must not hold g.mu (it locks).
func (g *contentGitIgnore) globalChanged() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, root := range g.repos {
		p := g.globalForLocked(root)
		if p == "" {
			continue
		}
		if contentIgnoreFileStat(p) != g.globalStats[root] {
			return true
		}
	}
	return false
}

// globalForLocked resolves (once) and returns the global excludes path for a
// repo root, snapshotting its stat for the digest and change detection.
// Caller holds g.mu.
func (g *contentGitIgnore) globalForLocked(root string) string {
	if p, ok := g.globalPaths[root]; ok {
		return p
	}
	p := contentGlobalExcludesFile(root)
	g.globalPaths[root] = p
	if p != "" {
		g.globalStats[root] = contentIgnoreFileStat(p)
	}
	return p
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
// the worktree root, then .git/info/exclude, then git's global excludes file;
// the first matching pattern decides.
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
	if result := exclude.match(comps, 0, isDir); result != gitignore.NoMatch {
		return result == gitignore.Exclude
	}
	// Git's global excludes file (core.excludesFile) ranks below
	// .git/info/exclude. Like exclude, its patterns are root-relative.
	if path := g.globalForLocked(root); path != "" {
		global, ok := g.global[root]
		if !ok {
			global = loadContentIgnoreFile(path, nil)
			g.global[root] = global
		}
		return global.match(comps, 0, isDir) == gitignore.Exclude
	}
	return false
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
// .gitignore paths found under it (from the filename index); globalFiles maps
// a worktree root to its resolved global excludes path, so editing (or
// creating) that file changes the digest and triggers a rebuild.
func contentIgnoreDigest(repos []string, ignoreFiles map[string][]string, globalFiles map[string]string) [32]byte {
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
		if gp := globalFiles[root]; gp != "" {
			add(gp)
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// contentGlobalExcludesFile resolves git's global excludes file for the repo
// at root (a normalized worktree root): core.excludesFile from the repo's own
// .git/config wins, then the user's global config (~/.gitconfig over
// $XDG_CONFIG_HOME/git/config), then the default $XDG_CONFIG_HOME/git/ignore
// (or %USERPROFILE%\.config\git\ignore). It returns "" only when no home is
// known and no config names a file. A named-but-missing file is still
// returned: the digest hashes its path, so creating it later rebuilds, and
// matching treats it as empty until then.
func contentGlobalExcludesFile(repoRoot string) string {
	if cfg := contentRepoGitConfig(repoRoot); cfg != "" {
		if v, ok := contentGitConfigFileValue(cfg, "core", "excludesfile"); ok {
			if p := contentExpandExcludesPath(v); p != "" {
				return p
			}
		}
	}
	home, _ := os.UserHomeDir()
	var v string
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		if gv, ok := contentGitConfigFileValue(filepath.Join(xdg, "git", "config"), "core", "excludesfile"); ok {
			v = gv
		}
	}
	if home != "" {
		if gv, ok := contentGitConfigFileValue(filepath.Join(home, ".gitconfig"), "core", "excludesfile"); ok {
			v = gv
		}
	}
	if strings.TrimSpace(v) != "" {
		if p := contentExpandExcludesPath(v); p != "" {
			return p
		}
	}
	return contentDefaultGlobalExcludesFile(home)
}

// contentDefaultGlobalExcludesFile is git's default global ignore path:
// $XDG_CONFIG_HOME/git/ignore, else %USERPROFILE%\.config\git\ignore. "" when
// no home is known.
func contentDefaultGlobalExcludesFile(home string) string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "git", "ignore")
	}
	if home == "" {
		if h, err := os.UserHomeDir(); err != nil || h == "" {
			return ""
		} else {
			home = h
		}
	}
	return filepath.Join(home, ".config", "git", "ignore")
}

// contentRepoGitConfig returns the repo's git config path: <root>/.git/config
// for a normal repo, or the config beside a worktree/submodule gitdir pointer
// (.git file with a "gitdir:" line, resolved against the repo root).
func contentRepoGitConfig(repoRoot string) string {
	dotGit := filepath.Join(repoRoot, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return filepath.Join(dotGit, "config")
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	target, ok := contentParseGitDirPointer(data)
	if !ok {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(repoRoot, target)
	}
	return filepath.Join(filepath.Clean(target), "config")
}

// contentGitConfigFileValue reads one key from a git config file. Missing or
// unreadable files read as absent.
func contentGitConfigFileValue(path, section, key string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return contentGitConfigValue(string(data), section, key)
}

// contentGitConfigValue parses one case-insensitive key from the named
// [section] of git config text (the last occurrence wins, as in git).
// Sections with subsections ([include "x"]) never match a bare section name.
// Values honor double quotes and backslash escapes; '#' and ';' comments are
// stripped outside quotes. [include] chains are not followed: only the named
// file is read.
func contentGitConfigValue(data, section, key string) (string, bool) {
	inTarget := false
	var found string
	ok := false
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				inTarget = false
				continue
			}
			name := strings.TrimSpace(line[1:end])
			if i := strings.IndexAny(name, " \t"); i >= 0 {
				// A subsection follows (e.g. [include "path"]): it never
				// matches the bare section name.
				if strings.Trim(name[i+1:], " \t\"'") != "" {
					inTarget = false
					continue
				}
				name = name[:i]
			}
			inTarget = strings.EqualFold(name, section)
			continue
		}
		if !inTarget {
			continue
		}
		name, value, hasEq := strings.Cut(line, "=")
		if !hasEq || !strings.EqualFold(strings.TrimSpace(name), key) {
			continue
		}
		found, ok = contentGitConfigUnquote(contentGitConfigUncomment(value)), true
	}
	return found, ok
}

// contentGitConfigUncomment strips a trailing '#' or ';' comment: the marker
// must start the value or follow whitespace, and markers inside double quotes
// are literal.
func contentGitConfigUncomment(v string) string {
	inQuotes := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '\\' && inQuotes && i+1 < len(v) {
			i++
			continue
		}
		if c == '"' {
			inQuotes = !inQuotes
			continue
		}
		if !inQuotes && (c == '#' || c == ';') && (i == 0 || v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i])
		}
	}
	return strings.TrimSpace(v)
}

// contentGitConfigUnquote removes one pair of double quotes and collapses
// backslash escapes the way git does (\" and \\ stay literal, \n is a newline,
// any other \x is x).
func contentGitConfigUnquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	var b strings.Builder
	inner := v[1 : len(v)-1]
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			i++
			switch inner[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			default:
				b.WriteByte(inner[i])
			}
			continue
		}
		b.WriteByte(inner[i])
	}
	return b.String()
}

// contentExpandExcludesPath expands a core.excludesFile value: a leading ~/
// home, and a relative path against the home directory (git leaves relative
// paths CWD-relative, which is meaningless for the service; home is the least
// surprising base and matches the ~/ convention). "" when nothing sane
// applies.
func contentExpandExcludesPath(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(v, "~") && (len(v) == 1 || v[1] == '/' || v[1] == '\\') {
		if home == "" {
			return ""
		}
		v = filepath.Join(home, v[1:])
	} else if !filepath.IsAbs(v) {
		if home == "" {
			return ""
		}
		v = filepath.Join(home, v)
	}
	return filepath.Clean(v)
}

// contentIsIgnoreFile reports whether a changed path can alter ignore rules.
func contentIsIgnoreFile(p string) bool {
	n := contentNormPath(p)
	return strings.HasSuffix(n, `\.gitignore`) || strings.HasSuffix(n, `\.git\info\exclude`)
}
