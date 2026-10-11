package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestGlobalRecordIDOrdering(t *testing.T) {
	cases := []struct {
		a, b globalRecordID
		want int
	}{
		{globalRecordID{volume: 0, local: 1}, globalRecordID{volume: 0, local: 1}, 0},
		{globalRecordID{volume: 0, local: 2}, globalRecordID{volume: 0, local: 3}, -1},
		{globalRecordID{volume: 0, local: 4}, globalRecordID{volume: 0, local: 3}, 1},
		{globalRecordID{volume: 0, local: 99}, globalRecordID{volume: 1, local: 0}, -1},
		{globalRecordID{volume: 2, local: 0}, globalRecordID{volume: 1, local: 99}, 1},
	}
	for _, tc := range cases {
		if got := compareGlobalRecordID(tc.a, tc.b); got != tc.want {
			t.Fatalf("compareGlobalRecordID(%+v, %+v) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestGlobalPostingIteratorDecodesMappedBlocksLazily(t *testing.T) {
	t.Setenv("SEEKFS_POSTING_CACHE_MB", "1")
	servicePostingBlockCache = postingBlockLRU{}
	t.Cleanup(func() { servicePostingBlockCache = postingBlockLRU{} })

	ids := make([]uint32, 2050)
	for i := range ids {
		ids[i] = uint32(i)
	}
	section := decodePostingSection(encodeStringPostingSection(map[string][]uint32{"txt": ids}, nil))
	posting, count, ok := section.stringPostingIterator("txt")
	if !ok || count != len(ids) {
		t.Fatalf("posting iterator = (%v, %d), want (true, %d)", ok, count, len(ids))
	}
	it := newGlobalPostingIterator(1, postingCountCandidate{it: posting, count: count, mapped: true})
	if got := len(servicePostingBlockCache.items); got != 0 {
		t.Fatalf("constructor decoded %d blocks, want 0", got)
	}
	if got, ok := it.Next(); !ok || got != (globalRecordID{volume: 1, local: 0}) {
		t.Fatalf("first id = (%+v, %v), want volume 1 local 0", got, ok)
	}
	if got := len(servicePostingBlockCache.items); got != 1 {
		t.Fatalf("first Next decoded %d blocks, want 1", got)
	}

	servicePostingBlockCache = postingBlockLRU{}
	posting, count, _ = section.stringPostingIterator("txt")
	it = newGlobalPostingIterator(1, postingCountCandidate{it: posting, count: count, mapped: true})
	got, ok := it.SeekGE(globalRecordID{volume: 1, local: 2048})
	if !ok || got != (globalRecordID{volume: 1, local: 2048}) {
		t.Fatalf("SeekGE = (%+v, %v), want volume 1 local 2048", got, ok)
	}
	if got := len(servicePostingBlockCache.items); got != 1 {
		t.Fatalf("SeekGE decoded %d blocks, want only the target block", got)
	}
	if got := it.CountHint(); got != 1 {
		t.Fatalf("CountHint after SeekGE = %d, want 1", got)
	}
}

func TestGlobalSubtreeIteratorNestedRootsDedupesAndOrders(t *testing.T) {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Records: []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "workspace", Mode: uint32(os.ModeDir)},
		{FRN: 3, ParentFRN: 2, Parent: 1, Name: "nested", Mode: uint32(os.ModeDir)},
		{FRN: 4, ParentFRN: 3, Parent: 2, Name: "model_v2.txt"},
		{FRN: 5, ParentFRN: 1, Parent: 0, Name: "sibling.txt"},
	}}
	buildOrders(idx)
	vol := newServiceVolumeIndex("nested-subtree.gsi", idx)
	it := newGlobalSubtreeScanIterator(0, vol, []int{1, 2})
	got := collectGlobalIterator(it, 0)
	want := []globalRecordID{{volume: 0, local: 1}, {volume: 0, local: 2}, {volume: 0, local: 3}}
	if !slices.Equal(got, want) {
		t.Fatalf("nested subtree IDs = %v, want %v", got, want)
	}
	it = newGlobalSubtreeScanIterator(0, vol, []int{1})
	if got, ok := it.SeekGE(globalRecordID{volume: 0, local: 3}); !ok || got.local != 3 {
		t.Fatalf("subtree SeekGE = (%+v, %v), want local 3", got, ok)
	}
}

