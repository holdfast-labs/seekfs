package main

import (
	"fmt"
	"os"
	"slices"
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

// The count path must stay identical too: count == len(search) for every
// non-stat shape (broad/selective/phrase/regex/OR/negation), single and multi
// volume, and the bounded candidate logic must not change the number. Counts
// never route through the page-limited bounded path (that requires !CountOnly);
// they tally in place over the full match set.
func TestContentBoundedCountMatchesSearchDifferential(t *testing.T) {
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
		"content:pelican",                  // selective: a handful
		`content:"quick brown fox"`,        // phrase
		"content:/down.*ad/",               // regex: no postings superset
		"content:download|content:pelican", // content-driven OR group
		"!content:pelican download",        // content negation
	}

	for _, scope := range scopes {
		for _, query := range queries {
			t.Run(scope.name+"/"+query, func(t *testing.T) {
				full, okFull, errFull := countServiceVolumes(scope.vols, queryOptions{Query: query, contentFullCandidates: true, Trace: &searchTrace{}})
				bounded, okBounded, errBounded := countServiceVolumes(scope.vols, queryOptions{Query: query, Trace: &searchTrace{}})
				if errFull != nil || errBounded != nil || !okFull || !okBounded {
					t.Fatalf("%q: count errors full=%v/%v bounded=%v/%v", query, okFull, errFull, okBounded, errBounded)
				}
				if full != bounded {
					t.Fatalf("%q: count full=%d bounded=%d", query, full, bounded)
				}
				matches, err := searchServiceVolumes(scope.vols, queryOptions{Query: query, Limit: 10_000}, false)
				if err != nil {
					t.Fatalf("%q: search: %v", query, err)
				}
				if len(matches) != bounded {
					t.Fatalf("%q: count=%d but len(search)=%d", query, bounded, len(matches))
				}
			})
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

// Item 9 (PB8): a bias only re-ranks, so the early stop must not change
// membership or the biased order. A biased broad query scans in biased order
// and stops once the limit is filled; contentFullCandidates selects the
// historical arm, which never takes the bias early stop and re-ranks the full
// candidate set under the same bias. Single volume: the caller applies the
// stable bias to the candidate slice, so the early stop's top-limit in biased
// order is exactly the non-early-stop result's prefix.
func TestContentBoundedBiasedDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentRecordVolume(t, "C:", contentBoundedDifferentialRecords("C:", 4_300, 1_000_000))
	vols := []*serviceVolumeIndex{vol}
	queries := []string{
		"content:download",
		"content:pelican",
		`content:"quick brown fox"`,
		"content:/down.*ad/",
	}
	sorts := []string{"", "sort:relevance", "sort:size", "sort:modified", "sort:extension", "sort:type", "sort:path"}
	biases := []string{`C:\alpha`, `C:\does-not-exist`, `C:\gamma`}
	for _, q := range queries {
		for _, s := range sorts {
			for _, bias := range biases {
				for _, limit := range []int{20, 5_000} {
					query := strings.TrimSpace(q + " " + s)
					t.Run(fmt.Sprintf("%s/%s/limit=%d", query, bias, limit), func(t *testing.T) {
						oldTrace := &searchTrace{}
						newTrace := &searchTrace{}
						oldOpts := queryOptions{Query: query, Limit: limit, RootBias: bias, contentFullCandidates: true, Trace: oldTrace}
						newOpts := queryOptions{Query: query, Limit: limit, RootBias: bias, Trace: newTrace}
						oldMatches, err := searchServiceVolumes(vols, oldOpts, false)
						if err != nil {
							t.Fatalf("full %q: %v", query, err)
						}
						newMatches, err := searchServiceVolumes(vols, newOpts, false)
						if err != nil {
							t.Fatalf("early-stop %q: %v", query, err)
						}
						assertBoundedDifferentialEqual(t, fmt.Sprintf("%s bias=%s limit=%d", query, bias, limit), oldTrace, newTrace, oldMatches, newMatches)
					})
				}
			}
		}
	}
}

// contentBiasedNestedRecords mirrors contentBoundedDifferentialRecords but adds
// a real directory record per top-level dir, so the compact path reconstruction
// that RootBias/CWDBias match against yields <volume>\<dir>\<name> instead of
// <volume>\<name>. The flat fixture is fine for shape coverage, but its records
// do not sit under any bias root, so a bias never actually reorders it.
func contentBiasedNestedRecords(volume string, baseFRN uint64, filesPerDir int) []contentVolRecord {
	dirs := []string{"alpha", "beta", "gamma", "delta"}
	exts := []string{".txt", ".md", ".go", ".nrrd"}
	recs := make([]contentVolRecord, 0, len(dirs)+len(dirs)*filesPerDir)
	dirIndex := make(map[string]int32, len(dirs))
	dirFRN := make(map[string]uint64, len(dirs))
	for _, d := range dirs {
		frn := baseFRN + uint64(len(recs)) + 1
		dirIndex[d] = int32(len(recs))
		dirFRN[d] = frn
		recs = append(recs, contentVolRecord{
			frn: frn, parent: -1, parentFRN: 1,
			name: d, mode: uint32(os.ModeDir),
			path: volume + `\` + d,
		})
	}
	for i := 0; i < len(dirs)*filesPerDir; i++ {
		d := dirs[i%len(dirs)]
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
			frn:       baseFRN + 1000 + uint64(i),
			parent:    dirIndex[d],
			parentFRN: dirFRN[d],
			name:      name,
			mode:      0,
			size:      int64((i*37)%997) + 1,
			modUnix:   int64(1_600_000_000 + (i*13)%100_000),
			path:      volume + `\` + d + `\` + name,
			content:   body.String(),
		})
	}
	return recs
}

// Item 9 blocker: the bias early stop verifies against the base record's
// Deleted flag but not the pending overlay hidden set. Hidden base records fill
// the page, the verify loop then drops every one of them, and the early stop
// cannot recover the non-hidden matches it never scanned. Hides every alpha
// file, more than a page, and uses an explicit sort so the page limit reaches
// the scan without the default-order window. The early-stop arm must equal the
// full-candidate arm byte-for-byte. Covers content (regex: no posting superset,
// so the bounded scan is the source) and filename shapes, single and multi
// volume.
func TestContentBoundedBiasedHiddenDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	volC := newContentRecordVolume(t, "C:", contentBiasedNestedRecords("C:", 1_000_000, 1_100))
	volF := newContentRecordVolume(t, "F:", contentBiasedNestedRecords("F:", 2_000_000, 1_100))
	// Hide every alpha file (i%4==0, frn = base + 1000 + i), far more than a
	// page, so a biased early stop can fill entirely with records the verify
	// loop will drop.
	hideAlpha := func(vol *serviceVolumeIndex, base uint64) {
		changes := make([]usnChange, 0, 1_100)
		for i := 0; i < 4*1_100; i += 4 {
			changes = append(changes, usnChange{FRN: base + 1000 + uint64(i), USN: int64(i) + 7, Reason: usnReasonFileDelete})
		}
		vol.applyUSNChanges(changes)
		if vol.snapshotHiddenBaseIDs().empty() {
			t.Fatal("pending deletes did not hide base records")
		}
	}
	hideAlpha(volC, 1_000_000)
	hideAlpha(volF, 2_000_000)

	scopes := []struct {
		name string
		vols []*serviceVolumeIndex
	}{
		{"single", []*serviceVolumeIndex{volC}},
		{"multi", []*serviceVolumeIndex{volC, volF}},
	}
	queries := []string{
		`content:/archive/`, // regex content: no posting superset -> bounded scan
		"content:download",  // posting superset: hidden-aware bounded cap
		"type:file",         // filename, no term -> bounded scan
		"scan",              // filename posting superset
	}
	sorts := []string{"", "sort:size", "sort:path"}
	biases := []string{`C:\alpha`, `C:\gamma`, `C:\does-not-exist`}
	for _, scope := range scopes {
		for _, query := range queries {
			for _, sort := range sorts {
				q := strings.TrimSpace(query + " " + sort)
				for _, bias := range biases {
					for _, limit := range []int{20, 5_000} {
						t.Run(fmt.Sprintf("%s/%s/%s/limit=%d", scope.name, q, bias, limit), func(t *testing.T) {
							oldTrace := &searchTrace{}
							newTrace := &searchTrace{}
							oldOpts := queryOptions{Query: q, Limit: limit, RootBias: bias, contentFullCandidates: true, Trace: oldTrace}
							newOpts := queryOptions{Query: q, Limit: limit, RootBias: bias, Trace: newTrace}
							oldMatches, err := searchServiceVolumes(scope.vols, oldOpts, false)
							if err != nil {
								t.Fatalf("full %q: %v", q, err)
							}
							newMatches, err := searchServiceVolumes(scope.vols, newOpts, false)
							if err != nil {
								t.Fatalf("early-stop %q: %v", q, err)
							}
							assertBoundedDifferentialEqual(t, fmt.Sprintf("%s %s bias=%s limit=%d", scope.name, q, bias, limit), oldTrace, newTrace, oldMatches, newMatches)
						})
					}
				}
			}
		}
	}
}

// Item 9 coverage: the biased differential above was content + single volume
// only. A bias is query-shape agnostic, so filename queries and multiple
// volumes must be byte-identical to the full-candidate arm too. Uses the nested
// fixture so a hit under the bias root actually reorders the result.
func TestContentBoundedBiasedFilenameMultiVolumeDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	volC := newContentRecordVolume(t, "C:", contentBiasedNestedRecords("C:", 1_000_000, 1_100))
	volF := newContentRecordVolume(t, "F:", contentBiasedNestedRecords("F:", 2_000_000, 1_100))
	scopes := []struct {
		name string
		vols []*serviceVolumeIndex
	}{
		{"single", []*serviceVolumeIndex{volC}},
		{"multi", []*serviceVolumeIndex{volC, volF}},
	}
	queries := []string{"scan", "scan-000001", "ext:txt", "type:file"}
	sorts := []string{"", "sort:size", "sort:path"}
	biases := []string{`C:\alpha`, `C:\gamma`, `C:\does-not-exist`}
	for _, scope := range scopes {
		for _, query := range queries {
			for _, sort := range sorts {
				q := strings.TrimSpace(query + " " + sort)
				for _, bias := range biases {
					for _, limit := range []int{20, 5_000} {
						t.Run(fmt.Sprintf("%s/%s/%s/limit=%d", scope.name, q, bias, limit), func(t *testing.T) {
							oldTrace := &searchTrace{}
							newTrace := &searchTrace{}
							oldOpts := queryOptions{Query: q, Limit: limit, RootBias: bias, contentFullCandidates: true, Trace: oldTrace}
							newOpts := queryOptions{Query: q, Limit: limit, RootBias: bias, Trace: newTrace}
							oldMatches, err := searchServiceVolumes(scope.vols, oldOpts, false)
							if err != nil {
								t.Fatalf("full %q: %v", q, err)
							}
							newMatches, err := searchServiceVolumes(scope.vols, newOpts, false)
							if err != nil {
								t.Fatalf("early-stop %q: %v", q, err)
							}
							assertBoundedDifferentialEqual(t, fmt.Sprintf("%s %s bias=%s limit=%d", scope.name, q, bias, limit), oldTrace, newTrace, oldMatches, newMatches)
						})
					}
				}
			}
		}
	}
}

// Item 9 (non-biased): the bounded-scan early stop has no overlay-hidden guard
// unless the query is biased, and scanCandidateMatches only sees the base
// Deleted flag, never the pending hidden set. Matching base records hidden by a
// pending delete therefore fill the page, the caller's verify loop drops every
// one of them, and the scan has already returned with limit candidates -- the
// result under-fills and cannot recover the non-hidden matches it never
// scanned. The first page's worth of matches in scan order are hidden, with
// non-hidden matches after them; the default arm must fill to the limit and
// equal the contentFullCandidates (non-early-stop) arm byte-for-byte, for both
// a content regex (no posting superset, so the bounded scan is the source) and
// a filename shape. Runs searchCompactWithCacheHidden directly so the global
// filename planners (which are already hidden-aware) cannot mask the per-volume
// scan this fixes.
func TestContentBoundedNonBiasedHiddenDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	const total = 200
	const hiddenTotal = 60
	const limit = 20
	mk := func(volume string, baseFRN uint64) []contentVolRecord {
		recs := make([]contentVolRecord, 0, total)
		for i := 0; i < total; i++ {
			name := fmt.Sprintf("hit-%06d.txt", i)
			recs = append(recs, contentVolRecord{
				frn:       baseFRN + uint64(i) + 2,
				parent:    -1,
				parentFRN: 1,
				name:      name,
				size:      int64(i) + 1,
				modUnix:   int64(1_600_000_000 + i),
				path:      volume + `\alpha\` + name,
				content:   "the archive worker syncs the download cache",
			})
		}
		return recs
	}
	vol := newContentRecordVolume(t, "C:", mk("C:", 1_000_000))
	changes := make([]usnChange, 0, hiddenTotal)
	for i := 0; i < hiddenTotal; i++ {
		changes = append(changes, usnChange{FRN: 1_000_000 + uint64(i) + 2, USN: int64(i) + 7, Reason: usnReasonFileDelete})
	}
	vol.applyUSNChanges(changes)
	hidden := vol.snapshotHiddenBaseIDs()
	if hidden.empty() {
		t.Fatal("pending deletes did not hide base records")
	}
	shapes := []struct {
		name       string
		query      string
		contentVol *serviceVolumeIndex
	}{
		// Explicit sort keeps the verify limit small (the default-order path
		// widens to the 4096-entry content window, which the hidden prefix would
		// never fill).
		{"content", "content:/archive/ sort:size", vol},
		{"filename", "type:file sort:size", nil},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			run := func(fullCandidates bool) ([]Entry, *searchTrace, error) {
				trace := &searchTrace{}
				opts := queryOptions{Query: shape.query, Limit: limit, Trace: trace, contentFullCandidates: fullCandidates}
				matches, err := searchCompactWithCacheHidden(vol.index, opts, false, make(map[int]string), vol.nameTermCandidates, hidden, shape.contentVol)
				return matches, trace, err
			}
			fullMatches, fullTrace, err := run(true)
			if err != nil {
				t.Fatalf("non-early-stop arm: %v", err)
			}
			gotMatches, gotTrace, err := run(false)
			if err != nil {
				t.Fatalf("default arm: %v", err)
			}
			if len(fullMatches) != limit {
				t.Fatalf("non-early-stop arm under-filled: got %d; want %d", len(fullMatches), limit)
			}
			if len(gotMatches) != limit {
				t.Fatalf("default arm under-filled: got %d; want %d", len(gotMatches), limit)
			}
			assertBoundedDifferentialEqual(t, shape.name, fullTrace, gotTrace, fullMatches, gotMatches)
		})
	}
}

// P5: the OR-group candidate union is a streaming k-way union, not a
// materialized pairwise merge. A broad OR query with a small budget must stop
// after budget+1 distinct docIDs without decoding any alternative's full posting
// list. The decode hook counts decoded postings; a materializing union would
// pull every posting of both alternatives (n).
func TestContentOrUnionStreamStopsEarly(t *testing.T) {
	const n = contentPostingBlockSize*3 + 7
	full := make([]contentDocFreq, n)
	for i := range full {
		full[i] = contentDocFreq{docID: uint32(i), tf: 1}
	}
	r := contentReaderFromGrams(t, n, map[string][]contentDocFreq{
		"abc": full, "bcd": full, "bce": full,
	})
	group := []parsedQuery{
		{Content: []contentLeaf{{Kind: contentLeafTerm, Text: "abcd", LeafID: 0}}},
		{Content: []contentLeaf{{Kind: contentLeafTerm, Text: "abce", LeafID: 1}}},
	}
	const budget = 5
	decoded := 0
	contentPostingDecodeHook = func() { decoded++ }
	defer func() { contentPostingDecodeHook = nil }()

	stream, driven, ok := contentOrGroupCandidateStream(r, group)
	if !ok || !driven || stream == nil {
		t.Fatalf("group stream driven=%v ok=%v nil=%v; want a driven stream", driven, ok, stream == nil)
	}
	got := collectDocIDStream(stream, budget+1)
	if len(got) != budget+1 {
		t.Fatalf("capped union = %v; want %d distinct docIDs", got, budget+1)
	}
	for i := range got {
		if got[i] != uint32(i) {
			t.Fatalf("union[%d] = %d; want %d (ascending distinct)", i, got[i], i)
		}
	}
	// Both alternatives are 2-gram intersections, so a full materialization
	// would decode ~4n postings; the cap must keep it O(budget).
	if decoded > 4*(budget+1)+16 {
		t.Fatalf("decoded %d postings for budget %d (n=%d); want a bounded early stop", decoded, budget, n)
	}
	if decoded >= n {
		t.Fatalf("decoded the whole %d-posting list despite the budget", n)
	}
}

// P5 differential: the streamed OR union must equal the historical
// unionSortedDocIDs merge of the fully materialized alternatives, both
// under-budget (complete) and over-budget (capped at budget+1), for a single
// alternative, multiple alternatives, multi-leaf intersections and nested
// content-driven OR groups -- the shapes the OR-in-content queries in this file
// cover.
func TestContentOrUnionStreamMatchesMaterializedUnion(t *testing.T) {
	const n = contentPostingBlockSize + 11
	docs := func(pred func(int) bool) []contentDocFreq {
		var out []contentDocFreq
		for i := 0; i < n; i++ {
			if pred(i) {
				out = append(out, contentDocFreq{docID: uint32(i), tf: 1})
			}
		}
		return out
	}
	r := contentReaderFromGrams(t, n, map[string][]contentDocFreq{
		"abc": docs(func(int) bool { return true }),
		"bcd": docs(func(i int) bool { return i%2 == 0 }),
		"cde": docs(func(i int) bool { return i%3 == 0 }),
		"def": docs(func(i int) bool { return i%5 == 0 }),
	})
	leaf := func(id int, text string) contentLeaf {
		return contentLeaf{Kind: contentLeafTerm, Text: text, LeafID: id}
	}
	groups := []struct {
		name  string
		group []parsedQuery
	}{
		{"single-alt", []parsedQuery{{Content: []contentLeaf{leaf(0, "abc")}}}},
		{"multi-alt", []parsedQuery{
			{Content: []contentLeaf{leaf(0, "abc")}},
			{Content: []contentLeaf{leaf(1, "bcd")}},
			{Content: []contentLeaf{leaf(2, "cde")}},
		}},
		{"multi-alt-intersect", []parsedQuery{
			{Content: []contentLeaf{leaf(0, "abcd")}}, // abc ∩ bcd = even
			{Content: []contentLeaf{leaf(1, "bcde")}}, // bcd ∩ cde = multiple of 6
		}},
		{"nested-or", []parsedQuery{
			{Content: []contentLeaf{leaf(0, "abcd")}, OrGroups: [][]parsedQuery{{
				{Content: []contentLeaf{leaf(1, "cde")}}, // abc ∩ bcd ∩ cde
			}}},
			{Content: []contentLeaf{leaf(2, "def")}},
		}},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			var want []uint32
			for i, alt := range g.group {
				altStream, driven, ok := contentAltCandidateStream(r, alt)
				if !ok || !driven {
					t.Fatalf("alt %d driven=%v ok=%v; want a content-driven alternative", i, driven, ok)
				}
				var altDocs []uint32
				if altStream != nil {
					altDocs = collectDocIDStream(altStream, 0)
				}
				if i == 0 {
					want = altDocs
				} else {
					want = unionSortedDocIDs(want, altDocs)
				}
			}
			underStream, driven, ok := contentOrGroupCandidateStream(r, g.group)
			if !ok || !driven || underStream == nil {
				t.Fatalf("group stream driven=%v ok=%v nil=%v", driven, ok, underStream == nil)
			}
			if got := collectDocIDStream(underStream, 0); !slices.Equal(got, want) {
				t.Fatalf("full streamed union = %v; want %v", got, want)
			}
			for _, budget := range []int{0, 1, 2, 5} {
				stream, _, _ := contentOrGroupCandidateStream(r, g.group)
				got := collectDocIDStream(stream, budget+1)
				exp := want
				if budget+1 < len(exp) {
					exp = exp[:budget+1]
				}
				if !slices.Equal(got, exp) {
					t.Fatalf("budget=%d streamed union = %v; want %v", budget, got, exp)
				}
			}
		})
	}
}

// Item 9: when the bias root holds matches, a biased broad query stops the scan
// as soon as the biased page is full, instead of scanning to the candidate
// budget. The candidate count in the trace is the scan's output size. Uses the
// nested fixture (real <volume>\<dir>\<name> paths) so the bias root actually
// has matches and the early stop is exercised, with no overlay so the guard
// does not disable it.
func TestContentBoundedBiasedEarlyStopStopsNearLimit(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newContentRecordVolume(t, "C:", contentBiasedNestedRecords("C:", 1_000_000, 1_100))
	const limit = 20
	trace := &searchTrace{}
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:download", Limit: limit, RootBias: `C:\alpha`, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != limit {
		t.Fatalf("results = %d; want %d", len(matches), limit)
	}
	if trace.Source != "content-scan" {
		t.Fatalf("source = %q; want content-scan", trace.Source)
	}
	if trace.Candidates > limit {
		t.Fatalf("biased early stop scanned %d candidates; want <= %d (the limit)", trace.Candidates, limit)
	}
}
