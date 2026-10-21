package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestRealIndexGeneratedPathSubstringMatrix(t *testing.T) {
	if !envBool("SEEKFS_REAL_INDEX_LOAD_MATRIX") {
		t.Skip("set SEEKFS_REAL_INDEX_LOAD_MATRIX=1 plus SEEKFS_REAL_INDEX_DBS or bench/real-indexes.local.txt to load real indexes in-process")
	}
	dbs := realIndexTestDBs(t)
	if len(dbs) == 0 {
		t.Skip("set SEEKFS_REAL_INDEX_DBS or create bench/real-indexes.local.txt to run real-index substring matrix")
	}
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	volumes := make([]*serviceVolumeIndex, 0, len(dbs))
	for _, db := range dbs {
		idx, err := loadIndexForService(db)
		if err != nil {
			t.Fatalf("load %s: %v", db, err)
		}
		vol := newServiceVolumeIndex(db, idx)
		if idx.Derived.Postings == nil || len(idx.Derived.NameOrder) == 0 || len(idx.Derived.NameRank) == 0 {
			vol.queryIndex = buildResidentQueryIndex(vol)
		}
		vol.resetNameTrigrams()
		if vol.needsCompactChildrenBuild() {
			vol.buildCompactChildren()
		}
		if vol.nameTrigramIndex() == nil {
			vol.rebuildNameTrigramsLocked()
		}
		volumes = append(volumes, vol)
	}
	queries := realIndexGeneratedPathQueries(volumes)
	if len(queries) == 0 {
		t.Fatal("no real-index queries generated")
	}
	if len(queries) > 96 {
		queries = queries[:96]
	}
	type slowQuery struct {
		query      string
		source     string
		decline    string
		candidates int
		ms         float64
		err        string
	}
	var slow []slowQuery
	for _, query := range queries {
		trace := &searchTrace{}
		opts := queryOptions{
			Query:        query,
			MatchPath:    true,
			Limit:        5,
			Trace:        trace,
			DeadlineUnix: time.Now().Add(750 * time.Millisecond).UnixNano(),
		}
		start := time.Now()
		_, err := searchServiceVolumes(volumes, opts, false)
		ms := float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			slow = append(slow, slowQuery{query: query, source: trace.Source, decline: trace.Decline, candidates: trace.Candidates, ms: ms, err: err.Error()})
			continue
		}
		if ms > 250 {
			slow = append(slow, slowQuery{query: query, source: trace.Source, decline: trace.Decline, candidates: trace.Candidates, ms: ms})
		}
	}
	if len(slow) > 0 {
		sort.Slice(slow, func(i, j int) bool { return slow[i].ms > slow[j].ms })
		limit := min(len(slow), 20)
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("real-index substring matrix had %d slow queries >250ms; slowest=%+v", len(slow), slow[:limit])
		}
		t.Logf("real-index substring matrix had %d slow queries over soft 250ms budget; slowest=%+v", len(slow), slow[:limit])
	}
}

func TestRealServiceGeneratedPathSubstringMatrix(t *testing.T) {
	if !envBool("SEEKFS_REAL_SERVICE_MATRIX") {
		t.Skip("set SEEKFS_REAL_SERVICE_MATRIX=1 to run against the resident service")
	}
	info, err := callService(defaultServicePipe, serviceRequest{Command: "info"})
	if err != nil {
		t.Fatalf("service info: %v", err)
	}
	if len(info.DBs) == 0 {
		t.Fatal("resident service has no loaded indexes")
	}
	queries := realServiceSeedQueries(info)
	queries = appendUniqueQueries(queries, realServiceSampledQueries(t, queries)...)
	if len(queries) > 128 {
		queries = queries[:128]
	}
	type slowQuery struct {
		query      string
		source     string
		decline    string
		candidates int
		searchMS   float64
		wallMS     float64
		err        string
	}
	var slow []slowQuery
	for i, query := range queries {
		opts := queryOptions{
			Query:        query,
			MatchPath:    true,
			Limit:        5,
			DeadlineUnix: time.Now().Add(750 * time.Millisecond).UnixNano(),
			RequestSeq:   time.Now().UnixNano() + int64(i),
		}
		start := time.Now()
		resp, err := benchServiceQuery(defaultServicePipe, opts)
		wallMS := float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			slow = append(slow, slowQuery{query: query, wallMS: wallMS, err: err.Error()})
			continue
		}
		if !resp.OK {
			slow = append(slow, slowQuery{query: query, source: resp.Source, decline: resp.Decline, candidates: resp.Candidates, searchMS: resp.SearchMS, wallMS: wallMS, err: resp.Message})
			continue
		}
		if wallMS > 250 || resp.SearchMS > 250 {
			slow = append(slow, slowQuery{query: query, source: resp.Source, decline: resp.Decline, candidates: resp.Candidates, searchMS: resp.SearchMS, wallMS: wallMS})
		}
	}
	if len(slow) > 0 {
		sort.Slice(slow, func(i, j int) bool { return slow[i].wallMS > slow[j].wallMS })
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("real-service substring matrix had %d slow/failing queries >250ms; slowest=%+v", len(slow), slow[:min(len(slow), 20)])
		}
		t.Logf("real-service substring matrix had %d slow/failing queries over soft 250ms budget; slowest=%+v", len(slow), slow[:min(len(slow), 20)])
	}
	t.Logf("checked %d resident-service real-index path substring queries", len(queries))
}

