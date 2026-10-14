package main

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Blind differential: random tiny direct indexes (real files, so --exists is
// deterministic), full service search+count vs the exhaustive oracle.
func TestShortTermBlindProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("blind probe")
	}
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	rng := rand.New(rand.NewSource(7))
	alpha := []string{"a", "b", "c", "go", "o", "r", "n", "x", "z", "Car", "GO", "log", "test", ".cc", ".go", "main", "README", "e", "ab", "qo", "deep", "keep", "pack", "caf\u00e9", "a.c", "x.y", "md"}
	terms := []string{"go", "o", "a", "e", "car", "log", "test", "cc", "main", "read", "ab", "qo", "deep", "keep", "pack", "caf\u00e9", "zz", "GO", "Car", "a.c", "x", "z", "md", "qo"}
	for iter := 0; iter < 60; iter++ {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "r")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		var recs []directRecord
		recs = append(recs, directRecord{FRN: 1, ParentFRN: 0, Mode: uint32(os.ModeDir), Name: "r", Path: root})
		names := map[string]bool{}
		frn := uint64(2)
		count := 2 + rng.Intn(9)
		for i := 0; i < count; i++ {
			nm := ""
			for k := 0; k < 1+rng.Intn(3); k++ {
				nm += alpha[rng.Intn(len(alpha))]
			}
			if names[nm] {
				continue
			}
			names[nm] = true
			p := filepath.Join(root, nm)
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			recs = append(recs, directRecord{FRN: frn, ParentFRN: 1, Size: info.Size(), ModUnix: info.ModTime().UnixNano(), Name: nm, Path: p})
			frn++
		}
		out := filepath.Join(tmp, "out", "b.gsi")
		if _, err := buildDirect(context.Background(), directBuildOptions{
			OutputPath: out, SpoolDir: filepath.Join(tmp, "out", "spool"),
			Roots: []string{root}, Volume: filepath.VolumeName(root), Source: "direct",
			BuiltAt: time.Unix(123, 0), Records: newDirectSliceSource(recs),
			RunRecords: 8, RunBytes: 4096,
		}); err != nil {
			t.Fatal(err)
		}
		idx, vol, err := loadConfiguredVolume(out)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer idx.MMapRecords.file.close()
			volumes := []*serviceVolumeIndex{vol}
			queries := []queryOptions{}
			for _, term := range terms {
				queries = append(queries,
					queryOptions{Query: term, Limit: 100},
					queryOptions{Query: term + " type:file", Limit: 100},
					queryOptions{Query: term, Limit: 100, Exists: true},
					queryOptions{Query: term, Limit: 100, MatchPath: true},
					queryOptions{Query: term, Limit: 100, Under: root},
				)
			}
			queries = append(queries,
				queryOptions{Query: "go log", Limit: 100, MatchPath: true},
				queryOptions{Query: "a e", Limit: 100, MatchPath: true},
			)
			pages := []queryOptions{}
			for _, term := range terms {
				pages = append(pages, queryOptions{Query: term, Limit: 2})
			}
			for _, opts := range queries {
				oracle, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
				if err != nil {
					t.Fatal(err)
				}
				copts := opts
				copts.Limit = 0
				copts.Trace = &searchTrace{}
				count, handled, err := countServiceVolumes(volumes, copts)
				if err != nil {
					t.Fatal(err)
				}
				if !handled {
					fb, err := searchServiceVolumes(volumes, copts, true)
					if err != nil {
						t.Fatal(err)
					}
					count, handled = len(fb), true
				}
				if count != len(oracle) {
					t.Fatalf("iter %d query %q exists=%v path=%v: count=%d oracle=%d (%v)", iter, opts.Query, opts.Exists, opts.MatchPath, count, len(oracle), pathsOf(oracle))
				}
				sopts := opts
				if len(oracle) > 0 {
					sopts.Limit = len(oracle)
				}
				sopts.Trace = &searchTrace{}
				got, err := searchServiceVolumes(volumes, sopts, false)
				if err != nil {
					t.Fatal(err)
				}
				if len(oracle) == 0 {
					for _, e := range got {
						pq, err := parseQuery(opts)
						if err != nil {
							t.Fatal(err)
						}
						if len(pq.Terms) == 1 && shortTermFuzzyLegit(pq.Terms[0], e.Name) {
							continue
						}
						t.Fatalf("iter %d query %q: zero-oracle non-typo hit %q", iter, opts.Query, e.Path)
					}
					continue
				}
				if !sameStringSet(pathsOf(got), pathsOf(oracle)) {
					t.Fatalf("iter %d query %q exists=%v path=%v: search=%v oracle=%v", iter, opts.Query, opts.Exists, opts.MatchPath, pathsOf(got), pathsOf(oracle))
				}
			}
			for _, opts := range pages {
				fullOpts := opts
				fullOpts.Limit = 100
				full, err := searchCompactWithCache(idx, fullOpts, false, make(map[int]string), nil)
				if err != nil {
					t.Fatal(err)
				}
				if len(full) < 2 {
					continue
				}
				got, err := searchServiceVolumes(volumes, opts, false)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != 2 {
					t.Fatalf("iter %d query %q: page len=%d want 2 (oracle %v)", iter, opts.Query, len(got), pathsOf(full))
				}
				wantSet := map[string]bool{}
				for _, e := range full {
					wantSet[e.Path] = true
				}
				for _, e := range got {
					if !wantSet[e.Path] {
						t.Fatalf("iter %d query %q: page hit %q outside oracle %v", iter, opts.Query, e.Path, pathsOf(full))
					}
				}
			}
		}()
		_ = sort.Strings
		_ = strings.Contains
	}
}
