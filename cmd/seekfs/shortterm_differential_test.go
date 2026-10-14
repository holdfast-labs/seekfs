package main

// Differential exactness for short/broad name terms on direct-built indexes.
//
// Every lane that uses a posting/iterator as a required source must either
// be exact or verify: these tests prove search/count == brute-force filtering
// over all records on a tiny direct index, for short/broad terms (1-3 chars,
// common substrings, selective and no-hit terms) crossed with type:/ext:/dir:,
// --exists, --under, git:, path, sort, OR/NOT and page shapes. The direct
// index is built in-test (see shortterm_lowr_test.go for the corpus); the
// volume loads through loadConfiguredVolume exactly like the service.

import (
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

func openShortTermVolume(t *testing.T, path string) (*Index, *serviceVolumeIndex) {
	t.Helper()
	idx, vol, err := loadConfiguredVolume(path)
	if err != nil {
		t.Fatalf("loadConfiguredVolume: %v", err)
	}
	t.Cleanup(func() {
		if idx.MMapRecords != nil {
			_ = idx.MMapRecords.file.close()
		}
	})
	return idx, vol
}

// shortTermDifferentialQueries covers short/broad terms (1-3 chars, common
// substrings, selective and no-hit terms) crossed with the late-verified and
// structural filters. Every entry carries Limit 0 (page exactly fills, so no
// fuzzy backfill) except the small-page shapes, which assert page fullness;
// zero-oracle single-term queries assert count == 0 with only within-tau
// fuzzy extras.
func shortTermDifferentialQueries(root string) []queryOptions {
	dir0 := filepath.Join(root, "dir0")
	return []queryOptions{
		{Query: "go", Limit: 100},
		{Query: "o", Limit: 100},
		{Query: "g", Limit: 100},
		{Query: "og", Limit: 100},
		{Query: "car", Limit: 100},
		{Query: "argo", Limit: 100},
		{Query: "cargo", Limit: 100},
		{Query: "main.cc", Limit: 100},
		{Query: "readme", Limit: 100},
		{Query: "zzz-no-hit", Limit: 100},
		{Query: "go type:file", Limit: 100},
		{Query: "go type:dir", Limit: 100},
		{Query: "cargo type:file", Limit: 100},
		{Query: "cargo type:dir", Limit: 100},
		{Query: "go ext:js", Limit: 100},
		{Query: "go", Limit: 100, Exists: true},
		{Query: "go type:file", Limit: 100, Exists: true},
		{Query: "cargo", Limit: 100, Exists: true},
		{Query: "main.cc", Limit: 100, Exists: true},
		{Query: "go", Limit: 100, Under: root},
		{Query: "go", Limit: 100, Under: dir0},
		{Query: "go type:file", Limit: 100, Under: root},
		{Query: "go git:clean", Limit: 100},
		{Query: "go git:tracked", Limit: 100},
		{Query: "go git:untracked", Limit: 100},
		{Query: "go git:modified", Limit: 100},
		{Query: "argo git:modified", Limit: 100},
		{Query: "cargo git:clean", Limit: 100},
		{Query: "go", Limit: 100, MatchPath: true},
		{Query: "cargo", Limit: 100, MatchPath: true},
		{Query: "readme", Limit: 100, MatchPath: true},
		{Query: "cargo json", Limit: 100, MatchPath: true},
		{Query: "go mod", Limit: 100, MatchPath: true},
		{Query: "dir0 go", Limit: 100, MatchPath: true},
		{Query: "go sort:size", Limit: 100},
		{Query: "go sort:path", Limit: 100},
		{Query: "cargo sort:modified", Limit: 100},
		{Query: "go|readme", Limit: 100},
		{Query: "go !cargo", Limit: 100},
		{Query: "argo json", Limit: 100},
		{Query: "parent:dir0", Limit: 100},
		{Query: "cargo dir:dir1", Limit: 100},
		{Query: "glob:*car*", Limit: 100},
	}
}

// shortTermFuzzyLegit reports whether name is a legitimate fuzzy append for a
// zero-result single-term query: within the term's own tau window.
func shortTermFuzzyLegit(term, name string) bool {
	if strings.Contains(strings.ToLower(name), term) {
		return false
	}
	if utf8.RuneCountInString(term) < fuzzyMinTermRunes {
		return false
	}
	tau := fuzzyThresholdForTerm(term)
	var sc fuzzyDistanceScratch
	termRunes := []rune(term)
	_, _, _, ok := fuzzyNameDistanceScratchKnown(strings.ToLower(name), termRunes, asciiOnlyString(term), asciiOnlyString(strings.ToLower(name)), tau, &sc)
	return ok
}

func TestShortTermPostingDifferential(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	root := t.TempDir()
	modifiedRel := filepath.Join("dir0", "go")
	untrackedRel := filepath.Join("dir3", "ongoing.log")
	out := buildShortTermDirectFixture(t, root, modifiedRel, untrackedRel)
	idx, vol := openShortTermVolume(t, out)
	volumes := []*serviceVolumeIndex{vol}

	for _, opts := range shortTermDifferentialQueries(root) {
		t.Run(opts.Query+existsUnderSuffix(opts), func(t *testing.T) {
			oracle, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("oracle search: %v", err)
			}
			countOpts := opts
			countOpts.Limit = 0
			countOpts.Trace = &searchTrace{}
			count, handled, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatalf("service count: %v", err)
			}
			if !handled {
				// Mirror serviceCommandSearch: a declined fast count falls
				// back to the search path in count mode.
				fallback, err := searchServiceVolumes(volumes, countOpts, true)
				if err != nil {
					t.Fatalf("service count fallback: %v", err)
				}
				count, handled = len(fallback), true
			}
			if count != len(oracle) {
				t.Fatalf("count = %d, oracle = %d (%v)", count, len(oracle), pathsOf(oracle))
			}
			searchOpts := opts
			if len(oracle) > 0 {
				searchOpts.Limit = len(oracle)
			}
			searchOpts.Trace = &searchTrace{}
			got, err := searchServiceVolumes(volumes, searchOpts, false)
			if err != nil {
				t.Fatalf("service search: %v", err)
			}
			if len(oracle) == 0 {
				for _, entry := range got {
					pq, err := parseQuery(opts)
					if err != nil {
						t.Fatal(err)
					}
					if len(pq.Terms) == 1 && shortTermFuzzyLegit(pq.Terms[0], entry.Name) {
						continue
					}
					t.Fatalf("zero-oracle query %q returned non-typo %q", opts.Query, entry.Path)
				}
				return
			}
			if !sameStringSet(pathsOf(got), pathsOf(oracle)) {
				t.Fatalf("search = %v, oracle = %v", pathsOf(got), pathsOf(oracle))
			}
		})
	}

	// Small pages must fill exactly (no truncation before verification) and
	// stay within the oracle set.
	for _, tc := range []queryOptions{
		{Query: "go", Limit: 2},
		{Query: "go type:file", Limit: 3},
		{Query: "cargo", Limit: 1},
	} {
		t.Run("page/"+tc.Query, func(t *testing.T) {
			oracle, err := searchCompactWithCache(idx, tc, false, make(map[int]string), nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = oracle
			fullOracle, err := searchCompactWithCache(idx, queryOptions{Query: tc.Query, MatchPath: tc.MatchPath, Exists: tc.Exists, Under: tc.Under, Limit: 1000}, false, make(map[int]string), nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := searchServiceVolumes(volumes, tc, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != min(tc.Limit, len(fullOracle)) {
				t.Fatalf("page len = %d, want %d (oracle %v)", len(got), min(tc.Limit, len(fullOracle)), pathsOf(fullOracle))
			}
			wantSet := make(map[string]struct{}, len(fullOracle))
			for _, e := range fullOracle {
				wantSet[e.Path] = struct{}{}
			}
			for _, e := range got {
				if _, ok := wantSet[e.Path]; !ok {
					t.Fatalf("page hit %q outside oracle %v", e.Path, pathsOf(fullOracle))
				}
			}
		})
	}
}

func existsUnderSuffix(opts queryOptions) string {
	s := ""
	if opts.Exists {
		s += "+exists"
	}
	if opts.Under != "" {
		s += "+under"
	}
	if opts.MatchPath {
		s += "+path"
	}
	return s
}

// TestSelectiveCandidateIDsDeclinesDroppedPostings pins the capped-posting
// rule: a gram that is counted but has no stored posting (a dropped common
// posting) must decline, never report an exact empty. Otherwise the
// unbounded name posting (maxIDs 0) serves a false empty as a required
// source and every planned lane underfills.
func TestSelectiveCandidateIDsDeclinesDroppedPostings(t *testing.T) {
	idx := commonSearchFixture()
	ti := buildNameTrigramIndex(idx)
	// "dat" hits sample.dat, sibling.dat and archive.tar.gz: common enough
	// to drop at cap 1, real enough to matter.
	ti.dropCommonPostings(1)
	if ti.hasStoredPosting(trigramKey("dat")) {
		t.Fatal("dat posting survived the drop; the test cap is wrong")
	}
	if ids, ok, _ := ti.selectiveCandidateIDs("dat", 0); ok {
		t.Fatalf("selectiveCandidateIDs(dat, 0) = (%v, true); want decline for a dropped posting", ids)
	}
	// End to end: the name posting must fall back to the exact scan, not the
	// false empty.
	vol := newServiceVolumeIndex("dropped.gsi", idx)
	vol.nameTrigrams.Store(ti)
	got := vol.nameTermPosting("dat")
	want := vol.scanNameTermPosting("dat")
	if len(want) == 0 {
		t.Fatal("fixture has no dat matches; the test corpus is wrong")
	}
	sort.Ints(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nameTermPosting(dat) = %v, want exact %v", got, want)
	}
}
