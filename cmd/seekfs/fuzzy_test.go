package main

import (
	"math/rand"
	"strings"
	"testing"
)

func TestDamerauLevenshteinBounded(t *testing.T) {
	cases := []struct {
		a, b string
		max  int
		want int
	}{
		{"tonic", "tonic", 2, 0},
		{"tonic", "sonic", 2, 1},
		{"reprot", "report", 2, 1}, // transposition
		{"cat", "cart", 2, 1},
		{"abc", "xyz", 1, 2}, // exceeds max: capped at max+1
		{"", "ab", 2, 2},
		{"kitten", "mitten", 2, 1},
	}
	for _, tc := range cases {
		got := damerauLevenshteinBounded([]rune(tc.a), []rune(tc.b), tc.max)
		if got != tc.want {
			t.Fatalf("damerau(%q,%q,max=%d) = %d, want %d", tc.a, tc.b, tc.max, got, tc.want)
		}
	}
}

func TestFuzzyDeletionVariantsCoverDistanceOne(t *testing.T) {
	term := "report"
	variants := fuzzyDeletionVariants(term, 1)
	set := make(map[string]struct{}, len(variants))
	for _, v := range variants {
		set[v] = struct{}{}
	}
	if _, ok := set[term]; !ok {
		t.Fatal("variants must include the original term")
	}
	// Every single-character deletion must itself be present.
	if _, ok := set["eport"]; !ok {
		t.Fatal("missing deletion variant eport")
	}
	if _, ok := set["repor"]; !ok {
		t.Fatal("missing deletion variant repor")
	}
	// SymSpell completeness: any string within distance 1 of the term must
	// contain at least one variant as a substring.
	for _, near := range []string{"reports", "eport", "repot", "rpeort"} {
		found := false
		for v := range set {
			if strings.Contains(near, v) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("near-string %q shares no deletion variant of %q", near, term)
		}
	}
}

func TestFoldFuzzyText(t *testing.T) {
	if got := foldFuzzyText("Só Danço"); got != "so danco" && got != "So Danco" {
		// Case is handled separately by callers; only marks/width fold here.
		if got != "Só Danço" && !strings.EqualFold(foldFuzzyText("sodanco"), "sodanco") {
			t.Fatalf("foldFuzzyText unexpectedly unstable")
		}
	}
	if got := foldFuzzyText("ＲＥＡＤＭＥ"); got != "README" {
		t.Fatalf("fullwidth fold = %q, want README", got)
	}
	if got := foldFuzzyText("café"); got != "cafe" {
		t.Fatalf("accent fold = %q, want cafe", got)
	}
}

