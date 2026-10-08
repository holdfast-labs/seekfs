package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"
)

func TestGlobalScalarFiltersDefaultAcrossVolumes(t *testing.T) {
	t.Setenv("SEEKFS_NAME_ORDER", "1")
	volumes := globalScalarTestVolumes()
	tests := []struct {
		name       string
		opts       queryOptions
		paths      []string
		wantSource string
	}{
		{
			name:       "type",
			opts:       queryOptions{Query: "type:file", Limit: 20},
			paths:      []string{`F:\a-mid.txt`, `C:\c-small.txt`, `F:\newest.txt`, `C:\z-big.txt`},
			wantSource: "global:scalar-scan",
		},
		{
			name:       "size",
			opts:       queryOptions{Query: "size:>=100 sort:size", Limit: 20},
			paths:      []string{`F:\a-mid.txt`, `F:\newest.txt`, `C:\z-big.txt`},
			wantSource: "global:scalar-range",
		},
		{
			name:       "date",
			opts:       queryOptions{Query: "dm:2026-07-03 sort:modified", Limit: 20},
			paths:      []string{`F:\newest.txt`, `F:\a-mid.txt`},
			wantSource: "global:scalar-range",
		},
		{
			name:       "modified-after",
			opts:       queryOptions{ModifiedAfter: "2026-07-02T12:00:00Z", Limit: 20},
			paths:      []string{`F:\a-mid.txt`, `F:\newest.txt`},
			wantSource: "global:scalar-range",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			trace := &searchTrace{}
			opts := tc.opts
			opts.Trace = trace
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-scalar" || trace.Source != tc.wantSource || trace.Complete == nil || !*trace.Complete {
				t.Fatalf("trace = %+v, want complete global scalar source %q", trace, tc.wantSource)
			}
			if paths := pathsOf(got); !slices.Equal(paths, tc.paths) {
				t.Fatalf("paths = %v, want %v", paths, tc.paths)
			}

			countTrace := &searchTrace{}
			countOpts := tc.opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || count != len(tc.paths) {
				t.Fatalf("count = %d ok=%v, want %d true", count, ok, len(tc.paths))
			}
			if countTrace.PlannerMode != "global-count-scalar" || countTrace.Source != tc.wantSource {
				t.Fatalf("count trace = %+v, want global-count-scalar source %q", countTrace, tc.wantSource)
			}

			countMatches, err := searchServiceVolumes(volumes, countOpts, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(countMatches) != count {
				t.Fatalf("count-search len = %d, count = %d", len(countMatches), count)
			}
		})
	}
}

func TestGlobalScalarVolumeAnchorKeepsMappedRangeAndCardinality(t *testing.T) {
	volumes := globalScalarTestVolumes()
	query := "C: size:>=100"
	want, err := searchAll([]*Index{volumes[0].index, volumes[1].index}, queryOptions{Query: query, Limit: 20}, false)
	if err != nil {
		t.Fatal(err)
	}

	searchRunTrace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: query, Limit: 20, Trace: searchRunTrace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pathsOf(got), pathsOf(want)) {
		t.Fatalf("anchored scalar paths=%v want=%v", pathsOf(got), pathsOf(want))
	}
	if searchRunTrace.Source != "global:scalar-range" || searchRunTrace.ScalarDriver == "" || searchRunTrace.ScalarInterval == 0 {
		t.Fatalf("anchored scalar search trace=%+v, want mapped scalar range", searchRunTrace)
	}

	countTrace := &searchTrace{}
	count, handled, err := countServiceVolumes(volumes, queryOptions{Query: query, Trace: countTrace})
	if err != nil || !handled || count != len(want) {
		t.Fatalf("anchored scalar count=%d handled=%v err=%v want=%d", count, handled, err, len(want))
	}
	if countTrace.Source != "global:scalar-range" || countTrace.ScalarDriver != "interval-cardinality" || countTrace.ScalarRecordsVerified != 0 {
		t.Fatalf("anchored scalar count trace=%+v, want interval cardinality", countTrace)
	}
}

