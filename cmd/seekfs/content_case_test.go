package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PF-6a: a case-sensitive content leaf (case:true) matches only the exact case;
// the default remains case-insensitive and matches both spellings.
func TestContentServiceCaseSensitiveTermLeaf(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "upper.txt", "xx Needle yy"},
		{3, "lower.txt", "xx needle yy"},
	})

	exact, err := contentServiceSearch(t, vol, "case:true content:Needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(exact); len(got) != 1 || got[0] != "upper.txt" {
		t.Fatalf("case:true content:Needle = %v; want [upper.txt]", got)
	}

	both, err := contentServiceSearch(t, vol, "content:Needle", false)
	if err != nil {
		t.Fatal(err)
	}
	got := namesOf(both)
	if len(got) != 2 || got[0] != "lower.txt" || got[1] != "upper.txt" {
		t.Fatalf("default content:Needle = %v; want [lower.txt upper.txt]", got)
	}

	none, err := contentServiceSearch(t, vol, "case:true content:NEEDLE", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("case:true content:NEEDLE = %v; want no matches", namesOf(none))
	}
}

// PF-6a: the regex leaf carries the query's case policy. The case-sensitive form
// respects case; the default compiles with (?i) and matches either.
func TestContentServiceCaseSensitiveRegexLeaf(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "upper.txt", "xx Needle yy"},
		{3, "lower.txt", "xx needle yy"},
	})

	exact, err := contentServiceSearch(t, vol, "case:true content:/Needle/", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(exact); len(got) != 1 || got[0] != "upper.txt" {
		t.Fatalf("case:true content:/Needle/ = %v; want [upper.txt]", got)
	}

	both, err := contentServiceSearch(t, vol, "content:/Needle/", false)
	if err != nil {
		t.Fatal(err)
	}
	got := namesOf(both)
	if len(got) != 2 || got[0] != "lower.txt" || got[1] != "upper.txt" {
		t.Fatalf("default content:/Needle/ = %v; want [lower.txt upper.txt]", got)
	}
}

// PF-6a: the prefilter folds with the same Unicode-aware rule as the index, so a
// case-sensitive query still reaches the exact-case match even when another
// document differs only in the case of an accented rune. ASCII-only folding
// would fold "CAFÉ" and "Café" to different byte grams and lose the candidate.
func TestContentServiceUnicodeFoldPrefilterSuperset(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "lower.txt", "le Café ici"},
		{3, "upper.txt", "le CAFÉ ici"},
		{4, "other.txt", "nothing here"},
	})

	exactLower, err := contentServiceSearch(t, vol, "case:true content:Café", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(exactLower); len(got) != 1 || got[0] != "lower.txt" {
		t.Fatalf("case:true content:Café = %v; want [lower.txt]", got)
	}

	exactUpper, err := contentServiceSearch(t, vol, "case:true content:CAFÉ", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(exactUpper); len(got) != 1 || got[0] != "upper.txt" {
		t.Fatalf("case:true content:CAFÉ = %v; want [upper.txt]", got)
	}

	// The insensitive form folds both spellings to the same bytes, proving the
	// index fold is Unicode-aware and not a byte-truncating ASCII lower.
	both, err := contentServiceSearch(t, vol, "content:Café", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(both); len(got) != 2 || got[0] != "lower.txt" || got[1] != "upper.txt" {
		t.Fatalf("content:Café = %v; want [lower.txt upper.txt]", got)
	}
}

// PF-6a: snippets are rendered from the case-preserving store, so the matched
// text keeps its original case.
func TestContentServiceSnippetPreservesCase(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "doc.txt", "xx Needle In A Haystack yy"},
	})
	for _, query := range []string{"content:needle", "case:true content:Needle"} {
		matches, err := contentServiceSearch(t, vol, query, false)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if len(matches) != 1 || !strings.Contains(matches[0].Snippet, "Needle") {
			t.Fatalf("%s snippet = %q; want original-case Needle", query, matches[0].Snippet)
		}
	}
}

// PF-6a: a v3 sidecar (case-folded text store) fails closed, while a v4 one
// round-trips. The version and magic are both bumped, so neither a stale magic
// nor a stale version can be read as case-preserving.
func TestContentIndexV4RoundTripAndV3Rejected(t *testing.T) {
	build := []contentBuildDoc{{frn: 7, path: "C:\\a.txt", text: []byte("Alpha Needle Beta")}}
	idx, err := assembleContentIndex(build, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if idx.Version != contentIndexVersion {
		t.Fatalf("built version = %d; want %d", idx.Version, contentIndexVersion)
	}
	encoded := contentIndexEncode(idx)

	decoded, err := contentIndexDecode(encoded)
	if err != nil {
		t.Fatalf("v4 round-trip failed: %v", err)
	}
	reader, err := openContentReader(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.docText(0); string(got) != "Alpha Needle Beta" {
		t.Fatalf("v4 stored text = %q; want case-preserved %q", got, "Alpha Needle Beta")
	}

	v3 := append([]byte(nil), encoded...)
	copy(v3[0:8], []byte("GOSCX003"))
	binary.LittleEndian.PutUint32(v3[8:], contentIndexVersion-1)
	if _, err := contentIndexDecode(v3); err == nil {
		t.Fatal("a v3 sidecar must be rejected (fail closed), not read as case-preserving")
	}
}

// PF-6a: an edit that changes only case is a change (the hash is over the
// case-preserving text) and is found case-sensitively once it lands in the
// delta, while the stale exact-case leaf no longer matches.
func TestContentDeltaCaseOnlyEditDetectedAndSearchable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.txt")
	if err := os.WriteFile(path, []byte("needle lower"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, changed, err := contentExtractDeltaDoc(2, path, [contentHashLen]byte{}, false)
	if err != nil || !changed {
		t.Fatalf("base extract = %+v changed=%v err=%v", base, changed, err)
	}
	if err := os.WriteFile(path, []byte("NEEDLE lower"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, changed, err := contentExtractDeltaDoc(2, path, base.Hash, true)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a case-only edit must be detected as a change")
	}

	vol := newContentQueryVolume(t, []contentFixtureFile{{2, "doc.txt", "placeholder"}})
	vol.content.deltaView().upsert(d)

	lower, err := contentServiceSearch(t, vol, "case:true content:needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(lower) != 0 {
		t.Fatalf("case:true content:needle matched after an upper-only edit: %v", namesOf(lower))
	}
	upper, err := contentServiceSearch(t, vol, "case:true content:NEEDLE", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(upper); len(got) != 1 || got[0] != "doc.txt" {
		t.Fatalf("case:true content:NEEDLE = %v; want [doc.txt]", got)
	}
	insensitive, err := contentServiceSearch(t, vol, "content:needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(insensitive) != 1 {
		t.Fatalf("default content:needle = %v; want one match", namesOf(insensitive))
	}
	if !bytes.Contains(d.Text, []byte("NEEDLE")) {
		t.Fatalf("delta stored text %q is not case-preserving", d.Text)
	}
}
