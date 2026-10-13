package main

// Fast scope selection scan: the metadata pass every service-owned content
// build runs over the whole filename index (and every scope-reconcile diff).
// The legacy flat loop paid per-file path reconstruction plus a full
// scope.allows() — each contentPathUnder cleaning and folding both sides, and
// a gitignore worktree lookup over every repo — for ~185µs/record on a large
// dev volume (4.5 minutes for 1.45M records, all of it reported as
// "unavailable").
//
// This scan keeps the exact admitted SET while cutting the per-file work:
//   - scope prefix lists (see buildScopePrefixes) replace every per-file
//     Clean/ToLower repetition with HasPrefix checks;
//   - a per-parent decision memo answers repo/excluded/segment/ignored/root
//     membership for a file from its parent directory's already-computed
//     decision, so whole excluded or ignored subtrees cost one map lookup
//     per file and no path building at all;
//   - paths still come from reconstructCompactPathCached (memoized per id),
//     so byte-level path identity with the legacy scan is preserved;
//   - record ranges split across workers, each with private memo, path
//     cache and gitignore clone, so no mutex sits in the hot loop. The
//     admitted set is worker-count independent (downstream sorts or
//     heap-merges); only the visit order varies.
//
// A contentScopeScanDifferential test binds the fast scan to allows() +
// priority() on randomized trees, so the two can never silently diverge.

import (
	"container/heap"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

// contentScanWorkers bounds selection-scan parallelism.
var contentScanWorkers = func() int {
	n := runtime.NumCPU()
	if g := runtime.GOMAXPROCS(0); g >= 1 && g < n {
		n = g
	}
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}()

// contentScanMinParallel is the record count above which the scan splits
// across workers. Below it one worker runs the same code, keeping
// small-corpus behavior (and tests) on the deterministic path.
const contentScanMinParallel = 65536

// contentScanMemoMax bounds one worker's parent-decision memo. Past it the
// memo resets and directories recompute: exactness never depends on the memo,
// only speed, so eviction degrades gracefully on huge trees.
const contentScanMemoMax = 1 << 16

// contentParentDecision is the scope decision for one directory record,
// inherited by its children. Every field is a pure function of the
// directory's normalized path (plus the scope), so a file's decision follows
// from its parent's without rebuilding or rematching anything hierarchical.
type contentParentDecision struct {
	repo     string // deepest worktree containing the dir, "" if none
	excluded bool   // under an exclude prefix (whole subtree out)
	segHit   bool   // an excluded segment on the dir path (non-final for children)
	ignored  bool   // git-ignored (whole subtree dead)
	unit     string // matched include root, "" if none
	kind     string // "repo" or "root"
	priority int    // scope priority for admitted files below
	computed bool
}

// contentScopeMember is one admitted file: everything the build snapshot and
// the scope-reconcile diff need, with no further index reads.
type contentScopeMember struct {
	id       int
	frn      uint64 // record FRN, or the stable path-hash key on walk volumes
	path     string
	size     int64
	mod      int64
	priority int
}

// contentScopeScanConfig bundles one selection scan.
type contentScopeScanConfig struct {
	scope  *contentScopeResolved
	idx    *Index
	isWalk bool
	// maxFiles caps admitted members per the build's file cap (<= 0 means
	// unbounded): each worker keeps a local top-maxFiles heap and the merge
	// keeps the global top, exactly like the legacy single-heap scan.
	maxFiles int
	// prog reports (records scanned, total); nil disables. Called under no
	// service lock; implementations must be thread-safe.
	prog func(scanned, total int)
	// stopped reports service shutdown; checked per batch.
	stopped func() bool
	// genChanged reports whether the base moved under the scan (same index
	// pointer, generation and journal); checked per batch under indexMu.
	genChanged func() bool
}

// contentScanWorker carries one range's private scan state: no mutex sits in
// the hot loop (the gitignore clone is private; the scope prefix lists and
// extension sets are read-only).
type contentScanWorker struct {
	scope  *contentScopeResolved
	ignore *contentGitIgnore
	idx    *Index
	isWalk bool
	memo   map[int32]contentParentDecision
	cache  map[int]string
	count  int
}

// scanScopeMembers runs cfg over the index, returning one admitted-member
// list per worker (concatenate, or heap-merge when capped: the admitted SET
// is worker-count independent; only visit order varies). It returns false
// when the base moved mid-scan (caller reschedules) — per-worker lists are
// then void.
func scanScopeMembers(s *goSearchService, vol *serviceVolumeIndex, cfg contentScopeScanConfig) ([][]contentScopeMember, bool) {
	count := cfg.idx.compactRecordCount()
	workers := 1
	if count >= contentScanMinParallel && contentScanWorkers > 1 {
		workers = contentScanWorkers
	}
	type workerOut struct {
		members []contentScopeMember
		top     contentScanHeap
		limited bool
		moved   bool
	}
	out := make([]workerOut, workers)
	var scanned atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * count / workers
		end := (w + 1) * count / workers
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			ws := *cfg.scope
			var repos []string
			if cfg.scope.Ignore != nil {
				repos = cfg.scope.Ignore.repos
			}
			ws.Ignore = newContentGitIgnore(repos)
			worker := &contentScanWorker{
				scope:  &ws,
				ignore: ws.Ignore,
				idx:    cfg.idx,
				isWalk: cfg.isWalk,
				memo:   make(map[int32]contentParentDecision, 4096),
				cache:  make(map[int]string, 1024),
				count:  count,
			}
			members, top, limited, moved := worker.scanRange(s, vol, cfg, start, end, &scanned)
			out[w] = workerOut{members: members, top: top, limited: limited, moved: moved}
		}(w, start, end)
	}
	wg.Wait()
	for w := range out {
		if out[w].moved {
			return nil, false
		}
	}
	lists := make([][]contentScopeMember, workers)
	for w := range out {
		if out[w].limited {
			members := make([]contentScopeMember, 0, len(out[w].top))
			for _, h := range out[w].top {
				members = append(members, h.member)
			}
			lists[w] = members
		} else {
			// Uncapped scans append to members; capped scans that never
			// filled keep arrivals in top (heap order starts only once
			// the cap fills). Concatenating both covers either shape —
			// dropping top here would silently select nothing whenever
			// the corpus stays under the cap.
			lists[w] = out[w].members
			for _, h := range out[w].top {
				lists[w] = append(lists[w], h.member)
			}
		}
	}
	return lists, true
}

