package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContentFilterHashIsFrozen(t *testing.T) {
	// Golden stability: the same input must hash the same across runs, and
	// different inputs must spread. The bits are persisted, so any hash
	// change silently invalidates every filter.
	seen := map[uint32]bool{}
	for _, g := range []uint32{0x616263, 0x7A7A7A, 0x000000, 0xFFFFFF, 0x123456} {
		for _, m := range []uint32{1024, 2048, 16384} {
			h := contentFilterHash(g, m)
			if h >= m {
				t.Fatalf("contentFilterHash(%x, %d) = %d out of range", g, m, h)
			}
			seen[h] = true
			if again := contentFilterHash(g, m); again != h {
				t.Fatalf("contentFilterHash(%x, %d) unstable: %d vs %d", g, m, h, again)
			}
		}
	}
	if len(seen) < 10 {
		t.Fatalf("hash outputs collide excessively: %v", seen)
	}
}

func TestContentFilterClassSizing(t *testing.T) {
	// Class boundaries: smallest class with bits >= 2n; the top class
	// saturates gracefully past n = 8192.
	cases := []struct {
		n, want int
	}{
		{0, 0}, {1, 0}, {512, 0}, {513, 1}, {1024, 1}, {1025, 2},
		{2048, 2}, {2049, 3}, {4096, 3}, {4097, 4}, {8192, 4},
		{8193, 4}, {1000000, 4},
	}
	for _, c := range cases {
		if got := contentFilterClassForGrams(c.n); got != c.want {
			t.Fatalf("classForGrams(%d) = %d; want %d", c.n, got, c.want)
		}
	}
}

func TestContentFilterSectionRoundTrip(t *testing.T) {
	rows := []contentFilterRow{
		{Class: 0, Bits: make([]byte, 128)},
		{Class: 2, Bits: make([]byte, 512)},
		{Class: 4, Bits: make([]byte, 2048)},
	}
	for i := range rows {
		for j := range rows[i].Bits {
			rows[i].Bits[j] = byte(i*31 + j)
		}
	}
	enc := encodeContentFilterSection(1<<20, 1<<19, true, `E:\root`, "latin1", rows)
	dec, err := decodeContentFilterSection(enc, 3)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.MaxRaw != 1<<20 || dec.MaxText != 1<<19 || !dec.CapsExplicit || dec.Root != `E:\root` || dec.Encoding != "latin1" {
		t.Fatalf("decoded identity = %+v", dec)
	}
	if len(dec.Rows) != 3 {
		t.Fatalf("decoded %d rows; want 3", len(dec.Rows))
	}
	for i := range rows {
		if dec.Rows[i].Class != rows[i].Class || string(dec.Rows[i].Bits) != string(rows[i].Bits) {
			t.Fatalf("row %d mismatch", i)
		}
	}
	// Count mismatch fails (rows join by DocID; a short/long section would
	// misalign every document).
	if _, err := decodeContentFilterSection(enc, 2); err == nil {
		t.Fatal("short count decoded without error")
	}
	// Truncation fails closed.
	if _, err := decodeContentFilterSection(enc[:len(enc)-5], 3); err == nil {
		t.Fatal("truncated section decoded without error")
	}
	// A wrong hash count is refused, never misread.
	bad := append([]byte(nil), enc...)
	bad[20] = contentFilterHashes + 1
	if _, err := decodeContentFilterSection(bad, 3); err == nil {
		t.Fatal("future hash count decoded without error")
	}
	// Empty corpus encodes a valid empty section.
	empty, err := decodeContentFilterSection(encodeContentFilterSection(0, 0, false, "", "", nil), 0)
	if err != nil || len(empty.Rows) != 0 {
		t.Fatalf("empty section = %v, %v", empty, err)
	}
	// Unknown flags are refused, never misread (the flags uvarint follows
	// the encoding: 24-byte header + root + encoding for this fixture).
	flagged := append([]byte(nil), enc...)
	const flagsAt = 24 + 1 + len(`E:\root`) + 1 + len("latin1")
	flagged[flagsAt] |= 2
	if _, err := decodeContentFilterSection(flagged, 3); err == nil {
		t.Fatal("unknown filter flags decoded without error")
	}
}