func TestGlobalScalarFiltersIncludeOverlaySnapshot(t *testing.T) {
	volumes := globalScalarTestVolumes()
	volumes[0].applyUSNChanges([]usnChange{{FRN: 3, USN: 10, Reason: usnReasonFileDelete}})
	volumes[1].applyUSNChanges([]usnChange{{FRN: 99, ParentFRN: 1, USN: 11, Reason: usnReasonFileCreate, Name: "overlay.txt"}})

	trace := &searchTrace{}
	opts := queryOptions{Query: "type:file", Limit: 20, Trace: trace}
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`F:\a-mid.txt`, `C:\c-small.txt`, `F:\newest.txt`, `F:\overlay.txt`}
	if paths := pathsOf(got); !slices.Equal(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if trace.PlannerMode != "global-scalar" {
		t.Fatalf("planner mode = %q, want global-scalar", trace.PlannerMode)
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes(volumes, queryOptions{Query: "type:file", Trace: countTrace})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || count != len(want) {
		t.Fatalf("count = %d ok=%v, want %d true", count, ok, len(want))
	}
	if countTrace.PlannerMode != "global-count-scalar" {
		t.Fatalf("count planner mode = %q, want global-count-scalar", countTrace.PlannerMode)
	}
}

func TestGlobalScalarPlannerDeclinesExists(t *testing.T) {
	pq := mustParseQuery(t, queryOptions{Query: "type:file", Exists: true})
	if globalScalarQuerySupported(pq) {
		t.Fatal("exists query must not use the global scalar planner")
	}
}

func TestGlobalScalarSearchKeepsOnlyPerVolumeTopN(t *testing.T) {
	makeVolume := func(volume string, offset int64) *serviceVolumeIndex {
		idx := &Index{
			Source:  "usn",
			Volume:  volume,
			Compact: true,
			Records: []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
		}
		for i := 0; i < 1_000; i++ {
			idx.Records = append(idx.Records, CompactRecord{
				FRN: uint64(i + 2), ParentFRN: 1, Parent: 0,
				Name: fmt.Sprintf("file-%04d.bin", i), Size: offset + int64(i),
			})
		}
		buildOrders(idx)
		return newServiceVolumeIndex(volume+"-scalar-top.gsi", idx)
	}
	volumes := []*serviceVolumeIndex{makeVolume("C:", 1_000), makeVolume("F:", 0)}
	trace := &searchTrace{}
	opts := queryOptions{Query: "size:>=0 type:file sort:size", Limit: 3, Trace: trace}
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || trace.Candidates > 6 {
		t.Fatalf("results/candidates = %d/%d, want 3/<=6", len(got), trace.Candidates)
	}
	for i, entry := range got {
		if entry.Size != int64(i) {
			t.Fatalf("result %d size = %d, want %d", i, entry.Size, i)
		}
	}
	count, ok, err := countServiceVolumes(volumes, queryOptions{Query: "size:>=0 type:file"})
	if err != nil || !ok || count != 2_000 {
		t.Fatalf("count = %d handled=%v err=%v, want 2000 true nil", count, ok, err)
	}
}

func TestGlobalScalarRanklessVolumeDoesNotPruneByRecordID(t *testing.T) {
	makeVolume := func(volume, first, second string) *serviceVolumeIndex {
		idx := &Index{
			Source: "usn", Volume: volume, Compact: true,
			Records: []CompactRecord{
				{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
				{FRN: 2, ParentFRN: 1, Parent: 0, Name: first},
				{FRN: 3, ParentFRN: 1, Parent: 0, Name: second},
			},
		}
		vol := newServiceVolumeIndex(volume+"-rankless-scalar.gsi", idx)
		vol.queryIndex.nameOrder = nil
		vol.queryIndex.nameRank = nil
		return vol
	}
	volumes := []*serviceVolumeIndex{
		makeVolume("C:", "z-file.txt", "a-file.txt"),
		makeVolume("F:", "y-file.txt", "b-file.txt"),
	}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "type:file", Limit: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(got); !slices.Equal(paths, []string{`C:\a-file.txt`}) {
		t.Fatalf("rankless scalar paths = %v, want global name order", paths)
	}
}

func TestGlobalScalarRangeBoundsAndIterator(t *testing.T) {
	vol := scalarRangeFixture()
	sizeCases := []struct {
		query string
		want  []int
	}{
		{query: "size:=10", want: []int{2, 1}},
		{query: "size:>10", want: []int{3}},
		{query: "size:>=10", want: []int{2, 1, 3}},
		{query: "size:<10", want: []int{0}},
		{query: "size:<=10", want: []int{0, 2, 1}},
	}
	for _, tc := range sizeCases {
		pq := mustParseQuery(t, queryOptions{Query: tc.query})
		scalar, ok := scalarRangeForVolume(vol, pq)
		if !ok {
			t.Fatalf("scalarRangeForVolume(%q) declined", tc.query)
		}
		it := newGlobalScalarRangeIterator(0, scalar)
		var got []int
		for {
			id, ok := it.Next()
			if !ok {
				break
			}
			got = append(got, id.local)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s ids = %v, want %v", tc.query, got, tc.want)
		}
	}

	date := func(after, before time.Time) parsedQuery {
		return parsedQuery{DateFilters: []dateFilter{{after: after, before: before}}}
	}
	for _, tc := range []struct {
		name string
		pq   parsedQuery
		want []int
	}{
		{
			name: "date-inclusive-exclusive",
			pq:   date(time.Unix(0, 100), time.Unix(0, 300)),
			want: []int{2, 1},
		},
		{
			name: "modified-after-strict",
			pq:   parsedQuery{HasModAfter: true, ModifiedAfter: time.Unix(0, 200)},
			want: []int{3},
		},
		{
			name: "zero-modification-excluded",
			pq:   parsedQuery{HasModAfter: true, ModifiedAfter: time.Unix(0, -1)},
			want: []int{3, 2, 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scalar, ok := scalarRangeForVolume(vol, tc.pq)
			if !ok {
				t.Fatal("scalarRangeForVolume declined")
			}
			it := newGlobalScalarRangeIterator(0, scalar)
			var got []int
			for {
				id, ok := it.Next()
				if !ok {
					break
				}
				got = append(got, id.local)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGlobalScalarRangeRoutePreservesDefaultGlobalOrder(t *testing.T) {
	idx := &Index{
		Source: "usn", Volume: "C:", Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "z-file.txt", Size: 1},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "a-file.txt", Size: 2},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("scalar-range-order.gsi", idx)
	installScalarOrders(vol)
	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "size:>=1", Limit: 1, Trace: trace,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(got); !slices.Equal(paths, []string{`C:\a-file.txt`}) {
		t.Fatalf("paths = %v, want default name order", paths)
	}
	if trace.Source != "global:scalar-range" {
		t.Fatalf("trace source = %q, want global:scalar-range", trace.Source)
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "size:>=1", Trace: countTrace,
	})
	if err != nil || !ok || count != 2 {
		t.Fatalf("count = %d handled=%v err=%v, want 2 true nil", count, ok, err)
	}
	if countTrace.Source != "global:scalar-range" {
		t.Fatalf("count trace source = %q, want global:scalar-range", countTrace.Source)
	}
}

func TestGlobalScalarRangeOverlayParity(t *testing.T) {
	vol := scalarRangeFixture()
	installScalarOrders(vol)
	vol.applyUSNChanges([]usnChange{
		{FRN: 4, USN: 10, Reason: usnReasonFileDelete},
		{FRN: 99, ParentFRN: 1, USN: 11, Reason: usnReasonFileCreate, Name: "overlay.txt"},
	})
	if hidden := vol.snapshotHiddenBaseIDs(); !hidden.contains(3) {
		t.Fatalf("overlay delete did not hide local id 3: %+v", hidden)
	}
	query := queryOptions{Query: "size:>=10", Limit: 20}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, query, false)
	if err != nil {
		t.Fatal(err)
	}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: query.Query})
	if err != nil || !ok {
		t.Fatalf("count = %d handled=%v err=%v", count, ok, err)
	}
	if len(got) != count {
		t.Fatalf("search/count parity = %d/%d, want equal", len(got), count)
	}
	for _, entry := range got {
		if entry.Path == `C:\c-file.txt` {
			t.Fatalf("deleted base scalar match leaked through range source; paths=%v", pathsOf(got))
		}
	}
}

func TestGlobalScalarRangeMappedResidentParity(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Source:  "usn",
		Volume:  "C:",
		Roots:   []string{`C:\`},
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), Size: 0, ModUnix: 100},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "small.txt", Size: 10, ModUnix: 200},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "large.txt", Size: 500, ModUnix: 300},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "medium.txt", Size: 100, ModUnix: 400},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "scalar-range-v9.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save index: %v", err)
	}

	residentIdx, err := loadIndex(db)
	if err != nil {
		t.Fatalf("load resident index: %v", err)
	}
	mappedIdx, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load mapped index: %v", err)
	}
	if mappedIdx.MMapRecords == nil {
		t.Fatal("mapped load did not use mmap records")
	}
	t.Cleanup(func() { _ = mappedIdx.MMapRecords.file.close() })
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex(db+"-resident", residentIdx),
		newServiceVolumeIndex(db+"-mapped", mappedIdx),
	}
	for _, vol := range volumes {
		for _, tc := range []struct {
			query string
			want  int
		}{
			{query: "size:>=100", want: 3},
			{query: "", want: 2},
		} {
			trace := &searchTrace{}
			opts := queryOptions{Query: tc.query, ModifiedAfter: "1970-01-01T00:00:00.00000025Z", Trace: trace}
			if tc.query != "" {
				opts.ModifiedAfter = ""
			}
			count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
			if err != nil || !ok || count != tc.want {
				t.Fatalf("%s query=%q count = %d handled=%v err=%v, want %d true nil", vol.dbPath, tc.query, count, ok, err, tc.want)
			}
			if trace.Source != "global:scalar-range" {
				t.Fatalf("%s query=%q trace source = %q, want global:scalar-range", vol.dbPath, tc.query, trace.Source)
			}
		}
	}
}

func TestGlobalScalarRangeMixedMetadataUsesSafeScanFallback(t *testing.T) {
	first := scalarRangeFixture()
	second := scalarRangeFixture()
	second.volume = "F:"
	second.index.Volume = "F:"
	second.queryIndex.sizeOrder = nil
	second.queryIndex.sizeRank = nil
	trace := &searchTrace{}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{first, second}, queryOptions{
		Query: "size:>=100", Trace: trace,
	})
	if err != nil || !ok || count != 2 {
		t.Fatalf("count = %d handled=%v err=%v, want 2 true nil", count, ok, err)
	}
	if trace.Source != "global:scalar-range+scan" {
		t.Fatalf("trace source = %q, want mixed range/scan source", trace.Source)
	}
}

func scalarRangeFixture() *serviceVolumeIndex {
	idx := &Index{
		Source: "usn", Volume: "C:", Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "b-file.txt", Size: 10, ModUnix: 100},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "a-file.txt", Size: 10, ModUnix: 200},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "c-file.txt", Size: 100, ModUnix: 300},
			{FRN: 5, ParentFRN: 1, Parent: 0, Name: "deleted.txt", Size: 100, ModUnix: 400, Deleted: true},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("scalar-range-fixture.gsi", idx)
	installScalarOrders(vol)
	return vol
}

// scalarDirAggregateFixture builds a volume where directory raw sizes and
// subtree totals disagree: bigdir reports raw Size 5 but owns a 500MB child.
// The size order (built from SubtreeBytes, like production) sorts it by the
// aggregate, so any raw-size probe or check misplaces it.
func scalarDirAggregateFixture() *serviceVolumeIndex {
	const mb = 1024 * 1024
	idx := &Index{
		Source: "usn", Volume: "F:", Compact: true, Roots: []string{`F:\`},
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: modeFromAttrs(fileAttributeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "bigdir", Mode: modeFromAttrs(fileAttributeDir), Size: 5},
			{FRN: 3, ParentFRN: 2, Parent: 1, Name: "child500.bin", Mode: modeFromAttrs(fileAttributeArchive), Size: 500 * mb},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "big.bin", Mode: modeFromAttrs(fileAttributeArchive), Size: 600 * mb},
			{FRN: 5, ParentFRN: 1, Parent: 0, Name: "tiny.txt", Mode: modeFromAttrs(fileAttributeArchive), Size: 10},
		},
	}
	idx.Derived.SubtreeBytes = []uint64{1100*mb + 10, 500 * mb, 0, 0, 0}
	buildOrders(idx)
	vol := newServiceVolumeIndex("scalar-dir-aggregate.gsi", idx)
	installScalarOrders(vol)
	return vol
}

// TestScalarSizeFilterUsesDirectoryAggregate pins count/search agreement for
// directory sizes: with raw-size probing the range collapses past bigdir
// (raw 5 sorts it nowhere near its 500MB aggregate) and both count and search
// miss it; the effective-size key must be used on both sides.
func TestScalarSizeFilterUsesDirectoryAggregate(t *testing.T) {
	volumes := []*serviceVolumeIndex{scalarDirAggregateFixture()}
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{name: "bare size", query: "size:>100mb", want: []string{`F:`, `F:\big.bin`, `F:\bigdir`, `F:\bigdir\child500.bin`}},
		{name: "type dir", query: "type:dir size:>100mb", want: []string{`F:`, `F:\bigdir`}},
		{name: "type file", query: "type:file size:>100mb", want: []string{`F:\big.bin`, `F:\bigdir\child500.bin`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := &searchTrace{}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: tc.query, Limit: 20, Trace: trace}, false)
			if err != nil {
				t.Fatal(err)
			}
			paths := pathsOf(got)
			sort.Strings(paths)
			if !slices.Equal(paths, tc.want) {
				t.Fatalf("search paths = %v, want %v (trace=%+v)", paths, tc.want, trace)
			}
			if trace.Complete == nil || !*trace.Complete {
				t.Fatalf("trace complete = %v, want true", trace.Complete)
			}
			n, ok, err := countServiceVolumes(volumes, queryOptions{Query: tc.query, Trace: &searchTrace{}})
			if err != nil || !ok || n != len(tc.want) {
				t.Fatalf("count = %d ok=%v err=%v, want %d true nil", n, ok, err, len(tc.want))
			}
		})
	}
}

// TestScalarCountExcludesTombstonedRangeMember pins the overlay correction:
// deleting big.bin must decrement the count by exactly one (a positional
// search over the size-ordered array cannot find the hidden id).
func TestScalarCountExcludesTombstonedRangeMember(t *testing.T) {
	vol := scalarDirAggregateFixture()
	vol.applyUSNChanges([]usnChange{{FRN: 4, USN: 10, Reason: usnReasonFileDelete}})
	volumes := []*serviceVolumeIndex{vol}
	n, ok, err := countServiceVolumes(volumes, queryOptions{Query: "size:>100mb", Trace: &searchTrace{}})
	if err != nil || !ok || n != 3 {
		t.Fatalf("count after delete = %d ok=%v err=%v, want 3", n, ok, err)
	}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "size:>100mb", Limit: 20, Trace: &searchTrace{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("search len = %d, count = %d; want agreement", len(got), n)
	}
	for _, e := range got {
		if e.Name == "big.bin" {
			t.Fatalf("deleted big.bin still returned: %v", pathsOf(got))
		}
	}
}

func installScalarOrders(vol *serviceVolumeIndex) {
	if vol == nil || vol.index == nil {
		return
	}
	sizeOrder, sizeRank := buildCompactSizeOrderRank(vol.index)
	modOrder, modRank := buildCompactModifiedOrderRank(vol.index)
	if vol.queryIndex == nil {
		vol.queryIndex = &residentQueryIndex{}
	}
	vol.queryIndex.sizeOrder, vol.queryIndex.sizeRank = sizeOrder, sizeRank
	vol.queryIndex.modOrder, vol.queryIndex.modRank = modOrder, modRank
}

func globalScalarTestVolumes() []*serviceVolumeIndex {
	makeVolume := func(volume string, records []CompactRecord) *serviceVolumeIndex {
		idx := &Index{Source: "usn", Volume: volume, Compact: true, Records: records}
		buildOrders(idx)
		return newServiceVolumeIndex(volume+"-scalar.gsi", idx)
	}
	date := func(day int) int64 {
		return time.Date(2026, 7, day, 12, 0, 0, 0, time.UTC).UnixNano()
	}
	return []*serviceVolumeIndex{
		makeVolume("C:", []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), ModUnix: date(1)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "c-small.txt", Size: 20, ModUnix: date(1)},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "z-big.txt", Size: 900, ModUnix: date(2)},
		}),
		makeVolume("F:", []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), ModUnix: date(1)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "a-mid.txt", Size: 100, ModUnix: date(3)},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "newest.txt", Size: 200, ModUnix: date(4)},
		}),
	}
}

func TestGlobalScalarRangeOperatorsMatchFullOracle(t *testing.T) {
	volumes := globalScalarTestVolumes()
	indexes := make([]*Index, len(volumes))
	for i, vol := range volumes {
		indexes[i] = vol.index
	}
	queries := []string{
		"size:>20",
		"size:>=100",
		"size:<100",
		"size:<=100",
		"size:100",
		"dm:2026-07-02",
		"dm:2026-07-03",
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			opts := queryOptions{Query: query, Limit: 20}
			want, err := searchAll(indexes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			trace := &searchTrace{}
			opts.Trace = trace
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(pathsOf(got), pathsOf(want)) {
				t.Fatalf("paths = %v, want %v", pathsOf(got), pathsOf(want))
			}
			if trace.Source != "global:scalar-range" {
				t.Fatalf("trace source = %q, want global:scalar-range", trace.Source)
			}
			countTrace := &searchTrace{}
			count, ok, err := countServiceVolumes(volumes, queryOptions{Query: query, Trace: countTrace})
			if err != nil || !ok || count != len(want) {
				t.Fatalf("count = %d handled=%v err=%v, want %d", count, ok, err, len(want))
			}
			if countTrace.Source != "global:scalar-range" {
				t.Fatalf("count trace source = %q, want global:scalar-range", countTrace.Source)
			}
		})
	}
}
