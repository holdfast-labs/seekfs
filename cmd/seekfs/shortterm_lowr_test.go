package main

// LOWR wire-format exactness for direct-built indexes.
//
// Root cause of the lossy short-term postings (WORK_QUEUE item 0, bug 1):
// `seekfs direct` wrote the LOWR (folded-name) section as interleaved
// per-record (offset,length) tuples instead of the split offs[]/lens[]/blob
// layout every reader expects, so every token-addressed lower-name read on a
// memory-mapped direct index was garbage. The mapped bulk name scan
// (`scanCompactLowerNameTerm`) then underfilled, and every `planned:term:X`
// lane using that posting as a required source underfilled with it.

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type shortTermFixtureFile struct {
	rel     string
	content string
	dir     bool
}

// shortTermFixtureFiles is the tiny direct-index corpus. Names deliberately
// share short substrings ("go" in 20+ names), mix case (README.md exercises
// the folded-name blob), and nest under same-substring dirs (cargo/, gopher/).
func shortTermFixtureFiles() []shortTermFixtureFile {
	return []shortTermFixtureFile{
		{rel: "dir0", dir: true},
		{rel: "dir1", dir: true},
		{rel: "dir2", dir: true},
		{rel: "dir3", dir: true},
		{rel: "gopher", dir: true},
		{rel: "cargo", dir: true},
		{rel: "dir0/go", content: "go binary\n"},
		{rel: "dir0/argo.yaml", content: "argo\n"},
		{rel: "dir0/google.mock", content: "google\n"},
		{rel: "dir0/gopher.txt", content: "gopher\n"},
		{rel: "dir0/main.cc", content: "int main(){}\n"},
		{rel: "dir1/agent_main.cc", content: "agent\n"},
		{rel: "dir1/cargo.json", content: "{}\n"},
		{rel: "dir1/django.py", content: "django\n"},
		{rel: "dir1/ego.js", content: "ego\n"},
		{rel: "dir1/ergo.xml", content: "ergo\n"},
		{rel: "dir1/golem.go", content: "golem\n"},
		{rel: "dir2/category.md", content: "category\n"},
		{rel: "dir2/go.mod", content: "module go\n"},
		{rel: "dir2/golden.txt", content: "golden\n"},
		{rel: "dir2/login.html", content: "login\n"},
		{rel: "dir2/mongo.cfg", content: "mongo\n"},
		{rel: "dir3/README.md", content: "readme\n"},
		{rel: "dir3/algo.c", content: "algo\n"},
		{rel: "dir3/good.py", content: "good\n"},
		{rel: "dir3/logo.png", content: "logo\n"},
		{rel: "dir3/logout.js", content: "logout\n"},
		{rel: "dir3/ongoing.log", content: "ongoing\n"},
		{rel: "dir3/legato.ini", content: "legato\n"},
		{rel: "gopher/nest.txt", content: "nested\n"},
		{rel: "cargo/inner_go.txt", content: "inner\n"},
	}
}

