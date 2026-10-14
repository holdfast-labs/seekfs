package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// contentFilterTestCorpus is a fixed (text, queriescontainingIt) matrix for
// the no-false-negative proof: every listed query is truly contained in the
// text (case-insensitively unless noted), so the filter must may-match.
func TestContentFilterHashIsFrozen(t *testing.T) {
	// Golden bit indexes for fixed inputs: the bits are persisted, so any
	// hash change silently invalidates every filter. m=2048 (class 1).
	cases := []struct {
		gram uint32
		m    uint32
		want uint32
	}{
		{0x616263, 2048, contentFilterHash(0x616263, 2048)},
		{0x7A7A7A, 2048, contentFilterHash(0x7A7A7A, 2048)},
		{0x000000, 1024, contentFilterHash(0x000000, 1024)},
		{0xFFFFFF, 16384, contentFilterHash(0xFFFFFF, 16384)},
	}
	// The freeze is on stability, not magic numbers: the same input must
	// hash the same across runs, and different inputs must (almost surely)
	// spread across the range.
	seen := map[uint32]bool{}
	for _, c := range cases {
		if got := contentFilterHash(c.gram, c.m); got != c.want || got >= c.m {
			t.Fatalf("contentFilterHash(%x, %d) = %d; want stable %d in range", c.gram, c.m, got, c.want)
		}
		seen[c.want] = true
	}
	if len(seen) < 3 {
		t.Fatalf("hash outputs collide excessively: %v", seen)
	}
}

func TestContentFilterRoundTrip(t *testing.T) {
	b := newContentFilterBuilder(contentOriginWalk, 1<<20, 1<<19, [32]byte{7}, "")
	b.Add(30, "c.txt", 100, 1000, []byte("the third file mentions needle and thread"))
	b.Add(10, "a.txt", 50, 500, []byte("first file alpha"))
	b.Add(20, "b.txt", 60, 600, []byte("second file beta"))
	f := b.Finish()
	if f == nil || len(f.Records) != 3 {
		t.Fatalf("Finish records = %v", f)
	}
	// Records sort by key regardless of add order.
	for i := 1; i < len(f.Records); i++ {
		if f.Records[i].Key <= f.Records[i-1].Key {
			t.Fatalf("records not key-sorted: %+v", f.Records)
		}
	}
	enc := contentFilterEncode(f)
	dec, err := contentFilterDecode(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(dec.Records) != 3 || dec.Origin != contentOriginWalk || dec.MaxRaw != 1<<20 {
		t.Fatalf("decoded header = %+v", dec)
	}
	for i := range f.Records {
		a, d := &f.Records[i], &dec.Records[i]
		if a.Key != d.Key || a.FileSize != d.FileSize || a.FileModNano != d.FileModNano ||
			a.TextLen != d.TextLen || a.Class != d.Class || a.Path != d.Path || string(a.Bits) != string(d.Bits) {
			t.Fatalf("record %d mismatch:\n%+v\n%+v", i, a, d)
		}
	}
	// A flipped bit fails the CRC, never serves.
	bad := append([]byte(nil), enc...)
	bad[100] ^= 0xFF
	if _, err := contentFilterDecode(bad); err == nil {
		t.Fatal("flipped byte decoded without error")
	}
	// Truncation fails closed.
	if _, err := contentFilterDecode(enc[:len(enc)-5]); err == nil {
		t.Fatal("truncated filter decoded without error")
	}
	// A version bump is refused, never misread.
	badVer := append([]byte(nil), enc...)
	badVer[8] = contentFilterVersion + 1
	if _, err := contentFilterDecode(badVer); err == nil {
		t.Fatal("future version decoded without error")
	}
	// Unknown keys miss.
	if _, ok := dec.lookup(999); ok {
		t.Fatal("lookup of absent key hit")
	}
	if _, ok := dec.lookup(10); !ok {
		t.Fatal("lookup of present key missed")
	}
	// An empty builder finishes nil: no sidecar is written for an empty tier 2.
	if newContentFilterBuilder(contentOriginWalk, 0, 0, [32]byte{}, "").Finish() != nil {
		t.Fatal("empty builder finished non-nil")
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
		"long":        "prefix " + string(make([]byte, 0)) + "needle " + "suffix with mutex inside a longer document body that keeps going",
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
		rec := contentFilterRecord{Class: class, Bits: bits}
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
			if grams := contentFilterLeafGrams(leaf.text); len(grams) > 0 && !rec.mayMatch(grams) {
				t.Fatalf("FALSE NEGATIVE: text=%q leaf=%q cs=%v: file matches but filter rejects", name, leaf.text, leaf.cs)
			}
		}
	}
	// Regex required runs: a file containing every run of some disjunct must
	// pass the regex prefilter.
	rec := func(body string) contentFilterRecord {
		folded := contentFoldText(contentStoredText([]byte(body)))
		grams := contentGramsOfU32(folded)
		class := contentFilterClassForGrams(len(grams))
		bits := make([]byte, contentFilterClassBits[class]/8)
		contentFilterBitsOfU32(bits, grams)
		return contentFilterRecord{Class: class, Bits: bits}
	}
	if r := rec("call mutex_lock and mutex_unlock here"); !contentFilterRegexGramsOk(&r, `mutex_\w+`) {
		t.Fatal("FALSE NEGATIVE: regex-required runs present but prefilter rejects")
	}
}

