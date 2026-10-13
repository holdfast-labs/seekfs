package main

import (
	"runtime"
	"testing"
)

// contentParallelShapes exercises every parallel content path: the bounded
// scan early-stop (broad terms), the candidate verify with early stop
// (selective/compound/phrase), the full-window verify (relevance,
// unlimited), the regex prefilter + scan arms, and the count tally.
func contentParallelShapes() []string {
	return []string{
		"content:download",
		"content:pelican",
		"content:pelican content:download",
		"ext:nrrd content:download",
		`content:"quick brown fox"`,
		`content:/dow\w+/`,
		`content:/\d+/`,
		"content:download sort:relevance",
		"content:pelican sort:relevance",
	}
}

// runContentParallelCase runs one query at workers-forcing GOMAXPROCS and
// returns paths, snippets and the trace completeness signal.
func runContentParallelCase(t *testing.T, vols []*serviceVolumeIndex, query string, limit int, gomax int) ([]Entry, int, searchTrace) {
	t.Helper()
	prev := runtime.GOMAXPROCS(gomax)
	defer runtime.GOMAXPROCS(prev)
	trace := &searchTrace{}
	matches, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: limit, Trace: trace}, false)
	if err != nil {
		t.Fatalf("gomax=%d %q limit=%d: %v", gomax, query, limit, err)
	}
	n, ok, err := countServiceVolumes(vols, queryOptions{Query: query, Trace: &searchTrace{}})
	if err != nil || !ok {
		t.Fatalf("gomax=%d count %q: %d %v %v", gomax, query, n, ok, err)
	}
	return matches, n, *trace
}

// TestContentParallelContentMatchesSerial proves the parallel content
// verifier (bounded scan, candidate verify, count tally) returns exactly
// what the serial walk does: same sets, order, snippets, counts and
// completeness, across shapes x limits x worker counts.
func TestContentParallelContentMatchesSerial(t *testing.T) {
	// 5000 docs: the broad term exceeds the limit-20 window (4096), so the
	// rank-ordered scan (A), the candidate verify (B) and the count tally
	// (C) all engage their parallel arms at GOMAXPROCS 2+.
	t.Setenv("SEEKFS_CONTENT_BENCH_DOCS", "5000")
	vols := contentBenchVolumes(t, 1)
	for _, query := range contentParallelShapes() {
		for _, limit := range []int{5, 20, 100} {
			serial, serialCount, serialTrace := runContentParallelCase(t, vols, query, limit, 1)
			for _, workers := range []int{2, 4} {
				got, count, trace := runContentParallelCase(t, vols, query, limit, workers)
				if len(got) != len(serial) {
					t.Fatalf("%q limit=%d workers=%d: %d hits; serial %d", query, limit, workers, len(got), len(serial))
				}
				for i := range got {
					if got[i].Path != serial[i].Path || got[i].Snippet != serial[i].Snippet {
						t.Fatalf("%q limit=%d workers=%d hit %d: got (%q,%q); serial (%q,%q)",
							query, limit, workers, i, got[i].Path, got[i].Snippet, serial[i].Path, serial[i].Snippet)
					}
				}
				if count != serialCount {
					t.Fatalf("%q limit=%d workers=%d: count=%d; serial %d", query, limit, workers, count, serialCount)
				}
				if trace.ContentIncomplete != serialTrace.ContentIncomplete ||
					trace.ContentPartial != serialTrace.ContentPartial {
					t.Fatalf("%q limit=%d workers=%d: flags %+v; serial %+v", query, limit, workers, trace, serialTrace)
				}
			}
		}
	}
}

// TestContentParallelCountMatchesSearchSum proves parallel counts tally the
// same total the (bounded) search page draws from, on shapes where the
// count must equal the full search.
func TestContentParallelCountMatchesSearchSum(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_BENCH_DOCS", "5000")
	vols := contentBenchVolumes(t, 1)
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	for _, query := range []string{"content:pelican", "content:download", `content:/dow\w+/`} {
		n, ok, err := countServiceVolumes(vols, queryOptions{Query: query, Trace: &searchTrace{}})
		if err != nil || !ok {
			t.Fatalf("count %q: %d %v %v", query, n, ok, err)
		}
		all, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: 100000}, false)
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		// A capped broad query reports incomplete rather than a silent
		// truncation; only complete searches must equal the count.
		tr := &searchTrace{}
		if _, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: 100000, Trace: tr}, false); err != nil {
			t.Fatal(err)
		}
		if !tr.ContentIncomplete && len(all) != n {
			t.Fatalf("%q: search=%d count=%d", query, len(all), n)
		}
		if tr.ContentIncomplete && len(all) > n {
			t.Fatalf("%q: incomplete search=%d exceeds count=%d", query, len(all), n)
		}
	}
}
