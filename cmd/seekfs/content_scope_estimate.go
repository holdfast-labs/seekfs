package main

// Scope estimation (WP2): derive from the filename index, without extracting
// anything, which files a scoped content build would consider — counts, bytes,
// per-extension and per-unit histograms, detected git repos, and a projected
// `.gsx` size — plus the git worktree detection the scope resolve depends on.

import (
	"container/heap"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
)

type contentEstimateCandidate struct {
	item            contentBuildItem
	unit, kind, ext string
	bytes           int64
	size            int64 // uncapped source bytes (for same-size grouping)
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
	Files    int64              `json:"files"`
	Bytes    int64              `json:"bytes"` // Σ min(size, per-file text cap)
	ByExt    map[string]int64   `json:"by_ext,omitempty"`
	ByUnit   []contentScopeUnit `json:"by_unit,omitempty"`
	Repos    []string           `json:"repos,omitempty"`
	Excluded map[string]int64   `json:"excluded,omitempty"` // reason -> files
	// ExcludedBytes maps the same reasons to source bytes, so every policy
	// decision is visible as files and bytes (see §6z).
	ExcludedBytes map[string]int64 `json:"excluded_bytes,omitempty"`
	// BaselineFiles/BaselineBytes are the legacy selection (every linked
	// worktree, no inclusion skips, the historical 16 MiB text cap) over
	// the same roots: before vs after in one run.
	BaselineFiles int64 `json:"baseline_files,omitempty"`
	BaselineBytes int64 `json:"baseline_bytes,omitempty"`
	// SkippedWorktrees are the linked worktrees the main-only policy left
	// out (empty with worktrees=all).
	SkippedWorktrees []string `json:"skipped_worktrees,omitempty"`
	// SameSizeFiles/SameSizeBytes count selected files sharing their size
	// with another selected file: the dedupe candidate pool (an upper
	// bound measured without reading bytes; the build reports exact
	// dedupe savings in its CXIS section).
	SameSizeFiles int64 `json:"same_size_files,omitempty"`
	SameSizeBytes int64 `json:"same_size_bytes,omitempty"`
	MaxFilesHit   bool  `json:"max_files_hit,omitempty"`
	BudgetHit     bool  `json:"budget_hit,omitempty"`
	BudgetBytes   int64 `json:"budget_bytes"`
	// EligibleFiles/EligibleBytes are the pre-budget totals over the whole
	// candidate set, so a budget_hit names exactly what was cut: eligible
	// files were considered, files fit. (When MaxFilesHit, both are over the
	// priority-truncated set, like Files/Bytes.)
	EligibleFiles int64 `json:"eligible_files"`
	EligibleBytes int64 `json:"eligible_bytes"`
	// Paths holds every selected (post-budget) path when the estimate was
	// asked to list them (--paths), for set reconciliation against ripgrep.
	Paths []string `json:"paths,omitempty"`
	// TextCandidates counts eligible files no registered extractor claims by
	// extension (unknown or missing): the build opens and head-sniffs them,
	// keeping the text ones like ripgrep does. BinaryCandidates counts the
	// cheap rejections (known-binary extensions); the build never opens those.
	TextCandidates   int64 `json:"text_candidates"`
	BinaryCandidates int64 `json:"binary_candidates,omitempty"`
	// SampledFiles/SampledBinary report an opt-in head-sniff sample over the
	// text candidates (--sniff-sample N): how many were opened and how many
	// sniffed binary. ProjectedTextFiles subtracts the projected binary share
	// from Files. Zero when sampling is off.
	SampledFiles       int   `json:"sampled_files,omitempty"`
	SampledBinary      int   `json:"sampled_binary,omitempty"`
	ProjectedTextFiles int64 `json:"projected_text_files,omitempty"`

	ProjectedGSXBytes int64 `json:"projected_gsx_bytes"`
}

// contentDetectGitRepos returns the normalized worktree roots of every `.git`
// entry in the index. A `.git` directory and a `.git` file (worktree/submodule
// pointer) both mark a repo root; paths are reconstructed only for markers.
func contentDetectGitRepos(idx *Index) []string {
	repos, _ := contentDetectGitLayout(idx)
	return repos
}