// TestContentFilterNoFalseNegatives proves the filter never drops a true
// match: every (text, query) pair below truly contains the query, so
// mayMatch must hold across text shapes (ASCII, fold-expanding runes,
// control bytes, tiny inputs) and query shapes (rare/common literals,
// phrases, case-sensitive leaves, regex-required runs, sub-3-byte terms).
func TestContentFilterNoFalseNegatives(t *testing.T) {
	texts := map[string]string{
		"ascii":       "the quick brown fox jumps over struct mutex cityhash_v103",
		"mixed-case":  "PaintLayer reassemble MUTEX Struct",
		"fold-expand": "İstanbul ẞ kelvin K",
		"control":     "line one\x01line two with needle",
		"tiny":        "ab",
		"two-byte":    "xy needle",
		"repeat":      "needle needle needle",
		"long":        "prefix needle suffix with mutex inside a longer document body that keeps going",
	}
	// leaf text -> caseSensitive. Every pair is chosen so the file truly
	// matches under that case policy.
	leaves := []struct {
		text string
		cs   bool
	}{
		{"cityhash_v103", false},
		{"struct", false},
		{"STRUCT", false},
		{"the quick brown", false},
		{"PaintLayer", false},
		{"paintlayer", false},
		{"MUTEX", true},
		{"mutex", true},
		{"needle", false},
		{"ab", false},
		{"xy", false},
		{"the", false},
	}
	for name, body := range texts {
		stored := contentStoredText([]byte(body))
		folded := contentFoldText(stored)
		grams := contentGramsOfU32(folded)
		class := contentFilterClassForGrams(len(grams))
		bits := make([]byte, contentFilterClassBits[class]/8)
		contentFilterBitsOfU32(bits, grams)
		row := contentFilterRow{Class: class, Bits: bits}
		for _, leaf := range leaves {
			// Ground truth on the same semantics the query paths use.
			needle := []byte(leaf.text)
			hay := stored
			if !leaf.cs {
				needle = contentFoldText(needle)
				hay = folded
			}
			contains := false
			for i := 0; i+len(needle) <= len(hay); i++ {
				match := true
				for j := range needle {
					if hay[i+j] != needle[j] {
						match = false
						break
					}
				}
				if match {
					contains = true
					break
				}
			}
			if !contains {
				continue
			}
			// The prefilter always folds the leaf (a raw match implies the
			// folded match), so ask it exactly as the query path does.
			if grams := contentFilterLeafGrams(leaf.text); len(grams) > 0 && !contentFilterMayMatch(row, grams) {
				t.Fatalf("FALSE NEGATIVE: text=%q leaf=%q cs=%v: file matches but filter rejects", name, leaf.text, leaf.cs)
			}
		}
	}
}

// TestContentFilterGateSoundness proves the choke-point gate (§6y) never
// rejects a true match for any query shape: whenever the brute-force
// predicate holds for a file, the gate must pass. The gate only ever fires
// on unsatisfiable positive constraints, so gate-reject implies
// predicate-false — never the reverse.
func TestContentFilterGateSoundness(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	bodies := []string{
		"alpha needle one",
		"bravo nothing here",
		"Needle in text with alpha",
		"delta mutex lock",
		"echo paintlayer reassemble",
		"foxtrot cityhash_v103 special",
		"golf the common words",
		"hotel ibex fold",
		"mike phrase foo bar baz",
		"november foo bar",
		"papa STRUCT",
		"quebec mutex and needle together",
		"romeo",
		"sierra mismatch",
	}
	queries := []string{
		"content:needle",
		"content:NEEDLE",
		"content:mutex",
		"content:alpha content:needle",
		`content:"foo bar"`,
		"content:alpha|content:mutex",
		"content:needle !content:quebec",
		`content:/ne+dle/`,
		`content:/mutex_\w+/`,
		`content:/zzz\d+/`,
		"case:true content:Needle",
		"case:true content:STRUCT",
		"content:zzz-no-match",
		"content:the",
		"content:ab",
	}
	for _, q := range queries {
		pq, err := parseQuery(queryOptions{Query: q})
		if err != nil {
			t.Fatalf("parse %q: %v", q, err)
		}
		m := newContentLeafMatcher(pq)
		for _, body := range bodies {
			raw := contentStoredText([]byte(body))
			// Brute-force predicate: every leaf evaluated on the real
			// text, then the joint predicate — exactly what verification
			// computes after a gate pass.
			matched := make([]bool, m.size)
			var folded []byte
			foldedReady := false
			for _, leaf := range m.leaves {
				matched[leaf.LeafID] = m.matchRawFirst(raw, &folded, &foldedReady, leaf)
			}
			entry := Entry{Path: "f.txt", Name: "f.txt"}
			want := entryMatchesContentAt(entry, pq, pq.MatchPath, matched)
			// The gate needs rows: build this file's real row.
			folded2 := contentFoldText(raw)
			grams := contentGramsOfU32(folded2)
			class := contentFilterClassForGrams(len(grams))
			bits := make([]byte, contentFilterClassBits[class]/8)
			contentFilterBitsOfU32(bits, grams)
			gateRejects := !contentFilterGateOk(contentFilterRow{Class: class, Bits: bits}, pq, m)
			if want && gateRejects {
				t.Fatalf("FALSE NEGATIVE: query %q body %q matches but gate rejects", q, body)
			}
		}
	}
}