// scanRange scans [start, end), batching the generation guard exactly like
// the legacy loop (indexMu released between batches so a persist/rebuild can
// proceed). Capped scans keep a local top-maxFiles heap; uncapped scans
// append. It returns true (with void results) when the base moved.
func (w *contentScanWorker) scanRange(s *goSearchService, vol *serviceVolumeIndex, cfg contentScopeScanConfig, start, end int, scanned *atomic.Int64) (members []contentScopeMember, top contentScanHeap, limited, moved bool) {
	capped := cfg.maxFiles > 0
	for batch := start; batch < end; batch += contentBuildSnapshotBatch {
		if cfg.stopped != nil && cfg.stopped() {
			return nil, nil, false, true
		}
		bend := batch + contentBuildSnapshotBatch
		if bend > end {
			bend = end
		}
		s.indexMu.RLock()
		if cfg.genChanged != nil && cfg.genChanged() {
			s.indexMu.RUnlock()
			return nil, nil, false, true
		}
		for id := batch; id < bend; id++ {
			rec := w.idx.compactRecord(id)
			if rec.Deleted || rec.FRN == 0 || rec.Mode&uint32(os.ModeDir) != 0 {
				continue
			}
			m, ok := w.admitRecord(id, rec)
			if !ok {
				continue
			}
			if !capped {
				members = append(members, m)
				continue
			}
			it := contentBuildItem{frn: m.frn, path: m.path, priority: m.priority}
			if !limited {
				top = append(top, contentScanHeapItem{item: it, member: m})
				if len(top) >= cfg.maxFiles {
					heap.Init(&top)
					limited = true
				}
				continue
			}
			if contentBuildItemLess(it, top[0].item) {
				top[0] = contentScanHeapItem{item: it, member: m}
				heap.Fix(&top, 0)
			}
		}
		s.indexMu.RUnlock()
		if len(w.cache) > contentBuildPathCacheMax {
			w.cache = make(map[int]string, 1024)
		}
		contentBuildSnapshotBatchHook(s, vol)
		if n := scanned.Add(int64(bend - batch)); cfg.prog != nil {
			cfg.prog(int(n), w.count)
		}
	}
	return members, top, limited, false
}

// contentScanHeapItem pairs a heap-ordered build item with its full member
// for the capped merge.
type contentScanHeapItem struct {
	item   contentBuildItem
	member contentScopeMember
}

