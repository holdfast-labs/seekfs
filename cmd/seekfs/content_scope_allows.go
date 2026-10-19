package main

// Scope matching (WP3): turn a contentScope plus the git worktrees detected in
// an index into the effective include roots / excludes / extension set, and
// decide membership for a path. The build, the estimate, and the USN drain all
// route through contentScopeResolved.allows so they agree exactly.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// contentSystemExcludeSegments are directory names excluded anywhere in a path
// (matched on non-final segments only, so a file literally named "target" is
// not dropped).
var contentSystemExcludeSegments = []string{
	"node_modules", ".git", ".svn", "__pycache__", ".venv", "venv",
	"target", "dist", "build", ".gradle", ".cache", ".npm", ".nuget", ".cargo",
}

// contentSystemExcludeVolumeDirs are top-level volume directories excluded when
// SystemExcludes is on (case-folded, volume-relative).
var contentSystemExcludeVolumeDirs = []string{
	"windows", "program files", "program files (x86)", "programdata",
	"$recycle.bin", "system volume information", "recovery", "perflogs",
}

// contentBinaryExtensions are known-binary extensions rejected without
// opening the file. Anything a registered extractor claims (PDF, OOXML, ...)
// is never listed here — the disjointness is asserted by
// TestContentBinarySkipListDisjointFromExtractors — and anything else with an
// unknown or missing extension is admitted for head-sniffing at build time.
// The list is intentionally conservative: an extension that could be either
// (like .dat) is left to the NUL sniff, which mirrors ripgrep.
var contentBinaryExtensions = map[string]struct{}{
	// Images.
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".bmp": {}, ".ico": {},
	".tif": {}, ".tiff": {}, ".webp": {}, ".avif": {}, ".heic": {}, ".heif": {},
	".psd": {}, ".ai": {},
	// Archives, installers, packages.
	".zip": {}, ".7z": {}, ".rar": {}, ".tar": {}, ".gz": {}, ".bz2": {},
	".xz": {}, ".cab": {}, ".iso": {}, ".dmg": {}, ".jar": {}, ".war": {},
	".ear": {}, ".apk": {}, ".aab": {}, ".msi": {}, ".pkg": {}, ".deb": {},
	".rpm": {},
	// Media.
	".mp3": {}, ".mp4": {}, ".wav": {}, ".avi": {}, ".mov": {}, ".mkv": {},
	".flac": {}, ".ogg": {}, ".ogv": {}, ".webm": {}, ".m4a": {}, ".m4v": {},
	".aac": {}, ".wma": {}, ".wmv": {}, ".mid": {}, ".midi": {}, ".opus": {},
	".3gp": {}, ".flv": {}, ".wasm": {},
	// Objects, libraries, executables, debug info.
	".o": {}, ".obj": {}, ".so": {}, ".dylib": {}, ".dll": {}, ".exe": {},
	".lib": {}, ".a": {}, ".pyc": {}, ".pyo": {}, ".class": {}, ".pdb": {},
	".ilk": {}, ".exp": {}, ".pch": {}, ".gch": {}, ".ko": {}, ".sys": {},
	".drv": {},
	// Fonts.
	".ttf": {}, ".otf": {}, ".woff": {}, ".woff2": {}, ".eot": {},
	// Legacy document formats with no registered extractor (OLE2; see §7).
	".doc": {}, ".xls": {}, ".ppt": {},
	// Misc certainly-binary stores.
	".bin": {}, ".db": {}, ".sqlite": {}, ".sqlite3": {},
}

// contentBinaryExtension reports whether ext is a known-binary extension:
// rejected without opening. ext must be lowercase with a leading dot (as
// contentPathExtension returns); "" (extensionless) is never binary here.
func contentBinaryExtension(ext string) bool {
	if ext == "" {
		return false
	}
	_, ok := contentBinaryExtensions[ext]
	return ok
}

// contentBinaryRejectSet precomputes the known-binary extensions minus any a
// registered extractor claims, so the per-file sniff-mode gate is one map
// lookup instead of a registry scan. All extractors register in init, so the
// once-lazy set is fixed before any build runs.
var contentBinaryRejectSet = sync.OnceValue(func() map[string]struct{} {
	out := make(map[string]struct{}, len(contentBinaryExtensions))
	for ext := range contentBinaryExtensions {
		if !contentRegistryClaimsExt(ext) {
			out[ext] = struct{}{}
		}
	}
	return out
})

