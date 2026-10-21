package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

const standaloneTestPipeSDDL = `D:(A;;GA;;;WD)`

func TestStandaloneServicePipeACLIsTestOnly(t *testing.T) {
	if standaloneTestPipeSDDL == defaultServiceSDDL {
		t.Fatal("test-only pipe ACL must not replace the production service ACL")
	}
	if defaultServiceSDDL != `D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)` {
		t.Fatalf("production service ACL changed: %q", defaultServiceSDDL)
	}
}

func TestCompactMMapEntryCountUsesMappedRecordCount(t *testing.T) {
	idx := &Index{
		Compact:     true,
		MMapRecords: &MMapRecords{count: 42},
	}

	if got := idx.entryCount(); got != 42 {
		t.Fatalf("entryCount() = %d, want mmap record count", got)
	}
}

func TestServiceVolumeIndexTrimSearchCachesLockedClearsOversizedCaches(t *testing.T) {
	t.Setenv("SEEKFS_POSTING_CACHE_MB", "1")
	vol := syntheticServiceVolumeIndexForCacheTests()
	for i := 0; i <= servicePathCacheLimit; i++ {
		vol.pathCache[i] = "cached-path"
	}
	vol.termCache["large"] = postingCacheEntry{ids: make([]int, int(postingListCacheMaxBytes()/int64(unsafe.Sizeof(int(0))))+1), gen: 1}
	vol.pathTermCache = map[string]postingCacheEntry{"path": {ids: []int{1}, gen: 1}}
	vol.extCache = map[string]postingCacheEntry{".txt": {ids: []int{1}, gen: 1}}

	vol.trimSearchCachesLocked()

	if len(vol.pathCache) != 0 {
		t.Fatalf("pathCache len = %d, want 0", len(vol.pathCache))
	}
	if vol.termCache != nil || vol.pathTermCache != nil || vol.extCache != nil {
		t.Fatalf("term caches were not cleared: term=%v pathTerm=%v ext=%v", vol.termCache, vol.pathTermCache, vol.extCache)
	}
}

