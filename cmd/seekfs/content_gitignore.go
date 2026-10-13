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
// directory stays ignored (a negation cannot re-include it). Patterns match
// case-sensitively, like ripgrep and like git for every file that exists:
// git's core.ignorecase only relaxes matching for paths with no on-disk entry
// (verified: build/.gitignore's LASTCHANGE* ignores a hypothetical
// LASTCHANGE.foo but not the tracked lastchange.gni), and the index only ever
// holds files that exist. Folding would silently drop tracked source whenever
// an ignore pattern differs in case (Chromium's lastchange.gni/.py).

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// contentIgnoreAffix is one prefix/suffix-shaped floating pattern
// (`rest*` / `*rest` with a literal rest) for the file-only dispatch.
type contentIgnoreAffix struct {
	prefix bool // HasPrefix (rest*) vs HasSuffix (*rest)
	lit    string
	pat    contentIgnorePattern
}

// contentIgnoreFile holds one ignore file's patterns, split so a path only
// tests the patterns that can match it: a pattern whose first component is a
// literal anchors to that component, everything else floats. The file-only
// dispatch (fileExact/fileAffix/fileRest) re-indexes a conservative subset of
// the floating patterns by shape so a file tests only provable candidates;
// directory matching keeps the exact floating list untouched.
type contentIgnoreFile struct {
	anchored map[string][]contentIgnorePattern
	floating []contentIgnorePattern
	// fileExact maps a magic-free, non-negated, non-dirOnly pattern to
	// itself; fileAffix holds the prefix/suffix shapes (same restrictions)
	// in line order; fileRest holds every other floating pattern in line
	// order (globs, other magic, negations, dirOnly). Only file matching
	// consults the buckets (see matchFile); directories use floating whole.
	fileExact map[string][]contentIgnorePattern
	fileAffix []contentIgnoreAffix
	fileRest  []contentIgnorePattern
}

// contentGitIgnore answers ignore questions for a set of worktrees. Ignore
// files are read lazily, once per directory, and directory decisions are
// memoized, so a build pays one read attempt per directory it touches. The
// global excludes path is resolved per repo (also lazily, then snapshotted)
// because it lives outside the volume: the digest hashes it and the drain
// watches its stat for edits the USN stream can never report.
type contentGitIgnore struct {
	repos []string // normalized worktree roots, deepest first
	// repoPrefix parallels repos with separator-terminated forms, so the
	// per-file worktree lookup is one HasPrefix per repo with no cleaning.
	repoPrefix []string

	mu          sync.Mutex
	files       map[string]*contentIgnoreFile // normalized dir -> its .gitignore (nil: none)
	excludes    map[string]*contentIgnoreFile // repo root -> .git/info/exclude (nil: none)
	global      map[string]*contentIgnoreFile // repo root -> global excludes (nil: none)
	globalPaths map[string]string             // repo root -> resolved global excludes path ("": none)
	globalStats map[string]contentIgnoreStat  // repo root -> stat snapshot at resolve time
	dirIgnored  map[string]bool
	// shared, when non-nil, is the parse cache of one scan, common to its
	// workers: each distinct ignore file is read and parsed once per scan
	// instead of once per worker, and misses cache as nil so absent files
	// cost no repeat opens. Parsed files are immutable, so sharing is safe;
	// the per-worker dirIgnored memo stays private. Callers without a scan
	// (estimate, drain, offline walk) leave it nil and keep the private maps.
	shared *contentIgnoreSharedCache
}

// contentIgnoreSharedCache is one scan's shared ignore-file store. The maps
// mirror contentGitIgnore's lazy ones; loaded counts read attempts (hits are
// free map lookups).
type contentIgnoreSharedCache struct {
	mu          sync.Mutex
	files       map[string]*contentIgnoreFile
	excludes    map[string]*contentIgnoreFile
	global      map[string]*contentIgnoreFile
	globalPaths map[string]string
	globalStats map[string]contentIgnoreStat
	loads       int64
	hits        int64
}

// newContentIgnoreSharedCache makes the parse cache one scan's workers share.
func newContentIgnoreSharedCache() *contentIgnoreSharedCache {
	return &contentIgnoreSharedCache{
		files:       make(map[string]*contentIgnoreFile),
		excludes:    make(map[string]*contentIgnoreFile),
		global:      make(map[string]*contentIgnoreFile),
		globalPaths: make(map[string]string),
		globalStats: make(map[string]contentIgnoreStat),
	}
}

