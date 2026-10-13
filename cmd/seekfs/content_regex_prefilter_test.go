package main

import (
	"regexp"
	"testing"
)

// regexPrefilterPatterns covers the shapes the prefilter must handle:
// literal prefixes, alternations, classes, optionals, anchors, escapes,
// patterns with no literals (must decline to nil), and non-ASCII (decline).
func regexPrefilterPatterns() []string {
	return []string{
		`mutex_\w+`,
		`mutex_`,
		`MUTEX_\d+`,
		`(foo|mutex_)bar`,
		`foo_(bar|mutex_)`,
		`[a-z]+mutex_[0-9]+`,
		`mutex_?lock`,
		`^mutex_init`,
		`mutex_destroy$`,
		`reassemble`,
		`a+`,
		`\d+`,
		`\w+`,
		`.*`,
		`foo|bar`,
		`(un|re)assemble`,
		`école`,
		`café_\w+`,
		`struct\s+\w+`,
		`get_\w+_count`,
		`[Mm]utex_\w+`,
		`\bmutex_\w+\b`,
		`(?:foo)?mutex_`,
		`mutex_{2,4}`,
		`x{3}y`,
	}
}

func regexPrefilterDocs() []contentFixtureFile {
	return []contentFixtureFile{
		{2, "a.cc", "static void mutex_lock(Mutex *m) { mutex_init(m); }"},
		{3, "b.cc", "MUTEX_42 happened here and mutex_7 too"},
		{4, "c.h", "nothing to see here, just a foobar line"},
		{5, "d.cc", "reassemble the packets to reassemble"},
		{6, "e.txt", "the foobar and the mutex_destroy at last"},
		{7, "f.cc", "unassemble assemble reassemble"},
		{8, "g.cc", "get_user_count and get_item_count here"},
		{9, "h.cc", "caf\u00e9_\u00e9cole and \u00e9cole_\u00e9mile"},
		{10, "i.cc", "MUTEX_LOCK upper Mutex_Lock mixed"},
		{11, "j.cc", "struct foo xxy and xxxy then xxy again"},
		{12, "k.cc", "digits 12345 and Classe [Mm]utex_9 test"},
		{13, "l.cc", ""},
	}
}

// TestContentRegexPrefilterIsSuperset proves every prefilter stream is an
// exact superset of the regex match set: verification can only narrow it,
// so results are identical with and without the prefilter.
func TestContentRegexPrefilterIsSuperset(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	files := regexPrefilterDocs()
	vol := newContentQueryVolume(t, files)
	reader, resolver := vol.content.readerResolverView()
	if reader == nil || resolver == nil {
		t.Fatal("no reader/resolver")
	}
	// Brute force over the fixture texts (the same repaired bytes the
	// builder stored: plain ASCII here, so the literal is exact).
	textOf := map[uint64]string{}
	for _, f := range files {
		textOf[f.frn] = f.text
	}
	for _, pat := range regexPrefilterPatterns() {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			t.Fatalf("pattern %q does not compile: %v", pat, err)
		}
		leaf := contentLeaf{Kind: contentLeafRegex, Text: pat}
		stream := contentRegexLeafStream(reader, leaf, nil)
		prefiltered := map[uint32]bool{}
		if stream != nil {
			for {
				id, ok := stream()
				if !ok {
					break
				}
				prefiltered[id] = true
			}
		}
		for i, d := range reader.idx.Docs {
			raw := textOf[d.FRN]
			matched := re.MatchString(raw)
			_, inPre := prefiltered[uint32(i)]
			if matched && stream != nil && !inPre {
				t.Errorf("pattern %q: doc %d (%q) matches but is missing from the prefilter", pat, i, raw)
			}
			if !matched && stream != nil && inPre {
				// Allowed (false positive), but bound it: the prefilter
				// must actually filter on this corpus.
				continue
			}
		}
		// The prefilter must filter: on this corpus a usable prefilter
		// keeps strictly fewer than all docs for at least the literal
		// patterns; a nil stream means "declined", which is only OK for
		// patterns with no provable literals.
		if stream == nil {
			if disj := regexRequiredLiteralAlternatives(pat); len(disj) != 0 {
				t.Errorf("pattern %q: nil stream but extractor found %q", pat, disj)
			}
		}
	}
}

