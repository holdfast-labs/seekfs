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

// contentScopeResolved is a scope with its effective roots and excludes for one
// volume plus the detected git worktrees.
type contentScopeResolved struct {
	Mode           contentScopeMode
	Roots          []string // effective include roots (normalized)
	Repos          []string // detected git worktree roots (normalized)
	Excludes       []string // normalized exclude prefixes
	ExtSet         map[string]struct{}
	BudgetBytes    int64
	SystemExcludes bool
}

// resolve computes the effective roots/excludes. repoRoots are the detected git
// worktree roots (contentDetectGitRepos).
func (s contentScope) resolve(volume string, repoRoots []string) contentScopeResolved {
	r := contentScopeResolved{Mode: s.Mode, BudgetBytes: s.BudgetBytes, SystemExcludes: s.SystemExcludes}
	if s.Exts != nil {
		r.ExtSet = s.Exts
	} else {
		r.ExtSet = contentServiceExtensions()
	}
	if s.IncludeGit {
		r.Repos = repoRoots
		r.Roots = append(r.Roots, repoRoots...)
	}
	r.Roots = append(r.Roots, s.Roots...)
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

// allows decides whether path is in scope, returning the matching include unit
// and its kind ("repo" or "root"). Both the build and the estimate use this, so
// the extension gate, excludes, and roots are applied identically.
func (r contentScopeResolved) allows(path string) (unit, kind string, ok bool) {
	if r.Mode == contentScopeOff {
		return "", "", false
	}
	p := contentNormPath(path)
	if p == "" {
		return "", "", false
	}
	if _, ok := r.ExtSet[contentPathExtension(p)]; !ok {
		return "", "", false
	}
	for _, ex := range r.Excludes {
		if contentPathUnder(p, ex) {
			return "", "", false
		}
	}
	if r.SystemExcludes && contentPathHasExcludedSegment(p, contentSystemExcludeSegments) {
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