// stats snapshots the cache counters for profiling.
func (c *contentIgnoreSharedCache) stats() (loads, hits int64, dirs int) {
	if c == nil {
		return 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads, c.hits, len(c.files)
}

// census counts parsed ignore files and patterns (anchored + floating) for
// profiling: how many patterns every file's match loops over.
func (c *contentIgnoreSharedCache) census() (files, patterns, anchored, floating int) {
	if c == nil {
		return 0, 0, 0, 0
	}
	count := func(f *contentIgnoreFile) {
		if f == nil {
			return
		}
		files++
		for _, l := range f.anchored {
			anchored += len(l)
		}
		floating += len(f.floating)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.files {
		count(f)
	}
	for _, f := range c.excludes {
		count(f)
	}
	for _, f := range c.global {
		count(f)
	}
	patterns = anchored + floating
	return files, patterns, anchored, floating
}

// censusTop reports the most pattern-heavy parsed files ("dir: anchored+floating")
// for profiling: the levels every file's match consults.
func (c *contentIgnoreSharedCache) censusTop(n int) []string {
	if c == nil {
		return nil
	}
	type entry struct {
		dir string
		n   int
	}
	var all []entry
	add := func(dir string, f *contentIgnoreFile) {
		if f == nil {
			return
		}
		m := len(f.floating)
		for _, l := range f.anchored {
			m += len(l)
		}
		all = append(all, entry{dir, m})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for dir, f := range c.files {
		add(dir, f)
	}
	for dir, f := range c.excludes {
		add(dir+"\\.git\\info\\exclude", f)
	}
	for dir, f := range c.global {
		add(dir+"\\global", f)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n })
	var out []string
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, all[i].dir+":"+strconv.Itoa(all[i].n))
	}
	return out
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
	g := &contentGitIgnore{
		repos:       roots,
		files:       make(map[string]*contentIgnoreFile),
		excludes:    make(map[string]*contentIgnoreFile),
		global:      make(map[string]*contentIgnoreFile),
		globalPaths: make(map[string]string),
		globalStats: make(map[string]contentIgnoreStat),
		dirIgnored:  make(map[string]bool),
	}
	g.rebuildRepoPrefix()
	return g
}

