package main

// Scope estimation (WP2): derive from the filename index, without extracting
// anything, which files a scoped content build would consider — counts, bytes,
// per-extension and per-unit histograms, detected git repos, and a projected
// `.gsx` size — plus the git worktree detection the scope resolve depends on.

import (
	"container/heap"
	"os"
	"sort"
	"strings"
)

type contentEstimateCandidate struct {
	item            contentBuildItem
	unit, kind, ext string
	bytes           int64
}

type contentEstimateHeap []contentEstimateCandidate

func (h contentEstimateHeap) Len() int           { return len(h) }
func (h contentEstimateHeap) Less(i, j int) bool { return contentBuildItemLess(h[j].item, h[i].item) }
func (h contentEstimateHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *contentEstimateHeap) Push(x any)        { *h = append(*h, x.(contentEstimateCandidate)) }
func (h *contentEstimateHeap) Pop() any          { n := len(*h) - 1; x := (*h)[n]; *h = (*h)[:n]; return x }

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
	BudgetHit         bool               `json:"budget_hit,omitempty"`
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
	candidates := make(contentEstimateHeap, 0, 1024)
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
		n := rec.Size
		if n < 0 {
			n = 0
		}
		if n > contentExtractMaxTextBytes {
			n = contentExtractMaxTextBytes
		}
		c := contentEstimateCandidate{item: contentBuildItem{frn: rec.FRN, path: path, priority: resolved.priority(path)}, unit: unit, kind: kind, ext: ext, bytes: n}
		if maxFiles <= 0 || len(candidates) < maxFiles {
			candidates = append(candidates, c)
			continue
		}
		if !est.MaxFilesHit {
			heap.Init(&candidates)
			est.MaxFilesHit = true
		}
		if contentBuildItemLess(c.item, candidates[0].item) {
			candidates[0] = c
			heap.Fix(&candidates, 0)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return contentBuildItemLess(candidates[i].item, candidates[j].item)
	})
	unitIndex := make(map[string]int)
	for _, c := range candidates {
		if est.Bytes+c.bytes > resolved.BudgetBytes {
			est.BudgetHit = true
			break
		}
		est.Files++
		est.Bytes += c.bytes
		est.ByExt[c.ext]++
		if i, seen := unitIndex[c.unit]; seen {
			est.ByUnit[i].Files++
			est.ByUnit[i].Bytes += c.bytes
		} else {
			unitIndex[c.unit] = len(est.ByUnit)
			est.ByUnit = append(est.ByUnit, contentScopeUnit{Root: c.unit, Kind: c.kind, Files: 1, Bytes: c.bytes})
		}
	}
	est.ProjectedGSXBytes = int64(float64(est.Bytes) * contentGSXOverheadMultiplier)
	return est
}
