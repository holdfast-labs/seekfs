package main

// Git awareness: git: filter, per-hit status, status + recent-commit boosts
// (WORK_QUEUE item 1, gap G8).
//
// No `git` CLI is ever spawned per query. Resolution is per path, backed by:
//   - the worktree's .git/index decoded with go-git (already in go.mod):
//     the tracked set, stage (conflict), blob hash, size and mtime;
//   - the HEAD tree via go-git (staged == index differs from HEAD; recent
//     commits via small tree diffs, skipping commits touching > 50 files);
//   - the existing contentGitIgnore matcher (pure Go, no CLI) for ignored.
//
// There is deliberately no whole-worktree status walk: a filter query already
// enumerates candidates through the search index, and each candidate costs
// ~one stat plus map lookups after the per-repo refresh. Refresh decodes the
// index and resolves HEAD only when .git/HEAD, .git/index, .git/packed-refs
// or the current branch ref changed since the last check (at most one stat
// batch per repo per query; the service's USN/walk change feed observes the
// same underlying .git changes, and the stat check is the portable read of
// them). Recent-commit files resolve once per HEAD, bounded by a timeout.
//
// Approximations (documented, matching the "bounded" brief):
//   - Directories never report modified/staged/conflict (those are file
//     properties; a dir prefix-matches the tracked set for tracked/clean).
//   - Worktree modification uses the index stat fast path (size or mtime
//     differs, or the file is gone); racy-smudge edge cases may differ from
//     `git status` in the same rare cases the index itself is racy.
//   - When a repo cannot be read (unsupported index version, corrupt HEAD),
//     files there resolve as unknown: positive git: predicates do not match,
//     negated ones pass (fail closed on inclusion, open on exclusion).

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// git status classes for the git: filter and the per-hit git_status field.
const (
	gitFilterModified  = "modified"
	gitFilterStaged    = "staged"
	gitFilterUntracked = "untracked"
	gitFilterIgnored   = "ignored"
	gitFilterConflict  = "conflict"
	gitFilterClean     = "clean"
	gitFilterTracked   = "tracked"
)

var validGitFilters = []string{
	gitFilterModified, gitFilterStaged, gitFilterUntracked, gitFilterIgnored,
	gitFilterConflict, gitFilterClean, gitFilterTracked,
}

// parseGitFilters parses a git: value: one class or comma-separated
// alternatives (git:modified,staged). Unknown classes are an error.
func parseGitFilters(spec string) ([]string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New(`empty git: filter; use one of modified, staged, untracked, ignored, conflict, clean, tracked (comma-separated for OR)`)
	}
	var out []string
	for _, part := range strings.Split(spec, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		valid := false
		for _, v := range validGitFilters {
			if part == v {
				valid = true
				break
			}
		}
		if !valid {
			return nil, errors.New(`unsupported git: filter ` + strconvQuote(part) + `; use one of modified, staged, untracked, ignored, conflict, clean, tracked`)
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, errors.New(`empty git: filter; use one of modified, staged, untracked, ignored, conflict, clean, tracked`)
	}
	return out, nil
}

// queryGitContext is the per-query git resolution generation: each repo
// stats its .git control files at most once per query.
type queryGitContext struct {
	id uint64
}

var queryGitContextSeq atomic.Uint64

func newQueryGitContext() *queryGitContext {
	return &queryGitContext{id: queryGitContextSeq.Add(1)}
}

// gitIndexLite is the per-path evidence kept from .git/index.
type gitIndexLite struct {
	hash     plumbing.Hash
	size     uint32
	mtime    time.Time
	unmerged bool
}

// gitRepoData is one refresh's immutable snapshot (swapped atomically; maps
// are never mutated after publication, so candidate lookups are lock-free).
type gitRepoData struct {
	index   map[string]*gitIndexLite // repo-relative slash path -> entry
	tracked []string                 // sorted relative slash paths (dir prefix checks)
	head    *object.Tree             // HEAD tree (nil when HEAD is unborn)
	headHex string
	recent  map[string]bool // repo-relative slash paths in the last commits
	ignore  *contentGitIgnore
	ok      bool // false when the refresh failed (unknown status)
}

