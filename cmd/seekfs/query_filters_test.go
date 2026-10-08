package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseQueryInfersPathModeForPathSeparators(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: `client_base_new_workspaces_since_20260619\clean_surface_dead_time_outlier_metrics_since_20260619.json`})
	if err != nil {
		t.Fatal(err)
	}
	if !pq.MatchPath {
		t.Fatal("query with path separator did not enable path matching")
	}
	wantTerms := []string{"client_base_new_workspaces_since_20260619", "clean_surface_dead_time_outlier_metrics_since_20260619.json"}
	if !sameStringSet(pq.Terms, wantTerms) {
		t.Fatalf("terms = %v, want %v", pq.Terms, wantTerms)
	}
}

func TestSearchJSONIncludesCompletionDiagnostic(t *testing.T) {
	payload, err := json.Marshal(jsonSearchResponse{
		OK:              true,
		Query:           "needle",
		Count:           1,
		PlannerMode:     "global-components",
		EligibleVolumes: []string{"C:", "F:"},
		Terms:           []traceTerm{{Term: "workspace-alpha", Kind: "path-substring", Source: "global:component-subtree"}},
		Declines:        []traceDecline{{Source: "global-ext", Reason: "missing-posting", Volume: "F:"}},
		Fallback:        "global-bounded-scan",
		Complete:        boolPtr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"complete":true`) {
		t.Fatalf("json response missing complete=true: %s", payload)
	}
	var decoded jsonSearchResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.PlannerMode != "global-components" || decoded.Fallback != "global-bounded-scan" {
		t.Fatalf("decoded route fields = %+v", decoded)
	}
	if !sameOrderedStrings(decoded.EligibleVolumes, []string{"C:", "F:"}) {
		t.Fatalf("eligible volumes = %v, want C:/F:", decoded.EligibleVolumes)
	}
	if !traceHasTerm(decoded.Terms, traceTerm{Term: "workspace-alpha", Kind: "path-substring", Source: "global:component-subtree"}) {
		t.Fatalf("terms = %+v, want component-subtree term", decoded.Terms)
	}
	if len(decoded.Declines) != 1 || decoded.Declines[0].Volume != "F:" {
		t.Fatalf("declines = %+v, want F: decline", decoded.Declines)
	}
}

func TestParseSizeFilter(t *testing.T) {
	cases := []struct {
		spec    string
		op      string
		bytes   int64
		wantErr bool
	}{
		{">100mb", ">", 100 << 20, false},
		{">=1gb", ">=", 1 << 30, false},
		{"<4k", "<", 4 << 10, false},
		{"<=512", "<=", 512, false},
		{"1024", "=", 1024, false},
		{"=2048b", "=", 2048, false},
		{"10m", ">", 0, false}, // op defaults to "=" but value parses
		{"", "", 0, true},
		{">notanumber", "", 0, true},
	}
	for _, c := range cases {
		sf, err := parseSizeFilter(c.spec)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSizeFilter(%q) expected error", c.spec)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSizeFilter(%q) unexpected error: %v", c.spec, err)
			continue
		}
		if c.spec != "10m" {
			if sf.op != c.op || sf.bytes != c.bytes {
				t.Errorf("parseSizeFilter(%q) = {%s %d}, want {%s %d}", c.spec, sf.op, sf.bytes, c.op, c.bytes)
			}
		}
	}
}

func TestSizeFilterMatches(t *testing.T) {
	sf := sizeFilter{op: ">", bytes: 100}
	if !sf.matches(101) || sf.matches(100) || sf.matches(99) {
		t.Fatal("size > 100 matched incorrectly")
	}
	sf = sizeFilter{op: "<=", bytes: 100}
	if !sf.matches(100) || !sf.matches(50) || sf.matches(101) {
		t.Fatal("size <= 100 matched incorrectly")
	}
}

func TestParseDateFilterRelativeAndMacros(t *testing.T) {
	for _, spec := range []string{"today", "yesterday", "thisweek", "lastweek", "24h", "7d", "2026-05-01"} {
		if _, err := parseDateFilter(spec); err != nil {
			t.Errorf("parseDateFilter(%q) unexpected error: %v", spec, err)
		}
	}
	if _, err := parseDateFilter("notadate"); err == nil {
		t.Error("parseDateFilter(notadate) expected error")
	}
}

func TestDateFilterMatchesToday(t *testing.T) {
	df, err := parseDateFilter("today")
	if err != nil {
		t.Fatal(err)
	}
	if !df.matches(time.Now().UnixNano()) {
		t.Fatal("dm:today did not match a record modified now")
	}
	if df.matches(time.Now().AddDate(0, 0, -2).UnixNano()) {
		t.Fatal("dm:today matched a record from two days ago")
	}
	if df.matches(0) {
		t.Fatal("dm:today matched a record with zero mtime")
	}
}

func TestUnknownFilterIsRejected(t *testing.T) {
	for _, q := range []string{"size2:>1mb", "color:red"} {
		if _, err := parseQuery(queryOptions{Query: q}); err == nil {
			t.Errorf("parseQuery(%q) should reject unknown filter, got nil error", q)
		}
	}
}

func TestAttribFilterParsing(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "attrib:HS"})
	if err != nil {
		t.Fatalf("parseQuery attrib: %v", err)
	}
	want := uint32(fileAttributeHidden | fileAttributeSystem)
	if len(pq.AttrFilters) != 1 || pq.AttrFilters[0] != want {
		t.Fatalf("AttrFilters = %#v, want %#x", pq.AttrFilters, want)
	}
	if _, err := parseQuery(queryOptions{Query: "attrib:Z"}); err == nil {
		t.Fatal("parseQuery(attrib:Z) should reject unsupported attribute flag")
	}
}

func TestSortSizeParsing(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "ext:txt sort:size"})
	if err != nil {
		t.Fatalf("parseQuery sort:size: %v", err)
	}
	if pq.SortColumn != "size" {
		t.Fatalf("SortColumn = %q, want size", pq.SortColumn)
	}
	pq, err = parseQuery(queryOptions{Query: "ext:txt sort:modified"})
	if err != nil {
		t.Fatalf("parseQuery sort:modified: %v", err)
	}
	if pq.SortColumn != "modified" {
		t.Fatalf("SortColumn = %q, want modified", pq.SortColumn)
	}
	pq, err = parseQuery(queryOptions{Query: "type:file sort:extension"})
	if err != nil {
		t.Fatalf("parseQuery sort:extension: %v", err)
	}
	if pq.SortColumn != "extension" {
		t.Fatalf("SortColumn = %q, want extension", pq.SortColumn)
	}
	pq, err = parseQuery(queryOptions{Query: "path:fixture sort:type"})
	if err != nil {
		t.Fatalf("parseQuery sort:type: %v", err)
	}
	if pq.SortColumn != "type" {
		t.Fatalf("SortColumn = %q, want type", pq.SortColumn)
	}
	pq, err = parseQuery(queryOptions{Query: "path:fixture sort:path"})
	if err != nil {
		t.Fatalf("parseQuery sort:path: %v", err)
	}
	if pq.SortColumn != "path" {
		t.Fatalf("SortColumn = %q, want path", pq.SortColumn)
	}
	if _, err := parseQuery(queryOptions{Query: "sort:size"}); err == nil {
		t.Fatal("sort:size without a searchable term/filter should be rejected")
	}
	if _, err := parseQuery(queryOptions{Query: "ext:txt sort:owner"}); err == nil {
		t.Fatal("parseQuery(sort:owner) should reject unsupported sort")
	}
}

func TestInvalidParentFilterIsRejected(t *testing.T) {
	for _, q := range []string{`parent:C:\repo`, "parent:foo/bar", "parent:foo*", "parent:foo?"} {
		if _, err := parseQuery(queryOptions{Query: q}); err == nil {
			t.Errorf("parseQuery(%q) should reject invalid parent filter, got nil error", q)
		}
	}
}

func TestKnownFiltersAndPathsAreNotRejected(t *testing.T) {
	// Drive-letter paths and supported filters must still parse.
	for _, q := range []string{`c:\windows main`, "ext:go", "parent:src", "size:>1mb", "dm:today", "attrib:H", "type:file"} {
		if _, err := parseQuery(queryOptions{Query: q, MatchPath: true}); err != nil {
			t.Errorf("parseQuery(%q) unexpectedly rejected: %v", q, err)
		}
	}
}

func TestPathFilterEnablesPathMatching(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "path:Downloads"})
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if !pq.MatchPath {
		t.Fatal("path: filter did not enable path matching")
	}
	if len(pq.Terms) != 1 || pq.Terms[0] != "downloads" {
		t.Fatalf("terms = %v, want [downloads]", pq.Terms)
	}
}

func TestGlobalBoundedScanBudgetGate(t *testing.T) {
	vol := func(records int) *serviceVolumeIndex {
		idx := &Index{Source: "usn", Volume: "C:", Compact: true, Records: make([]CompactRecord, records)}
		return newServiceVolumeIndex("budget.gsi", idx)
	}
	now := time.Now()
	if !globalBoundedScanBudgetOK(nil, parsedQuery{}, 3) {
		t.Fatal("no deadline must allow the scan")
	}
	if globalBoundedScanBudgetOK([]*serviceVolumeIndex{vol(100)}, parsedQuery{DeadlineUnix: now.Add(-time.Second).UnixNano()}, 3) {
		t.Fatal("expired deadline must decline the scan")
	}
	if !globalBoundedScanBudgetOK([]*serviceVolumeIndex{vol(100)}, parsedQuery{DeadlineUnix: now.Add(30 * time.Second).UnixNano()}, 3) {
		t.Fatal("tiny volume with generous deadline must run the scan")
	}
	if globalBoundedScanBudgetOK([]*serviceVolumeIndex{vol(40_000_000)}, parsedQuery{DeadlineUnix: now.Add(5 * time.Second).UnixNano()}, 3) {
		t.Fatal("40M records need far more than 5s; must decline early instead of blocking to the deadline")
	}
}

func TestLooseMultiTermQueryStaysNameMode(t *testing.T) {
	if queryLooksLoosePathScoped("Downloads nrrd") {
		t.Fatal("plain multi-term query must stay name mode; loose path inference would force a slow path scan")
	}
	if queryLooksLoosePathScoped("aker log") {
		t.Fatal("plain multi-term name query must stay name mode")
	}
	if queryLooksLoosePathScoped("ext:raw !path:Assets") {
		t.Fatal("negated path filters should not force top-level path mode")
	}
	if !queryLooksLoosePathScoped("path:Downloads nrrd") {
		t.Fatal("explicit path: filter must enable path mode")
	}
	if !queryLooksLoosePathScoped(`Downloads\Incoming nrrd`) {
		t.Fatal("a term with a path separator must enable path mode")
	}
}

func TestPathExtensionSyntaxMatrixMatchesAcrossSearchPaths(t *testing.T) {
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		name      string
		opts      queryOptions
		wantNames []string
	}{
		{
			name:      "path filter and explicit extension",
			opts:      queryOptions{Query: "path:Downloads ext:raw", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path filter and dotted extension",
			opts:      queryOptions{Query: "path:Downloads .raw", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "absolute path filter and dotted extension",
			opts:      queryOptions{Query: `path:C:\fixture\Downloads .raw`, Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "bare path-like term and dotted extension",
			opts:      queryOptions{Query: `C:\fixture\Downloads .raw`, MatchPath: true, Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "dir filter and dotted ext filter",
			opts:      queryOptions{Query: "dir:Downloads ext:.raw", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path filter and glob",
			opts:      queryOptions{Query: "path:Downloads glob:*.raw", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path filter and regex",
			opts:      queryOptions{Query: `path:Downloads regex:Downloads.*\.raw$`, Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path filter extension type and not",
			opts:      queryOptions{Query: "path:Downloads ext:raw type:file !draft", Limit: 20},
			wantNames: []string{"camera.raw"},
		},
		{
			name:      "path filter and extension or",
			opts:      queryOptions{Query: "path:Downloads ext:raw|nrrd", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw", "scan.nrrd"},
		},
		{
			name:      "path filter inside or alternatives",
			opts:      queryOptions{Query: "path:Downloads|path:Assets ext:raw", Limit: 20},
			wantNames: []string{"asset.raw", "camera.raw", "draft.raw"},
		},
		{
			name:      "path filter inside not",
			opts:      queryOptions{Query: "ext:raw !path:Assets", Limit: 20},
			wantNames: []string{"camera.raw", "draft.raw", "lab.raw"},
		},
		{
			name:      "middle dotted substring is not extension",
			opts:      queryOptions{Query: ".opencode", Limit: 20},
			wantNames: []string{"ai.opencode.desktop"},
		},
		{
			name:      "name mode dotted extension remains substring",
			opts:      queryOptions{Query: ".pdf", Limit: 20},
			wantNames: []string{"manual.pdf", "manual.pdf.bak"},
		},
		{
			name:      "path mode bare dotted extension is exact extension",
			opts:      queryOptions{Query: "path:.pdf", Limit: 20},
			wantNames: []string{"manual.pdf"},
		},
		{
			name:      "path term and dotted extension is exact extension",
			opts:      queryOptions{Query: "path:Reports .pdf", Limit: 20},
			wantNames: []string{"manual.pdf"},
		},
		{
			name:      "dir filter and dotted extension is exact extension",
			opts:      queryOptions{Query: "dir:Reports .pdf", Limit: 20},
			wantNames: []string{"manual.pdf"},
		},
		{
			name:      "drive scoped middle dotted substring",
			opts:      queryOptions{Query: "path:C: .opencode", Limit: 20},
			wantNames: []string{"ai.opencode.desktop", "settings.json"},
		},
		{
			name:      "explicit extension remains exact",
			opts:      queryOptions{Query: "ext:opencode", Limit: 20},
			wantNames: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, err := searchCompactWithCache(idx, tc.opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			if got := namesOf(full); !sameStringSet(got, tc.wantNames) {
				t.Fatalf("full names = %v, want %v", got, tc.wantNames)
			}
			fast, err := searchCompactWithCache(idx, tc.opts, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("candidate search: %v", err)
			}
			if got := namesOf(fast); !sameStringSet(got, tc.wantNames) {
				t.Fatalf("candidate names = %v, want %v", got, tc.wantNames)
			}
		})
	}
}

func TestStrictSpaceSplitDoesNotInferFusedPathExtensions(t *testing.T) {
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "path:C:.nrrd", Limit: 20},
		{Query: "path:C:.NRRD", Limit: 20},
		{Query: "path:Downloads.raw", Limit: 20},
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
			if len(full) != 0 || len(fast) != 0 {
				t.Fatalf("strict fused query matched full=%v fast=%v, want no inferred extension matches", namesOf(full), namesOf(fast))
			}
		})
	}
}

func TestStrictSpaceSplitTokenPermutationsMatchAcrossSearchPaths(t *testing.T) {
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		name      string
		tokens    []string
		wantNames []string
	}{
		{
			name:      "path ext type",
			tokens:    []string{"path:Downloads", "ext:raw", "type:file"},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path dotted type",
			tokens:    []string{"path:Downloads", ".raw", "type:file"},
			wantNames: []string{"camera.raw", "draft.raw"},
		},
		{
			name:      "path ext not",
			tokens:    []string{"path:Downloads", "ext:raw", "!draft"},
			wantNames: []string{"camera.raw"},
		},
		{
			name:      "path ext or",
			tokens:    []string{"path:Downloads", "ext:raw|nrrd"},
			wantNames: []string{"camera.raw", "draft.raw", "scan.nrrd"},
		},
	}
	for _, tc := range cases {
		for _, tokens := range permutations(tc.tokens) {
			query := strings.Join(tokens, " ")
			t.Run(tc.name+"/"+query, func(t *testing.T) {
				full, err := searchCompactWithCache(idx, queryOptions{Query: query, Limit: 20}, false, make(map[int]string), nil)
				if err != nil {
					t.Fatalf("full search: %v", err)
				}
				fast, err := searchCompactWithCache(idx, queryOptions{Query: query, Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
				if err != nil {
					t.Fatalf("candidate search: %v", err)
				}
				if got := namesOf(full); !sameStringSet(got, tc.wantNames) {
					t.Fatalf("full names = %v, want %v", got, tc.wantNames)
				}
				if got := namesOf(fast); !sameStringSet(got, tc.wantNames) {
					t.Fatalf("candidate names = %v, want %v", got, tc.wantNames)
				}
			})
		}
	}
}

func TestPathFilterParsingPropagatesThroughNestedSyntax(t *testing.T) {
	cases := []struct {
		query     string
		wantPath  bool
		wantTerms []string
		wantExts  []string
	}{
		{
			query:     "path:Downloads .raw",
			wantPath:  true,
			wantTerms: []string{"downloads"},
			wantExts:  []string{"raw"},
		},
		{
			query:     `path:C:\fixture\Downloads ext:.raw`,
			wantPath:  true,
			wantTerms: []string{"c:", "fixture", "downloads"},
			wantExts:  []string{"raw"},
		},
		{
			query:     "path:C:.nrrd",
			wantPath:  true,
			wantTerms: []string{"c:.nrrd"},
			wantExts:  nil,
		},
		{
			query:     "path:.nrrd",
			wantPath:  true,
			wantTerms: nil,
			wantExts:  []string{"nrrd"},
		},
		{
			query:    "path:Downloads|path:Assets ext:raw",
			wantPath: true,
			wantExts: []string{"raw"},
		},
		{
			query:    "ext:raw !path:Assets",
			wantPath: false,
			wantExts: []string{"raw"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			pq, err := parseQuery(queryOptions{Query: tc.query})
			if err != nil {
				t.Fatalf("parseQuery: %v", err)
			}
			if pq.MatchPath != tc.wantPath {
				t.Fatalf("MatchPath = %v, want %v", pq.MatchPath, tc.wantPath)
			}
			if !sameStringSet(pq.Terms, tc.wantTerms) {
				t.Fatalf("terms = %v, want %v", pq.Terms, tc.wantTerms)
			}
			if !sameStringSet(pq.Exts, tc.wantExts) {
				t.Fatalf("exts = %v, want %v", pq.Exts, tc.wantExts)
			}
		})
	}
}

func permutations(values []string) [][]string {
	out := make([][]string, 0)
	var walk func(int)
	items := append([]string(nil), values...)
	walk = func(pos int) {
		if pos == len(items) {
			out = append(out, append([]string(nil), items...))
			return
		}
		for i := pos; i < len(items); i++ {
			items[pos], items[i] = items[i], items[pos]
			walk(pos + 1)
			items[pos], items[i] = items[i], items[pos]
		}
	}
	walk(0)
	return out
}

func TestSearchCompactHonorsQueryDeadlineAndCancel(t *testing.T) {
	idx := syntheticCompactIndex(5000)
	if _, err := searchCompactWithCache(idx, queryOptions{
		Query:        "file",
		MatchPath:    true,
		Limit:        20,
		DeadlineUnix: time.Now().Add(-time.Millisecond).UnixNano(),
	}, false, make(map[int]string), nil); !errors.Is(err, errQueryCanceled) {
		t.Fatalf("deadline error = %v, want %v", err, errQueryCanceled)
	}
	if _, err := searchCompactWithCache(idx, queryOptions{
		Query:     "file",
		MatchPath: true,
		Limit:     20,
		Cancel:    func() bool { return true },
	}, false, make(map[int]string), nil); !errors.Is(err, errQueryCanceled) {
		t.Fatalf("cancel error = %v, want %v", err, errQueryCanceled)
	}
}

func TestReconstructCompactPathSkipsSyntheticDotRoot(t *testing.T) {
	idx := &Index{
		Compact: true,
		Volume:  "F:",
		Records: []CompactRecord{
			{FRN: 5, ParentFRN: 5, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 10, ParentFRN: 5, Parent: 0, Name: "git", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 1, Name: "seekfs", Mode: uint32(os.ModeDir)},
			{FRN: 12, ParentFRN: 11, Parent: 2, Name: "main.go"},
		},
	}
	if got, want := idx.reconstructCompactPath(3), `F:\git\seekfs\main.go`; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if !pathUnder(idx.reconstructCompactPath(3), `F:\git\seekfs`) {
		t.Fatal("reconstructed path should be under F:\\git\\seekfs")
	}
}

func TestUnderSearchFiltersStaleFilesystemEntriesByDefault(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "stale-indexed-path.txt")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := &Index{}
	if err := walkRoot(root, idx); err != nil {
		t.Fatal(err)
	}
	buildOrders(idx)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err := search(idx, queryOptions{Query: "stale-indexed-path.txt", Under: root, Limit: 20}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("matches = %+v, want stale indexed path filtered", got)
	}
	countMatches, err := search(idx, queryOptions{Query: "stale-indexed-path.txt", Under: root, Limit: 20}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(countMatches) != 1 {
		t.Fatalf("count matches = %+v, want indexed count behavior unchanged", countMatches)
	}
}

// TestCountUnderDivergenceFlagged pins the CountDivergent marker: like its
// content twin, it is set on potential (Under/Exists shapes, where search
// stat-filters stale entries and counts do not), never on plain counts or
// searches.
func TestCountUnderDivergenceFlagged(t *testing.T) {
	records := []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "scope", Mode: modeFromAttrs(fileAttributeDir)},
		{FRN: 3, ParentFRN: 2, Parent: 1, Name: "run_pipeline.bat", Mode: modeFromAttrs(fileAttributeArchive)},
	}
	idx := &Index{Source: "usn", Volume: "F:", Roots: []string{`F:\`}, Compact: true, Records: records}
	buildOrders(idx)
	vol := newServiceVolumeIndex("divergence.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	volumes := []*serviceVolumeIndex{vol}

	underTrace := &searchTrace{}
	n, ok, err := countServiceVolumes(volumes, queryOptions{Query: "run_pipeline", Under: `F:\scope`, Limit: 20, Trace: underTrace})
	if err != nil || !ok || n != 1 {
		t.Fatalf("under count = %d ok=%v err=%v, want 1 true nil", n, ok, err)
	}
	if !underTrace.CountDivergent {
		t.Fatal("count under scope did not set CountDivergent")
	}
	searchTr := &searchTrace{}
	if _, err := searchServiceVolumes(volumes, queryOptions{Query: "run_pipeline", Under: `F:\scope`, Limit: 20, Trace: searchTr}, false); err != nil {
		t.Fatal(err)
	}
	if searchTr.CountDivergent {
		t.Fatal("scoped search set CountDivergent; only counts diverge")
	}
	existsTrace := &searchTrace{}
	if _, _, err := countServiceVolumes(volumes, queryOptions{Query: "run_pipeline", Exists: true, Limit: 20, Trace: existsTrace}); err != nil {
		t.Fatal(err)
	}
	if !existsTrace.CountDivergent {
		t.Fatal("count with Exists did not set CountDivergent")
	}
	plainTrace := &searchTrace{}
	if _, _, err := countServiceVolumes(volumes, queryOptions{Query: "run_pipeline", Limit: 20, Trace: plainTrace}); err != nil {
		t.Fatal(err)
	}
	if plainTrace.CountDivergent {
		t.Fatal("plain count set CountDivergent without Under/Exists")
	}
}

func TestImplicitFilenameGlobQuery(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "*_test.go", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(pq.Globs) != 1 || pq.Globs[0] != "*_test.go" {
		t.Fatalf("expected implicit glob for *_test.go, got globs=%v", pq.Globs)
	}
	if len(pq.Terms) != 0 {
		t.Fatalf("expected no plain terms for implicit glob, got %v", pq.Terms)
	}
}

func TestImplicitFilenameGlobMatchesFixture(t *testing.T) {
	idx := commonSearchFixture()
	got := searchFixtureNames(t, idx, queryOptions{Query: "*_test.go", MatchPath: true, Limit: 20})
	if len(got) != 1 || got[0] != "search_test.go" {
		t.Fatalf("implicit *_test.go glob = %v, want [search_test.go]", got)
	}
}

func TestImplicitFilenameGlobMatchesFixtureByName(t *testing.T) {
	idx := commonSearchFixture()
	got := searchFixtureNames(t, idx, queryOptions{Query: "*_test.go", Limit: 20})
	if len(got) != 1 || got[0] != "search_test.go" {
		t.Fatalf("implicit *_test.go filename glob = %v, want [search_test.go]", got)
	}
}

func TestParseOrGroup(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "ext:png|jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pq.OrGroups) != 1 {
		t.Fatalf("expected 1 OR group, got %d", len(pq.OrGroups))
	}
	if len(pq.OrGroups[0]) != 2 {
		t.Fatalf("expected 2 alternatives, got %d", len(pq.OrGroups[0]))
	}
}

func TestParseNotGroup(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: "main !test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pq.NotGroups) != 1 {
		t.Fatalf("expected 1 NOT group, got %d", len(pq.NotGroups))
	}
	if len(pq.Terms) != 1 || pq.Terms[0] != "main" {
		t.Fatalf("expected term 'main', got %v", pq.Terms)
	}
}

func TestOrNotSizeOnFixture(t *testing.T) {
	idx := commonSearchFixture()

	// OR over extensions: .dat OR .txt files.
	orFiles := searchFixtureNames(t, idx, queryOptions{Query: "ext:dat|txt", MatchPath: true, Limit: 50})
	wantOr := map[string]bool{"sample.dat": true, "notes.txt": true, "sibling.dat": true}
	for _, n := range orFiles {
		if !wantOr[n] {
			t.Errorf("ext:dat|txt returned unexpected %q", n)
		}
		delete(wantOr, n)
	}
	if len(wantOr) != 0 {
		t.Errorf("ext:dat|txt missed %v", wantOr)
	}

	// NOT: .go files excluding *test*.
	goNoTest := searchFixtureNames(t, idx, queryOptions{Query: "ext:go !test", MatchPath: true, Limit: 50})
	for _, n := range goNoTest {
		if n == "search_test.go" {
			t.Errorf("ext:go !test should have excluded search_test.go")
		}
	}
	foundMain := false
	for _, n := range goNoTest {
		if n == "main.go" {
			foundMain = true
		}
	}
	if !foundMain {
		t.Error("ext:go !test should include main.go")
	}
}

func TestSizeAndModFiltersRequireCapableIndex(t *testing.T) {
	// An index with no sizes and no mtimes must reject size:/dm:/recent filters
	// rather than silently returning nothing.
	bare := &Index{Source: "usn", Volume: "C:", Compact: true}
	bare.Records = []CompactRecord{{FRN: 1, Parent: -1, Name: "C:", Mode: uint32(os.ModeDir)}}
	buildOrders(bare)

	for _, q := range []string{"size:>1mb", "dm:today", "ext:txt sort:modified"} {
		opts := queryOptions{Query: q}
		if _, err := searchCompactWithCache(bare, opts, false, make(map[int]string), nil); err == nil {
			t.Errorf("query %q on a size/mtime-less index should error", q)
		}
	}
	if _, err := searchCompactWithCache(bare, queryOptions{Query: "ext:go", Recent: "24h"}, false, make(map[int]string), nil); err == nil {
		t.Error("--recent on an mtime-less index should error")
	}
	if _, err := searchCompactWithCache(bare, queryOptions{Query: "attrib:H"}, false, make(map[int]string), nil); err == nil {
		t.Error("attrib: on an attr-less index should error")
	}

	// The standard fixture carries sizes and mtimes, so the same filters work.
	idx := commonSearchFixture()
	if !idx.compactHasSize() {
		t.Fatal("fixture should advertise size capability")
	}
	if !idx.compactHasModTime() {
		t.Fatal("fixture should advertise mtime capability")
	}
	if _, err := searchCompactWithCache(idx, queryOptions{Query: "size:>1kb"}, false, make(map[int]string), nil); err != nil {
		t.Errorf("size: on a capable index should not error: %v", err)
	}
}

func TestAttribFilterMatchesCompactRecords(t *testing.T) {
	idx := attribSearchFixture()
	vol := newServiceVolumeIndex("attrs.gsi", idx)
	cases := []struct {
		query string
		want  []string
	}{
		{"attrib:H", []string{"hidden.txt", "hidden-system.dat"}},
		{"attrib:HS", []string{"hidden-system.dat"}},
		{"ext:txt attrib:H", []string{"hidden.txt"}},
		{"!attrib:H ext:txt", []string{"plain.txt"}},
		{"attrib:D", []string{".", "attrs"}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			full, err := searchCompactWithCache(idx, queryOptions{Query: tc.query, Limit: 20}, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			fast, err := searchCompactWithCache(idx, queryOptions{Query: tc.query, Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("planner search: %v", err)
			}
			if !sameStringSet(namesOf(full), tc.want) {
				t.Fatalf("full names = %v, want %v", namesOf(full), tc.want)
			}
			if !sameStringSet(namesOf(fast), tc.want) {
				t.Fatalf("planner names = %v, want %v", namesOf(fast), tc.want)
			}
			fullCount, err := searchCompactWithCache(idx, queryOptions{Query: tc.query}, true, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("full count: %v", err)
			}
			fastCount, err := searchCompactWithCache(idx, queryOptions{Query: tc.query}, true, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatalf("planner count: %v", err)
			}
			if len(fullCount) != len(tc.want) || len(fastCount) != len(tc.want) {
				t.Fatalf("counts full=%d fast=%d want=%d", len(fullCount), len(fastCount), len(tc.want))
			}
		})
	}
}

func TestAttribFilterDoesNotPrecapBeforeVerification(t *testing.T) {
	idx := attribSearchFixture()
	vol := newServiceVolumeIndex("attrs.gsi", idx)
	got, err := searchCompactWithCache(idx, queryOptions{Query: "ext:txt attrib:H", Limit: 1}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"hidden.txt"}) {
		t.Fatalf("names = %v, want hidden.txt", names)
	}
}

func TestAttribFilterBuildsCandidateSource(t *testing.T) {
	idx := attribSearchFixture()
	vol := newServiceVolumeIndex("attrs.gsi", idx)
	pq := mustParseQuery(t, queryOptions{Query: "attrib:HS"})
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan did not handle attrib:HS")
	}
	if len(plan.sources) != 1 || plan.sources[0].name != "attrib:HS" {
		t.Fatalf("plan sources = %+v, want attrib:HS source", plan.sources)
	}
	ids := plan.execute()
	if names := namesOf(entriesForIDs(idx, ids)); !sameStringSet(names, []string{"hidden-system.dat"}) {
		t.Fatalf("candidate source names = %v, want hidden-system.dat", names)
	}
}

func TestAttribFilterRejectsNonCompactIndex(t *testing.T) {
	idx := &Index{
		Source: "walk",
		Entries: []Entry{
			{Path: `C:\plain.txt`, Name: "plain.txt", Mode: 0},
		},
		NameOrder: []int{0},
		PathOrder: []int{0},
	}
	if _, err := search(idx, queryOptions{Query: "attrib:H"}, false); err == nil {
		t.Fatal("attrib: on noncompact index should error")
	}
}

func TestAttribFilterUsesGlobalPlannerSource(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	vol := newServiceVolumeIndex("attrs.gsi", attribSearchFixture())
	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "attrib:H", Limit: 10, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s", trace.PlannerMode, trace.Decline)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"hidden.txt", "hidden-system.dat"}) {
		t.Fatalf("names = %v, want hidden attrib matches", names)
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "attrib:H", Trace: countTrace})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("countServiceVolumes did not handle attrib:H")
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
}

func TestAttribFilterUsesMappedLowmemSource(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	idx := attribSearchFixture()
	db := filepath.Join(t.TempDir(), "attrs.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("load for service: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	if len(loaded.Derived.AttrBits) == 0 {
		t.Fatal("mapped attr postings were not loaded")
	}
	vol := newServiceVolumeIndex(db, loaded)
	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "attrib:H", Limit: 10, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s", trace.PlannerMode, trace.Decline)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"hidden.txt", "hidden-system.dat"}) {
		t.Fatalf("mapped attr names = %v, want hidden attr matches", names)
	}
}

func TestAttribOrUsesGlobalPlannerSource(t *testing.T) {
	vol := newServiceVolumeIndex("attrs.gsi", attribSearchFixture())
	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "attrib:H|attrib:A", Limit: 10, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s", trace.PlannerMode, trace.Decline)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"plain.txt", "hidden.txt", "hidden-system.dat"}) {
		t.Fatalf("names = %v, want archive or hidden attr matches", names)
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "attrib:H|attrib:A", Trace: countTrace})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("countServiceVolumes declined attrib OR")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
}

func TestSortSizeRanksCompactResults(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "a-large.txt", Size: 300},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "b-small.txt", Size: 10},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "c-mid.txt", Size: 100},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("sort-size.gsi", idx)
	got, err := searchCompactWithCache(idx, queryOptions{Query: "ext:txt sort:size", Limit: 3}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"a-large.txt", "b-small.txt", "c-mid.txt"}) {
		t.Fatalf("names set = %v", names)
	}
	if got[0].Name != "b-small.txt" || got[1].Name != "c-mid.txt" || got[2].Name != "a-large.txt" {
		t.Fatalf("size order = %v, want b-small, c-mid, a-large", []string{got[0].Name, got[1].Name, got[2].Name})
	}
	limited, err := searchCompactWithCache(idx, queryOptions{Query: "ext:txt sort:size", Limit: 1}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Name != "b-small.txt" {
		t.Fatalf("limited size order = %v, want b-small.txt", namesOf(limited))
	}
}

func TestGlobalPlannerExtVerifierFiltersDefault(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
	}
	add := func(frn, parentFRN uint64, parent int32, name string, size int64, mod time.Time, mode uint32) int32 {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      size,
			ModUnix:   mod.UnixNano(),
		})
		return int32(len(idx.Records) - 1)
	}
	rootTime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	root := add(1, 1, -1, ".", 0, rootTime, uint32(os.ModeDir))
	add(2, 1, root, "small-old.txt", 10, time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC), 0)
	add(3, 1, root, "large-new.txt", 100, rootTime, 0)
	add(4, 1, root, "large-new.bin", 100, rootTime, 0)
	buildOrders(idx)
	vol := newServiceVolumeIndex("verifier-filters.gsi", idx)

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{query: "ext:txt size:>50", want: []string{"large-new.txt"}},
		{query: "ext:txt dm:2026-05-01", want: []string{"large-new.txt"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			trace := &searchTrace{}
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: tc.query, Limit: 10, Trace: trace}, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
			}
			if names := namesOf(got); !sameStringSet(names, tc.want) {
				t.Fatalf("names = %v, want %v", names, tc.want)
			}
			countTrace := &searchTrace{}
			count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: tc.query, Trace: countTrace})
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("countServiceVolumes declined verifier filter")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
			}
			if count != len(tc.want) {
				t.Fatalf("count = %d, want %d", count, len(tc.want))
			}
		})
	}
}

func TestSortModifiedRanksCompactResults(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), ModUnix: 1},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "old.txt", ModUnix: 10},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "new.txt", ModUnix: 30},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "mid.txt", ModUnix: 20},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("sort-modified.gsi", idx)
	got, err := searchCompactWithCache(idx, queryOptions{Query: "ext:txt sort:modified", Limit: 3}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "new.txt" || got[1].Name != "mid.txt" || got[2].Name != "old.txt" {
		t.Fatalf("modified order = %v, want new, mid, old", []string{got[0].Name, got[1].Name, got[2].Name})
	}
	limited, err := searchCompactWithCache(idx, queryOptions{Query: "ext:txt sort:modified", Limit: 1}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Name != "new.txt" {
		t.Fatalf("limited modified order = %v, want new.txt", namesOf(limited))
	}
}

func TestSortExtensionRanksCompactResults(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "b.go"},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "a.md"},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "c.go"},
			{FRN: 5, ParentFRN: 1, Parent: 0, Name: "z.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("sort-extension.gsi", idx)
	got, err := searchCompactWithCache(idx, queryOptions{Query: "type:file sort:extension", Limit: 4}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "b.go" || got[1].Name != "c.go" || got[2].Name != "a.md" || got[3].Name != "z.txt" {
		t.Fatalf("extension order = %v, want b.go, c.go, a.md, z.txt", namesOf(got))
	}
	limited, err := searchCompactWithCache(idx, queryOptions{Query: "type:file sort:extension", Limit: 1}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Name != "b.go" {
		t.Fatalf("limited extension order = %v, want b.go", namesOf(limited))
	}
}

func TestSortTypeRanksCompactResults(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "fixture", Mode: uint32(os.ModeDir)},
			{FRN: 3, ParentFRN: 2, Parent: 1, Name: "b.go"},
			{FRN: 4, ParentFRN: 2, Parent: 1, Name: "src", Mode: uint32(os.ModeDir)},
			{FRN: 5, ParentFRN: 2, Parent: 1, Name: "a.md"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("sort-type.gsi", idx)
	got, err := searchCompactWithCache(idx, queryOptions{Query: "path:fixture sort:type", Limit: 4}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "fixture" || got[1].Name != "src" || got[2].Name != "a.md" || got[3].Name != "b.go" {
		t.Fatalf("type order = %v, want fixture, src, a.md, b.go", namesOf(got))
	}
	limited, err := searchCompactWithCache(idx, queryOptions{Query: "path:fixture sort:type", Limit: 1}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Name != "fixture" {
		t.Fatalf("limited type order = %v, want fixture", namesOf(limited))
	}
}

func TestSortPathRanksCompactResults(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "zeta", Mode: uint32(os.ModeDir)},
			{FRN: 3, ParentFRN: 2, Parent: 1, Name: "a.txt"},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "alpha", Mode: uint32(os.ModeDir)},
			{FRN: 5, ParentFRN: 4, Parent: 3, Name: "z.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("sort-path.gsi", idx)
	got, err := searchCompactWithCache(idx, queryOptions{Query: "path:txt sort:path", Limit: 2}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Path != `C:\alpha\z.txt` || got[1].Path != `C:\zeta\a.txt` {
		t.Fatalf("path order = %v, want alpha before zeta", pathsOf(got))
	}
	limited, err := searchCompactWithCache(idx, queryOptions{Query: "path:txt sort:path", Limit: 1}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Path != `C:\alpha\z.txt` {
		t.Fatalf("limited path order = %v, want C:\\alpha\\z.txt", pathsOf(limited))
	}
}

func TestPlannedCountFastPathMatchesFullForFilters(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "ext:dat"},
		{Query: "ext:dat|txt"},
		{Query: "type:file ext:go"},
		{Query: "ext:go !test"},
		{Query: "size:>=0"}, // every record qualifies
	}
	for _, opts := range cases {
		t.Run(opts.Query, func(t *testing.T) {
			pq, err := parseQuery(opts)
			if err != nil {
				t.Fatal(err)
			}
			// Confirm these queries take the no-path fast path.
			if queryNeedsPath(pq) {
				t.Fatalf("query %q unexpectedly needs path reconstruction", opts.Query)
			}
			got, ok := vol.plannedCount(pq)
			if !ok {
				// Some pure-filter queries decline the planner; fall back to full.
				full, ferr := searchCompactWithCache(idx, opts, true, make(map[int]string), nil)
				if ferr != nil {
					t.Fatal(ferr)
				}
				if len(full) == 0 {
					t.Skipf("query %q produced no candidates and declined planner", opts.Query)
				}
				return
			}
			full, err := searchCompactWithCache(idx, opts, true, make(map[int]string), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != len(full) {
				t.Fatalf("planned count = %d, full count = %d for %q", got, len(full), opts.Query)
			}
		})
	}
}

// searchFixtureNames runs a query against the fixture through the resident
// volume planner path and returns matched names.
func searchFixtureNames(t *testing.T, idx *Index, opts queryOptions) []string {
	t.Helper()
	entries, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("search %q: %v", opts.Query, err)
	}
	return namesOf(entries)
}

func pathSyntaxFixture() *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      1024,
			ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
	}
	add(1, 1, -1, ".", uint32(os.ModeDir))
	add(2, 1, 0, "fixture", uint32(os.ModeDir))
	add(3, 2, 1, "Downloads", uint32(os.ModeDir))
	add(4, 3, 2, "camera.raw", 0)
	add(5, 3, 2, "scan.nrrd", 0)
	add(6, 3, 2, "draft.raw", 0)
	add(7, 2, 1, "Assets", uint32(os.ModeDir))
	add(8, 7, 6, "asset.raw", 0)
	add(9, 2, 1, "Lab", uint32(os.ModeDir))
	add(10, 9, 8, "lab.raw", 0)
	add(11, 2, 1, "ai.opencode.desktop", uint32(os.ModeDir))
	add(12, 11, 10, "settings.json", 0)
	add(13, 2, 1, "Reports", uint32(os.ModeDir))
	add(14, 13, 12, "manual.pdf", 0)
	add(15, 13, 12, "manual.pdf.bak", 0)
	buildOrders(idx)
	return idx
}

func attribSearchFixture() *Index {
	idx := &Index{
		Source:       "usn",
		Volume:       "C:",
		Compact:      true,
		CompactAttrs: true,
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      1024,
			ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
	}
	add(1, 1, -1, ".", modeFromAttrs(fileAttributeDir))
	add(2, 1, 0, "attrs", modeFromAttrs(fileAttributeDir))
	add(3, 2, 1, "plain.txt", modeFromAttrs(fileAttributeArchive))
	add(4, 2, 1, "hidden.txt", modeFromAttrs(fileAttributeHidden|fileAttributeArchive))
	add(5, 2, 1, "hidden-system.dat", modeFromAttrs(fileAttributeHidden|fileAttributeSystem|fileAttributeArchive))
	buildOrders(idx)
	return idx
}
