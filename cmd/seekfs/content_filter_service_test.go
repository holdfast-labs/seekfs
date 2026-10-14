package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// contentFilterV11Fixture builds a USN service volume over real files with a
// v11 index (postings + filter, no stored text): every file is verified from
// disk, so the fixture doubles as the freshness oracle — no stubs.
func contentFilterV11Fixture(t *testing.T, bodies map[string]string) *serviceVolumeIndex {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	dir := t.TempDir()
	names := make([]string, 0, len(bodies))
	for name := range bodies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(bodies[name]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx := &Index{Source: "usn", Volume: dir, Compact: true}
	var frn uint64 = 10
	var build []contentBuildDoc
	for _, name := range names {
		full := filepath.Join(dir, filepath.FromSlash(name))
		st, err := os.Stat(full)
		if err != nil {
			t.Fatal(err)
		}
		idx.Records = append(idx.Records, CompactRecord{FRN: frn, ParentFRN: 1, Parent: -1, Name: filepath.Base(name), Size: st.Size()})
		build = append(build, contentBuildDoc{path: full, frn: frn, text: []byte(bodies[name])})
		frn++
	}
	contentIndexFRNs(idx)
	cidx, err := assembleContentIndex(build, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cidx.Origin = contentOriginUSN
	reader, err := openContentReader(cidx)
	if err != nil {
		t.Fatal(err)
	}
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_v11.gsi"), idx)
	if vol.content == nil {
		t.Fatal("content state not initialized")
	}
	frns := make([]uint64, len(cidx.Docs))
	ids := make([]uint32, len(cidx.Docs))
	for i := range cidx.Docs {
		frns[i] = cidx.Docs[i].FRN
		ids[i] = uint32(contentRecordIndexForFRN(idx, cidx.Docs[i].FRN))
	}
	vol.content.setReady(cidx, reader, buildContentResolver(cidx.Docs, frns, ids))
	vol.rebuildNameTrigramsLocked()
	releaseVolumeContentOnCleanup(t, vol)
	return vol
}

// contentFilterV11BruteForce evaluates the full content predicate for one
// query over real files: the oracle the v11 pipeline must equal. Entry paths
// are the reconstructed record paths, exactly as the service builds them.
func contentFilterV11BruteForce(t *testing.T, vol *serviceVolumeIndex, bodies map[string]string, query string) []string {
	t.Helper()
	opts := queryOptions{Query: query}
	pq, err := parseQuery(opts)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	m := newContentLeafMatcher(pq)
	settings := contentServiceExtractSettings()
	ctx := contentWithExtractSettings(t.Context(), settings)
	var out []string
	cache := map[int]string{}
	for id := 0; id < vol.index.compactRecordCount(); id++ {
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, cache)
		if path == "" {
			continue
		}
		s, serr := contentSlurpNoRecall(path, settings.maxRaw)
		if serr != nil {
			continue
		}
		e := contentExtractorForPath(path, s.headBytes())
		if e == nil {
			s.close()
			continue
		}
		res, eerr := contentExtractSafely(ctx, e, s.reader(), s.size)
		s.close()
		if eerr != nil || res.Skipped || res.Binary || len(res.Text) == 0 {
			continue
		}
		text := contentStoredText(res.Text)
		entry := Entry{Path: path, Name: rec.Name, LowerName: strings.ToLower(rec.Name), LowerPath: strings.ToLower(path)}
		matched := make([]bool, m.size)
		var folded []byte
		foldedReady := false
		for _, leaf := range m.leaves {
			matched[leaf.LeafID] = m.matchRawFirst(text, &folded, &foldedReady, leaf)
		}
		if entryMatchesContentAt(entry, pq, pq.MatchPath, matched) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// TestContentFilterV11ServiceDifferential proves v11 answers equal brute
// force through the full service path (postings + filter gate + disk verify
// + merge + limit) for every query shape: rare/common literals, AND,
// phrase, OR, NOT, regex with required literals, case-sensitive, filename
// compounds, and the empty answer. Count agrees with search throughout.
func TestContentFilterV11ServiceDifferential(t *testing.T) {
	bodies := map[string]string{
		"a.txt": "alpha needle one",
		"b.txt": "bravo nothing here",
		"c.txt": "Needle in text with alpha",
		"d.txt": "delta mutex lock",
		"e.txt": "echo paintlayer reassemble",
		"f.txt": "foxtrot cityhash_v103 special",
		"g.txt": "golf the common words",
		"h.txt": "hotel ibex fold",
		"i.txt": "india",
		"j.txt": "juliet needle needle needle",
		"k.txt": "kilo",
		"l.txt": "lima mutex and needle together",
		"m.txt": "mike phrase foo bar baz",
		"n.txt": "november foo bar",
		"o.txt": "oscar",
		"p.txt": "papa STRUCT",
		"q.txt": "quebec mutex needle",
	}
	vol := contentFilterV11Fixture(t, bodies)
	vols := []*serviceVolumeIndex{vol}
	queries := []string{
		"content:needle",
		"content:NEEDLE",
		"content:mutex",
		"content:alpha content:needle",
		`content:"foo bar"`,
		"content:alpha|content:mutex",
		"content:needle !content:juliet",
		`content:/ne+dle/`,
		`content:/mutex_\w+/`,
		`content:/zzz\d+/`,
		"case:true content:Needle",
		"case:true content:STRUCT",
		"ext:txt content:needle",
		"content:zzz-no-match",
		"content:the",
		"content:ab",
	}
	for _, q := range queries {
		matches, err := contentServiceSearch(t, vol, q, false)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		got := map[string]bool{}
		for _, e := range matches {
			got[e.Path] = true
			if e.Snippet == "" && !strings.HasPrefix(q, "content:/") {
				t.Fatalf("%s: hit %q has no snippet", q, e.Path)
			}
		}
		want := map[string]bool{}
		for _, p := range contentFilterV11BruteForce(t, vol, bodies, q) {
			want[p] = true
		}
		if len(got) != len(want) {
			t.Fatalf("%s: got %v; want %v", q, got, want)
		}
		for p := range want {
			if !got[p] {
				t.Fatalf("%s: missing %q in %v", q, p, got)
			}
		}
		if n := contentServiceCount(t, vols, q); n != len(want) {
			t.Fatalf("%s: count=%d search=%d", q, n, len(want))
		}
	}
}

// TestContentFilterV11LaneParity proves the compound lane answers identically
// with the lane off on real files: same sets, order, snippets and counts.
func TestContentFilterV11LaneParity(t *testing.T) {
	bodies := map[string]string{
		"a.txt": "alpha needle one",
		"b.txt": "bravo needle two",
		"c.txt": "charlie needle three",
		"d.txt": "delta nothing",
		"e.md":  "echo needle markdown",
	}
	vol := contentFilterV11Fixture(t, bodies)
	vols := []*serviceVolumeIndex{vol}
	queries := []string{
		"ext:txt content:needle",
		"ext:txt content:alpha content:needle",
		`ext:txt content:/ne+dle/`,
	}
	for _, q := range queries {
		on, onTrace := contentGlobalLaneSearch(vols, q, 100, true)
		off, _ := contentGlobalLaneSearch(vols, q, 100, false)
		if onTrace == nil || onTrace.PlannerMode != "global-content-components" {
			mode := ""
			if onTrace != nil {
				mode = onTrace.PlannerMode
			}
			t.Fatalf("%s: lane did not engage (mode=%q); the parity is vacuous", q, mode)
		}
		if len(on) != len(off) {
			t.Fatalf("%s: lane=%d hits, per-volume=%d hits", q, len(on), len(off))
		}
		for i := range on {
			if on[i].Path != off[i].Path || on[i].Snippet != off[i].Snippet {
				t.Fatalf("%s: hit %d differs:\nlane=%+v\nslow =%+v", q, i, on[i], off[i])
			}
		}
		if nOn, _ := contentGlobalLaneCount(vols, q, true); nOn != len(off) {
			t.Fatalf("%s: lane count=%d search=%d", q, nOn, len(off))
		}
		if nOff, _ := contentGlobalLaneCount(vols, q, false); nOff != len(off) {
			t.Fatalf("%s: per-volume count=%d search=%d", q, nOff, len(off))
		}
	}
}

// TestContentDiskVerifyBudgetBoundsQuery proves the per-query disk bound on a
// live volume: a tiny budget stops the search (incomplete, bounded hits)
// and refuses the count (never partial).
func TestContentDiskVerifyBudgetBoundsQuery(t *testing.T) {
	bodies := map[string]string{}
	for i := 0; i < 30; i++ {
		bodies["f"+string(rune('a'+i/10))+string(rune('0'+i%10))+".txt"] = "common filler needle"
	}
	vol := contentFilterV11Fixture(t, bodies)
	prev := contentDiskVerifyBudget
	contentDiskVerifyBudget = 3
	t.Cleanup(func() { contentDiskVerifyBudget = prev })
	matches, trace, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:needle", 100)
	if err != nil {
		t.Fatal(err)
	}
	if trace == nil || !trace.ContentIncomplete {
		t.Fatalf("capped query did not mark incomplete (hits=%d)", len(matches))
	}
	if len(matches) > 3 {
		t.Fatalf("capped query returned %d hits past a 3-verify budget", len(matches))
	}
	if _, _, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:needle", Trace: &searchTrace{}}); err == nil {
		t.Fatal("capped count was answered instead of refused")
	}
}

// TestContentFilterV11BuildBudgetDropsUncovered proves budget-dropped files
// have no coverage in v11 (no sidecar): the policy records the exact drop,
// health reports it uncovered, and queries miss it (fail-closed, honest).
func TestContentFilterV11BuildBudgetDropsUncovered(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	dir := t.TempDir()
	var bigBody strings.Builder
	bigBody.WriteString("charlie padding ")
	for i := 0; i < 150; i++ {
		bigBody.WriteString("padding text for the dropped file. ")
	}
	big := bigBody.String()
	for name, body := range map[string]string{"a.txt": "alpha", "b.txt": "bravo!", "c.txt": big} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const journal = uint64(95)
	const cp = int64(103)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 1, Parent: -1, Name: "a.txt", Size: 5},
		{FRN: 2, Parent: -1, Name: "b.txt", Size: 6},
		{FRN: 3, Parent: -1, Name: "c.txt", Size: int64(len(big))},
	})
	s := contentTestService(t)
	// The budget is in projected index bytes: a.txt + b.txt fit, c.txt does not.
	s.contentCfg.Content.BudgetBytes = contentIndexCostEstimate(5) + contentIndexCostEstimate(6)
	s.ensureContentBuild(vol)
	built, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	releaseContentIndexOnCleanup(t, built)
	if len(built.Docs) != 2 || built.Policy.ScopeDropped != 1 {
		t.Fatalf("budgeted index docs=%d dropped=%d; want 2 docs, 1 dropped", len(built.Docs), built.Policy.ScopeDropped)
	}
	h := vol.content.healthSnapshot(0)
	if h.ScopeDropped != 1 || h.ScopeDroppedBytes != int64(len(big)) {
		t.Fatalf("health drops = %+v; want the exact uncovered file", h)
	}
	if !h.Incomplete {
		t.Fatal("budget-truncated sidecar must report incomplete")
	}
	// The dropped file is unfindable (no coverage, no stale hit)...
	if matches, _ := contentServiceSearch(t, vol, "content:padding", false); len(matches) != 0 {
		t.Fatalf("dropped file served without coverage: %v", matches)
	}
	// ...while indexed files answer.
	if matches, _ := contentServiceSearch(t, vol, "content:alpha", false); len(matches) != 1 {
		t.Fatalf("indexed file missed: %v", matches)
	}
}

