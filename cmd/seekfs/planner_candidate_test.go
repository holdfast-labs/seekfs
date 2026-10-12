package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPlannedCandidatesMatchFullSearchForStructuralFilters(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "src ext:go", MatchPath: true, Limit: 20},
		{Query: "dir:src ext:go", MatchPath: true, Limit: 20},
		{Query: "glob:*test*.go", MatchPath: true, Limit: 20},
		{Query: `regex:Assets.*\.dat$`, MatchPath: true, Under: `C:\fixture\workspace`, Limit: 20},
		{Query: "type:file glob:*.go", MatchPath: true, Under: `C:\fixture\workspace`, Limit: 20},
	}
	for _, opts := range cases {
		t.Run(opts.Query, func(t *testing.T) {
			pq, err := parseQuery(opts)
			if err != nil {
				t.Fatal(err)
			}
			pq.Limit = normalizedLimit(opts.Limit, false)
			got, ok := vol.plannedCandidates(pq)
			if !ok {
				t.Fatal("plannedCandidates declined query")
			}
			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, func(parsedQuery) ([]int, bool) {
				return got, true
			})
			if err != nil {
				t.Fatalf("planned search: %v", err)
			}
			if !sameStringSet(namesOf(fast), namesOf(full)) {
				t.Fatalf("planned names = %v, full names = %v", namesOf(fast), namesOf(full))
			}
		})
	}
}

func TestNilCandidateProviderMeansEmptyCandidateSet(t *testing.T) {
	idx := commonSearchFixture()
	got, err := searchCompactWithCache(idx, queryOptions{Query: "main", Limit: 20}, false, make(map[int]string), func(parsedQuery) ([]int, bool) {
		return nil, true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("nil candidate provider returned %d matches, want 0", len(got))
	}
}

func TestExactTopCandidatesFilterPathTerms(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "src ext:go", MatchPath: true, Limit: 2}
	pq := mustParseQuery(t, opts)
	pq.Limit = normalizedLimit(opts.Limit, false)

	if candidates, ok := vol.exactTopPlannedCandidates(pq); ok {
		t.Fatalf("exactTopPlannedCandidates returned %d candidates for ext + path term query, want decline", len(candidates))
	}
	candidates, ok := vol.plannedCandidates(pq)
	if !ok {
		t.Fatal("plannedCandidates declined ext + path term query")
	}
	fast := entriesForIDs(idx, candidates)
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("exact top ext+path candidates = %v, full = %v", got, want)
	}
}

func TestPlannedCountMatchesFullSearchCount(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "type:file ext:go", MatchPath: true},
		{Query: "dir:src ext:go", MatchPath: true},
		{Query: "glob:*test*.go", MatchPath: true},
		{Query: "ext:dat", MatchPath: true, Under: `C:\fixture\workspace\Assets`},
		{Query: `regex:Assets.*\.(dat|txt)$`, MatchPath: true, Under: `C:\fixture\workspace`},
	}
	for _, opts := range cases {
		t.Run(opts.Query, func(t *testing.T) {
			pq, err := parseQuery(opts)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := vol.plannedCount(pq)
			if !ok {
				t.Fatal("plannedCount declined query")
			}
			full, err := searchCompactWithCache(idx, opts, true, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full count search: %v", err)
			}
			if got != len(full) {
				t.Fatalf("planned count = %d, full count = %d", got, len(full))
			}
		})
	}
}

