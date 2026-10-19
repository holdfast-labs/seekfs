package main

// Inclusion policy (WORK_QUEUE item 1, gap G3): which files content search
// indexes. The reference design (Noah Dunnagan's fsearch) keeps a small index
// by indexing only what's worth searching: text files up to 1 MiB (10 MiB
// inside git repos) minus what git ignores, skipping lockfiles, large JSON,
// minified/bundled assets and build-output folders outside repos, indexing a
// linked worktree's main checkout only, indexing identical files once, with a
// user skip list.
//
// Rules, in the order they are evaluated (first hit names the skip reason):
//
//  1. worktree  — linked git worktrees (a `.git` FILE pointing at
//     `<main>/.git/worktrees/<name>`) are excluded unless
//     `[content] worktrees = "all"`. Applied at scope resolution: linked
//     roots never join Repos/Roots, so selection, estimate, reconcile and
//     drift all agree. Submodule pointers (`.../.git/modules/...`) and
//     plain repos (a `.git` directory) are still indexed.
//  2. gitignore — inside a worktree git's rules decide (unchanged).
//  3. user_skip — the operator's `content-skip` list (paths/globs).
//  4. lockfile  — well-known dependency lockfiles, by exact basename.
//  5. large_json — `.json`/`.jsonc`/`.json5` at or above 100 KiB.
//  6. minified  — `*.min.js/css`, bundle/chunk names, `*.map`, or sampled
//     text whose average line length looks generated.
//  7. build_output — dist/build/out/target/… segments, only outside repos
//     (inside a repo git's ignore rules own directories).
//  8. dependency — Python venv / site-packages, JS node_modules, and other
//     dependency or tool-cache trees (`.venv`, `venv`, `site-packages`,
//     `node_modules`, `__pycache__`, `.tox`, `.mypy_cache`, `.pytest_cache`,
//     `.ruff_cache`), only outside repos: inside a repo git's ignore rules
//     decide, so a tracked/un-ignored tree stays indexable. Rust `target`
//     dirs are covered by build_output outside repos; CACHEDIR.TAG is not
//     consulted (a name-based rule is enough and cheaper).
//  9. cache — well-known cache trees (Go/npm/pip/uv/Cargo/Gradle/NuGet,
//     browser caches), inside or outside repos.
//
// Size caps are per file, not per build: the default text cap is ~1 MiB
// outside git repos and ~10 MiB inside. An explicit operator cap
// (`-max-raw`/`-max-text`) overrides both uniformly. Over-cap text is
// indexed as a bounded prefix (Truncated, as before); over-cap containers
// are Skipped.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skip-reason codes. Stable strings: they are persisted in the CXIS section
// and surfaced in `--estimate` and `loaded --json`, so never rename one.
const (
	contentSkipWorktree    = "worktree"
	contentSkipLockfile    = "lockfile"
	contentSkipLargeJSON   = "large_json"
	contentSkipMinified    = "minified"
	contentSkipBuildOutput = "build_output"
	contentSkipDependency  = "dependency"
	contentSkipCache       = "cache"
	contentSkipUserSkip    = "user_skip"
)

// contentWorktreesAll is the `[content] worktrees` value that indexes linked
// worktrees too. The default ("main") indexes only the main checkout.
const contentWorktreesAll = "all"

// contentSkipSizeCap is the skip reason for a container file over the
// per-file cap (text files over the cap are truncated, not skipped).
const contentSkipSizeCap = "size_cap"

// contentInclusionReason reports whether an extraction skip reason is a
// canonical inclusion-policy code (tally per reason) as opposed to a
// free-form extractor message (tally coarse only).
func contentInclusionReason(reason string) bool {
	switch reason {
	case contentSkipLockfile, contentSkipLargeJSON, contentSkipMinified,
		contentSkipBuildOutput, contentSkipDependency, contentSkipCache,
		contentSkipUserSkip, contentSkipWorktree, contentSkipSizeCap:
		return true
	}
	return false
}

