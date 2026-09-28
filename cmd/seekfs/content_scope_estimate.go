package main

// Scope estimation (WP2): derive from the filename index, without extracting
// anything, which files a scoped content build would consider — counts, bytes,
// per-extension and per-unit histograms, detected git repos, and a projected
// `.gsx` size — plus the git worktree detection the scope resolve depends on.

import (
	"os"
	"strings"
)

// contentScopeUnit is one include root's contribution to an estimate.
type contentScopeUnit struct {
	Root  string `json:"root"`
	Kind  string `json:"kind"` // "repo" | "root"
	Files int64  `json:"files"`
	Bytes int64  `json:"bytes"`
}

// contentEstimate is the pre-flight estimate for a scoped build.
type contentEstimate struct {
	Files             int64              `json:"files"`
	Bytes             int64              `json:"bytes"` // Σ min(size, contentExtractMaxTextBytes)
	ByExt             map[string]int64   `json:"by_ext,omitempty"`
	ByUnit            []contentScopeUnit `json:"by_unit,omitempty"`
	Repos             []string           `json:"repos,omitempty"`
	Excluded          map[string]int64   `json:"excluded,omitempty"` // reason -> files
	MaxFilesHit       bool               `json:"max_files_hit,omitempty"`
	ProjectedGSXBytes int64              `json:"projected_gsx_bytes"`
}

// contentDetectGitRepos returns the normalized worktree roots of every `.git`
// entry in the index. A `.git` directory and a `.git` file (worktree/submodule
// pointer) both mark a repo root; paths are reconstructed only for markers.
func contentDetectGitRepos(idx *Index) []string {
	if idx == nil {
		return nil
	}
	count := idx.compactRecordCount()
	cache := make(map[int]string, 256)
	seen := make(map[string]struct{})
	var out []string
	for id := 0; id < count; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted || rec.FRN == 0 || !contentGitMarkerName(rec.Name) {
			continue
		}
		path := idx.reconstructCompactPathCached(id, cache)
		if path == "" {
			continue
		}
		m := contentNewGitMarker(path, rec.Mode&uint32(os.ModeDir) != 0)
		if m.Root == "" {
			continue
		}
		if _, ok := seen[m.Root]; ok {
			continue
		}
		seen[m.Root] = struct{}{}
		out = append(out, m.Root)
	}
	return out
}

// scanContentScope estimates what a scoped build would index. maxFiles mirrors
// contentBuildOptions.MaxFiles (0 = unbounded); reaching it sets MaxFilesHit.
func scanContentScope(idx *Index, scope contentScope, volume string, maxFiles int) contentEstimate {
	est := contentEstimate{
		ByExt:    make(map[string]int64),
		Excluded: make(map[string]int64),
	}
	if idx == nil {
		return est
	}
	repos := contentDetectGitRepos(idx)
	resolved := scope.resolve(volume, repos)
	est.Repos = repos

	count := idx.compactRecordCount()
	cache := make(map[int]string, 1024)
	unitIndex := make(map[string]int)
	for id := 0; id < count; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted || rec.FRN == 0 || rec.Mode&uint32(os.ModeDir) != 0 {
			continue
		}
		if contentGitMarkerName(rec.Name) {
			continue // already counted as a repo marker
		}
		ext := strings.ToLower(contentPathExtension(rec.Name))
		if _, ok := resolved.ExtSet[ext]; !ok {
			est.Excluded["ext"]++
			continue
		}
		path := idx.reconstructCompactPathCached(id, cache)
		if path == "" {
			est.Excluded["path"]++
			continue
		}
		unit, kind, ok := resolved.allows(path)
		if !ok {
			est.Excluded["exclude"]++
			continue
		}
		if maxFiles > 0 && est.Files >= int64(maxFiles) {
			est.MaxFilesHit = true
			break
		}
		n := rec.Size
		if n < 0 {
			n = 0
		}
		if n > contentExtractMaxTextBytes {
			n = contentExtractMaxTextBytes
		}
		est.Files++
		est.Bytes += n
		est.ByExt[ext]++
		if i, seen := unitIndex[unit]; seen {
			est.ByUnit[i].Files++
			est.ByUnit[i].Bytes += n
		} else {
			unitIndex[unit] = len(est.ByUnit)
			est.ByUnit = append(est.ByUnit, contentScopeUnit{Root: unit, Kind: kind, Files: 1, Bytes: n})
		}
	}
	est.ProjectedGSXBytes = int64(float64(est.Bytes) * contentGSXOverheadMultiplier)
	return est
}