// gitStatSig is a cheap change signature for one control file.
type gitStatSig struct {
	mtime  int64
	size   int64
	absent bool
}

func statSigFor(path string) gitStatSig {
	fi, err := os.Stat(path)
	if err != nil {
		return gitStatSig{absent: true}
	}
	return gitStatSig{mtime: fi.ModTime().UnixNano(), size: fi.Size()}
}

// gitRepoState is one worktree's cached state.
type gitRepoState struct {
	root   string // cleaned absolute worktree root
	gitDir string // cleaned absolute .git dir
	mu     sync.Mutex
	gen    uint64 // last query generation checked
	head   gitStatSig
	index  gitStatSig
	packed gitStatSig
	branch gitStatSig
	data   atomic.Pointer[gitRepoData]
	treeMu sync.Mutex // serializes HEAD tree descents (go-git object reads)
}

var (
	gitRepoCacheMu sync.Mutex
	gitRepoCache   = make(map[string]*gitRepoState)
	gitDirCacheMu  sync.Mutex
	gitDirCache    = make(map[string]gitDirCacheEntry)
)

type gitDirCacheEntry struct {
	root    string
	expires time.Time
}

// gitRepoRootForPath finds the deepest enclosing git worktree for an absolute
// path by walking up to a .git entry (directory, or a file for linked
// worktrees/submodules). Second return is false outside any worktree.
// Directory->root mappings are cached (negatives for a minute, positives
// until evicted by size).
func gitRepoRootForPath(absPath string) (string, bool) {
	dir := absPath
	if fi, err := os.Stat(absPath); err == nil && !fi.IsDir() {
		dir = filepath.Dir(absPath)
	} else if !filepath.IsAbs(dir) {
		return "", false
	}
	dir = filepath.Clean(dir)
	gitDirCacheMu.Lock()
	if e, ok := gitDirCache[dir]; ok {
		if e.root != "" || time.Now().Before(e.expires) {
			gitDirCacheMu.Unlock()
			return e.root, e.root != ""
		}
	}
	gitDirCacheMu.Unlock()
	root, ok := gitRepoRootWalk(dir)
	gitDirCacheMu.Lock()
	if len(gitDirCache) > 4096 {
		gitDirCache = make(map[string]gitDirCacheEntry)
	}
	if ok {
		gitDirCache[dir] = gitDirCacheEntry{root: root}
	} else {
		gitDirCache[dir] = gitDirCacheEntry{expires: time.Now().Add(time.Minute)}
	}
	gitDirCacheMu.Unlock()
	return root, ok
}

func gitRepoRootWalk(dir string) (string, bool) {
	cur := dir
	for {
		marker := filepath.Join(cur, ".git")
		if fi, err := os.Stat(marker); err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			return cur, true
		}
		parent := filepath.Dir(cur)
		if parent == cur || len(parent) < 3 {
			return "", false
		}
		cur = parent
	}
}

// gitRepoStateFor returns the cached state for a worktree root.
func gitRepoStateFor(root string) *gitRepoState {
	gitRepoCacheMu.Lock()
	defer gitRepoCacheMu.Unlock()
	if len(gitRepoCache) > 256 {
		gitRepoCache = make(map[string]*gitRepoState)
	}
	if s, ok := gitRepoCache[root]; ok {
		return s
	}
	s := &gitRepoState{root: root, gitDir: filepath.Join(root, ".git")}
	gitRepoCache[root] = s
	return s
}

// resetGitRepoCacheForTest drops all cached git state (tests only).
func resetGitRepoCacheForTest() {
	gitRepoCacheMu.Lock()
	gitRepoCache = make(map[string]*gitRepoState)
	gitRepoCacheMu.Unlock()
	gitDirCacheMu.Lock()
	gitDirCache = make(map[string]gitDirCacheEntry)
	gitDirCacheMu.Unlock()
}

