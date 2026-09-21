package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
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

// BenchmarkContentVsFilename compares filename and content queries on the same
// synthetic corpus, single-volume and 4-volume, at Limit 20. It is a
// measurement harness: the loops are the only timed work (b.ResetTimer after
// one untimed warm/verify call).
func BenchmarkContentVsFilename(b *testing.B) {
	single := contentBenchVolumes(b, 1)
	multi := contentBenchVolumes(b, 4)

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
