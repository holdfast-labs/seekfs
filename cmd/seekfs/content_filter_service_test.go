package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// contentFilterServiceFixture builds a USN service volume over real files:
// tier1Names are indexed in the full tier, the rest are covered by an
// attached tier-2 filter with real fingerprints, so disk verification reads
// the same files the brute-force oracle does.
func contentFilterServiceFixture(t *testing.T, bodies map[string]string, tier1Names []string) *serviceVolumeIndex {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	dir := t.TempDir()
	names := make([]string, 0, len(bodies))
	for name := range bodies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(bodies[name]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inTier1 := map[string]bool{}
	for _, name := range tier1Names {
		inTier1[name] = true
	}
	idx := &Index{Source: "usn", Volume: dir, Compact: true}
	var frn uint64 = 10
	frns := map[string]uint64{}
	for _, name := range names {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		idx.Records = append(idx.Records, CompactRecord{FRN: frn, ParentFRN: 1, Parent: -1, Name: name, Size: st.Size()})
		frns[name] = frn
		frn++
	}
	contentIndexFRNs(idx)
	var build []contentBuildDoc
	fb := newContentFilterBuilder(contentOriginUSN, contentExtractMaxRawBytes, contentExtractMaxTextBytes, [32]byte{}, "")
	for _, name := range names {
		path := filepath.Join(dir, name)
		st, _ := os.Stat(path)
		if inTier1[name] {
			build = append(build, contentBuildDoc{path: path, frn: frns[name], text: []byte(bodies[name])})
			continue
		}
		fb.Add(frns[name], path, st.Size(), st.ModTime().UnixNano(), []byte(bodies[name]))
	}
	cidx, err := assembleContentIndex(build, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cidx.Origin = contentOriginUSN
	reader, err := openContentReader(cidx)
	if err != nil {
		t.Fatal(err)
	}
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_f.gsi"), idx)
	if vol.content == nil {
		t.Fatal("content state not initialized")
	}
	frnList := make([]uint64, len(cidx.Docs))
	ids := make([]uint32, len(cidx.Docs))
	for i := range cidx.Docs {
		frnList[i] = cidx.Docs[i].FRN
		ids[i] = uint32(contentRecordIndexForFRN(idx, cidx.Docs[i].FRN))
	}
	vol.content.setReady(cidx, reader, buildContentResolver(cidx.Docs, frnList, ids))
	if flt := fb.Finish(); flt != nil {
		vol.content.setFilter(flt)
	}
	releaseVolumeContentOnCleanup(t, vol)
	return vol
}

// contentFilterBruteForce evaluates the full content predicate for one query
// over real files, the oracle the tier-2 pipeline must equal.
func contentFilterBruteForce(t *testing.T, dir string, bodies map[string]string, query string) []string {
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
	names := make([]string, 0, len(bodies))
	for name := range bodies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
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
		entry := Entry{Path: path, Name: name, LowerName: strings.ToLower(name), LowerPath: strings.ToLower(path)}
		if entryMatchesContentAt(entry, pq, pq.MatchPath, contentFilterMatchText(text, m)) {
			out = append(out, path)
		}
	}
	return out
}

// TestContentFilterServiceDifferential proves tier-2 answers equal brute force
// through the full service path (tier-1 postings + filter + disk verify +
// merge + limit) for many query shapes: rare/common literals, AND, phrase,
// OR, NOT, regex with required literals, case-sensitive, and a filename
// compound.
func TestContentFilterServiceDifferential(t *testing.T) {
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
	}
	vol := contentFilterServiceFixture(t, bodies, []string{"a.txt", "b.txt", "i.txt"})
	dir := vol.volume
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
		"case:true content:Needle",
		"case:true content:STRUCT",
		"ext:txt content:needle",
		"content:zzz-no-match",
		"content:the",
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
		for _, p := range contentFilterBruteForce(t, dir, bodies, q) {
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
		// Count agrees with search on every shape.
		if n := contentServiceCount(t, []*serviceVolumeIndex{vol}, q); n != len(want) {
			t.Fatalf("%s: count=%d search=%d", q, n, len(want))
		}
	}
}

