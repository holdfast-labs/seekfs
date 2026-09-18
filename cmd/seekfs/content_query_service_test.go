package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// contentFixtureFile is one record + content document for the service tests.
type contentFixtureFile struct {
	frn  uint64
	name string
	text string
}

func contentQueryIndex(volume string, files []contentFixtureFile) *Index {
	idx := &Index{Source: "usn", Volume: volume, Compact: true}
	for _, f := range files {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       f.frn,
			ParentFRN: 1,
			Parent:    -1,
			Name:      f.name,
			Size:      10,
		})
	}
	return idx
}

// newContentQueryVolume attaches an FRN-keyed content index to a fresh
// service volume, so searchServiceVolumes exercises the full P3 path.
func newContentQueryVolume(t *testing.T, files []contentFixtureFile) *serviceVolumeIndex {
	return newContentQueryVolumeNamed(t, "C:", files)
}

func newContentQueryVolumeNamed(t *testing.T, volume string, files []contentFixtureFile) *serviceVolumeIndex {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	idx := contentQueryIndex(volume, files)
	contentIndexFRNs(idx)
	build := make([]contentBuildDoc, 0, len(files))
	for _, f := range files {
		build = append(build, contentBuildDoc{path: volume + "\\" + f.name, frn: f.frn, text: []byte(f.text)})
	}
	cidx, err := assembleContentIndex(build, t.TempDir())
	if err != nil {
		t.Fatalf("assemble content index: %v", err)
	}
	cidx.Origin = contentOriginUSN
	reader, err := openContentReader(cidx)
	if err != nil {
		t.Fatalf("open content reader: %v", err)
	}
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_"+strings.TrimSuffix(volume, ":")+".gsi"), idx)
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
	return vol
}

// newUnattachedContentVolume builds a USN volume with no content index (its
// content state is unavailable), used for the degraded multi-volume tests.
func newUnattachedContentVolume(t *testing.T, volume string, files []contentFixtureFile) *serviceVolumeIndex {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	return newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_"+strings.TrimSuffix(volume, ":")+".gsi"), contentQueryIndex(volume, files))
}

func contentServiceSearch(t *testing.T, vol *serviceVolumeIndex, query string, countOnly bool) ([]Entry, error) {
	t.Helper()
	opts := queryOptions{Query: query}
	if countOnly {
		opts.Limit = 0
	} else {
		opts.Limit = 100
	}
	return searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, countOnly)
}

func contentServiceSearchLimit(t *testing.T, vols []*serviceVolumeIndex, query string, limit int) ([]Entry, *searchTrace, error) {
	t.Helper()
	trace := &searchTrace{}
	matches, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: limit, Trace: trace}, false)
	return matches, trace, err
}

func contentServiceCount(t *testing.T, vols []*serviceVolumeIndex, query string) int {
	t.Helper()
	count, ok, err := countServiceVolumes(vols, queryOptions{Query: query, Trace: &searchTrace{}})
	if err != nil || !ok {
		t.Fatalf("countServiceVolumes(%q) = %d, %v, %v", query, count, ok, err)
	}
	return count
}

func containsIntSlice(list []int, want int) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestContentServiceTermQueryCaseInsensitive(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "notes.txt", "alpha needle beta"},
		{3, "b.md", "nothing here"},
		{4, "c.md", "Needle in markdown"},
	})
	for _, query := range []string{"content:needle", "content:NEEDLE"} {
		matches, err := contentServiceSearch(t, vol, query, false)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		got := namesOf(matches)
		want := []string{"c.md", "notes.txt"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("%s = %v; want %v", query, got, want)
		}
	}
	if matches, err := contentServiceSearch(t, vol, "content:zzznope", false); err != nil || len(matches) != 0 {
		t.Fatalf("no-match content query = %v, %v; want empty nil-error", namesOf(matches), err)
	}
}