func TestGlobalSubtreeIntervalIteratorUsesSubtreeMetadata(t *testing.T) {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Records: []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "workspace", Mode: uint32(os.ModeDir)},
		{FRN: 3, ParentFRN: 2, Parent: 1, Name: "nested", Mode: uint32(os.ModeDir)},
		{FRN: 4, ParentFRN: 3, Parent: 2, Name: "model_v2.txt"},
		{FRN: 5, ParentFRN: 1, Parent: 0, Name: "sibling.txt"},
	}}
	buildOrders(idx)
	vol := newServiceVolumeIndex("interval-subtree.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	it := newGlobalSubtreeIterator(0, vol, []int{1, 2})
	got := collectGlobalIterator(it, 0)
	want := []globalRecordID{{volume: 0, local: 1}, {volume: 0, local: 2}, {volume: 0, local: 3}}
	if !slices.Equal(got, want) {
		t.Fatalf("interval subtree IDs = %v, want %v", got, want)
	}
	if _, ok := it.SeekGE(globalRecordID{volume: 0, local: 2}); ok {
		t.Fatal("exhausted interval iterator unexpectedly returned an ID")
	}
}

func TestMappedComponentCoverageMergesNestedRootsAndHiddenSelfHits(t *testing.T) {
	idx := &Index{
		Version: indexVersion, Source: "usn", Volume: "C:", Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "needle", Mode: uint32(os.ModeDir)},
			{FRN: 3, ParentFRN: 2, Parent: 1, Name: "needle", Mode: uint32(os.ModeDir)},
			{FRN: 4, ParentFRN: 3, Parent: 2, Name: "inside.txt"},
			{FRN: 5, ParentFRN: 2, Parent: 1, Name: "other.txt"},
			{FRN: 6, ParentFRN: 1, Parent: 0, Name: "needle-report.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("component-coverage.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	vol.queryIndex = &residentQueryIndex{components: map[string][]uint32{"needle": {1, 2}}}

	coverage, ok := vol.buildMappedComponentCoverage("needle", []int{3, 5})
	if !ok {
		t.Fatal("mapped component coverage declined")
	}
	if coverage.rootCount != 2 || len(coverage.intervals) != 1 {
		t.Fatalf("coverage roots/intervals = %d/%d, want 2/1", coverage.rootCount, len(coverage.intervals))
	}
	if coverage.cardinality != 5 {
		t.Fatalf("coverage cardinality = %d, want 5 (nested roots deduped and one file self-hit)", coverage.cardinality)
	}
	count, verified := coverage.countLive(vol, func(id int) bool { return id == 3 })
	if count != 4 || verified != 5 {
		t.Fatalf("hidden count/verified = %d/%d, want 4/5", count, verified)
	}
}

func TestMappedComponentCoverageIncludesPartialNameRoots(t *testing.T) {
	idx := &Index{
		Version: indexVersion, Source: "usn", Volume: "C:", Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "content", Mode: uint32(os.ModeDir)},
			{FRN: 3, ParentFRN: 2, Parent: 1, Name: "a.txt"},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "main_content", Mode: uint32(os.ModeDir)},
			{FRN: 5, ParentFRN: 4, Parent: 3, Name: "b.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("component-partial.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	vol.queryIndex = &residentQueryIndex{components: map[string][]uint32{"content": {1}, "main_content": {3}}}

	coverage, ok := vol.buildMappedComponentCoverage("content", nil)
	if !ok {
		t.Fatal("mapped component coverage declined")
	}
	// content, a.txt, main_content, b.txt: "content" is a substring of every
	// path under main_content, so its subtree belongs to the coverage.
	if coverage.rootCount != 2 || coverage.cardinality != 4 {
		t.Fatalf("coverage roots/cardinality = %d/%d, want 2/4", coverage.rootCount, coverage.cardinality)
	}
	if !coverage.contains(vol, 4) {
		t.Fatal("main_content\\b.txt missing from content coverage")
	}
}

func TestMappedComponentSubstringCoverageUsesCompletePCMPDictionary(t *testing.T) {
	idx := &Index{
		Version: indexVersion, Source: "usn", Volume: "C:", Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "Windows", Mode: uint32(os.ModeDir)},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "Windows.old", Mode: uint32(os.ModeDir)},
			{FRN: 4, ParentFRN: 2, Parent: 1, Name: "inside.txt"},
			{FRN: 5, ParentFRN: 3, Parent: 2, Name: "old.txt"},
			{FRN: 6, ParentFRN: 1, Parent: 0, Name: "WindowsReport.txt"},
			{FRN: 7, ParentFRN: 1, Parent: 0, Name: "UsersBackup", Mode: uint32(os.ModeDir)},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("component-substring.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	vol.index.Derived.Postings = map[uint32]mappedPostingSection{
		indexSectionPCMP: {Data: encodeStringPostingSection(map[string][]uint32{
			"windows":     {1},
			"windows.old": {2},
			"usersbackup": {6},
		}, nil)},
	}
	coverage, ok := vol.mappedComponentSubstringCoverage("windows")
	if !ok {
		t.Fatal("substring PCMP coverage declined")
	}
	if coverage.rootCount != 2 || coverage.cardinality != 5 {
		t.Fatalf("substring coverage roots/cardinality = %d/%d, want 2/5", coverage.rootCount, coverage.cardinality)
	}
	if len(coverage.selfIDs) != 1 || coverage.selfIDs[0] != 5 {
		t.Fatalf("substring self hits = %v, want [5]", coverage.selfIDs)
	}
}

