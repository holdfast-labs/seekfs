package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// contentBenchDocCount is the synthetic corpus size per volume. It defaults to
// 50k and can be shrunk with SEEKFS_CONTENT_BENCH_DOCS when a machine cannot
// afford the fixture build.
func contentBenchDocCount() int {
	if v := os.Getenv("SEEKFS_CONTENT_BENCH_DOCS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 50_000
}

// contentBenchFiles builds one volume's synthetic records+documents directly
// (no file IO/extraction). Names carry a broad term ("nrrd", every record) and
// a selective token ("quasar", a handful); bodies carry a broad term
// ("download", every doc), a selective term ("pelican", a few) and a phrase
// ("quick brown fox", some). Body length varies with i%7.
func contentBenchFiles(volumeIndex int) []contentFixtureFile {
	const (
		selectiveNameEvery = 6_250 // ~8 of 50k
		selectiveTermEvery = 3_125 // ~16 of 50k
		phraseEvery        = 50    // ~1k of 50k
	)
	n := contentBenchDocCount()
	files := make([]contentFixtureFile, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("scan-%06d.nrrd", i)
		if i%selectiveNameEvery == 0 {
			name = fmt.Sprintf("quasar-scan-%06d.nrrd", i)
		}
		var body strings.Builder
		for r := 0; r <= i%7; r++ {
			body.WriteString("the archive worker syncs the download cache to the remote volume every interval ")
		}
		if i%phraseEvery == 0 {
			body.WriteString("the quick brown fox jumps over the lazy dog by the river ")
		}
		if i%selectiveTermEvery == 0 {
			body.WriteString("pelican ")
		}
		files = append(files, contentFixtureFile{
			frn:  uint64(volumeIndex)*1_000_000 + uint64(i) + 2,
			name: name,
			text: body.String(),
		})
	}
	return files
}

// contentBenchVolumes builds count independent volumes of the same shape with
// distinct volume names and FRNs.
func contentBenchVolumes(b testing.TB, count int) []*serviceVolumeIndex {
	b.Helper()
	vols := make([]*serviceVolumeIndex, 0, count)
	for v := 0; v < count; v++ {
		vol := newContentQueryVolumeNamed(b, string(rune('C'+v))+":", contentBenchFiles(v))
		// Steady-state fixture: the running service builds the resident name
		// trigram index in the background, so build it here before timing (it
		// is what the service's own rebuildNameTrigramsLocked does).
		vol.rebuildNameTrigramsLocked()
		vols = append(vols, vol)
	}
	return vols
}

// contentBenchProfileIters bounds the gated attribution loop so profiling stays
// cheap relative to the benchmark itself.
const contentBenchProfileIters = 200

// contentBenchProfilePhase logs a per-op attribution of the content-broad query
// across the phases PB8 targets. It is deliberately gated
// (SEEKFS_CONTENT_BENCH_PROFILE=1) and runs untimed work before the benchmark
// loops. The phases are measured by calling the same helpers the query path
// uses, on the same steady-state fixture, in nanoseconds per op:
//
//   - candidate materialization + rank sort: contentCandidatesBounded + sortCandidateIDs
//   - entry construction:                     compactEntryFromRecordPath
//   - text read:                              contentTextForEntry
//   - text fold:                              contentFoldText
//   - final order sort:                       sortSearchAllEntries
//
// Before PB8 the candidate phase materialized the whole posting superset (the
// full query budget); after PB8 it is capped at the relevance window and, when
// that cap is hit, the query switches to the rank-ordered bounded scan, so the
// entry/text phases run only for the page. The attribution makes that visible.
//
// Measured on the 50k synthetic corpus (SEEKFS_CONTENT_BENCH_PROFILE=1,
// single/content-broad, ns/op):
//
//	candidate materialization + rank sort: ~395000   (probe capped at the 4096 window)
//	entry construction (page of 20):       ~7700
//	text read (page of 20):                ~0
//	text fold (page of 20):                ~10000
//	final order sort (page of 20):         ~2500
//	sum:                                   ~416000
//
// The same phases run over all 4096 window candidates before PB8 (~5.2 ms/op
// dominated by entry construction + fold), which is why the page-limit
// reduction matters: candidate materialization is bounded, and verification is
// proportional to the page rather than the window.
func contentBenchProfilePhase(b *testing.B, vols []*serviceVolumeIndex) {
	b.Helper()
	if os.Getenv("SEEKFS_CONTENT_BENCH_PROFILE") != "1" || len(vols) == 0 {
		return
	}
	vol := vols[0]
	pq, err := parseQuery(queryOptions{Query: "content:download", Limit: 20})
	if err != nil {
		b.Fatalf("parseQuery: %v", err)
	}
	window := contentRelevanceWindow(20, contentCandidateBudgetOf(pq))
	if window <= 0 {
		window = 4096
	}
	pq.Limit = window
	pq.Trace = &searchTrace{}

	start := time.Now()
	var candidates []int
	for i := 0; i < contentBenchProfileIters; i++ {
		c, ok, _ := vol.contentCandidatesBounded(pq, window)
		if !ok {
			b.Fatal("contentCandidatesBounded declined a broad content term")
		}
		sortCandidateIDs(c, pq, vol.index, vol.rankForQuery(pq))
		candidates = c
	}
	candNs := time.Since(start).Nanoseconds() / contentBenchProfileIters
	probeCandidates := len(candidates)
	// After PB8 only the page (the user limit) is verified and materialized as
	// Entries; the bounded scan stops there. Measure those phases over the page
	// so the attribution reflects the bounded path.
	const page = 20
	if len(candidates) > page {
		candidates = candidates[:page]
	}

	entries := make([]Entry, 0, len(candidates))
	start = time.Now()
	for i := 0; i < contentBenchProfileIters; i++ {
		pathCache := make(map[int]string)
		entries = entries[:0]
		for _, id := range candidates {
			entries = append(entries, compactEntryFromRecordPath(vol.index, id, vol.index.compactRecord(id), pathCache, false, true))
		}
	}
	entryNs := time.Since(start).Nanoseconds() / contentBenchProfileIters

	start = time.Now()
	textBytes := 0
	texts := make([][]byte, len(entries))
	for i := 0; i < contentBenchProfileIters; i++ {
		textBytes = 0
		for j := range entries {
			if text, ok := vol.contentTextForEntry(&entries[j]); ok {
				texts[j] = text
				textBytes += len(text)
				continue
			}
			texts[j] = nil
		}
	}
	textNs := time.Since(start).Nanoseconds() / contentBenchProfileIters

	start = time.Now()
	for i := 0; i < contentBenchProfileIters; i++ {
		for j := range texts {
			if texts[j] != nil {
				_ = contentFoldText(texts[j])
			}
		}
	}
	foldNs := time.Since(start).Nanoseconds() / contentBenchProfileIters

	start = time.Now()
	for i := 0; i < contentBenchProfileIters; i++ {
		sortSearchAllEntries(entries, pq)
	}
	sortNs := time.Since(start).Nanoseconds() / contentBenchProfileIters

	b.Logf("content-broad attribution (ns/op, probed candidates=%d window=%d page=%d text=%dB):\n"+
		"  candidate materialization + rank sort: %d\n"+
		"  entry construction (page):            %d\n"+
		"  text read (page):                     %d\n"+
		"  text fold (page):                     %d\n"+
		"  final order sort (page):              %d\n"+
		"  sum:                                  %d",
		probeCandidates, window, len(candidates), textBytes, candNs, entryNs, textNs, foldNs, sortNs,
		candNs+entryNs+textNs+foldNs+sortNs)
}

// BenchmarkContentVsFilename compares filename and content queries on the same
// synthetic corpus, single-volume and 4-volume, at Limit 20. It is a
// measurement harness: the loops are the only timed work (b.ResetTimer after
// one untimed warm/verify call).
func BenchmarkContentVsFilename(b *testing.B) {
	single := contentBenchVolumes(b, 1)
	multi := contentBenchVolumes(b, 4)
	contentBenchProfilePhase(b, single)

	cases := []struct {
		name  string
		query string
	}{
		{"filename-broad", "nrrd"},
		{"filename-selective", "quasar"},
		{"content-broad", "content:download"},
		{"content-selective", "content:pelican"},
		{"content-phrase", `content:"quick brown fox"`},
	}
	scopes := []struct {
		label string
		vols  []*serviceVolumeIndex
	}{{"single", single}, {"multi", multi}}

	for _, scope := range scopes {
		for _, tc := range cases {
			b.Run(scope.label+"/"+tc.name, func(b *testing.B) {
				opts := queryOptions{Query: tc.query, Limit: 20}
				matches, err := searchServiceVolumes(scope.vols, opts, false)
				if err != nil {
					b.Fatalf("searchServiceVolumes(%q): %v", tc.query, err)
				}
				if len(matches) == 0 {
					b.Fatalf("searchServiceVolumes(%q) returned no matches", tc.query)
				}
				b.ReportMetric(float64(len(matches)), "results")
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := searchServiceVolumes(scope.vols, opts, false); err != nil {
						b.Fatalf("searchServiceVolumes(%q): %v", tc.query, err)
					}
				}
			})
		}
	}
}

