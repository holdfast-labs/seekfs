package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Randomized lane fuzz over tiny direct indexes: the ranked-posting filename
// lane (and its count twin) must equal brute force for every term/limit.
func TestShortTermRankedLaneFuzz(t *testing.T) {
	if testing.Short() {
		t.Skip("fuzz probe")
	}
	rng := rand.New(rand.NewSource(42))
	alpha := []string{"a", "b", "c", "go", "o", "r", "n", "x", "z", "Car", "GO", "log", "test", ".cc", ".go", "main", "README"}
	bad := 0
	for iter := 0; iter < 60; iter++ {
		names := map[string]bool{".": true}
		var recs []directRecord
		dir := fmt.Sprintf("d%d", iter)
		recs = append(recs, directRecord{FRN: 1, ParentFRN: 0, Mode: uint32(os.ModeDir), Name: "root", Path: "R:"})
		recs = append(recs, directRecord{FRN: 2, ParentFRN: 1, Mode: uint32(os.ModeDir), Name: dir, Path: "R:" + dir})
		count := 4 + rng.Intn(9)
		for i := 0; i < count; i++ {
			nm := ""
			for k := 0; k < 1+rng.Intn(3); k++ {
				nm += alpha[rng.Intn(len(alpha))]
			}
			if names[nm] {
				continue
			}
			names[nm] = true
			recs = append(recs, directRecord{FRN: uint64(10 + i), ParentFRN: 2, Name: nm, Path: "R:" + dir + "/" + nm, Size: int64(i + 1), ModUnix: time.Unix(1000+int64(i), 0).UnixNano()})
		}
		tmp := t.TempDir()
		out := filepath.Join(tmp, "f.gsi")
		if _, err := buildDirect(context.Background(), directBuildOptions{
			OutputPath: out, SpoolDir: filepath.Join(tmp, "spool"),
			Roots: []string{"R:"}, Volume: "R:", Source: "direct",
			BuiltAt: time.Unix(123, 0), Records: newDirectSliceSource(recs),
			RunRecords: 8, RunBytes: 4096,
		}); err != nil {
			t.Fatal(err)
		}
		idx, err := loadIndexMMap(out)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer idx.MMapRecords.file.close()
			vol := newServiceVolumeIndex(out, idx)
			terms := []string{"go", "o", "a", "car", "log", "test", "cc", "main", "read", "GO", "Car", "x", "zz", "o"}
			for _, term := range terms {
				truth := []int{}
				for id := 0; id < idx.compactRecordCount(); id++ {
					rec := idx.compactRecord(id)
					if rec.Deleted {
						continue
					}
					if strings.Contains(strings.ToLower(idx.compactNameAt(id)), strings.ToLower(term)) {
						truth = append(truth, id)
					}
				}
				sort.Ints(truth)
				for _, limit := range []int{1, 2, 5, 100} {
					pq, err := parseQuery(queryOptions{Query: term, Limit: limit})
					if err != nil {
						t.Fatal(err)
					}
					pq.Limit = limit
					pq.Trace = &searchTrace{}
					outRanked, ok := vol.completeFilenameRankedPosting(term, limit, pq)
					if !ok {
						continue
					}
					for _, id := range outRanked {
						found := false
						for _, w := range truth {
							if w == id {
								found = true
								break
							}
						}
						if !found {
							t.Fatalf("iter %d term %q limit %d: ranked lane emitted non-match id=%d name=%q truth=%v", iter, term, limit, id, idx.compactNameAt(id), truth)
							bad++
						}
					}
					if len(truth) >= limit && len(outRanked) != limit {
						t.Fatalf("iter %d term %q limit %d: ranked lane underfilled %d of %d (truth %v)", iter, term, limit, len(outRanked), limit, truth)
					}
					// Full service top-1: with the page filled by exact hits
					// there is no fuzzy room, so the single result must be a
					// true match.
					if len(truth) > 0 {
						got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: term, Limit: 1}, false)
						if err != nil {
							t.Fatal(err)
						}
						if len(got) != 1 {
							t.Fatalf("iter %d term %q: top-1 returned %d hits, want 1", iter, term, len(got))
						}
						if !strings.Contains(strings.ToLower(got[0].Name), strings.ToLower(term)) {
							t.Fatalf("iter %d term %q: top-1 %q is a non-match (truth %v)", iter, term, got[0].Name, truth)
						}
					}
				}
			}
		}()
	}
	if bad > 0 {
		t.Fatalf("%d false positives", bad)
	}
}