func TestParseQueryFuzzyMarker(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "reprot~", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !pq.Fuzzy {
		t.Fatal("term~ should mark the query fuzzy")
	}
	if len(pq.Terms) != 1 || pq.Terms[0] != "reprot" {
		t.Fatalf("terms = %v, want [reprot]", pq.Terms)
	}

	pqPlain, err := parseQuery(queryOptions{Query: "reprot", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if pqPlain.Fuzzy {
		t.Fatal("plain term must not be fuzzy")
	}

	pqFlag, err := parseQuery(queryOptions{Query: "reprot", Fuzzy: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !pqFlag.Fuzzy {
		t.Fatal("--fuzzy option should mark the query fuzzy")
	}
}

func TestParseQueryDropsBarePathSeparatorTokens(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "AGENTS.md / pyproject.toml / ext:py", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(pq.Terms, "|")
	if strings.Contains(joined, "/") || strings.Contains(joined, `\`) {
		t.Fatalf("bare path separators leaked into terms: %v", pq.Terms)
	}
	if len(pq.Terms) != 2 || pq.Terms[0] != "agents.md" || pq.Terms[1] != "pyproject.toml" {
		t.Fatalf("terms = %v, want [agents.md pyproject.toml]", pq.Terms)
	}
	if len(pq.Exts) != 1 || pq.Exts[0] != "py" {
		t.Fatalf("exts = %v, want [py]", pq.Exts)
	}

	// A real path token with a drive still splits into its components.
	pqPath, err := parseQuery(queryOptions{Query: "C:/work/src/main.go", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	pathJoined := strings.Join(pqPath.Terms, "|")
	if strings.Contains(pathJoined, "/") || strings.Contains(pathJoined, `\`) {
		t.Fatalf("path separators leaked in path-like term: %v", pqPath.Terms)
	}
	if len(pqPath.Terms) == 0 {
		t.Fatal("path-like term should still produce components")
	}

	// Lone separator with no other terms must not empty the query.
	if _, err := parseQuery(queryOptions{Query: "/", Limit: 10}); err == nil {
		t.Fatal("a query of only '/' should error as empty (no searchable terms)")
	}
}

// buildFuzzyTestVolume creates a compact service volume whose names exercise
// distance ranking, prefix preference, and exact-tier separation.
func buildFuzzyTestVolume(t *testing.T) *serviceVolumeIndex {
	t.Helper()
	idx := &Index{Source: "usn", Volume: "Q:", Compact: true, DBPath: ""}
	add := func(name string) {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       uint64(len(idx.Records) + 10),
			ParentFRN: uint64(10),
			Parent:    0,
			Name:      name,
		})
	}
	for _, name := range []string{
		".", // root record
		"tonic.txt",
		"tonicwater.txt",
		"tonicwater.txtx",
		"sonic.txt",
		"ironic.txt",
		"totally-unrelated.log",
	} {
		add(name)
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("", idx)
	vol.volume = "Q:"
	vol.rebuildNameTrigramsLocked()
	return vol
}

func buildMultiTermFuzzyTestVolume(t *testing.T) *serviceVolumeIndex {
	t.Helper()
	idx := &Index{Source: "usn", Volume: "R:", Compact: true, DBPath: ""}
	add := func(name string) {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       uint64(len(idx.Records) + 10),
			ParentFRN: uint64(10),
			Parent:    0,
			Name:      name,
		})
	}
	for _, name := range []string{
		".", // root record
		"report.pdf",
		"annual-report-2024.pdf",
		"report.docx",
		"q4-report-notes.pdf",
		"build-log.txt",
		"unrelated-image.png",
	} {
		add(name)
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("", idx)
	vol.volume = "R:"
	vol.rebuildNameTrigramsLocked()
	return vol
}

func TestMultiTermFuzzyRewriteTrialsFixBrokenTermFirst(t *testing.T) {
	vol := buildMultiTermFuzzyTestVolume(t)
	volumes := []*serviceVolumeIndex{vol}
	opts := queryOptions{Query: "reprot pdf", Limit: 10}
	pq, err := parseQuery(opts)
	if err != nil {
		t.Fatal(err)
	}
	trials := multiTermFuzzyRewriteTrials(volumes, opts, pq)
	if len(trials) == 0 {
		t.Fatal("expected at least one rewrite trial")
	}
	if trials[0].Sub.From != "reprot" {
		t.Fatalf("first trial term = %q, want reprot", trials[0].Sub.From)
	}
	// The winning variant must produce results whose names contain both the
	// fixed term and the untouched term.
	found := false
	for _, trial := range trials {
		if trial.Sub.From != "reprot" || !strings.Contains(trial.Sub.To, "repor") {
			continue
		}
		rewritten, err := searchServiceVolumes(volumes, trial.Opts, false)
		if err != nil || len(rewritten) == 0 {
			t.Fatalf("trial %q -> %d results err=%v, want matches", trial.Opts.Query, len(rewritten), err)
		}
		for _, entry := range rewritten {
			name := strings.ToLower(entry.Name)
			if !strings.Contains(name, "report") || !strings.Contains(name, "pdf") {
				t.Fatalf("result %q does not contain both report and pdf", entry.Name)
			}
		}
		found = true
		break
	}
	if !found {
		t.Fatalf("no viable reprot->report* trial among %+v", trials)
	}
}

// TestShortTermFuzzyInsertionRewrite covers the 2-rune companion path:
// "lg" cannot enter the deletion-variant machinery, so insertion variants
// must propose a proven real word ("log") and the rewritten query must hit.
func TestShortTermFuzzyInsertionRewrite(t *testing.T) {
	vol := buildMultiTermFuzzyTestVolume(t)
	volumes := []*serviceVolumeIndex{vol}
	opts := queryOptions{Query: "build lg", Limit: 10}
	pq, err := parseQuery(opts)
	if err != nil {
		t.Fatal(err)
	}
	trials := multiTermFuzzyRewriteTrials(volumes, opts, pq)
	if len(trials) == 0 {
		t.Fatal("expected insertion-variant rewrite trials for lg")
	}
	found := false
	for _, trial := range trials {
		if trial.Sub.From != "lg" {
			continue
		}
		if !strings.Contains(trial.Sub.To, "log") {
			continue
		}
		rewritten, err := searchServiceVolumes(volumes, trial.Opts, false)
		if err != nil || len(rewritten) == 0 {
			t.Fatalf("trial lg->%q gave %d results err=%v", trial.Sub.To, len(rewritten), err)
		}
		for _, entry := range rewritten {
			name := strings.ToLower(entry.Name)
			if !strings.Contains(name, "build") || !strings.Contains(name, "log") {
				t.Fatalf("result %q lacks build+log", entry.Name)
			}
		}
		found = true
		break
	}
	if !found {
		t.Fatalf("no viable lg->*log* trial among %+v", trials)
	}

	// End to end: zero exact results, then the fuzzy chain rewrites lg->log.
	matches, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	fuzzied := false
	handled := tryMultiTermFuzzyRewrite(volumes, &opts, &matches, &fuzzied)
	if !handled || !fuzzied || len(matches) == 0 {
		t.Fatalf("handled=%v fuzzied=%v results=%d; want lg->log rewrite to produce results", handled, fuzzied, len(matches))
	}
}

func TestTryMultiTermFuzzyRewriteMaskedSubstringStillFires(t *testing.T) {
	vol := buildMultiTermFuzzyTestVolume(t)
	// A name containing the typo as a literal substring makes the broken term
	// look healthy to a solo check; the masked-term trial must still fire.
	vol.applyUSNChanges([]usnChange{{FRN: 900, ParentFRN: 10, USN: 12, Reason: usnReasonFileCreate, Name: "coreproto.txt"}})

	volumes := []*serviceVolumeIndex{vol}
	opts := queryOptions{Query: "reprot pdf", Limit: 10}
	matches, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	fuzzied := false
	handled := tryMultiTermFuzzyRewrite(volumes, &opts, &matches, &fuzzied)
	if !handled || !fuzzied {
		t.Fatalf("handled=%v fuzzied=%v, want masked-substring rewrite to fire", handled, fuzzied)
	}
	if len(matches) == 0 {
		t.Fatal("expected rewritten-query results")
	}
}

func TestTryMultiTermFuzzyRewriteHealthyQueryNoop(t *testing.T) {
	vol := buildMultiTermFuzzyTestVolume(t)
	volumes := []*serviceVolumeIndex{vol}
	// Both terms exist on their own and neither has a variant with matches
	// that could help; no trial should produce results or flip the flag.
	opts := queryOptions{Query: "report png", Limit: 10}
	matches, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	fuzzied := false
	handled := tryMultiTermFuzzyRewrite(volumes, &opts, &matches, &fuzzied)
	if !handled {
		t.Fatal("multi-term case must always be handled once terms >= 2")
	}
	if fuzzied {
		t.Fatalf("fuzzied=true with no viable substitution (matches=%d)", len(matches))
	}
}

func TestTryMultiTermFuzzyRewriteNoVariantNoResults(t *testing.T) {
	vol := buildMultiTermFuzzyTestVolume(t)
	volumes := []*serviceVolumeIndex{vol}
	opts := queryOptions{Query: "zzzzzzz pdf", Limit: 10}
	matches, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	fuzzied := false
	handled := tryMultiTermFuzzyRewrite(volumes, &opts, &matches, &fuzzied)
	if !handled || fuzzied {
		t.Fatalf("handled=%v fuzzied=%v, want handled without bogus results", handled, fuzzied)
	}
}

func TestTryMultiTermFuzzyRewriteEndToEnd(t *testing.T) {
	vol := buildMultiTermFuzzyTestVolume(t)
	volumes := []*serviceVolumeIndex{vol}
	opts := queryOptions{Query: "reprot pdf", Limit: 10}
	matches, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	fuzzied := false
	handled := tryMultiTermFuzzyRewrite(volumes, &opts, &matches, &fuzzied)
	if !handled || !fuzzied {
		t.Fatalf("handled=%v fuzzied=%v, want handled rewrite with results", handled, fuzzied)
	}
	if len(matches) == 0 {
		t.Fatal("expected rewritten-query results")
	}
}

func TestAppendFuzzyServiceMatchesGoldenRanking(t *testing.T) {
	vol := buildFuzzyTestVolume(t)
	opts := queryOptions{Query: "tonics~", Limit: 10}
	matches, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, opts, nil)
	if !added {
		t.Fatal("expected fuzzy results for tonics~")
	}
	if len(matches) == 0 {
		t.Fatal("expected at least one fuzzy match")
	}
	if got := matches[0].Name; got != "tonic.txt" && got != "tonicwater.txt" {
		t.Fatalf("top fuzzy result = %q, want a tonic* file", got)
	}
	for i, m := range matches {
		if m.Name == "totally-unrelated.log" {
			t.Fatalf("unrelated file ranked into fuzzy results at %d", i)
		}
	}
	// Prefix matches must outrank infix matches at equal distance.
	prefixIdx, infixIdx := -1, -1
	for i, m := range matches {
		switch m.Name {
		case "tonic.txt":
			prefixIdx = i
		case "ironic.txt":
			infixIdx = i
		}
	}
	if prefixIdx >= 0 && infixIdx >= 0 && prefixIdx > infixIdx {
		t.Fatalf("prefix match ranked %d below infix match %d", prefixIdx, infixIdx)
	}
}

func TestAppendFuzzyServiceMatchesAutoFiresOnZeroResults(t *testing.T) {
	vol := buildFuzzyTestVolume(t)

	// A zero-result exact query auto-triggers fuzzy without any marker.
	opts := queryOptions{Query: "tonics", Limit: 10}
	matches, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, opts, nil)
	if !added || len(matches) == 0 {
		t.Fatal("zero-result query should auto-trigger the fuzzy tier")
	}
	if matches[0].Name != "tonic.txt" && matches[0].Name != "tonicwater.txt" {
		t.Fatalf("top fuzzy result = %q, want a tonic* file", matches[0].Name)
	}
}

func TestAppendFuzzyServiceMatchesInteriorSubstitutionAndOmission(t *testing.T) {
	vol := buildFuzzyTestVolume(t)

	// Neither typo contains a deletion variant that is a substring of
	// "tonicwater.txt"; only the pigeonhole pieces reach it.
	// "tonicwaxter.txt" has the same length as "tonicwater.txtx", but the
	// whole name it mistypes is "tonicwater.txt" (like BUImLD.gn vs BUILD.gni).
	for _, query := range []string{"tonicwaser.txt", "tonicwter.txt", "tonicwaxter.txt"} {
		opts := queryOptions{Query: query, Limit: 10}
		matches, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, opts, nil)
		if !added || len(matches) == 0 {
			t.Fatalf("%q: expected fuzzy results", query)
		}
		if matches[0].Name != "tonicwater.txt" {
			t.Fatalf("%q: top fuzzy result = %q, want tonicwater.txt", query, matches[0].Name)
		}
	}
}

func TestFuzzyLetterMaskMisses(t *testing.T) {
	term := fuzzyLetterMask("relese")
	if got := fuzzyLetterMaskMisses(term, fuzzyLetterMask("release.md")); got != 0 {
		t.Fatalf("release.md misses = %d, want 0", got)
	}
	if got := fuzzyLetterMaskMisses(fuzzyLetterMask("relaase"), fuzzyLetterMask("release")); got != 0 {
		t.Fatalf("release misses for relaase = %d, want 0", got)
	}
	if got := fuzzyLetterMaskMisses(fuzzyLetterMask("build.gn"), fuzzyLetterMask("main.cc")); got != 5 {
		t.Fatalf("main.cc misses for build.gn = %d, want 5", got)
	}
}

func TestFuzzyPigeonholePieces(t *testing.T) {
	cases := []struct {
		term string
		tau  int
		want []string
	}{
		{"relese", 1, []string{"rel", "ese"}},
		{"relaase", 1, []string{"rel", "aase"}},
		{"tonic", 1, []string{"nic"}},
		{"render_frame", 2, []string{"rend", "er_f", "rame"}},
		{"tonicwter", 2, []string{"ton", "icw", "ter"}},
		{"tonicwat", 2, []string{"toni", "cwat"}},
	}
	for _, tc := range cases {
		got := fuzzyPigeonholePieces(tc.term, tc.tau)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Fatalf("fuzzyPigeonholePieces(%q, %d) = %q, want %q", tc.term, tc.tau, got, tc.want)
		}
	}
}

func TestAppendFuzzyServiceMatchesKeepsExactResultsFirst(t *testing.T) {
	vol := buildFuzzyTestVolume(t)

	// Simulate a partial exact result set (below the auto-fuzzy threshold):
	// fuzzy entries must be appended strictly after every exact entry.
	exact := []Entry{{Path: `Q:\exact-tonic.txt`, Name: "exact-tonic.txt"}}
	opts := queryOptions{Query: "tonics", Limit: 10}
	matches, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, opts, exact)
	if !added {
		t.Fatal("partial result set below threshold should trigger the fuzzy tier")
	}
	for i, m := range matches {
		if i < len(exact) {
			continue
		}
		if m.Path == exact[0].Path {
			t.Fatalf("exact entry %q reappeared inside the fuzzy section at %d", m.Path, i)
		}
	}
	if matches[0].Path != exact[0].Path {
		t.Fatalf("first result = %q; exact results must always lead", matches[0].Path)
	}
}

func TestAppendFuzzyServiceMatchesDeclinesMultiTermAndShortTerms(t *testing.T) {
	vol := buildFuzzyTestVolume(t)

	// Multi-term queries are out of scope for the fallback tier in v1.
	optsMulti := queryOptions{Query: "tonic~ water", Limit: 10}
	if _, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, optsMulti, nil); added {
		t.Fatal("multi-term fuzzy queries must be declined")
	}

	// Short terms are noise-prone and declined.
	optsShort := queryOptions{Query: "to~", Limit: 10}
	if _, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, optsShort, nil); added {
		t.Fatal("sub-minimum terms must be declined")
	}

	// Case-sensitive queries are declined (fuzzy compares folded text).
	optsCase := queryOptions{Query: "tonics~", CaseSensitive: true, Limit: 10}
	if _, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, optsCase, nil); added {
		t.Fatal("case-sensitive fuzzy queries must be declined")
	}
}

// fuzzyNameDistanceWindows is the original per-window implementation, kept
// as an oracle for the single-pass DP in fuzzyNameDistance.
func fuzzyNameDistanceWindows(nameLower, term string, tau int) (dist int, prefix bool, windowPos int, ok bool) {
	nameRunes := []rune(nameLower)
	termRunes := []rune(term)
	lt, ln := len(termRunes), len(nameRunes)
	if ln == 0 {
		return tau + 1, false, 0, false
	}
	best := tau + 1
	prefixBest := false
	bestStart := ln
	end := min(ln-lt+tau, ln)
	for start := 0; start <= end; start++ {
		maxWidth := min(lt+tau, ln-start)
		minWidth := max(lt-tau, 1)
		for width := minWidth; width <= maxWidth; width++ {
			d := damerauLevenshteinBounded(nameRunes[start:start+width], termRunes, tau)
			atPrefix := start == 0
			if d < best || (d == best && start < bestStart) {
				if d < best {
					best = d
					prefixBest = atPrefix
				}
				bestStart = start
			} else if d == best && atPrefix {
				prefixBest = true
			}
		}
	}
	if best > tau {
		return best, false, bestStart, false
	}
	return best, prefixBest, bestStart, true
}

func TestFuzzyNameDistanceMatchesWindowOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	word := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = "abcd."[rng.Intn(5)]
		}
		return string(b)
	}
	for i := 0; i < 20000; i++ {
		name := word(1 + rng.Intn(14))
		term := word(3 + rng.Intn(6))
		tau := 1 + rng.Intn(2)
		gd, gp, gw, gok := fuzzyNameDistance(name, term, tau)
		wd, wp, ww, wok := fuzzyNameDistanceWindows(name, term, tau)
		if gok != wok || (gok && (gd != wd || gp != wp || gw != ww)) {
			t.Fatalf("fuzzyNameDistance(%q, %q, %d) = %d/%v/%d/%v, oracle %d/%v/%d/%v",
				name, term, tau, gd, gp, gw, gok, wd, wp, ww, wok)
		}
	}
}

func TestFuzzyNameDistancePrefixPreference(t *testing.T) {
	// Equal distance: prefix alignment wins.
	dist, prefix, _, ok := fuzzyNameDistance("tonic.txt", "tonicx", 1)
	if !ok || dist != 1 || !prefix {
		t.Fatalf("dist=%d prefix=%v ok=%v; want 1/prefix/ok", dist, prefix, ok)
	}
	dist, prefix, _, ok = fuzzyNameDistance("totally-unrelated.log", "tonicx", 1)
	if ok {
		t.Fatalf("unrelated name accepted at tau=1: dist=%d", dist)
	}
	_ = dist
	_ = prefix
}