func TestContentServicePhraseAndRegex(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.txt", "alpha needle beta"},
		{3, "b.md", "say to be or not to be"},
		{4, "c.md", "Needle in markdown"},
	})
	phrase, err := contentServiceSearch(t, vol, `content:"to be or not"`, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(phrase); len(got) != 1 || got[0] != "b.md" {
		t.Fatalf(`phrase = %v; want [b.md]`, got)
	}
	re, err := contentServiceSearch(t, vol, `content:/nee.*le/`, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(re); len(got) != 2 || got[0] != "a.txt" || got[1] != "c.md" {
		t.Fatalf(`regex = %v; want [a.txt c.md]`, got)
	}
}

func TestContentServiceOrAndNot(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.txt", "alpha only"},
		{3, "b.txt", "beta only"},
		{4, "c.txt", "gamma only"},
	})
	or, err := contentServiceSearch(t, vol, "content:alpha|content:beta", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(or); len(got) != 2 || got[0] != "a.txt" || got[1] != "b.txt" {
		t.Fatalf("OR = %v; want [a.txt b.txt]", got)
	}
	not, err := contentServiceSearch(t, vol, "content:alpha !content:beta", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(not); len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("NOT = %v; want [a.txt]", got)
	}
}

func TestContentServiceMixedIntersects(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "notes.txt", "alpha needle beta"},
		{3, "b.md", "Needle in markdown"},
		{4, "other.md", "nothing here"},
	})
	md, err := contentServiceSearch(t, vol, "ext:.md content:needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(md); len(got) != 1 || got[0] != "b.md" {
		t.Fatalf("ext+content = %v; want [b.md]", got)
	}
	named, err := contentServiceSearch(t, vol, "notes content:needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(named); len(got) != 1 || got[0] != "notes.txt" {
		t.Fatalf("term+content = %v; want [notes.txt]", got)
	}
}

func TestContentServiceDisabledAndUnavailable(t *testing.T) {
	// Enabled but no content index attached: refuse, never report empty.
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	unattached := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_c.gsi"), contentQueryIndex("C:", []contentFixtureFile{
		{2, "notes.txt", "alpha needle beta"},
	}))
	matches, err := contentServiceSearch(t, unattached, "content:needle", false)
	if err == nil || !strings.Contains(err.Error(), "content indexing unavailable") {
		t.Fatalf("unattached content query = %v, %v; want unavailable", matches, err)
	}

	// Disabled: the parser refuses the token with the disabled message.
	t.Setenv("SEEKFS_CONTENT_SEARCH", "")
	disabled := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_c.gsi"), contentQueryIndex("C:", nil))
	_, err = contentServiceSearch(t, disabled, "content:needle", false)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled content query returned %v; want disabled error", err)
	}
}

// TestContentServiceCandidatesSuperset is the consistency check: for a small
// fixture the candidate set from contentCandidates must contain every record
// the post-filtered search returns, and an empty candidate set must be a real
// empty answer (ok=true), not a fallback.
func TestContentServiceCandidatesSuperset(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.txt", "alpha needle beta"},
		{3, "b.md", "Needle in markdown"},
		{4, "c.txt", "packed trigram content"},
	})
	pq, err := parseQuery(queryOptions{Query: "content:needle"})
	if err != nil {
		t.Fatal(err)
	}
	candidates, ok := vol.contentCandidates(pq)
	if !ok {
		t.Fatal("contentCandidates declined a content-only query")
	}
	matches, err := contentServiceSearch(t, vol, "content:needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("expected two needle matches")
	}
	for _, entry := range matches {
		recIndex := contentRecordIndexForFRN(vol.index, entry.FRN)
		if recIndex < 0 || !containsIntSlice(candidates, recIndex) {
			t.Fatalf("result %s (frn %d, rec %d) not in candidates %v", entry.Name, entry.FRN, recIndex, candidates)
		}
	}

	// No postings => a real empty candidate set with ok=true.
	empty, ok := vol.contentCandidates(parseServiceQuery(t, "content:zzznope"))
	if !ok {
		t.Fatal("empty content candidate set must still be ok=true")
	}
	if len(empty) != 0 {
		t.Fatalf("empty candidate set = %v", empty)
	}
}

func TestContentServiceCountParity(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "notes.txt", "alpha needle beta"},
		{3, "b.md", "Needle in markdown"},
		{4, "c.txt", "nothing here"},
	})
	matches, err := contentServiceSearch(t, vol, "content:needle", false)
	if err != nil {
		t.Fatal(err)
	}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:needle"})
	if err != nil || !ok {
		t.Fatalf("countServiceVolumes = %d, %v, %v", count, ok, err)
	}
	if count != len(matches) {
		t.Fatalf("count %d != search %d", count, len(matches))
	}
}