// contentScanHeap orders like contentBuildItemHeap (the worst retained
// candidate at the root), so capped scans keep the global top by the same
// rule as the legacy single-heap scan.
type contentScanHeap []contentScanHeapItem

func (h contentScanHeap) Len() int { return len(h) }

func (h contentScanHeap) Less(i, j int) bool { return contentBuildItemLess(h[j].item, h[i].item) }

func (h contentScanHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *contentScanHeap) Push(x any) { *h = append(*h, x.(contentScanHeapItem)) }

func (h *contentScanHeap) Pop() any {
	n := len(*h) - 1
	x := (*h)[n]
	*h = (*h)[:n]
	return x
}

// mergeScanMembers flattens per-worker lists into build items, preserving the
// legacy capped semantics: uncapped concatenates; capped keeps the global
// top-maxFiles by (priority, path, frn). Either way the result reports
// whether the cap truncated the corpus.
func mergeScanMembers(lists [][]contentScopeMember, maxFiles int) (items []contentBuildItem, limited bool) {
	if maxFiles <= 0 {
		for _, l := range lists {
			for _, m := range l {
				items = append(items, contentBuildItem{frn: m.frn, path: m.path, priority: m.priority})
			}
		}
		return items, false
	}
	var h contentScanHeap
	for _, l := range lists {
		for _, m := range l {
			it := contentBuildItem{frn: m.frn, path: m.path, priority: m.priority}
			if len(h) < maxFiles {
				h = append(h, contentScanHeapItem{item: it, member: m})
				if len(h) == maxFiles {
					heap.Init(&h)
				}
				continue
			}
			if contentBuildItemLess(it, h[0].item) {
				h[0] = contentScanHeapItem{item: it, member: m}
				heap.Fix(&h, 0)
			}
		}
	}
	for _, e := range h {
		items = append(items, e.item)
	}
	return items, len(h) == maxFiles
}

// admitRecord decides one file record, returning its scope member when the
// scope admits it. Directory inheritance carries every hierarchical input,
// so the per-file work is a memo lookup, an extension check, and (for
// in-repo files) the basename pattern match.
func (w *contentScanWorker) admitRecord(id int, rec CompactRecord) (contentScopeMember, bool) {
	var out contentScopeMember
	parent := rec.Parent
	dec, ok := w.memo[parent]
	if !ok {
		dec = w.dirDecision(parent)
	}
	if dec.excluded || dec.ignored {
		return out, false
	}
	// Extension gate first: it needs only the name, no path.
	ext := contentPathExtension(rec.Name)
	if w.scope.SniffText {
		if contentBinaryExtension(ext) && !contentRegistryClaimsExt(ext) {
			return out, false
		}
	} else if _, ok := w.scope.ExtSet[ext]; !ok {
		return out, false
	}
	raw := w.idx.reconstructCompactPathCached(id, w.cache)
	if raw == "" {
		return out, false
	}
	var priority int
	if dec.repo != "" {
		priority = 0
	} else if dec.unit != "" {
		priority = dec.priority
		// Outside worktrees the name-based system excludes still apply to
		// the file's own ancestry (in-repo files never consult segments —
		// git decides there).
		if w.scope.SystemExcludes && dec.segHit {
			return out, false
		}
	} else {
		// The parent matched no root: the file can only join through a
		// root naming it exactly (a root strictly inside the parent
		// cannot prefix the file without naming it — the boundary would
		// have to fall inside the file name).
		p := contentNormPath(raw)
		unit, hit := w.scope.rootExact[p]
		if !hit {
			return out, false
		}
		if _, isExplicit := w.scope.explicitSet[unit]; isExplicit {
			priority = 1
		} else {
			priority = 2
		}
	}
	if dec.repo != "" {
		// In-repo files still face the basename patterns; the directory
		// part already cleared dirIgnored via the memo.
		if w.ignore.ignoredInRepo(raw, false, dec.repo) {
			return out, false
		}
	} else if w.scope.SystemExcludes && contentPathHasExcludedSegment(contentNormPath(raw), contentSystemExcludeSegments) {
		return out, false
	}
	frn := rec.FRN
	if w.isWalk {
		frn = contentWalkKey(raw)
		if frn == 0 {
			return out, false
		}
	}
	out = contentScopeMember{id: id, frn: frn, path: raw, size: rec.Size, mod: rec.ModUnix, priority: priority}
	return out, true
}