func TestCompleteLowerNameTermScanMatchesPackedOracle(t *testing.T) {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Records: []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "Users", Mode: uint32(os.ModeDir)},
		{FRN: 3, ParentFRN: 1, Parent: 0, Name: "UsersBackup.txt"},
		{FRN: 4, ParentFRN: 1, Parent: 0, Name: "unrelated.txt"},
	}}
	buildOrders(idx)
	idx.packCompactRecords(true)
	got, ok := idx.scanCompactLowerNameTerm("users")
	if !ok {
		t.Fatal("packed lower-name scan declined")
	}
	want := make([]int, 0, len(got))
	for id := 0; id < idx.compactRecordCount(); id++ {
		if strings.Contains(idx.compactLowerNameAt(id), "users") && !idx.compactRecord(id).Deleted {
			want = append(want, id)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("packed lower-name scan = %v, want %v", got, want)
	}
}

// TestR5HolisticPlannerGeneratedCoverageMatrix keeps the component and boolean
// planner work honest across the dimensions called out by the R5 exit gate.
// This is deliberately a selected cross-product: it exercises every family
// and limit/sort pair while keeping the exhaustive oracle bounded and the
// seed stable for reproducible failures.
func TestR5HolisticPlannerGeneratedCoverageMatrix(t *testing.T) {
	const seed int64 = 0x5eedf5
	rng := rand.New(rand.NewSource(seed))
	terms := []string{"workspace", "Users", "src", "nrrd", "pdf", "raw", "x", "zzzz-nohit"}
	altTerms := []string{"needle", "backup", "dataset", "report"}
	families := []string{"bare", "implicit-path", "explicit-path", "dir", "ext", "scalar", "date", "parent", "attrib", "glob", "regex", "or", "not", "under"}
	sorts := []string{"", "sort:path", "sort:size", "sort:modified", "sort:extension", "sort:type"}
	limits := []int{1, 20, 100}
	volumeStates := []string{"single", "both", "absent", "early-miss-later-hit", "mixed-v8", "missing-derived"}
	resultStates := []string{"self-hit", "directory-root", "nested-root", "tie"}
	overlayStates := []string{"none", "create", "hide", "delete", "rename"}
	pairCounts := make(map[string]int)
	cases := make([]struct {
		family, volume, result, overlay, sort string
		query, under                          string
		matchPath                             bool
		limit                                 int
	}, 0, 48)
	for i := 0; i < 48; i++ {
		family := families[i%len(families)]
		term := terms[rng.Intn(len(terms))]
		alt := altTerms[rng.Intn(len(altTerms))]
		query := term
		matchPath := false
		under := ""
		switch family {
		case "implicit-path":
			matchPath = true
		case "explicit-path":
			query, matchPath = "path:"+term, true
		case "dir":
			query, matchPath = "dir:"+term, true
		case "ext":
			query = "ext:" + map[string]string{"nrrd": "nrrd", "pdf": "pdf", "raw": "raw"}[term]
			if strings.HasSuffix(query, ":") {
				query = "ext:nrrd"
			}
		case "scalar":
			query = "size:>=0 " + term
		case "date":
			query = "dm:2026-01-10 " + term
		case "parent":
			query = "parent:workspace"
		case "attrib":
			query = "type:file " + term
		case "glob":
			query = "glob:*" + term + "*"
		case "regex":
			query = "regex:.*" + regexpSafeLiteral(term) + ".*"
		case "or":
			query, matchPath = term+"|"+alt, true
		case "not":
			query, matchPath = term+" !backup", true
		case "under":
			query, matchPath, under = term, true, `C:\workspace`
		}
		sortToken := sorts[rng.Intn(len(sorts))]
		if sortToken != "" {
			query += " " + sortToken
		}
		volumeState := volumeStates[rng.Intn(len(volumeStates))]
		resultState := resultStates[rng.Intn(len(resultStates))]
		overlayState := overlayStates[rng.Intn(len(overlayStates))]
		limit := limits[rng.Intn(len(limits))]
		cases = append(cases, struct {
			family, volume, result, overlay, sort string
			query, under                          string
			matchPath                             bool
			limit                                 int
		}{family, volumeState, resultState, overlayState, sortToken, query, under, matchPath, limit})
		pairCounts[family+"/"+sortToken]++
	}

	for i, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%02d-%s-%s-%s", i, tc.family, tc.volume, tc.overlay), func(t *testing.T) {
			baseC := randomCorpusIndex(seed+int64(i), randomCorpusParams{Records: 96})
			baseC.Source, baseC.Volume, baseC.Roots = "usn", "C:", []string{`C:\`}
			baseF := cloneCompactIndex(baseC)
			baseF.Volume, baseF.Roots = "F:", []string{`F:\`}
			if tc.volume == "mixed-v8" {
				baseF.Version = indexVersion
				baseF.Derived = indexDerivedSections{}
			}
			if tc.volume == "missing-derived" {
				baseF.Version = indexVersion
				baseF.Derived = indexDerivedSections{}
			}
			volC := newServiceVolumeIndex(fmt.Sprintf("r5-matrix-c-%d.gsi", i), baseC)
			volF := newServiceVolumeIndex(fmt.Sprintf("r5-matrix-f-%d.gsi", i), baseF)
			volumes := []*serviceVolumeIndex{volC, volF}
			switch tc.volume {
			case "single", "absent":
				volumes = []*serviceVolumeIndex{volC}
			case "early-miss-later-hit":
				volumes = []*serviceVolumeIndex{volF, volC}
			}
			query := tc.query
			under := tc.under
			if tc.volume == "absent" {
				query = "path:F: " + query
				// Keep the absent-volume case orthogonal to an explicit C:-root
				// under constraint; that combination is intentionally covered by
				// the separate under/volume adversarial tests.
				under = ""
			}
			opts := queryOptions{Query: query, MatchPath: tc.matchPath, Under: under, Limit: tc.limit}
			want, err := r5ExhaustivePlannerOracle(volumes, opts, false)
			if err != nil {
				t.Fatalf("exhaustive search oracle: %v", err)
			}
			trace := &searchTrace{}
			fast, err := searchServiceVolumes(volumes, queryOptions{Query: query, MatchPath: tc.matchPath, Under: under, Limit: tc.limit, Trace: trace}, false)
			if err != nil {
				t.Fatalf("planned search: %v trace=%+v", err, *trace)
			}
			if got, expected := pathsOf(fast), pathsOf(want); !sameOrderedStrings(got, expected) {
				t.Fatalf("search parity query=%q family=%s result=%s sort=%s got=%v want=%v trace=%+v", query, tc.family, tc.result, tc.sort, got, expected, *trace)
			}
			wantCount, err := r5ExhaustivePlannerOracle(volumes, opts, true)
			if err != nil {
				t.Fatalf("exhaustive count oracle: %v", err)
			}
			gotCount, err := searchServiceVolumes(volumes, queryOptions{Query: query, MatchPath: tc.matchPath, Under: under, Limit: tc.limit, Trace: &searchTrace{}}, true)
			if err != nil {
				t.Fatalf("planned count: %v", err)
			}
			if len(gotCount) != len(wantCount) {
				t.Fatalf("count parity query=%q family=%s got=%d want=%d", query, tc.family, len(gotCount), len(wantCount))
			}
			if tc.volume == "absent" && (trace.Candidates != 0 || trace.BlocksDecoded != 0) {
				t.Fatalf("absent volume did work: candidates=%d decoded=%d skipped=%d trace=%+v", trace.Candidates, trace.BlocksDecoded, trace.BlocksSkipped, *trace)
			}
		})
	}
	pairs := make([]string, 0, len(pairCounts))
	for pair, count := range pairCounts {
		pairs = append(pairs, fmt.Sprintf("%s=%d", pair, count))
	}
	sort.Strings(pairs)
	t.Logf("R5 matrix seed=%d cases=%d dimension-pairs=%s; oracle=exhaustive search/count, absent-volume zero-work asserted", seed, len(cases), strings.Join(pairs, ","))
}

func r5ExhaustivePlannerOracle(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, error) {
	selected, err := serviceVolumesForQuery(volumes, opts)
	if err != nil {
		return nil, err
	}
	selected = prioritizeServiceVolumesForPathTerms(selected, opts)
	out := make([]Entry, 0)
	for _, vol := range selected {
		child := opts
		if !countOnly {
			child.Limit = max(normalizedLimit(opts.Limit, false), vol.index.compactRecordCount())
		}
		got, err := searchCompactWithCache(vol.index, child, countOnly, make(map[int]string), nil)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	if !countOnly && len(selected) > 1 {
		pq, err := parseQuery(opts)
		if err != nil {
			return nil, err
		}
		sortSearchAllEntries(out, pq)
	}
	if !countOnly {
		limit := normalizedLimit(opts.Limit, false)
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
	}
	return out, nil
}

func TestR5GeneratedOverlayCoverageMatrix(t *testing.T) {
	const seed int64 = 0x0a11ce
	states := []struct {
		name    string
		changes []usnChange
	}{
		{name: "none"},
		{name: "create", changes: []usnChange{{FRN: 301, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "matrix-needle.txt"}}},
		{name: "hide", changes: []usnChange{{FRN: 302, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "matrix-hidden.txt", Attr: fileAttributeHidden}}},
		{name: "delete", changes: []usnChange{{FRN: 101, USN: 12, Reason: usnReasonFileDelete}}},
		{name: "rename", changes: []usnChange{{FRN: 101, ParentFRN: 100, USN: 13, Reason: usnReasonRenameOld, Name: "needle-base.txt"}, {FRN: 101, ParentFRN: 100, USN: 14, Reason: usnReasonRenameNew, Name: "renamed-away.txt"}}},
	}
	queries := []queryOptions{
		{Query: "needle", MatchPath: true, Limit: 1},
		{Query: "path:base-parent needle", Limit: 20},
		{Query: "needle|base", MatchPath: true, Limit: 100},
		{Query: "ext:txt sort:path", Limit: 20},
		{Query: "type:file needle", MatchPath: true, Limit: 20},
		{Query: "path:F: needle|base", MatchPath: true, Limit: 20},
		{Query: "path:F: needle !base", MatchPath: true, Limit: 20},
	}
	caseCount := 0
	for _, state := range states {
		state := state
		t.Run(state.name, func(t *testing.T) {
			vol := engineOverlaySearchTestVolume(t)
			logical := make(map[uint64]CompactRecord, vol.index.compactRecordCount())
			for i := 0; i < vol.index.compactRecordCount(); i++ {
				rec := vol.index.compactRecord(i)
				logical[rec.FRN] = rec
			}
			if len(state.changes) > 0 {
				vol.applyUSNChanges(state.changes)
				applyLogicalUSNChanges(logical, state.changes)
			}
			fresh := freshIndexFromLogicalRecords("F:", logical)
			fresh.Roots = []string{`F:\`}
			freshVol := newServiceVolumeIndex(fmt.Sprintf("r5-overlay-oracle-%d.gsi", seed+int64(len(state.name))), fresh)
			for _, baseOpts := range queries {
				caseCount++
				opts := baseOpts
				want, err := r5ExhaustivePlannerOracle([]*serviceVolumeIndex{freshVol}, opts, false)
				if err != nil {
					t.Fatalf("state=%s query=%q oracle search: %v", state.name, opts.Query, err)
				}
				got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
				if err != nil {
					t.Fatalf("state=%s query=%q overlay search: %v", state.name, opts.Query, err)
				}
				if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !sameOrderedStrings(gotPaths, wantPaths) {
					t.Fatalf("state=%s query=%q paths=%v want=%v fresh-volume=%q fresh-records=%d fresh-state=%q", state.name, opts.Query, gotPaths, wantPaths, fresh.Volume, len(fresh.Records), freshVol.state)
				}
				wantCount, err := r5ExhaustivePlannerOracle([]*serviceVolumeIndex{freshVol}, opts, true)
				if err != nil {
					t.Fatalf("state=%s query=%q oracle count: %v", state.name, opts.Query, err)
				}
				gotCount, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, true)
				if err != nil {
					t.Fatalf("state=%s query=%q overlay count: %v", state.name, opts.Query, err)
				}
				if len(gotCount) != len(wantCount) {
					t.Fatalf("state=%s query=%q count=%d want=%d", state.name, opts.Query, len(gotCount), len(wantCount))
				}
			}
		})
	}
	t.Logf("R5 overlay matrix seed=%d cases=%d states=none/create/hide/delete/rename; volume+overlay+boolean triples included; oracle=clean rebuilt index search/count", seed, caseCount)
}

// TestR5RequiredAdversarialCoverageMatrix makes the exit-gate dimensions that
// are easy to miss explicit.  It uses one stable fixture and checks both
// search ordering and exact count against the exhaustive oracle for every
// case, including the v8 fallback volume.
func TestR5RequiredAdversarialCoverageMatrix(t *testing.T) {
	const seed int64 = 0x71e5eed
	baseC := randomCorpusIndex(seed, randomCorpusParams{Records: 144})
	baseC.Source, baseC.Volume, baseC.Roots, baseC.CompactAttrs = "usn", "C:", []string{`C:\`}, true
	for i := range baseC.Records {
		if baseC.Records[i].Mode&uint32(os.ModeDir) == 0 && i%9 == 0 {
			baseC.Records[i].Mode = modeFromAttrs(fileAttributeHidden | fileAttributeArchive)
		}
		if i >= 8 && i <= 11 {
			baseC.Records[i].Size = 4096
			baseC.Records[i].ModUnix = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UnixNano()
		}
	}
	buildOrders(baseC)
	baseF := cloneCompactIndex(baseC)
	baseF.Volume, baseF.Roots = "F:", []string{`F:\`}
	baseF.Version, baseF.Derived = indexVersion, indexDerivedSections{}
	volC := newServiceVolumeIndex("r5-adversarial-c.gsi", baseC)
	volF := newServiceVolumeIndex("r5-adversarial-f-v8.gsi", baseF)

	cases := []struct {
		name, query, volume, under string
		matchPath                  bool
		limit                      int
	}{
		{"attrib-hidden", "attrib:H", "C", "", false, 20},
		{"exact-name", "workspace", "C", "", true, 1},
		{"substring-superset", "work", "C", "", true, 20},
		{"common-term", "a", "C", "", true, 100},
		{"rare-term", "Ã¼ber", "C", "", true, 20},
		{"short-term", "x", "C", "", true, 1},
		{"dotted-term", ".leading-dot", "C", "", true, 20},
		{"case-varied", "WORKSPACE", "C", "", true, 20},
		{"no-hit", "zzzz-nohit", "C", "", true, 20},
		{"separator-derived", `path:workspace\src`, "C", "", true, 20},
		{"file-self-hit", "path:README.md", "C", "", true, 20},
		{"directory-self-hit", "path:workspace", "C", "", true, 20},
		{"nested-overlap", "path:workspace|path:src", "C", "", true, 100},
		{"under-root", "workspace", "C", `C:\workspace`, true, 20},
		{"size-ascending-ties", "type:file sort:size", "C", "", true, 20},
		{"modified-descending-ties", "type:file sort:modified", "C", "", true, 20},
		{"path-order", "path:workspace sort:path", "both", "", true, 20},
		{"type-order", "path:workspace sort:type", "both", "", true, 20},
		{"extension-order", "ext:nrrd sort:extension", "both", "", true, 20},
		{"v8-fallback", "nrrd sort:size", "F", "", true, 20},
		{"mixed-metadata-ties", "nrrd sort:modified", "both", "", true, 100},
		{"absent-volume", "path:F: .pdf", "C", "", true, 20},
		{"volume-boolean", "path:F: nrrd|raw", "F", "", true, 20},
		{"volume-not", "path:F: nrrd !backup", "F", "", true, 20},
	}
	pairs := make(map[string]struct{})
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{volC}
			switch tc.volume {
			case "F":
				volumes = []*serviceVolumeIndex{volF}
			case "both":
				volumes = []*serviceVolumeIndex{volC, volF}
			}
			opts := queryOptions{Query: tc.query, MatchPath: tc.matchPath, Under: tc.under, Limit: tc.limit}
			want, err := r5ExhaustivePlannerOracle(volumes, opts, false)
			if err != nil {
				t.Fatalf("oracle search: %v", err)
			}
			trace := &searchTrace{}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: tc.query, MatchPath: tc.matchPath, Under: tc.under, Limit: tc.limit, Trace: trace}, false)
			if err != nil {
				t.Fatalf("planned search: %v trace=%+v", err, *trace)
			}
			if !sameOrderedStrings(pathsOf(got), pathsOf(want)) {
				t.Fatalf("search parity got=%v want=%v trace=%+v", pathsOf(got), pathsOf(want), *trace)
			}
			wantCount, err := r5ExhaustivePlannerOracle(volumes, opts, true)
			if err != nil {
				t.Fatalf("oracle count: %v", err)
			}
			gotCount, err := searchServiceVolumes(volumes, queryOptions{Query: tc.query, MatchPath: tc.matchPath, Under: tc.under, Limit: tc.limit, Trace: &searchTrace{}}, true)
			if err != nil {
				t.Fatalf("planned count: %v", err)
			}
			if len(gotCount) != len(wantCount) {
				t.Fatalf("count parity got=%d want=%d", len(gotCount), len(wantCount))
			}
			if tc.name == "absent-volume" && (trace.Candidates != 0 || trace.BlocksDecoded != 0) {
				t.Fatalf("absent volume did work: trace=%+v", *trace)
			}
			pairs[tc.name+"/"+tc.volume] = struct{}{}
		})
	}
	keys := make([]string, 0, len(pairs))
	for pair := range pairs {
		keys = append(keys, pair)
	}
	sort.Strings(keys)
	t.Logf("R5 required adversarial matrix seed=%d cases=%d dimension-pairs=%s; search/count exhaustive parity, zero-work absent volume, global tie order asserted", seed, len(cases), strings.Join(keys, ","))
}

func TestMappedComponentTopUsesDescendantRankBounds(t *testing.T) {
	const roots = 2048
	idx := &Index{
		Version: indexVersion, Source: "usn", Volume: "C:", Compact: true,
		Roots: []string{`C:\`}, Records: make([]CompactRecord, 0, 1+roots*3),
	}
	idx.Records = append(idx.Records, CompactRecord{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)})
	for i := 0; i < roots; i++ {
		bucketID := len(idx.Records)
		bucketFRN := uint64(bucketID + 1)
		idx.Records = append(idx.Records, CompactRecord{
			FRN: bucketFRN, ParentFRN: 1, Parent: 0,
			Name: fmt.Sprintf("bucket-%04d", i), Mode: uint32(os.ModeDir),
		})
		workspaceID := len(idx.Records)
		workspaceFRN := uint64(workspaceID + 1)
		idx.Records = append(idx.Records, CompactRecord{
			FRN: workspaceFRN, ParentFRN: bucketFRN, Parent: int32(bucketID),
			Name: "workspace", Mode: uint32(os.ModeDir),
		})
		childName := fmt.Sprintf("z-%04d.txt", i)
		if i < roots/2 {
			childName = fmt.Sprintf("a-%04d.txt", i)
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN: uint64(len(idx.Records) + 1), ParentFRN: workspaceFRN, Parent: int32(workspaceID),
			Name: childName, Size: int64(i + 1), ModUnix: int64(i + 1),
		})
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "component-top.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save index: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load mapped index: %v", err)
	}
	if loaded.MMapRecords == nil {
		t.Fatal("expected mapped records")
	}
	t.Cleanup(func() { _ = loaded.MMapRecords.file.close() })
	vol := newServiceVolumeIndex(db, loaded)
	for _, sortColumn := range []string{"", "path", "size", "modified", "extension", "type"} {
		query := "path:workspace"
		if sortColumn != "" {
			query += " sort:" + sortColumn
		}
		opts := queryOptions{Query: query, Limit: 1}
		var want []Entry
		if sortColumn == "" {
			want, err = searchCompactWithCache(loaded, opts, false, make(map[int]string), nil)
		} else {
			want, err = searchAll([]*Index{loaded}, opts, false)
		}
		if err != nil {
			t.Fatalf("%s oracle search: %v", sortColumn, err)
		}
		trace := &searchTrace{}
		opts.Trace = trace
		got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
		if err != nil {
			t.Fatalf("%s mapped component search: %v", sortColumn, err)
		}
		if !slices.Equal(pathsOf(got), pathsOf(want)) {
			t.Fatalf("%s paths = %v, want %v", sortColumn, pathsOf(got), pathsOf(want))
		}
		if trace.Source != "global:component-top" {
			t.Fatalf("%s trace = %+v, want component-top", sortColumn, trace)
		}
		if trace.ComponentDriver == "persisted-order-pngc-self" {
			if trace.ComponentRecordsVerified == 0 {
				t.Fatalf("%s complete self-name route did no bounded verification: %+v", sortColumn, trace)
			}
		} else if trace.BlocksSkipped == 0 {
			t.Fatalf("%s trace = %+v, want component-top with skipped blocks or complete self-name route", sortColumn, trace)
		}
	}
}
