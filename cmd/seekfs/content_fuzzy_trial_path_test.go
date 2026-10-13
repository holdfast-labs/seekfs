package main

import (
	"reflect"
	"testing"
)

// Trial-path parity: a fuzzy retry evaluates its rewritten tree through the
// parsedOverride path, which must answer exactly like a normally-parsed
// search of the same query (same sets, order, snippets, counts and planner
// source). A past regression dropped the request's per-volume fields
// (contentWindow/contentProbe) on the override path, so a broad trial capped
// its posting superset at the page limit and fell back to a full-corpus
// bounded scan: identical results at ~50x the cost. The source assertions
// below pin the candidate path; the entry/count compares pin the results.

func fuzzyTrialPathFiles() []contentFixtureFile {
	return []contentFixtureFile{
		{2, "a.txt", "receive the confirmation message please receive it"},
		{3, "b.txt", "receive the archive and receive the manifest"},
		{4, "c.txt", "receive all confirmations here"},
		{5, "d.md", "the release notes describe delivery"},
	}
}

// fuzzyTrialDecoyFiles forces candidates to strictly exceed matches: the
// decoy docs carry every trigram of "receive" scattered across filler words,
// so the posting intersection admits them while verification rejects them.
// At limit 4 the trial page (2 true matches) never fills, so no widen
// re-search can mask which path the first search took.
func fuzzyTrialDecoyFiles() []contentFixtureFile {
	return []contentFixtureFile{
		{2, "a.txt", "receive the confirmation message please receive it"},
		{3, "b.txt", "receive the archive promptly"},
		{4, "c.txt", "rec ece cei eiv ive filler words around"},
		{5, "d.txt", "more rec ece cei eiv ive filler text here"},
		{6, "e.txt", "yet more rec ece cei eiv ive filler text"},
		{7, "f.md", "the release notes describe delivery"},
	}
}

// trialVsNormalSearch runs the rewritten tree through the override path the
// fuzzy trials use (page limit + completeness window, as the reduced
// default-order driver sets them) and the equivalent query string through
// the normal parse, and requires identical answers.
func trialVsNormalSearch(t *testing.T, vol *serviceVolumeIndex, query string, sub map[string]string, normalQuery string, limit, window int) {
	t.Helper()
	vols := []*serviceVolumeIndex{vol}
	base := queryOptions{Query: query, Limit: limit, Trace: &searchTrace{}}
	pq, err := parseQuery(base)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	tree := contentFuzzyRewriteTree(pq, sub)

	probeO := &contentCompletenessProbe{}
	traceO := &searchTrace{}
	overrideOpts := queryOptions{Query: query, Limit: limit, Trace: traceO, parsedOverride: tree, contentWindow: window, contentProbe: probeO}
	overrideMatches, err := searchServiceVolumes(vols, overrideOpts, false)
	if err != nil {
		t.Fatalf("override %q: %v", query, err)
	}
	probeN := &contentCompletenessProbe{}
	traceN := &searchTrace{}
	normalOpts := queryOptions{Query: normalQuery, Limit: limit, Trace: traceN, contentWindow: window, contentProbe: probeN}
	normalMatches, err := searchServiceVolumes(vols, normalOpts, false)
	if err != nil {
		t.Fatalf("normal %q: %v", normalQuery, err)
	}
	if !reflect.DeepEqual(entriesFingerprint(overrideMatches), entriesFingerprint(normalMatches)) {
		t.Fatalf("override vs normal mismatch for %q -> %q:\n override=%v\n normal=%v", query, normalQuery, entriesFingerprint(overrideMatches), entriesFingerprint(normalMatches))
	}
	if traceO.Source != traceN.Source || traceO.PlannerMode != traceN.PlannerMode {
		t.Fatalf("override vs normal planner mismatch for %q -> %q: override(src=%q planner=%q) normal(src=%q planner=%q)",
			query, normalQuery, traceO.Source, traceO.PlannerMode, traceN.Source, traceN.PlannerMode)
	}
	if traceO.ContentIncomplete != traceN.ContentIncomplete || traceO.ContentPartial != traceN.ContentPartial {
		t.Fatalf("override vs normal completeness mismatch for %q -> %q: override=%+v normal=%+v", query, normalQuery, traceO, traceN)
	}
	// Counts take the same tally path and must agree exactly.
	countO, okO, err := countServiceVolumes(vols, queryOptions{Query: query, Limit: limit, Trace: &searchTrace{}, parsedOverride: tree, contentWindow: window, contentProbe: &contentCompletenessProbe{}})
	if err != nil || !okO {
		t.Fatalf("override count %q: n=%d ok=%v err=%v", query, countO, okO, err)
	}
	countN, okN, err := countServiceVolumes(vols, queryOptions{Query: normalQuery, Limit: limit, Trace: &searchTrace{}, contentWindow: window, contentProbe: &contentCompletenessProbe{}})
	if err != nil || !okN {
		t.Fatalf("normal count %q: n=%d ok=%v err=%v", normalQuery, countN, okN, err)
	}
	if countO != countN || countO != 3 {
		t.Fatalf("count mismatch for %q -> %q: override=%d normal=%d search=%d (want 3)", query, normalQuery, countO, countN, len(normalMatches))
	}
}