// M-minor: unusable volumes are always named in the degraded set, including
// nil and empty entries that would otherwise be silently dropped.
func TestContentServiceUsableVolumesReportsNilAndEmpty(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	pq := parsedQuery{Content: []contentLeaf{{Kind: contentLeafTerm, Text: "x"}}}
	usable, skipped := contentUsableVolumes([]*serviceVolumeIndex{nil, {}}, pq)
	if len(usable) != 0 {
		t.Fatalf("usable = %v; want none", usable)
	}
	if len(skipped) != 2 || skipped[0] != "<nil>" || skipped[1] != "<unknown>" {
		t.Fatalf("skipped = %v; want [<nil> <unknown>]", skipped)
	}
}

// M-minor: a canceled content query stops before starting any volume scan.
func TestContentServiceCanceledContentQuery(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{{2, "a.txt", "needle"}})
	opts := queryOptions{Query: "content:needle", Limit: 10, Cancel: func() bool { return true }}
	if _, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false); err != errQueryCanceled {
		t.Fatalf("canceled content search err = %v; want errQueryCanceled", err)
	}
}

func parseServiceQuery(t *testing.T, query string) parsedQuery {
	t.Helper()
	pq, err := parseQuery(queryOptions{Query: query})
	if err != nil {
		t.Fatalf("parseQuery(%q): %v", query, err)
	}
	return pq
}

// B1: the user limit applies AFTER content filtering. A fixture with more
// matching records than the limit must return the limit but count all matches.
func TestContentServiceLimitAfterContentFilter(t *testing.T) {
	var files []contentFixtureFile
	// A trigram false positive for "needle": all four trigrams present but not
	// the substring. It sorts first so a pre-filter limit would spend the cap on
	// it (and it is dropped by the post-filter).
	files = append(files, contentFixtureFile{frn: 1, name: "aaa-false.txt", text: "neexxeedxxedlxxdle"})
	for i := 0; i < 6; i++ {
		files = append(files, contentFixtureFile{frn: uint64(100 + i), name: fmt.Sprintf("hit%02d.txt", i), text: "needle here"})
	}
	files = append(files, contentFixtureFile{frn: 200, name: "skip.txt", text: "nothing"})
	vol := newContentQueryVolume(t, files)

	matches, _, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:needle", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("content:needle limit=2 returned %d results; want 2 (limit after filter)", len(matches))
	}
	for _, entry := range matches {
		if text, ok := vol.contentTextForEntry(&entry); !ok || !strings.Contains(string(text), "needle") {
			t.Fatalf("content:needle returned a false positive: %s (%q)", entry.Name, text)
		}
	}
	if got := contentServiceCount(t, []*serviceVolumeIndex{vol}, "content:needle"); got != 6 {
		t.Fatalf("content:needle count = %d; want 6 (limit must not cap the count)", got)
	}

	// Negative content: 3 records survive the post-filter, 2 are excluded by
	// !content:x. The limit must apply to the survivors, not the candidates.
	neg := newContentQueryVolume(t, []contentFixtureFile{
		{10, "a.txt", "has x here"},
		{11, "b.txt", "clean one"},
		{12, "c.txt", "has x too"},
		{13, "d.txt", "clean two"},
		{14, "e.txt", "clean three"},
	})
	negMatches, _, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{neg}, "!content:x", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(negMatches) != 2 {
		t.Fatalf("!content:x limit=2 returned %d results; want 2", len(negMatches))
	}
	for _, entry := range negMatches {
		if text, ok := neg.contentTextForEntry(&entry); ok && strings.Contains(string(text), "x") {
			t.Fatalf("!content:x returned a record whose content contains x: %s", entry.Name)
		}
	}
	if got := contentServiceCount(t, []*serviceVolumeIndex{neg}, "!content:x"); got != 3 {
		t.Fatalf("!content:x count = %d; want 3 (limit must not cap the count)", got)
	}
}