// dirDecision computes (and memoizes) the scope decision for the directory
// record id, climbing to a memoized ancestor or a root iteratively so deep
// trees cannot overflow the stack. Invalid ids fall back to a standalone
// decision through the shared allows()/priority() semantics.
func (w *contentScanWorker) dirDecision(id int32) contentParentDecision {
	if d, ok := w.memo[id]; ok {
		return d
	}
	count := w.idx.compactRecordCount()
	if id < 0 || int(id) >= count {
		// A root record (Parent == -1) or a corrupt parent link: decide
		// standalone through the same predicates allows() applies.
		return w.dirDecisionStandalone(id)
	}
	// Climb to a memoized ancestor, a root, or the depth bound.
	var chain []int32
	cur := id
	for len(chain) < 1024 {
		if _, ok := w.memo[cur]; ok {
			break
		}
		rec := w.idx.compactRecord(int(cur))
		chain = append(chain, cur)
		p := rec.Parent
		if p < 0 || int(p) >= count || p == cur {
			break
		}
		cur = p
	}
	var base contentParentDecision
	if d, ok := w.memo[cur]; ok {
		base = d
	} else {
		base = w.dirDecisionStandalone(cur)
		w.memoize(cur, base)
	}
	// Unwind toward id, deriving each level from its parent.
	for i := len(chain) - 1; i >= 0; i-- {
		did := chain[i]
		if _, ok := w.memo[did]; ok {
			base = w.memo[did]
			continue
		}
		rec := w.idx.compactRecord(int(did))
		base = w.deriveChildDecision(base, rec.Name, did)
		w.memoize(did, base)
		if did == id {
			return base
		}
	}
	if d, ok := w.memo[id]; ok {
		return d
	}
	return w.dirDecisionStandalone(id)
}

// memoize stores a decision, bounding the memo so huge trees degrade to
// recomputation instead of unbounded growth. Exactness never depends on the
// memo, only speed.
func (w *contentScanWorker) memoize(id int32, d contentParentDecision) {
	if len(w.memo) >= contentScanMemoMax {
		w.memo = make(map[int32]contentParentDecision, 4096)
	}
	w.memo[id] = d
}