func realServiceSeedQueries(info serviceResponse) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, 128)
	add := func(query string) {
		if _, ok := seen[query]; ok {
			return
		}
		seen[query] = struct{}{}
		out = append(out, query)
	}
	for _, query := range []string{
		"Downloads docx",
		"fixtureproj",
		"F: fixtureproj",
		"path:F: fixtureproj",
		"Downloads raw",
		"Downloads nrrd",
		"path:C: raw",
		"path:C: docx",
		"path:F: pdf",
		"raw path:C:",
		"docx path:C:",
		"pdf path:F:",
		"path:Downloads ext:docx",
		"path:Downloads ext:raw",
		"path:Downloads ext:nrrd",
		"path:C: ext:raw",
		"path:C: ext:docx",
		"path:F: ext:pdf",
		"trainingdata Dataset nrrd",
		"Dataset trainingdata nrrd",
		"nrrd Dataset trainingdata",
		"path:trainingdata Dataset .nrrd",
		"path:Dataset trainingdata .nrrd",
	} {
		add(query)
	}
	terms := []string{"raw", "nrrd", "pdf", "pvsm", "json", "docx", "opencode", "downloads", "users", "appdata", "trainingdata"}
	for _, db := range info.DBs {
		volume := db.Volume
		if volume == "" {
			continue
		}
		for _, term := range terms {
			add(fmt.Sprintf("path:%s %s", volume, term))
			if realIndexTermLooksExtension(term) {
				add(fmt.Sprintf("path:%s .%s", volume, term))
			}
		}
	}
	return out
}

func realServiceSampledQueries(t *testing.T, seeds []string) []string {
	t.Helper()
	seen := make(map[string]struct{})
	var out []string
	add := func(query string) {
		query = strings.Join(strings.Fields(query), " ")
		if query == "" {
			return
		}
		if _, ok := seen[query]; ok {
			return
		}
		seen[query] = struct{}{}
		out = append(out, query)
	}
	for i, seed := range seeds {
		resp, err := benchServiceQuery(defaultServicePipe, queryOptions{
			Query:        seed,
			MatchPath:    true,
			Limit:        12,
			DeadlineUnix: time.Now().Add(750 * time.Millisecond).UnixNano(),
			RequestSeq:   time.Now().UnixNano() + int64(i),
		})
		if err != nil || !resp.OK {
			continue
		}
		for _, path := range resp.Results {
			terms := pathTermsForRealIndexQuery(path)
			if len(terms) < 2 {
				continue
			}
			volume := volumePrefixFromPath(path)
			for n := 2; n <= min(5, len(terms)); n++ {
				for start := 0; start+n <= len(terms) && start < 3; start++ {
					window := append([]string(nil), terms[start:start+n]...)
					add(strings.Join(window, " "))
					if volume != "" {
						add(fmt.Sprintf("path:%s %s", volume, strings.Join(window, " ")))
					}
					rev := append([]string(nil), window...)
					for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
						rev[i], rev[j] = rev[j], rev[i]
					}
					add(strings.Join(rev, " "))
					if volume != "" {
						add(fmt.Sprintf("path:%s %s", volume, strings.Join(rev, " ")))
					}
					if ext := extensionTermFromQueryTerms(window); ext != "" {
						add(fmt.Sprintf("path:%s ext:%s", window[0], ext))
						if volume != "" {
							add(fmt.Sprintf("path:%s %s ext:%s", volume, window[0], ext))
						}
					}
				}
			}
		}
	}
	return out
}

func appendUniqueQueries(base []string, more ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(more))
	out := make([]string, 0, len(base)+len(more))
	for _, query := range append(append([]string(nil), base...), more...) {
		query = strings.Join(strings.Fields(query), " ")
		if query == "" {
			continue
		}
		if _, ok := seen[query]; ok {
			continue
		}
		seen[query] = struct{}{}
		out = append(out, query)
	}
	return out
}

func volumePrefixFromPath(path string) string {
	if len(path) >= 2 && path[1] == ':' {
		return strings.ToUpper(path[:2])
	}
	return ""
}

func extensionTermFromQueryTerms(terms []string) string {
	for i := len(terms) - 1; i >= 0; i-- {
		term := strings.TrimPrefix(terms[i], ".")
		if realIndexTermLooksExtension(term) {
			return term
		}
	}
	return ""
}

func realIndexTestDBs(t *testing.T) []string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("SEEKFS_REAL_INDEX_DBS"))
	var paths []string
	if raw != "" {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == '\n' || r == '\r' }) {
			if path := strings.TrimSpace(part); path != "" {
				paths = append(paths, path)
			}
		}
	}
	if len(paths) == 0 {
		path := filepath.Join("..", "..", "bench", "real-indexes.local.txt")
		data, err := os.ReadFile(path)
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				paths = append(paths, line)
			}
		}
	}
	out := paths[:0]
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		} else {
			t.Logf("skipping missing real index %s: %v", path, err)
		}
	}
	return out
}

func realIndexGeneratedPathQueries(volumes []*serviceVolumeIndex) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, 256)
	add := func(query string) {
		query = strings.TrimSpace(query)
		if query == "" {
			return
		}
		if _, ok := seen[query]; ok {
			return
		}
		seen[query] = struct{}{}
		out = append(out, query)
	}
	commonSuffixes := []string{"raw", "nrrd", "pdf", "pvsm", "json", "docx", "opencode", "downloads", "users", "appdata"}
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			continue
		}
		volume := vol.index.Volume
		for _, term := range commonSuffixes {
			add(fmt.Sprintf("path:%s %s", volume, term))
			if realIndexTermLooksExtension(term) {
				add(fmt.Sprintf("path:%s .%s", volume, term))
			}
		}
		for _, terms := range sampleRealPathTerms(vol, 24) {
			if len(terms) == 0 {
				continue
			}
			add(fmt.Sprintf("path:%s %s", volume, strings.Join(terms, " ")))
			if len(terms) >= 2 {
				rev := append([]string(nil), terms...)
				for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
					rev[i], rev[j] = rev[j], rev[i]
				}
				add(fmt.Sprintf("path:%s %s", volume, strings.Join(rev, " ")))
				add(strings.Join(terms, " "))
			}
		}
	}
	return out
}

