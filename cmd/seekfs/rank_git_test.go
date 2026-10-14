package main

// Tests for git awareness: git: filter classes, per-hit status, cache
// invalidation, recent-commit files, and unknown-repo behavior. Repos are
// built with go-git (pure Go, no git CLI).

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type gitTestRepo struct {
	root string
	repo *git.Repository
	work *git.Worktree
}

func initGitTestRepo(t *testing.T) *gitTestRepo {
	t.Helper()
	root := t.TempDir()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	work, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	resetGitRepoCacheForTest()
	t.Cleanup(resetGitRepoCacheForTest)
	return &gitTestRepo{root: root, repo: repo, work: work}
}

func (r *gitTestRepo) write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(r.root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (r *gitTestRepo) addCommit(t *testing.T, msg string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := r.work.Add(p); err != nil {
			t.Fatalf("Add %s: %v", p, err)
		}
	}
	_, err := r.work.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{Name: "seekfs-test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func gitTestEntry(path string) Entry {
	return Entry{Path: path, Name: filepath.Base(path)}
}

func mustParseGitQuery(t *testing.T, q string) parsedQuery {
	t.Helper()
	pq, err := parseQuery(queryOptions{Query: q})
	if err != nil {
		t.Fatalf("parseQuery(%q): %v", q, err)
	}
	return pq
}

func assertGitMatch(t *testing.T, q, path string, want bool) {
	t.Helper()
	pq := mustParseGitQuery(t, q)
	if got := entryMatches(gitTestEntry(path), pq, pq.MatchPath); got != want {
		t.Fatalf("query %q on %s = %v, want %v", q, path, got, want)
	}
}

func TestGitFilterClasses(t *testing.T) {
	r := initGitTestRepo(t)
	clean := r.write(t, "clean.go", "package clean\n")
	r.write(t, "sub/mod.go", "package mod\n")
	r.addCommit(t, "initial", "clean.go", "sub/mod.go")

	modPath := filepath.Join(r.root, "sub", "mod.go")
	if err := os.WriteFile(modPath, []byte("package mod\n// edited with more bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	stagedPath := r.write(t, "staged.go", "package staged\n")
	if _, err := r.work.Add("staged.go"); err != nil {
		t.Fatal(err)
	}
	untrackedPath := r.write(t, "untracked.go", "package untracked\n")
	r.write(t, ".gitignore", "*.log\n")
	ignoredPath := r.write(t, "debug.log", "noise")

	assertGitMatch(t, "mod.go git:modified", modPath, true)
	assertGitMatch(t, "clean.go git:modified", clean, false)
	assertGitMatch(t, "mod.go git:clean", modPath, false)
	assertGitMatch(t, "clean.go git:clean", clean, true)
	assertGitMatch(t, "staged.go git:staged", stagedPath, true)
	assertGitMatch(t, "mod.go git:staged", modPath, false)
	assertGitMatch(t, "untracked.go git:untracked", untrackedPath, true)
	assertGitMatch(t, "clean.go git:untracked", clean, false)
	assertGitMatch(t, "debug.log git:ignored", ignoredPath, true)
	assertGitMatch(t, "untracked.go git:ignored", untrackedPath, false)
	assertGitMatch(t, "clean.go git:tracked", clean, true)
	assertGitMatch(t, "mod.go git:tracked", modPath, true)
	assertGitMatch(t, "staged.go git:tracked", stagedPath, true)
	assertGitMatch(t, "untracked.go git:tracked", untrackedPath, false)
	assertGitMatch(t, "clean.go git:conflict", clean, false)
	// OR form and negation compose through the existing machinery.
	assertGitMatch(t, "mod.go git:modified,staged", modPath, true)
	assertGitMatch(t, "staged.go git:modified,staged", stagedPath, true)
	assertGitMatch(t, "clean.go git:modified,staged", clean, false)
	assertGitMatch(t, "untracked.go !git:tracked", untrackedPath, true)
	assertGitMatch(t, "clean.go !git:tracked", clean, false)
}

func TestGitFilterUnknownClass(t *testing.T) {
	if _, err := parseQuery(queryOptions{Query: "x git:bogus"}); err == nil {
		t.Fatal("unknown git: class must be rejected")
	}
	if _, err := parseQuery(queryOptions{Query: "x git:"}); err == nil {
		t.Fatal("empty git: must be rejected")
	}
	pq := mustParseGitQuery(t, "x git:modified")
	if len(pq.GitFilters) != 1 || pq.GitFilters[0] != gitFilterModified {
		t.Fatalf("GitFilters = %v", pq.GitFilters)
	}
	if !pq.lateVerified() {
		t.Fatal("git: queries are late-verified (decline rank-truncated lanes)")
	}
}

func TestGitStatusPerHit(t *testing.T) {
	r := initGitTestRepo(t)
	clean := r.write(t, "clean.go", "package clean\n")
	r.addCommit(t, "initial", "clean.go")
	modPath := clean
	if err := os.WriteFile(modPath, []byte("package clean\n// changed, longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	untrackedPath := r.write(t, "new.go", "package new\n")

	pq := mustParseGitQuery(t, "go git:modified")
	if got := gitStatusField(pq, gitTestEntry(modPath)); got != gitFilterModified {
		t.Fatalf("modified status = %q", got)
	}
	pqClean := mustParseGitQuery(t, "go git:clean")
	// After the edit the file is not clean.
	if got := gitStatusField(pqClean, gitTestEntry(modPath)); got == gitFilterClean {
		t.Fatalf("edited file reports clean")
	}
	if got := gitStatusField(pqClean, gitTestEntry(untrackedPath)); got != gitFilterUntracked {
		t.Fatalf("untracked status = %q", got)
	}
	// Outside any repo: empty.
	if got := gitStatusField(pq, gitTestEntry(`T:\nowhere\x.go`)); got != "" {
		t.Fatalf("outside-repo status = %q", got)
	}
}

func TestGitCacheInvalidation(t *testing.T) {
	r := initGitTestRepo(t)
	p := r.write(t, "a.go", "package a\n")
	r.addCommit(t, "initial", "a.go")

	assertGitMatch(t, "a.go git:clean", p, true)
	// Edit: a fresh query generation must observe the index stat change.
	if err := os.WriteFile(p, []byte("package a\n// edited, now longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertGitMatch(t, "a.go git:modified", p, true)
	assertGitMatch(t, "a.go git:clean", p, false)
	// Commit the edit: HEAD moves, the file is clean again.
	if _, err := r.work.Add("a.go"); err != nil {
		t.Fatal(err)
	}
	_, err := r.work.Commit("edit", &git.CommitOptions{
		Author: &object.Signature{Name: "seekfs-test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertGitMatch(t, "a.go git:clean", p, true)
	assertGitMatch(t, "a.go git:modified", p, false)
}

func TestGitRecentCommits(t *testing.T) {
	r := initGitTestRepo(t)
	a := r.write(t, "a.go", "package a\n")
	r.write(t, "b.go", "package b\n")
	r.addCommit(t, "first", "a.go", "b.go")
	ctx := newQueryGitContext()
	if !gitRecentHit(r.root, a, false, ctx) {
		t.Fatal("a.go touched by the last commit must be a recent hit")
	}
	if gitRecentHit(r.root, filepath.Join(r.root, "missing.go"), false, ctx) {
		t.Fatal("untouched path must not be a recent hit")
	}
	// A wide commit (> 50 files) is skipped: its files stay non-recent.
	var wide []string
	for i := 0; i < 55; i++ {
		name := filepath.Join("wide", "f"+string(rune('a'+i%26))+itoa(i)+".go")
		r.write(t, name, "package wide\n")
		wide = append(wide, name)
	}
	r.addCommit(t, "wide", wide...)
	ctx2 := newQueryGitContext()
	if gitRecentHit(r.root, filepath.Join(r.root, wide[0]), false, ctx2) {
		t.Fatal("files from a >50-file commit must be skipped")
	}
	// ...while the earlier small commit is still inside the window.
	if !gitRecentHit(r.root, a, false, ctx2) {
		t.Fatal("a.go must still be recent after the wide commit")
	}
}

func TestGitOutsideRepo(t *testing.T) {
	resetGitRepoCacheForTest()
	for _, q := range []string{"x git:modified", "x git:staged", "x git:untracked", "x git:tracked", "x git:clean", "x git:ignored", "x git:conflict"} {
		assertGitMatch(t, q, `T:\nowhere\x.go`, false)
	}
	// Negation passes outside repos (fail open on exclusion).
	assertGitMatch(t, "x !git:modified", `T:\nowhere\x.go`, true)
}

func TestGitEvidenceMatchesUnit(t *testing.T) {
	ev := gitEvidence{inRepo: true, ok: true, tracked: true, conflict: true}
	if !gitEvidenceMatches(ev, gitFilterConflict) || gitEvidenceMatches(ev, gitFilterClean) {
		t.Fatalf("conflict evidence = %+v", ev)
	}
	ev = gitEvidence{inRepo: true, ok: true, tracked: true, clean: true}
	if !gitEvidenceMatches(ev, gitFilterClean) || !gitEvidenceMatches(ev, gitFilterTracked) {
		t.Fatalf("clean evidence = %+v", ev)
	}
	ev = gitEvidence{inRepo: true, ok: true, ignored: true}
	if !gitEvidenceMatches(ev, gitFilterIgnored) || gitEvidenceMatches(ev, gitFilterUntracked) {
		t.Fatalf("ignored evidence = %+v", ev)
	}
	// Unknown repo state fails closed on positive predicates.
	ev = gitEvidence{inRepo: true, ok: false}
	if gitEvidenceMatches(ev, gitFilterModified) {
		t.Fatal("unknown status must not match positive predicates")
	}
}

func TestGitQueryGates(t *testing.T) {
	// A bare git: filter is a valid (narrowing) query.
	pq := mustParseGitQuery(t, "go git:modified")
	if pq.isEmpty() {
		t.Fatal("git: filter must count as a searchable constraint")
	}
	// near: is ranking-only; alone it is not a query.
	if _, err := parseQuery(queryOptions{Query: "near:T:\\proj"}); err == nil {
		t.Fatal("bare near: must not satisfy the query")
	}
	// OR-alternatives each carry their own git: filter.
	pq = mustParseGitQuery(t, "go git:modified|git:staged")
	if len(pq.OrGroups) != 1 || len(pq.OrGroups[0]) != 2 {
		t.Fatalf("git: OR group = %+v", pq.OrGroups)
	}
	// cloneParsedQuery preserves the new fields.
	clone := cloneParsedQuery(pq)
	if len(clone.OrGroups) != 1 {
		t.Fatalf("clone dropped OrGroups: %+v", clone)
	}
	pq2 := mustParseGitQuery(t, "go git:staged")
	pq2.Near = `T:\proj`
	clone2 := cloneParsedQuery(pq2)
	if len(clone2.GitFilters) != 1 || clone2.Near == "" || clone2.gitCtx != pq2.gitCtx {
		t.Fatalf("clone must preserve GitFilters/Near/gitCtx: %+v", clone2)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