// gitRelSlash maps an absolute path to its repo-relative slash form.
func gitRelSlash(root, absPath string) (string, bool) {
	rel, err := filepath.Rel(root, absPath)
	if err != nil || rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// gitBranchRef reads HEAD's symref target (refs/heads/<branch>) when HEAD is
// a symbolic ref; empty otherwise (detached or unborn).
func gitBranchRef(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "ref: ") {
		return ""
	}
	return filepath.FromSlash(strings.TrimPrefix(line, "ref: "))
}

// ensureFresh refreshes the repo snapshot when its .git control files
// changed since the query generation checked (at most one stat batch per
// repo per query). It never walks the worktree.
func (s *gitRepoState) ensureFresh(ctx *queryGitContext) *gitRepoData {
	var gen uint64
	if ctx != nil {
		gen = ctx.id
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx != nil && s.gen == gen {
		return s.data.Load()
	}
	s.gen = gen
	head := statSigFor(filepath.Join(s.gitDir, "HEAD"))
	indexSig := statSigFor(filepath.Join(s.gitDir, "index"))
	packed := statSigFor(filepath.Join(s.gitDir, "packed-refs"))
	var branch gitStatSig
	if ref := gitBranchRef(s.gitDir); ref != "" {
		branch = statSigFor(filepath.Join(s.gitDir, ref))
	}
	if head == s.head && indexSig == s.index && packed == s.packed && branch == s.branch {
		return s.data.Load()
	}
	s.head, s.index, s.packed, s.branch = head, indexSig, packed, branch
	data := s.refreshLocked()
	s.data.Store(data)
	return data
}

// refreshLocked rebuilds the snapshot from .git/index + HEAD (caller holds
// mu). No worktree walk: the index decode is O(tracked files) and HEAD
// resolution is O(1); recent-commit files come from small tree diffs.
func (s *gitRepoState) refreshLocked() *gitRepoData {
	data := &gitRepoData{
		index:  make(map[string]*gitIndexLite),
		ignore: newContentGitIgnore([]string{contentNormPath(s.root)}),
	}
	idx, err := decodeGitIndex(filepath.Join(s.gitDir, "index"))
	if err != nil {
		// Unreadable index (unsupported version, lock file mid-write):
		// unknown status, fail closed on positive predicates.
		data.ok = false
		return data
	}
	for _, e := range idx.Entries {
		lite := data.index[e.Name]
		if lite == nil {
			lite = &gitIndexLite{hash: e.Hash, size: e.Size, mtime: e.ModifiedAt}
			data.index[e.Name] = lite
		}
		if e.Stage != 0 {
			lite.unmerged = true
		}
	}
	data.tracked = make([]string, 0, len(data.index))
	for name := range data.index {
		data.tracked = append(data.tracked, name)
	}
	sort.Strings(data.tracked)
	repo, err := git.PlainOpen(s.root)
	if err != nil {
		data.ok = false
		return data
	}
	headRef, err := repo.Head()
	if err != nil || headRef == nil {
		// Unborn HEAD (fresh repo): everything tracked is staged; no tree.
		data.ok = true
		return data
	}
	data.headHex = headRef.Hash().String()
	commit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		data.ok = false
		return data
	}
	tree, err := commit.Tree()
	if err != nil {
		data.ok = false
		return data
	}
	data.head = tree
	data.recent = gitRecentFiles(repo, headRef.Hash())
	data.ok = true
	return data
}

// decodeGitIndex reads .git/index with go-git (supports v2; v3/v4 with
// extensions decode where go-git handles them, else an error -> unknown).
func decodeGitIndex(path string) (*index.Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	idx := &index.Index{}
	dec := index.NewDecoder(f)
	if err := dec.Decode(idx); err != nil {
		return nil, err
	}
	return idx, nil
}