func realIndexTermLooksExtension(term string) bool {
	if len(term) < 2 || len(term) > 8 {
		return false
	}
	switch term {
	case "raw", "nrrd", "pdf", "pvsm", "json", "docx":
		return true
	default:
		return false
	}
}

func sampleRealPathTerms(vol *serviceVolumeIndex, limit int) [][]string {
	recordCount := vol.index.compactRecordCount()
	if recordCount == 0 || limit <= 0 {
		return nil
	}
	step := max(1, recordCount/limit)
	out := make([][]string, 0, limit)
	for id := 0; id < recordCount && len(out) < limit; id += step {
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Name == "" || rec.Name == "." {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, vol.pathCache)
		terms := pathTermsForRealIndexQuery(path)
		if len(terms) > 0 {
			out = append(out, terms)
		}
	}
	return out
}

func pathTermsForRealIndexQuery(path string) []string {
	fields := strings.FieldsFunc(strings.ToLower(path), func(r rune) bool {
		return r == '\\' || r == '/' || r == ':' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, 5)
	for _, field := range fields {
		field = strings.Trim(field, "._-()[]{}")
		if len(field) < 3 || len(field) > 32 {
			continue
		}
		if strings.IndexFunc(field, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
		}) >= 0 {
			continue
		}
		out = append(out, field)
		if ext := strings.TrimPrefix(filepath.Ext(field), "."); len(ext) >= 2 && len(ext) <= 8 {
			out = append(out, ext)
		}
		if len(out) >= 5 {
			break
		}
	}
	return out
}

type randomCorpusParams struct {
	Records int
}

func TestSeededQueryOracleMatrix(t *testing.T) {
	seeds := []int64{
		1, 2, 3, 4, 5, 6, 7, 8,
		11, 13, 17, 19, 23, 29, 31, 37,
		41, 43, 47, 53, 59, 61, 67, 71,
		73, 79, 83, 89, 97, 101, 103, 107,
	}
	if raw := strings.TrimSpace(os.Getenv("SEEKFS_QUERY_MATRIX_SEED")); raw != "" {
		var seed int64
		if _, err := fmt.Sscanf(raw, "%d", &seed); err != nil {
			t.Fatalf("invalid SEEKFS_QUERY_MATRIX_SEED %q: %v", raw, err)
		}
		seeds = []int64{seed}
	} else if !testSoakEnabled() {
		// Seeds guard against corpus luck; the first 8 still drive every
		// query x variant shape. The full 32 run in the soak tier (CI)
		// with the same assertions.
		seeds = seeds[:8]
		t.Logf("oracle seeds truncated to 8 (SEEKFS_TEST_SOAK=1 runs all 32 in CI)")
	}
	for _, seed := range seeds {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			idx := randomCorpusIndex(seed, randomCorpusParams{Records: 240})
			variants := []struct {
				name  string
				setup func(*serviceVolumeIndex)
			}{
				{name: "resident", setup: func(vol *serviceVolumeIndex) {
					vol.rebuildNameTrigramsLocked()
				}},
				{name: "rebuilt", setup: func(vol *serviceVolumeIndex) {
					vol.rebuildNameTrigramsLocked()
					vol.resetNameTrigrams()
					vol.rebuildNameTrigramsLocked()
				}},
				{name: "no-query-index", setup: func(vol *serviceVolumeIndex) {
					vol.queryIndex = nil
				}},
				{name: "no-child-ranges", setup: func(vol *serviceVolumeIndex) {
					vol.children = nil
					vol.childOffsets = nil
					vol.childIDs = nil
					vol.subtreeStart = nil
					vol.subtreeEnd = nil
					vol.subtreeOrder = nil
				}},
			}
			queries := randomQueries(seed, idx)
			for _, variant := range variants {
				variant := variant
				t.Run(variant.name, func(t *testing.T) {
					// One volume per (seed, variant): the old code rebuilt the
					// volume AND re-ran the variant setup (including the double
					// trigram rebuild of "rebuilt") for every query x countOnly
					// arm, which dominated the runtime. The asserts only read
					// vol.pathCache/vol.nameTermCandidates as deterministic
					// memo caches, so reusing one setup volume across the
					// sequential queries compares the same arms with the same
					// assertions; the "no-query-index" variant still starts
					// from a nil query index, so its cold path runs exactly
					// as before (once, on the first query).
					vol := newServiceVolumeIndex(fmt.Sprintf("seed-%d-%s.gsi", seed, variant.name), idx)
					variant.setup(vol)
					for i, opts := range queries {
						opts := opts
						for _, countOnly := range []bool{false, i%7 == 0} {
							assertCompactOracleParity(t, seed, idx, vol, opts, countOnly)
							assertQueryMetamorphicProperties(t, seed, idx, vol, opts)
						}
					}
				})
			}
		})
	}
}

func TestSecondOracleSeededSubsample(t *testing.T) {
	for _, seed := range []int64{1, 6, 17, 31} {
		idx := randomCorpusIndex(seed, randomCorpusParams{Records: 96})
		queries := randomQueries(seed, idx)
		if len(queries) > 18 {
			queries = queries[:18]
		}
		for _, opts := range queries {
			opts := opts
			t.Run(fmt.Sprintf("seed-%d/%s", seed, opts.Query), func(t *testing.T) {
				if _, err := parseQuery(opts); err != nil {
					t.Skipf("generated invalid query: %v", err)
				}
				full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
				if err != nil {
					t.Fatalf("compact oracle: %v", err)
				}
				naive, err := secondOracleSearch(idx, opts)
				if err != nil {
					t.Fatalf("second oracle: %v", err)
				}
				if !sameOrderedStrings(pathsOf(full), pathsOf(naive)) {
					t.Fatalf("second oracle paths=%v compact=%v", pathsOf(naive), pathsOf(full))
				}
			})
		}
	}
}

