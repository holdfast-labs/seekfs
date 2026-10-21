package main

import (
	"regexp"
	"strings"
	"testing"
)

// TestContentRegexVerifyCountsBounded proves the v11 regex prune
// deterministically: for regexes with required literals, the number of disk
// verifies equals the number of files whose indexed trigrams pass the
// tier-1 postings and the tier-2 bloom gate — i.e. files containing all of
// some disjunct's runs' trigrams (bloom false positives included) — and a
// regex whose literals are absent from the corpus does 0 verifies and
// reports complete.
func TestContentRegexVerifyCountsBounded(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	files := []contentFixtureFile{
		{2, "a.cc", "static void mutex_lock(Mutex *m) { mutex_init(m); }"},
		{3, "b.cc", "cityhash_v103 mixing and cityhash_crc256 here"},
		{4, "c.h", "nothing to see here, just a foobar line"},
		{5, "d.cc", "reassemble the packets\nsecond line here"},
		{6, "e.txt", "the foobar and the mutex_destroy at last"},
		{7, "f.cc", "plain filler text with no markers at all"},
		{8, "g.cc", "get_user_count and get_item_count here"},
		{9, "h.cc", "MUTEX_LOCK upper Mutex_Lock mixed"},
	}
	vol := newContentQueryVolume(t, files)
	vols := []*serviceVolumeIndex{vol}
	queries := []string{
		`content:/mutex_\w+/`,
		`content:/cityhash_\w+/`,
		`content:/zzz\d+qqq/`,
		`content:/get_\w+_count/`,
		// Mixed term+regex: the regex's literals AND with the term's.
		`content:foobar content:/mutex_\w+/`,
	}
	for _, q := range queries {
		pq, err := parseQuery(queryOptions{Query: q})
		if err != nil {
			t.Fatalf("%s: parse: %v", q, err)
		}
		m := newContentLeafMatcher(pq)
		// Brute-force expected hits over the fixture texts.
		var want []string
		for _, f := range files {
			if f.text == "" {
				continue
			}
			raw := []byte(f.text)
			folded := contentFoldText(raw)
			ok := true
			for _, leaf := range contentPositiveLeaves(pq) {
				if !m.match(raw, folded, leaf) {
					ok = false
					break
				}
			}
			if ok {
				want = append(want, "C:\\"+f.name)
			}
		}
		// Expected verifies: files passing tier-1 (postings) and tier-2
		// (bloom gate). The gate sees the real rows, so false positives
		// are bounded by construction; compute the pass set directly.
		reader, _ := vol.content.readerResolverView()
		if reader == nil {
			t.Fatal("no reader")
		}
		wantVerifies := 0
		for i, f := range files {
			if f.text == "" {
				continue
			}
			row, ok := reader.filterRow(uint32(i))
			if !ok {
				t.Fatalf("%s: no filter row for doc %d", q, i)
			}
			if contentFilterGateOk(row, pq, m) {
				wantVerifies++
			}
		}
		trace := &searchTrace{}
		matches, err := searchServiceVolumes(vols, queryOptions{Query: q, Limit: 100, Trace: trace}, false)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var got []string
		for _, e := range matches {
			got = append(got, e.Path)
		}
		if len(got) != len(want) {
			t.Errorf("%s: got %d %q; want %d %q", q, len(got), got, len(want), want)
		} else {
			set := map[string]bool{}
			for _, p := range got {
				set[p] = true
			}
			for _, p := range want {
				if !set[p] {
					t.Errorf("%s: missing %q (got %q)", q, p, got)
				}
			}
		}
		wantTotalReads := wantVerifies + len(matches) // optional snippets share the exact query cap
		if trace.DiskVerified != wantTotalReads {
			t.Errorf("%s: DiskVerified=%d; want %d (%d gate-pass files + %d snippet reads)", q, trace.DiskVerified, wantTotalReads, wantVerifies, len(matches))
		}
		if trace.DiskCapped {
			t.Errorf("%s: DiskCapped=true; want complete", q)
		}
		if trace.Complete == nil || !*trace.Complete {
			t.Errorf("%s: trace not complete", q)
		}
	}
}

