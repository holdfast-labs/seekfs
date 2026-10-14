package main

// Tests for the name-search lessons (WORK_QUEUE "Name-search lessons",
// REFERENCE_TOOLS G11): accent folding, name operators ('exact, ^prefix,
// suffix$, **/brace globs, in:), quoted exactness, typo budget, and
// current-file demotion.

import (
	"strings"
	"testing"
)

func mustParseNameQuery(t *testing.T, query string) parsedQuery {
	t.Helper()
	pq, err := parseQuery(queryOptions{Query: query, Limit: 20})
	if err != nil {
		t.Fatalf("parseQuery(%q): %v", query, err)
	}
	return pq
}

func TestFoldAccentLowerBasics(t *testing.T) {
	cases := []struct{ in, want string }{
		{"resume", "resume"},
		{"Resume", "resume"},
		{"résumé", "resume"},
		{"RESUME", "resume"},
		{"Só Danço", "so danco"},
		{"naïve", "naive"},
		{"café", "cafe"},
		{"ＡＢＣ", "abc"},       // fullwidth folds to ASCII
		{"ﬁle", "ﬁle"},         // ligature has no decomposition to ASCII: unchanged
		{"hello-world_2.0", "hello-world_2.0"},
	}
	for _, c := range cases {
		if got := foldAccentLower(c.in); got != c.want {
			t.Errorf("foldAccentLower(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// NFD and NFC spellings of the same name fold identically.
	nfc := "résumé"
	nfd := "re\u0301sume\u0301"
	if foldAccentLower(nfc) != foldAccentLower(nfd) {
		t.Errorf("NFC/NFD fold mismatch: %q vs %q", foldAccentLower(nfc), foldAccentLower(nfd))
	}
	// ASCII input is exactly strings.ToLower.
	for _, s := range []string{"Main.GO", "aBc-123_x", ""} {
		if got := foldAccentLower(s); got != strings.ToLower(s) {
			t.Errorf("foldAccentLower(%q) = %q, want ToLower %q", s, got, strings.ToLower(s))
		}
	}
}

func TestNameOperatorParsing(t *testing.T) {
	pq := mustParseNameQuery(t, "'Makefile")
	if len(pq.Terms) != 1 || pq.Terms[0] != "makefile" {
		t.Fatalf("'exact Terms = %v", pq.Terms)
	}
	if len(pq.ExactTerms) != 1 || pq.ExactTerms[0] != "makefile" {
		t.Fatalf("'exact ExactTerms = %v", pq.ExactTerms)
	}

	pq = mustParseNameQuery(t, "^chrome")
	if len(pq.Prefixes) != 1 || pq.Prefixes[0] != "chrome" || len(pq.Terms) != 1 {
		t.Fatalf("^prefix = %+v", pq)
	}

	pq = mustParseNameQuery(t, "gate.cc$")
	if len(pq.Suffixes) != 1 || pq.Suffixes[0] != "gate.cc" {
		t.Fatalf("suffix$ = %+v", pq)
	}

	pq = mustParseNameQuery(t, "^main$")
	if len(pq.Prefixes) != 1 || len(pq.Suffixes) != 1 {
		t.Fatalf("^both$ = %+v", pq)
	}

	// Lone markers stay literal terms.
	for _, q := range []string{"^", "$", "'"} {
		pq = mustParseNameQuery(t, q)
		if len(pq.ExactTerms)+len(pq.Prefixes)+len(pq.Suffixes) != 0 {
			t.Fatalf("%q must stay literal: %+v", q, pq)
		}
	}

	// Quoted phrases: one term, marked quoted.
	pq = mustParseNameQuery(t, `"foo bar"`)
	if len(pq.Terms) != 1 || pq.Terms[0] != "foo bar" {
		t.Fatalf("quoted Terms = %v", pq.Terms)
	}
	if _, ok := pq.QuotedTerms["foo bar"]; !ok {
		t.Fatalf("quoted term must be marked: %+v", pq.QuotedTerms)
	}

	// in: merges with --under (nested collapses to the deepest).
	pq, err := parseQuery(queryOptions{Query: "in:E:\\bench\\chromium\\base main", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if pq.Under == "" {
		t.Fatalf("in: must set Under: %+v", pq)
	}
}

func TestInUnderMerging(t *testing.T) {
	base := `E:\bench\chromium`
	pq, err := parseQuery(queryOptions{Query: "main", Under: base + `\base`, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	_ = pq
	// Nested in: roots collapse to the deepest.
	pq2, err := parseQuery(queryOptions{Query: "in:" + base + " main", Under: base + `\base`, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if pq2.Under == "" || !strings.HasSuffix(pq2.Under, "base") {
		t.Fatalf("nested unders must collapse to deepest, got %q", pq2.Under)
	}
	// Disjoint roots are impossible (no scan).
	pq3, err := parseQuery(queryOptions{Query: `in:E:\aaa in:E:\bbb main`, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !pq3.Impossible {
		t.Fatalf("disjoint in: roots must be impossible: %+v", pq3)
	}
	// Negated in: becomes an exclusion, not Under.
	pq4 := mustParseNameQuery(t, `main !in:E:\bench\chromium\base`)
	if len(pq4.NotUnders) != 1 {
		t.Fatalf("!in: must set NotUnders: %+v", pq4)
	}
	if pq4.Under != "" {
		t.Fatalf("!in: must not set Under: %+v", pq4)
	}
	pq5 := mustParseNameQuery(t, `!in:E:\bench\chromium\base`)
	if len(pq5.NotUnders) != 1 {
		t.Fatalf("pure !in: must constrain: %+v", pq5)
	}
}

func TestBraceAndDoublestarParsing(t *testing.T) {
	pq := mustParseNameQuery(t, "glob:src/**/*.{c,h}")
	if len(pq.PathGlobs) != 1 {
		t.Fatalf("path glob must land in PathGlobs: %+v", pq)
	}
	foundSrc := false
	for _, d := range pq.Dirs {
		if d == "src" {
			foundSrc = true
		}
	}
	if !foundSrc {
		t.Fatalf("brace-common literal must join Dirs: %+v", pq.Dirs)
	}

	pq = mustParseNameQuery(t, "glob:*.{c,h}")
	if len(pq.Globs) != 0 || len(pq.OrGroups) != 1 || len(pq.OrGroups[0]) != 2 {
		t.Fatalf("ext-set brace glob must become an ext OR group: %+v", pq)
	}
	for i, want := range []string{"c", "h"} {
		alt := pq.OrGroups[0][i]
		if len(alt.Exts) != 1 || alt.Exts[0] != want {
			t.Fatalf("ext OR alt %d = %+v, want ext:%s", i, alt, want)
		}
	}
	// The ext OR matches exactly the brace set (and nothing else).
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"x.c", true}, {"x.h", true}, {"x.go", false}, {"x.ch", false},
	} {
		e := nameTestEntry(`E:\r\`+tc.name, tc.name)
		if got := entryMatches(e, pq, false); got != tc.want {
			t.Errorf("glob:*.{c,h} vs %s = %v, want %v", tc.name, got, tc.want)
		}
	}

	pq = mustParseNameQuery(t, "src/**/*.{c,h}")
	if len(pq.PathGlobs) != 1 {
		t.Fatalf("bare glob path must route to PathGlobs: %+v", pq)
	}

	// Brace sets are OR over extensions: no single ext may be required.
	if exts, ok := simpleGlobExts([]string{"*.{c,h}"}); ok {
		t.Fatalf("simpleGlobExts must decline braces, got %v", exts)
	}
	if exts := complexGlobExts([]string{"*.{c,h}"}); len(exts) != 0 {
		t.Fatalf("complexGlobExts must skip braces, got %v", exts)
	}
	lits := globCommonLiterals("src/**/*.{c,h}", false)
	has := func(want string) bool {
		for _, l := range lits {
			if l == want {
				return true
			}
		}
		return false
	}
	if !has("src") {
		t.Fatalf("common literals must include src: %v", lits)
	}
	if has("c") || has("h") {
		t.Fatalf("alternation branches must not leak into literals: %v", lits)
	}
}

func TestExpandBraces(t *testing.T) {
	got := expandBraceAlternatives("src/**/*.{c,h}")
	if len(got) != 2 || got[0] != "src/**/*.c" || got[1] != "src/**/*.h" {
		t.Fatalf("brace expand = %v", got)
	}
	got = expandBraceAlternatives("*.{c,h,cc}")
	if len(got) != 3 {
		t.Fatalf("three-way expand = %v", got)
	}
	got = expandBraceAlternatives("plain*.cc")
	if len(got) != 1 || got[0] != "plain*.cc" {
		t.Fatalf("braceless pattern must pass through: %v", got)
	}
	got = expandBraceAlternatives("a{b")
	if len(got) != 1 || got[0] != "a{b" {
		t.Fatalf("unbalanced brace must stay literal: %v", got)
	}
	got = expandBraceAlternatives("{GUID}")
	if len(got) != 1 || got[0] != "{GUID}" {
		t.Fatalf("single brace pair must stay literal: %v", got)
	}
	// ... and parses as a literal substring term, not a glob.
	pqLit := mustParseNameQuery(t, "{GUID}")
	if len(pqLit.Globs) != 0 || len(pqLit.Terms) != 1 || pqLit.Terms[0] != "{guid}" {
		t.Fatalf("{GUID} must stay a literal term: %+v", pqLit)
	}
}

func TestDoublestarMatch(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"src/**/*.cc", "src/a/b/c.cc", true},
		{"src/**/*.cc", "src/c.cc", true},
		{"src/**/*.cc", "src/c.h", false},
		{"src/**/*.cc", "other/c.cc", false},
		{"**/OWNERS", "a/b/OWNERS", true},
		{"**/OWNERS", "OWNERS", true},
		{"**/*.cc", "a/b/c.cc", true},
		{"src/*/main.cc", "src/a/main.cc", true},
		{"src/*/main.cc", "src/a/b/main.cc", false},
		{"**", "anything/at/all", true},
	}
	for _, c := range cases {
		if got := matchDoublestar(c.pat, c.path); got != c.want {
			t.Errorf("matchDoublestar(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
	// Path globs match the full path with brace sets.
	if !matchPathGlob("src/**/*.{c,h}", `E:\r\src\a\b\x.c`, false) {
		t.Error("brace path glob must match")
	}
	if matchPathGlob("src/**/*.{c,h}", `E:\r\src\a\b\x.go`, false) {
		t.Error("brace path glob must reject other extensions")
	}
	// Filename brace globs match any alternative.
	if !matchNameGlob("*.{c,h}", "x.c") || !matchNameGlob("*.{c,h}", "x.h") {
		t.Error("filename brace glob must match either branch")
	}
	if matchNameGlob("*.{c,h}", "x.go") {
		t.Error("filename brace glob must reject other extensions")
	}
}

func nameTestEntry(path, name string) Entry {
	return Entry{Path: path, Name: name, LowerPath: strings.ToLower(path), LowerName: strings.ToLower(name)}
}

func TestAnchorMatching(t *testing.T) {
	entry := nameTestEntry(`E:\r\src\chrome_main.cc`, "chrome_main.cc")
	match := func(q string, pathMode bool) bool {
		pq := mustParseNameQuery(t, q)
		return entryMatches(entry, pq, pathMode)
	}
	for _, q := range []string{"'chrome_main.cc", "^chrome", "main.cc$", "^chrome_main.cc$"} {
		if !match(q, false) || !match(q, true) {
			t.Errorf("%q must match in both modes", q)
		}
	}
	for _, q := range []string{"'chrome_main", "^main", "chrome$", "'chrome_main.cc$"} {
		if match(q, false) || match(q, true) {
			t.Errorf("%q must not match in either mode", q)
		}
	}
	// Anchors always name the file, even in -path mode.
	pq := mustParseNameQuery(t, "^src")
	if entryMatches(entry, pq, true) {
		t.Error("^src must not match: the anchor names the file, not the path")
	}
}

func TestAccentNameMatching(t *testing.T) {
	entry := nameTestEntry("E:\\docs\\r\u00e9sum\u00e9.pdf", "r\u00e9sum\u00e9.pdf")
	pq := mustParseNameQuery(t, "resume")
	if !entryMatches(entry, pq, false) {
		t.Error("resume must find résumé (filename mode)")
	}
	pq = mustParseNameQuery(t, "resume")
	if !entryMatches(entry, pq, true) {
		t.Error("resume must find résumé (path mode)")
	}
	// The reverse direction works too: an accented query finds ASCII names.
	ascii := nameTestEntry(`E:\docs\resume.pdf`, "resume.pdf")
	pq = mustParseNameQuery(t, "r\u00e9sum\u00e9")
	if !entryMatches(ascii, pq, false) {
		t.Error("résumé query must find resume")
	}
	// Quoted terms keep exact-match semantics: no folding.
	pq = mustParseNameQuery(t, `"resume"`)
	if entryMatches(entry, pq, false) {
		t.Error(`quoted "resume" must not match résumé`)
	}
	if !entryMatches(ascii, pq, false) {
		t.Error(`quoted "resume" must still match resume`)
	}
	// case: keeps exact-match semantics too.
	pq = mustParseNameQuery(t, "case: resume")
	if entryMatches(entry, pq, false) {
		t.Error("case: resume must not match résumé")
	}
	// Anchors fold like terms: ^resume matches résumé.pdf by prefix.
	pq = mustParseNameQuery(t, "^resume")
	if !entryMatches(entry, pq, false) {
		t.Error("^resume must prefix-match résumé.pdf")
	}
	pq = mustParseNameQuery(t, "'resume.pdf")
	if !entryMatches(entry, pq, false) {
		t.Error("folded 'resume.pdf must equal résumé.pdf (exact folds like terms)")
	}
	// ... while quoted and case-sensitive exactness stays byte-exact.
	pq = mustParseNameQuery(t, `"resume.pdf"`)
	if entryMatches(entry, pq, false) {
		t.Error(`quoted "resume.pdf" must not match résumé.pdf`)
	}
	pq = mustParseNameQuery(t, "'r\u00e9sum\u00e9.pdf")
	if !entryMatches(entry, pq, false) {
		t.Error("exact accented name must match")
	}
	// dir: folds as well.
	pq = mustParseNameQuery(t, "dir:documents")
	if !entryMatches(nameTestEntry("E:\\Documents\\x.txt", "x.txt"), pq, true) {
		t.Error("sanity: dir:documents must match Documents")
	}
	if !entryMatches(nameTestEntry("E:\\Docum\u00e9nts\\x.txt", "x.txt"), pq, true) {
		t.Error("folded dir:documents must match Documénts")
	}
}

func TestMultiWordSingleName(t *testing.T) {
	// Several words spelling one name: AND across snake_case ...
	entry := nameTestEntry(`E:\c\chrome_main_delegate.cc`, "chrome_main_delegate.cc")
	for _, q := range []string{"chrome main", "main delegate", "chrome delegate", "delegate chrome"} {
		pq := mustParseNameQuery(t, q)
		if !entryMatches(entry, pq, false) {
			t.Errorf("%q must match chrome_main_delegate.cc (order-free AND)", q)
		}
	}
	// ... and camelCase (case-insensitive substrings, any order).
	camel := nameTestEntry(`E:\c\mainDelegate.cc`, "mainDelegate")
	for _, q := range []string{"main delegate", "delegate main"} {
		pq := mustParseNameQuery(t, q)
		if !entryMatches(camel, pq, false) {
			t.Errorf("%q must match mainDelegate", q)
		}
	}
	// `src main` spanning path vs name needs -path (documented): in
	// filename mode src is not in the name.
	srcEntry := nameTestEntry(`E:\c\src\main.cc`, "main.cc")
	pq := mustParseNameQuery(t, "src main")
	if entryMatches(srcEntry, pq, false) {
		t.Error("filename mode must not span path vs name")
	}
	if !entryMatches(srcEntry, pq, true) {
		t.Error("-path mode must span path vs name")
	}
}

func TestPathGlobMatching(t *testing.T) {
	entry := nameTestEntry(`E:\r\src\a\b\x.c`, "x.c")
	pq := mustParseNameQuery(t, "glob:src/**/*.{c,h}")
	if !entryMatches(entry, pq, true) {
		t.Error("brace path glob must match (path mode)")
	}
	// Path globs constrain the path even in filename mode.
	if !entryMatches(entry, pq, false) {
		t.Error("brace path glob must match (filename mode)")
	}
	other := nameTestEntry(`E:\r\other\x.c`, "x.c")
	if entryMatches(other, pq, true) {
		t.Error("brace path glob must reject other dirs")
	}
	pq = mustParseNameQuery(t, "glob:**/OWNERS")
	owners := nameTestEntry(`E:\r\a\b\OWNERS`, "OWNERS")
	if !entryMatches(owners, pq, true) {
		t.Error("**/OWNERS must match at depth")
	}
}

func TestNotUnderMatching(t *testing.T) {
	inEntry := nameTestEntry(`E:\bench\chromium\base\x.cc`, "x.cc")
	outEntry := nameTestEntry(`E:\bench\chromium\net\x.cc`, "x.cc")
	pq := mustParseNameQuery(t, `x !in:E:\bench\chromium\base`)
	if entryMatches(inEntry, pq, true) {
		t.Error("excluded root must not match")
	}
	if !entryMatches(outEntry, pq, true) {
		t.Error("outside the excluded root must match")
	}
}

func TestTypoBudgetThresholds(t *testing.T) {
	// fsearch rule: one typo per word, two in words of 8+ letters.
	for _, tc := range []struct {
		term string
		want int
	}{
		{"abc", 1}, {"abcdefg", 1}, {"abcdefgh", 2}, {"presentation", 2},
	} {
		if got := fuzzyThresholdForTerm(tc.term); got != tc.want {
			t.Errorf("fuzzyThresholdForTerm(%q) = %d, want %d", tc.term, got, tc.want)
		}
	}
	// Transpositions cost one (Damerau) and keep working.
	if d := damerauLevenshteinBounded([]rune("browser"), []rune("browesr"), 2); d != 1 {
		t.Fatalf("adjacent transposition must cost 1, got %d", d)
	}
	if d := damerauLevenshteinBounded([]rune("chrome"), []rune("chorme"), 1); d != 1 {
		t.Fatalf("swap must be within tau=1, got %d", d)
	}
}

// fuzzyFixtureVolume builds a tiny records index with trigrams for the
// rewrite-guard tests: chrome.exe, browser_main.cc and ui_help.h.
func fuzzyFixtureVolume(t *testing.T) []*serviceVolumeIndex {
	t.Helper()
	idx := &Index{Compact: true, Source: "fuzzy-fixture", Volume: "T:"}
	add := func(parent int32, name string) int32 {
		idx.Records = append(idx.Records, CompactRecord{FRN: uint64(len(idx.Records) + 1), Parent: parent, Name: name})
		return int32(len(idx.Records) - 1)
	}
	root := add(-1, ".")
	src := add(root, "src")
	add(src, "chrome.exe")
	add(src, "browser_main.cc")
	add(src, "ui_help.h")
	vol := newServiceVolumeIndex("fuzzy-fixture.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	return []*serviceVolumeIndex{vol}
}

func TestFuzzySkipsAnchoredAndQuotedTerms(t *testing.T) {
	vols := fuzzyFixtureVolume(t)
	// Control: the unanchored typo is rewrite-eligible (proves the fixture
	// can produce trials at all).
	controlPQ := mustParseNameQuery(t, "chrome brwser ui")
	control := multiTermFuzzyRewriteTrials(vols, queryOptions{Query: "chrome brwser ui"}, controlPQ)
	if len(control) == 0 {
		t.Fatal("fixture must yield a rewrite trial for the unanchored typo")
	}
	// Anchored typo fields never equal their stripped terms, so the
	// multi-term backfill cannot rewrite them: they stay hard constraints.
	pq := mustParseNameQuery(t, "chrome ^brwser ui")
	trials := multiTermFuzzyRewriteTrials(vols, queryOptions{Query: "chrome ^brwser ui"}, pq)
	for _, tr := range trials {
		for _, f := range strings.Fields(tr.Opts.Query) {
			if f == "^browser" {
				t.Fatalf("anchored typo must stay a hard constraint: %q", tr.Opts.Query)
			}
		}
		if !strings.Contains(tr.Opts.Query, "^brwser") {
			t.Fatalf("anchored field must be preserved verbatim: %q", tr.Opts.Query)
		}
	}
	// Quoted typos are exact: the quoted field never matches its term.
	pq = mustParseNameQuery(t, `"brwser" foo`)
	trials = multiTermFuzzyRewriteTrials(vols, queryOptions{Query: `"brwser" foo`}, pq)
	for _, tr := range trials {
		if strings.Contains(tr.Opts.Query, "\"browser\"") {
			t.Fatalf("quoted typo must not be corrected: %q", tr.Opts.Query)
		}
	}
}

func TestCurrentFileDemotion(t *testing.T) {
	seedBlendStore(t)
	a := `T:\proj\a\main.go`
	b := `T:\proj\b\main.go`
	pq := mustParseBlendQuery(t, queryOptions{Query: "main.go", Limit: 10, Current: b})
	if pq.Current == "" {
		t.Fatal("--current must set the demotion path")
	}
	// Same band (same name): the current file sinks even from the front.
	page := []Entry{blendTestEntry(b, "main.go", 0), blendTestEntry(a, "main.go", 0)}
	got := applySignalReorder(page, pq)
	if got[0].Path != a || got[1].Path != b {
		t.Fatalf("current file must sink within its band: %v", got)
	}
	// Across bands the demotion never inverts: an exact-name current file
	// still beats a mere substring match.
	exactCurrent := `T:\proj\main.go`
	sub := `T:\proj\domain-main.go`
	pq2 := mustParseBlendQuery(t, queryOptions{Query: "main", Limit: 10, Current: exactCurrent})
	page2 := []Entry{blendTestEntry(exactCurrent, "main.go", 0), blendTestEntry(sub, "domain-main.go", 0)}
	got2 := applySignalReorder(page2, pq2)
	if got2[0].Path != exactCurrent {
		t.Fatalf("band rule must beat current demotion: %v", got2)
	}
	// Sets are unchanged: same members before and after.
	if len(got) != 2 || len(got2) != 2 {
		t.Fatalf("demotion must not change result sets: %v %v", got, got2)
	}
	// Counts never reorder (signal path is search-only).
	if signalReorderActive(mustParseBlendQuery(t, queryOptions{Query: "main", Limit: 10})) {
		t.Fatal("empty signals without --current must stay inactive")
	}
}

func TestQueryFoldUnionActiveGate(t *testing.T) {
	// ASCII queries on indexes without foldable names never activate.
	idx := &Index{Compact: true, Records: []CompactRecord{
		{FRN: 1, Parent: -1, Name: ".", Mode: uint32(0)},
		{FRN: 2, Parent: 0, Name: "resume.pdf"},
	}}
	pq := mustParseNameQuery(t, "resume")
	if accentQueryAddsMatches(idx, pq) {
		t.Fatal("ASCII index must not activate the fold union")
	}
	if got := accentRecordIDs(idx); len(got) != 0 {
		t.Fatalf("ASCII index must have no foldable IDs, got %v", got)
	}
	// An accented name activates and is served through the posting union.
	idx2 := &Index{Compact: true, Records: []CompactRecord{
		{FRN: 1, Parent: -1, Name: ".", Mode: uint32(0)},
		{FRN: 2, Parent: 0, Name: "r\u00e9sum\u00e9.pdf"},
		{FRN: 3, Parent: 0, Name: "notes.txt"},
	}}
	if got := accentRecordIDs(idx2); len(got) != 1 || got[0] != 1 {
		t.Fatalf("foldable IDs = %v, want [1]", got)
	}
	vol := newServiceVolumeIndex("fold-test.gsi", idx2)
	list := vol.nameTermPosting("resume")
	found := false
	for _, id := range list {
		if id == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("nameTermPosting(resume) must include the résumé record via the union: %v", list)
	}
	// A query unrelated to any accent fold keeps legacy lanes even on an
	// accent-bearing index (lane-selection stability).
	pqOther := mustParseNameQuery(t, "notes")
	if accentQueryAddsMatches(idx2, pqOther) {
		t.Fatal("unrelated query must not activate the union")
	}
	// Quoted terms never activate it either.
	pqQ := mustParseNameQuery(t, `"resume"`)
	if accentQueryAddsMatches(idx2, pqQ) {
		t.Fatal("quoted query must not activate the union")
	}
}

func TestCompactPathContainsFolded(t *testing.T) {	idx := &Index{
		Compact: true,
		Volume:  "E:",
		Records: []CompactRecord{
			{FRN: 1, Parent: -1, Name: ".", Mode: uint32(0)},
			{FRN: 2, Parent: 0, Name: "docs", Mode: uint32(0)},
			{FRN: 3, Parent: 1, Name: "r\u00e9sum\u00e9.pdf"},
		},
	}
	pq := mustParseNameQuery(t, "resume")
	if !idx.compactPathContainsAllFolded(2, []string{"resume"}, pq) {
		t.Error("folded path-contains must find resume in résumé.pdf")
	}
	pqQ := mustParseNameQuery(t, `"resume"`)
	if idx.compactPathContainsAllFolded(2, []string{"resume"}, pqQ) {
		t.Error("quoted path-contains must stay byte-exact")
	}
}

func TestRegexMaskPrefilterSound(t *testing.T) {	// The mask prefilter may only skip MatchString, never change the verdict:
	// every true match must pass its disjunct masks, and RegexMasks must
	// parallel Regexps.
	paths := []string{
		`E:\r\src\chrome_main.cc`, `E:\r\test\browser_test.cc`,
		`E:\r\README.md`, `E:\r\src\net\url_request.cc`,
	}
	for _, pat := range []string{`chrome.*browser`, `.*test.*\.cc`, `README\.(md|txt)`, `(foo|bar)_unittest`, `^E:\\r\\src\\.*\.h$`, `.*`, `a*`, `[a-z]+::[0-9]+`} {
		pq := mustParseNameQuery(t, "regex:"+pat)
		if len(pq.Regexps) != 1 || len(pq.RegexMasks) != 1 {
			t.Fatalf("regex %q must yield one regexp+mask slot, got %+v", pat, pq)
		}
		re := pq.Regexps[0]
		masks := pq.RegexMasks[0]
		for _, p := range paths {
			want := re.MatchString(p)
			possible := len(masks) == 0
			if !possible {
				pm := memoMaskOf(p)
				for _, dm := range masks {
					if dm&^pm == 0 {
						possible = true
						break
					}
				}
			}
			if want && !possible {
				t.Errorf("regex %q matches %q but the mask prefilter would skip it", pat, p)
			}
		}
	}
}

func TestContentFoldingStaysCaseOnly(t *testing.T) {
	// Verification (not a change): content matching folds case only —
	// changing it would alter the content index format, which is out of
	// scope. Accents survive contentFoldText, so `resume` does not match
	// `résumé` in content: exact search (the zero-result fuzzy backfill may
	// still bridge single-edit differences per word).
	in := "R\u00e9sum\u00e9 CAF\u00c9"
	if got := string(contentFoldText([]byte(in))); got != strings.ToLower(in) {
		t.Fatalf("contentFoldText must be case-only, got %q", got)
	}
	if foldAccentLower(in) == strings.ToLower(in) {
		t.Fatal("sanity: the name fold must differ from case-only here")
	}
}