// gitRecentMaxCommits caps the recent-commit boost window (fff default 10)
// and gitRecentMaxFilesPerCommit skips wide commits (fff skips > 50 files).
const (
	gitRecentMaxCommits        = 10
	gitRecentMaxFilesPerCommit = 50
	gitRecentTimeout           = 10 * time.Second
)

// gitRecentFiles returns the repo-relative slash paths touched by the last
// gitRecentMaxCommits commits (skipping commits touching more than
// gitRecentMaxFilesPerCommit files). Tree diffs prune identical subtrees by
// hash, so this walks changed structure only, never the whole worktree. A
// timeout yields whatever resolved in time (possibly empty: no boost).
func gitRecentFiles(repo *git.Repository, head plumbing.Hash) map[string]bool {
	type result struct {
		files map[string]bool
	}
	done := make(chan result, 1)
	go func() {
		out := make(map[string]bool)
		iter, err := repo.Log(&git.LogOptions{From: head})
		if err != nil {
			done <- result{out}
			return
		}
		count := 0
		_ = iter.ForEach(func(c *object.Commit) error {
			if count >= gitRecentMaxCommits {
				return errors.New("stop")
			}
			count++
			var parent *object.Commit
			if p, err := c.Parents().Next(); err == nil {
				parent = p
			}
			patch, err := c.Patch(parent)
			if err != nil {
				return nil
			}
			fps := patch.FilePatches()
			if len(fps) > gitRecentMaxFilesPerCommit {
				return nil
			}
			for _, fp := range fps {
				from, to := fp.Files()
				if from != nil && from.Path() != "" {
					out[from.Path()] = true
				}
				if to != nil && to.Path() != "" {
					out[to.Path()] = true
				}
			}
			return nil
		})
		done <- result{out}
	}()
	timer := time.NewTimer(gitRecentTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.files
	case <-timer.C:
		return make(map[string]bool)
	}
}

// gitEvidence is the resolved per-path evidence (ok=false: repo unreadable).
type gitEvidence struct {
	inRepo    bool
	ok        bool
	tracked   bool
	conflict  bool
	ignored   bool
	modified  bool // worktree differs from index (files only)
	staged    bool // index differs from HEAD (files only; + staged deletes)
	clean     bool
	isDir     bool
	recentHit bool
	headHash  string
}

// gitResolve returns the evidence for one absolute path. Need flags avoid
// unneeded work: stat for modified/clean, HEAD descent for staged/clean,
// ignore matching for untracked/ignored.
func gitResolve(root, absPath string, isDir bool, ctx *queryGitContext, need gitNeed) gitEvidence {
	state := gitRepoStateFor(root)
	data := state.ensureFresh(ctx)
	return gitResolveWith(state, data, root, absPath, isDir, need)
}

// gitBoostRecent resolves one hit's boost class and recent-commit membership
// with a single status resolution (one repo-state visit instead of two).
// Boost classes: 0 dirty (conflict/staged/modified files), 1 neutral
// (outside repos, unknown, dirs), 2 untracked, 3 clean/tracked/ignored.
func gitBoostRecent(root, absPath string, isDir bool, ctx *queryGitContext) (int, bool) {
	state := gitRepoStateFor(root)
	data := state.ensureFresh(ctx)
	if data == nil || !data.ok {
		return 1, false
	}
	need := gitNeed{modified: true, staged: true, untracked: true, ignored: true, recent: true}
	ev := gitResolveWith(state, data, root, absPath, isDir, need)
	if !ev.inRepo || !ev.ok {
		return 1, false
	}
	var boost int
	switch {
	case (ev.conflict || ev.modified || ev.staged) && !ev.isDir:
		boost = 0
	case !ev.tracked && !ev.ignored:
		boost = 2
	default:
		boost = 3
	}
	return boost, ev.recentHit
}