func TestServiceVolumeIndexFastPostingCount(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	pq, err := parseQuery(queryOptions{Query: "type:file ext:go c:", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	count, ok := vol.fastPostingCount(pq)
	if !ok {
		t.Fatal("fastPostingCount declined safe posting query")
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestServiceVolumeIndexFastPostingCountUsesSimpleExtensionGlob(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	pq, err := parseQuery(queryOptions{Query: "type:file glob:*.go"})
	if err != nil {
		t.Fatal(err)
	}
	count, ok := vol.fastPostingCount(pq)
	if !ok {
		t.Fatal("fastPostingCount declined simple extension glob")
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestSimpleGlobExtsDeclinesComplexGlobs(t *testing.T) {
	if got, ok := simpleGlobExts([]string{"*.go", "*.md"}); !ok || len(got) != 2 || got[0] != "go" || got[1] != "md" {
		t.Fatalf("simpleGlobExts = %v, %v; want [go md], true", got, ok)
	}
	if _, ok := simpleGlobExts([]string{"*test*.go"}); ok {
		t.Fatal("simpleGlobExts accepted complex wildcard glob")
	}
	if got := complexGlobExts([]string{"*test*.go", "README.*"}); len(got) != 1 || got[0] != "go" {
		t.Fatalf("complexGlobExts = %v, want [go]", got)
	}
}

func TestServiceSubtreeIntervalsDefaultEnabled(t *testing.T) {
	t.Setenv("SEEKFS_SUBTREE_INTERVALS", "")
	if !serviceSubtreeIntervalsEnabled() {
		t.Fatal("subtree intervals should be enabled by default")
	}
	t.Setenv("SEEKFS_SUBTREE_INTERVALS", "0")
	if serviceSubtreeIntervalsEnabled() {
		t.Fatal("subtree intervals should be disabled by explicit opt-out")
	}
}

func TestServicePathGramsDefaultEnabled(t *testing.T) {
	t.Setenv("SEEKFS_PATH_GRAMS", "")
	if !servicePathGramsEnabled() {
		t.Fatal("path grams should be enabled by default")
	}
	t.Setenv("SEEKFS_PATH_GRAMS", "false")
	if servicePathGramsEnabled() {
		t.Fatal("path grams should be disabled by explicit opt-out")
	}
}

func TestServiceNameOrderDefaultEnabled(t *testing.T) {
	t.Setenv("SEEKFS_NAME_ORDER", "")
	if !serviceNameOrderEnabled() {
		t.Fatal("name order should be enabled by default policy")
	}
	vol := newServiceVolumeIndex("fixture.gsi", commonSearchFixture())
	if got := vol.nameOrderStateString(); got != "ready" {
		t.Fatalf("small name order state = %q, want ready", got)
	}
	if vol.queryIndex == nil || len(vol.queryIndex.nameOrder) == 0 || len(vol.queryIndex.nameRank) == 0 {
		t.Fatal("small resident name order/rank was not built synchronously")
	}
}

func TestCompactChildrenBuildNotNeededAfterVolumeConstruction(t *testing.T) {
	vol := newServiceVolumeIndex("fixture.gsi", commonSearchFixture())
	if len(vol.childOffsets) == 0 || len(vol.childIDs) == 0 {
		t.Fatal("compact child ranges were not built during volume construction")
	}
	if vol.needsCompactChildrenBuild() {
		t.Fatal("volume requested duplicate compact child build")
	}
	vol.childOffsets = nil
	vol.childIDs = nil
	if !vol.needsCompactChildrenBuild() {
		t.Fatal("volume did not request compact child rebuild after ranges were invalidated")
	}
}

func TestServiceVolumesForQueryMatchesWalkRootVolume(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\fixture-root\churn-soak`},
		Source:  "walk",
		Entries: []Entry{{
			Path:      `F:\fixture-root\churn-soak\needle.txt`,
			Name:      "needle.txt",
			LowerPath: strings.ToLower(`F:\fixture-root\churn-soak\needle.txt`),
			LowerName: "needle.txt",
		}},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("walk.gsi", idx)
	got, err := serviceVolumesForQuery([]*serviceVolumeIndex{vol}, queryOptions{
		Query:     "needle",
		MatchPath: true,
		Under:     `F:\fixture-root\churn-soak`,
	})
	if err != nil {
		t.Fatalf("serviceVolumesForQuery: %v", err)
	}
	if len(got) != 1 || got[0] != vol {
		t.Fatalf("matched volumes = %d, want walk volume", len(got))
	}
}

func TestSortFRNIndexEntriesSortsOnlyWhenNeeded(t *testing.T) {
	sortedFRNs := []uint64{1, 2, 3}
	sortedIDs := []uint32{10, 20, 30}
	sortFRNIndexEntries(sortedFRNs, sortedIDs)
	if !sameUint64s(sortedFRNs, []uint64{1, 2, 3}) || !sameUint32s(sortedIDs, []uint32{10, 20, 30}) {
		t.Fatalf("sorted entries = %v/%v, want %v/%v", sortedFRNs, sortedIDs, []uint64{1, 2, 3}, []uint32{10, 20, 30})
	}

	unsortedFRNs := []uint64{3, 1, 2}
	unsortedIDs := []uint32{30, 10, 20}
	sortFRNIndexEntries(unsortedFRNs, unsortedIDs)
	if !sameUint64s(unsortedFRNs, []uint64{1, 2, 3}) || !sameUint32s(unsortedIDs, []uint32{10, 20, 30}) {
		t.Fatalf("unsorted entries = %v/%v, want %v/%v", unsortedFRNs, unsortedIDs, []uint64{1, 2, 3}, []uint32{10, 20, 30})
	}
}

func TestServiceStartupWorkerCount(t *testing.T) {
	t.Setenv("SEEKFS_STARTUP_WORKERS", "")
	if got := serviceStartupWorkerCount(0); got != 1 {
		t.Fatalf("workers for 0 dbs = %d, want 1", got)
	}
	if got := serviceStartupWorkerCount(1); got != 1 {
		t.Fatalf("workers for 1 db = %d, want 1", got)
	}
	if got := serviceStartupWorkerCount(3); got != serviceStartupDefaultWorkers {
		t.Fatalf("default workers for 3 dbs = %d, want %d", got, serviceStartupDefaultWorkers)
	}
	t.Setenv("SEEKFS_STARTUP_WORKERS", "8")
	if got := serviceStartupWorkerCount(3); got != 3 {
		t.Fatalf("capped workers = %d, want 3", got)
	}
	t.Setenv("SEEKFS_STARTUP_WORKERS", "1")
	if got := serviceStartupWorkerCount(3); got != 1 {
		t.Fatalf("forced workers = %d, want 1", got)
	}
	t.Setenv("SEEKFS_STARTUP_WORKERS", "invalid")
	if got := serviceStartupWorkerCount(3); got != serviceStartupDefaultWorkers {
		t.Fatalf("invalid env workers = %d, want %d", got, serviceStartupDefaultWorkers)
	}
}

func sameUint64s(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func BenchmarkSortFRNIndexEntries(b *testing.B) {
	const n = 500_000
	baseSortedFRNs := make([]uint64, n)
	baseSortedIDs := make([]uint32, n)
	baseShuffledFRNs := make([]uint64, n)
	baseShuffledIDs := make([]uint32, n)
	for i := 0; i < n; i++ {
		baseSortedFRNs[i] = uint64(i + 1)
		baseSortedIDs[i] = uint32(i)
		baseShuffledFRNs[i] = uint64((i*7919)%n + 1)
		baseShuffledIDs[i] = uint32(i)
	}
	b.Run("already-sorted", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			frns := append([]uint64(nil), baseSortedFRNs...)
			ids := append([]uint32(nil), baseSortedIDs...)
			sortFRNIndexEntries(frns, ids)
		}
	})
	b.Run("shuffled", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			frns := append([]uint64(nil), baseShuffledFRNs...)
			ids := append([]uint32(nil), baseShuffledIDs...)
			sortFRNIndexEntries(frns, ids)
		}
	})
}

func TestServiceNameOrderCanBeDisabledByEnv(t *testing.T) {
	t.Setenv("SEEKFS_NAME_ORDER", "0")
	if serviceNameOrderEnabled() {
		t.Fatal("name order should be disabled by explicit env")
	}
	vol := newServiceVolumeIndex("fixture.gsi", commonSearchFixture())
	if got := vol.nameOrderStateString(); got != "" {
		t.Fatalf("name order state = %q, want empty when disabled", got)
	}
	if vol.queryIndex != nil && (len(vol.queryIndex.nameOrder) != 0 || len(vol.queryIndex.nameRank) != 0) {
		t.Fatal("name order/rank should not be built when disabled")
	}
}

func TestBackgroundNameOrderBuildPublishesResidentView(t *testing.T) {
	t.Setenv("SEEKFS_NAME_ORDER", "1")
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	if vol.queryIndex == nil {
		t.Fatal("queryIndex was nil")
	}
	vol.queryIndex.nameOrder = nil
	vol.queryIndex.nameRank = nil
	vol.nameOrderState.Store(nameTrigramStatePending)
	if got := vol.nameOrderStateString(); got != "pending" {
		t.Fatalf("initial name order state = %q, want pending", got)
	}
	s := &goSearchService{}
	s.rebuildNameOrderInBackground(vol)
	if got := vol.nameOrderStateString(); got != "ready" {
		t.Fatalf("name order state = %q, want ready", got)
	}
	if len(vol.queryIndex.nameOrder) == 0 || len(vol.queryIndex.nameRank) != idx.compactRecordCount() {
		t.Fatalf("name order/rank lengths = %d/%d, records=%d", len(vol.queryIndex.nameOrder), len(vol.queryIndex.nameRank), idx.compactRecordCount())
	}
	if vol.nameOrderMillis.Load() < 0 {
		t.Fatalf("name order build millis = %d, want non-negative", vol.nameOrderMillis.Load())
	}
}

func TestBuildExtTopPostingsKeepsTopRankedIDsOnly(t *testing.T) {
	ext := map[string][]uint32{
		"json": {9, 1, 8, 2, 7, 3, 6, 4, 5, 0},
		"go":   {4, 1},
	}
	ranks := []uint32{90, 10, 20, 30, 40, 50, 60, 70, 80, 0}

	top := buildExtTopPostings(ext, ranks, 4)
	if got, want := top["json"], []uint32{9, 1, 2, 3}; !sameUint32s(got, want) {
		t.Fatalf("json top = %v, want %v", got, want)
	}
	if got, want := top["go"], []uint32{1, 4}; !sameUint32s(got, want) {
		t.Fatalf("go top = %v, want %v", got, want)
	}
}

func sameUint32s(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func BenchmarkBuildExtTopPostingsBounded(b *testing.B) {
	const records = 250_000
	ext := map[string][]uint32{
		"json": make([]uint32, records),
		"pdf":  make([]uint32, records/2),
		"go":   make([]uint32, records/20),
	}
	ranks := make([]uint32, records)
	for i := range ranks {
		ranks[i] = uint32(records - i)
	}
	for key, ids := range ext {
		offset := 0
		if key == "pdf" {
			offset = 1
		} else if key == "go" {
			offset = 3
		}
		for i := range ids {
			ids[i] = uint32((i*7 + offset) % records)
		}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		top := buildExtTopPostings(ext, ranks, serviceExtTopPostingLimit)
		if len(top["json"]) != serviceExtTopPostingLimit {
			b.Fatalf("json top len = %d", len(top["json"]))
		}
	}
}

func TestParallelTopCandidateIDsByRankMatchesSerial(t *testing.T) {
	const records = serviceRankParallelMinIDs*2 + 123
	const limit = 20
	ids := make([]int, records)
	ranks := make([]int, records)
	for i := 0; i < records; i++ {
		id := (i*7919 + 17) % records
		ids[i] = id
		ranks[id] = (records - i*3571%records)
	}
	rankOf := func(id int) int {
		if id < 0 || id >= len(ranks) {
			return records + id
		}
		return ranks[id]
	}
	serial := topCandidateIDsByRankSerial(ids, limit, rankOf)
	parallel := topCandidateIDsByRankParallel(ids, limit, rankOf)
	if !sameIntSlices(serial, parallel) {
		t.Fatalf("parallel top IDs = %v, serial = %v", parallel, serial)
	}
}

func sameIntSlices(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestServiceNameTrigramsDefaultEnabledUnderCap(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "")
	t.Setenv("SEEKFS_NAME_TRIGRAM_MAX_RECORDS", "")
	if !serviceNameTrigramsEnabled() {
		t.Fatal("name trigrams should be enabled by default policy")
	}
	vol := newServiceVolumeIndex("fixture.gsi", commonSearchFixture())
	if vol.nameTrigramIndex() != nil {
		t.Fatal("name trigram resident index built synchronously")
	}
	if got := vol.nameTrigramStateString(); got != "pending" {
		t.Fatalf("name trigram state = %q, want pending", got)
	}
}

func TestServiceNameTrigramsCanBeDisabledByEnv(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "0")
	if serviceNameTrigramsEnabled() {
		t.Fatal("name trigrams should be disabled by explicit env")
	}
	vol := newServiceVolumeIndex("fixture.gsi", commonSearchFixture())
	if got := vol.nameTrigramStateString(); got != "" {
		t.Fatalf("name trigram state = %q, want empty when disabled", got)
	}
}

func TestServiceNameTrigramsEnabledStartsPending(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	if !serviceNameTrigramsEnabled() {
		t.Fatal("name trigrams should be enabled by explicit opt-in")
	}
	vol := newServiceVolumeIndex("fixture.gsi", commonSearchFixture())
	if vol.nameTrigramIndex() != nil {
		t.Fatal("name trigram resident index built synchronously")
	}
	if got := vol.nameTrigramStateString(); got != "pending" {
		t.Fatalf("name trigram state = %q, want pending", got)
	}
}

func TestServiceNameTrigramRecordCapAndForceEnable(t *testing.T) {
	idx := commonSearchFixture()
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "")
	t.Setenv("SEEKFS_NAME_TRIGRAM_MAX_RECORDS", "1")
	if serviceNameTrigramsEnabledForIndex(idx) {
		t.Fatal("name trigrams should be disabled over default cap")
	}
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	if !serviceNameTrigramsEnabledForIndex(idx) {
		t.Fatal("explicit opt-in should force name trigrams over cap")
	}
}

func TestBackgroundNameTrigramBuildPublishesAtomicIndex(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	vol := newServiceVolumeIndex("fixture.gsi", commonSearchFixture())
	s := &goSearchService{}
	s.rebuildNameTrigramsInBackground(vol)
	ti := vol.nameTrigramIndex()
	if ti == nil || ti.keyCount() == 0 {
		t.Fatal("background name trigram index was not published")
	}
	if got := vol.nameTrigramStateString(); got != "ready" {
		t.Fatalf("name trigram state = %q, want ready", got)
	}
	info := vol.residentMemoryInfo()
	if info.NameTrigramBytes == 0 || info.NameTrigramKeys == 0 {
		t.Fatalf("name trigram memory = (%d bytes, %d keys), want populated", info.NameTrigramBytes, info.NameTrigramKeys)
	}
	if vol.nameTrigramMillis.Load() < 0 {
		t.Fatalf("name trigram build millis = %d, want non-negative", vol.nameTrigramMillis.Load())
	}
}

func TestResidentBackgroundLoadingTracksPendingAccelerators(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	vol := newServiceVolumeIndex("fixture.gsi", commonSearchFixture())
	if !serviceResidentBackgroundLoading([]*serviceVolumeIndex{vol}) {
		t.Fatal("background loading = false with pending name trigrams")
	}
	s := &goSearchService{}
	s.rebuildNameTrigramsInBackground(vol)
	if serviceResidentBackgroundLoading([]*serviceVolumeIndex{vol}) {
		t.Fatal("background loading = true after resident accelerators are ready")
	}
}

func TestSearchFallsBackWhileNameTrigramsPending(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "path:C: .opencode", Limit: 20}
	if vol.nameTrigramIndex() != nil || vol.nameTrigramStateString() != "pending" {
		t.Fatalf("unexpected trigram state index=%v state=%q", vol.nameTrigramIndex(), vol.nameTrigramStateString())
	}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := namesOf(fast), namesOf(full); !sameStringSet(got, want) {
		t.Fatalf("pending trigram fallback names = %v, full names = %v", got, want)
	}
}

func TestServiceVolumesForQuerySkipsStaleNonMatchingUnderVolume(t *testing.T) {
	c := &serviceVolumeIndex{index: &Index{Volume: "C:"}, state: "stale", staleReason: "old checkpoint"}
	f := &serviceVolumeIndex{index: &Index{Volume: "F:"}, state: "ready"}
	got, err := serviceVolumesForQuery([]*serviceVolumeIndex{c, f}, queryOptions{Under: `F:\workspace\project`})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != f {
		t.Fatalf("volumes = %+v, want only ready F: volume", got)
	}
}

func TestServiceVolumesForQueryUsesLoadedStaleVolume(t *testing.T) {
	c := &serviceVolumeIndex{index: &Index{Volume: "C:"}, state: "stale", staleReason: "old checkpoint"}
	got, err := serviceVolumesForQuery([]*serviceVolumeIndex{c}, queryOptions{Under: `C:\Data`})
	if err != nil {
		t.Fatalf("error = %v, want loaded stale volume to remain searchable", err)
	}
	if len(got) != 1 || got[0] != c {
		t.Fatalf("volumes = %+v, want stale C: volume", got)
	}
}

func TestServiceVolumesForQueryRoutesPathVolumeTerm(t *testing.T) {
	c := &serviceVolumeIndex{index: &Index{Volume: "C:"}, state: "ready"}
	f := &serviceVolumeIndex{index: &Index{Volume: "F:"}, state: "ready"}
	cases := []string{
		"path:F: .nrrd",
		"path:F: .raw",
		"path:F: .pdf",
		"F: ext:pdf",
	}
	for _, query := range cases {
		t.Run(query, func(t *testing.T) {
			got, err := serviceVolumesForQuery([]*serviceVolumeIndex{c, f}, queryOptions{Query: query, MatchPath: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0] != f {
				t.Fatalf("volumes = %+v, want only F: volume", got)
			}
		})
	}
}

func TestServiceVolumesForQueryKeepsAllVolumesWithoutUniqueVolumeTerm(t *testing.T) {
	c := &serviceVolumeIndex{index: &Index{Volume: "C:"}, state: "ready"}
	f := &serviceVolumeIndex{index: &Index{Volume: "F:"}, state: "ready"}
	got, err := serviceVolumesForQuery([]*serviceVolumeIndex{c, f}, queryOptions{Query: "path:C:|F: .pdf", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("volumes = %+v, want both volumes for ambiguous volume query", got)
	}
}

func TestLockVolumeSearchCancelsWhileWaiting(t *testing.T) {
	vol := &serviceVolumeIndex{}
	vol.searchMu.Lock()
	defer vol.searchMu.Unlock()
	start := time.Now()
	locked, ok := lockVolumeSearch(vol, queryOptions{
		Cancel: func() bool { return true },
	})
	if ok || locked {
		t.Fatal("lockVolumeSearch acquired lock for canceled query")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("lockVolumeSearch took %s for canceled query, want quick return", elapsed)
		}
		t.Logf("lockVolumeSearch took %s, over soft 100ms budget", elapsed)
	}
}

func TestLooksLikeSearchWithoutSubcommand(t *testing.T) {
	if !looksLikeSearchWithoutSubcommand([]string{"ExamplePanel.cpp", "--under", `F:\workspace\app`}) {
		t.Fatal("expected omitted search command to be recognized")
	}
	if looksLikeSearchWithoutSubcommand([]string{"bogus"}) {
		t.Fatal("single unknown command should not be treated as omitted search")
	}
}

func TestNormalizeSearchArgsMovesFlagsBeforeQuery(t *testing.T) {
	got := normalizeSearchArgs([]string{"ExamplePanel.cpp", "--under", `F:\workspace\app`, "-path"})
	want := []string{"--under", `F:\workspace\app`, "-path", "ExamplePanel.cpp"}
	if !sameStringSet(got, want) || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalized args = %v, want %v", got, want)
	}
}

func TestNormalizeSearchArgsKeepsFuzzyFlagOutOfQuery(t *testing.T) {
	for _, flag := range []string{"-fuzzy", "--fuzzy"} {
		got := normalizeSearchArgs([]string{"acme", "lg", flag})
		if len(got) != 3 {
			t.Fatalf("%s: normalized args = %v, want 3 elements", flag, got)
		}
		if got[0] != flag {
			t.Fatalf("%s: flag should be moved before query terms, got %v", flag, got)
		}
		joined := strings.Join(got, " ")
		if strings.Contains(joined, "fuzzy") && !strings.HasPrefix(joined, flag) {
			t.Fatalf("%s: -fuzzy leaked into query text: %v", flag, got)
		}
	}
}

func TestServiceVolumeIndexFastPostingCountIncludesRecentAndDeleted(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	vol.recentIDs = map[int]struct{}{1: {}, 3: {}}
	rec := vol.index.compactRecord(1)
	rec.Deleted = true
	vol.index.setCompactRecord(1, rec)
	vol.index.appendCompactRecord(CompactRecord{FRN: 4, ParentFRN: 1, Parent: 0, Name: "new.go"})

	pq, err := parseQuery(queryOptions{Query: "type:file ext:go c:", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	count, ok := vol.fastPostingCount(pq)
	if !ok {
		t.Fatal("fastPostingCount declined safe posting query")
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
}

func TestServiceVolumeIndexFastPostingCountHandlesDirQueryWithPlanner(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	pq, err := parseQuery(queryOptions{Query: "dir:src ext:go", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	count, ok := vol.fastPostingCount(pq)
	if !ok {
		t.Fatal("fastPostingCount declined planned dir query")
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}

func TestServiceVolumeIndexFastPostingCountDeclinesBareFileType(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	pq, err := parseQuery(queryOptions{Query: "type:file"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vol.fastPostingCount(pq); ok {
		t.Fatal("fastPostingCount accepted bare type:file without a narrowing posting")
	}
}

func TestServiceVolumeIndexMultiNameTermCandidatesWarmsEachTerm(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	vol.index.appendCompactRecord(CompactRecord{FRN: 4, ParentFRN: 1, Parent: 0, Name: "alpha_report.txt"})
	vol.queryIndex = buildResidentQueryIndex(vol)

	got := vol.multiNameTermCandidates([]string{"alpha", "report"})
	if len(got) != 1 || got[0] != 3 {
		t.Fatalf("candidates = %v, want [3]", got)
	}
	if len(vol.termCache["alpha"].ids) != 1 || len(vol.termCache["report"].ids) != 1 {
		t.Fatalf("term cache not warmed for both terms: %#v", vol.termCache)
	}
	cached, ok := vol.cachedMultiNameTermCandidates([]string{"alpha", "report"})
	if !ok {
		t.Fatal("cachedMultiNameTermCandidates missed warmed terms")
	}
	if len(cached) != 1 || cached[0] != 3 {
		t.Fatalf("cached candidates = %v, want [3]", cached)
	}
}

func TestServiceVolumeIndexPathTermPostingFallsBackWithoutChildRanges(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil

	got := vol.pathTermPosting("main")
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("pathTermPosting fallback = %v, want [2]", got)
	}
}

func TestServiceVolumeIndexResidentMemoryInfoReflectsSkippedViews(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	vol.index.packCompactRecords(true)
	vol.queryIndex.nameOrder = nil
	vol.queryIndex.nameRank = nil
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil
	vol.rootIDs = nil
	vol.subtreeOrder = nil
	vol.subtreeStart = nil
	vol.subtreeEnd = nil

	info := vol.residentMemoryInfo()
	if info == nil {
		t.Fatal("residentMemoryInfo returned nil")
	}
	if info.Records != vol.index.compactRecordCount() {
		t.Fatalf("records = %d, want %d", info.Records, vol.index.compactRecordCount())
	}
	if info.NameBlobBytes == 0 || info.RecordBytes == 0 {
		t.Fatalf("expected record memory fields to be populated: %+v", info)
	}
	if info.NameOrderBytes != 0 || info.ChildBytes != 0 {
		t.Fatalf("skipped resident views still reported memory: %+v", info)
	}
	if info.ExtPostBytes == 0 {
		t.Fatalf("expected extension posting memory field: %+v", info)
	}
	if info.FRNIndexBytes == 0 || info.KnownBytes == 0 {
		t.Fatalf("expected FRN and known memory fields: %+v", info)
	}
}

func TestLowMemoryModeKeepsBoundedSearchPostingsByDefault(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := dottedPathBenchmarkIndex(1000)
	vol := newServiceVolumeIndex("test.gsi", idx)

	if vol.index.PackedRecords == nil || len(vol.index.Records) != 0 {
		t.Fatalf("lowmem synthetic index was not packed: packed=%v records=%d", vol.index.PackedRecords != nil, len(vol.index.Records))
	}
	if len(vol.frns) != 0 || len(vol.frnRecordIDs) != 0 {
		t.Fatalf("lowmem built resident FRN arrays: frns=%d ids=%d", len(vol.frns), len(vol.frnRecordIDs))
	}
	if len(vol.childOffsets) == 0 || len(vol.childIDs) == 0 || len(vol.subtreeOrder) != 0 {
		t.Fatalf("lowmem child/subtree views = offsets=%d ids=%d subtree=%d, want child ranges without subtree intervals", len(vol.childOffsets), len(vol.childIDs), len(vol.subtreeOrder))
	}
	if got := vol.nameOrderStateString(); got != "" {
		t.Fatalf("lowmem name order state = %q, want empty", got)
	}
	if got := vol.nameTrigramStateString(); got != "pending" {
		t.Fatalf("lowmem trigram state = %q, want pending", got)
	}
	if vol.queryIndex == nil || len(vol.queryIndex.ext) == 0 || len(vol.queryIndex.components) == 0 || len(vol.queryIndex.pathGrams) == 0 {
		t.Fatal("lowmem should keep extension/component/path-gram postings by default")
	}
	info := vol.residentMemoryInfo()
	if info == nil || info.NameOrderBytes != 0 || info.NameTrigramBytes != 0 || info.ChildBytes == 0 || info.FRNIndexBytes != 0 {
		t.Fatalf("lowmem memory still includes skipped accelerators: %+v", info)
	}
}

func TestTrimSearchCachesDoesNotForceMemoryRelease(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	for i := 0; i < 32; i++ {
		vol.trimSearchCachesLocked()
	}
	if vol.searchCount != 32 {
		t.Fatalf("searchCount = %d, want 32", vol.searchCount)
	}
	if vol.pathCache == nil {
		t.Fatal("path cache was cleared unexpectedly")
	}
}

func TestServiceVolumeIndexPersistFailureBackoffRetainsQueryableState(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	vol.state = "ready"
	vol.staleReason = ""
	now := time.Unix(100, 0)

	vol.notePersistFailureLocked(errors.New("not enough space on the disk"), now)

	if vol.state != "ready" || vol.staleReason != "" {
		t.Fatalf("persist failure changed query state: state=%q stale=%q", vol.state, vol.staleReason)
	}
	if vol.persistFailures != 1 {
		t.Fatalf("persistFailures = %d, want 1", vol.persistFailures)
	}
	if got, want := vol.persistRetryAfter.Sub(now), time.Minute; got != want {
		t.Fatalf("retry delay = %s, want %s", got, want)
	}
	if vol.lastPersistErr == "" {
		t.Fatal("lastPersistErr was not recorded")
	}
}

func TestPersistFailureBackoffCaps(t *testing.T) {
	if got := persistFailureBackoff(1); got != time.Minute {
		t.Fatalf("first backoff = %s, want 1m", got)
	}
	if got := persistFailureBackoff(9); got != 32*time.Minute {
		t.Fatalf("capped backoff = %s, want 32m", got)
	}
}

func TestServiceWildcardUnderQueryMatchesWithoutPathMode(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	got, err := searchCompactWithCache(idx, queryOptions{
		Query: "*_test.go",
		Under: `C:\fixture\workspace\src`,
		Limit: 20,
	}, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "search_test.go" {
		t.Fatalf("matches = %+v, want [search_test.go]", got)
	}
}

func TestUnderQueryFallsBackToPathFilterWhenRootRecordIsDeleted(t *testing.T) {
	idx := commonSearchFixture()
	root := idx.compactRecord(2) // C:\fixture\workspace
	root.Deleted = true
	idx.setCompactRecord(2, root)
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	got, err := searchCompactWithCache(idx, queryOptions{
		Query: "readme",
		Under: `C:\fixture\workspace`,
		Limit: 20,
	}, false, make(map[int]string), vol.plannedCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"README.md", "readme.md"}) {
		t.Fatalf("matches = %v, want readme files under deleted root record", names)
	}
}

func TestServiceSearchFallsBackToFilesystemForScopedIndexMiss(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "specific_fixture_tool.py"), []byte("pass"), 0o644); err != nil {
		t.Fatal(err)
	}
	volume := filepath.VolumeName(root)
	idx := &Index{
		Source:  "usn",
		Volume:  volume,
		Compact: true,
		Records: []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)

	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "specific_fixture_tool.py",
		Under: root,
		Limit: 20,
		Trace: trace,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "specific_fixture_tool.py" {
		t.Fatalf("matches = %+v, want filesystem fallback result", got)
	}
	if trace.Fallback != "filesystem-under-fallback" || trace.Complete == nil || !*trace.Complete {
		t.Fatalf("trace fallback=%q complete=%v, want filesystem-under-fallback complete=true", trace.Fallback, trace.Complete)
	}
}

func TestFilesystemUnderFallbackIsBounded(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 12; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("item-%02d.txt", i)), []byte("pass"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, complete := filesystemUnderFallbackSearchLimited(queryOptions{
		Query: "item-11.txt",
		Under: root,
		Limit: 20,
	}, false, 3, time.Minute)
	if ok {
		t.Fatalf("fallback returned %v after maxVisited cap; want no result", got)
	}
	if complete {
		t.Fatal("fallback complete = true after maxVisited cap, want false")
	}
}

func TestFilesystemUnderFallbackReportsCompleteWhenExhausted(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "specific_fixture_tool.py"), []byte("pass"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok, complete := filesystemUnderFallbackSearchLimited(queryOptions{
		Query: "specific_fixture_tool.py",
		Under: root,
		Limit: 20,
	}, false, 100, time.Minute)
	if !ok || len(got) != 1 {
		t.Fatalf("fallback returned %v ok=%v, want one result", got, ok)
	}
	if !complete {
		t.Fatal("fallback complete = false after exhausting tree, want true")
	}
}

func TestExchangeServiceJSONTimesOutHungQuery(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		defer server.Close()
		var req serviceRequest
		_ = json.NewDecoder(server).Decode(&req)
		time.Sleep(200 * time.Millisecond)
	}()

	start := time.Now()
	_, err := exchangeServiceJSON(client, serviceRequest{Command: "search"}, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("timeout elapsed = %s, want under 150ms", elapsed)
		}
		t.Logf("timeout elapsed = %s, over soft 150ms budget", elapsed)
	}
}

func TestStandaloneServicePipeSearchEndToEnd(t *testing.T) {
	vol := syntheticServiceVolumeIndexForCacheTests()
	idx := vol.index
	pipeName := fmt.Sprintf(`\\.\pipe\seekfs-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	svc := &goSearchService{
		pipeName: pipeName,
		// The production SDDL intentionally limits access to interactive users.
		// The test process is not guaranteed to carry an interactive-token SID,
		// so use an owned, unique pipe that is explicitly accessible to the
		// authenticated test user instead of weakening the production ACL.
		sddl:    standaloneTestPipeSDDL,
		stop:    make(chan struct{}),
		indexes: []*Index{idx},
		volumes: []*serviceVolumeIndex{vol},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.servePipeListener()
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() { close(svc.stop) })
		if handle, err := openPipeClientWithTimeout(pipeName, 100*time.Millisecond); err == nil {
			_ = os.NewFile(uintptr(handle), pipeName).Close()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("standalone service listener did not stop")
		}
	}
	t.Cleanup(stop)

	info, err := callService(pipeName, serviceRequest{Command: "info"})
	if err != nil {
		t.Fatalf("info request failed: %v", err)
	}
	if !info.OK || info.Entries != idx.entryCount() {
		t.Fatalf("info response = %+v, want ok entries=%d", info, idx.entryCount())
	}

	search, err := callService(pipeName, serviceRequestFromOptions(queryOptions{
		Query: "ext:go",
		Limit: 20,
	}, false))
	if err != nil {
		t.Fatalf("search request failed: %v", err)
	}
	if !search.OK {
		t.Fatalf("search response = %+v, want ok", search)
	}
	if search.Count != 1 || len(search.Rows) != 1 || search.Rows[0].Name != "main.go" {
		t.Fatalf("search rows = %+v count=%d, want main.go", search.Rows, search.Count)
	}
	if search.SearchMS < 0 {
		t.Fatalf("search_ms = %f, want non-negative", search.SearchMS)
	}
	if search.Source == "" || search.Candidates == 0 {
		t.Fatalf("search trace source=%q candidates=%d, want populated service metadata", search.Source, search.Candidates)
	}

	count, err := callService(pipeName, serviceRequestFromOptions(queryOptions{
		Query: "ext:go",
		Limit: 20,
	}, true))
	if err != nil {
		t.Fatalf("count request failed: %v", err)
	}
	if !count.OK {
		t.Fatalf("count response = %+v, want ok", count)
	}
	if count.Count != 1 || len(count.Rows) != 0 || len(count.Results) != 0 {
		t.Fatalf("count response rows=%+v results=%+v count=%d, want count-only metadata", count.Rows, count.Results, count.Count)
	}
	if count.PlannerMode == "" || count.Source == "" || len(count.EligibleVolumes) == 0 || count.Complete == nil || !*count.Complete {
		t.Fatalf("count trace planner=%q source=%q eligible=%v complete=%v, want populated metadata", count.PlannerMode, count.Source, count.EligibleVolumes, count.Complete)
	}
}

func TestReplayWALWithLimitMarksOversizedWALRebuildable(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.gsi")
	if err := os.WriteFile(walPath(db), []byte("oversized wal"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol := &serviceVolumeIndex{dbPath: db, volume: "F:"}
	err := vol.replayWALWithLimit(4)
	if err == nil {
		t.Fatal("expected oversized wal error")
	}
	if !shouldRebuildStaleIndex(err) {
		t.Fatalf("err = %v, want rebuildable stale index error", err)
	}
}

func TestServiceCallTimeoutLeavesIndexRequestsUnlimited(t *testing.T) {
	if got := serviceCallTimeout(serviceRequest{Command: "search"}); got != serviceQueryTimeout {
		t.Fatalf("search timeout = %s, want %s", got, serviceQueryTimeout)
	}
	if got := serviceCallTimeout(serviceRequest{Command: "index-usn"}); got != 0 {
		t.Fatalf("index-usn timeout = %s, want 0", got)
	}
}

func TestServiceRequestRoundTripPreservesControlFields(t *testing.T) {
	opts := queryOptions{
		Query:        "ext:nrrd",
		MatchPath:    true,
		Limit:        200,
		DeadlineUnix: 123456789,
		RequestSeq:   42,
	}
	req := serviceRequestFromOptions(opts, false)
	if req.DeadlineUnix != opts.DeadlineUnix || req.RequestSeq != opts.RequestSeq {
		t.Fatalf("request deadline/seq = (%d, %d), want (%d, %d)", req.DeadlineUnix, req.RequestSeq, opts.DeadlineUnix, opts.RequestSeq)
	}
	got := requestToOptionsFromService(req)
	if got.DeadlineUnix != opts.DeadlineUnix || got.RequestSeq != opts.RequestSeq || got.Query != opts.Query || !got.MatchPath || got.Limit != opts.Limit {
		t.Fatalf("round trip options = %+v, want query/deadline/seq preserved", got)
	}
}

func TestServiceRequestStaysNameModeForPlainMultiTerm(t *testing.T) {
	req := serviceRequestFromOptions(queryOptions{Query: "Downloads nrrd"}, false)
	if req.MatchPath {
		t.Fatal("plain multi-term query must stay name mode; loose path inference would force a slow path scan")
	}
	req = serviceRequestFromOptions(queryOptions{Query: "path:Downloads nrrd"}, false)
	if !req.MatchPath {
		t.Fatal("explicit path: filter must force service path mode")
	}
	req = serviceRequestFromOptions(queryOptions{Query: "ext:raw !path:Assets"}, false)
	if req.MatchPath {
		t.Fatal("negated path filter should not force top-level service path mode")
	}
}

func syntheticServiceVolumeIndexForCacheTests() *serviceVolumeIndex {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: "."},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "one.txt"},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "main.go"},
		},
	}
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.termCache = make(map[string]postingCacheEntry)
	vol.pathTermCache = make(map[string]postingCacheEntry)
	vol.extCache = make(map[string]postingCacheEntry)
	return vol
}

// Trigram name postings are cached as ID-sorted lists; the filename planner
// rank-sorts its candidates in place, so handing out the cached slice let a
// search reorder the cache and break later sorted intersections.
func TestNameTrigramPostingCacheStaysSortedAfterSearch(t *testing.T) {
	idx := randomCorpusIndex(59, randomCorpusParams{Records: 240})
	vol := newServiceVolumeIndex("trigram-cache.gsi", idx)
	vol.rebuildNameTrigramsLocked()
	opts := queryOptions{Query: "md 020", Limit: 25}
	want, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	for key, entry := range vol.termCache {
		if !sort.IntsAreSorted(entry.ids) {
			t.Fatalf("cached posting %q reordered by search: %v", key, entry.ids)
		}
	}
	negative := opts
	negative.Query = "md 020 !no-such-term-xq9"
	got, err := searchCompactWithCache(idx, negative, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if !sameOrderedStrings(pathsOf(got), pathsOf(want)) {
		t.Fatalf("negated miss changed results: %v, want %v", pathsOf(got), pathsOf(want))
	}
}