// B3: a mixed OR must not lose content matches beyond the limit. The name-order
// scan used to stop at the limit before the content post-filter, dropping the
// only content matches here.
func TestContentServiceMixedOrKeepsContentMatches(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "alpha.txt", "nothing"},
		{3, "beta.txt", "nothing"},
		{4, "delta.txt", "needle present"},
		{5, "gamma.txt", "needle again"},
		{6, "zzz.txt", "nothing"},
	})
	matches, _, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:needle|zzz", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("mixed OR returned %v; want 2 results", namesOf(matches))
	}
	contentHits := 0
	for _, entry := range matches {
		if text, ok := vol.contentTextForEntry(&entry); ok && strings.Contains(string(text), "needle") {
			contentHits++
		}
	}
	if contentHits == 0 {
		t.Fatalf("mixed OR dropped every content match past the limit: %v", namesOf(matches))
	}

	// The candidate set must be a superset of the post-filtered results even
	// though contentCandidates declines a mixed group.
	pq := parseServiceQuery(t, "content:needle|zzz")
	candidates, ok := vol.nameTermCandidates(pq)
	if !ok {
		t.Fatal("nameTermCandidates declined a mixed content OR query")
	}
	for _, entry := range matches {
		recIndex := contentRecordIndexForFRN(vol.index, entry.FRN)
		if recIndex < 0 || !containsIntSlice(candidates, recIndex) {
			t.Fatalf("result %s (rec %d) is not in candidate set", entry.Name, recIndex)
		}
	}
}

// Inline verification keeps memory proportional to the limit: with the
// candidate budget lowered below the match count, the postings candidate set is
// capped and the degradation is visible, yet search still returns exactly the
// user limit.
func TestContentServiceCandidateBudgetMarksIncomplete(t *testing.T) {
	const budget = 3

	var files []contentFixtureFile
	for i := 0; i < 8; i++ {
		files = append(files, contentFixtureFile{frn: uint64(100 + i), name: fmt.Sprintf("hit%02d.txt", i), text: "needle here"})
	}
	vol := newContentQueryVolume(t, files)

	pq, err := parseQuery(queryOptions{Query: "content:needle", ContentCandidateBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	pq.Trace = &searchTrace{}
	candidates, ok := vol.nameTermCandidates(pq)
	if !ok {
		t.Fatal("nameTermCandidates declined a postings content query")
	}
	if len(candidates) > budget {
		t.Fatalf("candidate set = %d; want capped at %d", len(candidates), budget)
	}
	if !pq.Trace.ContentIncomplete {
		t.Fatal("capped candidate set did not mark the trace incomplete")
	}

	trace := &searchTrace{}
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:needle", Limit: 2, ContentCandidateBudget: budget, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("content:needle limit=2 returned %d; want exactly 2 despite the cap", len(matches))
	}
	if !trace.ContentIncomplete {
		t.Fatal("search did not surface ContentIncomplete")
	}
	s := &goSearchService{volumes: []*serviceVolumeIndex{vol}}
	if h := s.searchContentHealth(trace); h == nil || h.State != contentStateDegraded || !h.Incomplete {
		t.Fatalf("content health = %+v; want degraded incomplete", h)
	}
}

// The fallback (boundedScan) path is capped for count and refuses an inexact
// number instead of returning a silently truncated one.
func TestContentServiceFallbackScanBudgetRefusesCount(t *testing.T) {
	var files []contentFixtureFile
	for i := 0; i < 8; i++ {
		files = append(files, contentFixtureFile{frn: uint64(100 + i), name: fmt.Sprintf("clean%02d.txt", i), text: "clean text"})
	}
	vol := newContentQueryVolume(t, files)

	trace := &searchTrace{}
	_, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "!content:marker", Trace: trace, ContentCandidateBudget: 3})
	if !ok || err == nil {
		t.Fatalf("incomplete content count = ok=%v err=%v; want a refusal", ok, err)
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("count error = %v; want an incomplete-candidate error", err)
	}
	if !trace.ContentIncomplete {
		t.Fatal("trace did not record the capped fallback scan")
	}
}