// deriveChildDecision derives directory name's decision from its parent's.
// Every hierarchical predicate is monotone in the path prefix (a child is
// under every prefix its parent is under), and the remaining checks
// (repo nesting, name segments, ignore rules) run once per directory.
func (w *contentScanWorker) deriveChildDecision(parent contentParentDecision, name string, id int32) contentParentDecision {
	raw := w.idx.reconstructCompactPathCached(int(id), w.cache)
	if raw == "" || name == "." || name == "" || strings.ContainsAny(name, `/\`) {
		// Unbuildable or reserved names fall back to the standalone
		// decision so behavior matches allows() exactly.
		return w.dirDecisionStandalone(id)
	}
	p := contentNormPath(raw)
	out := contentParentDecision{}
	// Excludes are prefixes: a child of an excluded dir is excluded, else
	// check the dir path itself.
	out.excluded = parent.excluded || w.scope.matchExcluded(p)
	// Segments: the parent's hit plus this dir's own name (non-final for
	// everything below, exactly what contentPathHasExcludedSegment tests).
	out.segHit = parent.segHit || contentSegmentExcluded(name)
	// Repo: the parent's repo still contains this dir unless a deeper repo
	// root was crossed; nested repos are rare, so confirm cheaply.
	out.repo = parent.repo
	if out.repo != "" {
		// Confirm the inherited repo still prefixes (it always does for a
		// true child, but a corrupt parent link could lie — then rescan).
		matched := false
		for i, root := range w.scope.Repos {
			if root == out.repo {
				sep := ""
				if i < len(w.scope.repoPrefix) {
					sep = w.scope.repoPrefix[i]
				} else {
					sep = contentScopePrefix(root)
				}
				matched = contentPrefixUnder(p, root, sep)
				break
			}
		}
		if !matched {
			out.repo = ""
		}
	}
	if out.repo == "" {
		if repo, ok := w.scope.matchRepo(p); ok {
			out.repo = repo
		}
	}
	// Gitignore for the dir itself (memoized by path inside the impl).
	if !out.excluded {
		out.ignored = w.ignore.ignored(raw, true)
	}
	// Root membership inherits like the repo (same prefix argument).
	out.unit, out.kind = parent.unit, parent.kind
	if out.unit != "" {
		matched := out.kind == "repo" && out.unit == out.repo
		if !matched {
			for i, root := range w.scope.Roots {
				if root == out.unit {
					sep := ""
					if i < len(w.scope.rootPrefix) {
						sep = w.scope.rootPrefix[i]
					} else {
						sep = contentScopePrefix(root)
					}
					matched = contentPrefixUnder(p, root, sep)
					break
				}
			}
		}
		if !matched {
			out.unit, out.kind = "", ""
		}
	}
	if out.unit == "" {
		if unit, kind, ok := w.scope.matchRoot(p); ok {
			out.unit, out.kind = unit, kind
		}
	}
	out.priority = w.priorityFor(out)
	return out
}

// dirDecisionStandalone decides a directory without inheritance (roots,
// corrupt links, reserved names): the same predicates allows() applies,
// evaluated on the directory's own path. Parent -1 names the volume root
// itself: flat test fixtures (and the volume's top-level records) hang
// files directly off it, so it decides as the volume directory rather than
// empty — otherwise every volume-root child would fall through to the
// rootExact lookup in admitRecord and be silently dropped, diverging from
// allows().
func (w *contentScanWorker) dirDecisionStandalone(id int32) contentParentDecision {
	var out contentParentDecision
	if id < 0 {
		if d, ok := w.memo[id]; ok {
			return d
		}
		d := w.volumeRootDecision()
		w.memoize(id, d)
		return d
	}
	if int(id) >= w.idx.compactRecordCount() {
		return out
	}
	raw := w.idx.reconstructCompactPathCached(int(id), w.cache)
	if raw == "" {
		return out
	}
	p := contentNormPath(raw)
	out.excluded = w.scope.matchExcluded(p)
	out.segHit = contentPathHasAllExcludedSegments(p)
	if repo, ok := w.scope.matchRepo(p); ok {
		out.repo = repo
	}
	if !out.excluded {
		out.ignored = w.ignore.ignored(raw, true)
	}
	if unit, kind, ok := w.scope.matchRoot(p); ok {
		out.unit, out.kind = unit, kind
	}
	out.priority = w.priorityFor(out)
	return out
}

// volumeRootDecision decides the volume directory itself (the parent of
// every Parent == -1 record): the same predicates allows() applies to any
// directory path, evaluated on the normalized volume root. Equality is
// covered by the prefix checks, so an explicit root or repo naming the
// volume directory matches exactly like it does for files below it.
func (w *contentScanWorker) volumeRootDecision() contentParentDecision {
	var out contentParentDecision
	vol := w.idx.Volume
	if vol == "" {
		return out
	}
	// Decide with a trailing separator: filepath.Clean keeps it for drive
	// roots ("C:\" stays "c:\") and strips it for plain directories, so the
	// normalized form always equals the normalized root form a root naming
	// this volume takes. Without it a drive-root volume ("C:") never prefix-
	// matches its own explicit root ("C:\") while every file below it does.
	p := contentNormPath(vol + string(filepath.Separator))
	out.excluded = w.scope.matchExcluded(p)
	out.segHit = contentPathHasAllExcludedSegments(p)
	if repo, ok := w.scope.matchRepo(p); ok {
		out.repo = repo
	}
	if !out.excluded {
		out.ignored = w.ignore.ignored(vol, true)
	}
	if unit, kind, ok := w.scope.matchRoot(p); ok {
		out.unit, out.kind = unit, kind
	}
	out.priority = w.priorityFor(out)
	return out
}

// repositories first, configured roots next, other roots last — the same
// rule as contentScopeResolved.priority.
func (w *contentScanWorker) priorityFor(d contentParentDecision) int {
	if d.repo != "" {
		return 0
	}
	if d.unit == "" {
		return 3
	}
	if _, ok := w.scope.explicitSet[d.unit]; ok {
		return 1
	}
	return 2
}

// contentSegmentExcluded reports whether a single directory name is an
// excluded segment. The name folds like the normalized path components
// contentPathHasExcludedSegment tests, so `Build` excludes exactly like
// `build` does through allows().
func contentSegmentExcluded(name string) bool {
	name = strings.ToLower(name)
	for _, s := range contentSystemExcludeSegments {
		if name == s {
			return true
		}
	}
	return false
}

// contentPathHasAllExcludedSegments reports whether ANY component of the
// normalized path is an excluded segment (unlike
// contentPathHasExcludedSegment, which spares the final component — used here
// for directory paths, whose own name is non-final for everything below).
func contentPathHasAllExcludedSegments(p string) bool {
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '\\' || r == '/' })
	for _, part := range parts {
		if contentSegmentExcluded(part) {
			return true
		}
	}
	return false
}