// rebuildRepoPrefix refreshes the separator-terminated repo forms after the
// root list changes. Callers that mutate repos (construction, addRepo) own
// the update; readers only need the slices to stay parallel.
func (g *contentGitIgnore) rebuildRepoPrefix() {
	g.repoPrefix = g.repoPrefix[:0]
	for _, root := range g.repos {
		g.repoPrefix = append(g.repoPrefix, contentScopePrefix(root))
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
	g.rebuildRepoPrefix()
}

// repoFor returns the deepest worktree containing the normalized path p.
// Repos resolve deepest-first, so the first prefix hit wins.
func (g *contentGitIgnore) repoFor(p string) (string, bool) {
	if g == nil {
		return "", false
	}
	for i, root := range g.repos {
		sep := ""
		if i < len(g.repoPrefix) {
			sep = g.repoPrefix[i]
		} else {
			sep = contentScopePrefix(root)
		}
		if contentPrefixUnder(p, root, sep) {
			return root, true
		}
	}
	return "", false
}

// ignored reports whether path (any case; normalized internally for worktree
// lookup) is excluded by git. Paths outside every worktree are never ignored
// here; callers apply their own rules there. The .git directory itself is
// always ignored. Pattern matching is case-sensitive on the path's original
// components (see the package comment); only worktree lookup folds case,
// because Windows path prefixes are case-insensitive.
func (g *contentGitIgnore) ignored(original string, isDir bool) bool {
	p := contentNormPath(original)
	root, ok := g.repoFor(p)
	if !ok || p == root {
		return false
	}
	relOrig := contentRelCompsOriginal(root, p, original)
	relFold := contentRelCompsOriginal(root, p, p)
	if relOrig == nil || relFold == nil {
		return false
	}
	for _, c := range relFold {
		if c == ".git" {
			return true
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(relOrig) > 1 && g.dirIgnoredLocked(root, relOrig[:len(relOrig)-1]) {
		return true
	}
	return g.matchLocked(root, relOrig, isDir)
}

// ignoredInRepo answers ignored() for a path already known to sit in repo,
// skipping the worktree lookup. The caller must have established membership
// with the same case rules repoFor uses (a normalized-prefix hit); behavior
// is otherwise identical to ignored().
func (g *contentGitIgnore) ignoredInRepo(original string, isDir bool, repo string) bool {
	p := contentNormPath(original)
	if p == "" || p == repo {
		return false
	}
	relOrig := contentRelCompsOriginal(repo, p, original)
	relFold := contentRelCompsOriginal(repo, p, p)
	if relOrig == nil || relFold == nil {
		return false
	}
	for _, c := range relFold {
		if c == ".git" {
			return true
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(relOrig) > 1 && g.dirIgnoredLocked(repo, relOrig[:len(relOrig)-1]) {
		return true
	}
	return g.matchLocked(repo, relOrig, isDir)
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
	if g.shared != nil {
		return g.shared.globalPathFor(root)
	}
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

// globalPathFor is the scan-shared form of globalForLocked: resolved once per
// scan no matter how many workers ask.
func (c *contentIgnoreSharedCache) globalPathFor(root string) string {
	c.mu.Lock()
	if p, ok := c.globalPaths[root]; ok {
		c.hits++
		c.mu.Unlock()
		return p
	}
	c.mu.Unlock()
	// Config reads outside the lock; concurrent duplicates resolve
	// identically and last-writer-wins below.
	p := contentGlobalExcludesFile(root)
	var st contentIgnoreStat
	if p != "" {
		st = contentIgnoreFileStat(p)
	}
	c.mu.Lock()
	c.globalPaths[root] = p
	if p != "" {
		c.globalStats[root] = st
	}
	c.loads++
	c.mu.Unlock()
	return p
}

// fileForDir returns the parsed .gitignore for the folded dir, loading (and
// caching, including misses) on first use. The scan-shared form loads once
// per scan across all workers; the private form keeps the historical
// once-per-gitignore-object behavior for non-scan callers. The load runs
// without the shared lock: parses are deterministic, so a duplicate load is
// harmless and last-writer-wins.
func (g *contentGitIgnore) fileForDir(dir string, load func() *contentIgnoreFile) *contentIgnoreFile {
	if g.shared != nil {
		c := g.shared
		c.mu.Lock()
		if f, ok := c.files[dir]; ok {
			c.hits++
			c.mu.Unlock()
			return f
		}
		c.mu.Unlock()
		f := load()
		c.mu.Lock()
		c.files[dir] = f
		c.loads++
		c.mu.Unlock()
		return f
	}
	if f, ok := g.files[dir]; ok {
		return f
	}
	f := load()
	g.files[dir] = f
	return f
}

// excludeFor returns the cached .git/info/exclude parse for a repo root,
// loading once on miss, through the shared cache when the scan provides one.
func (g *contentGitIgnore) excludeFor(root string, load func() *contentIgnoreFile) *contentIgnoreFile {
	if g.shared != nil {
		return g.shared.parsedForShared(g.shared.excludes, root, load)
	}
	if f, ok := g.excludes[root]; ok {
		return f
	}
	f := load()
	g.excludes[root] = f
	return f
}

// globalParseFor returns the cached global-excludes parse for a repo root,
// loading once on miss, through the shared cache when the scan provides one.
func (g *contentGitIgnore) globalParseFor(root string, load func() *contentIgnoreFile) *contentIgnoreFile {
	if g.shared != nil {
		return g.shared.parsedForShared(g.shared.global, root, load)
	}
	if f, ok := g.global[root]; ok {
		return f
	}
	f := load()
	g.global[root] = f
	return f
}

// parsedForShared returns the cached repo-level parse, loading (without the
// lock; parses are deterministic) once on miss.
func (c *contentIgnoreSharedCache) parsedForShared(m map[string]*contentIgnoreFile, key string, load func() *contentIgnoreFile) *contentIgnoreFile {
	c.mu.Lock()
	if f, ok := m[key]; ok {
		c.hits++
		c.mu.Unlock()
		return f
	}
	c.mu.Unlock()
	f := load()
	c.mu.Lock()
	m[key] = f
	c.loads++
	c.mu.Unlock()
	return f
}

func (g *contentGitIgnore) dirIgnoredLocked(root string, relOrig []string) bool {
	key := root + `\` + strings.Join(relOrig, `\`)
	if v, ok := g.dirIgnored[key]; ok {
		return v
	}
	res := len(relOrig) > 1 && g.dirIgnoredLocked(root, relOrig[:len(relOrig)-1])
	if !res {
		res = g.matchLocked(root, relOrig, true)
	}
	g.dirIgnored[key] = res
	return res
}

// matchLocked applies the ignore files from the path's own directory up to
// the worktree root, then .git/info/exclude, then git's global excludes file;
// the first matching pattern decides. relOrig holds the path's original-case
// components below root, which is what patterns match against; the ignore
// files themselves are loaded through case-folded directory paths (the same
// physical file on a case-insensitive volume) and cached under them.
func (g *contentGitIgnore) matchLocked(root string, relOrig []string, isDir bool) bool {
	relFold := make([]string, len(relOrig))
	for i, c := range relOrig {
		relFold[i] = strings.ToLower(c)
	}
	for depth := len(relOrig) - 1; depth >= 0; depth-- {
		dir := root
		if depth > 0 {
			dir = root + `\` + strings.Join(relFold[:depth], `\`)
		}
		domain := relOrig[:depth]
		file := g.fileForDir(dir, func() *contentIgnoreFile {
			return loadContentIgnoreFile(filepath.Join(dir, ".gitignore"), domain)
		})
		var result gitignore.MatchResult
		if isDir {
			result = file.match(relOrig, depth, true)
		} else {
			result = file.matchFile(relOrig, depth)
		}
		if result != gitignore.NoMatch {
			return result == gitignore.Exclude
		}
	}
	exclude := g.excludeFor(root, func() *contentIgnoreFile {
		return loadContentIgnoreFile(filepath.Join(root, ".git", "info", "exclude"), nil)
	})
	if result := fileMatchResult(exclude, relOrig, 0, isDir); result != gitignore.NoMatch {
		return result == gitignore.Exclude
	}
	// Git's global excludes file (core.excludesFile) ranks below
	// .git/info/exclude. Like exclude, its patterns are root-relative.
	if path := g.globalForLocked(root); path != "" {
		global := g.globalParseFor(root, func() *contentIgnoreFile {
			return loadContentIgnoreFile(path, nil)
		})
		return fileMatchResult(global, relOrig, 0, isDir) == gitignore.Exclude
	}
	return false
}

// fileMatchResult dispatches one ignore file's match on isDir so directory
// decisions keep the exact historical path while files take matchFile.
func fileMatchResult(f *contentIgnoreFile, comps []string, depth int, isDir bool) gitignore.MatchResult {
	if isDir {
		return f.match(comps, depth, true)
	}
	return f.matchFile(comps, depth)
}

// matchFile evaluates one ignore file's patterns against a file (not a
// directory) with the same result match would return, testing only provable
// candidates: anchored patterns at comps[depth], exact-name and
// prefix/suffix hits on the basename, and every other floating pattern in
// full. The bucket skips are sound: bucketed patterns are non-negated and
// non-dirOnly, so a match on an intermediate component would have ignored
// that component's directory first (go-git's isDir is irrelevant to such
// patterns, and directories take the full floating list) — the file only
// reaches here with all intermediates proven clean, so a bucketed pattern
// can only match the basename, and a basename hit decides Exclude exactly
// as Match would (the domain prefix always holds at a consulted level).
// Negations and dirOnly patterns keep the full Match because an
// intermediate-component match is load-bearing for precedence (a negation
// re-includes over lower lines, e.g. `.gemini/.gitignore`'s `!commands/`
// shadowing `/*`). Sources merge by descending line, exactly like match's
// anchored/floating interleave.
func (f *contentIgnoreFile) matchFile(comps []string, depth int) gitignore.MatchResult {
	if f == nil || depth >= len(comps) {
		return gitignore.NoMatch
	}
	base := comps[len(comps)-1]
	a := f.anchored[comps[depth]]
	ia := len(a)
	e := f.fileExact[base]
	ie := len(e)
	r := f.fileRest
	ir := len(r)
	x := f.fileAffix
	ix := len(x)
	hit := func(ax contentIgnoreAffix) bool {
		if ax.prefix {
			return strings.HasPrefix(base, ax.lit)
		}
		return strings.HasSuffix(base, ax.lit)
	}
	for ix > 0 && !hit(x[ix-1]) {
		ix--
	}
	for ia > 0 || ie > 0 || ix > 0 || ir > 0 {
		best, bestLine := 3, -1
		if ia > 0 && a[ia-1].line > bestLine {
			best, bestLine = 0, a[ia-1].line
		}
		if ie > 0 && e[ie-1].line > bestLine {
			best, bestLine = 1, e[ie-1].line
		}
		if ix > 0 && x[ix-1].pat.line > bestLine {
			best, bestLine = 2, x[ix-1].pat.line
		}
		if ir > 0 && r[ir-1].line > bestLine {
			best, bestLine = 3, r[ir-1].line
		}
		switch best {
		case 0:
			if res := a[ia-1].pattern.Match(comps, false); res != gitignore.NoMatch {
				return res
			}
			ia--
		case 1:
			return gitignore.Exclude
		case 2:
			return gitignore.Exclude
		default:
			if res := r[ir-1].pattern.Match(comps, false); res != gitignore.NoMatch {
				return res
			}
			ir--
		}
	}
	return gitignore.NoMatch
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
		// No case-folding: patterns match case-sensitively, like ripgrep and
		// like git for every file that exists (see the package comment).
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := contentIgnorePattern{line: i, pattern: gitignore.ParsePattern(line, domain)}
		if key, ok := contentIgnoreAnchor(line); ok {
			f.anchored[key] = append(f.anchored[key], p)
		} else {
			f.floating = append(f.floating, p)
			// Only non-negated, non-dirOnly patterns enter the buckets.
			// A negated pattern matching an intermediate component
			// re-includes the file and shadows lower-precedence patterns
			// (e.g. `.gemini/.gitignore`'s `!commands/` over `/*`), so
			// negations keep the full Match; dirOnly patterns can match
			// an intermediate file-or-dir alike, so they do too.
			if strings.HasPrefix(line, "!") || contentIgnoreDirOnly(line) {
				f.fileRest = append(f.fileRest, p)
				continue
			}
			if kind, lit := contentIgnoreFileShape(line); kind != "" {
				switch kind {
				case "exact":
					if f.fileExact == nil {
						f.fileExact = make(map[string][]contentIgnorePattern)
					}
					f.fileExact[lit] = append(f.fileExact[lit], p)
				case "prefix":
					f.fileAffix = append(f.fileAffix, contentIgnoreAffix{prefix: true, lit: lit, pat: p})
				case "suffix":
					f.fileAffix = append(f.fileAffix, contentIgnoreAffix{prefix: false, lit: lit, pat: p})
				}
			} else {
				f.fileRest = append(f.fileRest, p)
			}
		}
	}
	if len(f.anchored) == 0 && len(f.floating) == 0 {
		return nil
	}
	return f
}

// contentIgnoreDirOnly reports whether the line is a directory-only pattern
// (trailing slash), using go-git ParsePattern's exact preprocessing.
func contentIgnoreDirOnly(line string) bool {
	s := strings.TrimPrefix(line, "!")
	if !strings.HasSuffix(s, `\ `) {
		s = strings.TrimRight(s, " ")
	}
	return strings.HasSuffix(s, "/")
}

// contentIgnoreFileShape classifies a floating non-dirOnly pattern line for
// the file-only dispatch, mirroring go-git ParsePattern's preprocessing
// (negation prefix, escaped trailing space, trailing slash). It returns
// ("exact"|"prefix"|"suffix", literal) for slash-free patterns whose only
// magic is one leading (suffix) or trailing (prefix) star over a literal
// rest, or no magic at all (exact); anything else ("", glob or other magic)
// takes the full Match path. A backslash counts as magic (conservative:
// filepath.Match treats it as a separator on Windows).
func contentIgnoreFileShape(line string) (kind, lit string) {
	s := strings.TrimPrefix(line, "!")
	if !strings.HasSuffix(s, `\ `) {
		s = strings.TrimRight(s, " ")
	}
	s = strings.TrimSuffix(s, "/")
	if strings.Contains(s, "/") {
		return "", ""
	}
	if strings.ContainsAny(s, `?[\`) {
		return "", ""
	}
	if !strings.Contains(s, "*") {
		return "exact", s
	}
	// One star total, leading or trailing, over a non-empty literal rest.
	if len(s) > 1 && strings.HasPrefix(s, "*") && !strings.Contains(s[1:], "*") {
		return "suffix", s[1:]
	}
	if len(s) > 1 && strings.HasSuffix(s, "*") && !strings.Contains(s[:len(s)-1], "*") {
		return "prefix", s[:len(s)-1]
	}
	return "", ""
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
	return contentSplitComponents(rel)
}

// contentSplitComponents splits a path into components on either separator.
func contentSplitComponents(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == '\\' || r == '/' })
}

// contentRelCompsOriginal splits original (any case) into the components below
// the folded worktree root, for case-sensitive pattern matching. folded is the
// normalized form of original; the root prefix is aligned component-wise (not
// byte-sliced, since case-folding can change byte length) and verified, so a
// misaligned path degrades to nil instead of matching the wrong components.
func contentRelCompsOriginal(root, folded, original string) []string {
	oComps := contentSplitComponents(original)
	fComps := contentSplitComponents(folded)
	rComps := contentSplitComponents(root)
	if len(oComps) != len(fComps) || len(fComps) <= len(rComps) {
		return nil
	}
	for i := range rComps {
		if fComps[i] != rComps[i] {
			return nil
		}
	}
	return oComps[len(rComps):]
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
