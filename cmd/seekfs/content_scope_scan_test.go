package main

// Differential + performance tests for the memoized parallel scope scan
// (content_scope_scan.go): the fast scan must admit exactly the set the
// legacy allows()/priority() loop admits, and do it fast enough that a
// million-record pre-build scan finishes in seconds, not minutes.

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// scanTreeFixture builds a real directory tree (so gitignore files exist on
// disk where the records say they do) plus a compact index over it, with
// repos, ignored output, excluded segments, binaries and odd extensions.
type scanTreeFixture struct {
	root string
	idx  *Index
}

func buildScanTreeFixture(t *testing.T, rnd *rand.Rand) *scanTreeFixture {
	t.Helper()
	root := t.TempDir()
	// Build everything on disk — a few thousand files is fast and keeps
	// every path honest (real stats, real ignore-file reads).
	exts := []string{".go", ".txt", ".md", ".png", ".log", "", ".html", ".bin"}
	dirNames := []string{"src", "node_modules", "build", "vendor", "docs", "out", "pkg", ".git"}
	repoRoots := map[string]string{
		filepath.Join(root, "proj"):  "*.log\n/out/\n!important.keep\n",
		filepath.Join(root, "other"): "build/\n",
	}
	for rel, ignore := range repoRoots {
		if err := os.MkdirAll(filepath.Join(rel, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rel, ".gitignore"), []byte(ignore), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Random subtrees under root and the repos.
	targets := append([]string{root}, filepath.Join(root, "proj"), filepath.Join(root, "other"))
	for _, base := range targets {
		for i := 0; i < 12; i++ {
			d := filepath.Join(base, dirNames[rnd.Intn(len(dirNames))], fmt.Sprintf("d%d", i))
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			for f := 0; f < 20; f++ {
				name := fmt.Sprintf("f%03d%s", rnd.Intn(500), exts[rnd.Intn(len(exts))])
				if err := os.WriteFile(filepath.Join(d, name), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// A few special files: explicitly kept despite ignore rules, binaries.
	for _, p := range []string{
		filepath.Join(root, "proj", "important.keep"),
		filepath.Join(root, "proj", "out", "generated.txt"),
		filepath.Join(root, "top.txt"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Index the real tree through the production walk path so parent links,
	// FRNs and paths are exactly what the service sees.
	walk := &Index{Version: indexVersion, Roots: []string{root}, BuiltAt: time.Now(), Source: "walk"}
	if err := walkRoot(root, walk); err != nil {
		t.Fatal(err)
	}
	buildOrders(walk)
	ensureCompactIndexForService(walk)
	return &scanTreeFixture{root: root, idx: walk}
}

// legacyScanSelection is the reference implementation: the pre-parallel
// loop (reconstruct + opts.allows + priority), single-threaded.
func legacyScanSelection(idx *Index, opts contentBuildOptions, isWalk bool) map[uint64]contentBuildItem {
	out := make(map[uint64]contentBuildItem)
	cache := make(map[int]string, 1024)
	for id := 0; id < idx.compactRecordCount(); id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted || rec.FRN == 0 || rec.Mode&uint32(os.ModeDir) != 0 {
			continue
		}
		path := idx.reconstructCompactPathCached(id, cache)
		size := rec.Size
		if size < 0 {
			size = 0
		}
		allows := opts.allows(path)
		if opts.Scope != nil {
			// The fast scan applies the size-gated inclusion rules with
			// the record size; the reference must too, or the two bind
			// different sets by construction.
			_, _, allows = opts.Scope.allowsSize(path, size)
		}
		if path == "" || !allows {
			continue
		}
		frn := rec.FRN
		if isWalk {
			frn = contentWalkKey(path)
			if frn == 0 {
				continue
			}
		}
		item := contentBuildItem{frn: frn, path: path, mod: rec.ModUnix}
		if opts.Scope != nil {
			item.priority = opts.Scope.priority(path)
		}
		out[frn] = item
	}
	return out
}

// fastScanSelection runs scanScopeMembers over the whole index and returns
// the admitted set.
func fastScanSelection(t *testing.T, s *goSearchService, vol *serviceVolumeIndex, scope *contentScopeResolved, idx *Index, isWalk bool) map[uint64]contentBuildItem {
	t.Helper()
	out := make(map[uint64]contentBuildItem)
	cfg := contentScopeScanConfig{
		scope:  scope,
		idx:    idx,
		isWalk: isWalk,
		stopped: func() bool {
			return false
		},
		genChanged: func() bool {
			return false
		},
	}
	lists, ok := scanScopeMembers(s, vol, cfg)
	if !ok {
		t.Fatal("fast scan reported a moved base on a pinned fixture")
	}
	for _, l := range lists {
		for _, m := range l {
			out[m.frn] = contentBuildItem{frn: m.frn, path: m.path, priority: m.priority, mod: m.mod, size: m.size}
		}
	}
	return out
}

// The fast scan must admit exactly the set the legacy loop admits, with the
// same priorities, across scopes (auto, explicit, sniff and allowlist),
// origins (USN FRNs and walk keys), and randomized trees.
func TestContentScanMatchesLegacy(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		rnd := rand.New(rand.NewSource(seed))
		fx := buildScanTreeFixture(t, rnd)
		s := contentTestService(t)
		vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "scan.gsi"), fx.idx)
		releaseVolumeContentOnCleanup(t, vol)
		for _, mode := range []string{"auto", "explicit"} {
			t.Setenv("SEEKFS_CONTENT_SCOPE", mode)
			t.Setenv("SEEKFS_CONTENT_ROOTS", fx.root)
			scope, ok := s.resolvedContentScope(vol)
			if !ok {
				t.Fatalf("seed %d scope %s: not resolvable", seed, mode)
			}
			for _, isWalk := range []bool{false, true} {
				opts := defaultContentBuildOptions()
				opts.Scope = &scope
				want := legacyScanSelection(fx.idx, opts, isWalk)
				want2 := legacyScanSelection(fx.idx, opts, isWalk)
				if len(want) != len(want2) {
					t.Fatalf("legacy nondeterministic: %d vs %d", len(want), len(want2))
				}
				for frn, a := range want {
					if b := want2[frn]; b.path != a.path {
						t.Fatalf("legacy nondeterministic at %d: %q vs %q", frn, a.path, b.path)
					}
				}
				got := fastScanSelection(t, s, vol, &scope, fx.idx, isWalk)
				got2 := fastScanSelection(t, s, vol, &scope, fx.idx, isWalk)
				if len(got) != len(got2) {
					t.Fatalf("fast nondeterministic: %d vs %d", len(got), len(got2))
				}
				for frn, a := range got {
					if b := got2[frn]; b.path != a.path {
						t.Fatalf("fast nondeterministic at %d: %q vs %q", frn, a.path, b.path)
					}
				}
				if len(got) != len(want) {
					t.Logf("seed %d scope %s walk=%v: fast=%d legacy=%d", seed, mode, isWalk, len(got), len(want))
				}
				var missing []string
				for frn, w := range want {
					g, ok := got[frn]
					if !ok {
						if len(missing) < 8 {
							missing = append(missing, w.path)
						}
						continue
					}
					if g.path != w.path || g.priority != w.priority {
						t.Fatalf("seed %d scope %s walk=%v: key %d fast=%+v legacy=%+v", seed, mode, isWalk, frn, g, w)
					}
				}
				if len(missing) > 0 {
					t.Fatalf("seed %d scope %s walk=%v: %d keys missing from fast scan, e.g. %q", seed, mode, isWalk, len(want)-len(got), missing)
				}
				// Cache-order determinism audit: forward, reverse, and
				// per-id-fresh reconstruction must all agree, or a shared
				// path cache can serve a doubled root prefix to later
				// lookups (the root record's own path must equal the root).
				n := fx.idx.compactRecordCount()
				fwd := make(map[int]string, n)
				for id := 0; id < n; id++ {
					fx.idx.reconstructCompactPathCached(id, fwd)
				}
				rev := make(map[int]string, n)
				for id := n - 1; id >= 0; id-- {
					fx.idx.reconstructCompactPathCached(id, rev)
				}
				bad := 0
				for id := 0; id < n; id++ {
					fresh := fx.idx.reconstructCompactPathCached(id, make(map[int]string, 4))
					if fwd[id] != fresh || rev[id] != fresh {
						if bad < 5 {
							t.Logf("id=%d fwd=%q rev=%q fresh=%q", id, fwd[id], rev[id], fresh)
						}
						bad++
					}
				}
				if bad > 0 {
					t.Fatalf("%d/%d records reconstruct cache-order-dependently", bad, n)
				}
			}
		}
	}
}

// Flat Parent == -1 records (volume-root children, the shape every
// hand-built USN fixture uses) must diff exactly too: the parent decision
// for -1 is the volume directory itself, not empty.
func TestContentScanFlatMatchesLegacy(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "alpha")
	write("b.txt", "beta")
	write(".gitignore", "b.txt\n")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(".git", "HEAD"), "x")
	idx := &Index{Source: "usn", Volume: dir, Compact: true}
	idx.Records = []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: 5},
		{FRN: 20, ParentFRN: 20, Parent: -1, Name: "b.txt", Size: 4},
		{FRN: 30, ParentFRN: 30, Parent: -1, Name: ".gitignore", Size: 6},
		{FRN: 40, ParentFRN: 40, Parent: -1, Name: ".git", Mode: uint32(os.ModeDir)},
	}
	contentIndexFRNs(idx)
	s := contentTestService(t)
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "scan.gsi"), idx)
	releaseVolumeContentOnCleanup(t, vol)
	// Drive-root volumes keep their trailing separator after Clean ("C:\"
	// stays "c:\"); the volume-root decision must still prefix-match a root
	// naming the volume, exactly like every file below it does.
	drive := &Index{Source: "usn", Volume: `C:\`, Compact: true}
	drive.Records = []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: 5},
		{FRN: 20, ParentFRN: 20, Parent: -1, Name: "b.txt", Size: 4},
	}
	contentIndexFRNs(drive)
	driveVol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "scan.gsi"), drive)
	releaseVolumeContentOnCleanup(t, driveVol)
	for _, mode := range []string{"auto", "explicit"} {
		t.Setenv("SEEKFS_CONTENT_SCOPE", mode)
		t.Setenv("SEEKFS_CONTENT_ROOTS", dir)
		scope, ok := s.resolvedContentScope(vol)
		if !ok {
			t.Fatalf("scope %s: not resolvable", mode)
		}
		opts := defaultContentBuildOptions()
		opts.Scope = &scope
		want := legacyScanSelection(idx, opts, false)
		got := fastScanSelection(t, s, vol, &scope, idx, false)
		if len(got) != len(want) {
			t.Fatalf("scope %s: fast=%d legacy=%d", mode, len(got), len(want))
		}
		for frn, w := range want {
			g, ok := got[frn]
			if !ok {
				t.Fatalf("scope %s: key %d (%q) missing from fast scan", mode, frn, w.path)
			}
			if g.path != w.path || g.priority != w.priority {
				t.Fatalf("scope %s: key %d fast=%+v legacy=%+v", mode, frn, g, w)
			}
		}
		t.Setenv("SEEKFS_CONTENT_ROOTS", `C:\`)
		driveScope, ok := s.resolvedContentScope(driveVol)
		if !ok {
			t.Fatalf("drive scope %s: not resolvable", mode)
		}
		driveOpts := defaultContentBuildOptions()
		driveOpts.Scope = &driveScope
		driveWant := legacyScanSelection(drive, driveOpts, false)
		driveGot := fastScanSelection(t, s, driveVol, &driveScope, drive, false)
		if len(driveGot) != len(driveWant) || len(driveWant) == 0 {
			t.Fatalf("drive scope %s: fast=%d legacy=%d (legacy must admit the volume-root files)", mode, len(driveGot), len(driveWant))
		}
		for frn, w := range driveWant {
			g, ok := driveGot[frn]
			if !ok {
				t.Fatalf("drive scope %s: key %d (%q) missing from fast scan", mode, frn, w.path)
			}
			if g.path != w.path || g.priority != w.priority {
				t.Fatalf("drive scope %s: key %d fast=%+v legacy=%+v", mode, frn, g, w)
			}
		}
	}
}

// The capped merge must keep the global top by (priority, path, frn),
// matching the legacy single-heap scan exactly.
func TestContentScanCappedMatchesLegacy(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	fx := buildScanTreeFixture(t, rnd)
	s := contentTestService(t)
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "scan.gsi"), fx.idx)
	releaseVolumeContentOnCleanup(t, vol)
	t.Setenv("SEEKFS_CONTENT_SCOPE", "explicit")
	t.Setenv("SEEKFS_CONTENT_ROOTS", fx.root)
	scope, ok := s.resolvedContentScope(vol)
	if !ok {
		t.Fatal("scope not resolvable")
	}
	opts := defaultContentBuildOptions()
	opts.Scope = &scope
	opts.MaxFiles = 50
	legacy := legacyScanSelection(fx.idx, opts, false)
	// Legacy applies the cap by priority order; emulate the snapshot's heap
	// rule: keep the 50 least by contentBuildItemLess.
	all := make([]contentBuildItem, 0, len(legacy))
	for _, it := range legacy {
		all = append(all, it)
	}
	sort.Slice(all, func(i, j int) bool { return contentBuildItemLess(all[i], all[j]) })
	if len(all) > 50 {
		all = all[:50]
	}
	cfg := contentScopeScanConfig{scope: &scope, idx: fx.idx, maxFiles: 50,
		stopped: func() bool { return false }, genChanged: func() bool { return false }}
	lists, ok := scanScopeMembers(s, vol, cfg)
	if !ok {
		t.Fatal("fast scan moved")
	}
	items, limited := mergeScanMembers(lists, 50)
	if !limited || len(items) != len(all) {
		t.Fatalf("capped merge = %d items limited=%v; want %d limited=true", len(items), limited, len(all))
	}
	want := make(map[uint64]contentBuildItem, len(all))
	for _, it := range all {
		want[it.frn] = it
	}
	for _, it := range items {
		w, ok := want[it.frn]
		if !ok || w.path != it.path || w.priority != it.priority {
			t.Fatalf("capped merge item %+v not in legacy top-50", it)
		}
	}
}

// A capped scan whose corpus never fills the cap must return the whole
// admitted set, exactly like the uncapped scan: the per-worker top holds
// arrivals until the cap fills, and dropping it would silently build an
// empty sidecar for every corpus under the file cap.
func TestContentScanCappedUnfilledMatchesLegacy(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	fx := buildScanTreeFixture(t, rnd)
	s := contentTestService(t)
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "scan.gsi"), fx.idx)
	releaseVolumeContentOnCleanup(t, vol)
	t.Setenv("SEEKFS_CONTENT_SCOPE", "explicit")
	t.Setenv("SEEKFS_CONTENT_ROOTS", fx.root)
	scope, ok := s.resolvedContentScope(vol)
	if !ok {
		t.Fatal("scope not resolvable")
	}
	opts := defaultContentBuildOptions()
	opts.Scope = &scope
	want := legacyScanSelection(fx.idx, opts, false)
	cfg := contentScopeScanConfig{scope: &scope, idx: fx.idx, maxFiles: 2000000,
		stopped: func() bool { return false }, genChanged: func() bool { return false }}
	lists, ok := scanScopeMembers(s, vol, cfg)
	if !ok {
		t.Fatal("fast scan moved")
	}
	items, limited := mergeScanMembers(lists, 2000000)
	if limited || len(items) != len(want) {
		t.Fatalf("unfilled capped merge = %d items limited=%v; want %d limited=false", len(items), limited, len(want))
	}
	for _, it := range items {
		w, ok := want[it.frn]
		if !ok || w.path != it.path || w.priority != it.priority {
			t.Fatalf("unfilled capped item %+v not in legacy set", it)
		}
	}
}

// Benchmark the selection scan on a synthetic 200k-record tree (nested dirs,
// mixed extensions, two real repos with ignore rules on disk): legacy loop
// vs fast scan.
func BenchmarkContentScanSelection(b *testing.B) {
	root := b.TempDir()
	for _, rel := range []string{"repoA", "repoB"} {
		if err := os.MkdirAll(filepath.Join(root, rel, ".git"), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel, ".gitignore"), []byte("*.log\n/out/\nbuild/\n"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	const dirs = 2000
	const filesPerDir = 100
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 100, Roots: []string{root}}
	records := []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: filepath.Base(root), Mode: uint32(os.ModeDir)}}
	frn := uint64(2)
	addDir := func(parent int32, name string) int32 {
		id := int32(len(records))
		records = append(records, CompactRecord{FRN: frn, ParentFRN: records[parent].FRN, Parent: parent, Name: name, Mode: uint32(os.ModeDir)})
		frn++
		return id
	}
	// Two synthetic repo subtrees whose .gitignore files exist on disk, so
	// pattern matching runs for real — plus 300 bare repo markers, so the
	// worktree lookup scales like a developer volume with hundreds of repos
	// (the per-file repoFor loop is the E: bottleneck).
	repoIDs := []int32{}
	for _, rel := range []string{"repoA", "repoB"} {
		rid := addDir(0, rel)
		addDir(rid, ".git")
		repoIDs = append(repoIDs, rid)
	}
	for i := 0; i < 300; i++ {
		rid := addDir(0, "zzrepo"+string(rune('a'+i%26))+string(rune('a'+(i/26)%26)))
		addDir(rid, ".git")
	}
	dirIDs := append([]int32{}, repoIDs...)
	exts := []string{".go", ".txt", ".md", ".png", ".log", ""}
	for d := 0; d < dirs; d++ {
		parent := repoIDs[d%len(repoIDs)]
		if d%7 == 0 {
			parent = 0
		} else if d%3 == 0 && len(dirIDs) > len(repoIDs) {
			parent = dirIDs[len(dirIDs)-1]
		}
		name := "node_modules"
		if d%5 != 0 {
			name = "srcdir"
		}
		id := addDir(parent, name)
		dirIDs = append(dirIDs, id)
		for f := 0; f < filesPerDir; f++ {
			records = append(records, CompactRecord{FRN: frn, ParentFRN: records[id].FRN, Parent: id, Name: "file.go", Size: 100})
			frn++
		}
	}
	// Vary names/extensions deterministically.
	for i := range records {
		if records[i].Mode&uint32(os.ModeDir) == 0 {
			records[i].Name = "f" + string(rune('a'+i%26)) + exts[i%len(exts)]
		}
	}
	idx.Records = records
	contentIndexFRNs(idx)
	b.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	b.Setenv("SEEKFS_CONTENT_SCOPE", "explicit")
	b.Setenv("SEEKFS_CONTENT_ROOTS", root)
	vol := newServiceVolumeIndex(filepath.Join(b.TempDir(), "bench.gsi"), idx)
	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)
	scope, ok := s.resolvedContentScope(vol)
	if !ok {
		b.Fatal("scope not resolvable")
	}
	opts := defaultContentBuildOptions()
	opts.Scope = &scope
	b.Run("legacy", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = legacyScanSelection(idx, opts, false)
		}
	})
	b.Run("fast", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			cfg := contentScopeScanConfig{scope: &scope, idx: idx,
				stopped: func() bool { return false }, genChanged: func() bool { return false }}
			lists, ok := scanScopeMembers(s, vol, cfg)
			if !ok {
				b.Fatal("moved")
			}
			n := 0
			for _, l := range lists {
				n += len(l)
			}
			if n == 0 {
				b.Fatal("empty scan")
			}
		}
	})
}