// Default per-file text caps (see contentInclusionTextCap). Raw caps are
// twice the text caps, the same 2:1 headroom the historical 32/16 MiB
// defaults kept (UTF-16 decodes wider than its source).
const (
	contentInclusionTextCapOutside = 1 << 20
	contentInclusionTextCapInside  = 10 << 20
)

// contentLargeJSONThresholdBytes skips JSON files at or above this size:
// serialized data, not searchable prose.
const contentLargeJSONThresholdBytes = 100 << 10

// contentMinifiedSampleBytes bounds the average-line-length sample, and
// contentMinifiedAvgLineLen is the skip threshold over it. A file at or
// above contentMinifiedMinSize whose sampled lines average longer than
// this is generated/bundled text (an 11.4 MiB single-line tokenizer.json
// made every broad query 4-5x slower). Tuned on Chromium; see §6z.
const (
	contentMinifiedSampleBytes  = 64 << 10
	contentMinifiedAvgLineLen   = 2048
	contentMinifiedMinSizeBytes = 32 << 10
)

// contentLockfileNames are exact (lowercase) basenames never indexed:
// resolved dependency pins, not searchable source.
var contentLockfileNames = map[string]struct{}{
	"package-lock.json": {}, "npm-shrinkwrap.json": {}, "yarn.lock": {},
	"pnpm-lock.yaml": {}, "bun.lockb": {}, "bun.lock": {},
	"cargo.lock": {}, "go.sum": {}, "go.work.sum": {},
	"poetry.lock": {}, "pdm.lock": {}, "pipfile.lock": {},
	"composer.lock": {}, "gemfile.lock": {}, "cargo.lock.json": {},
	"mix.lock": {}, "pubspec.lock": {}, "packages.lock.json": {},
}

// contentLargeJSONExts are the extensions the large-JSON skip applies to.
// Line-delimited `.jsonl`/`.ndjson` logs stay indexed: their lines are
// short and greppable, unlike one-blob serialized JSON.
var contentLargeJSONExts = map[string]struct{}{
	".json": {}, ".jsonc": {}, ".json5": {},
}

// contentMinifiedNameSubs are lowercase filename substrings marking
// minified/bundled assets. `.map` (sourcemaps) are build artifacts too.
var contentMinifiedNameSubs = []string{
	".min.", "-min.", ".bundle.", ".chunk.", ".minified.",
}

// contentBuildOutputSegments are directory names treated as build output
// OUTSIDE git repos only: inside a repo git's ignore rules own directories
// (a tracked `build/` dir is source). Entries already covered by the
// system excludes keep their system reason; this set is checked first so
// the build-output reason wins for the names both lists share.
var contentBuildOutputSegments = []string{
	"dist", "build", "out", "target", "bin", "obj", "_build", ".next", ".nuxt", "coverage",
}

// contentCacheSegments are directory names that mark a cache tree wherever
// they appear (inside or outside repos).
var contentCacheSegments = []string{
	"gocache", "go-build", ".npm", "_cacache", ".pnpm-store", "pnpm-store",
	".yarn", "yarn-cache", ".cargo", ".gradle", ".nuget", ".cache",
}

// contentDependencySegments are directory names marking dependency / virtualenv
// trees, applied OUTSIDE git repos only: inside a repo gitignore decides, so a
// tracked or deliberately-un-ignored tree stays indexable (e.g. a vendored
// node_modules). Rust `target` directories, with or without a CACHEDIR.TAG,
// are already covered by contentBuildOutputSegments outside repos.
var contentDependencySegments = []string{
	"site-packages", "node_modules", "__pycache__", ".venv", "venv",
	".tox", ".mypy_cache", ".pytest_cache", ".ruff_cache",
}