// gitResolveWith is gitResolve against an already-fetched snapshot.
func gitResolveWith(state *gitRepoState, data *gitRepoData, root, absPath string, isDir bool, need gitNeed) gitEvidence {
	ev := gitEvidence{isDir: isDir}
	rel, ok := gitRelSlash(root, absPath)
	if !ok {
		return ev
	}
	ev.inRepo = true
	if data == nil || !data.ok {
		return ev // ok=false: unknown
	}
	ev.ok = true
	ev.headHash = data.headHex
	lite, tracked := data.index[rel]
	ev.tracked = tracked
	if tracked && lite.unmerged {
		ev.conflict = true
	}
	if need.ignored || (need.untracked && !tracked) {
		ev.ignored = data.ignore.ignored(absPath, isDir)
	}
	if !tracked {
		// Staged deletion: in HEAD but dropped from the index.
		if need.staged || need.clean || need.field {
			if data.head != nil && gitHeadHas(state, data, rel) {
				ev.staged = true
				ev.tracked = true // visible to the tracked predicate as staged
			}
			// Otherwise untracked (git shows untracked dirs collapsed; same
			// rule): nothing else to compute.
		}
		if data.recent != nil && need.recent {
			ev.recentHit = data.recent[rel] || (isDir && gitRecentHasUnder(data.recent, rel))
		}
		return ev
	}
	if isDir {
		// Directories: tracked-ness is by prefix below; file-only classes
		// never match (documented approximation).
		ev.tracked = gitTrackedHasDir(data.tracked, rel)
		ev.clean = ev.tracked
		if data.recent != nil && need.recent {
			ev.recentHit = gitRecentHasUnder(data.recent, rel)
		}
		return ev
	}
	if need.modified || need.clean || need.field {
		ev.modified = gitWorktreeModified(absPath, lite)
	}
	if need.staged || need.clean || need.field {
		ev.staged = gitIndexDiffersHead(state, data, rel, lite)
	}
	if !ev.conflict && !ev.modified && !ev.staged {
		ev.clean = true
	}
	if data.recent != nil && need.recent {
		ev.recentHit = data.recent[rel]
	}
	return ev
}

// gitNeed selects which evidence to compute (lazy: stats and HEAD descents
// only run when a predicate or the status field needs them).
type gitNeed struct {
	modified  bool
	staged    bool
	untracked bool
	ignored   bool
	clean     bool
	recent    bool
	field     bool
}

func gitNeedForFilters(filters []string) gitNeed {
	var n gitNeed
	for _, f := range filters {
		switch f {
		case gitFilterModified:
			n.modified = true
		case gitFilterStaged:
			n.staged = true
		case gitFilterUntracked:
			n.untracked = true
		case gitFilterIgnored:
			n.ignored = true
		case gitFilterClean:
			n.clean = true
		}
	}
	return n
}

// gitWorktreeModified compares the worktree file against its index entry
// (size or mtime differs, or the file is gone). Dirs never qualify.
func gitWorktreeModified(absPath string, lite *gitIndexLite) bool {
	fi, err := os.Stat(absPath)
	if err != nil {
		return true // tracked but gone from the worktree: deleted
	}
	if fi.IsDir() {
		return false
	}
	if uint32(fi.Size()) != lite.size {
		return true
	}
	return !fi.ModTime().Equal(lite.mtime)
}

// gitHeadHas reports whether rel is in the HEAD tree.
func gitHeadHas(state *gitRepoState, data *gitRepoData, rel string) bool {
	state.treeMu.Lock()
	defer state.treeMu.Unlock()
	_, err := data.head.FindEntry(rel)
	return err == nil
}

// gitIndexDiffersHead reports staged changes: added (not in HEAD) or index
// hash/mode differs from HEAD.
func gitIndexDiffersHead(state *gitRepoState, data *gitRepoData, rel string, lite *gitIndexLite) bool {
	if data.head == nil {
		return true // unborn HEAD: everything tracked is staged
	}
	state.treeMu.Lock()
	defer state.treeMu.Unlock()
	te, err := data.head.FindEntry(rel)
	if err != nil {
		return true // in index, not in HEAD: added
	}
	return te.Hash != lite.hash
}

