package main

import (
	"fmt"
	"testing"
)

// contentLargeBudgetRegexRecords builds a single-volume corpus for a pure regex
// with no provable literal prefilter (`/a.b/`): every file is a scan candidate
// but only 1 in 10 contains a match. This is the live low-precision shape (a
// regex over tens of thousands of candidates where ~1 in 11 files truly match)
// whose candidate superset cannot be materialized from postings.
func contentLargeBudgetRegexRecords(volume string, n int) []contentVolRecord {
	recs := make([]contentVolRecord, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("scan-%06d.txt", i)
		body := "hello world nothing to see"
		if i%10 == 0 {
			body = "aZb the real match here"
		}
		recs = append(recs, contentVolRecord{
			frn: uint64(i) + 2, parent: -1, parentFRN: 1,
			name: name, size: int64(i + 1), modUnix: int64(1_600_000_000 + i),
			path: volume + `\` + name, content: body,
		})
	}
	return recs
}

// contentLargeBudgetLiteralRecords is the literal twin: every file carries the
// query's trigram evidence (the candidate posting superset) but only 1 in 10
// contains the full literal.
func contentLargeBudgetLiteralRecords(volume string, n int) []contentVolRecord {
	recs := make([]contentVolRecord, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("scan-%06d.txt", i)
		body := "aba and bab are here"
		if i%10 == 0 {
			body = "ababab the real match here"
		}
		recs = append(recs, contentVolRecord{
			frn: uint64(i) + 2, parent: -1, parentFRN: 1,
			name: name, size: int64(i + 1), modUnix: int64(1_600_000_000 + i),
			path: volume + `\` + name, content: body,
		})
	}
	return recs
}

// TestContentLargeBudgetVerifyBounded pins the page cost independently of the
// explicit disk-verify budget. A pure-regex query with no provable literal
// prefilter (so no posting superset to probe completeness from) fills its page
// in the reduced default-order path, but the reduced path cannot decide the
// completeness probe: the driver then widens back to the 10k completeness
// window. That widen re-scans the volume and, with a large explicit
// --content-verify-budget, reads far past the filled page. The probe must stay
// bounded so a large budget yields the same page and the same reads as the
// default budget. The plain-literal twin (a known posting superset) already
// stays bounded and guards against a regression there.
func TestContentLargeBudgetVerifyBounded(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")

	const (
		files = 600
		limit = 20
	)

	cases := []struct {
		name  string
		query string
		recs  func(string, int) []contentVolRecord
	}{
		{"regex-unknown-probe", `content:/a.b/`, contentLargeBudgetRegexRecords},
		{"literal-known-superset", "content:ababab", contentLargeBudgetLiteralRecords},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := newContentRecordVolume(t, "C:", tc.recs("C:", files))
			vols := []*serviceVolumeIndex{vol}

			prev := contentDiskReadForVerify
			reads := 0
			contentDiskReadForVerify = func(path string, s contentExtractSettings) ([]byte, bool) {
				reads++
				return prev(path, s)
			}
			t.Cleanup(func() { contentDiskReadForVerify = prev })

			run := func(budget int) ([]Entry, int, bool) {
				reads = 0
				tr := &searchTrace{}
				matches, err := searchServiceVolumes(vols, queryOptions{
					Query: tc.query, Limit: limit, Trace: tr,
					ContentDiskVerifyBudget: budget,
				}, false)
				if err != nil {
					t.Fatalf("budget=%d: %v", budget, err)
				}
				return matches, reads, traceCompleteValue(tr)
			}

			defMatches, defReads, _ := run(0)
			bigMatches, bigReads, _ := run(100_000)

			if len(defMatches) != limit {
				t.Fatalf("default budget page = %d hits; want %d", len(defMatches), limit)
			}
			if len(bigMatches) != len(defMatches) {
				t.Fatalf("large-budget page = %d hits; want same %d", len(bigMatches), len(defMatches))
			}
			// One verify batch is 4*workers; the page itself costs ~limit/precision.
			batch := 4 * contentParallelWorkers()
			if bigReads > defReads+batch {
				t.Fatalf("large budget read %d files (default %d, batch %d); a large budget must not widen the verify set",
					bigReads, defReads, batch)
			}
			t.Logf("query=%q default reads=%d large reads=%d", tc.query, defReads, bigReads)
		})
	}
}
