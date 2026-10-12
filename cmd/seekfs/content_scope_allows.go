package main

// Scope matching (WP3): turn a contentScope plus the git worktrees detected in
// an index into the effective include roots / excludes / extension set, and
// decide membership for a path. The build, the estimate, and the USN drain all
// route through contentScopeResolved.allows so they agree exactly.

import (
	"os"
	"path/filepath"
	"strings"
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

// contentScopeResolved is a scope with its effective roots and excludes for one
// volume plus the detected git worktrees.
type contentScopeResolved struct {
	Mode          contentScopeMode
	Roots         []string // effective include roots (normalized)
	Repos         []string // detected git worktree roots (normalized)
	ExplicitRoots []string // configured roots, before known folders are added
	Excludes      []string // normalized exclude prefixes
	ExtSet        map[string]struct{}
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
	// Ignore applies git ignore rules inside detected worktrees in place of
	// the name-based system excludes; IgnoreDigest fingerprints the ignore
	// files it reads so a sidecar built under other rules is rebuilt.
	Ignore       *contentGitIgnore
	IgnoreDigest [32]byte
}

// resolve computes the effective roots/excludes. repoRoots are the detected git
// worktree roots (contentDetectGitRepos).
func (s contentScope) resolve(volume string, repoRoots []string) contentScopeResolved {
	r := contentScopeResolved{Mode: s.Mode, BudgetBytes: s.BudgetBytes, BudgetExplicit: s.BudgetExplicit, SystemExcludes: s.SystemExcludes}
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
		}
	}
	r.Excludes = dedupeContentPaths(r.Excludes)
	return r
}

// priority orders budget candidates by user intent: repositories first,
// configured roots next, then automatically detected known folders.
func (r contentScopeResolved) priority(path string) int {
	p := contentNormPath(path)
	for _, repo := range r.Repos {
		if contentPathUnder(p, repo) {
			return 0
		}
	}
	for _, root := range r.ExplicitRoots {
		if contentPathUnder(p, root) {
			return 1
		}
	}
	for _, root := range r.Roots {
		if contentPathUnder(p, root) {
			return 2
		}
	}
	return 3
}

// allows decides whether path is in scope, returning the matching include unit
// and its kind ("repo" or "root"). Both the build and the estimate use this, so
// the extension gate, excludes, and roots are applied identically. In sniff
// mode (the default) only known-binary extensions are rejected here; anything
// else is admitted and head-sniffed at build time.
func (r contentScopeResolved) allows(path string) (unit, kind string, ok bool) {
	if r.Mode == contentScopeOff {
		return "", "", false
	}
	p := contentNormPath(path)
	if p == "" {
		return "", "", false
	}
	ext := contentPathExtension(p)
	if r.SniffText {
		if contentBinaryExtension(ext) && !contentRegistryClaimsExt(ext) {
			return "", "", false
		}
	} else if _, ok := r.ExtSet[ext]; !ok {
		return "", "", false
	}
	for _, ex := range r.Excludes {
		if contentPathUnder(p, ex) {
			return "", "", false
		}
	}
	if _, inRepo := r.Ignore.repoFor(p); inRepo {
		// Inside a worktree git decides: tracked "build" or "vendor"
		// directories are source, while ignored output is skipped. The
		// original-case path drives pattern matching (case-sensitive,
		// like git and ripgrep); p is only the normalized lookup key.
		if r.Ignore.ignored(path, false) {
			return "", "", false
		}
	} else if r.SystemExcludes && contentPathHasExcludedSegment(p, contentSystemExcludeSegments) {
		return "", "", false
	}
	for _, repo := range r.Repos {
		if contentPathUnder(p, repo) {
			return repo, "repo", true
		}
	}
	for _, root := range r.Roots {
		if contentPathUnder(p, root) {
			return root, "root", true
		}
	}
	return "", "", false
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