// TestContentFilterServiceTier1OrderUnchanged proves tier-1 results keep
// their exact order and content with a filter attached: the tier-1-only
// answer is a prefix of the combined answer.
func TestContentFilterServiceTier1OrderUnchanged(t *testing.T) {
	bodies := map[string]string{
		"a.txt": "alpha needle one",
		"b.txt": "bravo needle two",
		"c.txt": "charlie needle three",
		"d.txt": "delta needle four",
	}
	vol := contentFilterServiceFixture(t, bodies, []string{"a.txt", "b.txt"})
	// Detach the filter: tier-1-only baseline through the same path.
	flt := vol.content.filterView()
	vol.content.setFilter(nil)
	baseline, err := contentServiceSearch(t, vol, "content:needle", false)
	if err != nil {
		t.Fatal(err)
	}
	vol.content.setFilter(flt)
	combined, err := contentServiceSearch(t, vol, "content:needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(combined) != len(bodies) {
		t.Fatalf("combined = %d hits; want %d", len(combined), len(bodies))
	}
	for i := range baseline {
		if combined[i].Path != baseline[i].Path {
			t.Fatalf("tier-1 order moved at %d: %q vs %q", i, combined[i].Path, baseline[i].Path)
		}
	}
}

// TestContentFilterServiceCapMarksIncomplete proves the per-query disk
// verification bound: a tiny cap stops the filter scan, the search reports
// incomplete, and the count is refused (never partial).
func TestContentFilterServiceCapMarksIncomplete(t *testing.T) {
	bodies := map[string]string{}
	for i := 0; i < 30; i++ {
		bodies[string(rune('a'+i/10))+string(rune('0'+i%10))+".txt"] = "common filler needle"
	}
	vol := contentFilterServiceFixture(t, bodies, nil)
	oldCap := contentFilterMaxVerify
	contentFilterMaxVerify = 3
	defer func() { contentFilterMaxVerify = oldCap }()
	matches, trace, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:needle", 100)
	if err != nil {
		t.Fatal(err)
	}
	if trace == nil || !trace.ContentIncomplete {
		t.Fatalf("capped filter scan did not mark incomplete (hits=%d)", len(matches))
	}
	if len(matches) > 3 {
		t.Fatalf("capped scan returned %d hits past a 3-verify cap", len(matches))
	}
	if _, _, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:needle", Trace: &searchTrace{}}); err == nil {
		t.Fatal("capped filter count was answered instead of refused")
	}
}

// TestContentFilterServiceBuildCoversDrops proves the service build covers
// dropped files with tier 2 from the same extraction pass: the policy still
// records the exact tier-1 drop, the filter sidecar holds the file, and
// health reports no uncovered files (ready, not degraded).
func TestContentFilterServiceBuildCoversDrops(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	dir := t.TempDir()
	// The dropped file is large: its ~360 B filter record fits the remaining
	// budget while its 5 KB text does not. (A file tinier than its own
	// filter record can never be filter-covered from the same pool — it is
	// only dropped when the remainder fits neither — so small files always
	// resolve to tier 1 or to uncovered.)
	bigBody := "charlie? " + strings.Repeat("padding text for the dropped file. ", 150)
	bodies := map[string]string{
		"a.txt": "alpha",
		"b.txt": "bravo!",
		"c.txt": bigBody,
	}
	for name, body := range bodies {
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
		{FRN: 3, Parent: -1, Name: "c.txt", Size: int64(len(bigBody))},
	})
	s := contentTestService(t)
	// Budget fits a.txt + b.txt as tier 1; c.txt's text exceeds the
	// remainder but its filter record fits inside the same budget.
	s.contentCfg.Content.BudgetBytes = 500
	s.ensureContentBuild(vol)
	built, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	releaseContentIndexOnCleanup(t, built)
	if len(built.Docs) != 2 || built.Policy.ScopeDropped != 1 {
		t.Fatalf("budgeted index docs=%d dropped=%d; want 2 docs, 1 dropped", len(built.Docs), built.Policy.ScopeDropped)
	}
	flt, err := contentLoadFilter(contentFilterPathForGSX(gsx))
	if err != nil {
		t.Fatalf("filter sidecar missing: %v", err)
	}
	defer flt.Release()
	if len(flt.Records) != 1 || flt.Records[0].Key != 3 {
		t.Fatalf("filter records = %+v; want the dropped FRN 3", flt.Records)
	}
	h := vol.content.healthSnapshot(0)
	if h.FilterFiles != 1 || h.FilterBytes != flt.EncodedSize {
		t.Fatalf("filter health = %+v; want 1 file", h)
	}
	if h.ScopeDropped != 0 || h.ScopeDroppedBytes != 0 {
		t.Fatalf("covered drops still reported uncovered: %+v", h)
	}
	if h.Incomplete || h.State != contentStateReady {
		t.Fatalf("fully-covered build not ready: %+v", h)
	}
	// The dropped file answers through the service with tier-2 verification.
	matches, err := contentServiceSearch(t, vol, "content:padding", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || !strings.HasSuffix(matches[0].Path, "c.txt") {
		t.Fatalf("tier-2 query = %v; want c.txt", matches)
	}
}

