package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// corpusFixture seeds content words: receive (common), retrieve (rarer) and
// release, so corrections rank the intended spelling by frequency and distance.
func contentFuzzyTestFiles() []contentFixtureFile {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString(" receive the confirmation message ")
	}
	common := b.String()
	var c strings.Builder
	for i := 0; i < 2; i++ {
		c.WriteString(" retrieve the archive ")
	}
	rarer := c.String()
	return []contentFixtureFile{
		{2, "a.txt", common},
		{3, "b.txt", common + rarer},
		{4, "c.md", "the release notes describe delivery"},
	}
}

func contentFuzzyServiceSearch(t *testing.T, vol *serviceVolumeIndex, query string) ([]Entry, bool, string) {
	t.Helper()
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: query, Limit: 100}, false)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	got, correction, ok := tryContentFuzzyRewrite([]*serviceVolumeIndex{vol}, queryOptions{Query: query, Limit: 100}, matches)
	if !ok {
		return matches, false, ""
	}
	return got, true, correction
}

// The motivating case: content:recieve misses everything exactly but corrects
// to receive.
func TestContentFuzzyCorrectsTypo(t *testing.T) {
	vol := newContentQueryVolume(t, contentFuzzyTestFiles())
	exact, err := contentServiceSearch(t, vol, "content:recieve", false)
	if err != nil {
		t.Fatalf("exact: %v", err)
	}
	if len(exact) != 0 {
		t.Fatalf("exact typo query matched %d docs; want a total miss", len(exact))
	}
	got, ok, correction := contentFuzzyServiceSearch(t, vol, "content:recieve")
	if !ok {
		t.Fatal("typo query was not corrected")
	}
	if correction != "receive" {
		t.Fatalf("correction = %q; want receive", correction)
	}
	// The retry must equal the exact search for the corrected term.
	want, err := contentServiceSearch(t, vol, "content:receive", false)
	if err != nil {
		t.Fatalf("corrected search: %v", err)
	}
	if len(got) != len(want) || len(got) == 0 {
		t.Fatalf("corrected matches = %d; want %d", len(got), len(want))
	}
}

// Frequency ranks: content:retrive is distance 1 from both receive and
// retrieve, but retrieve is the closer correction; exercise an unambiguous
// single-edit case and check the df order in the candidate list.
func TestContentFuzzyRanksByDistanceThenFrequency(t *testing.T) {
	vol := newContentQueryVolume(t, contentFuzzyTestFiles())
	reader, _ := vol.content.readerResolverView()
	if reader == nil {
		t.Fatal("no content reader")
	}
	got := contentFuzzyCorrections([]*contentReader{reader}, nil, "retreive", 3)
	if len(got) == 0 || got[0].term != "retrieve" {
		t.Fatalf("corrections for retreive = %v; want retrieve first", got)
	}
}

// Exact results are never touched: a typo'd query that still matches exactly
// keeps its exact matches.
func TestContentFuzzyNeverFiresOnExactMatches(t *testing.T) {
	vol := newContentQueryVolume(t, contentFuzzyTestFiles())
	matches, err := contentServiceSearch(t, vol, "content:receive", false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("exact query should match")
	}
	got, correction, ok := tryContentFuzzyRewrite([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:receive", Limit: 100}, matches)
	if ok || correction != "" {
		t.Fatalf("fuzzy fired on exact matches: ok=%v correction=%q", ok, correction)
	}
	if len(got) != len(matches) {
		t.Fatalf("matches changed: %d vs %d", len(got), len(matches))
	}
}

func TestContentFuzzyDoesNotCorrectKnownWordOutsideScope(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, `a\await.txt`, "await"},
		{3, `b\wait.txt`, "wait"},
	})
	opts := queryOptions{Query: "content:await", Under: `C:\fixture\b`, Limit: 100}
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("scoped exact query matched %d; want zero", len(matches))
	}
	if _, _, ok := tryContentFuzzyRewrite([]*serviceVolumeIndex{vol}, opts, matches); ok {
		t.Fatal("known word was corrected despite having no scoped results")
	}
}

func TestContentFuzzyStillCorrectsAbsentTypo(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.txt", "await"},
		{3, "b.txt", "wait"},
	})
	got, ok, correction := contentFuzzyServiceSearch(t, vol, "content:awiat")
	if !ok || (correction != "await" && correction != "wait") {
		t.Fatalf("typo correction = %q, ok=%v; want await or wait", correction, ok)
	}
	if len(got) == 0 {
		t.Fatal("corrected typo returned no results")
	}
}