// TestContentFilterV11FoldReReadsBase proves folds re-extract base docs from
// disk: a file modified behind the index (no USN, no drain) still converges
// at the next fold, and the folded base carries the new text.
func TestContentFilterV11FoldReReadsBase(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	dir := t.TempDir()
	files := map[string]string{
		"a.txt": "alpha tier one",
		"b.txt": "bravo tier one",
		"c.txt": "charlie tier one",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const journal = uint64(96)
	const cp = int64(104)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 1, Parent: -1, Name: "a.txt", Size: 14},
		{FRN: 2, Parent: -1, Name: "b.txt", Size: 14},
		{FRN: 3, Parent: -1, Name: "c.txt", Size: 15},
	})
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("build state = %q; want ready", got)
	}
	// Churn c.txt through the drain (delta) and silently rewrite a.txt on
	// disk (no USN): the fold must carry both — the delta text and the
	// re-read bytes.
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("charlie delta newword"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol.contentCoord.observeChanges([]usnChange{{FRN: 3, USN: 200, Reason: usnReasonDataOverwrite | usnReasonClose}})
	vol.contentCoord.processQueue(func(frn uint64) (string, bool) {
		if frn == 3 {
			return filepath.Join(dir, "c.txt"), true
		}
		return "", false
	})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha DISKEDIT tier one"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.runContentFold(vol)
	// The delta text published...
	if matches, _ := contentServiceSearch(t, vol, "content:newword", false); len(matches) != 1 {
		t.Fatalf("delta text lost across the fold: %v", matches)
	}
	// ...and the silently rewritten base file converged by re-read.
	if matches, _ := contentServiceSearch(t, vol, "content:DISKEDIT", false); len(matches) != 1 {
		t.Fatalf("silently rewritten base file not converged by the fold: %v", matches)
	}
	if matches, _ := contentServiceSearch(t, vol, `content:"charlie tier one"`, false); len(matches) != 0 {
		t.Fatalf("stale base text still served: %v", matches)
	}
	// The folded base carries no text store.
	reloaded, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	releaseContentIndexOnCleanup(t, reloaded)
	if _, ok := reloaded.Sections[contentSectionText]; ok {
		t.Fatal("folded base must not carry a text store")
	}
	if _, ok := reloaded.Sections[contentSectionFilter]; !ok {
		t.Fatal("folded base must carry the filter section")
	}
}

// TestContentFilterHealthMerge proves the aggregate still sums drops.
func TestContentFilterHealthMerge(t *testing.T) {
	a := contentHealth{State: contentStateReady, Docs: 10, ScopeDropped: 1, ScopeDroppedBytes: 50}
	b := contentHealth{State: contentStateReady, Docs: 7, ScopeDropped: 0}
	mergeContentHealth(&a, b)
	if a.Docs != 17 || a.ScopeDropped != 1 || a.ScopeDroppedBytes != 50 {
		t.Fatalf("merged = %+v", a)
	}
}