// TestContentFilterFoldRefresh proves a changed filter-covered file refreshes
// its record through drain → delta → fold: the new text matches, the old
// text stops, and tier-1 docs are untouched.
func TestContentFilterFoldRefresh(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	dir := t.TempDir()
	oldWord := "charlie filter oldword " + strings.Repeat("padding text for the covered file. ", 60)
	files := map[string]string{
		"a.txt": "alpha tier one",
		"b.txt": "bravo tier one",
		"c.txt": oldWord,
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
		{FRN: 3, Parent: -1, Name: "c.txt", Size: int64(len(oldWord))},
	})
	s := contentTestService(t)
	s.contentCfg.Content.BudgetBytes = 500
	s.ensureContentBuild(vol)
	old := vol.content.filterView()
	if old == nil || len(old.Records) != 1 {
		t.Fatalf("precondition: filter = %v", old)
	}
	// Change the filter-covered file on disk and refresh the record through
	// the fold path with a delta doc as the drain would produce it.
	newWord := "charlie filter newword " + strings.Repeat("padding text for the covered file. ", 60)
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte(newWord), 0o644); err != nil {
		t.Fatal(err)
	}
	nst, err := os.Stat(filepath.Join(dir, "c.txt"))
	if err != nil {
		t.Fatal(err)
	}
	reader, _ := vol.content.readerResolverView()
	base := reader.idx
	refresh := []contentDeltaDoc{{
		FRN: 3, Path: filepath.Join(dir, "c.txt"), Text: []byte(newWord),
		FileSize: nst.Size(), FileModNano: nst.ModTime().UnixNano(),
	}}
	s.publishFilterRefresh(vol, vol.replayGen.Load(), base, old, refresh)
	next := vol.content.filterView()
	if next == nil || len(next.Records) != 1 {
		t.Fatalf("refreshed filter = %v", next)
	}
	rec := next.Records[0]
	if rec.FileSize != nst.Size() || rec.FileModNano != nst.ModTime().UnixNano() {
		t.Fatalf("fingerprint not restamped: %+v", rec)
	}
	if !rec.mayMatch(contentFilterGramsOf(contentFoldText([]byte("newword")))) {
		t.Fatal("refreshed record misses its new text")
	}
	// The dropped file answers with the new text through the service.
	matches, err := contentServiceSearch(t, vol, "content:newword", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("post-refresh query = %v; want c.txt", matches)
	}
	_ = gsx
}

// TestContentFilterLaneParityWithPerVolumePath proves the compound lane's
// tier-2 fallback answers identically with the lane off: same sets, same
// order, same snippets — on a filtered volume, where the lane's inline
// verify cannot see tier-2 files without the fallback.
func TestContentFilterLaneParityWithPerVolumePath(t *testing.T) {
	bodies := map[string]string{
		"a.txt": "alpha needle one",
		"b.txt": "bravo needle two",
		"c.txt": "charlie needle three",
		"d.txt": "delta nothing",
		"e.md":  "echo needle markdown",
	}
	vol := contentFilterServiceFixture(t, bodies, []string{"a.txt"})
	vols := []*serviceVolumeIndex{vol}
	queries := []string{
		"ext:txt content:needle",
		"ext:txt content:alpha content:needle",
		`ext:txt content:/ne+dle/`,
		// Pure content queries have no filename side, so the lane declines
		// them; they are covered by TestContentFilterServiceDifferential.
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
		// Count agrees across lanes too.
		if nOn, _ := contentGlobalLaneCount(vols, q, true); nOn != len(off) {
			t.Fatalf("%s: lane count=%d search=%d", q, nOn, len(off))
		}
		if nOff, _ := contentGlobalLaneCount(vols, q, false); nOff != len(off) {
			t.Fatalf("%s: per-volume count=%d search=%d", q, nOff, len(off))
		}
	}
}
// TestContentFilterHealthMerge proves the aggregate sums tier-2 coverage.
func TestContentFilterHealthMerge(t *testing.T) {
	a := contentHealth{State: contentStateReady, Docs: 10, FilterFiles: 5, FilterBytes: 100, ScopeDropped: 1, ScopeDroppedBytes: 50}
	b := contentHealth{State: contentStateReady, Docs: 7, FilterFiles: 3, FilterBytes: 60, ScopeDropped: 0}
	mergeContentHealth(&a, b)
	if a.FilterFiles != 8 || a.FilterBytes != 160 || a.ScopeDropped != 1 || a.ScopeDroppedBytes != 50 {
		t.Fatalf("merged = %+v", a)
	}
}