// gitTrackedHasDir reports whether any tracked path sits under dir rel.
func gitTrackedHasDir(tracked []string, rel string) bool {
	prefix := rel + "/"
	pos := sort.SearchStrings(tracked, prefix)
	return pos < len(tracked) && strings.HasPrefix(tracked[pos], prefix)
}

// gitRecentHasUnder reports whether any recent path sits under dir rel.
func gitRecentHasUnder(recent map[string]bool, rel string) bool {
	prefix := rel + "/"
	for p := range recent {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// gitEvidenceMatches reports whether evidence satisfies one filter class.
func gitEvidenceMatches(ev gitEvidence, filter string) bool {
	if !ev.inRepo || !ev.ok {
		return false
	}
	switch filter {
	case gitFilterModified:
		return ev.modified && !ev.conflict && !ev.isDir
	case gitFilterStaged:
		return ev.staged && !ev.conflict && !ev.isDir
	case gitFilterUntracked:
		return !ev.tracked && !ev.ignored
	case gitFilterIgnored:
		return ev.ignored
	case gitFilterConflict:
		return ev.conflict && !ev.isDir
	case gitFilterClean:
		return ev.clean
	case gitFilterTracked:
		return ev.tracked
	}
	return false
}

// gitFiltersMatch reports whether entry satisfies at least one of pq's
// git: filters (true when no filters). Unknown status fails closed.
func (pq parsedQuery) gitFiltersMatch(entry Entry) bool {
	if len(pq.GitFilters) == 0 {
		return true
	}
	need := gitNeedForFilters(pq.GitFilters)
	need.field = false
	need.recent = false
	root, ok := gitRepoRootForPath(cleanPathIfNeeded(entry.Path))
	if !ok {
		return false
	}
	ev := gitResolve(root, cleanPathIfNeeded(entry.Path), entry.Mode&uint32(os.ModeDir) != 0, pq.gitCtx, need)
	for _, f := range pq.GitFilters {
		if gitEvidenceMatches(ev, f) {
			return true
		}
	}
	return false
}

// gitStatusField resolves the per-hit git_status string ("", or one of
// conflict, modified, staged, untracked, ignored, clean). Empty when git was
// not resolved (caller gates on signals/filter presence) or the file sits
// outside any worktree.
func gitStatusField(pq parsedQuery, entry Entry) string {
	path := cleanPathIfNeeded(entry.Path)
	root, ok := gitRepoRootForPath(path)
	if !ok {
		return ""
	}
	need := gitNeedForFilters(pq.GitFilters)
	need.field = true
	need.recent = false
	ev := gitResolve(root, path, entry.Mode&uint32(os.ModeDir) != 0, pq.gitCtx, need)
	if !ev.inRepo || !ev.ok {
		return ""
	}
	switch {
	case ev.conflict && !ev.isDir:
		return gitFilterConflict
	case ev.modified && !ev.isDir:
		return gitFilterModified
	case ev.staged && !ev.isDir:
		return gitFilterStaged
	case ev.ignored:
		return gitFilterIgnored
	case !ev.tracked:
		return gitFilterUntracked
	case ev.clean:
		return gitFilterClean
	default:
		return gitFilterTracked
	}
}

// annotateJSONGitStatus fills the git_status field for each row when the
// query carries a git: filter (explicit opt-in to git resolution cost).
// Boost-only queries skip the field so the default path stays within noise.
func annotateJSONGitStatus(rows []jsonResult, matches []Entry, pq parsedQuery) {
	if len(pq.GitFilters) == 0 || len(rows) == 0 {
		return
	}
	for i := range rows {
		if i < len(matches) {
			rows[i].GitStatus = gitStatusField(pq, matches[i])
		}
	}
}

// gitRecentHit reports whether absPath was touched by recent commits (for the
// recent-commit boost). False when the repo is unknown or the log is empty.
func gitRecentHit(root, absPath string, isDir bool, ctx *queryGitContext) bool {
	_, recent := gitBoostRecent(root, absPath, isDir, ctx)
	return recent
}