// buildShortTermDirectFixture materializes the corpus under root (a real git
// repo: everything committed except modifiedFile which is edited after the
// commit and untrackedFile which is never added), then builds a direct v9
// index over it and returns the output path.
func buildShortTermDirectFixture(t *testing.T, root, modifiedRel, untrackedRel string) string {
	t.Helper()
	for _, f := range shortTermFixtureFiles() {
		p := filepath.Join(root, f.rel)
		if f.dir {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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
	for _, f := range shortTermFixtureFiles() {
		if f.dir || f.rel == untrackedRel {
			continue
		}
		if _, err := work.Add(filepath.ToSlash(f.rel)); err != nil {
			t.Fatalf("Add %s: %v", f.rel, err)
		}
	}
	if _, err := work.Commit("fixture", &git.CommitOptions{
		Author: &object.Signature{Name: "seekfs-test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Deterministic dirty state: one modified (size changes, so the index
	// stat fast path sees it), one untracked.
	modPath := filepath.Join(root, modifiedRel)
	mod, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modPath, append(mod, []byte("// edited after commit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	// directRecord spool in walk order: root, dirs, then files. FRNs are
	// dense from 1; the builder resolves parents from ParentFRN.
	type seed struct {
		frn, parentFRN uint64
		path           string
		name           string
		mode           uint32
	}
	seeds := []seed{{frn: 1, parentFRN: 0, path: root, name: filepath.Base(root), mode: uint32(os.ModeDir)}}
	frnByPath := map[string]uint64{root: 1}
	nextFRN := uint64(2)
	addDir := func(rel string) {
		p := filepath.Join(root, rel)
		parent := filepath.Dir(p)
		seeds = append(seeds, seed{frn: nextFRN, parentFRN: frnByPath[parent], path: p, name: filepath.Base(p), mode: uint32(os.ModeDir)})
		frnByPath[p] = nextFRN
		nextFRN++
	}
	for _, d := range []string{"dir0", "dir1", "dir2", "dir3", "gopher", "cargo"} {
		addDir(d)
	}
	records := make([]directRecord, 0, len(seeds)+32)
	for _, s := range seeds {
		records = append(records, directRecord{FRN: s.frn, ParentFRN: s.parentFRN, Mode: s.mode, Name: s.name, Path: s.path})
	}
	for _, f := range shortTermFixtureFiles() {
		if f.dir {
			continue
		}
		p := filepath.Join(root, f.rel)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, directRecord{
			FRN: nextFRN, ParentFRN: frnByPath[filepath.Dir(p)],
			Mode: 0, Size: info.Size(), ModUnix: info.ModTime().UnixNano(),
			Name: filepath.Base(p), Path: p,
		})
		nextFRN++
	}
	out := filepath.Join(root, "out", "shortterm.gsi")
	if _, err := buildDirect(context.Background(), directBuildOptions{
		OutputPath: out,
		SpoolDir:   filepath.Join(root, "out", "spool"),
		Roots:      []string{root},
		Volume:     filepath.VolumeName(root),
		Source:     "direct",
		BuiltAt:    time.Unix(123, 0),
		Records:    newDirectSliceSource(records),
		RunRecords: 8,
		RunBytes:   4096,
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestDirectLOWRSectionIsCanonical pins the LOWR wire format: split offs[],
// lens[], blob addressed by name token, identical to encodeLowerSection over
// the index's own token names (direct builds use one token per record, in
// record order).
func TestDirectLOWRSectionIsCanonical(t *testing.T) {
	root := t.TempDir()
	out := buildShortTermDirectFixture(t, root, filepath.Join("dir0", "go"), filepath.Join("dir3", "ongoing.log"))
	raw := directTestSectionData(t, out, indexSectionLOWR)
	blob, offs, lens := decodeLowerSection(raw)
	idx, err := loadIndexMMap(out)
	if err != nil {
		t.Fatalf("loadIndexMMap: %v", err)
	}
	defer idx.MMapRecords.file.close()
	m := idx.MMapRecords
	if m == nil {
		t.Fatal("direct index did not load memory-mapped")
	}
	tokens := len(m.tokenTable) / 6
	if len(offs) != tokens || len(lens) != tokens {
		t.Fatalf("LOWR arrays = %d/%d, want %d tokens", len(offs), len(lens), tokens)
	}
	// Reference: canonical encoding of the token names in token order.
	names := make([]string, tokens)
	for i := range names {
		name, _ := m.nameByID(uint32(i))
		names[i] = name
	}
	ref := encodeLowerSection(names)
	refBlob, refOffs, refLens := decodeLowerSection(ref)
	if !reflect.DeepEqual(offs, refOffs) || !reflect.DeepEqual(lens, refLens) || !reflect.DeepEqual(blob, refBlob) {
		t.Fatalf("direct LOWR != canonical encoding (offs=%v lens=%v blob=%q)", offs, lens, string(blob))
	}
	// Invariant every consumer relies on: each offset is either SameAsName
	// or a live blob range.
	for i, off := range offs {
		if off == packedLowerSameAsName {
			continue
		}
		if end := int(off) + int(lens[i]); end < int(off) || end > len(blob) {
			t.Fatalf("LOWR token %d escapes the blob (off=%d len=%d blob=%d)", i, off, lens[i], len(blob))
		}
	}
	// The mapped bulk name scan must equal the brute-force name scan.
	for _, term := range []string{"go", "o", "argo", "readme", "zzz"} {
		fast, ok := idx.scanCompactLowerNameTerm(term)
		if !ok {
			t.Fatalf("scanCompactLowerNameTerm(%q) declined", term)
		}
		var want []int
		for id := 0; id < idx.compactRecordCount(); id++ {
			rec := idx.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if strings.Contains(idx.compactLowerNameAt(id), term) {
				want = append(want, id)
			}
		}
		sort.Ints(fast)
		if len(fast) != len(want) {
			t.Fatalf("scanCompactLowerNameTerm(%q) = %v, want %v", term, fast, want)
		}
		for i := range fast {
			if fast[i] != want[i] {
				t.Fatalf("scanCompactLowerNameTerm(%q) = %v, want %v", term, fast, want)
			}
		}
	}
}