// BenchmarkContentCountVsFilename measures the count path on the same synthetic
// corpus as BenchmarkContentVsFilename. Counts tally in place and never
// materialize Entries or apply a result window; a broad content count is bounded
// by the candidate/visit budgets, not by any limit, so its cost tracks the
// budget rather than the result count.
func BenchmarkContentCountVsFilename(b *testing.B) {
	vols := contentBenchVolumes(b, 1)
	cases := []struct{ name, query string }{
		{"filename-broad", "nrrd"},
		{"content-broad", "content:download"},
		{"content-selective", "content:pelican"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			n, ok, err := countServiceVolumes(vols, queryOptions{Query: tc.query})
			if err != nil || !ok {
				b.Fatalf("count %q = %d, %v, %v", tc.query, n, ok, err)
			}
			if n == 0 {
				b.Fatalf("count %q = 0", tc.query)
			}
			b.ReportMetric(float64(n), "results")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok, err := countServiceVolumes(vols, queryOptions{Query: tc.query}); err != nil || !ok {
					b.Fatalf("count %q: ok=%v err=%v", tc.query, ok, err)
				}
			}
		})
	}
}

// contentBenchMeasureAllocBytes returns the total bytes allocated per call of fn,
// measured with runtime.MemStats so the regression gate is framework-free and
// deterministic.
func contentBenchMeasureAllocBytes(tb testing.TB, times int, fn func()) uint64 {
	tb.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < times; i++ {
		fn()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / uint64(times)
}

// PB8 regression gate. Before PB8 a broad content query materialized the whole
// posting superset and verified the full 4096-candidate window (~80 MB/op at
// 50k docs); after PB8 it verifies only the page and bounds the superset to the
// window (~0.2 MB/op at 12k docs). The thresholds are deliberately loose so a
// return of the old behavior (tens of MB/op) fails while normal noise cannot:
// the ratio gate is 300x the filename-broad query and the absolute cap is 8 MB.
func TestContentBroadAllocationBudget(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_BENCH_DOCS", "12000")
	vols := contentBenchVolumes(t, 1)

	const (
		contentBroadMaxBytes   = 8 << 20
		filenameRatioNumerator = 300
	)
	contentBroad := contentBenchMeasureAllocBytes(t, 20, func() {
		if _, err := searchServiceVolumes(vols, queryOptions{Query: "content:download", Limit: 20}, false); err != nil {
			t.Fatalf("content-broad: %v", err)
		}
	})
	filenameBroad := contentBenchMeasureAllocBytes(t, 20, func() {
		if _, err := searchServiceVolumes(vols, queryOptions{Query: "nrrd", Limit: 20}, false); err != nil {
			t.Fatalf("filename-broad: %v", err)
		}
	})
	if contentBroad > contentBroadMaxBytes {
		t.Fatalf("content-broad allocation regressed: %d B/op > %d B/op", contentBroad, contentBroadMaxBytes)
	}
	if filenameBroad > 0 && contentBroad > filenameBroad*filenameRatioNumerator {
		t.Fatalf("content-broad allocation ratio regressed: %d B/op > %d x %d B/op (filename-broad)",
			contentBroad, filenameRatioNumerator, filenameBroad)
	}
	// A selective content term has no broad superset to bound and must not
	// regress either.
	selective := contentBenchMeasureAllocBytes(t, 20, func() {
		if _, err := searchServiceVolumes(vols, queryOptions{Query: "content:pelican", Limit: 20}, false); err != nil {
			t.Fatalf("content-selective: %v", err)
		}
	})
	if selective > contentBroadMaxBytes {
		t.Fatalf("content-selective allocation regressed: %d B/op > %d B/op", selective, contentBroadMaxBytes)
	}
}