func entriesFingerprint(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path+"\x00"+e.Snippet)
	}
	return out
}

func TestContentFuzzyTrialParityWithNormalParse(t *testing.T) {
	vol := newContentQueryVolume(t, fuzzyTrialPathFiles())
	// "receive" hits 3 docs; limit 2 forces the old capped-at-page-limit
	// fallback while the windowed path stays uncapped.
	trialVsNormalSearch(t, vol, "content:recieve", map[string]string{"recieve": "receive"}, "content:receive", 2, 4096)
	trialVsNormalSearch(t, vol, "ext:txt content:recieve", map[string]string{"recieve": "receive"}, "ext:txt content:receive", 2, 4096)
	trialVsNormalSearch(t, vol, "content:recieve", map[string]string{"recieve": "receive"}, "content:receive", 1, 4096)
}

// The winning trial of an end-to-end fuzzy rewrite must come from the posting
// candidate path, not a full-corpus bounded scan. The decoy fixture admits 5
// posting candidates for "receive" with only 2 true matches; at limit 4 the
// trial page never fills, so no widen re-search can mask the first search's
// path. A page-capped materialization (cap 4 < 5) would declare the superset
// capped and scan; the windowed trial must stay on candidates.
func TestContentFuzzyWinningTrialTakesCandidatePath(t *testing.T) {
	vol := newContentQueryVolume(t, fuzzyTrialDecoyFiles())
	vols := []*serviceVolumeIndex{vol}
	matches, err := searchServiceVolumes(vols, queryOptions{Query: "content:recieve", Limit: 4, Trace: &searchTrace{}}, false)
	if err != nil {
		t.Fatalf("exact: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("exact typo query matched %d docs; want a total miss", len(matches))
	}
	// A fresh trace for the rewrite: trace sources are first-write-wins,
	// so reusing the exact query's trace would mask the trial's own path.
	rewriteTrace := &searchTrace{}
	got, correction, ok := tryContentFuzzyRewrite(vols, queryOptions{Query: "content:recieve", Limit: 4, Trace: rewriteTrace}, matches)
	if !ok || correction != "receive" {
		t.Fatalf("rewrite ok=%v correction=%q; want ok=true correction=receive", ok, correction)
	}
	if len(got) != 2 {
		t.Fatalf("rewrite matches = %d; want the 2 true matches", len(got))
	}
	if rewriteTrace.Source != "content-candidates" {
		t.Fatalf("winning trial source = %q; want content-candidates", rewriteTrace.Source)
	}
	want, err := searchServiceVolumes(vols, queryOptions{Query: "content:receive", Limit: 4, Trace: &searchTrace{}}, false)
	if err != nil {
		t.Fatalf("corrected search: %v", err)
	}
	if !reflect.DeepEqual(entriesFingerprint(got), entriesFingerprint(want)) {
		t.Fatalf("rewrite hits = %v; want %v", entriesFingerprint(got), entriesFingerprint(want))
	}
}

// The override path must carry the request's per-search scalars onto the
// cloned tree: the retry driver sets them after the tree was built, and the
// candidate path reads them from the parsed query.
func TestParseQueryOverrideCarriesSearchScalars(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	base := queryOptions{Query: "content:receive", Limit: 20, Trace: &searchTrace{}}
	tree, err := parseQuery(base)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	probe := &contentCompletenessProbe{}
	trace := &searchTrace{}
	cancelled := false
	out, err := parseQuery(queryOptions{
		Query: "content:receive", Limit: 20, Trace: trace,
		DeadlineUnix: 123456789, Cancel: func() bool { return cancelled },
		ContentCandidateBudget: 1000, ContentScanVisitBudget: 2000,
		parsedOverride: &tree, contentWindow: 4096, contentProbe: probe,
	})
	if err != nil {
		t.Fatalf("override parse: %v", err)
	}
	if out.Trace != trace {
		t.Fatal("override dropped opts.Trace")
	}
	if out.DeadlineUnix != 123456789 || out.Cancel == nil || out.Cancel() != false {
		t.Fatal("override dropped deadline/cancel")
	}
	if out.ContentCandidateBudget != 1000 || out.ContentScanVisitBudget != 2000 {
		t.Fatal("override dropped budgets")
	}
	if out.contentWindow != 4096 || out.contentProbe != probe {
		t.Fatalf("override dropped contentWindow/contentProbe (window=%d)", out.contentWindow)
	}
	// Leaf fields stay with the tree.
	if len(out.Content) != 1 || out.Content[0].Text != "receive" {
		t.Fatalf("override disturbed leaves: %+v", out.Content)
	}
}

