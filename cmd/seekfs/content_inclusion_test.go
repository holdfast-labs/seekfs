package main

// Tests for the inclusion policy (§6z, WORK_QUEUE item 1): linked-worktree
// filtering, per-file caps, and every skip class.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// stubWorktreeProbe replaces the `.git` filesystem probe with a set of
// linked-worktree roots for one test.
func stubWorktreeProbe(t *testing.T, linked ...string) {
	t.Helper()
	set := make(map[string]struct{}, len(linked))
	for _, r := range linked {
		set[contentNormPath(r)] = struct{}{}
	}
	restore := contentWorktreeProbeFunc
	contentWorktreeProbeFunc = func(repoRoot string) bool {
		_, ok := set[contentNormPath(repoRoot)]
		return ok
	}
	t.Cleanup(func() { contentWorktreeProbeFunc = restore })
}

func TestContentWorktreeProbe(t *testing.T) {
	dir := t.TempDir()
	// Main checkout: a `.git` directory is never a linked worktree.
	main := filepath.Join(dir, "main")
	if err := os.MkdirAll(filepath.Join(main, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Linked worktree: a `.git` FILE pointing at the main's worktrees dir.
	wt := filepath.Join(dir, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "wt")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Submodule: a `.git` file pointing at the modules dir stays indexed.
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../.git/modules/sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Bare pointer garbage and a missing entry fail open (indexed).
	bad := filepath.Join(dir, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, ".git"), []byte("not a pointer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{main: false, wt: true, sub: false, bad: false, filepath.Join(dir, "missing"): false} {
		if got := contentWorktreeProbeFunc(path); got != want {
			t.Errorf("probe(%q) = %v; want %v", path, got, want)
		}
	}
}

func TestContentFilterLinkedWorktrees(t *testing.T) {
	stubWorktreeProbe(t, `E:\w\linked`)
	repos := []string{contentNormPath(`E:\w\main`), contentNormPath(`E:\w\linked`)}
	kept, dropped := contentFilterLinkedWorktrees(repos, false)
	if len(kept) != 1 || kept[0] != repos[0] || len(dropped) != 1 || dropped[0] != repos[1] {
		t.Fatalf("main-only kept=%v dropped=%v", kept, dropped)
	}
	kept, dropped = contentFilterLinkedWorktrees(repos, true)
	if len(kept) != 2 || len(dropped) != 0 {
		t.Fatalf("all kept=%v dropped=%v", kept, dropped)
	}
}

func TestContentResolveFiltersLinkedWorktrees(t *testing.T) {
	stubWorktreeProbe(t, `C:\w\linked`)
	repos := []string{contentNormPath(`C:\w\main`), contentNormPath(`C:\w\linked`)}
	s := defaultContentScope()
	r := s.resolve("C:", repos)
	if len(r.Repos) != 1 || r.Repos[0] != repos[0] {
		t.Fatalf("repos = %v; want only the main checkout", r.Repos)
	}
	if len(r.DroppedWorktrees) != 1 || r.DroppedWorktrees[0] != repos[1] {
		t.Fatalf("dropped = %v; want the linked worktree", r.DroppedWorktrees)
	}
	// The mode joins the fingerprint: main vs all select differently.
	sAll := defaultContentScope()
	sAll.WorktreesAll = true
	rAll := sAll.resolve("C:", repos)
	if len(rAll.Repos) != 2 {
		t.Fatalf("worktrees=all repos = %v; want both", rAll.Repos)
	}
	if r.fingerprint() == rAll.fingerprint() {
		t.Fatal("worktrees mode did not change the scope identity")
	}
}

func TestContentInclusionSkipClasses(t *testing.T) {
	norm := func(p string) string { return contentNormPath(p) }
	baseOf := func(p string) string {
		p = norm(p)
		for i := len(p) - 1; i >= 0; i-- {
			if p[i] == '\\' || p[i] == '/' {
				return p[i+1:]
			}
		}
		return p
	}
	cases := []struct {
		path   string
		size   int64
		inRepo bool
		sys    bool
		skip   []string
		reason string
	}{
		{`C:\w\app\package-lock.json`, 100, true, true, nil, contentSkipLockfile},
		{`C:\w\app\yarn.lock`, 100, false, true, nil, contentSkipLockfile},
		{`C:\w\app\go.sum`, 100, true, true, nil, contentSkipLockfile},
		{`C:\w\app\data.json`, 100 << 10, true, true, nil, contentSkipLargeJSON},
		{`C:\w\app\data.json`, 99, true, true, nil, ""},
		{`C:\w\app\data.json`, -1, true, true, nil, ""}, // unknown size admits
		{`C:\w\app\app.min.js`, 100, true, true, nil, contentSkipMinified},
		{`C:\w\app\bundle.chunk.js`, 100, true, true, nil, contentSkipMinified},
		{`C:\w\app\sourcemap.map`, 100, true, true, nil, contentSkipMinified},
		{`C:\w\app\app.js`, 100, true, true, nil, ""},
		// Build output only outside repos (inside, git decides).
		{`C:\w\dist\a.js`, 100, false, true, nil, contentSkipBuildOutput},
		{`C:\w\build\a.js`, 100, false, true, nil, contentSkipBuildOutput},
		{`C:\w\out\a.js`, 100, false, true, nil, contentSkipBuildOutput},
		{`C:\w\target\a.js`, 100, false, true, nil, contentSkipBuildOutput},
		{`C:\w\build\a.js`, 100, true, true, nil, ""},
		{`C:\w\build`, 100, false, true, nil, ""}, // file named build, not a dir
		// system_excludes=false opts out of the directory rules.
		{`C:\w\dist\a.js`, 100, false, false, nil, ""},
		{`C:\w\app\package-lock.json`, 100, false, false, nil, contentSkipLockfile},
		// Cache trees anywhere.
		{`C:\w\gocache\a`, 100, false, true, nil, contentSkipCache},
		{`C:\Users\u\.npm\_cacache\x`, 100, false, true, nil, contentSkipCache},
		{`C:\w\repo\.cache\uv\x`, 100, true, true, nil, contentSkipCache},
		// User skip list: prefixes and globs.
		{`C:\w\secret\a.go`, 100, false, true, []string{contentNormPath(`C:\w\secret`)}, contentSkipUserSkip},
		{`C:\w\secret\sub\a.go`, 100, false, true, []string{contentNormPath(`C:\w\secret`)}, contentSkipUserSkip},
		{`C:\w\other\a.go`, 100, false, true, []string{contentNormPath(`C:\w\secret`)}, ""},
		{`C:\w\gen\a.pb.go`, 100, false, true, []string{`c:\w\gen\*.pb.go`}, contentSkipUserSkip},
		{`C:\w\gen\sub\a.pb.go`, 100, false, true, []string{`c:\w\gen\*.pb.go`}, ""},
		{`C:\w\gen\sub\a.pb.go`, 100, false, true, []string{`c:\w\gen\**\*.pb.go`}, contentSkipUserSkip},
	}
	for _, c := range cases {
		p := norm(c.path)
		reason, skip := contentInclusionSkip(p, baseOf(c.path), contentPathExtension(p), c.size, c.inRepo, c.sys, contentNormalizeSkipPatterns(c.skip))
		if want := c.reason != ""; skip != want || reason != c.reason {
			t.Errorf("skip(%q) = (%q,%v); want (%q,%v)", c.path, reason, skip, c.reason, want)
		}
	}
}

func TestContentInclusionCaps(t *testing.T) {
	raw, text := contentInclusionCapsForFile(false, 0, 0)
	if text != contentInclusionTextCapOutside || raw != 2*contentInclusionTextCapOutside {
		t.Fatalf("outside caps = %d/%d; want 2MiB/1MiB", raw, text)
	}
	raw, text = contentInclusionCapsForFile(true, 0, 0)
	if text != contentInclusionTextCapInside || raw != 2*contentInclusionTextCapInside {
		t.Fatalf("inside caps = %d/%d; want 20MiB/10MiB", raw, text)
	}
	// Explicit caps apply uniformly, clamped to the ceiling.
	raw, text = contentInclusionCapsForFile(false, 1<<30, 1<<30)
	if raw != contentExtractHardMaxRawBytes || text != contentExtractHardMaxTextBytes {
		t.Fatalf("explicit caps = %d/%d; want the hard ceiling", raw, text)
	}
	raw, text = contentInclusionCapsForFile(true, 4096, 2048)
	if raw != 4096 || text != 2048 {
		t.Fatalf("explicit caps = %d/%d; want 4096/2048", raw, text)
	}
	// Through the context: defaults resolve per file, explicit is uniform.
	ctx := contentWithExtractSettings(context.Background(), contentBuildOptions{}.extractSettings())
	if _, fileText := contentFileCaps(contentExtractSettingsFromContext(ctx), false); fileText != contentInclusionTextCapOutside {
		t.Fatalf("default outside text cap = %d", fileText)
	}
	explicit := contentWithExtractSettings(context.Background(), contentBuildOptions{MaxRaw: 1 << 20, MaxText: 1 << 19}.extractSettings())
	if _, fileText := contentFileCaps(contentExtractSettingsFromContext(explicit), false); fileText != 1<<19 {
		t.Fatalf("explicit outside text cap = %d; want uniform override", fileText)
	}
	// Unstamped contexts fail open to the in-repo caps.
	if inRepo, ok := contentInRepoFromContext(context.Background()); !inRepo || ok {
		t.Fatalf("unstamped inRepo = %v,%v; want true,false", inRepo, ok)
	}
}

func TestContentLooksMinified(t *testing.T) {
	singleLine := make([]byte, 0, 40<<10)
	for i := 0; i < 40<<10; i++ {
		singleLine = append(singleLine, 'x')
	}
	if !contentLooksMinified(singleLine) {
		t.Fatal("40 KiB single line must read as minified")
	}
	normal := make([]byte, 0, 40<<10)
	for i := 0; i < 2000; i++ {
		normal = append(normal, []byte("a short source line with code();\n")...)
	}
	if contentLooksMinified(normal) {
		t.Fatal("short lines must not read as minified")
	}
	small := []byte("one very long line without any newline at all, but tiny")
	if contentLooksMinified(small) {
		t.Fatal("files under the min size must stay indexed")
	}
}

func TestContentSkipGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pat, path string
		want      bool
	}{
		{`c:\w\gen\*.pb.go`, `c:\w\gen\a.pb.go`, true},
		{`c:\w\gen\*.pb.go`, `c:\w\gen\sub\a.pb.go`, false},
		{`c:\w\gen\**.pb.go`, `c:\w\gen\a.pb.go`, true},
		{`c:\w\gen\**\*.pb.go`, `c:\w\gen\sub\a.pb.go`, true},
		{`c:\w\gen\**`, `c:\w\gen\sub\a.pb.go`, true},
		{`c:\w\**\secret`, `c:\w\a\b\secret`, true},
		{`c:\w\gen\?.go`, `c:\w\gen\a.go`, true},
		{`c:\w\gen\?.go`, `c:\w\gen\ab.go`, false},
	} {
		if got := contentSkipGlobMatch(c.pat, c.path); got != c.want {
			t.Errorf("glob(%q,%q) = %v; want %v", c.pat, c.path, got, c.want)
		}
	}
}