func TestCandidatePlanUsesCheapestUnderOrPostingSource(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "type:file ext:go", MatchPath: true, Under: `C:\fixture\workspace`})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined query")
	}
	if len(plan.sources) == 0 || plan.sources[0].name != "ext:go" {
		t.Fatalf("plan sources = %+v, want extension posting before subtree materialization", plan.sources)
	}

	pq, err = parseQuery(queryOptions{Query: "type:file", MatchPath: true, Under: `C:\fixture\workspace\src`})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok = vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined under-only query")
	}
	if len(plan.sources) == 0 || plan.sources[0].name != "under" {
		t.Fatalf("plan sources = %+v, want under source for unposted subtree query", plan.sources)
	}
	if len(plan.sources[0].ids) != 0 || len(plan.sources[0].roots) == 0 {
		t.Fatalf("under source = %+v, want lazy roots without materialized ids", plan.sources[0])
	}
	full, err := searchCompactWithCache(idx, queryOptions{Query: "type:file", MatchPath: true, Under: `C:\fixture\workspace\src`}, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := searchCompactWithCache(idx, queryOptions{Query: "type:file", MatchPath: true, Under: `C:\fixture\workspace\src`}, false, make(map[int]string), func(parsedQuery) ([]int, bool) {
		return plan.execute(), true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sameStringSet(pathsOf(fast), pathsOf(full)) {
		t.Fatalf("lazy under paths = %v, full paths = %v", pathsOf(fast), pathsOf(full))
	}
}

// TestCandidatePlanMultiTermIncludesBoundedTermSources reproduces the R5
// performance gap where a loose multi-term path query like
// `Dataset trainingdata nrrd` promoted `nrrd` to an extension and drove the
// whole plan off the (huge) extension posting, verifying every other term
// against it.  The plan must instead add bounded sources for the remaining
// terms so the intersection drives off the smallest (a zero-match term makes
// the plan empty immediately).
func TestCandidatePlanMultiTermIncludesBoundedTermSources(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) int32 {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      1024,
			ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
		return int32(len(idx.Records) - 1)
	}
	root := add(1, 1, -1, ".", uint32(os.ModeDir))
	dataset := add(2, 1, root, "Dataset", uint32(os.ModeDir))
	datasetFRN := idx.Records[dataset].FRN
	notes := add(3, 2, dataset, "notes.txt", 0)
	_ = notes
	for i := 0; i < 40; i++ {
		add(100+uint64(i), datasetFRN, dataset, fmt.Sprintf("scan-%02d.nrrd", i), 0)
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	pq, err := parseQuery(queryOptions{Query: "Dataset notes nrrd", MatchPath: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !pq.MatchPath {
		t.Fatal("parseQuery lost MatchPath")
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined multi-term path query")
	}
	sawTerm := false
	for _, source := range plan.sources {
		if strings.HasPrefix(source.name, "term:") || strings.HasPrefix(source.name, "path-term:") {
			sawTerm = true
		}
	}
	if !sawTerm {
		t.Fatalf("plan sources = %+v, want a bounded term source alongside the extension posting", plan.sources)
	}
	// A zero-match required term must make the plan empty rather than
	// materialize the extension posting and verify every candidate against it.
	pqZero, err := parseQuery(queryOptions{Query: "Dataset zzz-missing-keyword nrrd", MatchPath: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	planZero, okZero := vol.buildCandidatePlan(pqZero)
	if !okZero {
		t.Fatal("buildCandidatePlan declined multi-term query with a zero-match term")
	}
	if !planZero.empty {
		t.Fatalf("plan sources = %+v, want empty plan because a required term has zero matches", planZero.sources)
	}
	if got := planZero.execute(); len(got) != 0 {
		t.Fatalf("empty plan executed to %v, want zero candidates", got)
	}
}

func TestCandidatePlanUsesNameTermBeforeUnderSubtree(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "main.go", Under: `C:\fixture\workspace`, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined query")
	}
	if len(plan.sources) == 0 {
		t.Fatal("buildCandidatePlan returned no sources")
	}
	for _, source := range plan.sources {
		if source.name == "under" {
			t.Fatalf("plan sources = %+v, should filter selective filename candidates by --under instead of materializing subtree", plan.sources)
		}
	}
	if plan.sources[0].name != "term:main.go" {
		t.Fatalf("first source = %q, want term:main.go", plan.sources[0].name)
	}
}

func TestCandidatePlanUsesExactKnownFileUnderRepo(t *testing.T) {
	idx := commonSearchFixture()
	idx.Records = append(idx.Records, CompactRecord{
		FRN:       17,
		ParentFRN: 3,
		Parent:    2,
		Name:      ".seekfs-agent-log.jsonl",
	})
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: ".seekfs-agent-log.jsonl", Under: `C:\fixture\workspace`, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined query")
	}
	got := plan.execute()
	if len(got) != 1 || got[0] != 16 {
		t.Fatalf("candidate ids = %v, want exact agent log file only", got)
	}
}

func TestPathDottedExtensionTermUsesExtensionPosting(t *testing.T) {
	idx := commonSearchFixture()
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 17, ParentFRN: 3, Parent: 2, Name: "Reports", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 18, ParentFRN: 17, Parent: 16, Name: "annual-report.docx"},
		CompactRecord{FRN: 19, ParentFRN: 17, Parent: 16, Name: "notes.txt"},
	)
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "Reports .docx", MatchPath: true, Limit: 20}
	pq, err := parseQuery(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(pq.Exts) != 1 || pq.Exts[0] != "docx" || len(pq.Terms) != 1 || pq.Terms[0] != "reports" {
		t.Fatalf("parsed query terms=%v exts=%v, want reports + docx extension", pq.Terms, pq.Exts)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined query")
	}
	if len(plan.sources) == 0 || plan.sources[0].name != "ext:docx" {
		t.Fatalf("plan sources = %+v, want ext:docx source", plan.sources)
	}
	got, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(got); len(names) != 1 || names[0] != "annual-report.docx" {
		t.Fatalf("matches = %v, want annual-report.docx", names)
	}
}

func TestPathOnlyDottedExtensionUsesExtensionCandidate(t *testing.T) {
	idx := commonSearchFixture()
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 17, ParentFRN: 3, Parent: 2, Name: "Downloads", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 18, ParentFRN: 17, Parent: 16, Name: "scan.nrrd"},
		CompactRecord{FRN: 19, ParentFRN: 17, Parent: 16, Name: "notes.txt"},
		CompactRecord{FRN: 20, ParentFRN: 3, Parent: 2, Name: "data.nrrd", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 21, ParentFRN: 20, Parent: 19, Name: "metadata.json"},
	)
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	pathPQ, err := parseQuery(queryOptions{Query: "path:.nrrd", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	extPQ, err := parseQuery(queryOptions{Query: "ext:.nrrd", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	pathPlan, ok := vol.buildCandidatePlan(pathPQ)
	if !ok || len(pathPlan.sources) == 0 {
		t.Fatalf("path:.nrrd plan = %+v ok=%v, want extension candidate source", pathPlan.sources, ok)
	}
	if got, want := pathPlan.sources[0].name, "ext:nrrd"; got != want {
		t.Fatalf("path:.nrrd source = %q, want %q", got, want)
	}
	extPlan, ok := vol.buildCandidatePlan(extPQ)
	if !ok || len(extPlan.sources) == 0 {
		t.Fatalf("ext:.nrrd plan = %+v ok=%v, want extension candidate source", extPlan.sources, ok)
	}
	if got, want := extPlan.sources[0].name, "ext:nrrd"; got != want {
		t.Fatalf("ext:.nrrd source = %q, want %q", got, want)
	}
	if got, want := len(pathPlan.execute()), len(extPlan.execute()); got != want {
		t.Fatalf("candidate counts path:.nrrd=%d ext:.nrrd=%d, want same exact-extension set", got, want)
	}
	pathMatches, err := searchCompactWithCache(idx, queryOptions{Query: "path:.nrrd", Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(pathMatches); !sameStringSet(names, []string{"data.nrrd", "scan.nrrd"}) {
		t.Fatalf("path:.nrrd names = %v, want extension matches only", names)
	}
	extMatches, err := searchCompactWithCache(idx, queryOptions{Query: "ext:.nrrd", Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(extMatches); !sameStringSet(names, []string{"data.nrrd", "scan.nrrd"}) {
		t.Fatalf("ext:.nrrd names = %v, want extension matches only", names)
	}
}

func TestPathModeDottedExtensionNarrowsSubstringSemantics(t *testing.T) {
	idx := commonSearchFixture()
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 17, ParentFRN: 3, Parent: 2, Name: "Downloads", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 18, ParentFRN: 17, Parent: 16, Name: "scan.nrrd"},
		CompactRecord{FRN: 19, ParentFRN: 17, Parent: 16, Name: "backup.nrrd.bak"},
		CompactRecord{FRN: 20, ParentFRN: 3, Parent: 2, Name: "data.nrrd", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 21, ParentFRN: 20, Parent: 19, Name: "metadata.json"},
	)
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	pathMode, err := searchCompactWithCache(idx, queryOptions{Query: "path:.nrrd", Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(pathMode); !sameStringSet(names, []string{"scan.nrrd", "data.nrrd"}) {
		t.Fatalf("path:.nrrd names = %v, want exact extension matches", names)
	}
	nameMode, err := searchCompactWithCache(idx, queryOptions{Query: ".nrrd", Limit: 20}, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(nameMode); !sameStringSet(names, []string{"scan.nrrd", "backup.nrrd.bak", "data.nrrd"}) {
		t.Fatalf("name-mode .nrrd names = %v, want substring matches", names)
	}
}

// TestCompactScanPathCacheBounded pins the path-memo bound on the compact
// scan loop: past contentScanPathCacheCap visited paths the memo resets
// instead of growing with the record count, while results stay complete.
func TestCompactScanPathCacheBounded(t *testing.T) {
	const files = contentScanPathCacheCap + 1000
	records := []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}}
	for i := 0; i < files; i++ {
		records = append(records, CompactRecord{
			FRN: uint64(2 + i), ParentFRN: 1, Parent: 0,
			Name: fmt.Sprintf("file-%05d.txt", i),
		})
	}
	idx := &Index{Source: "usn", Volume: "F:", Compact: true, Records: records}
	buildOrders(idx)
	pathCache := make(map[int]string)
	got, err := searchCompactWithCacheHidden(idx, queryOptions{Query: "type:file", Limit: files}, false, pathCache, nil, hiddenBaseIDs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != files {
		t.Fatalf("results = %d, want %d complete matches", len(got), files)
	}
	if len(pathCache) > contentScanPathCacheCap {
		t.Fatalf("path memo = %d entries, want bounded by %d", len(pathCache), contentScanPathCacheCap)
	}
}

func TestSinglePathTermCandidateCanSkipEntryMatches(t *testing.T) {
	pq := mustParseQuery(t, queryOptions{Query: "path:.opencode", Limit: 20})
	pq.Limit = normalizedLimit(20, false)
	if !compactCandidateCanSkipEntryMatches(pq, true) {
		t.Fatal("single path-term candidate query should skip redundant entryMatches")
	}
	if compactCandidateCanSkipEntryMatches(pq, false) {
		t.Fatal("non-candidate query skipped entryMatches")
	}
	withFilter := mustParseQuery(t, queryOptions{Query: "path:.opencode ext:json", Limit: 20})
	withFilter.Limit = normalizedLimit(20, false)
	if compactCandidateCanSkipEntryMatches(withFilter, true) {
		t.Fatal("candidate query with extra extension filter skipped entryMatches")
	}
	multiTerm := mustParseQuery(t, queryOptions{Query: "path:Downloads .nrrd", Limit: 20})
	multiTerm.Limit = normalizedLimit(20, false)
	if compactCandidateCanSkipEntryMatches(multiTerm, true) {
		t.Fatal("multi-term path query skipped entryMatches")
	}
	withFeature := mustParseQuery(t, queryOptions{Query: "path:.opencode", Limit: 20})
	withFeature.Limit = normalizedLimit(20, false)
	withFeature.Features = append(withFeature.Features, featureLeaf{Name: "companion", Text: "needle"})
	if compactCandidateCanSkipEntryMatches(withFeature, true) {
		t.Fatal("candidate query with feature leaf skipped entryMatches")
	}
}

// TestCaseSensitivePathQueryStaysExact pins the S2 routing invariant: no
// candidate lane may serve an unverified case-insensitive superset to a
// case-sensitive query (compactPathContainsTerm folds case, and the skip
// path would then admit wrong-case hits). Every such lane declines today,
// leaving the fully-verifying bounded scan; this test fails if any future
// lane serves sensitive queries from insensitive postings.
func TestCaseSensitivePathQueryStaysExact(t *testing.T) {
	records := []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "Run_Me.txt", Mode: modeFromAttrs(fileAttributeArchive)},
		{FRN: 3, ParentFRN: 1, Parent: 0, Name: "run_me.txt", Mode: modeFromAttrs(fileAttributeArchive)},
		{FRN: 4, ParentFRN: 1, Parent: 0, Name: "RUN_ME.txt", Mode: modeFromAttrs(fileAttributeArchive)},
	}
	idx := &Index{Source: "usn", Volume: "F:", Roots: []string{`F:\`}, Compact: true, Records: records}
	buildOrders(idx)
	vol := newServiceVolumeIndex("case-sensitive-pin.gsi", idx)
	volumes := []*serviceVolumeIndex{vol}
	for _, tc := range []struct {
		name      string
		query     string
		path      bool
		want      []string
		skipCount bool
	}{
		// Count declines case-sensitive queries entirely (no count lane
		// serves them; pre-existing gap shared with sub-3-char path
		// counts) — search correctness is what this test pins.
		{name: "name", query: "case:true Run_Me", want: []string{`F:\Run_Me.txt`}, skipCount: true},
		{name: "path", query: "case:true Run_Me", want: []string{`F:\Run_Me.txt`}, skipCount: true},
		{name: "insensitive control", query: "run_me", want: []string{`F:\Run_Me.txt`, `F:\run_me.txt`, `F:\RUN_ME.txt`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matchPath := tc.name == "path"
			got, err := searchServiceVolumes(volumes, queryOptions{Query: tc.query, MatchPath: matchPath, Limit: 20}, false)
			if err != nil {
				t.Fatal(err)
			}
			if paths := pathsOf(got); !slices.Equal(paths, tc.want) {
				t.Fatalf("paths = %v, want %v", paths, tc.want)
			}
			if tc.skipCount {
				return
			}
			n, ok, err := countServiceVolumes(volumes, queryOptions{Query: tc.query, MatchPath: matchPath})
			if err != nil || !ok || n != len(tc.want) {
				t.Fatalf("count = %d ok=%v err=%v, want %d true nil", n, ok, err, len(tc.want))
			}
		})
	}
}

func TestDottedExtensionMultiPathUsesExtensionPosting(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "Downloads .nrrd", MatchPath: true, Limit: 20}
	trace := &searchTrace{}
	fast, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, MatchPath: opts.MatchPath, Limit: opts.Limit, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Source != "planned:ext:nrrd" && trace.Source != "path-directory-term-top" &&
		trace.Source != "planned:ext:nrrd+path-term:downloads" && trace.Source != "planned:empty" {
		t.Fatalf("source = %q, want planned:ext:nrrd, path-directory-term-top, or the bounded plan route", trace.Source)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestBareExtensionMultiPathUsesExtensionPosting(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "Downloads nrrd", MatchPath: true, Limit: 20}
	trace := &searchTrace{}
	fast, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, MatchPath: opts.MatchPath, Limit: opts.Limit, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Source != "path-bare-extension-multi-top" && trace.Source != "path-directory-term-top" &&
		trace.Source != "planned:ext:nrrd+path-term:downloads" && trace.Source != "planned:empty" {
		t.Fatalf("source = %q, want path-bare-extension-multi-top, path-directory-term-top, or the bounded plan route", trace.Source)
	}
	dotted, err := searchCompactWithCache(idx, queryOptions{Query: "Downloads .nrrd", MatchPath: true, Limit: 20}, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(dotted); !sameOrderedStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestOverlayAwareCandidatesKeepNonEmptyBaseAndDeclineEmpty(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	hit := mustParseQuery(t, queryOptions{Query: "Downloads .nrrd", MatchPath: true, Limit: 20})
	hit.Limit = normalizedLimit(20, false)
	candidates, ok := vol.overlayAwareNameTermCandidates(hit)
	if !ok || len(candidates) == 0 {
		t.Fatalf("overlayAwareNameTermCandidates hit = %d ok=%v, want non-empty candidates", len(candidates), ok)
	}

	miss := mustParseQuery(t, queryOptions{Query: "Downloads missing-nrrd-token", MatchPath: true, Limit: 20})
	miss.Limit = normalizedLimit(20, false)
	if candidates, ok := vol.overlayAwareNameTermCandidates(miss); ok {
		t.Fatalf("overlayAwareNameTermCandidates miss = %d ok=true, want decline", len(candidates))
	}
}

func TestMultiTermEmptyPathDeclinesDirectoryComponentQueries(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq := mustParseQuery(t, queryOptions{Query: "Downloads nrrd", MatchPath: true, Limit: 20})
	pq.Limit = normalizedLimit(20, false)
	if candidates, ok := vol.multiTermEmptyPathCandidates(pq); ok {
		t.Fatalf("multiTermEmptyPathCandidates returned %d candidates, want decline", len(candidates))
	}
}

func TestLimitedDottedPathScanDeclinesUnderfilledResults(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "path:ai.opencode.desktop", Limit: 20}
	pq := mustParseQuery(t, opts)
	pq.Limit = normalizedLimit(opts.Limit, false)
	if candidates, ok := vol.limitedDottedPathScanCandidates(pq); ok {
		t.Fatalf("limitedDottedPathScanCandidates returned %d underfilled candidates, want decline", len(candidates))
	}
	filtered := mustParseQuery(t, queryOptions{Query: "path:ai.opencode.desktop ext:json", Limit: 20})
	filtered.Limit = normalizedLimit(20, false)
	if _, ok := vol.limitedDottedPathScanCandidates(filtered); ok {
		t.Fatal("limited dotted path scan accepted filtered query")
	}
}

func TestSearchTraceReportsCandidateSource(t *testing.T) {
	idx := dottedPathBenchmarkIndex(1200)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		name       string
		opts       queryOptions
		wantSource string
		wantTerms  []traceTerm
	}{
		{
			name:       "dotted path extension",
			opts:       queryOptions{Query: "path:.nrrd", Limit: 20},
			wantSource: "planned:ext-top",
			wantTerms:  []traceTerm{{Term: "nrrd", Kind: "extension", Source: "planned:ext-top", Exact: true}},
		},
		{
			name:       "extension planner",
			opts:       queryOptions{Query: "ext:.pdf", MatchPath: true, Limit: 20},
			wantSource: "planned:ext:pdf",
			wantTerms:  []traceTerm{{Term: "pdf", Kind: "extension", Source: "ext:pdf", Exact: true}},
		},
		{
			name:       "limited missing term",
			opts:       queryOptions{Query: "zzzzzz-no-hit", Limit: 20},
			wantSource: "limited-single-term",
		},
		{
			name:       "broad path terms",
			opts:       queryOptions{Query: "workspace plain", MatchPath: true, Limit: 20},
			wantSource: "planned:path-term:plain+path-term:workspace",
			wantTerms: []traceTerm{
				{Term: "plain", Kind: "path-substring", Source: "path-term:plain", Exact: false},
				{Term: "workspace", Kind: "path-substring", Source: "path-term:workspace", Exact: false},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trace := &searchTrace{}
			opts := tc.opts
			opts.Trace = trace
			if _, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates); err != nil {
				t.Fatal(err)
			}
			if trace.Source != tc.wantSource {
				t.Fatalf("trace source = %q, want %q", trace.Source, tc.wantSource)
			}
			if trace.Candidates < 0 {
				t.Fatalf("trace candidates = %d, want non-negative", trace.Candidates)
			}
			for _, want := range tc.wantTerms {
				if !traceHasTerm(trace.Terms, want) {
					t.Fatalf("trace terms = %+v, missing %+v", trace.Terms, want)
				}
			}
		})
	}
}

func traceHasTerm(terms []traceTerm, want traceTerm) bool {
	for _, term := range terms {
		if term.Term == want.Term && term.Kind == want.Kind && term.Source == want.Source && term.Exact == want.Exact {
			return true
		}
	}
	return false
}

func TestSearchTraceRecordsDeclineList(t *testing.T) {
	trace := &searchTrace{}
	trace.setDecline("component-trigram:unsupported-query")
	trace.replaceDecline("global-components:missing-source")
	trace.addDeclineForVolume("global-ext:missing-posting", "F:")

	if trace.Decline != "global-ext:missing-posting" {
		t.Fatalf("decline = %q, want latest decline", trace.Decline)
	}
	if len(trace.Declines) != 3 {
		t.Fatalf("declines = %+v, want three entries", trace.Declines)
	}
	if trace.Declines[0].Source != "component-trigram" || trace.Declines[0].Reason != "unsupported-query" {
		t.Fatalf("first decline = %+v", trace.Declines[0])
	}
	if trace.Declines[1].Source != "global-components" || trace.Declines[1].Reason != "missing-source" {
		t.Fatalf("second decline = %+v", trace.Declines[1])
	}
	if trace.Declines[2].Source != "global-ext" || trace.Declines[2].Reason != "missing-posting" || trace.Declines[2].Volume != "F:" {
		t.Fatalf("third decline = %+v", trace.Declines[2])
	}
}

func TestServiceResponseJSONIncludesStructuredTraceFields(t *testing.T) {
	complete := true
	resp := serviceResponse{
		OK:              true,
		Count:           2,
		SearchMS:        1.25,
		Source:          "global:components",
		PlannerMode:     "global-components",
		EligibleVolumes: []string{"C:", "F:"},
		Terms: []traceTerm{
			{Term: "workspace-alpha", Kind: "path-substring", Source: "global:component-subtree", CountHint: 16},
			{Term: "file", Kind: "type", Source: "global:type", CountHint: 16, Exact: true},
		},
		Declines: []traceDecline{
			{Source: "global-ext", Reason: "missing-posting", Volume: "F:"},
		},
		Fallback: "global-bounded-scan",
		Complete: &complete,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded serviceResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.PlannerMode != resp.PlannerMode || decoded.Source != resp.Source || decoded.Fallback != resp.Fallback {
		t.Fatalf("decoded trace route fields = %+v", decoded)
	}
	if !slices.Equal(decoded.EligibleVolumes, resp.EligibleVolumes) {
		t.Fatalf("eligible volumes = %v, want %v", decoded.EligibleVolumes, resp.EligibleVolumes)
	}
	if decoded.Complete == nil || !*decoded.Complete {
		t.Fatalf("complete = %v, want true pointer", decoded.Complete)
	}
	if !traceHasTerm(decoded.Terms, traceTerm{Term: "workspace-alpha", Kind: "path-substring", Source: "global:component-subtree"}) {
		t.Fatalf("decoded terms = %+v, missing component-subtree term", decoded.Terms)
	}
	if len(decoded.Declines) != 1 || decoded.Declines[0].Source != "global-ext" || decoded.Declines[0].Reason != "missing-posting" || decoded.Declines[0].Volume != "F:" {
		t.Fatalf("decoded declines = %+v", decoded.Declines)
	}
}

func TestExtensionShapedPathTopCandidatesAvoidDottedScan(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:.pdf", Limit: 20}
	trace := &searchTrace{}
	fastOpts := opts
	fastOpts.Trace = trace

	fast, err := searchCompactWithCache(idx, fastOpts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("extension-shaped top paths = %v, full paths = %v", got, want)
	}
	if trace.Source != "planned:ext-top" {
		t.Fatalf("trace source = %q, want planned:ext-top", trace.Source)
	}
	if trace.Candidates != 20 {
		t.Fatalf("trace candidates = %d, want 20", trace.Candidates)
	}
}

func TestFusedDottedPathNoHitUsesIntersectedTrigramCandidates(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:Downloads.nrrd", Limit: 20}
	trace := &searchTrace{}
	fastOpts := opts
	fastOpts.Trace = trace

	fast, err := searchCompactWithCache(idx, fastOpts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(fast) != 0 {
		t.Fatalf("fused no-hit matches = %v, want none", pathsOf(fast))
	}
	if trace.Source != "path-component-trigram" {
		t.Fatalf("trace source = %q, want path-component-trigram", trace.Source)
	}
	if trace.Candidates != 0 {
		t.Fatalf("trace candidates = %d, want 0", trace.Candidates)
	}
}

func TestLongComponentTermUsesIntersectedTrigramDespiteCommonGrams(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:opencode", Limit: 20}
	trace := &searchTrace{}
	fastOpts := opts
	fastOpts.Trace = trace

	fast, err := searchCompactWithCache(idx, fastOpts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("opencode trigram paths = %v, full paths = %v", got, want)
	}
	if trace.Source != "path-component-trigram" {
		t.Fatalf("trace source = %q, want path-component-trigram", trace.Source)
	}
}

func TestLimitedPathTermUsesTrigramNameMatches(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:opencode", Limit: 20}
	pq := mustParseQuery(t, opts)
	pq.Limit = normalizedLimit(opts.Limit, false)

	candidates, ok := vol.pathPlanTermPostingLimited("opencode", pq)
	if !ok {
		t.Fatal("pathPlanTermPostingLimited declined opencode")
	}
	fast := entriesForIDs(idx, candidates)
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("limited opencode paths = %v, full paths = %v", got, want)
	}
}

func TestPathNameTrigramTopCandidatesForManyDirectMatches(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := manyDirectNameMatchIndex("opencode", 80)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:opencode", Limit: 20}
	pq := mustParseQuery(t, opts)
	pq.Limit = normalizedLimit(opts.Limit, false)

	candidates, ok := vol.nameTrigramPathNameTopCandidates(pq)
	if !ok {
		t.Fatal("nameTrigramPathNameTopCandidates declined direct opencode matches")
	}
	if len(candidates) != 20 {
		t.Fatalf("candidate count = %d, want 20", len(candidates))
	}
	fast := entriesForIDs(idx, candidates)
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("top trigram paths = %v, full paths = %v", got, want)
	}
}

func TestPathNameTrigramTopCandidatesBoundLargeDirectory(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := broadComponentExpansionIndex(serviceComponentTrigramExpansionMaxIDs + 500)
	dir := idx.compactRecord(1)
	dir.Name = "opencode"
	idx.setCompactRecord(1, dir)
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:opencode", Limit: 20}
	pq := mustParseQuery(t, opts)
	pq.Limit = normalizedLimit(opts.Limit, false)

	candidates, ok := vol.nameTrigramPathNameTopCandidates(pq)
	if !ok {
		t.Fatal("nameTrigramPathNameTopCandidates declined large matching directory")
	}
	if len(candidates) != 20 {
		t.Fatalf("candidate count = %d, want 20", len(candidates))
	}
	fast := entriesForIDs(idx, candidates)
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("large directory top paths = %v, full paths = %v", got, want)
	}
}

func TestDottedPathSubstringAndExtensionSemanticsMatrix(t *testing.T) {
	idx := dottedPathBenchmarkIndex(200)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		query     string
		wantHas   []string
		wantLacks []string
	}{
		{
			query:     "path:.nrrd",
			wantHas:   []string{"scan-000037.nrrd", "dataset-000000.nrrd"},
			wantLacks: []string{"backup-000053.nrrd.bak", "metadata-000000.json"},
		},
		{
			query:     "ext:.nrrd",
			wantHas:   []string{"scan-000037.nrrd", "dataset-000000.nrrd"},
			wantLacks: []string{"backup-000053.nrrd.bak", "metadata-000000.json"},
		},
		{
			query:     "type:file ext:.nrrd",
			wantHas:   []string{"scan-000037.nrrd"},
			wantLacks: []string{"dataset-000000.nrrd", "backup-000053.nrrd.bak", "metadata-000000.json"},
		},
		{
			query:     "path:nrrd",
			wantHas:   []string{"scan-000037.nrrd", "nrrd-cache", "cache-000097.json"},
			wantLacks: []string{"plain-000001.txt"},
		},
		{
			query:     "path:.nrrd ext:json",
			wantHas:   nil,
			wantLacks: []string{"metadata-000000.json", "scan-000037.nrrd", "backup-000053.nrrd.bak", "cache-000097.json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got, err := searchCompactWithCache(idx, queryOptions{Query: tc.query, Limit: 500}, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatal(err)
			}
			names := namesOf(got)
			for _, want := range tc.wantHas {
				if !containsString(names, want) {
					t.Fatalf("%q names missing %q: %v", tc.query, want, names)
				}
			}
			for _, unwanted := range tc.wantLacks {
				if containsString(names, unwanted) {
					t.Fatalf("%q names unexpectedly included %q: %v", tc.query, unwanted, names)
				}
			}
		})
	}
}

func TestLimitedBroadSubstringCandidatesPreserveFullSearchFirstPage(t *testing.T) {
	idx := broadSubstringOrderingFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "path:nrrd", Limit: 5},
		{Query: "nrrd", Limit: 5},
		{Query: "path:.nrrd", Limit: 5},
	}
	for _, opts := range cases {
		t.Run(opts.Query, func(t *testing.T) {
			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("candidate search: %v", err)
			}
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("candidate first page = %v, full first page = %v", got, want)
			}
		})
	}
}

func TestBroadPathSearchAndCountParityMatrix(t *testing.T) {
	idx := dottedPathBenchmarkIndex(600)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	queries := []queryOptions{
		{Query: "path:.nrrd", Limit: 25},
		{Query: "path:.nrrd", Limit: 200},
		{Query: "path:nrrd", Limit: 25},
		{Query: "path:.nrrd ext:json", Limit: 25},
		{Query: "path:.nrrd type:file", Limit: 25},
		{Query: "path:nrrd !backup", Limit: 25},
		{Query: "path:nrrd type:file", Limit: 25},
		{Query: "path:nrrd ext:txt", Limit: 25},
		{Query: "path:nrrd size:>0", Limit: 25},
		{Query: "path:nrrd dm:2026-05-01", Limit: 25},
		{Query: "path:nrrd glob:*.json", Limit: 25},
		{Query: "path:cache ext:json", Limit: 25},
		{Query: "path:workspace .nrrd", Limit: 25},
		{Query: "path:workspace ext:nrrd|json", Limit: 25},
		{Query: "path:dataset ext:json", Limit: 25},
		{Query: "path:trainingdata .nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:Dataset trainingdata .nrrd", Limit: 25},
		{Query: ".nrrd path:trainingdata Dataset", Limit: 25},
		{Query: "path:trainingdata path:Dataset .nrrd", Limit: 25},
		{Query: "path:workspace trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset ext:nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset glob:*.nrrd", Limit: 25},
		{Query: "path:trainingdata missing .nrrd", Limit: 25},
		{Query: "path:.nrrd ext:nrrd|json", Limit: 50},
		{Query: "path:.nrrd !metadata", Limit: 50},
		{Query: "path:.nrrd", Under: `C:\workspace\dataset-000000.nrrd`, Limit: 25},
		{Query: "ext:json", Under: `C:\workspace\dataset-000000.nrrd`, Limit: 25},
		{Query: "path:.nrrd ext:json", Under: `C:\workspace`, Limit: 25},
	}
	for _, opts := range queries {
		t.Run(opts.Query+"/search", func(t *testing.T) {
			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("candidate search: %v", err)
			}
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("candidate paths = %v, full paths = %v", got, want)
			}
		})
		t.Run(opts.Query+"/count", func(t *testing.T) {
			countOpts := opts
			countOpts.Limit = 0
			full, err := searchCompactWithCache(idx, countOpts, true, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full count search: %v", err)
			}
			count, ok := vol.plannedCount(mustParseQuery(t, countOpts))
			if !ok {
				t.Skip("plannedCount declined query")
			}
			if count != len(full) {
				t.Fatalf("planned count = %d, full count = %d", count, len(full))
			}
		})
	}
}

func TestGeneratedBroadPathQueryParity(t *testing.T) {
	idx := dottedPathBenchmarkIndex(800)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	pathTerms := []string{
		"path:.nrrd",
		"path:nrrd",
		"path:cache",
		"path:dataset",
		"path:workspace",
		"path:trainingdata",
		"path:trainingdata Dataset",
		"path:Dataset trainingdata",
		"path:workspace trainingdata Dataset",
		"path:missing Dataset",
	}
	filters := []string{"", "ext:nrrd", "ext:json", "type:file", "glob:*.json", "!backup", "!metadata", "ext:nrrd|json"}
	filters = append(filters, ".nrrd", "glob:*.nrrd")
	limits := []int{1, 5, 25, 100}
	unders := []string{"", `C:\workspace`, `C:\workspace\nrrd-cache`, `C:\workspace\dataset-000000.nrrd`}
	for _, term := range pathTerms {
		for _, filter := range filters {
			for _, limit := range limits {
				for _, under := range unders {
					query := strings.TrimSpace(term + " " + filter)
					opts := queryOptions{Query: query, Limit: limit, Under: under}
					t.Run(fmt.Sprintf("%s/limit:%d/under:%s", query, limit, under), func(t *testing.T) {
						full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
						if err != nil {
							t.Fatalf("full search: %v", err)
						}
						fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
						if err != nil {
							t.Fatalf("candidate search: %v", err)
						}
						if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
							t.Fatalf("candidate paths = %v, full paths = %v", got, want)
						}
					})
				}
			}
		}
	}
}

func TestMultiPartPathQueriesUseBoundedCandidateSources(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := dottedPathBenchmarkIndex(2000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	queries := []queryOptions{
		{Query: "path:trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:Dataset trainingdata .nrrd", Limit: 25},
		{Query: ".nrrd path:trainingdata Dataset", Limit: 25},
		{Query: "path:trainingdata path:Dataset .nrrd", Limit: 25},
		{Query: "path:workspace trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset ext:nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset glob:*.nrrd", Limit: 25},
		{Query: "path:trainingdata missing .nrrd", Limit: 25},
	}
	for _, opts := range queries {
		t.Run(opts.Query, func(t *testing.T) {
			pq := mustParseQuery(t, opts)
			pq.Limit = normalizedLimit(opts.Limit, false)
			plan, ok := vol.buildCandidatePlan(pq)
			if !ok {
				t.Fatal("buildCandidatePlan declined multi-part path query")
			}
			summary := plan.sourceSummary()
			if !plan.empty && !strings.Contains(summary, "path-term:") &&
				!strings.Contains(summary, "ext:") && !strings.Contains(summary, "glob-ext:") {
				t.Fatalf("plan sources = %s, want a bounded path, extension, or glob source", summary)
			}
			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("candidate search: %v", err)
			}
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("candidate paths = %v, full paths = %v", got, want)
			}
		})
	}
}

func TestExtensionBoundedPathTermsAvoidBroadPathReconstruction(t *testing.T) {
	idx := highExtensionFanoutPathIndex(3500)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	queries := []queryOptions{
		{Query: "path:trainingdata Dataset ext:nrrd", Limit: 20},
		{Query: "path:trainingdata Dataset .nrrd", Limit: 20},
		{Query: "path:trainingdata Dataset nrrd", Limit: 20},
		{Query: "path:absent Dataset ext:nrrd", Limit: 20},
		{Query: "path:absent Dataset .nrrd", Limit: 20},
		{Query: "path:absent Dataset nrrd", Limit: 20},
	}
	for _, opts := range queries {
		t.Run(opts.Query, func(t *testing.T) {
			pq := mustParseQuery(t, opts)
			dropSatisfiedVolumeTerms(&pq, idx.Volume)
			pq.Limit = normalizedLimit(opts.Limit, false)
			plan, ok := vol.buildCandidatePlan(pq)
			if !ok {
				t.Fatalf("plan = %+v, ok=%v, want bounded candidate plan", plan, ok)
			}
			summary := plan.sourceSummary()
			if !plan.empty && !strings.Contains(summary, "ext:nrrd") &&
				!strings.Contains(summary, "term:") && !strings.Contains(summary, "path-term:") {
				t.Fatalf("plan sources = %s, want extension or bounded term source", summary)
			}

			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			pathCache := make(map[int]string)
			fast, err := searchCompactWithCache(idx, opts, false, pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("candidate search: %v", err)
			}
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("candidate paths = %v, full paths = %v", got, want)
			}
			if len(pathCache) > 32 {
				t.Fatalf("path cache grew to %d entries for %q; path terms should be verified before broad path reconstruction", len(pathCache), opts.Query)
			}
		})
	}
}

func TestDriveScopedBareSuffixPathTermsPreserveSubstringParity(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := dottedPathBenchmarkIndex(5000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	queries := []queryOptions{
		{Query: "path:C: nrrd", Limit: 25},
		{Query: "path:C: raw", Limit: 25},
		{Query: "path:C: pdf", Limit: 25},
		{Query: "path:C: pvsm", Limit: 25},
	}
	for _, opts := range queries {
		t.Run(opts.Query, func(t *testing.T) {
			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("candidate search: %v", err)
			}
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("candidate paths = %v, full paths = %v", got, want)
			}
		})
	}
}

func TestGeneratedMultiPartPathSyntaxParityMatrix(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := highExtensionFanoutPathIndex(1200)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	pathPhrases := []string{
		"path:trainingdata Dataset",
		"path:Dataset trainingdata",
		"trainingdata path:Dataset",
		"path:trainingdata path:Dataset",
		"path:workspace trainingdata Dataset",
		"path:absent Dataset",
	}
	extForms := []string{"nrrd", ".nrrd", "ext:nrrd", "glob:*.nrrd"}
	volumeForms := []string{"", "path:C:"}
	negativeForms := []string{"", "!backup", "!metadata"}
	limits := []int{1, 5, 20}
	for _, volume := range volumeForms {
		for _, phrase := range pathPhrases {
			for _, ext := range extForms {
				for _, neg := range negativeForms {
					for _, limit := range limits {
						query := strings.Join(nonEmptyStrings(volume, phrase, ext, neg), " ")
						opts := queryOptions{Query: query, Limit: limit}
						t.Run(fmt.Sprintf("%s/limit:%d", query, limit), func(t *testing.T) {
							full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
							if err != nil {
								t.Fatalf("full search: %v", err)
							}
							fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
							if err != nil {
								t.Fatalf("candidate search: %v", err)
							}
							if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
								t.Fatalf("candidate paths = %v, full paths = %v", got, want)
							}
						})
					}
				}
			}
		}
	}
}

func TestGeneratedLooseKeywordSyntaxParityMatrix(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := dottedPathBenchmarkIndex(1600)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	queries := []string{
		"nrrd",
		".nrrd",
		"raw",
		".raw",
		"pdf",
		".pdf",
		"pvsm",
		".pvsm",
		"F: nrrd",
		"F: .nrrd",
		"C: nrrd",
		"C: .nrrd",
		"C: pvsm",
		"C: .pvsm",
		"trainingdata Dataset nrrd",
		"Dataset trainingdata nrrd",
		"workspace trainingdata Dataset nrrd",
		"Downloads nrrd",
		"Users exampleuser",
		"exampleuser Users",
		"Users exampleuser Downloads",
		"Downloads exampleuser Users",
		"Downloads docx",
		"fixtureproj",
		"F: fixtureproj",
		"path:F: fixtureproj",
		"nrrd !backup",
		"nrrd type:file",
		"nrrd glob:*.json",
		"nrrd ext:json",
		`workspace\nrrd-cache cache`,
		`workspace/dataset-000000.nrrd metadata`,
	}
	limits := []int{1, 5, 25, 100}
	for _, query := range queries {
		for _, limit := range limits {
			opts := queryOptions{Query: query, Limit: limit}
			t.Run(fmt.Sprintf("%s/limit:%d", query, limit), func(t *testing.T) {
				full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
				if err != nil {
					t.Fatalf("full search: %v", err)
				}
				fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
				if err != nil {
					t.Fatalf("candidate search: %v", err)
				}
				if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
					t.Fatalf("candidate paths = %v, full paths = %v", got, want)
				}
			})
		}
	}
}

func TestLooseKeywordServiceVolumeMatrixUnderTarget(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	cIdx := dottedPathBenchmarkIndex(60_000)
	fIdx := dottedPathBenchmarkIndex(60_000)
	fIdx.Volume = "F:"
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("loose-c.gsi", cIdx),
		newServiceVolumeIndex("loose-f.gsi", fIdx),
	}
	for _, vol := range volumes {
		vol.rebuildNameTrigramsLocked()
	}
	queries := []string{
		"nrrd",
		".nrrd",
		"raw",
		".raw",
		"pdf",
		".pdf",
		"pvsm",
		".pvsm",
		"F: nrrd",
		"F: .nrrd",
		"F: raw",
		"F: .raw",
		"F: pdf",
		"F: .pdf",
		"C: pvsm",
		"C: .pvsm",
		"trainingdata Dataset nrrd",
		"Dataset trainingdata nrrd",
		"workspace trainingdata Dataset nrrd",
		"nrrd !backup",
		"nrrd type:file",
		"nrrd glob:*.json",
		`workspace\nrrd-cache cache`,
	}
	all := make([]float64, 0, len(queries)*3)
	perQuery := make(map[string][]float64, len(queries))
	for i := 0; i < 3; i++ {
		for _, query := range queries {
			start := time.Now()
			_, err := searchServiceVolumes(volumes, queryOptions{Query: query, Limit: 100}, false)
			if err != nil {
				t.Fatalf("query %q failed: %v", query, err)
			}
			ms := float64(time.Since(start).Microseconds()) / 1000
			all = append(all, ms)
			perQuery[query] = append(perQuery[query], ms)
		}
	}
	if p95 := percentile(append([]float64(nil), all...), 0.95); p95 > 100 {
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("loose keyword matrix p95 = %.3fms, want <= 100ms; per-query=%v", p95, perQuery)
		}
		t.Logf("loose keyword matrix p95 = %.3fms over soft 100ms budget; per-query=%v", p95, perQuery)
	}
}

func TestGeneratedLooseQueryFuzzMatrix(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	cIdx := dottedPathBenchmarkIndex(24_000)
	fIdx := dottedPathBenchmarkIndex(24_000)
	fIdx.Volume = "F:"
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("fuzz-c.gsi", cIdx),
		newServiceVolumeIndex("fuzz-f.gsi", fIdx),
	}
	for _, vol := range volumes {
		vol.rebuildNameTrigramsLocked()
	}

	queries := generatedLooseFuzzQueries()
	all := make([]float64, 0, len(queries)*2)
	perQuery := make(map[string][]float64, len(queries))
	for i := 0; i < 2; i++ {
		for _, query := range queries {
			matchPath := looseFuzzMatchPath(query)
			opts := queryOptions{Query: query, MatchPath: matchPath, Limit: 50}
			start := time.Now()
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatalf("query %q failed: %v", query, err)
			}
			ms := float64(time.Since(start).Microseconds()) / 1000
			all = append(all, ms)
			perQuery[query] = append(perQuery[query], ms)

			full, err := searchAll([]*Index{cIdx, fIdx}, opts, false)
			if err != nil {
				t.Fatalf("full query %q failed: %v", query, err)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(full); !sameOrderedStrings(gotPaths, wantPaths) {
				t.Fatalf("query %q paths = %v, full paths = %v", query, gotPaths, wantPaths)
			}
		}
	}
	if p95 := percentile(append([]float64(nil), all...), 0.95); p95 > 100 {
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("loose fuzz matrix p95 = %.3fms, want <= 100ms; per-query=%v", p95, perQuery)
		}
		t.Logf("loose fuzz matrix p95 = %.3fms over soft 100ms budget; per-query=%v", p95, perQuery)
	}
}