func TestParserTokenProperties(t *testing.T) {
	tokens := []string{
		"workspace", "README.md", "Übersicht", "資料", "x", "xy",
		"ext:go", "ext:pdf|txt", "dir:src", "glob:*.go", "regex:.*go.*",
		"type:file", "type:dir", "case:", "!backup", "go|pdf", "path:src",
		"C:", "F", "bad:filter",
	}
	for seed := int64(1); seed <= 128; seed++ {
		rng := rand.New(rand.NewSource(seed))
		fields := make([]string, 0, 5)
		for i := 0; i < 1+rng.Intn(5); i++ {
			fields = append(fields, tokens[rng.Intn(len(tokens))])
		}
		opts := queryOptions{
			Query:     strings.Join(fields, " "),
			MatchPath: rng.Intn(2) == 0,
			Limit:     25,
		}
		pq, err := parseQueryNoPanic(t, opts)
		if err != nil {
			continue
		}
		if len(pq.Regexps) > 0 || len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 ||
			len(pq.NotGroups) > 0 || (pq.CaseSensitive && len(pq.OrGroups) > 0) {
			continue
		}
		rendered := renderParsedQueryForTest(pq)
		if rendered == "" {
			continue
		}
		round, err := parseQueryNoPanic(t, queryOptions{Query: rendered, MatchPath: pq.MatchPath, Limit: 25})
		if err != nil {
			t.Fatalf("seed=%d rendered query %q did not parse: %v", seed, rendered, err)
		}
		if got, want := parsedQuerySignature(round), parsedQuerySignature(pq); got != want {
			t.Fatalf("seed=%d query=%q rendered=%q signature=%q want=%q", seed, opts.Query, rendered, got, want)
		}
	}
}

func TestBroadPathScanCancellationPropagates(t *testing.T) {
	idx := dottedPathBenchmarkIndex(2000)
	vol := newServiceVolumeIndex("cancel-broad.gsi", idx)
	opts := queryOptions{
		Query:        "plain",
		MatchPath:    true,
		Limit:        20,
		DeadlineUnix: time.Now().Add(-time.Second).UnixNano(),
	}
	_, err := searchCompactWithCache(idx, opts, false, vol.pathCache, func(pq parsedQuery) ([]int, bool) {
		return vol.broadPathScanCandidates(pq)
	})
	if err != errQueryCanceled {
		t.Fatalf("broad scan cancellation err = %v, want %v", err, errQueryCanceled)
	}
}

func FuzzQueryOracleParity(f *testing.F) {
	f.Add(int64(1), "workspace")
	f.Add(int64(6), "Übersicht")
	f.Add(int64(17), "ext:pdf")
	f.Add(int64(29), "path:src type:file")
	f.Fuzz(func(t *testing.T, seed int64, queryBytes string) {
		idx := randomCorpusIndex(seed, randomCorpusParams{Records: 96})
		queries := randomQueries(seed, idx)
		if query := fuzzQueryString(queryBytes); query != "" {
			queries = append(queries, queryOptions{
				Query:     query,
				MatchPath: len(query)%2 == 0 || queryLooksPathScoped(query),
				Limit:     []int{1, 2, 25, 10000}[len(query)%4],
			})
		}
		vol := newServiceVolumeIndex(fmt.Sprintf("fuzz-%d.gsi", seed), idx)
		vol.rebuildNameTrigramsLocked()
		for _, opts := range queries {
			if _, err := parseQuery(opts); err != nil {
				continue
			}
			assertCompactOracleParity(t, seed, idx, vol, opts, false)
			if opts.Limit != 1 {
				countOpts := opts
				countOpts.Limit = 0
				assertCompactOracleParity(t, seed, idx, vol, countOpts, true)
			}
		}
	})
}