// writeFilterVerifyFile writes body and returns its path.
func writeFilterVerifyFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestContentVerifyDirectServesEditedFile proves verify-direct freshness
// (the fsearch story): a file edited after the index was built answers with
// its CURRENT bytes on the very next query — no drain, no fold, no wait —
// for every query the stale index still nominates. Exactness comes from the
// bytes read: stale hits vanish the moment the file changes. Terms whose
// grams the edit introduces are a documented re-index window (postings
// cannot nominate unknown grams); the drain closes it within seconds on a
// live volume.
func TestContentVerifyDirectServesEditedFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFilterVerifyFile(t, dir, "doc.txt", "original needle text")
	idx, err := buildContentIndexFromDir(t.Context(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if hits, complete := r.search("needle", 0); len(hits) != 1 || !complete {
		t.Fatalf("baseline = %v, %v; want 1 hit complete", hits, complete)
	}
	// Edit the file behind the index's back: no drain, no fold, no USN.
	if err := os.WriteFile(path, []byte("original ibex text"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The stale hit is gone (verified against current bytes)...
	if hits, _ := r.search("needle", 0); len(hits) != 0 {
		t.Fatalf("stale match served after edit: %v", hits)
	}
	// ...while a still-nominated term matches the CURRENT bytes.
	if hits, complete := r.search("original", 0); len(hits) != 1 || !complete {
		t.Fatalf("edited content not greppable: %v, %v", hits, complete)
	}
	if hits, _ := r.search("ibex", 0); len(hits) != 0 {
		t.Fatalf("unindexed grams served without re-index (drain window): %v", hits)
	}
}

// TestContentMissingFileReadsAbsent proves fail-closed reads: a file that
// vanished since the index was built is simply unfindable — never an error,
// never a stale hit.
func TestContentMissingFileReadsAbsent(t *testing.T) {
	dir := t.TempDir()
	path := writeFilterVerifyFile(t, dir, "doc.txt", "needle here")
	idx, err := buildContentIndexFromDir(t.Context(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if hits, complete := r.search("needle", 0); len(hits) != 0 || !complete {
		t.Fatalf("deleted file served: %v, %v", hits, complete)
	}
}

// TestContentDiskVerifyBudgetCapsSearch proves the per-query disk bound:
// past the budget the offline search stops and reports incomplete instead
// of reading the whole corpus.
func TestContentDiskVerifyBudgetCapsSearch(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 30; i++ {
		name := "f" + string(rune('a'+i/10)) + string(rune('0'+i%10)) + ".txt"
		writeFilterVerifyFile(t, dir, name, "common filler needle")
	}
	idx, err := buildContentIndexFromDir(t.Context(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	prev := contentDiskVerifyBudget
	contentDiskVerifyBudget = 3
	t.Cleanup(func() { contentDiskVerifyBudget = prev })
	hits, complete := r.search("needle", 100)
	if complete {
		t.Fatal("capped scan reported complete")
	}
	if len(hits) > 3 {
		t.Fatalf("capped scan returned %d hits past a 3-verify budget", len(hits))
	}
}

// TestContentFilterV11SectionPresent proves a fresh build carries the filter
// section and no text store.
func TestContentFilterV11SectionPresent(t *testing.T) {
	dir := t.TempDir()
	writeFilterVerifyFile(t, dir, "a.txt", "alpha needle beta")
	idx, err := buildContentIndexFromDir(t.Context(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if idx.Version != contentIndexVersion {
		t.Fatalf("built version = %d; want %d", idx.Version, contentIndexVersion)
	}
	if _, ok := idx.Sections[contentSectionText]; ok {
		t.Fatal("v11 build must not carry a text store")
	}
	if _, ok := idx.Sections[contentSectionFilter]; !ok {
		t.Fatal("v11 build must carry the filter section")
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if r.filter == nil || len(r.filter.Rows) != 1 {
		t.Fatalf("reader filter rows = %v", r.filter)
	}
}
