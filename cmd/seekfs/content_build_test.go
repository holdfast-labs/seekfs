package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// A root that cannot be read (a typo'd or inaccessible path) must fail rather
// than silently publish an empty index.
func TestContentBuildMissingRootErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := buildContentIndexFromDir(context.Background(), root, defaultContentBuildOptions()); err == nil {
		t.Fatal("missing root must return an error, not an empty index")
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

	var peak uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	close(stop)
	<-done
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
	const budget = 512 << 20
	if peak > budget {
		t.Fatalf("build peak heap %dMB exceeds the %dMB budget", peak>>20, budget>>20)
	}
	t.Logf("bounded build: docs=%d peak_heap=%dMB", docs, peak>>20)
}