// contentBinaryRejected reports whether ext is rejected without opening the
// file under sniff-mode selection: a known-binary extension no registered
// extractor claims. Identical to contentBinaryExtension(ext) &&
// !contentRegistryClaimsExt(ext) with the registry scan hoisted.
func contentBinaryRejected(ext string) bool {
	if ext == "" {
		return false
	}
	_, ok := contentBinaryRejectSet()[ext]
	return ok
}

// contentScopeResolved is a scope with its effective roots and excludes for one
// volume plus the detected git worktrees.
type contentScopeResolved struct {
	Mode          contentScopeMode
	Roots         []string // effective include roots (normalized)
	Repos         []string // detected git worktree roots (normalized)
	ExplicitRoots []string // configured roots, before known folders are added
	// DriveRoot is the normalized volume root (auto mode only), added as the
	// final, lowest-priority include root so text files outside repos and
	// known folders are still in scope (parity with Noah's fsearch, which
	// indexes text drive-wide and only skips junk). "" when the volume names
	// no drive root or the scope is explicit.
	DriveRoot string
	Excludes  []string // normalized exclude prefixes
	ExtSet    map[string]struct{}
	// SniffText selects ripgrep-style selection: any file not cheaply rejected
	// (the binary skip list) is admitted and its 512-byte head sniffed at
	// build time (NUL in the first 8 KiB means binary). It is on unless the
	// operator configured an explicit extension allowlist, which stays an
	// opt-in verbatim gate.
	SniffText   bool
	BudgetBytes int64
	// BudgetExplicit mirrors contentScope.BudgetExplicit: only an explicit
	// budget joins the scope fingerprint (see contentScopeSelectionVersion).
	BudgetExplicit bool
	SystemExcludes bool
	// WorktreesAll indexes linked git worktrees too; false indexes only
	// each main checkout (linked roots never join Repos/Roots).
	WorktreesAll bool
	// Skip is the normalized content-skip list (paths/globs).
	Skip []string
	// DroppedWorktrees are the linked worktrees the main-only policy
	// filtered out of Repos, reported for estimate/health visibility.
	DroppedWorktrees []string
	// InclusionOff disables the §6z inclusion skips for the estimate's
	// legacy baseline (before vs after in one run). Never set on a
	// persisted scope: it does not join the fingerprint.
	InclusionOff bool
	// Ignore applies git ignore rules inside detected worktrees in place of
	// the name-based system excludes; IgnoreDigest fingerprints the ignore
	// files it reads so a sidecar built under other rules is rebuilt.
	Ignore       *contentGitIgnore
	IgnoreDigest [32]byte
	// prefix holds the trailing-separator form of each entry in a path
	// list, parallel to it: matching an already-normalized path is one
	// HasPrefix per entry, with no per-file Clean/ToLower. Built once by
	// resolve(); read-only afterwards.
	repoPrefix     []string
	rootPrefix     []string
	excludePrefix  []string
	explicitPrefix []string
	// driveRootPrefix is the separator-terminated form of DriveRoot ("" when
	// unset), so the lowest-priority match is one HasPrefix.
	driveRootPrefix string
	// rootExact maps a normalized path that exactly equals a root to it, for
	// the corner where a root names the file itself while its parent matches
	// no root (the only way a file joins without its parent joining).
	rootExact map[string]string
	// explicitSet marks ExplicitRoots for O(1) priority derivation.
	explicitSet map[string]struct{}
}

// contentScopePrefix returns the separator-terminated form of a normalized
// path for prefix matching with a separator boundary (so `sub` never
// matches `submarine`).
func contentScopePrefix(p string) string {
	if strings.HasSuffix(p, string(filepath.Separator)) {
		return p
	}
	return p + string(filepath.Separator)
}

// contentPrefixUnder reports contentPathUnder for an already-normalized
// path p against an already-normalized prefix u with its precomputed
// separator-terminated form uSep: exact equality or a boundary prefix.
// Identical to contentPathUnder on normalized inputs, without cleaning.
func contentPrefixUnder(p, u, uSep string) bool {
	return p == u || strings.HasPrefix(p, uSep)
}