func randomCorpusIndex(seed int64, params randomCorpusParams) *Index {
	if params.Records <= 0 {
		params.Records = 200
	}
	rng := rand.New(rand.NewSource(seed))
	idx := &Index{
		Source:  "random",
		Volume:  "C:",
		Compact: true,
		Records: make([]CompactRecord, 0, params.Records+32),
	}
	nextFRN := uint64(1)
	add := func(parent int32, name string, mode uint32) int32 {
		parentFRN := nextFRN
		if parent >= 0 && int(parent) < len(idx.Records) {
			parentFRN = idx.Records[parent].FRN
		}
		rec := CompactRecord{
			FRN:       nextFRN,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      int64(rng.Intn(1_000_000)),
			ModUnix:   time.Date(2026, time.January, 1+rng.Intn(28), rng.Intn(24), 0, 0, 0, time.UTC).UnixNano(),
		}
		if rng.Intn(11) == 0 {
			rec.Size = 0
		}
		if rng.Intn(13) == 0 {
			rec.ModUnix = 0
		}
		idx.Records = append(idx.Records, rec)
		nextFRN++
		return int32(len(idx.Records) - 1)
	}
	root := add(-1, ".", uint32(os.ModeDir))
	workspace := add(root, "workspace", uint32(os.ModeDir))
	users := add(root, "Users", uint32(os.ModeDir))
	user := add(users, "exampleuser", uint32(os.ModeDir))
	downloads := add(user, "Downloads", uint32(os.ModeDir))
	src := add(workspace, "src", uint32(os.ModeDir))
	docs := add(workspace, "docs", uint32(os.ModeDir))
	unicodeDir := add(workspace, "Übersicht", uint32(os.ModeDir))
	parents := []int32{workspace, users, user, downloads, src, docs, unicodeDir}
	fixed := []struct {
		parent int32
		name   string
		mode   uint32
	}{
		{src, "main.go", 0},
		{src, "MAIN_test.go", 0},
		{docs, "README.md", 0},
		{docs, "Project Specification - v1.2.docx", 0},
		{unicodeDir, "Übersicht.pdf", 0},
		{unicodeDir, "ДОКУМЕНТ.txt", 0},
		{unicodeDir, "資料.csv", 0},
		{downloads, "a.b.c.d", 0},
		{downloads, ".leading-dot", 0},
		{downloads, "trailing-dot.", 0},
		{downloads, "glob-[set]-star*.txt", 0},
		{workspace, "ext", uint32(os.ModeDir)},
		{workspace, "path", uint32(os.ModeDir)},
		{workspace, "c:", uint32(os.ModeDir)},
		{workspace, "x", 0},
		{workspace, "xy", 0},
	}
	for _, item := range fixed {
		id := add(item.parent, item.name, item.mode)
		if item.mode&uint32(os.ModeDir) != 0 {
			parents = append(parents, id)
		}
	}
	stems := []string{"alpha", "Beta", "cache", "dataset", "needle", "report", "scan", "backup", "tmp", "control", "über", "東京"}
	exts := []string{"txt", "go", "pdf", "nrrd", "raw", "json", "md", "csv", "bin"}
	for len(idx.Records) < params.Records {
		parent := parents[rng.Intn(len(parents))]
		if rng.Intn(8) == 0 {
			dir := add(parent, fmt.Sprintf("%s-dir-%03d", stems[rng.Intn(len(stems))], len(idx.Records)), uint32(os.ModeDir))
			parents = append(parents, dir)
			continue
		}
		stem := stems[rng.Intn(len(stems))]
		name := fmt.Sprintf("%s-%03d.%s", stem, rng.Intn(60), exts[rng.Intn(len(exts))])
		switch rng.Intn(10) {
		case 0:
			name = fmt.Sprintf("%s.%s.%03d.%s", stem, stems[rng.Intn(len(stems))], rng.Intn(50), exts[rng.Intn(len(exts))])
		case 1:
			name = strings.Repeat("longname", 8) + fmt.Sprintf("-%03d.%s", rng.Intn(50), exts[rng.Intn(len(exts))])
		case 2:
			name = fmt.Sprintf("%s [%03d] ?.txt", stem, rng.Intn(50))
		case 3:
			name = strings.ToUpper(stem) + fmt.Sprintf("-%03d.%s", rng.Intn(50), exts[rng.Intn(len(exts))])
		}
		id := add(parent, name, 0)
		if rng.Intn(31) == 0 {
			idx.Records[id].Deleted = true
		}
	}
	buildOrders(idx)
	return idx
}

func randomQueries(seed int64, idx *Index) []queryOptions {
	rng := rand.New(rand.NewSource(seed ^ 0x5eed5eed))
	paths := randomCorpusPaths(idx)
	terms := randomCorpusTerms(paths)
	base := []queryOptions{
		{Query: "workspace", MatchPath: true, Limit: 25},
		{Query: "src main", MatchPath: true, Limit: 2},
		{Query: "Übersicht", MatchPath: true, Limit: 25},
		{Query: "übersicht", MatchPath: true, Limit: 25},
		{Query: "ДОКУМЕНТ", MatchPath: true, Limit: 25},
		{Query: "資料", MatchPath: true, Limit: 25},
		{Query: "x", MatchPath: true, Limit: 25},
		{Query: "xy", MatchPath: true, Limit: 25},
		{Query: "ext:go", MatchPath: true, Limit: 25},
		{Query: "ext:pdf|txt", MatchPath: true, Limit: 25},
		{Query: "glob:*.go", MatchPath: true, Limit: 25},
		{Query: "type:dir", MatchPath: true, Limit: 25},
		{Query: "type:file !backup", MatchPath: true, Limit: 25},
		{Query: "regex:.*Specification.*", MatchPath: true, Limit: 25},
		{Query: "dir:downloads", MatchPath: true, Limit: 25},
		{Query: "go|pdf", MatchPath: true, Limit: 25},
		{Query: "case: MAIN", MatchPath: true, Limit: 25},
	}
	limits := []int{0, 1, 2, 25, 10000}
	for i := 0; i < 48 && len(terms) > 0; i++ {
		a := terms[rng.Intn(len(terms))]
		b := terms[rng.Intn(len(terms))]
		query := a
		switch rng.Intn(9) {
		case 0:
			query = a + " " + b
		case 1:
			query = "path:" + a
		case 2:
			query = "!" + a + " " + b
		case 3:
			query = a + "|" + b
		case 4:
			query = "dir:" + a
		case 5:
			query = "regex:.*" + regexpSafeLiteral(a) + ".*"
		case 6:
			query = "type:file " + a
		case 7:
			query = "case: " + a
		}
		opt := queryOptions{
			Query:     query,
			MatchPath: rng.Intn(2) == 0 || strings.Contains(query, "path:") || strings.Contains(query, "dir:"),
			Limit:     limits[rng.Intn(len(limits))],
		}
		if rng.Intn(6) == 0 && len(paths) > 0 {
			opt.Under = parentPathForQuery(paths[rng.Intn(len(paths))])
			opt.MatchPath = true
		}
		if rng.Intn(10) == 0 && len(paths) > 0 {
			opt.CWDBias = parentPathForQuery(paths[rng.Intn(len(paths))])
		}
		if rng.Intn(10) == 0 && len(paths) > 0 {
			opt.RootBias = parentPathForQuery(paths[rng.Intn(len(paths))])
		}
		base = append(base, opt)
	}
	return base
}

func randomCorpusPaths(idx *Index) []string {
	paths := make([]string, 0, idx.compactRecordCount())
	cache := make(map[int]string)
	for i := 0; i < idx.compactRecordCount(); i++ {
		rec := idx.compactRecord(i)
		if rec.Deleted {
			continue
		}
		paths = append(paths, idx.reconstructCompactPathCached(i, cache))
	}
	return paths
}

