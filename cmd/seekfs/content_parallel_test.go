package main

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// parallelTestCorpus builds a deterministic synthetic corpus stressing the
// verifier: common words (dense postings), rare words, unicode fold edges
// (İ/ß/KELVIN change byte length when folded), empty docs (skipped like the
// real builder skips them — here kept as zero-length to pin the empty path),
// and one multi-block giant.
func parallelTestCorpus(seed int64, n int) []contentBuildDoc {
	rng := rand.New(rand.NewSource(seed))
	words := []string{"needle", "mutex", "struct", "lorem", "ipsum", "quasar", "zxqywidth", "ffi", "straße", "KELVIN", "İstanbul"}
	docs := make([]contentBuildDoc, 0, n)
	for i := 0; i < n; i++ {
		var b strings.Builder
		nw := 5 + rng.Intn(40)
		for w := 0; w < nw; w++ {
			b.WriteString(words[rng.Intn(len(words))])
			b.WriteByte(' ')
		}
		if i%37 == 0 {
			b.WriteString(" uniqueword" + fmt.Sprint(i))
		}
		if i%11 == 0 {
			b.WriteString(" MÜNCHEN café naïve ")
		}
		body := b.String()
		if i%13 == 0 {
			body = ""
		}
		docs = append(docs, contentBuildDoc{
			path: fmt.Sprintf("sub/dir%02d/file%04d.txt", i%17, i),
			frn:  uint64(5000 + i*7),
			text: []byte(body),
		})
	}
	// One giant spanning many blocks, one empty.
	docs = append(docs, contentBuildDoc{path: "big/huge.txt", frn: 999001, text: []byte(strings.Repeat("needle haystack ", 40000))})
	docs = append(docs, contentBuildDoc{path: "big/empty.txt", frn: 999002, text: []byte{}})
	return docs
}

func parallelTestReader(t *testing.T, docs []contentBuildDoc) *contentReader {
	t.Helper()
	idx, err := assembleContentIndex(docs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Serial and parallel search must agree exactly — paths, docIDs, offsets,
// snippets — for every term shape, limit, and worker count. workers=1 pins
// the in-parallel serial fallback.
func TestContentParallelMatchesSerial(t *testing.T) {
	r := parallelTestReader(t, parallelTestCorpus(42, 600))
	terms := []string{
		"needle", "mutex", "struct", "uniqueword37", "uniqueword599",
		"missingword", "ß", "a", "e", "", "lorem ipsum", "MÜNCHEN",
		"need", "eedl", "dle", "STRAßE", "straße",
	}
	limits := []int{0, 1, 2, 5, 20, 1000}
	workers := []int{1, 2, 3, 7, 8}
	for _, term := range terms {
		tFold := strings.ToLower(term)
		if term == "" {
			continue
		}
		want := r.searchSerial(tFold, 0)
		for _, limit := range limits {
			wantLim := want
			if limit > 0 && len(wantLim) > limit {
				wantLim = wantLim[:limit]
			}
			for _, w := range workers {
				got := r.searchParallel(tFold, limit, w)
				if !reflect.DeepEqual(got, wantLim) {
					t.Fatalf("term=%q limit=%d workers=%d: parallel=%d hits, serial=%d hits (first diff below)",
						term, limit, w, len(got), len(wantLim))
				}
			}
		}
	}
	// Empty term: all arms nil (also covered by the loop above skipping
	// "", asserted here directly against both arms).
	for _, w := range workers {
		if got := r.searchParallel("", 0, w); got != nil {
			t.Fatalf("empty term workers=%d: got %d hits; want nil", w, len(got))
		}
	}
	if got := r.searchSerial("", 0); got != nil {
		t.Fatalf("empty term serial: got %d hits; want nil", len(got))
	}
	if got := r.search("", 0); got != nil {
		t.Fatalf("empty term dispatch: got %d hits; want nil", len(got))
	}
}

// The dispatcher picks serial for one worker and parallel otherwise, with
// identical results either way.
func TestContentSearchDispatchMatches(t *testing.T) {
	r := parallelTestReader(t, parallelTestCorpus(7, 200))
	for _, term := range []string{"needle", "missingword", "uniqueword37", "e"} {
		want := r.searchSerial(strings.ToLower(term), 7)
		if got := r.search(term, 7); !reflect.DeepEqual(got, want) {
			t.Fatalf("term=%q: search=%v serial=%v", term, len(got), len(want))
		}
	}
}