// buildScopePrefixes precomputes the prefix forms resolve() derived. It runs
// once per resolution; every per-file matcher below reuses them.
func (r *contentScopeResolved) buildScopePrefixes() {
	r.repoPrefix = r.repoPrefix[:0]
	for _, root := range r.Repos {
		r.repoPrefix = append(r.repoPrefix, contentScopePrefix(root))
	}
	r.rootPrefix = r.rootPrefix[:0]
	for _, root := range r.Roots {
		r.rootPrefix = append(r.rootPrefix, contentScopePrefix(root))
	}
	r.excludePrefix = r.excludePrefix[:0]
	for _, ex := range r.Excludes {
		r.excludePrefix = append(r.excludePrefix, contentScopePrefix(ex))
	}
	r.explicitPrefix = r.explicitPrefix[:0]
	for _, root := range r.ExplicitRoots {
		r.explicitPrefix = append(r.explicitPrefix, contentScopePrefix(root))
	}
	// Rebuilt unconditionally (not merged): resolve() runs on a fresh struct
	// each time, and a reused struct must not retain removed roots.
	r.rootExact = make(map[string]string, len(r.Roots))
	for _, root := range r.Roots {
		r.rootExact[root] = root
	}
	r.explicitSet = make(map[string]struct{}, len(r.ExplicitRoots))
	for _, root := range r.ExplicitRoots {
		r.explicitSet[root] = struct{}{}
	}
	if r.DriveRoot != "" {
		r.driveRootPrefix = contentScopePrefix(r.DriveRoot)
	} else {
		r.driveRootPrefix = ""
	}
}

// explicitPrefixAt is the ExplicitRoots counterpart of repoPrefixAt.
func (r contentScopeResolved) explicitPrefixAt(i int) string {
	if i < len(r.explicitPrefix) {
		return r.explicitPrefix[i]
	}
	return contentScopePrefix(r.ExplicitRoots[i])
}

// matchRepo returns the deepest worktree containing normalized path p.
// Repos resolve deepest-first, so the first prefix hit wins — the same rule
// as contentGitIgnore.repoFor over the same list.
func (r *contentScopeResolved) matchRepo(p string) (string, bool) {
	for i, root := range r.Repos {
		if contentPrefixUnder(p, root, r.repoPrefixAt(i)) {
			return root, true
		}
	}
	return "", false
}

// matchRoot returns the include root containing normalized path p, preferring
// repos exactly like allows(): a repo hit wins, else the first root hit.
// Equality is covered by the prefix check (a root naming the file itself
// matches even when its parent matches no root).
func (r *contentScopeResolved) matchRoot(p string) (unit, kind string, ok bool) {
	for i, repo := range r.Repos {
		if contentPrefixUnder(p, repo, r.repoPrefixAt(i)) {
			return repo, "repo", true
		}
	}
	for i, root := range r.Roots {
		if contentPrefixUnder(p, root, r.rootPrefixAt(i)) {
			return root, "root", true
		}
	}
	return "", "", false
}

// matchExcluded reports whether normalized path p falls under any exclude
// prefix (separator-boundary, like contentPathUnder).
func (r *contentScopeResolved) matchExcluded(p string) bool {
	for i, ex := range r.Excludes {
		if contentPrefixUnder(p, ex, r.excludePrefixAt(i)) {
			return true
		}
	}
	return false
}

// excludePrefixAt is the Excludes counterpart of repoPrefixAt.
func (r *contentScopeResolved) excludePrefixAt(i int) string {
	if i < len(r.excludePrefix) {
		return r.excludePrefix[i]
	}
	return contentScopePrefix(r.Excludes[i])
}

