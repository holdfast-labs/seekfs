package main

import "testing"

// filenameFallbackVolumes builds one content-usable volume (C:) and one whose
// content index is unavailable (F:). The decoys put a content-only OR term and
// a top-level content term on F: so an inexact strip would surface them.
func filenameFallbackVolumes(t *testing.T) (usable, unusable *serviceVolumeIndex) {
	t.Helper()
	usable = newContentQueryVolumeNamed(t, "C:", []contentFixtureFile{
		{2, "c-other.txt", "has bar inside"},
		{3, "c-nope.txt", "nothing at all"},
	})
	unusable = newUnattachedContentVolume(t, "F:", []contentFixtureFile{
		{3, "f-foo.txt", "no bar here"},
		{4, "f-decoy.txt", "has bar inside"},
	})
	return usable, unusable
}

// PF-7b: a mixed OR still yields the unusable volume's filename matches while
// the usable volume answers its content matches. The content-only alternative
// must not degenerate to match-all on the unusable volume.
func TestContentFilenameFallbackMergesUnusableVolume(t *testing.T) {
	volC, volF := filenameFallbackVolumes(t)
	vols := []*serviceVolumeIndex{volC, volF}

	matches, trace, err := contentServiceSearchLimit(t, vols, "content:bar|foo", 100)
	if err != nil {
		t.Fatalf("filename fallback search failed: %v", err)
	}
	got := namesOf(matches)
	want := []string{"c-other.txt", "f-foo.txt"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("filename fallback = %v; want %v", got, want)
	}
	if !trace.ContentPartial || len(trace.ContentSkippedVolumes) != 1 || trace.ContentSkippedVolumes[0] != "F:" {
		t.Fatalf("degraded signal = partial=%v skipped=%v; want F: skipped", trace.ContentPartial, trace.ContentSkippedVolumes)
	}
	if count := contentServiceCount(t, vols, "content:bar|foo"); count != len(matches) {
		t.Fatalf("filename fallback count %d != search %d", count, len(matches))
	}
}

// PF-7b: a query that is not filename-answerable (a required top-level content
// leaf, or a content negation) skips the unusable volume rather than return its
// unverified filename matches.
func TestContentFilenameFallbackSkipsUnanswerable(t *testing.T) {
	volC := newContentQueryVolumeNamed(t, "C:", []contentFixtureFile{
		{2, "c-foo.txt", "has bar inside"},
	})
	volF := newUnattachedContentVolume(t, "F:", []contentFixtureFile{
		{3, "f-foo.txt", "has bar inside"},
	})
	vols := []*serviceVolumeIndex{volC, volF}

	matches, trace, err := contentServiceSearchLimit(t, vols, "foo content:bar", 100)
	if err != nil {
		t.Fatalf("top-level content search failed: %v", err)
	}
	if got := namesOf(matches); len(got) != 1 || got[0] != "c-foo.txt" {
		t.Fatalf("top-level content = %v; want [c-foo.txt] (F: must be skipped)", got)
	}
	if !trace.ContentPartial || len(trace.ContentSkippedVolumes) != 1 || trace.ContentSkippedVolumes[0] != "F:" {
		t.Fatalf("degraded signal = partial=%v skipped=%v; want F: skipped", trace.ContentPartial, trace.ContentSkippedVolumes)
	}

	negC := newContentQueryVolumeNamed(t, "C:", []contentFixtureFile{
		{10, "c-clean.txt", "no marker"},
		{11, "c-hit.txt", "has x here"},
	})
	negF := newUnattachedContentVolume(t, "F:", []contentFixtureFile{
		{12, "f-clean.txt", "no marker"},
	})
	neg, negTrace, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{negC, negF}, "!content:x", 100)
	if err != nil {
		t.Fatalf("negated content search failed: %v", err)
	}
	if got := namesOf(neg); len(got) != 1 || got[0] != "c-clean.txt" {
		t.Fatalf("!content:x = %v; want [c-clean.txt] (F: must be skipped)", got)
	}
	if !negTrace.ContentPartial || len(negTrace.ContentSkippedVolumes) != 1 || negTrace.ContentSkippedVolumes[0] != "F:" {
		t.Fatalf("!content:x degraded = partial=%v skipped=%v; want F: skipped", negTrace.ContentPartial, negTrace.ContentSkippedVolumes)
	}
}

// PF-7b: stripping the content-only OR alternative must not turn it into
// match-all. On the unusable volume alone, only the filename answerable
// alternative may match, and count must agree.
func TestContentFilenameFallbackNoFalsePositiveForContentAlternative(t *testing.T) {
	volF := newUnattachedContentVolume(t, "F:", []contentFixtureFile{
		{3, "f-foo.txt", "nothing"},
		{4, "f-decoy.txt", "has bar inside"},
	})
	vols := []*serviceVolumeIndex{volF}

	matches, trace, err := contentServiceSearchLimit(t, vols, "content:bar|foo", 100)
	if err != nil {
		t.Fatalf("filename-only search failed: %v", err)
	}
	if got := namesOf(matches); len(got) != 1 || got[0] != "f-foo.txt" {
		t.Fatalf("filename-only strip = %v; want [f-foo.txt] (content:bar must be false)", got)
	}
	if !trace.ContentPartial || len(trace.ContentSkippedVolumes) != 1 || trace.ContentSkippedVolumes[0] != "F:" {
		t.Fatalf("filename-only degraded = partial=%v skipped=%v; want F: skipped", trace.ContentPartial, trace.ContentSkippedVolumes)
	}
	if count := contentServiceCount(t, vols, "content:bar|foo"); count != len(matches) {
		t.Fatalf("filename-only count %d != search %d", count, len(matches))
	}
}

// PF-7b: a query with no content leaf never enters the content fallback.
func TestContentFilenameFallbackPlainQueryUnaffected(t *testing.T) {
	volF := newUnattachedContentVolume(t, "F:", []contentFixtureFile{
		{3, "f-foo.txt", "nothing"},
	})
	trace := &searchTrace{}
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{volF}, queryOptions{Query: "foo", Limit: 100, Trace: trace}, false)
	if err != nil {
		t.Fatalf("plain filename search failed: %v", err)
	}
	if got := namesOf(matches); len(got) != 1 || got[0] != "f-foo.txt" {
		t.Fatalf("plain filename query = %v; want [f-foo.txt]", got)
	}
	if trace.ContentPartial || len(trace.ContentSkippedVolumes) != 0 {
		t.Fatalf("plain query marked content-partial: %+v", trace)
	}
}

// M6 review: an empty stream set must yield nothing, not a closure that repeats
// (0, true) forever.
func TestIntersectDocIDStreamsEmptyYieldsNothing(t *testing.T) {
	next := intersectDocIDStreams(nil)
	if id, ok := next(); ok {
		t.Fatalf("empty intersection yielded (%d, true); want (0, false)", id)
	}
}