// contentCachePathSubs are lowercase `/`-separated path substrings marking
// cache trees whose directory names alone are too generic to match
// (e.g. `CacheStorage` under a browser profile, `registry` under cargo).
// Matched against the `/`-joined lowercase path with slashes on both sides,
// so `registry` never matches `my-registry-tool`.
var contentCachePathSubs = []string{
	"/go-build/", "/_cacache/", "/.npm/", "/pnpm-store/", "/.yarn/",
	"/cargo/registry/", "/cargo/git/", "/.cargo/", "/.gradle/", "/.nuget/",
	"/pip/cache/", "/pip-cache/", "/uv/cache/", "/.cache/uv/",
	"/cachestorage/", "/gpucache/", "/code cache/", "/dawncache/",
	"/mozilla/firefox/", "/google/chrome/", "/microsoft/edge/",
	"/chromium/user data/", "/node_modules/.cache/",
}

// contentLockfileBase reports whether the lowercase basename is a lockfile.
func contentLockfileBase(base string) bool {
	_, ok := contentLockfileNames[base]
	return ok
}

// contentLargeJSONSkip reports whether ext+size is a skippable large JSON
// blob. size < 0 means unknown (selection without a stat): admit.
func contentLargeJSONSkip(ext string, size int64) bool {
	if _, ok := contentLargeJSONExts[ext]; !ok {
		return false
	}
	return size >= contentLargeJSONThresholdBytes
}

// contentMinifiedName reports whether the lowercase basename looks like a
// minified/bundled asset by name alone.
func contentMinifiedName(base string) bool {
	if strings.HasSuffix(base, ".map") {
		return true
	}
	for _, sub := range contentMinifiedNameSubs {
		if strings.Contains(base, sub) {
			return true
		}
	}
	return false
}

// contentBuildOutputSegment reports whether a single lowercase path segment
// names build output.
func contentBuildOutputSegment(seg string) bool {
	for _, s := range contentBuildOutputSegments {
		if seg == s {
			return true
		}
	}
	return false
}

// contentCacheSegment reports whether a single lowercase path segment marks
// a cache tree.
func contentCacheSegment(seg string) bool {
	for _, s := range contentCacheSegments {
		if seg == s {
			return true
		}
	}
	return false
}

// contentDependencySegment reports whether a single lowercase path segment
// names a dependency/virtualenv tree (outside repos only; see
// contentDependencySegments).
func contentDependencySegment(seg string) bool {
	for _, s := range contentDependencySegments {
		if seg == s {
			return true
		}
	}
	return false
}

// contentCachePath reports whether the lowercase `/`-joined path matches a
// cache-tree substring.
func contentCachePath(slashPath string) bool {
	for _, sub := range contentCachePathSubs {
		if strings.Contains(slashPath, sub) {
			return true
		}
	}
	return false
}

// contentSplitLowerSegments splits a normalized (folded) path into segments.
func contentSplitLowerSegments(normPath string) []string {
	return strings.FieldsFunc(normPath, func(r rune) bool { return r == '\\' || r == '/' })
}

// contentInclusionSkip applies the file-level inclusion skips to one file.
// normPath is the folded normalized path, base/ext its lowercase basename
// and extension, size the source bytes (< 0 = unknown: size rules admit),
// inRepo whether the path is inside a git worktree, sysExcludes whether
// generated-directory skipping is on (system_excludes=false opts out of
// the directory rules but not the file rules), and skip the operator's
// content-skip patterns. It returns the reason code and true when the file
// must not be indexed. Git-ignore and root membership are NOT handled here:
// the caller applies them in place (allows/admitRecord/estimate) so reason
// order stays: git rules first, then the operator list, then the built-ins.
func contentInclusionSkip(normPath, base, ext string, size int64, inRepo bool, sysExcludes bool, skip []string) (string, bool) {
	if contentUserSkipMatch(normPath, skip) {
		return contentSkipUserSkip, true
	}
	if contentLockfileBase(base) {
		return contentSkipLockfile, true
	}
	if contentLargeJSONSkip(ext, size) {
		return contentSkipLargeJSON, true
	}
	if contentMinifiedName(base) {
		return contentSkipMinified, true
	}
	if !sysExcludes {
		return "", false
	}
	segs := contentSplitLowerSegments(normPath)
	// Non-final segments only: a file literally named `build` or `cache`
	// is not a build-output dir or a cache tree.
	last := len(segs) - 1
	if !inRepo {
		for i := 0; i < last; i++ {
			if contentBuildOutputSegment(segs[i]) {
				return contentSkipBuildOutput, true
			}
		}
		for i := 0; i < last; i++ {
			if contentDependencySegment(segs[i]) {
				return contentSkipDependency, true
			}
		}
	}
	for i := 0; i < last; i++ {
		if contentCacheSegment(segs[i]) {
			return contentSkipCache, true
		}
	}
	if contentCachePath("/" + strings.Join(segs, "/") + "/") {
		return contentSkipCache, true
	}
	return "", false
}