// The content fallback scan has a hard visited-record budget independent of the
// match budget: past it the scan stops and marks the result incomplete (search
// returns what it found, degraded; count refuses) instead of walking the whole
// volume for a sparse-match query.
func TestContentServiceFallbackScanVisitBudget(t *testing.T) {
	const visitBudget = 3

	var files []contentFixtureFile
	for i := 0; i < 8; i++ {
		files = append(files, contentFixtureFile{frn: uint64(100 + i), name: fmt.Sprintf("clean%02d.txt", i), text: "clean text"})
	}
	vol := newContentQueryVolume(t, files)

	// Search: return the matches found before the budget, visibly incomplete.
	trace := &searchTrace{}
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "!content:marker", Limit: 100, ContentScanVisitBudget: visitBudget, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != visitBudget {
		t.Fatalf("budgeted content scan returned %d matches; want %d (visited budget)", len(matches), visitBudget)
	}
	if !trace.ContentIncomplete {
		t.Fatal("budgeted content scan did not mark the search incomplete")
	}
	if trace.Complete == nil || *trace.Complete {
		t.Fatal("budgeted content scan reported the result complete")
	}
	s := &goSearchService{volumes: []*serviceVolumeIndex{vol}}
	if h := s.searchContentHealth(trace); h == nil || h.State != contentStateDegraded || !h.Incomplete {
		t.Fatalf("content health = %+v; want degraded incomplete", h)
	}

	// Count: refuse instead of returning a partial number.
	_, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "!content:marker", Trace: &searchTrace{}, ContentScanVisitBudget: visitBudget})
	if !ok || err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("budgeted content count = ok=%v err=%v; want an incomplete refusal", ok, err)
	}
}

// The content scan's path memo is capped: at the cap it is reset rather than
// growing with the record count, so a scan's memory is O(contentScanPathCacheCap).
func TestContentScanPathCacheBounded(t *testing.T) {
	cache := make(map[int]string, contentScanPathCacheCap)
	for i := 0; i < contentScanPathCacheCap; i++ {
		cache[i] = "path"
	}
	if bounded := boundContentPathCache(cache); len(bounded) != 0 {
		t.Fatalf("cache at cap not reset: len=%d", len(bounded))
	}
	below := make(map[int]string, contentScanPathCacheCap)
	for i := 0; i < contentScanPathCacheCap-1; i++ {
		below[i] = "path"
	}
	if got := boundContentPathCache(below); len(got) != contentScanPathCacheCap-1 {
		t.Fatalf("cache below cap was reset: len=%d", len(got))
	}
}

// For search the same fallback streams: inline verification stops the candidate
// scan at the limit, so all matching candidates are never materialized.
func TestContentServiceNegatedSearchStreamsToLimit(t *testing.T) {
	var files []contentFixtureFile
	for i := 0; i < 8; i++ {
		files = append(files, contentFixtureFile{frn: uint64(100 + i), name: fmt.Sprintf("clean%02d.txt", i), text: "clean text"})
	}
	vol := newContentQueryVolume(t, files)

	pq := parseServiceQuery(t, "!content:marker")
	pq.Limit = 2
	pq.Trace = &searchTrace{}
	candidates, ok := vol.nameTermCandidates(pq)
	if !ok {
		t.Fatal("nameTermCandidates declined the negated content query")
	}
	if len(candidates) != 2 {
		t.Fatalf("streamed candidates = %d; want exactly the limit 2, not all 8", len(candidates))
	}
	if pq.Trace.ContentIncomplete {
		t.Fatal("a limit-bounded search must be complete, not incomplete")
	}

	matches, trace, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "!content:marker", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("!content:marker limit=2 returned %d; want 2", len(matches))
	}
	if trace.ContentIncomplete {
		t.Fatal("limit-bounded search reported incomplete")
	}
}

// Joint-alternative rule: a mixed OR must not be satisfied by one alternative
// supplying the content match and another supplying the name match. A flat
// evaluation would treat the content-only alternative (no name terms) as always
// true and wrongly return readme.txt.
func TestContentServiceMixedOrRequiresSameAlternative(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "readme.txt", "hello world"},
		{3, "bar.txt", "hello world"},
		{4, "plain.txt", "foo inside"},
	})
	matches, err := contentServiceSearch(t, vol, "content:foo|bar", false)
	if err != nil {
		t.Fatal(err)
	}
	got := namesOf(matches)
	want := []string{"bar.txt", "plain.txt"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("mixed OR = %v; want %v (readme.txt must not match)", got, want)
	}
}