// resolve computes the effective roots/excludes. repoRoots are the detected git
// worktree roots (contentDetectGitRepos).
func (s contentScope) resolve(volume string, repoRoots []string) contentScopeResolved {
	r := contentScopeResolved{Mode: s.Mode, BudgetBytes: s.BudgetBytes, BudgetExplicit: s.BudgetExplicit, SystemExcludes: s.SystemExcludes, WorktreesAll: s.WorktreesAll, Skip: s.Skip}
	kept, dropped := contentFilterLinkedWorktrees(repoRoots, s.WorktreesAll)
	r.DroppedWorktrees = dropped
	repoRoots = kept
	if s.Exts != nil {
		r.ExtSet = s.Exts
	} else {
		// No explicit allowlist: sniff-based selection over the registry.
		// ExtSet still carries the registry so the fingerprint names the
		// selection, but allows() consults the skip list instead of it.
		r.ExtSet = contentServiceExtensions()
		r.SniffText = true
	}
	if s.IncludeGit {
		r.Repos = repoRoots
		r.Roots = append(r.Roots, repoRoots...)
	}
	r.Roots = append(r.Roots, s.Roots...)
	r.ExplicitRoots = dedupeContentPaths(append([]string(nil), s.Roots...))
	if s.Mode == contentScopeAuto {
		r.Roots = append(r.Roots, contentKnownFolders(volume)...)
		// The volume root is the final, lowest-priority root: it admits
		// text files outside repos and known folders (drive-wide coverage,
		// like the reference indexer) while priority() keeps the budget
		// filling repos, configured roots and known folders first. All
		// excludes stay in force, and the inclusion skips below drop
		// dependency/cache trees.
		r.DriveRoot = contentVolumeRoot(volume)
		if r.DriveRoot != "" {
			r.Roots = append(r.Roots, r.DriveRoot)
		}
	}
	r.Roots = dedupeContentPaths(r.Roots)

	r.Excludes = append(r.Excludes, s.Exclude...)
	for _, repo := range repoRoots {
		r.Excludes = append(r.Excludes, contentNormPath(filepath.Join(repo, ".git")))
	}
	if s.SystemExcludes && s.Mode == contentScopeAuto {
		vol := contentNormPath(volume + string(filepath.Separator))
		for _, d := range contentSystemExcludeVolumeDirs {
			r.Excludes = append(r.Excludes, contentNormPath(vol+d))
		}
		if home := contentHomeOnVolume(volume); home != "" {
			r.Excludes = append(r.Excludes, contentNormPath(filepath.Join(home, "AppData", "Local", "Temp")))
			// With the drive root in scope, the rest of AppData (application
			// state, extension and package caches) would come in too; it is
			// not user text, so it stays out like the system dirs.
			if r.DriveRoot != "" {
				r.Excludes = append(r.Excludes, contentNormPath(filepath.Join(home, "AppData")))
			}
		}
	}
	r.Excludes = dedupeContentPaths(r.Excludes)
	r.buildScopePrefixes()
	return r
}

// priority orders budget candidates by user intent: repositories first,
// configured roots next, then automatically detected known folders, then the
// whole-volume drive root (auto mode) last. A path matching no root at all
// ranks below every include (4); in auto mode the drive root makes that
// unreachable.
func (r contentScopeResolved) priority(path string) int {
	p := contentNormPath(path)
	for i, repo := range r.Repos {
		if contentPrefixUnder(p, repo, r.repoPrefixAt(i)) {
			return 0
		}
	}
	for i, root := range r.ExplicitRoots {
		if contentPrefixUnder(p, root, r.explicitPrefixAt(i)) {
			return 1
		}
	}
	for i, root := range r.Roots {
		if r.DriveRoot != "" && root == r.DriveRoot {
			continue
		}
		if contentPrefixUnder(p, root, r.rootPrefixAt(i)) {
			return 2
		}
	}
	if r.DriveRoot != "" && contentPrefixUnder(p, r.DriveRoot, r.driveRootPrefixAt()) {
		return 3
	}
	return 4
}

// driveRootPrefixAt is DriveRoot's precomputed separator-terminated form,
// falling back to computing it for hand-built test scopes.
func (r contentScopeResolved) driveRootPrefixAt() string {
	if r.driveRootPrefix != "" {
		return r.driveRootPrefix
	}
	return contentScopePrefix(r.DriveRoot)
}

// repoPrefixAt returns the precomputed separator-terminated form of
// Repos[i], falling back to computing it when the scope was hand-built
// without buildScopePrefixes (tests). Production scopes always precompute.
func (r contentScopeResolved) repoPrefixAt(i int) string {
	if i < len(r.repoPrefix) {
		return r.repoPrefix[i]
	}
	return contentScopePrefix(r.Repos[i])
}

// rootPrefixAt is the Roots counterpart of repoPrefixAt.
func (r contentScopeResolved) rootPrefixAt(i int) string {
	if i < len(r.rootPrefix) {
		return r.rootPrefix[i]
	}
	return contentScopePrefix(r.Roots[i])
}

// allows decides whether path is in scope, returning the matching include unit
// and its kind ("repo" or "root"). Both the build and the estimate use this, so
// the extension gate, excludes, and roots are applied identically. In sniff
// mode (the default) only known-binary extensions are rejected here; anything
// else is admitted and head-sniffed at build time. Size-gated rules (large
// JSON) admit when the size is unknown; pass a real size via allowsSize.
func (r contentScopeResolved) allows(path string) (unit, kind string, ok bool) {
	unit, kind, _, ok = r.allowsReason(path, -1)
	return unit, kind, ok
}