// contentInclusionTextCap returns the default extracted-text cap for one
// file: ~1 MiB outside git repos, ~10 MiB inside.
func contentInclusionTextCap(inRepo bool) int64 {
	if inRepo {
		return contentInclusionTextCapInside
	}
	return contentInclusionTextCapOutside
}

// contentInclusionCapsForFile resolves the raw/text caps for one file.
// Explicit operator caps (either > 0) apply uniformly, exactly as before
// (normalized and clamped); otherwise the repo-aware inclusion defaults
// apply (1 MiB text outside repos, 10 MiB inside, raw at 2:1 headroom).
func contentInclusionCapsForFile(inRepo bool, explicitRaw, explicitText int64) (maxRaw, maxText int64) {
	if explicitRaw > 0 || explicitText > 0 {
		raw, text := explicitRaw, explicitText
		if raw <= 0 {
			raw = contentExtractMaxRawBytes
		}
		if text <= 0 {
			text = contentExtractMaxTextBytes
		}
		return contentNormalizeExtractCaps(raw, text)
	}
	text := contentInclusionTextCap(inRepo)
	return contentNormalizeExtractCaps(2*text, text)
}

// contentAverageLineLen returns the average line length over the first
// contentMinifiedSampleBytes of data (newline-delimited; a missing trailing
// newline still terminates the last line).
func contentAverageLineLen(data []byte) float64 {
	if len(data) > contentMinifiedSampleBytes {
		data = data[:contentMinifiedSampleBytes]
	}
	if len(data) == 0 {
		return 0
	}
	lines := 1
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	return float64(len(data)) / float64(lines)
}

// contentLooksMinified reports whether extracted text looks like generated/
// bundled output by average line length. Only applied to files at or above
// contentMinifiedMinSizeBytes, so a small file with one long line stays
// indexed.
func contentLooksMinified(text []byte) bool {
	if len(text) < contentMinifiedMinSizeBytes {
		return false
	}
	return contentAverageLineLen(text) > contentMinifiedAvgLineLen
}

// contentUserSkipMatch reports whether the folded normalized path matches
// any content-skip pattern: a plain entry matches the path itself or any
// directory below it (prefix with a separator boundary); an entry with glob
// magic (*?[]) matches with ** spanning separators.
func contentUserSkipMatch(normPath string, patterns []string) bool {
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		if contentSkipHasMagic(pat) {
			if contentSkipGlobMatch(pat, normPath) {
				return true
			}
			continue
		}
		if normPath == pat || strings.HasPrefix(normPath, contentScopePrefix(pat)) {
			return true
		}
	}
	return false
}

// contentSkipHasMagic reports whether a normalized pattern holds glob magic.
func contentSkipHasMagic(pat string) bool {
	return strings.ContainsAny(pat, "*?[")
}

// contentNormalizeSkipPatterns folds and cleans skip entries. Glob magic
// survives Clean (it has no path semantics to collapse); a trailing
// separator is kept as a directory-prefix marker by trimming it here — the
// prefix check adds the boundary itself.
func contentNormalizeSkipPatterns(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if contentSkipHasMagic(p) {
			// Fold separators and case; drop a trailing separator so
			// `dir/**` and `dir/` behave alike.
			p = strings.ToLower(strings.ReplaceAll(p, "/", string(filepath.Separator)))
			p = strings.TrimSuffix(p, string(filepath.Separator))
			if p != "" {
				out = append(out, p)
			}
			continue
		}
		if n := contentNormPath(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// contentSkipGlobMatch matches a normalized glob pattern against a
// normalized path, segment-wise; ** spans any number of segments.
func contentSkipGlobMatch(pattern, path string) bool {
	split := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return r == '\\' || r == '/' })
	}
	return contentSkipMatchSegs(split(pattern), split(path))
}

