package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type deterministicQueryCase struct {
	Name         string
	Query        string
	Limit        int
	MatchPath    bool
	Under        string
	WantNonEmpty bool
	Family       string
}

func TestDeterministicV2QueryMatrixAcrossEngineVariants(t *testing.T) {
	cases := deterministicQueryCases()
	normalVolumes := deterministicMultiVolumeCorpus(t, 42)
	// The lowmem subtests below used to rebuild this same seed-42 corpus
	// for every case (2 builds x 28 cases). Construction reads
	// SEEKFS_MEMORY_MODE (lowmem packs records instead of building
	// resident maps), so the pair is built once here under lowmem and
	// reused read-only across the sequential subtests: same volumes, same
	// queries, same assertions, minus the redundant rebuilds. The warm
	// pair additionally carries the resident trigram build the old
	// per-case code ran.
	// t.Setenv cannot scope to these builds (it would leak lowmem into
	// the normal-service subtests), so the env is set around the builds
	// and restored immediately; the lowmem-* subtests still t.Setenv for
	// their query paths as before.
	prevMemMode, hadMemMode := os.LookupEnv("SEEKFS_MEMORY_MODE")
	os.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	lowmemColdVolumes := deterministicMultiVolumeCorpus(t, 42)
	lowmemWarmVolumes := deterministicMultiVolumeCorpus(t, 42)
	for _, vol := range lowmemWarmVolumes {
		vol.rebuildNameTrigramsLocked()
	}
	if hadMemMode {
		os.Setenv("SEEKFS_MEMORY_MODE", prevMemMode)
	} else {
		os.Unsetenv("SEEKFS_MEMORY_MODE")
	}
	oracle := func(tc deterministicQueryCase) []Entry {
		t.Helper()
		return deterministicFullScanAcrossVolumes(t, normalVolumes, tc)
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			want := oracle(tc)
			if tc.WantNonEmpty && len(want) == 0 {
				t.Fatalf("oracle returned no results for non-empty case %q", tc.Query)
			}
			if strings.Contains(tc.Query, "path:C:") || strings.HasPrefix(strings.ToLower(tc.Under), `c:\`) {
				assertAllPathsOnVolume(t, want, "C:")
			}
			if strings.Contains(tc.Query, "path:F:") || strings.HasPrefix(strings.ToLower(tc.Under), `f:\`) {
				assertAllPathsOnVolume(t, want, "F:")
			}

			t.Run("normal-service", func(t *testing.T) {
				trace := &searchTrace{}
				got := deterministicServiceSearch(t, normalVolumes, tc, trace, false)
				assertSameOrderedPaths(t, got, want)
				assertPlannerBudget(t, tc, *trace, deterministicRecordCount(normalVolumes))
			})

			t.Run("normal-count", func(t *testing.T) {
				got := deterministicServiceSearch(t, normalVolumes, tc, &searchTrace{}, true)
				wantCount := deterministicFullScanCountAcrossVolumes(t, normalVolumes, tc)
				if len(got) != wantCount {
					t.Fatalf("count-only result count = %d, want %d", len(got), wantCount)
				}
			})

			t.Run("lowmem-cold", func(t *testing.T) {
				t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
				volumes := lowmemColdVolumes
				want := deterministicFullScanAcrossVolumes(t, volumes, tc)
				trace := &searchTrace{}
				got := deterministicServiceSearch(t, volumes, tc, trace, false)
				assertSameOrderedPaths(t, got, want)
				assertPlannerBudget(t, tc, *trace, deterministicRecordCount(volumes))
			})

			t.Run("lowmem-warm-trigram", func(t *testing.T) {
				t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
				volumes := lowmemWarmVolumes
				want := deterministicFullScanAcrossVolumes(t, volumes, tc)
				trace := &searchTrace{}
				got := deterministicServiceSearch(t, volumes, tc, trace, false)
				assertSameOrderedPaths(t, got, want)
				assertPlannerBudget(t, tc, *trace, deterministicRecordCount(volumes))
			})
		})
	}
}

func TestDeterministicV2LooseAndExplicitEquivalence(t *testing.T) {
	volumes := deterministicMultiVolumeCorpus(t, 42)
	for _, pair := range []struct {
		name     string
		loose    deterministicQueryCase
		explicit deterministicQueryCase
	}{
		{
			name:     "downloads-nrrd",
			loose:    deterministicQueryCase{Query: "Downloads nrrd", MatchPath: true, Limit: 20},
			explicit: deterministicQueryCase{Query: "path:Downloads .nrrd", Limit: 20},
		},
		{
			name:     "fixtureproj-trainingdata",
			loose:    deterministicQueryCase{Query: "fixtureproj trainingdata", MatchPath: true, Limit: 20},
			explicit: deterministicQueryCase{Query: `path:F:\fixtureproj trainingdata`, Limit: 20},
		},
	} {
		t.Run(pair.name, func(t *testing.T) {
			loose := deterministicFullScanAcrossVolumes(t, volumes, pair.loose)
			explicit := deterministicFullScanAcrossVolumes(t, volumes, pair.explicit)
			assertSameOrderedPaths(t, loose, explicit)
		})
	}
}

func deterministicCorpus(t *testing.T, seed int) *Index {
	t.Helper()
	return deterministicVolumeCorpus(t, "C:", seed)
}

func deterministicMultiVolumeCorpus(t *testing.T, seed int) []*serviceVolumeIndex {
	t.Helper()
	return []*serviceVolumeIndex{
		newServiceVolumeIndex("deterministic-c.gsi", deterministicVolumeCorpus(t, "C:", seed)),
		newServiceVolumeIndex("deterministic-f.gsi", deterministicVolumeCorpus(t, "F:", seed+1)),
	}
}

func deterministicQueryCases() []deterministicQueryCase {
	return []deterministicQueryCase{
		{Name: "bare-md", Query: "md", Limit: 20, WantNonEmpty: true, Family: "bare-extension"},
		{Name: "bare-nrrd", Query: "nrrd", Limit: 20, WantNonEmpty: true, Family: "bare-extension"},
		{Name: "bare-pdf", Query: "pdf", Limit: 20, WantNonEmpty: true, Family: "bare-extension"},
		{Name: "dotted-md", Query: ".md", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "dotted-extension"},
		{Name: "dotted-nrrd", Query: ".nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "dotted-extension"},
		{Name: "dotted-pdf", Query: ".pdf", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "dotted-extension"},
		{Name: "downloads-md", Query: "Downloads md", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "loose-path"},
		{Name: "downloads-nrrd", Query: "Downloads nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "loose-path"},
		{Name: "downloads-dotted-nrrd", Query: "Downloads .nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "loose-path"},
		{Name: "fixtureproj-trainingdata", Query: "fixtureproj trainingdata", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "loose-path"},
		{Name: "path-downloads-md", Query: "path:Downloads md", Limit: 20, WantNonEmpty: true, Family: "explicit-path"},
		{Name: "path-downloads-dotted-nrrd", Query: "path:Downloads .nrrd", Limit: 20, WantNonEmpty: true, Family: "explicit-path"},
		{Name: "path-fixtureproj-trainingdata", Query: `path:F:\fixtureproj trainingdata`, Limit: 20, WantNonEmpty: true, Family: "explicit-path"},
		{Name: "path-f-fixtureproj-trainingdata", Query: "path:F: fixtureproj trainingdata", Limit: 20, WantNonEmpty: true, Family: "drive-scoped"},
		{Name: "path-c-trainingdata-dataset-nrrd", Query: "path:C: trainingdata Dataset .nrrd", Limit: 20, WantNonEmpty: true, Family: "drive-scoped"},
		{Name: "path-f-trainingdata-dataset-nrrd", Query: "path:F: trainingdata Dataset .nrrd", Limit: 20, WantNonEmpty: true, Family: "drive-scoped"},
		{Name: "trainingdata-dataset-nrrd", Query: "trainingdata Dataset nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "reordered"},
		{Name: "dataset-trainingdata-nrrd", Query: "Dataset trainingdata nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "reordered"},
		{Name: "nrrd-dataset-trainingdata", Query: "nrrd Dataset trainingdata", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "reordered"},
		{Name: "ext-nrrd-path-downloads", Query: "ext:nrrd path:Downloads", Limit: 20, WantNonEmpty: true, Family: "mixed-filter"},
		{Name: "dir-downloads-nrrd", Query: "dir:Downloads nrrd", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "mixed-filter"},
		{Name: "downloads-md-not-draft", Query: "Downloads md !draft", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "negation"},
		{Name: "trainingdata-nrrd-not-backup", Query: "trainingdata nrrd !backup", MatchPath: true, Limit: 20, WantNonEmpty: true, Family: "negation"},
		{Name: "downloads-nohit-selective", Query: "Downloads zzzz-nohit-md", MatchPath: true, Limit: 20, Family: "no-hit-selective"},
		{Name: "nohit-broad-grams", Query: "path:node_modules commonishzz", Limit: 20, Family: "no-hit-broad"},
		{Name: "path-node-modules-md", Query: "path:node_modules md", Limit: 20, WantNonEmpty: true, Family: "broad-common"},
		{Name: "path-appdata-json", Query: "path:AppData json", Limit: 20, WantNonEmpty: true, Family: "broad-common"},
		{Name: "under-downloads-nrrd", Query: "nrrd", MatchPath: true, Under: `C:\Users\exampleuser\Downloads`, Limit: 20, WantNonEmpty: true, Family: "under"},
	}
}

func deterministicVolumeCorpus(t *testing.T, volume string, seed int) *Index {
	t.Helper()
	idx := &Index{
		Source:  "usn",
		Volume:  volume,
		Compact: true,
		Records: make([]CompactRecord, 0, 7000),
	}
	nextFRN := uint64(seed * 100_000)
	if nextFRN == 0 {
		nextFRN = 100_000
	}
	add := func(parent int32, parentFRN uint64, name string, mode uint32) int32 {
		nextFRN++
		rec := CompactRecord{
			FRN:       nextFRN,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      int64(100 + len(name)),
			ModUnix:   time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC).Add(time.Duration(len(idx.Records)) * time.Second).UnixNano(),
		}
		idx.Records = append(idx.Records, rec)
		return int32(len(idx.Records) - 1)
	}
	rootFRN := nextFRN + 1
	idx.Records = append(idx.Records, CompactRecord{FRN: rootFRN, ParentFRN: rootFRN, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)})
	nextFRN = rootFRN
	root := int32(0)
	users := add(root, rootFRN, "Users", uint32(os.ModeDir))
	user := add(users, idx.Records[users].FRN, "exampleuser", uint32(os.ModeDir))
	downloads := add(user, idx.Records[user].FRN, "Downloads", uint32(os.ModeDir))
	appData := add(user, idx.Records[user].FRN, "AppData", uint32(os.ModeDir))
	workspace := add(root, rootFRN, "workspace", uint32(os.ModeDir))
	trainingdata := add(workspace, idx.Records[workspace].FRN, "trainingdata", uint32(os.ModeDir))
	dataset := add(trainingdata, idx.Records[trainingdata].FRN, "Dataset", uint32(os.ModeDir))
	nodeModules := add(workspace, idx.Records[workspace].FRN, "node_modules", uint32(os.ModeDir))
	target := add(workspace, idx.Records[workspace].FRN, "target", uint32(os.ModeDir))
	gitDir := add(workspace, idx.Records[workspace].FRN, ".git", uint32(os.ModeDir))
	unicodeDir := add(workspace, idx.Records[workspace].FRN, "Ünicode", uint32(os.ModeDir))

	add(downloads, idx.Records[downloads].FRN, "product_summary.md", 0)
	add(downloads, idx.Records[downloads].FRN, "scenario_region_summary.md", 0)
	add(downloads, idx.Records[downloads].FRN, "labels-cleaned.nrrd", 0)
	add(downloads, idx.Records[downloads].FRN, "filtered-volume-cleaned.nrrd", 0)
	add(downloads, idx.Records[downloads].FRN, "fixturemetrics_surface_stats.csv", 0)
	add(downloads, idx.Records[downloads].FRN, "draft-notes.md", 0)
	add(appData, idx.Records[appData].FRN, "settings.json", 0)
	add(appData, idx.Records[appData].FRN, "profile-cache.json", 0)
	add(dataset, idx.Records[dataset].FRN, "training-volume.nrrd", 0)
	add(dataset, idx.Records[dataset].FRN, "validation-volume.nrrd", 0)
	add(dataset, idx.Records[dataset].FRN, "backup-volume.nrrd", 0)
	add(nodeModules, idx.Records[nodeModules].FRN, "readme.md", 0)
	add(nodeModules, idx.Records[nodeModules].FRN, "package.json", 0)
	add(target, idx.Records[target].FRN, "artifact.raw", 0)
	add(gitDir, idx.Records[gitDir].FRN, "config", 0)
	add(unicodeDir, idx.Records[unicodeDir].FRN, "Résumé.pdf", 0)

	if strings.EqualFold(volume, "F:") {
		fixtureproj := add(root, rootFRN, "fixtureproj-dev", uint32(os.ModeDir))
		projects := add(fixtureproj, idx.Records[fixtureproj].FRN, "projects", uint32(os.ModeDir))
		model := add(projects, idx.Records[projects].FRN, "model", uint32(os.ModeDir))
		acmeTrainingdata := add(model, idx.Records[model].FRN, "trainingdata", uint32(os.ModeDir))
		rawFiles := add(acmeTrainingdata, idx.Records[acmeTrainingdata].FRN, "raw files", uint32(os.ModeDir))
		for i := 0; i < 120; i++ {
			add(rawFiles, idx.Records[rawFiles].FRN, fmt.Sprintf("volume-%03d.nrrd", i), 0)
		}
	} else {
		add(root, rootFRN, "fixtureproj-old-empty", uint32(os.ModeDir))
	}

	for i := 0; i < 900; i++ {
		add(downloads, idx.Records[downloads].FRN, fmt.Sprintf("download-noise-%04d.bin", i), 0)
	}
	for i := 0; i < 900; i++ {
		add(appData, idx.Records[appData].FRN, fmt.Sprintf("cache-entry-%04d.json", i), 0)
	}
	for i := 0; i < 900; i++ {
		add(nodeModules, idx.Records[nodeModules].FRN, fmt.Sprintf("package-readme-%04d.md", i), 0)
	}
	for i := 0; i < 900; i++ {
		add(target, idx.Records[target].FRN, fmt.Sprintf("build-output-%04d.raw", i), 0)
	}
	for i := 0; i < 600; i++ {
		parent := workspace
		parentFRN := idx.Records[workspace].FRN
		ext := "txt"
		switch i % 7 {
		case 0:
			ext = "md"
		case 1:
			ext = "nrrd"
		case 2:
			ext = "raw"
		case 3:
			ext = "pdf"
		case 4:
			ext = "json"
		case 5:
			ext = "go"
		case 6:
			ext = "py"
		}
		add(parent, parentFRN, fmt.Sprintf("common-file-%04d.%s", i, ext), 0)
	}
	buildOrders(idx)
	return idx
}

func runDeterministicQueryAcrossVariants(t *testing.T, volumes []*serviceVolumeIndex, tc deterministicQueryCase) {
	t.Helper()
	want := deterministicFullScanAcrossVolumes(t, volumes, tc)
	got := deterministicServiceSearch(t, volumes, tc, &searchTrace{}, false)
	assertSameOrderedPaths(t, got, want)
}

func deterministicFullScanAcrossVolumes(t *testing.T, volumes []*serviceVolumeIndex, tc deterministicQueryCase) []Entry {
	t.Helper()
	opts := deterministicOptions(tc, nil)
	selected, err := serviceVolumesForQuery(volumes, opts)
	if err != nil {
		t.Fatal(err)
	}
	selected = prioritizeServiceVolumesForPathTerms(selected, opts)
	limit := normalizedLimit(opts.Limit, false)
	out := make([]Entry, 0, limit)
	for _, vol := range selected {
		childOpts := opts
		childOpts.Limit = max(limit, vol.index.compactRecordCount())
		got, err := searchCompactWithCache(vol.index, childOpts, false, make(map[int]string), nil)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got...)
	}
	if entriesSpanMultipleVolumes(out) {
		pq, err := parseQuery(opts)
		if err != nil {
			t.Fatal(err)
		}
		sortSearchAllEntries(out, pq)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func deterministicFullScanCountAcrossVolumes(t *testing.T, volumes []*serviceVolumeIndex, tc deterministicQueryCase) int {
	t.Helper()
	opts := deterministicOptions(tc, nil)
	selected, err := serviceVolumesForQuery(volumes, opts)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, vol := range selected {
		got, err := searchCompactWithCache(vol.index, opts, true, make(map[int]string), nil)
		if err != nil {
			t.Fatal(err)
		}
		total += len(got)
	}
	return total
}

func deterministicServiceSearch(t *testing.T, volumes []*serviceVolumeIndex, tc deterministicQueryCase, trace *searchTrace, countOnly bool) []Entry {
	t.Helper()
	opts := deterministicOptions(tc, trace)
	got, err := searchServiceVolumes(volumes, opts, countOnly)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func deterministicOptions(tc deterministicQueryCase, trace *searchTrace) queryOptions {
	return queryOptions{
		Query:     tc.Query,
		MatchPath: tc.MatchPath || queryLooksLoosePathScoped(tc.Query),
		Limit:     tc.Limit,
		Under:     tc.Under,
		Trace:     trace,
	}
}

func assertSameOrderedPaths(t *testing.T, got, want []Entry) {
	t.Helper()
	if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !sameOrderedStrings(gotPaths, wantPaths) {
		t.Fatalf("paths = %v, want %v", gotPaths, wantPaths)
	}
}

func assertPlannerBudget(t *testing.T, tc deterministicQueryCase, trace searchTrace, recordCount int) {
	t.Helper()
	if trace.Source == "" {
		t.Fatalf("query %q did not report planner source", tc.Query)
	}
	if trace.Source == "filesystem-under-fallback" {
		t.Fatalf("query %q used filesystem fallback", tc.Query)
	}
	budget := deterministicCandidateBudget(tc, tc.Limit, trace.Candidates, recordCount)
	if trace.Source == "global:bounded-scan" {
		budget = recordCount
	}
	if trace.Candidates > budget {
		t.Fatalf("query %q source=%s candidates=%d budget=%d", tc.Query, trace.Source, trace.Candidates, budget)
	}
	if trace.Source == "compact-name-order-scan" && trace.Candidates > max(512, tc.Limit*4) {
		t.Fatalf("query %q used broad compact-name-order-scan candidates=%d", tc.Query, trace.Candidates)
	}
}

func deterministicCandidateBudget(tc deterministicQueryCase, limit, resultCount, recordCount int) int {
	budget := max(4*limit, 512)
	switch tc.Family {
	case "broad-common", "cold-trigram", "dotted-extension", "mixed-filter", "negation", "under":
		return min(recordCount, max(budget, 50_000))
	default:
		return min(recordCount, budget)
	}
}

func deterministicRecordCount(volumes []*serviceVolumeIndex) int {
	total := 0
	for _, vol := range volumes {
		if vol != nil && vol.index != nil {
			total += vol.index.compactRecordCount()
		}
	}
	return total
}

func assertAllPathsOnVolume(t *testing.T, entries []Entry, volume string) {
	t.Helper()
	for _, entry := range entries {
		if !strings.HasPrefix(strings.ToLower(entry.Path), strings.ToLower(volume+`\`)) {
			t.Fatalf("path %q is not on volume %s", entry.Path, volume)
		}
	}
}

type coverageGapCase struct {
	Name         string
	Query        string
	MatchPath    bool
	Limit        int
	Exists       bool
	WantNonEmpty bool
	Corpus       func(t *testing.T) []*serviceVolumeIndex
}

// TestDeterministicCoverageGapFamilies is the R5 audit regression: every
// planner family that declines to a full-scan fallback must still be exact,
// complete, and count/search-parity across a multi-volume synthetic corpus.
func TestDeterministicCoverageGapFamilies(t *testing.T) {
	cases := []coverageGapCase{
		{Name: "short-term-x", Query: "x", Limit: 5000, WantNonEmpty: true},
		{Name: "complex-glob", Query: "glob:*volume*", Limit: 5000, WantNonEmpty: true},
		{Name: "non-literal-regex", Query: `regex:.*\.(md|txt)$`, Limit: 5000, WantNonEmpty: true},
		{Name: "rootless-exists", Query: "needle", Limit: 20, Exists: true, WantNonEmpty: true, Corpus: deterministicRealPathCorpus},
		{Name: "type-dir-term", Query: "type:dir node_modules", Limit: 5000, WantNonEmpty: true},
		{Name: "parent-no-subt-large", Query: "parent:workspace-alpha", Limit: 5000, WantNonEmpty: true, Corpus: deterministicParentLargeCorpus},
		{Name: "broad-common-term", Query: "commonish", Limit: 5000, WantNonEmpty: true, Corpus: deterministicBroadTermCorpus},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			volumes := deterministicMultiVolumeCorpus(t, 42)
			if tc.Corpus != nil {
				volumes = tc.Corpus(t)
			}
			assertCoverageGapFamily(t, volumes, tc)
		})
	}
}

func assertCoverageGapFamily(t *testing.T, volumes []*serviceVolumeIndex, tc coverageGapCase) {
	t.Helper()
	opts := queryOptions{Query: tc.Query, MatchPath: tc.MatchPath, Limit: tc.Limit, Exists: tc.Exists}
	want := deterministicFullScanAcrossVolumesOptions(t, volumes, opts)
	if tc.WantNonEmpty && len(want) == 0 {
		t.Fatalf("oracle returned no results for %q", tc.Query)
	}
	wantCount := deterministicFullScanCountAcrossVolumesOptions(t, volumes, opts)

	trace := &searchTrace{}
	searchOpts := opts
	searchOpts.Trace = trace
	got, err := searchServiceVolumes(volumes, searchOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	assertSameOrderedPaths(t, got, want)
	if !completeTrace(trace) {
		t.Fatalf("query %q search trace is not complete: source=%s mode=%s", tc.Query, trace.Source, trace.PlannerMode)
	}
	if len(got) != min(tc.Limit, wantCount) {
		t.Fatalf("query %q search results = %d, want min(limit=%d, count=%d)", tc.Query, len(got), tc.Limit, wantCount)
	}

	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, handled, err := countServiceVolumes(volumes, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatalf("query %q count not handled", tc.Query)
	}
	if count != wantCount {
		t.Fatalf("query %q count = %d, want oracle count %d", tc.Query, count, wantCount)
	}
	if !completeTrace(countTrace) {
		t.Fatalf("query %q count trace is not complete: source=%s mode=%s", tc.Query, countTrace.Source, countTrace.PlannerMode)
	}
	if count != wantCount {
		t.Fatalf("query %q count/search parity: count %d, search exhaustive %d", tc.Query, count, wantCount)
	}
}

func completeTrace(trace *searchTrace) bool {
	return trace != nil && trace.Complete != nil && *trace.Complete
}

func deterministicFullScanAcrossVolumesOptions(t *testing.T, volumes []*serviceVolumeIndex, opts queryOptions) []Entry {
	t.Helper()
	selected, err := serviceVolumesForQuery(volumes, opts)
	if err != nil {
		t.Fatal(err)
	}
	selected = prioritizeServiceVolumesForPathTerms(selected, opts)
	limit := normalizedLimit(opts.Limit, false)
	out := make([]Entry, 0, limit)
	for _, vol := range selected {
		childOpts := opts
		childOpts.Limit = max(limit, vol.index.compactRecordCount())
		got, err := searchCompactWithCache(vol.index, childOpts, false, make(map[int]string), nil)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got...)
	}
	if entriesSpanMultipleVolumes(out) {
		pq, err := parseQuery(opts)
		if err != nil {
			t.Fatal(err)
		}
		sortSearchAllEntries(out, pq)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func deterministicFullScanCountAcrossVolumesOptions(t *testing.T, volumes []*serviceVolumeIndex, opts queryOptions) int {
	t.Helper()
	selected, err := serviceVolumesForQuery(volumes, opts)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, vol := range selected {
		got, err := searchCompactWithCache(vol.index, opts, true, make(map[int]string), nil)
		if err != nil {
			t.Fatal(err)
		}
		total += len(got)
	}
	return total
}

// deterministicParentLargeCorpus builds large volumes with no SUBT metadata
// (nothing populates idx.Derived), so parent: must resolve through the child
// map without ever touching a subtree interval.
func deterministicParentLargeCorpus(t *testing.T) []*serviceVolumeIndex {
	t.Helper()
	build := func(volume string, seed int) *serviceVolumeIndex {
		idx := &Index{
			Source:  "usn",
			Volume:  volume,
			Compact: true,
			Records: make([]CompactRecord, 0, 5000),
		}
		nextFRN := uint64(seed * 100_000)
		add := func(parent int32, parentFRN uint64, name string, mode uint32) int32 {
			nextFRN++
			idx.Records = append(idx.Records, CompactRecord{
				FRN: nextFRN, ParentFRN: parentFRN, Parent: parent, Name: name, Mode: mode,
				Size: 100, ModUnix: time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC).UnixNano(),
			})
			return int32(len(idx.Records) - 1)
		}
		rootFRN := nextFRN + 1
		idx.Records = append(idx.Records, CompactRecord{FRN: rootFRN, ParentFRN: rootFRN, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)})
		nextFRN = rootFRN
		root := int32(0)
		workspace := add(root, rootFRN, "workspace-alpha", uint32(os.ModeDir))
		workspaceFRN := idx.Records[workspace].FRN
		for i := 0; i < 2000; i++ {
			add(workspace, workspaceFRN, fmt.Sprintf("child-%04d.bin", i), 0)
		}
		for i := 0; i < 2000; i++ {
			add(root, rootFRN, fmt.Sprintf("noise-%04d.bin", i), 0)
		}
		buildOrders(idx)
		return newServiceVolumeIndex(strings.ToLower(strings.TrimSuffix(volume, ":"))+"-parent-large.gsi", idx)
	}
	return []*serviceVolumeIndex{build("C:", 1), build("F:", 2)}
}

// deterministicBroadTermCorpus builds volumes where a single gram posting is
// above the selective trigram candidate cap, so every candidate source must
// decline to the exhaustive scan.
func deterministicBroadTermCorpus(t *testing.T) []*serviceVolumeIndex {
	t.Helper()
	build := func(volume string, seed int) *serviceVolumeIndex {
		idx := &Index{
			Source:  "usn",
			Volume:  volume,
			Compact: true,
			Records: make([]CompactRecord, 0, 30_500),
		}
		nextFRN := uint64(seed * 100_000)
		add := func(parent int32, parentFRN uint64, name string, mode uint32) int32 {
			nextFRN++
			idx.Records = append(idx.Records, CompactRecord{
				FRN: nextFRN, ParentFRN: parentFRN, Parent: parent, Name: name, Mode: mode,
				Size: 100, ModUnix: time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC).UnixNano(),
			})
			return int32(len(idx.Records) - 1)
		}
		rootFRN := nextFRN + 1
		idx.Records = append(idx.Records, CompactRecord{FRN: rootFRN, ParentFRN: rootFRN, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)})
		nextFRN = rootFRN
		root := int32(0)
		broad := add(root, rootFRN, "broad", uint32(os.ModeDir))
		broadFRN := idx.Records[broad].FRN
		for i := 0; i < 30_000; i++ {
			add(broad, broadFRN, fmt.Sprintf("commonish-token-%05d.bin", i), 0)
		}
		buildOrders(idx)
		vol := newServiceVolumeIndex(strings.ToLower(strings.TrimSuffix(volume, ":"))+"-broad.gsi", idx)
		vol.rebuildNameTrigramsLocked()
		return vol
	}
	return []*serviceVolumeIndex{build("C:", 1), build("F:", 2)}
}

// deterministicRealPathCorpus builds volumes whose reconstructed paths exist on
// disk, so `exists` filtering resolves through os.Stat exactly like production.
func deterministicRealPathCorpus(t *testing.T) []*serviceVolumeIndex {
	t.Helper()
	build := func(volume string, names ...string) *serviceVolumeIndex {
		root := filepath.Join(t.TempDir(), "exists-"+strings.ToLower(strings.TrimSuffix(volume, ":")))
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		sub := filepath.Join(root, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		idx := &Index{
			Source:  "usn",
			Volume:  root,
			Compact: true,
			Records: make([]CompactRecord, 0, len(names)+2),
		}
		add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) int32 {
			idx.Records = append(idx.Records, CompactRecord{
				FRN: frn, ParentFRN: parentFRN, Parent: parent, Name: name, Mode: mode,
				Size: 1024, ModUnix: time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC).UnixNano(),
			})
			return int32(len(idx.Records) - 1)
		}
		rootRec := add(1, 1, -1, ".", uint32(os.ModeDir))
		subRec := add(2, 1, rootRec, "sub", uint32(os.ModeDir))
		frn := uint64(10)
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(sub, name), []byte("payload"), 0o644); err != nil {
				t.Fatal(err)
			}
			add(frn, 2, subRec, name, 0)
			frn++
		}
		buildOrders(idx)
		return newServiceVolumeIndex(strings.ToLower(strings.TrimSuffix(volume, ":"))+"-exists.gsi", idx)
	}
	return []*serviceVolumeIndex{build("C:", "alpha-needle.txt", "gamma-other.md"), build("F:", "beta-needle.log")}
}

// TestExtCountRecentLegacyParity is the BUG-1 regression: on a legacy engine
// (no overlay snapshot published), a live USN change is tracked only in
// vol.recentIDs.  The ext count planner must reconcile those records the same
// way extTopPosting does, so count stays in parity with an oracle volume that
// has the change folded into its base index.
func TestExtCountRecentLegacyParity(t *testing.T) {
	liveIdx, recentID := extCountRecentFixture(t, "F:", "recent-tool.bin")
	liveVol := newServiceVolumeIndex("live-ext-recent.gsi", liveIdx)
	if liveVol.recentIDs == nil {
		liveVol.recentIDs = make(map[int]struct{})
	}
	liveVol.recentIDs[recentID] = struct{}{}
	liveVol.recentSeq++

	oracleIdx, _ := extCountRecentFixture(t, "F:", "recent-tool.bin")
	oracleVol := newServiceVolumeIndex("oracle-ext-recent.gsi", oracleIdx)

	setExtTopForRecentTest(t, liveVol)
	setExtTopForRecentTest(t, oracleVol)

	other := workspaceAlphaModelVolume("C:", false)
	live := []*serviceVolumeIndex{liveVol, other}
	oracle := []*serviceVolumeIndex{oracleVol, other}

	countOpts := queryOptions{Query: "ext:bin", Limit: 100}
	liveCount, ok, err := countServiceVolumesGlobalOnly(live, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("global ext count declined")
	}
	oracleCount, ok2, err2 := countServiceVolumesGlobalOnly(oracle, countOpts)
	if err2 != nil {
		t.Fatal(err2)
	}
	if !ok2 {
		t.Fatal("oracle global ext count declined")
	}
	if want := 9; liveCount != want {
		t.Fatalf("live ext count = %d, want %d (recent record must be counted)", liveCount, want)
	}
	if liveCount != oracleCount {
		t.Fatalf("live count %d != oracle count %d", liveCount, oracleCount)
	}

	searchOpts := queryOptions{Query: "ext:bin", Limit: 5}
	liveGot, handled, err := searchServiceVolumesGlobalExtOnly(live, searchOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("global ext search declined")
	}
	oracleGot, handled, err := searchServiceVolumesGlobalExtOnly(oracle, searchOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("oracle global ext search declined")
	}
	if !sameOrderedStrings(pathsOf(liveGot), pathsOf(oracleGot)) {
		t.Fatalf("live search paths = %v, oracle = %v", pathsOf(liveGot), pathsOf(oracleGot))
	}
	if !containsString(pathsOf(liveGot), `F:\workspace\recent-tool.bin`) {
		t.Fatalf("recent record missing from live search results: %v", pathsOf(liveGot))
	}
}

func extCountRecentFixture(t *testing.T, volume, recentName string) (*Index, int) {
	t.Helper()
	idx := &Index{
		Source:  "usn",
		Volume:  volume,
		Compact: true,
		Records: make([]CompactRecord, 0, 12),
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) int32 {
		idx.Records = append(idx.Records, CompactRecord{
			FRN: frn, ParentFRN: parentFRN, Parent: parent, Name: name, Mode: mode,
			Size: 1024, ModUnix: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
		return int32(len(idx.Records) - 1)
	}
	root := add(1, 1, -1, ".", uint32(os.ModeDir))
	workspace := add(2, 1, root, "workspace", uint32(os.ModeDir))
	nextFRN := uint64(10)
	for i := 0; i < 8; i++ {
		add(nextFRN, 2, workspace, fmt.Sprintf("target-%02d.bin", i), 0)
		nextFRN++
	}
	buildOrders(idx)
	if recentName == "" {
		return idx, -1
	}
	id := idx.appendCompactRecord(CompactRecord{
		FRN: nextFRN, ParentFRN: 2, Parent: workspace, Name: recentName, Size: 1,
		ModUnix: time.Now().UnixNano(),
	})
	return idx, int(id)
}

// setExtTopForRecentTest installs a rank-aware resident extTop plus a full
// name-rank slice so both the live and oracle volumes take the persisted
// extTop branch of extTopPosting.  The appended recent record gets the best
// rank so it must surface in the top-N page.
func setExtTopForRecentTest(t *testing.T, vol *serviceVolumeIndex) {
	t.Helper()
	if vol.queryIndex == nil {
		vol.queryIndex = &residentQueryIndex{}
	}
	recordCount := vol.index.compactRecordCount()
	ranks := make([]uint32, recordCount)
	for i := 0; i < recordCount; i++ {
		ranks[i] = uint32(i + 1)
	}
	ranks[recordCount-1] = 0
	vol.queryIndex.nameRank = ranks
	vol.queryIndex.extTop = buildExtTopPostingsMin(vol.queryIndex.ext, ranks, serviceExtTopPostingLimit, 1)
}
