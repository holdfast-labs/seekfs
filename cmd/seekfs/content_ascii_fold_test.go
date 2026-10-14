package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// asciiFoldCorpus exercises the ASCII fast path's edge cases: pure ASCII,
// case variants, and the non-ASCII runes where folding changes byte length
// (İ 2->3, ß/ẞ, KELVIN SIGN) or meaning, plus CJK/emoji and control bytes.
func asciiFoldCorpus() [][]byte {
	base := [][]byte{
		[]byte(""),
		[]byte("mutex MUTEX Mutex MuTeX"),
		[]byte("the STRASSE and straße and STRAßE"),
		[]byte("KELVIN k K \u212a"),
		[]byte("İ i I ı"),
		[]byte("café CAFÉ Caf\u00e9"),
		[]byte("struct STRUCT StrUcT"),
		[]byte("日本語テスト content 日本語"),
		[]byte("emoji \U0001F600 grin \U0001F600"),
		[]byte("a\x01b\x02c\x7fd"),
		[]byte("short"),
		[]byte("MiXeD CaSe DoC with \"reassemble\" inside REASSEMBLE"),
	}
	// Deterministic pseudo-random docs over a mixed alphabet.
	rng := rand.New(rand.NewSource(20261013))
	alpha := []rune("abABcCdDeEfFgG 012 _-İßé日\U0001F600")
	for d := 0; d < 60; d++ {
		n := 1 + rng.Intn(120)
		rs := make([]rune, n)
		for i := range rs {
			rs[i] = alpha[rng.Intn(len(alpha))]
		}
		base = append(base, []byte(string(rs)))
	}
	return base
}

func asciiFoldNeedles() [][]byte {
	out := [][]byte{
		[]byte("mutex"), []byte("MUTEX"), []byte("strasse"), []byte("STRASSE"),
		[]byte("k"), []byte("K"), []byte("i"), []byte("é"), []byte("É"),
		[]byte("café"), []byte("日本"), []byte("\U0001F600"), []byte("a"),
		[]byte("reassemble"), []byte("struct"), []byte("zznope"),
		[]byte("verylongneedlethatmatchesnothinginthedocs"),
	}
	rng := rand.New(rand.NewSource(77))
	alpha := []rune("abABeE 012_-é")
	for d := 0; d < 40; d++ {
		n := 1 + rng.Intn(9)
		rs := make([]rune, n)
		for i := range rs {
			rs[i] = alpha[rng.Intn(len(alpha))]
		}
		out = append(out, []byte(string(rs)))
	}
	return out
}

// TestAsciiCaseIndexMatchesFold proves the allocation-free ASCII search
// decides exactly like fold-then-substring: a hit here is always a true
// match at the identical offset, and a miss with ascii=true means the fold
// would miss too (only non-ASCII raw defers to the fold).
func TestAsciiCaseIndexMatchesFold(t *testing.T) {
	for _, raw := range asciiFoldCorpus() {
		for _, needleRaw := range asciiFoldNeedles() {
			needle := contentFoldText(needleRaw)
			got, ascii := asciiCaseIndex(raw, needle)
			folded := contentFoldText(raw)
			want := bytes.Index(folded, needle)
			if !isASCIIBytes(needle) {
				// Non-ASCII needles never take the fast path in production;
				// only assert the ASCII verdict is exact here.
				if ascii != isASCIIBytes(raw) {
					t.Fatalf("needle %q raw %q: ascii=%v; want %v", needle, raw, ascii, isASCIIBytes(raw))
				}
				continue
			}
			if !ascii {
				if isASCIIBytes(raw) {
					t.Fatalf("needle %q raw %q: ascii=false on ASCII text", needle, raw)
				}
				// Non-ASCII raw: the fold decides and the fast offset is
				// unused (a fold expansion like İ→i̇ can match earlier
				// than any ASCII match). The fallback's own equality is
				// covered by TestMatchRawFirstEqualsMatch. A fast hit
				// must still be a true hit, though.
				if got >= 0 && want < 0 {
					t.Fatalf("needle %q raw %q: fast hit=%d but fold misses", needle, raw, got)
				}
				continue
			}
			if (got >= 0) != (want >= 0) {
				t.Fatalf("needle %q raw %q: ascii=%d want=%d", needle, raw, got, want)
			}
			if got >= 0 && got != want {
				t.Fatalf("needle %q raw %q: ascii offset=%d fold offset=%d", needle, raw, got, want)
			}
		}
	}
}