// allowsSize is allows with the source file size for the size-gated
// inclusion rules (large JSON). Callers with a stat pass it; callers
// without one use allows and get the size-agnostic decision.
func (r contentScopeResolved) allowsSize(path string, size int64) (unit, kind string, ok bool) {
	unit, kind, _, ok = r.allowsReason(path, size)
	return unit, kind, ok
}

// allowsReason is the reason-carrying form of allows: on rejection it names
// the skip reason ("exclude", "gitignore", "system", a contentSkip* code,
// "binary", "ext" or "root"). Git rules come first, then the operator's
// content-skip list, then the built-in inclusion skips — so a reason names
// the first rule that fired, in policy order.
func (r contentScopeResolved) allowsReason(path string, size int64) (unit, kind, reason string, ok bool) {
	if r.Mode == contentScopeOff {
		return "", "", "exclude", false
	}
	p := contentNormPath(path)
	if p == "" {
		return "", "", "exclude", false
	}
	ext := contentPathExtension(p)
	if r.SniffText {
		if contentBinaryRejected(ext) {
			return "", "", "binary", false
		}
	} else if _, ok := r.ExtSet[ext]; !ok {
		return "", "", "ext", false
	}
	if r.matchExcluded(p) {
		return "", "", "exclude", false
	}
	_, inRepo := r.Ignore.repoFor(p)
	if inRepo {
		// Inside a worktree git decides: tracked "build" or "vendor"
		// directories are source, while ignored output is skipped. The
		// original-case path drives pattern matching (case-sensitive,
		// like git and ripgrep); p is only the normalized lookup key.
		if r.Ignore.ignored(path, false) {
			return "", "", "gitignore", false
		}
	}
	base := p
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		base = p[i+1:]
	}
	// The operator list and the built-in inclusion skips run before the
	// legacy system segments, so a specific reason (user_skip, lockfile,
	// build_output, cache, ...) wins over the generic "system" bucket.
	// The estimate's legacy baseline skips this block entirely.
	if !r.InclusionOff {
		if reason, skip := contentInclusionSkip(p, base, ext, size, inRepo, r.SystemExcludes, r.Skip); skip {
			return "", "", reason, false
		}
	}
	if !inRepo && r.SystemExcludes && contentPathHasExcludedSegment(p, contentSystemExcludeSegments) {
		return "", "", "system", false
	}
	unit, kind, ok = r.matchRoot(p)
	if !ok {
		return "", "", "root", false
	}
	return unit, kind, "", true
}

// contentVolumeRoot returns the normalized drive-root include for volume
// ("C:" -> "c:\"), or "" when the volume names no drive root (auto mode's
// whole-volume coverage). Only a bare drive ("C:") or its root form qualifies;
// a deeper path volume is left alone so a scoped path never silently widens to
// the whole drive.
func contentVolumeRoot(volume string) string {
	v := strings.TrimSpace(volume)
	if len(v) < 2 || v[1] != ':' {
		return ""
	}
	if len(v) > 2 {
		rest := v[2:]
		if rest != string(filepath.Separator) && rest != "/" {
			return ""
		}
	}
	return contentNormPath(v[:2] + string(filepath.Separator))
}

// contentKnownFolders returns the user's Documents/Desktop/Downloads when the
// home directory is on the given volume (auto mode only).
func contentKnownFolders(volume string) []string {
	home := contentHomeOnVolume(volume)
	if home == "" {
		return nil
	}
	out := make([]string, 0, 3)
	for _, sub := range []string{"Documents", "Desktop", "Downloads"} {
		out = append(out, filepath.Join(home, sub))
	}
	return normalizeContentPaths(out)
}

// contentHomeOnVolume returns the user home directory when it is on volume.
func contentHomeOnVolume(volume string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || len(volume) < 2 {
		return ""
	}
	if len(home) < 2 || !strings.EqualFold(home[:2], volume[:2]) {
		return ""
	}
	return home
}

// contentPathHasExcludedSegment reports whether any non-final path segment is in
// segs. The basename is skipped so a file named like a generated directory is
// not excluded.
func contentPathHasExcludedSegment(p string, segs []string) bool {
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '\\' || r == '/' })
	for i := 0; i+1 < len(parts); i++ {
		for _, s := range segs {
			if parts[i] == s {
				return true
			}
		}
	}
	return false
}

func dedupeContentPaths(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, p := range in {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