func contentSkipMatchSegs(pat, path []string) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(path); i++ {
			if contentSkipMatchSegs(pat[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	// A `**` inside a segment (not spanning) degrades to `*`: only a
	// full `**` segment crosses separators.
	seg := strings.ReplaceAll(pat[0], "**", "*")
	ok, err := filepath.Match(seg, path[0])
	if err != nil || !ok {
		return false
	}
	return contentSkipMatchSegs(pat[1:], path[1:])
}

// contentInRepoContextKey carries one file's worktree membership for
// extraction-time policy (per-file caps) through the extraction context.
type contentInRepoContextKey struct{}

// contentWithInRepo stamps one file's worktree membership onto ctx.
func contentWithInRepo(ctx context.Context, inRepo bool) context.Context {
	return context.WithValue(ctx, contentInRepoContextKey{}, inRepo)
}

// contentInRepoFromContext returns the stamped membership and whether one
// was stamped. Unset means unknown: callers fail open to the in-repo
// (larger) caps so a missing scope never starves a file.
func contentInRepoFromContext(ctx context.Context) (bool, bool) {
	if ctx != nil {
		if v, ok := ctx.Value(contentInRepoContextKey{}).(bool); ok {
			return v, true
		}
	}
	return true, false
}

// contentFileCaps resolves the raw/text caps for one extraction from the
// context settings and the file's worktree membership. Explicit operator
// caps apply uniformly; otherwise the repo-aware inclusion defaults apply
// (1 MiB text outside repos, 10 MiB inside, raw at 2:1 headroom).
func contentFileCaps(settings contentExtractSettings, inRepo bool) (maxRaw, maxText int64) {
	if settings.capsExplicit {
		return contentNormalizeExtractCaps(settings.maxRaw, settings.maxText)
	}
	return contentInclusionCapsForFile(inRepo, 0, 0)
}

// contentFileExtractContext derives the per-file extraction context: the
// parent's encoding with this file's caps. It returns the context and the
// caps, so the caller bounds both the slurp and the extractor identically.
func contentFileExtractContext(ctx context.Context, inRepo bool) (context.Context, int64, int64) {
	settings := contentExtractSettingsFromContext(ctx)
	fileRaw, fileText := contentFileCaps(settings, inRepo)
	if fileRaw == settings.maxRaw && fileText == settings.maxText {
		return ctx, fileRaw, fileText
	}
	return contentWithExtractSettings(ctx, contentExtractSettings{
		encoding:     settings.encoding,
		maxRaw:       fileRaw,
		maxText:      fileText,
		capsExplicit: true,
	}), fileRaw, fileText
}

// contentWorktreeProbeFunc reads the `.git` entry at repoRoot from disk and
// reports whether it marks a LINKED worktree (a `.git` FILE whose `gitdir:`
// line points inside some `<main>/.git/worktrees/` dir). A `.git` directory
// (plain repo or main checkout), a submodule pointer (`.../modules/...`),
// a missing/unreadable entry, or unparseable content is NOT a linked
// worktree: selection fails open. A var so tests stub the filesystem.
var contentWorktreeProbeFunc = func(repoRoot string) bool {
	gitPath := filepath.Join(repoRoot, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil || info.IsDir() {
		return false
	}
	data, err := os.ReadFile(gitPath)
	if err != nil || len(data) > 64<<10 {
		return false
	}
	target, ok := contentParseGitDirPointer(data)
	if !ok {
		return false
	}
	folded := strings.ToLower(filepath.ToSlash(target))
	return strings.Contains(folded, "/.git/worktrees/")
}

// contentLinkedWorktreeDir reports whether dir holds a linked-worktree
// `.git` pointer. It delegates to contentWorktreeProbeFunc (a var) so
// tests stub one filesystem probe for every caller.
func contentLinkedWorktreeDir(dir string) bool {
	return contentWorktreeProbeFunc(dir)
}

// contentFilterLinkedWorktrees drops linked-worktree roots unless keepAll.
// Order is preserved; the dropped count is reported for health/estimate.
func contentFilterLinkedWorktrees(repos []string, keepAll bool) (kept []string, dropped []string) {
	if keepAll {
		return repos, nil
	}
	for _, r := range repos {
		if contentWorktreeProbeFunc(r) {
			dropped = append(dropped, r)
		} else {
			kept = append(kept, r)
		}
	}
	return kept, dropped
}

// contentInclusionReasonOrder returns the Skipped reasons in stable
// policy order for display.
func contentInclusionReasonOrder(s contentInclusionStats) []string {
	order := []string{"exclude", "gitignore", contentSkipUserSkip,
		contentSkipLockfile, contentSkipLargeJSON, contentSkipMinified,
		contentSkipBuildOutput, contentSkipDependency, contentSkipCache,
		"system", "binary", "ext", "root", "path",
		contentSkipSizeCap, contentSkipWorktree}
	var out []string
	seen := make(map[string]bool)
	for _, r := range order {
		if n := s.Skipped[r]; n > 0 {
			out = append(out, r)
			seen[r] = true
		}
	}
	var rest []string
	for r, n := range s.Skipped {
		if n > 0 && !seen[r] {
			rest = append(rest, r)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// contentInclusionStats tallies per-reason inclusion decisions for one
// build (or estimate): files and source bytes per skip reason, the
// truncated (bounded-prefix) files, and the content-hash dedupe savings.
// It is persisted in the CXIS section and surfaced in `loaded --json`
// and `--estimate`, so every policy decision is visible.
type contentInclusionStats struct {
	// Skipped maps reason -> files excluded.
	Skipped map[string]int64 `json:"skipped,omitempty"`
	// SkippedBytes maps reason -> source bytes excluded.
	SkippedBytes map[string]int64 `json:"skipped_bytes,omitempty"`
	// Truncated counts files indexed as a bounded prefix (over the
	// per-file text cap).
	Truncated int64 `json:"truncated,omitempty"`
	// TruncatedBytes counts source bytes cut by the cap.
	TruncatedBytes int64 `json:"truncated_bytes,omitempty"`
	// DedupeFiles/DedupeBytes count duplicate files stored once and the
	// extracted-text bytes that sharing saved.
	DedupeFiles int64 `json:"dedupe_files,omitempty"`
	DedupeBytes int64 `json:"dedupe_bytes,omitempty"`
	// WorktreesSkipped counts linked worktrees excluded by the main-only
	// policy.
	WorktreesSkipped int64 `json:"worktrees_skipped,omitempty"`
}

func (s *contentInclusionStats) add(reason string, bytes int64) {
	if s.Skipped == nil {
		s.Skipped = make(map[string]int64)
	}
	if s.SkippedBytes == nil {
		s.SkippedBytes = make(map[string]int64)
	}
	s.Skipped[reason]++
	s.SkippedBytes[reason] += bytes
}

func (s *contentInclusionStats) merge(o contentInclusionStats) {
	for r, n := range o.Skipped {
		if s.Skipped == nil {
			s.Skipped = make(map[string]int64)
		}
		s.Skipped[r] += n
	}
	for r, n := range o.SkippedBytes {
		if s.SkippedBytes == nil {
			s.SkippedBytes = make(map[string]int64)
		}
		s.SkippedBytes[r] += n
	}
	s.Truncated += o.Truncated
	s.TruncatedBytes += o.TruncatedBytes
	s.DedupeFiles += o.DedupeFiles
	s.DedupeBytes += o.DedupeBytes
	s.WorktreesSkipped += o.WorktreesSkipped
}
