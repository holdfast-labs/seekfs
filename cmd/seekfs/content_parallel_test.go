package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
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
	// v10 verifies from disk: materialize the synthetic corpus as real
	// files and index their absolute paths. FRNs stay unique and ascending,
	// so DocID order still matches input order.
	dir := t.TempDir()
	for i := range docs {
		p := filepath.Join(dir, filepath.FromSlash(docs[i].path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, docs[i].text, 0o644); err != nil {
			t.Fatal(err)
		}
		docs[i].path = p
	}
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

// Disk verification is deterministic: the same query twice returns identical
// hits in identical order with completion, across term shapes and limits.
func TestContentDiskVerifyIsDeterministic(t *testing.T) {
	r := parallelTestReader(t, parallelTestCorpus(42, 600))
	terms := []string{
		"needle", "mutex", "struct", "uniqueword37", "uniqueword599",
		"missingword", "ß", "a", "e", "lorem ipsum", "MÜNCHEN",
		"need", "eedl", "dle", "STRAßE", "straße",
	}
	limits := []int{0, 1, 2, 5, 20, 1000}
	for _, term := range terms {
		for _, limit := range limits {
			first, completeFirst := r.search(term, limit)
			second, completeSecond := r.search(term, limit)
			if completeFirst != completeSecond || !reflect.DeepEqual(first, second) {
				t.Fatalf("term=%q limit=%d: runs differ (%d/%v vs %d/%v)",
					term, limit, len(first), completeFirst, len(second), completeSecond)
			}
		}
	}
	if got, complete := r.search("", 0); got != nil || !complete {
		t.Fatalf("empty term: got %d hits complete=%v; want nil/true", len(got), complete)
	}
}