func randomCorpusTerms(paths []string) []string {
	seen := make(map[string]struct{})
	for _, path := range paths {
		fields := strings.FieldsFunc(path, func(r rune) bool {
			return r == '\\' || r == '/' || r == ':' || r == ' ' || r == '\t' || r == '(' || r == ')'
		})
		for _, field := range fields {
			field = strings.Trim(field, "._-[]{}")
			if field == "" || strings.ContainsAny(field, `*?[]`) {
				continue
			}
			addTerm(seen, field)
			lower := strings.ToLower(field)
			addTerm(seen, lower)
			lowerRunes := []rune(lower)
			if len(lowerRunes) > 3 {
				addTerm(seen, string(lowerRunes[:3]))
				addTerm(seen, string(lowerRunes[1:min(len(lowerRunes), 5)]))
			}
			if ext := strings.TrimPrefix(filepath.Ext(field), "."); ext != "" {
				addTerm(seen, ext)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for term := range seen {
		out = append(out, term)
	}
	sort.Strings(out)
	return out
}

func addTerm(seen map[string]struct{}, term string) {
	term = strings.TrimSpace(term)
	if term == "" || strings.ContainsAny(term, `\/*?[]|`) {
		return
	}
	seen[term] = struct{}{}
}

func parentPathForQuery(path string) string {
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return path
	}
	return dir
}

func regexpSafeLiteral(term string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `.`, `\.`, `+`, `\+`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`, `(`, `\(`, `)`, `\)`, `^`, `\^`, `$`, `\$`)
	return replacer.Replace(term)
}

func fuzzQueryString(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	fields := strings.Fields(raw)
	out := make([]string, 0, min(len(fields), 4))
	for _, field := range fields {
		field = strings.Trim(field, "\"'")
		if field == "" || strings.ContainsAny(field, "\x00\r\n") {
			continue
		}
		if strings.Contains(field, ":") && !strings.HasPrefix(field, "ext:") &&
			!strings.HasPrefix(field, "path:") && !strings.HasPrefix(field, "dir:") &&
			!strings.HasPrefix(field, "glob:") && !strings.HasPrefix(field, "regex:") &&
			!strings.HasPrefix(field, "type:") && !strings.HasPrefix(field, "case:") {
			continue
		}
		out = append(out, field)
		if len(out) >= 4 {
			break
		}
	}
	return strings.Join(out, " ")
}

func assertCompactOracleParity(t *testing.T, seed int64, idx *Index, vol *serviceVolumeIndex, opts queryOptions, countOnly bool) {
	t.Helper()
	full, err := searchCompactWithCache(idx, opts, countOnly, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("seed=%d query=%q count=%v full search: %v", seed, opts.Query, countOnly, err)
	}
	trace := &searchTrace{}
	fastOpts := opts
	fastOpts.Trace = trace
	fast, err := searchCompactWithCache(idx, fastOpts, countOnly, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatalf("seed=%d query=%q count=%v fast search: %v trace=%+v", seed, opts.Query, countOnly, err, *trace)
	}
	if gotPaths, wantPaths := pathsOf(fast), pathsOf(full); !sameOrderedStrings(gotPaths, wantPaths) {
		t.Fatalf("seed=%d query=%q count=%v source=%s candidates=%d paths=%v want=%v", seed, opts.Query, countOnly, trace.Source, trace.Candidates, gotPaths, wantPaths)
	}
	assertTraceBudget(t, seed, trace, opts, len(fast), idx.compactRecordCount())
}

func assertTraceBudget(t *testing.T, seed int64, trace *searchTrace, opts queryOptions, resultCount int, recordCount int) {
	t.Helper()
	if trace == nil || trace.Source == "" {
		t.Fatalf("seed=%d query=%q missing trace source", seed, opts.Query)
	}
	if trace.Source == "compact-name-order-scan" {
		t.Fatalf("seed=%d query=%q routed to unbounded compact scan", seed, opts.Query)
	}
	switch trace.Source {
	case "legacy-planner", "path-dir-filter", "filter", "path-root-limited", "path-term-subtree", "regex-literal", "exact-dir", "exact-name", "name-prefix", "cached-multi-name-term", "multi-name-term", "name-term-posting":
		t.Fatalf("seed=%d query=%q routed behind bounded floor to %s", seed, opts.Query, trace.Source)
	}
	budget := recordCount
	if opts.Limit > 0 && resultCount < opts.Limit && resultCount < budget {
		budget = recordCount
	}
	if trace.Candidates > budget {
		t.Fatalf("seed=%d query=%q source=%s candidates=%d budget=%d", seed, opts.Query, trace.Source, trace.Candidates, budget)
	}
}

func assertQueryMetamorphicProperties(t *testing.T, seed int64, idx *Index, vol *serviceVolumeIndex, opts queryOptions) {
	t.Helper()
	if _, err := parseQuery(opts); err != nil {
		t.Fatalf("seed=%d generated invalid query %q: %v", seed, opts.Query, err)
	}
	if strings.Fields(opts.Query)[0] == "case:" {
		return
	}
	base, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("seed=%d query=%q metamorphic base: %v", seed, opts.Query, err)
	}
	fields := strings.Fields(opts.Query)
	if len(fields) > 1 && !strings.Contains(opts.Query, "regex:") {
		permuted := opts
		rev := append([]string(nil), fields...)
		for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
			rev[i], rev[j] = rev[j], rev[i]
		}
		permuted.Query = strings.Join(rev, " ")
		got, err := searchCompactWithCache(idx, permuted, false, vol.pathCache, vol.nameTermCandidates)
		if err != nil {
			t.Fatalf("seed=%d query=%q permuted=%q: %v", seed, opts.Query, permuted.Query, err)
		}
		if !sameOrderedStrings(pathsOf(got), pathsOf(base)) {
			t.Fatalf("seed=%d query=%q permuted=%q paths=%v want=%v", seed, opts.Query, permuted.Query, pathsOf(got), pathsOf(base))
		}
	}
	if opts.Limit != 1 {
		small := opts
		small.Limit = 1
		large := opts
		large.Limit = max(opts.Limit, 25)
		gotSmall, err := searchCompactWithCache(idx, small, false, vol.pathCache, vol.nameTermCandidates)
		if err != nil {
			t.Fatalf("seed=%d query=%q small limit: %v", seed, opts.Query, err)
		}
		gotLarge, err := searchCompactWithCache(idx, large, false, vol.pathCache, vol.nameTermCandidates)
		if err != nil {
			t.Fatalf("seed=%d query=%q large limit: %v", seed, opts.Query, err)
		}
		if len(gotSmall) > len(gotLarge) || !sameOrderedStrings(pathsOf(gotSmall), pathsOf(gotLarge)[:len(gotSmall)]) {
			t.Fatalf("seed=%d query=%q limit monotonic small=%v large=%v", seed, opts.Query, pathsOf(gotSmall), pathsOf(gotLarge))
		}
	}
	negative := opts
	negative.Query = strings.TrimSpace(negative.Query + " !no-such-term-xq9")
	gotNegative, err := searchCompactWithCache(idx, negative, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatalf("seed=%d query=%q negative identity: %v", seed, opts.Query, err)
	}
	if !sameOrderedStrings(pathsOf(gotNegative), pathsOf(base)) {
		t.Fatalf("seed=%d query=%q negative identity paths=%v want=%v", seed, opts.Query, pathsOf(gotNegative), pathsOf(base))
	}
}

func secondOracleSearch(idx *Index, opts queryOptions) ([]Entry, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, err
	}
	dropSatisfiedVolumeTerms(&pq, idx.Volume)
	limit := normalizedLimit(opts.Limit, false)
	pq.Limit = limit
	order := idx.CompactNameOrder
	if pq.RootBias != "" || pq.CWDBias != "" {
		order = idx.biasOrderCompact(order, firstNonEmpty(pq.CWDBias, pq.RootBias))
	}
	cache := make(map[int]string)
	out := make([]Entry, 0, min(limit, 1024))
	for pos := 0; pos < compactOrderLen(order, idx.compactRecordCount()); pos++ {
		id := compactOrderAt(order, pos)
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		path := idx.reconstructCompactPathCached(id, cache)
		entry := Entry{
			Path:        path,
			Name:        rec.Name,
			LowerPath:   strings.ToLower(path),
			LowerName:   strings.ToLower(rec.Name),
			Mode:        rec.Mode,
			Size:        rec.Size,
			ModUnix:     rec.ModUnix,
			IndexSource: idx.Source,
		}
		if secondOracleEntryMatches(entry, pq, pq.MatchPath) {
			out = append(out, entry)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func secondOracleEntryMatches(entry Entry, pq parsedQuery, matchPath bool) bool {
	path := filepath.Clean(entry.Path)
	name := entry.Name
	if name == "" {
		name = filepath.Base(path)
	}
	cmpPath, cmpName := path, name
	if !pq.CaseSensitive {
		cmpPath = strings.ToLower(cmpPath)
		cmpName = strings.ToLower(cmpName)
	}
	haystack := cmpName
	if matchPath {
		haystack = cmpPath
	}
	if pq.Under != "" && !secondOraclePathUnder(path, pq.Under) {
		return false
	}
	if pq.HasModAfter {
		if entry.ModUnix == 0 || !time.Unix(0, entry.ModUnix).After(pq.ModifiedAfter) {
			return false
		}
	}
	if pq.Type == "file" && entry.Mode&uint32(os.ModeDir) != 0 {
		return false
	}
	if pq.Type == "dir" && entry.Mode&uint32(os.ModeDir) == 0 {
		return false
	}
	for _, term := range pq.Terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	for _, ext := range pq.Exts {
		actual := strings.TrimPrefix(filepath.Ext(name), ".")
		if !pq.CaseSensitive {
			actual = strings.ToLower(actual)
		}
		if actual != ext {
			return false
		}
	}
	for _, dir := range pq.Dirs {
		if !strings.Contains(cmpPath, dir) {
			return false
		}
	}
	for _, glob := range pq.Globs {
		ok, err := filepath.Match(glob, cmpName)
		if err != nil || !ok {
			return false
		}
	}
	for _, re := range pq.Regexps {
		if !re.MatchString(path) {
			return false
		}
	}
	for _, sf := range pq.SizeFilters {
		if !sf.matches(entry.Size) {
			return false
		}
	}
	for _, df := range pq.DateFilters {
		if !df.matches(entry.ModUnix) {
			return false
		}
	}
	for _, group := range pq.OrGroups {
		ok := false
		for _, alt := range group {
			if secondOracleEntryMatches(entry, alt, matchPath || alt.MatchPath) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, neg := range pq.NotGroups {
		if secondOracleEntryMatches(entry, neg, matchPath || neg.MatchPath) {
			return false
		}
	}
	return true
}

func secondOraclePathUnder(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if strings.EqualFold(path, root) {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

func parseQueryNoPanic(t *testing.T, opts queryOptions) (pq parsedQuery, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("parseQuery panicked for %q: %v", opts.Query, r)
		}
	}()
	return parseQuery(opts)
}

func renderParsedQueryForTest(pq parsedQuery) string {
	fields := make([]string, 0, len(pq.Terms)+len(pq.Exts)+len(pq.Dirs)+len(pq.Globs)+len(pq.NotGroups)+2)
	if pq.CaseSensitive {
		fields = append(fields, "case:")
	}
	if pq.Type != "" {
		fields = append(fields, "type:"+pq.Type)
	}
	fields = append(fields, pq.Terms...)
	for _, ext := range pq.Exts {
		fields = append(fields, "ext:"+ext)
	}
	for _, dir := range pq.Dirs {
		fields = append(fields, "dir:"+dir)
	}
	for _, parent := range pq.Parents {
		fields = append(fields, "parent:"+parent)
	}
	for _, glob := range pq.Globs {
		fields = append(fields, "glob:"+glob)
	}
	fields = append(fields, pq.RegexTerms...)
	for _, group := range pq.OrGroups {
		parts := make([]string, 0, len(group))
		for _, alt := range group {
			if rendered := renderParsedQueryForTest(alt); rendered != "" && !strings.Contains(rendered, " ") {
				parts = append(parts, rendered)
			}
		}
		if len(parts) > 1 {
			fields = append(fields, strings.Join(parts, "|"))
		}
	}
	for _, neg := range pq.NotGroups {
		if rendered := renderParsedQueryForTest(neg); rendered != "" && !strings.Contains(rendered, " ") {
			fields = append(fields, "!"+rendered)
		}
	}
	return strings.Join(fields, " ")
}

func parsedQuerySignature(pq parsedQuery) string {
	parts := []string{
		fmt.Sprintf("path=%v", pq.MatchPath),
		fmt.Sprintf("case=%v", pq.CaseSensitive),
		"type=" + pq.Type,
		"terms=" + strings.Join(sortedCopy(pq.Terms), ","),
		"ext=" + strings.Join(sortedCopy(pq.Exts), ","),
		"dirs=" + strings.Join(sortedCopy(pq.Dirs), ","),
		"globs=" + strings.Join(sortedCopy(pq.Globs), ","),
		"regexTerms=" + strings.Join(sortedCopy(pq.RegexTerms), ","),
	}
	for _, group := range pq.OrGroups {
		groupParts := make([]string, 0, len(group))
		for _, alt := range group {
			groupParts = append(groupParts, parsedQuerySignature(alt))
		}
		sort.Strings(groupParts)
		parts = append(parts, "or="+strings.Join(groupParts, "|"))
	}
	for _, neg := range pq.NotGroups {
		parts = append(parts, "not="+parsedQuerySignature(neg))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func generatedLooseFuzzQueries() []string {
	seed := []string{
		"nrrd",
		"NRRD",
		".nrrd",
		".NRRD",
		"raw",
		"pdf",
		"pvsm",
		"F: nrrd",
		"F nrrd",
		"C: pvsm",
		"C pvsm",
		"F: nrrd !backup",
		"F: nrrd !backup !tmp",
		"nrrd raw",
		".nrrd .raw",
		"pvsm json",
		"pdf docx",
		"Users nrrd",
		"Users exampleuser",
		"exampleuser Users",
		"Users exampleuser Downloads",
		"Downloads exampleuser Users",
		"AppData pvsm",
		"Downloads raw",
		"Downloads docx",
		"workspace pdf",
		"nrrd Dataset trainingdata",
		"trainingdata Dataset nrrd",
		"Dataset trainingdata nrrd",
		"workspace trainingdata Dataset nrrd",
		"nrrd nrrd",
		"F: nrrd nrrd",
		"nrrd missing",
		"trainingdata missing nrrd",
		"zzzz nrrd",
		"nrrd|raw",
		"F: nrrd|raw",
		`workspace\nrrd-cache cache`,
		`workspace/dataset-000000.nrrd metadata`,
		"path: C: nrrd",
		"path:F: nrrd",
		"path:F: .nrrd",
		"path:C:.nrrd",
		"path:Downloads.nrrd",
	}
	terms := [][]string{
		{"trainingdata", "Dataset", "nrrd"},
		{"Dataset", "trainingdata", "nrrd"},
		{"workspace", "trainingdata", "Dataset", "nrrd"},
		{"workspace", "nrrd", "metadata", "json"},
		{"Downloads", "raw"},
		{"Downloads", "docx"},
		{"Users", "nrrd"},
		{"Users", "exampleuser"},
		{"Users", "exampleuser", "Downloads"},
		{"fixtureproj", "projects"},
	}
	suffixes := []string{"nrrd", ".nrrd", "raw", ".raw", "pdf", ".pdf", "pvsm", ".pvsm"}
	drives := []string{"", "C:", "F:", "C", "F"}
	negatives := []string{"", "!backup", "!metadata"}

	seen := make(map[string]struct{}, 256)
	out := make([]string, 0, 180)
	add := func(query string) {
		query = strings.Join(strings.Fields(query), " ")
		if query == "" {
			return
		}
		if _, ok := seen[query]; ok {
			return
		}
		seen[query] = struct{}{}
		out = append(out, query)
	}
	for _, query := range seed {
		add(query)
	}
	for _, drive := range drives {
		for _, suffix := range suffixes {
			for _, neg := range negatives {
				add(strings.Join(nonEmptyStrings(drive, suffix, neg), " "))
			}
		}
	}
	for _, parts := range terms {
		for _, neg := range negatives {
			add(strings.Join(nonEmptyStrings(strings.Join(parts, " "), neg), " "))
			rev := append([]string(nil), parts...)
			for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
				rev[i], rev[j] = rev[j], rev[i]
			}
			add(strings.Join(nonEmptyStrings(strings.Join(rev, " "), neg), " "))
		}
	}
	if len(out) > 160 {
		out = out[:160]
	}
	return out
}

func looseFuzzMatchPath(query string) bool {
	fields := strings.Fields(query)
	plain := 0
	for _, field := range fields {
		raw := strings.TrimLeft(field, "!-")
		if raw == "" {
			continue
		}
		if isVolumeQueryTerm(raw) || strings.ContainsAny(raw, `\/`) {
			return true
		}
		key, value, hasPrefix := strings.Cut(raw, ":")
		if hasPrefix {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "path", "fullpath", "full-path", "full_path", "fullpathname", "full-path-name", "location":
				return true
			case "ext", "extension", "glob", "regex", "size", "sz", "dm", "date", "date-modified", "datemodified", "modified", "type", "case":
				continue
			}
			if value != "" {
				plain++
			}
			continue
		}
		plain++
	}
	return plain >= 2
}