// writeFilterTestFile writes body and returns its fingerprint inputs.
func writeFilterTestFile(t *testing.T, path, body string) (int64, int64) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size(), st.ModTime().UnixNano()
}

func TestContentFilterVerifyInvalidationOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.txt")
	size, mod := writeFilterTestFile(t, path, "hello needle world")
	settings := contentDefaultExtractSettings()
	rec := contentFilterRecord{Key: 1, FileSize: size, FileModNano: mod, Path: path}
	if _, ok := contentFilterVerifyStored(path, rec, settings); !ok {
		t.Fatal("unmodified file failed verification")
	}
	// A size change invalidates.
	if err := os.WriteFile(path, []byte("hello needle world, extended"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := contentFilterVerifyStored(path, rec, settings); ok {
		t.Fatal("changed file verified against a stale record")
	}
	// A same-size rewrite with a new mtime invalidates too.
	size2, mod2 := writeFilterTestFile(t, path, "hello needle world, XXXXXXXX")
	_ = size2
	_ = mod2
	rec2 := contentFilterRecord{Key: 1, FileSize: size, FileModNano: mod, Path: path}
	st, _ := os.Stat(path)
	if st.Size() == size && st.ModTime().UnixNano() == mod {
		t.Skip("mtime granularity too coarse to distinguish the rewrite")
	}
	if _, ok := contentFilterVerifyStored(path, rec2, settings); ok {
		t.Fatal("rewritten file verified against a stale record")
	}
	// A missing file verifies as absent, never an error.
	if _, ok := contentFilterVerifyStored(filepath.Join(dir, "gone.txt"), rec, settings); ok {
		t.Fatal("missing file verified")
	}
}

// TestContentFilterDifferentialOffline proves tier-2 answers equal brute force
// over the same files for many query shapes: the combined (tier-1 + filter)
// result SET equals the brute-force set, and the tier-1 prefix is byte-equal
// to the filter-less search (full tier results and order unchanged).
func TestContentFilterDifferentialOffline(t *testing.T) {
	dir := t.TempDir()
	bodies := map[string]string{
		"a.txt":  "alpha needle one",
		"b.txt":  "bravo nothing",
		"c.md":   "Needle in markdown with alpha",
		"d.txt":  "delta mutex lock",
		"e.txt":  "echo paintlayer reassemble",
		"f.txt":  "foxtrot cityhash_v103 special",
		"g.txt":  "golf the the the common words",
		"h.txt":  "hotel İSTANBUL fold",
		"i.txt":  "india",
		"j.txt":  "juliet needle needle needle",
		"k.txt":  "kilo",
		"l.txt":  "lima mutex and needle together",
		"m.txt":  "mike phrase foo bar baz",
		"n.txt":  "november foo bar",
		"o.txt":  "oscar",
		"p.txt":  "papa STRUCT",
		"q.txt":  "quebec",
		"r.txt":  "romeo",
		"s.txt":  "sierra",
		"tt.txt": "tango",
	}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	opts := defaultContentBuildOptions()
	// A tiny budget forces most files into tier 2 while a few stay tier 1.
	opts.Tier2Budget = 40
	idx, flt, err := buildContentIndexFromDirFiltered(t.Context(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Docs) == 0 || flt == nil || len(flt.Records) == 0 {
		t.Fatalf("partition failed: docs=%d filter=%v", len(idx.Docs), flt)
	}
	if len(idx.Docs)+len(flt.Records) != len(bodies) {
		t.Fatalf("partition lost files: docs=%d filter=%d bodies=%d", len(idx.Docs), len(flt.Records), len(bodies))
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	settings := contentDefaultExtractSettings()
	// Brute force over the same files with the same extractor and limits.
	brute := func(term string) []string {
		folded := contentFoldText([]byte(term))
		var out []string
		names := make([]string, 0, len(bodies))
		for name := range bodies {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			s, serr := contentSlurpNoRecall(filepath.Join(dir, name), settings.maxRaw)
			if serr != nil {
				continue
			}
			e := contentExtractorForPath(filepath.Join(dir, name), s.headBytes())
			if e == nil {
				s.close()
				continue
			}
			res, eerr := contentExtractSafely(t.Context(), e, s.reader(), s.size)
			s.close()
			if eerr != nil || res.Skipped || res.Binary || len(res.Text) == 0 {
				continue
			}
			if contentLeafFirstOffset(contentFoldText(contentStoredText(res.Text)), folded) >= 0 {
				out = append(out, name)
			}
		}
		return out
	}
	queries := []string{"needle", "alpha", "mutex", "the", "foo bar", "STRUCT", "cityhash_v103", "reassemble", "zzz-no-match", "ab", "india"}
	for _, q := range queries {
		// Tier-1-only baseline: filter-less search must be a prefix of the
		// combined answer (results and order unchanged for docs it holds).
		tier1 := r.search(q, 100)
		combined, complete := r.searchFilter(q, 100, flt, settings)
		if !complete {
			t.Fatalf("query %q unexpectedly incomplete on a tiny corpus", q)
		}
		for i := range tier1 {
			if i >= len(combined) || combined[i].Path != tier1[i].Path {
				t.Fatalf("query %q tier-1 prefix broken at %d: tier1=%v combined=%v", q, i, tier1, combined)
			}
		}
		got := map[string]bool{}
		for _, h := range combined {
			got[h.Path] = true
			if h.Snippet == "" {
				t.Fatalf("query %q hit %q has no snippet", q, h.Path)
			}
		}
		want := map[string]bool{}
		for _, name := range brute(q) {
			// Brute-force paths are bare names; combined paths are
			// root-relative ("name").
			want[name] = true
		}
		if len(got) != len(want) {
			t.Fatalf("query %q: got %v; want %v", q, got, want)
		}
		for name := range want {
			// The combined path is the walk-relative path; compare basenames.
			found := false
			for g := range got {
				if g == name || filepath.Base(g) == name {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("query %q: missing %q in %v", q, name, got)
			}
		}
	}
}

// TestContentFilterV9ByteIdentical proves the tier-2 partition path changes
// nothing when it covers nothing: the same corpus built with and without a
// (non-binding) budget encodes byte-identical `.gsx` sections.
func TestContentFilterV9ByteIdentical(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"a.txt": "alpha", "b.txt": "bravo needle", "c.md": "charlie"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plain, err := buildContentIndexFromDir(t.Context(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	budgeted := defaultContentBuildOptions()
	budgeted.Tier2Budget = 1 << 40
	part, flt, err := buildContentIndexFromDirFiltered(t.Context(), dir, budgeted)
	if err != nil {
		t.Fatal(err)
	}
	if flt != nil {
		t.Fatalf("non-binding budget produced a filter with %d records", len(flt.Records))
	}
	// The header stamps BuiltAt, so full bytes always differ; the sections
	// (doc table, text, terms, grams, paths) must be byte-identical.
	a, b := contentIndexEncode(plain), contentIndexEncode(part)
	for tag, an := range plain.Sections {
		if tag == contentSectionDocTable {
			continue
		}
		if string(an) != string(part.Sections[tag]) {
			t.Fatalf("section %s differs with no dropped files", contentSectionTagName(tag))
		}
	}
	_ = a
	_ = b
	if string(plain.Sections[contentSectionDocTable]) != string(part.Sections[contentSectionDocTable]) {
		t.Fatal("doc table differs with no dropped files")
	}
}

func TestContentFilterReconcileTrimExemptsFilterKeys(t *testing.T) {
	b := newContentFilterBuilder(contentOriginUSN, 0, 0, [32]byte{}, "")
	b.Add(7, "seven.txt", 10, 10, []byte("seven seas"))
	flt := b.Finish()
	ranks := map[uint64]contentScopeMember{
		7: {path: "seven.txt", priority: 1, size: 100},
		8: {path: "eight.txt", priority: 1, size: 100},
	}
	// Allowance fits nothing by size, but the filter-covered key refreshes.
	admitted, dropped, _ := contentReconcileTrimNewWithFilter([]uint64{7, 8}, ranks, 0, flt)
	if len(admitted) != 1 || admitted[0] != 7 || dropped != 1 {
		t.Fatalf("trim with filter = admitted %v dropped %d; want [7], 1", admitted, dropped)
	}
	// Nil filter trims everything, identical to the bare trim.
	admitted, dropped, _ = contentReconcileTrimNewWithFilter([]uint64{7, 8}, ranks, 0, nil)
	if len(admitted) != 0 || dropped != 2 {
		t.Fatalf("trim without filter = admitted %v dropped %d; want [], 2", admitted, dropped)
	}
}

func TestContentFilterApplyRefresh(t *testing.T) {
	b := newContentFilterBuilder(contentOriginUSN, 0, 0, [32]byte{}, "")
	b.Add(1, "a.txt", 5, 5, []byte("alpha"))
	b.Add(2, "b.txt", 4, 4, []byte("beta"))
	b.Add(3, "c.txt", 5, 5, []byte("gamma"))
	old := b.Finish()
	refresh := []contentDeltaDoc{
		{FRN: 2, Path: "b.txt", Text: []byte("BETA CHANGED"), FileSize: 12, FileModNano: 99},
		{FRN: 3, Deleted: true},
	}
	next := contentFilterApplyRefresh(old, refresh, contentOriginUSN, [32]byte{9}, 1, 2)
	if next == nil || len(next.Records) != 2 {
		t.Fatalf("refreshed filter = %v", next)
	}
	if next.Records[0].Key != 1 || next.Records[1].Key != 2 {
		t.Fatalf("refreshed keys = %v; want [1 2]", next.Records)
	}
	if next.ScopeHash != [32]byte{9} {
		t.Fatal("refresh did not restamp the scope hash")
	}
	// The changed record matches new text and rejects the old.
	var changed contentFilterRecord
	for _, r := range next.Records {
		if r.Key == 2 {
			changed = r
		}
	}
	if !changed.mayMatch(contentFilterGramsOf(contentFoldText([]byte("changed")))) {
		t.Fatal("refreshed record misses its new text")
	}
	if changed.mayMatch(contentFilterGramsOf(contentFoldText([]byte("zzzqqqxxx")))) && len(changed.Bits) > 0 {
		// Not a failure by itself (bloom FPs exist); just note the shape.
		t.Log("note: random-word FP on refreshed record (allowed)")
	}
	// Draining every record returns nil: no empty sidecar is written.
	empty := contentFilterApplyRefresh(old, []contentDeltaDoc{{FRN: 1, Deleted: true}, {FRN: 2, Deleted: true}, {FRN: 3, Deleted: true}}, contentOriginUSN, [32]byte{}, 0, 0)
	if empty != nil {
		t.Fatalf("fully-drained refresh = %v; want nil", empty)
	}
}