// B2: content works across multiple volumes (per-volume search + merge), not the
// multi-volume decline error.
func TestContentServiceMultiVolumeMerge(t *testing.T) {
	volC := newContentQueryVolumeNamed(t, "C:", []contentFixtureFile{
		{2, "c-needle.txt", "needle on C"},
		{3, "c-other.txt", "nope"},
	})
	volF := newContentQueryVolumeNamed(t, "F:", []contentFixtureFile{
		{2, "f-needle.txt", "needle on F"},
		{3, "f-other.txt", "nope"},
	})
	matches, _, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{volC, volF}, "content:needle", 100)
	if err != nil {
		t.Fatalf("multi-volume content search failed: %v", err)
	}
	want := []string{"c-needle.txt", "f-needle.txt"}
	if got := namesOf(matches); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("multi-volume union = %v; want %v", got, want)
	}
	count := contentServiceCount(t, []*serviceVolumeIndex{volC, volF}, "content:needle")
	if count != len(matches) {
		t.Fatalf("multi-volume count %d != search %d", count, len(matches))
	}
}

// M1: an unusable volume degrades the result instead of bricking the whole
// service; the degraded (partial) signal is visible to the caller.
func TestContentServiceDegradedWhenSomeVolumesUnusable(t *testing.T) {
	volC := newContentQueryVolumeNamed(t, "C:", []contentFixtureFile{
		{2, "c-needle.txt", "needle on C"},
	})
	volF := newUnattachedContentVolume(t, "F:", []contentFixtureFile{
		{3, "f-needle.txt", "needle on F"},
	})
	matches, trace, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{volC, volF}, "content:needle", 100)
	if err != nil {
		t.Fatalf("degraded multi-volume content search failed: %v", err)
	}
	if got := namesOf(matches); len(got) != 1 || got[0] != "c-needle.txt" {
		t.Fatalf("degraded result = %v; want [c-needle.txt]", got)
	}
	if !trace.ContentPartial || len(trace.ContentSkippedVolumes) != 1 || trace.ContentSkippedVolumes[0] != "F:" {
		t.Fatalf("degraded signal = partial=%v skipped=%v; want F: skipped", trace.ContentPartial, trace.ContentSkippedVolumes)
	}
	s := &goSearchService{volumes: []*serviceVolumeIndex{volC, volF}}
	h := s.searchContentHealth(trace)
	if h == nil || h.State != contentStateDegraded || !h.Partial || len(h.DegradedVolumes) != 1 {
		t.Fatalf("content health = %+v; want degraded partial with F:", h)
	}
}

// M2: a delta doc whose FRN is not in the resident FRN arrays (low-memory mode)
// still maps through the persisted FRN column and is found.
func TestContentServiceDeltaInLowMemoryMode(t *testing.T) {
	files := []contentFixtureFile{
		{600, "base1.txt", "base content only"},
		{601, "base2.txt", "base content only"},
	}
	vol := newContentQueryVolume(t, files)
	// Force the low-memory shape: no resident FRN map/array.
	vol.frns = nil
	vol.frnRecordIDs = nil
	vol.frnToID = nil
	if _, ok := vol.idForFRN(600); ok {
		t.Fatal("resident FRN lookup should be unavailable in the low-memory fixture")
	}
	recIndex, ok := vol.recordIDForFRN(600)
	if !ok || recIndex != 0 {
		t.Fatalf("recordIDForFRN(600) = %d, %v; want 0, true via persisted column", recIndex, ok)
	}
	vol.content.deltaView().upsert(contentDeltaDoc{FRN: 600, Text: []byte("freshtoken"), Hash: sha256Of([]byte("freshtoken"))})

	pq := parseServiceQuery(t, "content:freshtoken")
	candidates, ok := vol.contentCandidates(pq)
	if !ok {
		t.Fatal("contentCandidates declined")
	}
	if !containsIntSlice(candidates, recIndex) {
		t.Fatalf("delta FRN 600 not in candidates %v", candidates)
	}
	matches, _, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:freshtoken", 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(matches); len(got) != 1 || got[0] != "base1.txt" {
		t.Fatalf("delta content search = %v; want [base1.txt]", got)
	}
}