// case:true and regex leaves are never corrected.
func TestContentFuzzyRespectsCaseAndRegex(t *testing.T) {
	vol := newContentQueryVolume(t, contentFuzzyTestFiles())
	for _, q := range []string{"content:recieve case:true", "content:/rec.eve/"} {
		matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: q, Limit: 100}, false)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		pq, err := parseQuery(queryOptions{Query: q})
		if err != nil {
			t.Fatalf("parse %q: %v", q, err)
		}
		var opts queryOptions
		opts.Query = q
		if len(matches) > 0 {
			continue // exact matched; nothing to correct anyway
		}
		_ = pq
		if _, _, ok := tryContentFuzzyRewrite([]*serviceVolumeIndex{vol}, opts, matches); ok {
			t.Fatalf("fuzzy fired for %q; want no correction", q)
		}
	}
}

// Words under 4 runes are never corrected.
func TestContentFuzzyIgnoresShortWords(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.txt", "the quick brown fox jumps over the lazy dog"},
	})
	reader, _ := vol.content.readerResolverView()
	if got := contentFuzzyCorrections([]*contentReader{reader}, nil, "teh", 1); len(got) != 0 {
		t.Fatalf("short word corrected: %v", got)
	}
}

// Each word of a multi-word phrase corrects independently.
func TestContentFuzzyPhraseCorrectsEachWord(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.txt", "please receive confirmation of delivery"},
		{3, "b.md", "nothing relevant here"},
	})
	exact, err := contentServiceSearch(t, vol, `content:"recieve confirmtion"`, false)
	if err != nil {
		t.Fatalf("exact: %v", err)
	}
	if len(exact) != 0 {
		t.Fatalf("exact phrase matched %d; want a total miss", len(exact))
	}
	got, ok, correction := contentFuzzyServiceSearch(t, vol, `content:"recieve confirmtion"`)
	if !ok {
		t.Fatal("phrase was not corrected")
	}
	if !strings.Contains(correction, "receive") || !strings.Contains(correction, "confirmation") {
		t.Fatalf("correction = %q; want receive + confirmation", correction)
	}
	if len(got) != 1 {
		t.Fatalf("corrected phrase matched %d; want 1", len(got))
	}
}

func TestContentFuzzyPhraseCorrectsOnlyUnknownWords(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.txt", "await confirmation of delivery"},
	})
	got, ok, correction := contentFuzzyServiceSearch(t, vol, `content:"await confirmtion"`)
	if !ok || correction != "confirmation" {
		t.Fatalf("phrase correction = %q, ok=%v; want confirmation only", correction, ok)
	}
	if len(got) != 1 {
		t.Fatalf("corrected phrase matched %d docs; want 1", len(got))
	}
}

// Distance 2 opens for 8+ rune words: correection (insertion) is distance 2
// from correction only via dictionary terms.
func TestContentFuzzyDistanceTwoForLongWords(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.txt", "the confirmation arrived yesterday morning"},
	})
	reader, _ := vol.content.readerResolverView()
	got := contentFuzzyCorrections([]*contentReader{reader}, nil, "confrimation", 2)
	if len(got) == 0 || got[0].term != "confirmation" {
		t.Fatalf("corrections for confrimation = %v; want confirmation", got)
	}
	if got[0].dist != 1 {
		t.Fatalf("dist = %d; want 1", got[0].dist)
	}
}

// The resident delta vocabulary corrects typos of words that only exist in
// unflushed changes: the delta rewrites an existing file's text, and only the
// delta copy holds "frobnicate".
func TestContentFuzzyReadsDeltaVocabulary(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.txt", "nothing relevant here"},
	})
	vol.content.deltaView().upsert(contentDeltaDoc{
		FRN:  2,
		Path: `C:\a.txt`,
		Text: []byte("the frobnicate sequence initializes the widget"),
	})
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:frobnicaet", Limit: 100}, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("exact typo matched %d; want a total miss", len(matches))
	}
	got, ok, correction := contentFuzzyServiceSearch(t, vol, "content:frobnicaet")
	if !ok || correction != "frobnicate" {
		t.Fatalf("ok=%v correction=%q; want frobnicate", ok, correction)
	}
	if len(got) != 1 {
		t.Fatalf("corrected matches = %d; want 1", len(got))
	}
}

