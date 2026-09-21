package main

import (
	"fmt"
	"strings"
	"testing"
)

// PB8 differential: the bounded content-candidate logic (bounded posting
// materialization + rank-ordered broad scan + single-volume page-limit
// verification) must return byte-identical results and order to the historical
// full-budget candidate logic for every query shape and sort column, single and
// multi volume. contentFullCandidates=true selects the old logic.
//
// The fixture is deliberately larger than contentRelevanceWindow (4096) so the
// broad path is exercised, and it carries varied sizes, modification times,
// extensions and paths so every sort column is a real ordering.

func contentBoundedDifferentialRecords(volume string, n int, baseFRN uint64) []contentVolRecord {
	dirs := []string{"alpha", "beta", "gamma", "delta"}
	exts := []string{".txt", ".md", ".go", ".nrrd"}
	recs := make([]contentVolRecord, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("scan-%06d%s", i, exts[i%4])
		var body strings.Builder
		for r := 0; r <= i%7; r++ {
			body.WriteString("the archive worker syncs the download cache to the remote volume every interval ")
		}
		if i%50 == 0 {
			body.WriteString("the quick brown fox jumps over the lazy dog ")
		}
		if i%3125 == 0 {
			body.WriteString("pelican ")
		}
		recs = append(recs, contentVolRecord{
			frn:       baseFRN + uint64(i) + 2,
			parent:    -1,
			parentFRN: 1,
			name:      name,
			mode:      0,
			size:      int64((i*37)%997) + 1,
			modUnix:   int64(1_600_000_000 + (i*13)%100_000),
			path:      volume + `\` + dirs[i%4] + `\` + name,
			content:   body.String(),
		})
	}
	return recs
}

// contentBoundedDifferentialSignature captures everything the two arms must
// agree on: the entry results in order AND the completeness metadata. The
// completeness line is first so a metadata-only regression is reported as a
// single leading element rather than a "missing result".
func contentBoundedDifferentialSignature(entries []Entry, trace *searchTrace) []string {
	out := make([]string, 0, len(entries)+1)
	out = append(out, fmt.Sprintf("complete=%v incomplete=%v partial=%v",
		traceCompleteValue(trace), traceFlag(trace, func(t *searchTrace) bool { return t.ContentIncomplete }),
		traceFlag(trace, func(t *searchTrace) bool { return t.ContentPartial })))
	for _, e := range entries {
		out = append(out, fmt.Sprintf("%s|%s|%d|%d|%d|%d|%s", e.Path, e.Name, e.Size, e.ModUnix, e.Mode, e.FRN, e.Snippet))
	}
	return out
}

func traceFlag(trace *searchTrace, pick func(*searchTrace) bool) bool {
	return trace != nil && pick(trace)
}

func traceCompleteValue(trace *searchTrace) bool {
	if trace == nil || trace.Complete == nil {
		return true
	}
	return *trace.Complete
}

func assertBoundedDifferentialEqual(t *testing.T, label string, oldTrace, newTrace *searchTrace, oldMatches, newMatches []Entry) {
	t.Helper()
	oldSig := contentBoundedDifferentialSignature(oldMatches, oldTrace)
	newSig := contentBoundedDifferentialSignature(newMatches, newTrace)
	if len(oldSig) != len(newSig) {
		t.Fatalf("%s: count diverged: old=%d new=%d\nold=%v\nnew=%v", label, len(oldSig), len(newSig), oldSig, newSig)
	}
	for i := range oldSig {
		if oldSig[i] != newSig[i] {
			t.Fatalf("%s: element %d diverged:\nold=%v\nnew=%v", label, i, oldSig, newSig)
		}
	}
}

func TestContentBoundedCandidatesDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	volC := newContentRecordVolume(t, "C:", contentBoundedDifferentialRecords("C:", 4_300, 1_000_000))
	volF := newContentRecordVolume(t, "F:", contentBoundedDifferentialRecords("F:", 4_300, 2_000_000))

	scopes := []struct {
		name string
		vols []*serviceVolumeIndex
	}{
		{"single", []*serviceVolumeIndex{volC}},
		{"multi", []*serviceVolumeIndex{volC, volF}},
	}
	queries := []string{
		"content:download",                 // broad: every document
		"content:download scan",            // broad + required name term
		"content:pelican",                  // selective: a handful
		`content:"quick brown fox"`,        // phrase
		"content:/down.*ad/",               // regex: no postings superset
		"content:download|content:pelican", // content-driven OR group
		"!content:pelican download",        // content negation
		"content:zz",                       // broad candidate stream (all docs), zero true matches
		"case:true content:download",       // case-sensitive: folded corpus still matches
		"case:true content:Download",       // case-sensitive: no match
	}
	sorts := []string{"", "sort:relevance", "sort:size", "sort:modified", "sort:extension", "sort:type", "sort:path"}

	for _, scope := range scopes {
		for _, q := range queries {
			for _, s := range sorts {
				for _, limit := range []int{20, 5_000} {
					query := strings.TrimSpace(q + " " + s)
					t.Run(fmt.Sprintf("%s/%s/limit=%d", scope.name, query, limit), func(t *testing.T) {
						oldTrace := &searchTrace{}
						newTrace := &searchTrace{}
						oldOpts := queryOptions{Query: query, Limit: limit, contentFullCandidates: true, Trace: oldTrace}
						newOpts := queryOptions{Query: query, Limit: limit, Trace: newTrace}
						oldMatches, err := searchServiceVolumes(scope.vols, oldOpts, false)
						if err != nil {
							t.Fatalf("old searchServiceVolumes(%q): %v", query, err)
						}
						newMatches, err := searchServiceVolumes(scope.vols, newOpts, false)
						if err != nil {
							t.Fatalf("new searchServiceVolumes(%q): %v", query, err)
						}
						assertBoundedDifferentialEqual(t, query, oldTrace, newTrace, oldMatches, newMatches)
					})
				}
			}
		}
	}
}

// The count path must stay identical too: count == len(search) for the
// non-stat shapes, and the bounded candidate logic must not change the number.
func TestContentBoundedCountMatchesSearchDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentRecordVolume(t, "C:", contentBoundedDifferentialRecords("C:", 4_300, 1_000_000))
	vols := []*serviceVolumeIndex{vol}

	for _, query := range []string{"content:download", "content:pelican", `content:"quick brown fox"`} {
		full, okFull, errFull := countServiceVolumes(vols, queryOptions{Query: query, contentFullCandidates: true})
		bounded, okBounded, errBounded := countServiceVolumes(vols, queryOptions{Query: query})
		if errFull != nil || errBounded != nil || !okFull || !okBounded {
			t.Fatalf("%q: count errors full=%v/%v bounded=%v/%v", query, okFull, errFull, okBounded, errBounded)
		}
		if full != bounded {
			t.Fatalf("%q: count full=%d bounded=%d", query, full, bounded)
		}
		matches, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: 10_000}, false)
		if err != nil {
			t.Fatalf("%q: search: %v", query, err)
		}
		if len(matches) != bounded {
			t.Fatalf("%q: count=%d but len(search)=%d", query, bounded, len(matches))
		}
	}
}

// Duplicate basenames across directories and volumes: the multi-volume merge
// orders name ties by path while a volume orders them by record id. A page
// limit that lands on a shared name must widen back to the window, or the
// global top-N would pick a different tie-group member than the unbounded
// search. This is the case that makes a per-volume page-limit reduction unsafe.
func TestContentBoundedMultiVolumeDuplicateNameDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	mk := func(volume string, baseFRN uint64, dirs []string) []contentVolRecord {
		recs := make([]contentVolRecord, 0, len(dirs))
		for i, dir := range dirs {
			recs = append(recs, contentVolRecord{
				frn:       baseFRN + uint64(i) + 2,
				parent:    -1,
				parentFRN: 1,
				name:      "notes.txt",
				size:      int64(i + 1),
				modUnix:   int64(1_600_000_000 + i),
				path:      volume + `\` + dir + `\notes.txt`,
				content:   "a shared needle body",
			})
		}
		return recs
	}
	volC := newContentRecordVolume(t, "C:", mk("C:", 100, []string{"zeta", "alpha", "mike", "beta"}))
	volF := newContentRecordVolume(t, "F:", mk("F:", 200, []string{"yankee", "bravo", "xray"}))
	vols := []*serviceVolumeIndex{volC, volF}

	for _, limit := range []int{1, 2, 3} {
		for _, sort := range []string{"", "sort:relevance", "sort:path"} {
			query := strings.TrimSpace("content:needle " + sort)
			oldTrace := &searchTrace{}
			newTrace := &searchTrace{}
			oldMatches, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: limit, contentFullCandidates: true, Trace: oldTrace}, false)
			if err != nil {
				t.Fatalf("old %q: %v", query, err)
			}
			newMatches, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: limit, Trace: newTrace}, false)
			if err != nil {
				t.Fatalf("new %q: %v", query, err)
			}
			assertBoundedDifferentialEqual(t, fmt.Sprintf("limit=%d %q", limit, query), oldTrace, newTrace, oldMatches, newMatches)
		}
	}
}

// Finding 2: a pending overlay hides base records, which disables the bounded
// candidate fast path. The default-order page-limit reduction must then also be
// disabled, or the window probe (len(matches) >= window) can never fire and a
// window-full query reports complete while the pre-PB8 arm reports incomplete.
func TestContentBoundedHiddenOverlayCompletenessDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentRecordVolume(t, "C:", contentBoundedDifferentialRecords("C:", 4_300, 1_000_000))
	// Tombstone the first base record (FRN 1_000_002) so the volume carries
	// hidden IDs. The broad term still matches every remaining document.
	vol.applyUSNChanges([]usnChange{{FRN: 1_000_002, USN: 7, Reason: usnReasonFileDelete}})
	if hidden := vol.snapshotHiddenBaseIDs(); hidden.empty() {
		t.Fatal("delete did not hide a base record")
	}
	vols := []*serviceVolumeIndex{vol}
	for _, limit := range []int{20, 1_000, 5_000} {
		query := "content:download"
		oldTrace := &searchTrace{}
		newTrace := &searchTrace{}
		oldMatches, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: limit, contentFullCandidates: true, Trace: oldTrace}, false)
		if err != nil {
			t.Fatalf("old %q: %v", query, err)
		}
		newMatches, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: limit, Trace: newTrace}, false)
		if err != nil {
			t.Fatalf("new %q: %v", query, err)
		}
		assertBoundedDifferentialEqual(t, fmt.Sprintf("hidden limit=%d", limit), oldTrace, newTrace, oldMatches, newMatches)
	}
}

// Finding 1: a pending overlay ADDITION leaves hidden empty, so the bounded
// candidate fast path and the default-order page-limit reduction stay enabled.
// The addition is merged in after the base page, so the merged match count
// exceeds userLimit even though the base page filled. An unknown-probe content
// query (regex: no posting superset, so neither windowCapped nor
// supersetBelowWindow is set) then must widen back to the window for the
// len(matches) >= window probe; if the widen is gated on an exact page count it
// is skipped and the query reports complete=true for a window never evaluated,
// while the pre-PB8 arm reports incomplete.
func TestContentBoundedOverlayAdditionCompletenessDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentRecordVolume(t, "C:", contentBoundedDifferentialRecords("C:", 4_300, 1_000_000))
	const addedFRN = 9_000_001
	vol.applyUSNChanges([]usnChange{{
		FRN: addedFRN, ParentFRN: 1, USN: 9_000_001,
		Reason: usnReasonFileCreate, Name: "overlay-addition.txt",
	}})
	vol.content.deltaView().upsert(contentDeltaDoc{
		FRN:  addedFRN,
		Path: `C:\alpha\overlay-addition.txt`,
		Text: []byte("the overlay addition body"),
	})
	if hidden := vol.snapshotHiddenBaseIDs(); !hidden.empty() {
		t.Fatalf("overlay addition must not hide base records, hidden=%v", hidden)
	}
	vols := []*serviceVolumeIndex{vol}
	for _, limit := range []int{20, 1_000} {
		query := `content:/the/`
		oldTrace := &searchTrace{}
		newTrace := &searchTrace{}
		oldMatches, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: limit, contentFullCandidates: true, Trace: oldTrace}, false)
		if err != nil {
			t.Fatalf("old %q limit=%d: %v", query, limit, err)
		}
		newMatches, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: limit, Trace: newTrace}, false)
		if err != nil {
			t.Fatalf("new %q limit=%d: %v", query, limit, err)
		}
		assertBoundedDifferentialEqual(t, fmt.Sprintf("overlay-addition limit=%d", limit), oldTrace, newTrace, oldMatches, newMatches)
	}
}

// Path-aware content shapes (under:/Exists) change entry construction during the
// candidate scan; the bounded arm must still match the full arm byte-for-byte,
// including completeness.
func TestContentBoundedPathShapeCompletenessDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentRecordVolume(t, "C:", contentBoundedDifferentialRecords("C:", 4_300, 1_000_000))
	vols := []*serviceVolumeIndex{vol}
	shapes := []struct {
		name   string
		mutate func(*queryOptions)
	}{
		{"under", func(o *queryOptions) { o.Under = `C:\alpha` }},
		{"under-broad", func(o *queryOptions) { o.Under = `C:\` }},
		{"exists", func(o *queryOptions) { o.Exists = true }},
		{"under-exists", func(o *queryOptions) { o.Under = `C:\beta`; o.Exists = true }},
	}
	for _, shape := range shapes {
		for _, limit := range []int{20, 5_000} {
			oldOpts := queryOptions{Query: "content:download", Limit: limit, contentFullCandidates: true, Trace: &searchTrace{}}
			newOpts := queryOptions{Query: "content:download", Limit: limit, Trace: &searchTrace{}}
			shape.mutate(&oldOpts)
			shape.mutate(&newOpts)
			oldMatches, err := searchServiceVolumes(vols, oldOpts, false)
			if err != nil {
				t.Fatalf("old %s limit=%d: %v", shape.name, limit, err)
			}
			newMatches, err := searchServiceVolumes(vols, newOpts, false)
			if err != nil {
				t.Fatalf("new %s limit=%d: %v", shape.name, limit, err)
			}
			assertBoundedDifferentialEqual(t, fmt.Sprintf("%s limit=%d", shape.name, limit), oldOpts.Trace, newOpts.Trace, oldMatches, newMatches)
		}
	}
}