// contentDetectGitLayout returns the worktree roots plus, per root, the
// .gitignore files the index holds under it (attributed to the deepest
// root), so the ignore digest needs no filesystem walk.
func contentDetectGitLayout(idx *Index) ([]string, map[string][]string) {
	if idx == nil {
		return nil, nil
	}
	count := idx.compactRecordCount()
	cache := make(map[int]string, 256)
	seen := make(map[string]struct{})
	var out, ignoreFiles []string
	for id := 0; id < count; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted || rec.FRN == 0 {
			continue
		}
		isIgnore := rec.Mode&uint32(os.ModeDir) == 0 && strings.EqualFold(rec.Name, ".gitignore")
		if !isIgnore && !contentGitMarkerName(rec.Name) {
			continue
		}
		path := idx.reconstructCompactPathCached(id, cache)
		if path == "" {
			continue
		}
		if isIgnore {
			ignoreFiles = append(ignoreFiles, path)
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
	byRepo := make(map[string][]string, len(out))
	if len(out) > 0 && len(ignoreFiles) > 0 {
		lookup := newContentGitIgnore(out)
		for _, path := range ignoreFiles {
			if root, ok := lookup.repoFor(contentNormPath(path)); ok {
				byRepo[root] = append(byRepo[root], path)
			}
		}
	}
	return out, byRepo
}

// resolveForIndex resolves the scope against the worktrees in idx and
// attaches their git ignore rules. It also returns every detected worktree,
// including ones the scope leaves out (git = false).
func (s contentScope) resolveForIndex(volume string, idx *Index) (contentScopeResolved, []string) {
	repos, ignoreFiles := contentDetectGitLayout(idx)
	r := s.resolve(volume, repos)
	r.attachGitIgnore(ignoreFiles)
	return r, repos
}

// resolveForIndexWithIgnoreFiles is resolveForIndex plus the detected
// ignore-file layout, so a second scope over the same index (the estimate's
// legacy baseline) attaches identical git rules.
func (s contentScope) resolveForIndexWithIgnoreFiles(volume string, idx *Index) (contentScopeResolved, []string, map[string][]string) {
	repos, ignoreFiles := contentDetectGitLayout(idx)
	r := s.resolve(volume, repos)
	r.attachGitIgnore(ignoreFiles)
	return r, repos, ignoreFiles
}

// attachGitIgnore enables git ignore rules for the resolved worktrees. It
// stats the ignore files, so callers holding index locks should detect the
// layout under the lock and attach after releasing it. The global excludes
// paths are resolved per repo here so the digest covers them.
func (r *contentScopeResolved) attachGitIgnore(ignoreFiles map[string][]string) {
	if len(r.Repos) == 0 {
		return
	}
	r.Ignore = newContentGitIgnore(r.Repos)
	r.IgnoreDigest = contentIgnoreDigest(r.Repos, ignoreFiles, r.Ignore.resolveGlobals())
}

// scanContentScope estimates what a scoped build would index. maxFiles mirrors
// contentBuildOptions.MaxFiles (0 = unbounded); reaching it sets MaxFilesHit.
// sniffSample opens up to that many text-candidate heads to project the binary
// share (0 = no sampling; the estimate otherwise never touches the filesystem).
// listPaths records every selected (post-budget) path in est.Paths.
func scanContentScope(idx *Index, scope contentScope, volume string, maxFiles int, sniffSample int, listPaths bool) contentEstimate {
	est := contentEstimate{
		ByExt:         make(map[string]int64),
		Excluded:      make(map[string]int64),
		ExcludedBytes: make(map[string]int64),
	}
	if idx == nil {
		return est
	}
	resolved, repos, ignoreFiles := scope.resolveForIndexWithIgnoreFiles(volume, idx)
	est.Repos = repos
	est.SkippedWorktrees = resolved.DroppedWorktrees
	// The legacy baseline resolves the same scope with the inclusion
	// policy off (every worktree kept, no skip rules, the historical
	// 16 MiB cap), so one run reports before vs after. It attaches the
	// same ignore layout, or in-repo membership (and the per-file caps
	// the estimate projects) would diverge by construction.
	legacyScope := scope
	legacyScope.WorktreesAll = true
	legacyScope.Skip = nil
	legacyResolved := legacyScope.resolve(volume, repos)
	legacyResolved.Skip = nil
	legacyResolved.InclusionOff = true
	legacyResolved.attachGitIgnore(ignoreFiles)
	exclude := func(reason string, bytes int64) {
		if reason == "" {
			reason = "exclude"
		}
		est.Excluded[reason]++
		est.ExcludedBytes[reason] += bytes
	}

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
		size := rec.Size
		if size < 0 {
			size = 0
		}
		ext := strings.ToLower(contentPathExtension(rec.Name))
		if resolved.SniffText {
			// Cheap binary rejections are tallied apart from scope
			// exclusions; the build never opens them.
			if contentBinaryRejected(ext) {
				exclude("binary", size)
				est.BinaryCandidates++
				continue
			}
		} else if _, ok := resolved.ExtSet[ext]; !ok {
			exclude("ext", size)
			continue
		}
		path := idx.reconstructCompactPathCached(id, cache)
		if path == "" {
			exclude("path", size)
			continue
		}
		// Legacy baseline tally (no inclusion skips, every worktree).
		if _, _, ok := legacyResolved.allows(path); ok {
			n := size
			if n > contentExtractMaxTextBytes {
				n = contentExtractMaxTextBytes
			}
			est.BaselineFiles++
			est.BaselineBytes += n
		}
		unit, kind, reason, ok := resolved.allowsReason(path, size)
		if !ok {
			exclude(reason, size)
			continue
		}
		// Per-file text cap: ~1 MiB outside repos, ~10 MiB inside.
		_, inRepo := resolved.Ignore.repoFor(contentNormPath(path))
		n := size
		if cap := contentInclusionTextCap(inRepo); n > cap {
			n = cap
		}
		c := contentEstimateCandidate{item: contentBuildItem{frn: rec.FRN, path: path, priority: resolved.priority(path)}, unit: unit, kind: kind, ext: ext, bytes: n, size: size}
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
	est.BudgetBytes = resolved.BudgetBytes
	eligibleSizes := make(map[int64]int64)
	for _, c := range candidates {
		est.EligibleFiles++
		est.EligibleBytes += c.bytes
		eligibleSizes[c.size]++
		if !contentKnownTextExt(c.ext) {
			est.TextCandidates++
		}
	}
	// Same-size grouping over the eligible files: the dedupe candidate
	// pool (an upper bound measured without reading bytes). Computed
	// pre-budget so a reserve-truncated run still reports it.
	for s, n := range eligibleSizes {
		if n > 1 {
			est.SameSizeFiles += n
			est.SameSizeBytes += s * n
		}
	}
	// An explicit budget truncates the priority-ordered candidates at the
	// byte cap. The default (BudgetBytes 0, non-explicit) truncates only
	// when the projected index would breach the volume's free-space
	// reserve; the anchor is the volume root, the same volume the sidecar
	// is written to.
	anchor := contentScopeBudgetAnchor(volume)
	for _, c := range candidates {
		if resolved.BudgetExplicit {
			if est.Bytes+c.bytes > resolved.BudgetBytes {
				est.BudgetHit = true
				break
			}
		} else if !contentScopeFitsReserve(anchor, est.Bytes+c.bytes) {
			est.BudgetHit = true
			break
		}
		est.Files++
		est.Bytes += c.bytes
		if listPaths {
			est.Paths = append(est.Paths, c.item.path)
		}
		est.ByExt[c.ext]++
		if i, seen := unitIndex[c.unit]; seen {
			est.ByUnit[i].Files++
			est.ByUnit[i].Bytes += c.bytes
		} else {
			unitIndex[c.unit] = len(est.ByUnit)
			est.ByUnit = append(est.ByUnit, contentScopeUnit{Root: c.unit, Kind: c.kind, Files: 1, Bytes: c.bytes})
		}
	}
	est.ProjectedGSXBytes = int64(float64(est.Bytes) * contentScopeEncodedMultiplier)
	if sniffSample > 0 {
		est.applySniffSample(candidates, sniffSample)
	}
	return est
}

// contentKnownTextExt reports whether ext is claimed by a registered extractor
// or is in the curated text list: files the build extracts without depending
// on the head sniff. Anything else admitted is a text candidate.
func contentKnownTextExt(ext string) bool {
	if contentRegistryClaimsExt(ext) {
		return true
	}
	for _, want := range contentDefaultTextExtensions {
		if want == ext {
			return true
		}
	}
	return false
}

// applySniffSample stride-samples up to n text-candidate heads for NUL bytes
// (the same predicate the build applies) and projects the binary share onto
// all text candidates: ProjectedTextFiles is the indexable total the build
// should land near. It opens files, so the estimate calls it only for an
// explicit --sniff-sample.
func (est *contentEstimate) applySniffSample(candidates contentEstimateHeap, n int) {
	var paths []string
	for _, c := range candidates {
		if !contentKnownTextExt(c.ext) {
			paths = append(paths, c.item.path)
		}
	}
	if len(paths) == 0 {
		return
	}
	if n > len(paths) {
		n = len(paths)
	}
	step := len(paths) / n
	if step < 1 {
		step = 1
	}
	sampled, binary := 0, 0
	for i := 0; i < len(paths) && sampled < n; i += step {
		head, err := contentReadHead(paths[i], 512)
		if err != nil {
			continue
		}
		sampled++
		if contentExtractorForPath(paths[i], head) == nil {
			binary++
		}
	}
	if sampled == 0 {
		return
	}
	est.SampledFiles = sampled
	est.SampledBinary = binary
	projectedBinary := int64(float64(len(paths)) * float64(binary) / float64(sampled))
	est.ProjectedTextFiles = est.Files - projectedBinary
	if est.ProjectedTextFiles < 0 {
		est.ProjectedTextFiles = 0
	}
}

// contentReadHead reads up to max head bytes of path for sniffing.
func contentReadHead(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, max)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}