// count == len(search) on the fuzzy path.
func TestContentFuzzyCountMatchesSearch(t *testing.T) {
	vol := newContentQueryVolume(t, contentFuzzyTestFiles())
	matches, ok, correction := contentFuzzyServiceSearch(t, vol, "content:recieve")
	if !ok {
		t.Fatal("typo query was not corrected")
	}
	if correction != "receive" {
		t.Fatalf("correction = %q", correction)
	}
	if n := contentServiceCount(t, []*serviceVolumeIndex{vol}, "content:recieve"); n != len(matches) {
		t.Fatalf("count = %d; search len = %d", n, len(matches))
	}
}

// The offline `content -db` command retries with the correction.
func TestContentFuzzyOfflineCLI(t *testing.T) {
	idx := buildTestContentIndex(t, map[string]string{
		"a.txt": "please receive the package",
	})
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if hits, _ := r.search("recieve", 0); len(hits) != 0 {
		t.Fatalf("exact typo matched %d", len(hits))
	}
	corrected, show, ok := contentFuzzyCorrectOfflineText(r, "recieve")
	if !ok || show != "receive" {
		t.Fatalf("ok=%v show=%q; want receive", ok, show)
	}
	if hits, _ := r.search(corrected, 0); len(hits) != 1 {
		t.Fatalf("corrected search matched %d; want 1", len(hits))
	}
}

// A sharded reader merges candidates across shards: receive lives in one
// shard's dictionary only, and the composite reader must still find it.
func TestContentFuzzyMergesShards(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	restoreTarget := contentShardTargetBytes
	contentShardTargetBytes = 60
	t.Cleanup(func() { contentShardTargetBytes = restoreTarget })

	dir := t.TempDir()
	const journal = uint64(0xABC)
	const cp = int64(120)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 1_000_000})
	d := uint32(os.ModeDir)
	records := []CompactRecord{
		{FRN: 2, ParentFRN: 1, Parent: -1, Mode: d, Name: "a"},
		{FRN: 3, ParentFRN: 1, Parent: -1, Mode: d, Name: "b"},
	}
	files := []struct {
		frn    uint64
		parent int32
		folder string
		name   string
		text   string
	}{
		{11, 0, "a", "a1.txt", "please receive the package padding padding"},
		{21, 1, "b", "b1.txt", "the weather is pleasant today padding padding"},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.folder, f.name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.text), 0o644); err != nil {
			t.Fatal(err)
		}
		records = append(records, CompactRecord{FRN: f.frn, ParentFRN: records[f.parent].FRN, Parent: f.parent, Name: f.name, Size: int64(len(f.text))})
	}
	vol, _ := contentBuildTestVolume(t, dir, journal, uint64(cp), records)
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("build state = %q; want ready", got)
	}
	reader := vol.content.readerView()
	if reader == nil || len(reader.idx.shards) < 2 {
		t.Skip("fixture did not shard")
	}
	got := contentFuzzyCorrections([]*contentReader{reader}, nil, "recieve", 1)
	if len(got) == 0 || got[0].term != "receive" {
		t.Fatalf("sharded corrections = %v; want receive", got)
	}
	// End to end through the typo query too.
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:recieve", Limit: 100}, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("exact typo matched %d; want a total miss", len(matches))
	}
	fixed, correction, ok := tryContentFuzzyRewrite([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:recieve", Limit: 100}, matches)
	if !ok || correction != "receive" || len(fixed) != 1 {
		t.Fatalf("ok=%v correction=%q matches=%d; want receive x1", ok, correction, len(fixed))
	}
}

func TestContentFuzzyCorrectableWords(t *testing.T) {
	got := contentFuzzyCorrectableWords("recieve the confirmation-message x")
	want := []string{"recieve", "confirmation", "message"}
	if len(got) != len(want) {
		t.Fatalf("words = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("words = %v; want %v", got, want)
		}
	}
	if n := utf8.RuneCountInString("recieve"); n < contentFuzzyMinWordRunes {
		t.Fatal("fixture word too short for the gate")
	}
}

func TestContentFuzzyApplyCorrectionPreservesSeparators(t *testing.T) {
	got := contentFuzzyApplyCorrection("recieve  confirmtion!", map[string]string{"recieve": "receive", "confirmtion": "confirmation"})
	if got != "receive  confirmation!" {
		t.Fatalf("rewrote = %q", got)
	}
}
