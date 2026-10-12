package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGlobalNamePlannerDefaultAcrossVolumes(t *testing.T) {
	volumes := globalNameTestVolumes(true)
	tests := []struct {
		query string
		want  []string
	}{
		{query: "nrrd", want: []string{`F:\a-scan.nrrd`, `C:\z-report.nrrd`}},
		{query: "raw", want: []string{`C:\raw-model.raw`}},
		{query: "pdf", want: []string{`C:\manual.pdf`, `F:\z-paper.pdf`}},
		{query: "model pvsm", want: []string{`F:\model-scene.pvsm`}},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			trace := &searchTrace{}
			opts := queryOptions{Query: tc.query, Limit: 20, Trace: trace}
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if paths := pathsOf(got); !slices.Equal(paths, tc.want) {
				t.Fatalf("paths = %v, want %v", paths, tc.want)
			}
			if trace.PlannerMode != "global-name" || trace.Source != "global:filename-trigram" ||
				trace.Complete == nil || !*trace.Complete || len(trace.Terms) == 0 {
				t.Fatalf("trace = %+v, want complete global filename-trigram plan", trace)
			}

			countTrace := &searchTrace{}
			count, ok, err := countServiceVolumes(volumes, queryOptions{Query: tc.query, Trace: countTrace})
			if err != nil {
				t.Fatal(err)
			}
			if !ok || count != len(tc.want) {
				t.Fatalf("count = %d ok=%v, want %d true", count, ok, len(tc.want))
			}
			if countTrace.PlannerMode != "global-count-name" || countTrace.Source != "global:filename-trigram" {
				t.Fatalf("count trace = %+v, want global-count-name", countTrace)
			}

			countMatches, err := searchServiceVolumes(volumes, queryOptions{Query: tc.query, Trace: &searchTrace{}}, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(countMatches) != count {
				t.Fatalf("count-search len = %d, count = %d", len(countMatches), count)
			}
		})
	}
}

func TestGlobalNamePlannerAppliesLimitAfterGlobalOrder(t *testing.T) {
	volumes := globalNameTestVolumes(true)
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "nrrd", Limit: 1, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(got); !slices.Equal(paths, []string{`F:\a-scan.nrrd`}) {
		t.Fatalf("paths = %v, want later-volume global best", paths)
	}
	if trace.PlannerMode != "global-name" {
		t.Fatalf("planner mode = %q, want global-name", trace.PlannerMode)
	}
}

func TestGlobalNamePlannerUsesOneOverlaySnapshot(t *testing.T) {
	volumes := globalNameTestVolumes(true)
	volumes[0].applyUSNChanges([]usnChange{{FRN: 2, USN: 10, Reason: usnReasonFileDelete}})
	volumes[1].applyUSNChanges([]usnChange{{FRN: 99, ParentFRN: 1, USN: 11, Reason: usnReasonFileCreate, Name: "000-overlay.nrrd"}})

	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "nrrd", Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`F:\000-overlay.nrrd`, `F:\a-scan.nrrd`}
	if paths := pathsOf(got); !slices.Equal(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes(volumes, queryOptions{Query: "nrrd", Trace: countTrace})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || count != len(want) {
		t.Fatalf("count = %d ok=%v, want %d true", count, ok, len(want))
	}
	if trace.PlannerMode != "global-name" || countTrace.PlannerMode != "global-count-name" {
		t.Fatalf("planner modes = %q/%q, want global name modes", trace.PlannerMode, countTrace.PlannerMode)
	}
}

func TestGlobalNamePlannerMissingTrigramFallsBackGlobally(t *testing.T) {
	volumes := globalNameTestVolumes(false)
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "nrrd", Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(got); !slices.Equal(paths, []string{`F:\a-scan.nrrd`, `C:\z-report.nrrd`}) {
		t.Fatalf("fallback paths = %v", paths)
	}
	if trace.PlannerMode != "global-bounded-scan" || trace.Fallback != "global-bounded-scan" {
		t.Fatalf("trace = %+v, want global bounded fallback", trace)
	}
	if !traceHasDecline(trace.Declines, "global-name", "trigram-not-ready") {
		t.Fatalf("declines = %+v, want global-name trigram-not-ready", trace.Declines)
	}
}