// TestMatchRawFirstEqualsMatch proves the service verifier's fast path
// decides exactly like the historical match on the same corpus.
func TestMatchRawFirstEqualsMatch(t *testing.T) {
	for _, raw := range asciiFoldCorpus() {
		for _, needleRaw := range asciiFoldNeedles() {
			leaf := contentLeaf{Kind: contentLeafTerm, Text: string(needleRaw), LeafID: 0}
			pq := parsedQuery{Content: []contentLeaf{leaf}}
			m := newContentLeafMatcher(pq)
			folded := contentFoldText(raw)
			want := m.match(raw, folded, leaf)
			var lazy []byte
			ready := false
			if got := m.matchRawFirst(raw, &lazy, &ready, leaf); got != want {
				t.Fatalf("needle %q raw %q: matchRawFirst=%v match=%v", needleRaw, raw, got, want)
			}
			// A pure-ASCII miss must not fold at all.
			if want == false && isASCIIBytes(raw) && isASCIIBytes(m.needleOf(leaf)) && ready {
				t.Fatalf("needle %q raw %q: folded a pure-ASCII miss", needleRaw, raw)
			}
		}
	}
}

// TestContentSnippetASCIIEquals proves the snippet fast path renders
// exactly what the fold-and-map path renders.
func TestContentSnippetASCIIEquals(t *testing.T) {
	for _, raw := range asciiFoldCorpus() {
		for _, needleRaw := range asciiFoldNeedles() {
			leaf := contentLeaf{Kind: contentLeafTerm, Text: string(needleRaw), LeafID: 0}
			pq := parsedQuery{Content: []contentLeaf{leaf}}
			m := newContentLeafMatcher(pq)
			folded := contentFoldText(raw)
			got := contentSnippet(raw, folded, []contentLeaf{leaf}, m)
			// Reference: the historical slow path, spelled out.
			want := ""
			if m.match(raw, folded, leaf) {
				needle := m.needleOf(leaf)
				off := contentLeafFirstOffset(m.haystackFor(raw, folded), needle)
				if off >= 0 {
					if m.caseSensitive {
						want = contentSnippetWindow(raw, off, len(needle))
					} else {
						rawOff, rawEnd := contentFoldSpanToRaw(raw, folded, off, off+len(needle))
						want = contentSnippetWindow(raw, rawOff, rawEnd-rawOff)
					}
				}
			}
			if got != want {
				t.Fatalf("needle %q raw %q: snippet=%q want %q", needleRaw, raw, got, want)
			}
		}
	}
}

// TestVerifyDocContentASCIIEquals proves the offline reader's fast path
// returns the identical hit (offset and snippet) as the fold path. v11
// verifies from disk, so the corpus is written to real files first.
func TestVerifyDocContentASCIIEquals(t *testing.T) {
	dir := t.TempDir()
	raws := asciiFoldCorpus()
	var build []contentBuildDoc
	for i, raw := range raws {
		name := fmt.Sprintf("f%03d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, name), append([]byte(nil), raw...), 0o644); err != nil {
			t.Fatal(err)
		}
		build = append(build, contentBuildDoc{
			path: filepath.Join(dir, name),
			frn:  uint64(1000 + i),
			text: append([]byte(nil), raw...),
		})
	}
	idx, err := assembleContentIndex(build, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	for _, needleRaw := range asciiFoldNeedles() {
		term := string(contentFoldText(needleRaw))
		if term == "" {
			continue
		}
		hits, complete := r.search(term, 0)
		if !complete {
			t.Fatalf("needle %q: tiny corpus hit the disk cap", term)
		}
		for _, h := range hits {
			// Reference decision + offset + snippet, spelled out slowly.
			text := raws[int(h.DocID)]
			lower := contentFoldText(text)
			off := bytes.Index(lower, []byte(term))
			if off < 0 {
				t.Fatalf("needle %q doc %d: hit but slow path misses", term, h.DocID)
			}
			rawOff, rawEnd := contentFoldSpanToRaw(text, lower, off, off+len(term))
			if h.Offset != rawOff {
				t.Fatalf("needle %q doc %d: offset=%d want %d", term, h.DocID, h.Offset, rawOff)
			}
			if want := contentSnippetWindow(text, rawOff, rawEnd-rawOff); h.Snippet != want {
				t.Fatalf("needle %q doc %d: snippet=%q want %q", term, h.DocID, h.Snippet, want)
			}
		}
		// And the slow decision agrees on the miss set: every doc the fast
		// path skipped must miss the slow test too.
		hitSet := make(map[uint32]bool, len(hits))
		for _, h := range hits {
			hitSet[h.DocID] = true
		}
		for id := range build {
			if hitSet[uint32(id)] {
				continue
			}
			text := raws[id]
			if len(text) == 0 {
				continue
			}
			if bytes.Contains(contentFoldText(text), []byte(term)) {
				t.Fatalf("needle %q doc %d: slow path hits but fast path missed", term, id)
			}
		}
	}
}