// TestContentRegexZeroLiteralZeroVerifies pins the sharpest case: a
// zero-result regex whose required literals are absent prunes to 0
// candidates and does 0 disk verifies on a corpus without those literals.
func TestContentRegexZeroLiteralZeroVerifies(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	files := []contentFixtureFile{
		{2, "a.cc", "static void mutex_lock(Mutex *m) { mutex_init(m); }"},
		{3, "b.cc", "cityhash_v103 mixing here"},
		{4, "c.h", "nothing to see here, just a foobar line"},
	}
	vol := newContentQueryVolume(t, files)
	trace := &searchTrace{}
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: `content:/zzz\d+qqq/`, Limit: 100, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("got %d matches; want 0", len(matches))
	}
	if trace.DiskVerified != 0 {
		t.Fatalf("DiskVerified=%d; want 0", trace.DiskVerified)
	}
	if trace.DiskCapped {
		t.Fatalf("DiskCapped=true; want complete")
	}
	if trace.Complete == nil || !*trace.Complete {
		t.Fatalf("trace not complete")
	}
}

// TestContentRegexMatchEqualsStdlib proves the verify-time matcher (literal
// prefilter + folded haystack + per-line anchors) agrees with a brute-force
// stdlib regexp over the fixture texts for anchored and unanchored shapes,
// and against the raw-text engine for the unanchored ones.
func TestContentRegexMatchEqualsStdlib(t *testing.T) {
	patterns := []string{
		`mutex_\w+`, `MUTEX_\d+`, `reassemble`, `^mutex_init`,
		`mutex_destroy$`, `get_\w+_count`, `zzz\d+qqq`, `\d+`,
		`foo|bar`, `^static`, `m\)$`, `cityhash_\w+`,
	}
	bodies := []string{
		"static void mutex_lock(Mutex *m) { mutex_init(m); }",
		"cityhash_v103 mixing and cityhash_crc256 here",
		"nothing to see here, just a foobar line",
		"reassemble the packets\nsecond line here",
		"the foobar and the mutex_destroy at last",
		"line one\n^static mid\nend m)",
		"MUTEX_LOCK upper Mutex_Lock mixed",
	}
	for _, pat := range patterns {
		for _, cs := range []bool{false, true} {
			pq := parsedQuery{CaseSensitive: cs}
			pq.Content = []contentLeaf{{Kind: contentLeafRegex, Text: pat, LeafID: 0}}
			m := newContentLeafMatcher(pq)
			leaf := pq.Content[0]
			spat := pat
			if !cs {
				spat = "(?i)" + spat
			}
			re := regexp.MustCompile(spat)
			for _, body := range bodies {
				raw := contentStoredText([]byte(body))
				folded := contentFoldText(raw)
				got := m.match(raw, folded, leaf)
				var lazy []byte
				var ready bool
				if got2 := m.matchRawFirst(raw, &lazy, &ready, leaf); got2 != got {
					t.Errorf("pat %q cs=%v body %q: match=%v matchRawFirst=%v", pat, cs, body, got, got2)
				}
				// Brute force over the same haystack the matcher uses.
				hay := raw
				if !cs {
					hay = folded
				}
				var want bool
				if contentRegexLineAnchored(pat) {
					for _, line := range strings.Split(string(hay), "\n") {
						line = strings.TrimSuffix(line, "\r")
						if re.MatchString(line) {
							want = true
							break
						}
					}
				} else {
					want = re.Match(hay)
				}
				if got != want {
					t.Errorf("pat %q cs=%v body %q: match=%v; brute force=%v", pat, cs, body, got, want)
				}
			}
		}
	}
}