func TestGlobalNamePlannerBroadTrigramRescuedByCompleteLane(t *testing.T) {
	t.Setenv("SEEKFS_SELF_NAME_GRAMS", "1")
	t.Setenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING", "1")
	makeVolume := func(volume string) *serviceVolumeIndex {
		records := []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		}
		for i := 0; i < 8; i++ {
			records = append(records, CompactRecord{FRN: uint64(i + 2), ParentFRN: 1, Parent: 0, Name: fmt.Sprintf("plain-%02d.txt", i)})
		}
		records = append(records, CompactRecord{FRN: 20, ParentFRN: 1, Parent: 0, Name: "unrelated.bin"})
		idx := &Index{Source: "usn", Volume: volume, Roots: []string{volume + `\`}, Compact: true, Records: records}
		buildOrders(idx)
		selective := buildSelectiveNameTrigramIndex(idx, 1)
		idx.Derived.NameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(selective, nil), idx.compactRecordCount())
		extra := optionalSelfNameGramIndex(idx, selective)
		idx.Derived.SelfNameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(extra, nil), idx.compactRecordCount())
		return newServiceVolumeIndex(strings.ToLower(strings.TrimSuffix(volume, ":"))+"-broad-name.gsi", idx)
	}
	volumes := []*serviceVolumeIndex{makeVolume("C:"), makeVolume("F:")}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "plain", Limit: 3, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("results=%v, want 3 exact plain* results via the complete lane", pathsOf(got))
	}
	for _, entry := range got {
		if !strings.Contains(entry.Name, "plain") {
			t.Fatalf("rescued results leaked non-matching name %q", entry.Name)
		}
	}
	if trace.PlannerMode != "global-name" || trace.Fallback != "" {
		t.Fatalf("trace = %+v, want global-name complete-lane rescue without fallback", trace)
	}
}

func TestScopedBareFilenameQueriesUseCompleteNamePostings(t *testing.T) {
	fVol := scopedNamePostingTestVolume("F:", true)
	cVol := scopedNamePostingTestVolume("C:", true)
	volumes := []*serviceVolumeIndex{cVol, fVol}
	sorts := []string{"", "sort:path", "sort:size", "sort:modified", "sort:extension", "sort:type"}
	for _, sortTerm := range sorts {
		query := strings.TrimSpace("F: nrrd " + sortTerm)
		t.Run(query, func(t *testing.T) {
			trace := &searchTrace{}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: query, Limit: 20, Trace: trace}, false)
			if err != nil {
				t.Fatal(err)
			}
			unscoped := strings.TrimSpace("nrrd " + sortTerm)
			want, err := searchServiceVolumes([]*serviceVolumeIndex{fVol}, queryOptions{Query: unscoped, Limit: 20, Trace: &searchTrace{}}, false)
			if err != nil {
				t.Fatal(err)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("scoped paths = %v, want filename paths %v", gotPaths, wantPaths)
			}
			for _, entry := range got {
				if !strings.Contains(strings.ToLower(entry.Name), "nrrd") {
					t.Fatalf("directory-only path match leaked into filename results: %q", entry.Path)
				}
			}
			if trace.PlannerMode != "global-name" || !strings.Contains(trace.FilenameDriver, "pngc") || !slices.Equal(trace.EligibleVolumes, []string{"F:"}) {
				t.Fatalf("trace = %+v, want F-only global PNGC filename route", trace)
			}

			countTrace := &searchTrace{}
			count, ok, err := countServiceVolumes(volumes, queryOptions{Query: query, Trace: countTrace})
			if err != nil || !ok {
				t.Fatalf("count ok=%v err=%v", ok, err)
			}
			if count != len(want) {
				t.Fatalf("count = %d, want %d", count, len(want))
			}
			if countTrace.PlannerMode != "global-count-name" || !strings.Contains(countTrace.FilenameDriver, "pngc") {
				t.Fatalf("count trace = %+v, want PNGC filename count", countTrace)
			}
		})
	}
}

func TestScopedBareFilenameBooleanSemantics(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		scopedNamePostingTestVolume("C:", true),
		scopedNamePostingTestVolume("F:", true),
	}
	for _, query := range []string{
		"F: nrrd|raw",
		"F: nrrd !backup",
		"F: nrrd|raw sort:path",
		"F: nrrd !backup sort:size",
	} {
		t.Run(query, func(t *testing.T) {
			got, err := searchServiceVolumes(volumes, queryOptions{Query: query, Limit: 20, Trace: &searchTrace{}}, false)
			if err != nil {
				t.Fatal(err)
			}
			unscoped := strings.TrimSpace(strings.Replace(query, "F:", "", 1))
			want, err := searchServiceVolumes([]*serviceVolumeIndex{volumes[1]}, queryOptions{Query: unscoped, Limit: 20, Trace: &searchTrace{}}, false)
			if err != nil {
				t.Fatal(err)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("scoped paths = %v, want filename paths %v", gotPaths, wantPaths)
			}
			for _, entry := range got {
				if !strings.HasPrefix(entry.Path, `F:\`) || entry.Name == "unrelated.bin" {
					t.Fatalf("scoped boolean query leaked path-only/wrong-volume result %q", entry.Path)
				}
			}
			count, ok, err := countServiceVolumes(volumes, queryOptions{Query: query, Trace: &searchTrace{}})
			if err != nil || !ok || count != len(want) {
				t.Fatalf("count=%d ok=%v err=%v, want %d", count, ok, err, len(want))
			}
		})
	}

	explicit, err := parseQuery(queryOptions{Query: "path:F: nrrd|raw"})
	if err != nil || !explicit.MatchPath {
		t.Fatalf("explicit path boolean query lost path semantics: pq=%+v err=%v", explicit, err)
	}
}

func TestScopedBareFilenameLegacyPNGRDeclinesSafely(t *testing.T) {
	legacy := scopedNamePostingTestVolume("C:", false)
	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{legacy}, queryOptions{Query: "C: nrrd", Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("legacy PNGR fallback matches = %v, want five filename hits", pathsOf(got))
	}
	for _, entry := range got {
		if !strings.Contains(strings.ToLower(entry.Name), "nrrd") {
			t.Fatalf("legacy fallback admitted path-only match %q", entry.Path)
		}
	}
	if trace.Source == "exact-empty" {
		t.Fatalf("legacy omitted PNGR gram became false exact-empty: %+v", trace)
	}
}

func scopedNamePostingTestVolume(volume string, withPNGC bool) *serviceVolumeIndex {
	records := []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "nrrd-cache", Mode: uint32(os.ModeDir)},
		{FRN: 3, ParentFRN: 2, Parent: 1, Name: "unrelated.bin", Size: 9, ModUnix: 9},
		{FRN: 4, ParentFRN: 1, Parent: 0, Name: "a-scan.nrrd", Size: 40, ModUnix: 4},
		{FRN: 5, ParentFRN: 1, Parent: 0, Name: "b-scan.nrrd", Size: 30, ModUnix: 5},
		{FRN: 6, ParentFRN: 1, Parent: 0, Name: "backup-scan.nrrd", Size: 20, ModUnix: 6},
		{FRN: 7, ParentFRN: 1, Parent: 0, Name: "c-scan.nrrd", Size: 10, ModUnix: 7},
		{FRN: 8, ParentFRN: 1, Parent: 0, Name: "scan.raw", Size: 50, ModUnix: 8},
	}
	idx := &Index{Source: "usn", Volume: volume, Roots: []string{volume + `\`}, Compact: true, Records: records}
	buildOrders(idx)
	selective := buildSelectiveNameTrigramIndex(idx, 1)
	idx.Derived.NameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(selective, nil), idx.compactRecordCount())
	if withPNGC {
		extra := optionalSelfNameGramIndex(idx, selective)
		idx.Derived.SelfNameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(extra, nil), idx.compactRecordCount())
	}
	return newServiceVolumeIndex(strings.ToLower(strings.TrimSuffix(volume, ":"))+"-scoped-name.gsi", idx)
}

// TestMultiTermPNGCIntersection verifies that a multi-term name query whose
// grams are all common (omitted from selective PNGR, present in PNGC) is
// answered by the multi-term posting-intersection lane instead of falling to
// the bounded scan.
func TestMultiTermPNGCIntersection(t *testing.T) {
	t.Setenv("SEEKFS_SELF_NAME_GRAMS", "1")
	t.Setenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING", "1")
	vol := scopedNamePostingTestVolume("C:", true)
	// "a-scan" and "scan.raw" both share "scan"; "a-scan.nrrd" and "b-scan.nrrd"
	// both contain "scan" AND "nrrd".  Query "scan nrrd" must return only the
	// records whose names contain both substrings.
	tests := []struct {
		query string
		want  []string
	}{
		{query: "scan nrrd", want: []string{`C:\a-scan.nrrd`, `C:\b-scan.nrrd`, `C:\backup-scan.nrrd`, `C:\c-scan.nrrd`}},
		{query: "nrrd scan", want: []string{`C:\a-scan.nrrd`, `C:\b-scan.nrrd`, `C:\backup-scan.nrrd`, `C:\c-scan.nrrd`}},
		{query: "scan raw", want: []string{`C:\scan.raw`}},
		{query: "nrrd cache", want: []string{`C:\nrrd-cache`}},
		{query: "nothing match", want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			trace := &searchTrace{}
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: tc.query, Limit: 20, Trace: trace}, false)
			if err != nil {
				t.Fatal(err)
			}
			if gotPaths := pathsOf(got); !slices.Equal(gotPaths, tc.want) {
				t.Fatalf("paths = %v, want %v", gotPaths, tc.want)
			}
			if trace.FilenameDriver != "posting-intersection-pngc-multi" && !(len(tc.want) == 0 && trace.FilenameDriver == "exact-empty") {
				t.Fatalf("FilenameDriver = %q, want posting-intersection-pngc-multi (trace=%+v)", trace.FilenameDriver, trace)
			}
			if trace.PlannerMode != "global-name" || trace.Complete == nil || !*trace.Complete {
				t.Fatalf("trace = %+v, want complete global-name", trace)
			}
			countTrace := &searchTrace{}
			count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: tc.query, Trace: countTrace})
			if err != nil {
				t.Fatal(err)
			}
			if !ok || count != len(tc.want) {
				t.Fatalf("count = %d ok=%v, want %d true", count, ok, len(tc.want))
			}
		})
	}
}

// TestMultiTermPNGCIntersectionBeatsBoundedScan verifies the trace declines do
// NOT include the bounded-scan fallback and the result stays complete for the
// real two-volume scenario with a common-gram query.
func TestMultiTermPNGCIntersectionBeatsBoundedScan(t *testing.T) {
	t.Setenv("SEEKFS_SELF_NAME_GRAMS", "1")
	t.Setenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING", "1")
	cVol := scopedNamePostingTestVolume("C:", true)
	fVol := scopedNamePostingTestVolume("F:", true)
	volumes := []*serviceVolumeIndex{cVol, fVol}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "scan nrrd", Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.FilenameDriver != "posting-intersection-pngc-multi" {
		t.Fatalf("FilenameDriver = %q, want posting-intersection-pngc-multi", trace.FilenameDriver)
	}
	if trace.PlannerMode != "global-name" {
		t.Fatalf("PlannerMode = %q, want global-name", trace.PlannerMode)
	}
	for _, decline := range trace.Declines {
		if decline.Source == "global-name" && decline.Reason == "no-selective-trigram" {
			t.Fatalf("query declined global-name:no-selective-trigram: %+v", trace.Declines)
		}
	}
	for _, entry := range got {
		lower := strings.ToLower(entry.Name)
		if !strings.Contains(lower, "scan") || !strings.Contains(lower, "nrrd") {
			t.Fatalf("multi-term lane admitted non-matching result %q", entry.Path)
		}
	}
}

func globalNameTestVolumes(withTrigrams bool) []*serviceVolumeIndex {
	makeVolume := func(volume string, names []string) *serviceVolumeIndex {
		records := []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}}
		for i, name := range names {
			records = append(records, CompactRecord{FRN: uint64(i + 2), ParentFRN: 1, Parent: 0, Name: name})
		}
		idx := &Index{Source: "usn", Volume: volume, Compact: true, Records: records}
		buildOrders(idx)
		vol := newServiceVolumeIndex(volume+"-name.gsi", idx)
		if withTrigrams {
			vol.rebuildNameTrigramsLocked()
		}
		return vol
	}
	return []*serviceVolumeIndex{
		makeVolume("C:", []string{"z-report.nrrd", "raw-model.raw", "manual.pdf", "noise.txt"}),
		makeVolume("F:", []string{"a-scan.nrrd", "model-scene.pvsm", "z-paper.pdf", "other.bin"}),
	}
}

// TestGlobalNamePlannerAppliesSizeFilter covers the name+scalar shape: the
// filename trigram planner drives candidate generation and the size predicate
// is verified per record (ranked-posting lane), so a name match below the
// threshold is never returned and the excluded record never inflates counts.
func TestGlobalNamePlannerAppliesSizeFilter(t *testing.T) {
	const mb = int64(1024 * 1024)
	makeVolume := func(volume string, recs []CompactRecord) *serviceVolumeIndex {
		records := []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}}
		records = append(records, recs...)
		idx := &Index{Source: "usn", Volume: volume, Compact: true, Records: records}
		buildOrders(idx)
		vol := newServiceVolumeIndex(volume+"-sizefilter.gsi", idx)
		vol.rebuildNameTrigramsLocked()
		return vol
	}
	volumes := []*serviceVolumeIndex{
		makeVolume("C:", []CompactRecord{
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "big.pdf", Size: 200 * mb},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "small.pdf", Size: 10 * mb},
		}),
		makeVolume("F:", []CompactRecord{
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "huge.pdf", Size: 3 * 1024 * mb},
			{FRN: 5, ParentFRN: 1, Parent: 0, Name: "big.txt", Size: 500 * mb},
		}),
	}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: ".pdf size:>100mb", Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`C:\big.pdf`, `F:\huge.pdf`}
	if paths := pathsOf(got); !slices.Equal(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if trace.PlannerMode != "global-name" {
		t.Fatalf("planner mode = %q, want global-name", trace.PlannerMode)
	}
	count, ok, err := countServiceVolumes(volumes, queryOptions{Query: ".pdf size:>100mb"})
	if err != nil || !ok || count != 2 {
		t.Fatalf("count = %d ok=%v err=%v, want 2 true nil", count, ok, err)
	}
	under, err := searchServiceVolumes(volumes, queryOptions{Query: ".pdf size:<100mb", Limit: 20}, false)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(under); !slices.Equal(paths, []string{`C:\small.pdf`}) {
		t.Fatalf("under paths = %v, want [C:\\small.pdf]", paths)
	}
}

// TestGlobalNameUnderUsesNameFirstFilter covers the --under slow-path
// regression: a selective filename term scoped by Under must use the global
// filename trigram (name-first, then Under subtree intersection) instead of
// falling to the full compact-name-order scan.
func TestGlobalNameUnderUsesNameFirstFilter(t *testing.T) {
	records := []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "scope", Mode: uint32(os.ModeDir)},
		{FRN: 3, ParentFRN: 1, Parent: 0, Name: "other", Mode: uint32(os.ModeDir)},
		{FRN: 4, ParentFRN: 2, Parent: 1, Name: "run_pipeline.bat"},
		{FRN: 5, ParentFRN: 3, Parent: 2, Name: "run_pipeline.bat"},
		{FRN: 6, ParentFRN: 1, Parent: 0, Name: "unrelated.txt"},
	}
	idx := &Index{Source: "usn", Volume: "F:", Roots: []string{`F:\`}, Compact: true, Records: records}
	buildOrders(idx)
	vol := newServiceVolumeIndex("f-under-name.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	volumes := []*serviceVolumeIndex{vol}

	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "run_pipeline", Under: `F:\scope`, Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "run_pipeline.bat" || !strings.HasPrefix(got[0].Path, `F:\scope`) {
		t.Fatalf("paths = %v, want single F:\\scope run_pipeline.bat", pathsOf(got))
	}
	if trace.PlannerMode != "global-name" || trace.Source == "compact-name-order-scan" {
		t.Fatalf("trace = %+v, want global-name (not full scan)", trace)
	}
	if trace.Complete == nil || !*trace.Complete {
		t.Fatalf("trace complete = %v, want true", trace.Complete)
	}
	// Outside the scope the same name must still resolve (proves the filter,
	// not the term, narrowed the scoped query).
	ungot, err := searchServiceVolumes(volumes, queryOptions{Query: "run_pipeline", Limit: 20, Trace: &searchTrace{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ungot) != 2 {
		t.Fatalf("unscoped paths = %v, want both run_pipeline hits", pathsOf(ungot))
	}
}

// TestGlobalNameCheapPredicatesUseNameFirst covers the same slow-lane class
// as Under: selective name + cheap predicate (type:/dir:/case:/dm:/Exists)
// must use the global filename trigram (name-first, then verify) instead of
// the full bounded scan.
func cheapPredicateTestVolumes(t *testing.T) []*serviceVolumeIndex {
	t.Helper()
	now := time.Now().Add(-time.Hour).UnixNano()
	old := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	records := []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "scope", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 3, ParentFRN: 1, Parent: 0, Name: "other", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 4, ParentFRN: 2, Parent: 1, Name: "run_pipeline.bat", Mode: modeFromAttrs(fileAttributeArchive), ModUnix: now},
		{FRN: 5, ParentFRN: 3, Parent: 2, Name: "run_pipeline.bat", Mode: modeFromAttrs(fileAttributeArchive), ModUnix: old},
		{FRN: 6, ParentFRN: 1, Parent: 0, Name: "run_pipeline-cache", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 7, ParentFRN: 1, Parent: 0, Name: "unrelated.txt", Mode: modeFromAttrs(fileAttributeArchive)},
		{FRN: 8, ParentFRN: 1, Parent: 0, Name: "run_pipeline", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 9, ParentFRN: 8, Parent: 7, Name: "notes.txt"},
	}
	idx := &Index{Source: "usn", Volume: "F:", Roots: []string{`F:\`}, Compact: true, Records: records}
	buildOrders(idx)
	// Build complete gram sections (PNGR selective + PNGC) so the
	// top-ranked and posting-intersection lanes engage exactly as in
	// production; without them every test would silently exercise only
	// the fallback lanes.
	selective := buildSelectiveNameTrigramIndex(idx, 1)
	idx.Derived.NameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(selective, nil), idx.compactRecordCount())
	extra := optionalSelfNameGramIndex(idx, selective)
	idx.Derived.SelfNameTrigrams = decodeGramPostingIndex(encodeGramPostingSection(extra, nil), idx.compactRecordCount())
	vol := newServiceVolumeIndex("f-cheap-predicates.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	return []*serviceVolumeIndex{vol}
}

func TestGlobalNameCheapPredicatesUseNameFirst(t *testing.T) {
	volumes := cheapPredicateTestVolumes(t)

	tests := []struct {
		name  string
		opts  queryOptions
		want  []string
		count int
	}{
		{name: "type file", opts: queryOptions{Query: "run_pipeline type:file", Limit: 20}, want: []string{`F:\other\run_pipeline.bat`, `F:\scope\run_pipeline.bat`}, count: 2},
		{name: "type dir", opts: queryOptions{Query: "run_pipeline type:dir", Limit: 20}, want: []string{`F:\run_pipeline`, `F:\run_pipeline-cache`}, count: 2},
		{name: "case miss", opts: queryOptions{Query: "case:true RUN_PIPELINE", Limit: 20}, want: nil, count: 0},
		{name: "case hit", opts: queryOptions{Query: "case:true run_pipeline", Limit: 20}, want: []string{`F:\run_pipeline`, `F:\run_pipeline-cache`, `F:\other\run_pipeline.bat`, `F:\scope\run_pipeline.bat`}, count: 4},
		{name: "dir filter", opts: queryOptions{Query: "run_pipeline dir:scope", Limit: 20}, want: []string{`F:\scope\run_pipeline.bat`}, count: 1},
		{name: "under filter", opts: queryOptions{Query: "run_pipeline", Under: `F:\scope`, Limit: 20}, want: []string{`F:\scope\run_pipeline.bat`}, count: 1},
		{name: "dm today", opts: queryOptions{Query: "run_pipeline dm:today", Limit: 20}, want: []string{`F:\scope\run_pipeline.bat`}, count: 1},
		{name: "exists miss", opts: queryOptions{Query: "run_pipeline", Exists: true, Limit: 20}, want: nil, count: 0},
		{name: "ext hit", opts: queryOptions{Query: "run_pipeline ext:bat", Limit: 20}, want: []string{`F:\other\run_pipeline.bat`, `F:\scope\run_pipeline.bat`}, count: 2},
		{name: "ext miss", opts: queryOptions{Query: "run_pipeline ext:txt", Limit: 20}, want: nil, count: 0},
		{name: "glob hit", opts: queryOptions{Query: "run_pipeline glob:*.bat", Limit: 20}, want: []string{`F:\other\run_pipeline.bat`, `F:\scope\run_pipeline.bat`}, count: 2},
		{name: "glob miss", opts: queryOptions{Query: "run_pipeline glob:*.txt", Limit: 20}, want: nil, count: 0},
		{name: "parent filter", opts: queryOptions{Query: "run_pipeline parent:scope", Limit: 20}, want: []string{`F:\scope\run_pipeline.bat`}, count: 1},
		{name: "attrib archive", opts: queryOptions{Query: "run_pipeline attrib:A", Limit: 20}, want: []string{`F:\other\run_pipeline.bat`, `F:\scope\run_pipeline.bat`}, count: 2},
		{name: "attrib dir", opts: queryOptions{Query: "run_pipeline attrib:D", Limit: 20}, want: []string{`F:\run_pipeline`, `F:\run_pipeline-cache`}, count: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			trace := &searchTrace{}
			tc.opts.Trace = trace
			got, err := searchServiceVolumes(volumes, tc.opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if paths := pathsOf(got); !slices.Equal(paths, tc.want) {
				t.Fatalf("paths = %v, want %v", paths, tc.want)
			}
			if trace.PlannerMode != "global-name" || trace.Source == "compact-name-order-scan" ||
				strings.Contains(trace.Source, "bounded-scan") {
				t.Fatalf("trace = %+v, want global-name fast lane", trace)
			}
			if trace.Complete == nil || !*trace.Complete {
				t.Fatalf("trace complete = %v, want true", trace.Complete)
			}
			cotrace := &searchTrace{}
			copts := tc.opts
			copts.Trace = cotrace
			n, ok, err := countServiceVolumes(volumes, copts)
			if err != nil || !ok || n != tc.count {
				t.Fatalf("count = %d ok=%v err=%v, want %d true nil", n, ok, err, tc.count)
			}
		})
	}
}

// TestGlobalNameUnderSkipsTopRanked pins the truncation guard: with Limit 1
// the global top-1 name hit is F:\run_pipeline-cache (outside the scope), so
// taking the truncated top-ranked lane would verify to zero and miss the
// in-scope hit. The complete lane must be used instead.
func TestGlobalNameUnderSkipsTopRanked(t *testing.T) {
	volumes := cheapPredicateTestVolumes(t)
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "run_pipeline", Under: `F:\scope`, Limit: 1, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(got); !slices.Equal(paths, []string{`F:\scope\run_pipeline.bat`}) {
		t.Fatalf("paths = %v, want scoped hit despite global top-1 being out of scope", paths)
	}
	if trace.PlannerMode != "global-name" {
		t.Fatalf("planner mode = %q, want global-name", trace.PlannerMode)
	}
}

// TestGlobalNameUnderAbsentScopeDeclines pins fallback preservation: a scope
// missing from the index must decline the global lane (not fast-empty) so the
// single-volume path can try the filesystem-under fallback.
func TestGlobalNameUnderAbsentScopeDeclines(t *testing.T) {
	volumes := cheapPredicateTestVolumes(t)
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "run_pipeline", Under: `F:\nope`, Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("paths = %v, want empty", pathsOf(got))
	}
	if trace.PlannerMode == "global-name" {
		t.Fatalf("planner mode = global-name, want decline to single-volume (fallback preserved)")
	}
}

// TestGlobalNameUnderMultiVolume pins volume filtering: the C: volume's hit
// must not leak into an F:-scoped query.
func TestGlobalNameUnderMultiVolume(t *testing.T) {
	fvol := cheapPredicateTestVolumes(t)[0]
	crecords := []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "run_pipeline.bat", Mode: modeFromAttrs(fileAttributeArchive)},
	}
	cidx := &Index{Source: "usn", Volume: "C:", Roots: []string{`C:\`}, Compact: true, Records: crecords}
	buildOrders(cidx)
	cvol := newServiceVolumeIndex("c-under-multi.gsi", cidx)
	cvol.rebuildNameTrigramsLocked()
	volumes := []*serviceVolumeIndex{cvol, fvol}

	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "run_pipeline", Under: `F:\scope`, Limit: 20, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(got); !slices.Equal(paths, []string{`F:\scope\run_pipeline.bat`}) {
		t.Fatalf("paths = %v, want only the F: scoped hit", paths)
	}
	if trace.PlannerMode != "global-name" || !slices.Equal(trace.EligibleVolumes, []string{"F:"}) {
		t.Fatalf("trace = %+v, want global-name over F: only", trace)
	}
}

func traceHasDecline(declines []traceDecline, source, reason string) bool {
	for _, decline := range declines {
		if decline.Source == source && decline.Reason == reason {
			return true
		}
	}
	return false
}