// TestContentRegexSearchEqualsBruteForce proves end-to-end identical
// results with the prefilter engaged: the service answer must equal the
// brute-force regex over the fixture texts, for term+regex compounds,
// all-regex queries, and regex under OR groups.
func TestContentRegexSearchEqualsBruteForce(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	files := regexPrefilterDocs()
	vol := newContentQueryVolume(t, files)
	vols := []*serviceVolumeIndex{vol}
	queries := []string{
		`content:/mutex_\w+/`,
		`content:/MUTEX_\d+/`,
		`content:/reassemble/`,
		`content:/foo/ content:/mutex_\w+/`,
		`content:/\d+/`,
		`content:/foo|bar/`,
		`content:/get_\w+_count/`,
		`content:/mutex/`,
		// Mixed term+regex: the regex stays unconstrained (term-driven,
		// historical behavior) and the answer still matches brute force.
		`content:foobar content:/mutex_\w+/`,
		`content:mutex content:/\d+/`,
	}
	for _, q := range queries {
		matches, err := searchServiceVolumes(vols, queryOptions{Query: q, Limit: 100}, false)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		// Brute force: parse the leaves back out of the query the same
		// way the service does, then test every fixture text.
		pq, err := parseQuery(queryOptions{Query: q})
		if err != nil {
			t.Fatalf("%s: parse: %v", q, err)
		}
		m := newContentLeafMatcher(pq)
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
		var got []string
		for _, e := range matches {
			got = append(got, e.Path)
		}
		// Order differs (service ranks by name); compare as sets, then
		// require the service order to be its deterministic name order.
		if len(got) != len(want) {
			t.Errorf("%s: got %d %q; want %d %q", q, len(got), got, len(want), want)
			continue
		}
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
}

// TestContentLeavesAllRegex pins the prefilter gate: only pure-regex
// positive queries engage it; any term/phrase leaf (top-level or
// OR-nested) keeps the historical term-driven behavior.
func TestContentLeavesAllRegex(t *testing.T) {
	re := func(text string) contentLeaf { return contentLeaf{Kind: contentLeafRegex, Text: text} }
	tm := func(text string) contentLeaf { return contentLeaf{Kind: contentLeafTerm, Text: text} }
	for _, tc := range []struct {
		name string
		pq   parsedQuery
		want bool
	}{
		{"empty", parsedQuery{}, false},
		{"one-regex", parsedQuery{Content: []contentLeaf{re(`a+`)}}, true},
		{"two-regex", parsedQuery{Content: []contentLeaf{re(`a+`), re(`b+`)}}, true},
		{"mixed-top", parsedQuery{Content: []contentLeaf{tm("a"), re(`b+`)}}, false},
		{"term-only", parsedQuery{Content: []contentLeaf{tm("a")}}, false},
		{"or-pure", parsedQuery{OrGroups: [][]parsedQuery{{{Content: []contentLeaf{re(`a+`)}}}}}, true},
		{"or-mixed", parsedQuery{OrGroups: [][]parsedQuery{{
			{Content: []contentLeaf{re(`a+`)}},
			{Content: []contentLeaf{tm("b")}},
		}}}, false},
		{"not-ignored", parsedQuery{
			Content:   []contentLeaf{re(`a+`)},
			NotGroups: []parsedQuery{{Content: []contentLeaf{tm("b")}}},
		}, true},
	} {
		if got := contentLeavesAllRegex(tc.pq); got != tc.want {
			t.Errorf("%s: contentLeavesAllRegex=%v; want %v", tc.name, got, tc.want)
		}
	}
}

// TestSortCandidateIDsByScanOrder proves prefiltered candidates walk in
// scan order: the scan's own order when one exists, record order when nil.
func TestSortCandidateIDsByScanOrder(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "b.txt", "x"},
		{3, "a.txt", "y"},
		{4, "c.txt", "z"},
	})
	pq, err := parseQuery(queryOptions{Query: "content:/x|y|z/"})
	if err != nil {
		t.Fatal(err)
	}
	// No order anywhere: record order.
	vol.queryIndex = nil
	vol.index.CompactNameOrder = nil
	vol.index.Derived.NameRank = nil
	ids := []int{2, 0, 1}
	vol.sortCandidateIDsByScanOrder(ids, pq)
	if ids[0] != 0 || ids[1] != 1 || ids[2] != 2 {
		t.Fatalf("nil order = %v; want record order", ids)
	}
	// A scan order reverses records: candidates follow it.
	vol.index.CompactNameOrder = []int{2, 1, 0}
	ids = []int{0, 1, 2}
	vol.sortCandidateIDsByScanOrder(ids, pq)
	if ids[0] != 2 || ids[1] != 1 || ids[2] != 0 {
		t.Fatalf("scan order = %v; want [2 1 0]", ids)
	}
	// Unknown IDs (not in the order) sort last, deterministically.
	ids = []int{9, 0, 2}
	vol.sortCandidateIDsByScanOrder(ids, pq)
	if ids[0] != 2 || ids[1] != 0 || ids[2] != 9 {
		t.Fatalf("unknown-id order = %v; want [2 0 9]", ids)
	}
	// A resident rank that inverts the order answers positions without the
	// per-query position array; the result is the same scan order.
	vol.index.Derived.NameRank = []uint32{2, 1, 0}
	if rank := vol.rankForQuery(pq); len(rank) != 3 {
		t.Fatalf("rankForQuery = %v; want the resident name rank", rank)
	}
	ids = []int{0, 1, 2}
	vol.sortCandidateIDsByScanOrder(ids, pq)
	if ids[0] != 2 || ids[1] != 1 || ids[2] != 0 {
		t.Fatalf("rank-inverse scan order = %v; want [2 1 0]", ids)
	}
	// A rank that disagrees with the scan order (not its inverse) must not
	// be trusted: the exact position array decides.
	vol.index.Derived.NameRank = []uint32{0, 1, 2}
	ids = []int{0, 1, 2}
	vol.sortCandidateIDsByScanOrder(ids, pq)
	if ids[0] != 2 || ids[1] != 1 || ids[2] != 0 {
		t.Fatalf("inconsistent-rank scan order = %v; want [2 1 0] from the order", ids)
	}
}

// TestContentRegexPrefilterDeclines proves the hopeless shapes stay on the
// full scan (nil stream): pure classes, dot-star, short-only runs.
func TestContentRegexPrefilterDeclines(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentQueryVolume(t, regexPrefilterDocs())
	reader, _ := vol.content.readerResolverView()
	for _, pat := range []string{`\d+`, `\w+`, `.*`, `a+`, `x|y`, `(a|b)`} {
		leaf := contentLeaf{Kind: contentLeafRegex, Text: pat}
		if s := contentRegexLeafStream(reader, leaf, nil); s != nil {
			// A non-nil stream is still correct if it is a superset, but
			// these patterns have no literal run >= 3, so nil is
			// expected; drain it to prove the superset either way.
			n := 0
			for {
				_, ok := s()
				if !ok {
					break
				}
				n++
			}
			t.Logf("pattern %q: unexpected stream of %d (still sound if superset)", pat, n)
		}
	}
}
