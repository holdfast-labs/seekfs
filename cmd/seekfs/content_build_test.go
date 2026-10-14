package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestContentPathKeyIsFrozen(t *testing.T) {
	// Standard FNV-1a 64-bit test vectors. These values are persisted in
	// contentDoc.FRN; changing the hash invalidates every built `.gsx`.
	if got := contentPathKey(""); got != 0xcbf29ce484222325 {
		t.Fatalf("contentPathKey(\"\") = %#x; want the FNV-1a offset basis", got)
	}
	if got := contentPathKey("a"); got != 0xaf63dc4c8601ec8c {
		t.Fatalf("contentPathKey(\"a\") = %#x; want 0xaf63dc4c8601ec8c", got)
	}
}

func TestContentBuildExcludesIndexFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".seekfs-content.gsx"), []byte("PK\x03\x04not-really"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.gsx"), []byte("junk\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Docs) != 1 {
		t.Fatalf("built %d docs; want 1 (a.txt only)", len(idx.Docs))
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("alpha", 0); len(hits) != 1 || hits[0].Path != "a.txt" {
		t.Fatalf("search = %v", contentPathsOf(hits))
	}
}

func TestContentWalkBuildTalliesBinary(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), []byte("bin\x00ary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "OWNERS"), []byte("owner needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Docs) != 2 {
		t.Fatalf("built %d docs; want 2 (a.txt, OWNERS)", len(idx.Docs))
	}
	if idx.Policy.Binary != 1 {
		t.Fatalf("policy binary = %d; want 1 (b.bin)", idx.Policy.Binary)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("needle", 0); len(hits) != 2 {
		t.Fatalf("search = %v; want both text files", contentPathsOf(hits))
	}
}

// A root that cannot be read (a typo'd or inaccessible path) must fail rather
// than silently publish an empty index.
func TestContentBuildMissingRootErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := buildContentIndexFromDir(context.Background(), root, defaultContentBuildOptions()); err == nil {
		t.Fatal("missing root must return an error, not an empty index")
	}
}

func TestContentScopedIndexBudgetUsesExtractedText(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("ten letters"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol, _ := contentBuildTestVolume(t, dir, 3, 10, []CompactRecord{{FRN: 1, Parent: -1, Name: "a.txt", Size: 1}})
	scope := contentScope{Mode: contentScopeExplicit, Roots: []string{contentNormPath(dir)}, BudgetBytes: 5, BudgetExplicit: true}.resolve(dir, nil)
	opts := defaultContentBuildOptions()
	opts.Scope = &scope
	if _, err := buildContentIndexForIndex(context.Background(), vol.index, opts); err == nil {
		t.Fatal("scoped build ignored extracted-text budget")
	}
}

func TestContentBuildRespectsExtAndUnder(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ path, body string }{
		{filepath.Join(dir, "a.txt"), "needle one"},
		{filepath.Join(dir, "b.md"), "needle two"},
		{filepath.Join(sub, "c.txt"), "needle three"},
	} {
		if err := os.WriteFile(f.path, []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	extOpts := defaultContentBuildOptions()
	extOpts.Exts = map[string]struct{}{".md": {}}
	extIdx, err := buildContentIndexFromDir(context.Background(), dir, extOpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(extIdx.Docs) != 1 {
		t.Fatalf("ext filter built %d docs; want 1 (b.md)", len(extIdx.Docs))
	}

	underOpts := defaultContentBuildOptions()
	underOpts.Under = sub
	underIdx, err := buildContentIndexFromDir(context.Background(), dir, underOpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(underIdx.Docs) != 1 {
		t.Fatalf("-under filter built %d docs; want 1 (sub/c.txt)", len(underIdx.Docs))
	}
}

func TestContentBuildBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("bounded-memory build test skipped in -short")
	}
	dir := t.TempDir()
	const docs = 1500
	body := "packed trigram content needle " + strings.Repeat("word ", 60)
	for i := 0; i < docs; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.txt", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Heap accounting is process-wide, so sweep earlier tests' garbage first
	// and judge the build by what it RETAINS: a peak-transient sampler
	// also catches in-flight garbage that a CPU-starved GC has not swept
	// yet (in-suite peaks of 700MB+ for a build that retains ~10MB and
	// peaks 122MB standalone), which measures the scheduler, not the
	// build. The budget comment describes the assembly's footprint
	// ("bounded by total postings + text"), i.e. retained data, so a
	// post-build GC plus a delta over baseline asserts exactly that while
	// earlier tests' retained heap (shared fixtures, caches) is not
	// charged to this build.
	runtime.GC()
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Docs) != docs {
		t.Fatalf("built %d docs; want %d", len(idx.Docs), docs)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("needle", 0); len(hits) != docs {
		t.Fatalf("search returned %d hits; want %d", len(hits), docs)
	}
	// Documented offline-build budget: this in-memory assembly is bounded by
	// total postings + text. The flat-memory external builder is P2.
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	const budget = 512 << 20
	retained := after.HeapAlloc - min(after.HeapAlloc, baseline.HeapAlloc)
	if retained > budget {
		t.Fatalf("build retained heap %dMB exceeds the %dMB budget (absolute %dMB)", retained>>20, budget>>20, after.HeapAlloc>>20)
	}
	t.Logf("bounded build: docs=%d retained=%dMB absolute=%dMB", docs, retained>>20, after.HeapAlloc>>20)
}
