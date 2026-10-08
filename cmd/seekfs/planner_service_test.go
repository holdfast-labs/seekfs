package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestServiceVolumesHighFanoutMultiPartPathQueries(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	cIdx := highExtensionFanoutPathIndex(2500)
	fIdx := highExtensionFanoutPathIndex(2500)
	fIdx.Volume = "F:"
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("c-high-fanout.gsi", cIdx),
		newServiceVolumeIndex("f-high-fanout.gsi", fIdx),
	}
	for _, vol := range volumes {
		vol.rebuildNameTrigramsLocked()
	}
	queries := []queryOptions{
		{Query: "path:C: trainingdata Dataset .nrrd", Limit: 20},
		{Query: "path:F: trainingdata Dataset .nrrd", Limit: 20},
		{Query: "path:trainingdata Dataset .nrrd", Limit: 20},
		{Query: "path:C: absent Dataset .nrrd", Limit: 20},
		{Query: "path:F: absent Dataset .nrrd", Limit: 20},
	}
	for _, opts := range queries {
		t.Run(opts.Query, func(t *testing.T) {
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatalf("service search: %v", err)
			}
			var full []Entry
			for _, vol := range volumes {
				if selected, err := serviceVolumesForQuery([]*serviceVolumeIndex{vol}, opts); err != nil || len(selected) == 0 {
					continue
				}
				matches, err := searchCompactWithCache(vol.index, opts, false, make(map[int]string), nil)
				if err != nil {
					t.Fatalf("full search %s: %v", vol.volume, err)
				}
				full = append(full, matches...)
			}
			if len(full) > opts.Limit {
				full = full[:opts.Limit]
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(full); !sameOrderedStrings(gotPaths, wantPaths) {
				t.Fatalf("service paths = %v, full paths = %v", gotPaths, wantPaths)
			}
			for _, entry := range got {
				if strings.Contains(opts.Query, "path:C:") && !strings.HasPrefix(strings.ToUpper(entry.Path), `C:\`) {
					t.Fatalf("C:-scoped query returned %q", entry.Path)
				}
				if strings.Contains(opts.Query, "path:F:") && !strings.HasPrefix(strings.ToUpper(entry.Path), `F:\`) {
					t.Fatalf("F:-scoped query returned %q", entry.Path)
				}
			}
		})
	}
}

func TestServiceVolumesPathModePrioritizesFullComponentVolume(t *testing.T) {
	cVol := workspaceAlphaModelVolume("C:", false)
	fVol := workspaceAlphaModelVolume("F:", true)
	opts := queryOptions{Query: "workspace-alpha model_v2 type:file", MatchPath: true, Limit: 10}

	volumes := prioritizeServiceVolumesForPathTerms([]*serviceVolumeIndex{cVol, fVol}, opts)
	if len(volumes) != 2 || volumes[0] != fVol {
		t.Fatalf("prioritized volumes = [%s %s], want F: before partial C:", volumes[0].volume, volumes[1].volume)
	}
}

func TestServiceVolumesPathModeLaterFullVolumeSearchCountParity(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	trace := &searchTrace{}
	opts := queryOptions{Query: "path:workspace-alpha model_v2 type:file", Limit: 10, Trace: trace}

	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
	}
	if want := []string{"F:", "C:"}; !slices.Equal(trace.EligibleVolumes, want) {
		t.Fatalf("eligible volumes = %v, want %v", trace.EligibleVolumes, want)
	}
	want := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		want = append(want, fmt.Sprintf(`F:\project-%02d\workspace-alpha\model_v2\target-model-%02d.bin`, i, i))
	}
	if paths := pathsOf(got); !sameOrderedStrings(paths, want) {
		t.Fatalf("search paths = %v, want %v", paths, want)
	}

	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	countMatches, err := searchServiceVolumes(volumes, countOpts, true)
	if err != nil {
		t.Fatal(err)
	}
	if countTrace.PlannerMode != "global-components" {
		t.Fatalf("count planner mode = %q, want global-components", countTrace.PlannerMode)
	}
	if want := []string{"F:", "C:"}; !slices.Equal(countTrace.EligibleVolumes, want) {
		t.Fatalf("count eligible volumes = %v, want %v", countTrace.EligibleVolumes, want)
	}
	if gotCount := len(countMatches); gotCount != len(got) {
		t.Fatalf("count/search parity = %d/%d, want equal", gotCount, len(got))
	}

	fastCountTrace := &searchTrace{}
	fastCountOpts := opts
	fastCountOpts.Trace = fastCountTrace
	fastCount, ok, err := countServiceVolumes(volumes, fastCountOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("countServiceVolumes declined exact fast count")
	}
	if fastCount != len(got) {
		t.Fatalf("fast count/search parity = %d/%d, want equal", fastCount, len(got))
	}
	if fastCountTrace.PlannerMode != "global-count-components" {
		t.Fatalf("fast count planner mode = %q, want global-count-components", fastCountTrace.PlannerMode)
	}
	if want := []string{"F:", "C:"}; !slices.Equal(fastCountTrace.EligibleVolumes, want) {
		t.Fatalf("fast count eligible volumes = %v, want %v", fastCountTrace.EligibleVolumes, want)
	}
}

func TestServiceVolumesPathModeExpiredDeadlineDoesNotReturnCompleteZero(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{
		Query:        "workspace-alpha model_v2 type:file",
		MatchPath:    true,
		Limit:        10,
		DeadlineUnix: time.Now().Add(-time.Second).UnixNano(),
	}

	got, err := searchServiceVolumes(volumes, opts, false)
	if !errors.Is(err, errQueryCanceled) {
		t.Fatalf("searchServiceVolumes err = %v, want %v; results=%v", err, errQueryCanceled, pathsOf(got))
	}
}

func TestGlobalPlannerExtOnlyMatchesServiceVolumes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		query  string
		source string
	}{
		{name: "ext", query: "ext:bin", source: "global:ext:bin"},
		{name: "simple-glob", query: "glob:*.bin", source: "global:glob-ext:bin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			opts := queryOptions{Query: tc.query, Limit: 10}

			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("legacy ext count declined")
			}
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-ext" {
				t.Fatalf("planner mode = %q, want global-ext", trace.PlannerMode)
			}
			if trace.Source != tc.source {
				t.Fatalf("trace source = %q, want %s", trace.Source, tc.source)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global ext paths = %v, want %v", gotPaths, wantPaths)
			}
			countSearchTrace := &searchTrace{}
			countSearchOpts := opts
			countSearchOpts.Trace = countSearchTrace
			countMatches, err := searchServiceVolumes(volumes, countSearchOpts, true)
			if err != nil {
				t.Fatal(err)
			}
			if countSearchTrace.PlannerMode != "global-ext" {
				t.Fatalf("count-search planner mode = %q, want global-ext", countSearchTrace.PlannerMode)
			}
			if len(countMatches) != wantCount {
				t.Fatalf("global ext count-search len = %d, want %d", len(countMatches), wantCount)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global ext count declined")
			}
			if countTrace.PlannerMode != "global-count-ext" {
				t.Fatalf("count planner mode = %q, want global-count-ext", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global ext count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerExtTypeFileDeclinesUnsafeTopN(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("c-ext-type.gsi", singleEntryKindCompactIndex("C:", "foo.bin", uint32(os.ModeDir))),
		newServiceVolumeIndex("f-ext-type.gsi", singleEntryKindCompactIndex("F:", "bar.bin", 0)),
	}
	trace := &searchTrace{}
	opts := queryOptions{Query: "type:file ext:bin", Limit: 1, Trace: trace}
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	// The ext lane serves type:file/dir exactly: the top-N posting paths
	// filter by record mode before truncating (and the rank-truncated
	// iterator draws from type-matching records only), so the directory
	// foo.bin cannot displace the file bar.bin even at limit 1.
	if trace.PlannerMode != "global-ext" {
		t.Fatalf("planner mode = %q, want global-ext; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\workspace\bar.bin`}) {
		t.Fatalf("paths = %v, want only file result", gotPaths)
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes(volumes, queryOptions{Query: "type:file ext:bin", Trace: countTrace})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("countServiceVolumes declined type:file ext:bin")
	}
	if countTrace.PlannerMode != "global-count-ext" {
		t.Fatalf("count planner mode = %q, want global-count-ext; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	// The mirror query must find the directory and exclude the file.
	dirTrace := &searchTrace{}
	dirGot, err := searchServiceVolumes(volumes, queryOptions{Query: "type:dir ext:bin", Limit: 1, Trace: dirTrace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if dirTrace.PlannerMode != "global-ext" {
		t.Fatalf("dir planner mode = %q, want global-ext; decline=%s fallback=%s", dirTrace.PlannerMode, dirTrace.Decline, dirTrace.Fallback)
	}
	if gotPaths := pathsOf(dirGot); !sameOrderedStrings(gotPaths, []string{`C:\workspace\foo.bin`}) {
		t.Fatalf("dir paths = %v, want only directory result", gotPaths)
	}
}

func TestGlobalPlannerExtDeclinesCaseSensitiveShortcut(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("c-case-ext.gsi", singleFileCompactIndex("C:", "case.txt")),
		newServiceVolumeIndex("f-case-ext.gsi", singleFileCompactIndex("F:", "case.TXT")),
	}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "case: ext:TXT", Limit: 10, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode == "global-ext" || trace.PlannerMode == "global-count-ext" {
		t.Fatalf("planner mode = %q, want verified fallback for case-sensitive ext", trace.PlannerMode)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\workspace\case.TXT`}) {
		t.Fatalf("paths = %v, want only exact-case TXT extension", gotPaths)
	}
}

func TestGlobalPlannerExtSortPathUsesGlobalPathOrder(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("F:", true),
		workspaceAlphaModelVolume("C:", true),
	}
	trace := &searchTrace{}
	opts := queryOptions{Query: "ext:bin sort:path", Limit: 3, Trace: trace}
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-ext" {
		t.Fatalf("planner mode = %q, want global-ext; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	paths := pathsOf(got)
	if len(paths) != 3 {
		t.Fatalf("paths = %v, want 3 results", paths)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, `C:\`) {
			t.Fatalf("global sort:path paths = %v, want C: paths before F: despite reversed volume order", paths)
		}
	}
}

func TestGlobalPlannerExtDefaultTopNUsesGlobalNameOrder(t *testing.T) {
	makeVolume := func(volume, match string, earlier int) *serviceVolumeIndex {
		idx := &Index{
			Source:  "usn",
			Volume:  volume,
			Compact: true,
			Records: []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
		}
		for i := 0; i < earlier; i++ {
			idx.Records = append(idx.Records, CompactRecord{FRN: uint64(2 + i), ParentFRN: 1, Parent: 0, Name: fmt.Sprintf("a-%03d.go", i)})
		}
		idx.Records = append(idx.Records, CompactRecord{FRN: uint64(2 + earlier), ParentFRN: 1, Parent: 0, Name: match})
		buildOrders(idx)
		return newServiceVolumeIndex(volume+"-ext-global-rank.gsi", idx)
	}
	volumes := []*serviceVolumeIndex{
		makeVolume("C:", "b-target.txt", 100),
		makeVolume("F:", "z-target.txt", 0),
	}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "ext:txt", Limit: 1, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-ext" {
		t.Fatalf("planner mode = %q, want global-ext; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`C:\b-target.txt`}) {
		t.Fatalf("paths = %v, want globally first name despite worse local rank", gotPaths)
	}
}

func TestGlobalPlannerExtNonPathSortsUseGlobalOrder(t *testing.T) {
	makeVolume := func(volume, firstName, secondName string, firstMode, secondMode uint32, firstSize, secondSize int64, firstMod, secondMod int64) *serviceVolumeIndex {
		idx := &Index{
			Source:  "usn",
			Volume:  volume,
			Compact: true,
		}
		add := func(frn, parentFRN uint64, parent int32, name string, mode uint32, size int64, mod int64) int32 {
			idx.Records = append(idx.Records, CompactRecord{
				FRN:       frn,
				ParentFRN: parentFRN,
				Parent:    parent,
				Name:      name,
				Mode:      mode,
				Size:      size,
				ModUnix:   mod,
			})
			return int32(len(idx.Records) - 1)
		}
		root := add(1, 1, -1, ".", uint32(os.ModeDir), 0, 1)
		add(2, 1, root, firstName, firstMode, firstSize, firstMod)
		add(3, 1, root, secondName, secondMode, secondSize, secondMod)
		buildOrders(idx)
		return newServiceVolumeIndex(strings.ToLower(strings.TrimSuffix(volume, ":"))+"-sort.gsi", idx)
	}
	volumes := []*serviceVolumeIndex{
		makeVolume("C:", "z-large.txt", "folder.txt", 0, uint32(os.ModeDir), 900, 200, 20, 10),
		makeVolume("F:", "a-small.txt", "newest.txt", 0, 0, 5, 100, 30, 100),
	}
	cases := []struct {
		query string
		want  string
	}{
		{query: "ext:txt sort:size", want: `F:\a-small.txt`},
		{query: "ext:txt sort:modified", want: `F:\newest.txt`},
		{query: "ext:txt sort:extension", want: `F:\a-small.txt`},
		{query: "ext:txt sort:type", want: `C:\folder.txt`},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			trace := &searchTrace{}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: tc.query, Limit: 1, Trace: trace}, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-ext" {
				t.Fatalf("planner mode = %q, want global-ext; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
			}
			if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{tc.want}) {
				t.Fatalf("paths = %v, want %s", gotPaths, tc.want)
			}
		})
	}
}

func TestGlobalPlannerComponentNonPathSortsUseGlobalOrder(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		componentSortVolume("C:", []componentSortFixtureEntry{
			{name: "target-large.zzz", size: 900, modUnix: 100},
			{name: "target-dir", mode: uint32(os.ModeDir), size: 0, modUnix: 50},
			{name: "target-new.bin", size: 100, modUnix: 300},
		}),
		componentSortVolume("F:", []componentSortFixtureEntry{
			{name: "target-small.bin", size: 5, modUnix: 200},
			{name: "target-alpha.aaa", size: 20, modUnix: 150},
		}),
	}
	cases := []struct {
		query string
		want  string
		count int
	}{
		{query: "path:workspace-alpha target type:file sort:size", want: `F:\project\workspace-alpha\model_v2\target-small.bin`, count: 4},
		{query: "path:workspace-alpha target type:file sort:modified", want: `C:\project\workspace-alpha\model_v2\target-new.bin`, count: 4},
		{query: "path:workspace-alpha target type:file sort:extension", want: `F:\project\workspace-alpha\model_v2\target-alpha.aaa`, count: 4},
		{query: "path:workspace-alpha target sort:type", want: `C:\project\workspace-alpha\model_v2\target-dir`, count: 5},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			trace := &searchTrace{}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: tc.query, Limit: 1, Trace: trace}, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
			}
			if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{tc.want}) {
				t.Fatalf("paths = %v, want %s", gotPaths, tc.want)
			}
			countTrace := &searchTrace{}
			count, ok, err := countServiceVolumes(volumes, queryOptions{Query: tc.query, Trace: countTrace})
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("countServiceVolumes declined component sort query")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
			}
			if count != tc.count {
				t.Fatalf("count = %d, want %d", count, tc.count)
			}
		})
	}
}

func singleEntryKindCompactIndex(volume, name string, mode uint32) *Index {
	idx := singleFileCompactIndex(volume, name)
	idx.Records[len(idx.Records)-1].Mode = mode
	buildOrders(idx)
	return idx
}

func TestGlobalPlannerOverlayExtMatchesServiceVolumes(t *testing.T) {
	vol := engineOverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{
		{FRN: 101, USN: 10, Reason: usnReasonFileDelete},
		{FRN: 301, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "aaa-overlay.txt"},
	})
	opts := queryOptions{Query: "ext:txt", Limit: 10}
	want, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	wantCount, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("legacy overlay ext count declined")
	}

	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	trace := &searchTrace{}
	globalOpts := opts
	globalOpts.Trace = trace
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, globalOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-ext" {
		t.Fatalf("planner mode = %q, want global-ext; decline=%s", trace.PlannerMode, trace.Decline)
	}
	if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("global overlay ext paths = %v, want %v", gotPaths, wantPaths)
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("global overlay ext count declined")
	}
	if countTrace.PlannerMode != "global-count-ext" {
		t.Fatalf("count planner mode = %q, want global-count-ext; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
	if count != wantCount {
		t.Fatalf("global overlay ext count = %d, want %d", count, wantCount)
	}
}

func TestGlobalPlannerComplexGlobFallsBack(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{Query: "glob:target-*.bin", Limit: 10}
	wantPaths := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		wantPaths = append(wantPaths, fmt.Sprintf(`F:\project-%02d\workspace-alpha\model_v2\target-model-%02d.bin`, i, i))
	}
	trace := &searchTrace{}
	globalOpts := opts
	globalOpts.Trace = trace
	got, err := searchServiceVolumes(volumes, globalOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-bounded-scan" {
		t.Fatalf("planner mode = %q, want global-bounded-scan; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if trace.Fallback != "global-bounded-scan" {
		t.Fatalf("fallback = %q, want global-bounded-scan; decline=%s declines=%+v", trace.Fallback, trace.Decline, trace.Declines)
	}
	if gotPaths := pathsOf(got); !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("fallback glob paths = %v, want %v", gotPaths, wantPaths)
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes(volumes, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("global bounded fallback count declined")
	}
	if countTrace.PlannerMode != "global-bounded-scan" {
		t.Fatalf("count planner mode = %q, want global-bounded-scan", countTrace.PlannerMode)
	}
	if countTrace.Source != "global:bounded-scan" {
		t.Fatalf("count source = %q, want global:bounded-scan", countTrace.Source)
	}
	if countTrace.Fallback != "global-bounded-scan" {
		t.Fatalf("count fallback = %q, want global-bounded-scan", countTrace.Fallback)
	}
	if countTrace.Complete == nil || !*countTrace.Complete {
		t.Fatalf("count complete = %v, want true", countTrace.Complete)
	}
	if countTrace.Candidates == 0 {
		t.Fatalf("count candidates = %d, want populated", countTrace.Candidates)
	}
	if count != len(wantPaths) {
		t.Fatalf("global bounded fallback count = %d, want %d", count, len(wantPaths))
	}
}

func TestGlobalPlannerCompleteFallbackDefaultsForRemainingFamilies(t *testing.T) {
	makeVolume := func(volume, match string, earlier int) *serviceVolumeIndex {
		idx := &Index{
			Source:  "usn",
			Volume:  volume,
			Compact: true,
			Records: []CompactRecord{{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
		}
		for i := 0; i < earlier; i++ {
			idx.Records = append(idx.Records, CompactRecord{FRN: uint64(2 + i), ParentFRN: 1, Parent: 0, Name: fmt.Sprintf("a-%03d.go", i)})
		}
		idx.Records = append(idx.Records, CompactRecord{FRN: uint64(2 + earlier), ParentFRN: 1, Parent: 0, Name: match})
		buildOrders(idx)
		return newServiceVolumeIndex(volume+"-complete-fallback.gsi", idx)
	}
	volumes := []*serviceVolumeIndex{
		makeVolume("C:", "b-needle.txt", 100),
		makeVolume("F:", "z-needle.txt", 0),
	}

	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "needle", Limit: 1, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-bounded-scan" || trace.Complete == nil || !*trace.Complete {
		t.Fatalf("trace = %+v, want complete global fallback", trace)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`C:\b-needle.txt`}) {
		t.Fatalf("paths = %v, want globally first remaining-family match", gotPaths)
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes(volumes, queryOptions{Query: "needle", Trace: countTrace})
	if err != nil || !ok || count != 2 {
		t.Fatalf("count = %d handled=%v err=%v, want 2 true nil", count, ok, err)
	}
	if countTrace.PlannerMode != "global-bounded-scan" {
		t.Fatalf("count planner mode = %q, want global-bounded-scan", countTrace.PlannerMode)
	}

	for _, opts := range []queryOptions{
		{Query: "x|needle", Limit: 1},
		{Query: "case: regex:.*needle.*", Limit: 1},
		{Query: "type:file", Exists: true, Limit: 1},
	} {
		routeTrace := &searchTrace{}
		opts.Trace = routeTrace
		if _, err := searchServiceVolumes(volumes, opts, false); err != nil {
			t.Fatalf("query %q: %v", opts.Query, err)
		}
		if routeTrace.PlannerMode != "global-bounded-scan" {
			t.Fatalf("query %q planner mode = %q, want global-bounded-scan; decline=%s", opts.Query, routeTrace.PlannerMode, routeTrace.Decline)
		}
	}
}

func TestGlobalPlannerComponentPathMatchesServiceVolumes(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{Query: "path:workspace-alpha model_v2", Limit: 10}

	wantPaths := make([]string, 0, 16)
	for i := 0; i < 8; i++ {
		wantPaths = append(wantPaths, fmt.Sprintf(`F:\project-%02d\workspace-alpha\model_v2`, i))
	}
	for i := 0; i < 8; i++ {
		wantPaths = append(wantPaths, fmt.Sprintf(`F:\project-%02d\workspace-alpha\model_v2\target-model-%02d.bin`, i, i))
	}
	trace := &searchTrace{}
	globalOpts := opts
	globalOpts.Trace = trace
	got, err := searchServiceVolumes(volumes, globalOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
	}
	if trace.Source != "global:components" {
		t.Fatalf("trace source = %q, want global:components", trace.Source)
	}
	for _, want := range []traceTerm{
		{Term: "workspace-alpha", Kind: "path-substring", Source: "global:component-subtree", Exact: false},
		{Term: "model_v2", Kind: "path-substring", Source: "global:component-subtree", Exact: false},
	} {
		if !traceHasTerm(trace.Terms, want) {
			t.Fatalf("trace terms = %+v, missing %+v", trace.Terms, want)
		}
	}
	if gotPaths := pathsOf(got); !slices.Equal(gotPaths, wantPaths[:len(gotPaths)]) {
		t.Fatalf("global component paths = %v, want prefix %v", gotPaths, wantPaths[:len(gotPaths)])
	}
	countSearchTrace := &searchTrace{}
	countSearchOpts := opts
	countSearchOpts.Trace = countSearchTrace
	countMatches, err := searchServiceVolumes(volumes, countSearchOpts, true)
	if err != nil {
		t.Fatal(err)
	}
	if countSearchTrace.PlannerMode != "global-components" {
		t.Fatalf("count-search planner mode = %q, want global-components", countSearchTrace.PlannerMode)
	}
	if len(countMatches) != len(wantPaths) {
		t.Fatalf("global component count-search len = %d, want %d", len(countMatches), len(wantPaths))
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes(volumes, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("global component count declined")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
	}
	if count != len(wantPaths) {
		t.Fatalf("global component count = %d, want %d", count, len(wantPaths))
	}
}

func TestGlobalPlannerComponentSortPathUsesGlobalPathOrder(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("F:", true),
		workspaceAlphaModelVolume("C:", true),
	}
	trace := &searchTrace{}
	opts := queryOptions{Query: "path:workspace-alpha model_v2 sort:path", Limit: 3, Trace: trace}
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	paths := pathsOf(got)
	if len(paths) != 3 {
		t.Fatalf("paths = %v, want 3 results", paths)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, `C:\`) {
			t.Fatalf("global component sort:path paths = %v, want C: paths before F: despite reversed volume order", paths)
		}
	}
}

func TestGlobalPlannerOverlayComponentMatchesServiceVolumes(t *testing.T) {
	vol := engineOverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{{
		FRN:       301,
		ParentFRN: 200,
		USN:       11,
		Reason:    usnReasonFileCreate,
		Name:      "needle-overlay.bin",
	}})
	opts := queryOptions{Query: "path:base-parent needle-overlay", MatchPath: true, Limit: 10}
	want, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	wantCount, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		countMatches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, true)
		if err != nil {
			t.Fatal(err)
		}
		wantCount = len(countMatches)
	}

	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	trace := &searchTrace{}
	globalOpts := opts
	globalOpts.Trace = trace
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, globalOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s", trace.PlannerMode, trace.Decline)
	}
	if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("global overlay component paths = %v, want %v", gotPaths, wantPaths)
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("global overlay component count declined")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
	if count != wantCount {
		t.Fatalf("global overlay component count = %d, want %d", count, wantCount)
	}
}

func TestGlobalPlannerOverlayDirectoryRenameUpdatesComponentDescendants(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	vol := engineOverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{
		{FRN: 301, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "staging", Attr: fileAttributeDir},
		{FRN: 302, ParentFRN: 301, USN: 12, Reason: usnReasonFileCreate, Name: "model_v2", Attr: fileAttributeDir},
		{FRN: 303, ParentFRN: 302, USN: 13, Reason: usnReasonFileCreate, Name: "result.bin"},
		{FRN: 301, ParentFRN: 100, USN: 14, Reason: usnReasonRenameOld, Name: "staging", Attr: fileAttributeDir},
		{FRN: 301, ParentFRN: 100, USN: 15, Reason: usnReasonRenameNew, Name: "workspace-alpha", Attr: fileAttributeDir},
	})

	assertMatches := func(want []string) {
		t.Helper()
		opts := queryOptions{Query: "path:workspace-alpha model_v2 type:file", Limit: 10}
		trace := &searchTrace{}
		opts.Trace = trace
		got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
		if err != nil {
			t.Fatal(err)
		}
		if trace.PlannerMode != "global-components" {
			t.Fatalf("planner mode = %q, want global-components; decline=%s", trace.PlannerMode, trace.Decline)
		}
		if gotPaths := pathsOf(got); !slices.Equal(gotPaths, want) {
			t.Fatalf("renamed overlay component paths = %v, want %v", gotPaths, want)
		}
		countTrace := &searchTrace{}
		opts.Trace = countTrace
		count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
		if err != nil || !ok {
			t.Fatalf("renamed overlay component count handled=%v err=%v", ok, err)
		}
		if countTrace.PlannerMode != "global-count-components" {
			t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
		}
		if count != len(want) {
			t.Fatalf("renamed overlay component count = %d, want %d", count, len(want))
		}
	}

	assertMatches([]string{`F:\workspace-alpha\model_v2\result.bin`})
	vol.applyUSNChanges([]usnChange{
		{FRN: 301, ParentFRN: 100, USN: 16, Reason: usnReasonRenameOld, Name: "workspace-alpha", Attr: fileAttributeDir},
		{FRN: 301, ParentFRN: 100, USN: 17, Reason: usnReasonRenameNew, Name: "archive", Attr: fileAttributeDir},
	})
	assertMatches(nil)
}

func TestGlobalPlannerOverlayParentMatchesServiceVolumes(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		changes []usnChange
	}{
		{
			name:  "overlay-child-under-base-parent",
			query: "parent:base-parent",
			changes: []usnChange{{
				FRN:       301,
				ParentFRN: 200,
				USN:       11,
				Reason:    usnReasonFileCreate,
				Name:      "overlay-child.txt",
			}},
		},
		{
			name:  "overlay-child-under-overlay-parent",
			query: "parent:overlay-parent",
			changes: []usnChange{
				{FRN: 301, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "overlay-parent", Attr: fileAttributeDir},
				{FRN: 302, ParentFRN: 301, USN: 12, Reason: usnReasonFileCreate, Name: "overlay-child.txt"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := engineOverlaySearchTestVolume(t)
			vol.applyUSNChanges(tc.changes)
			opts := queryOptions{Query: tc.query, Limit: 10}
			want, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				countMatches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				wantCount = len(countMatches)
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components; decline=%s", trace.PlannerMode, trace.Decline)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global overlay parent paths = %v, want %v", gotPaths, wantPaths)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global overlay parent count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
			}
			if count != wantCount {
				t.Fatalf("global overlay parent count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerComponentOrNotMatchesServiceVolumes(t *testing.T) {
	cases := []string{
		"path:workspace-alpha model_v2|alpha-notes",
		"path:workspace-alpha !model_v2",
		"path:workspace-alpha parent:model_v2|parent:workspace-alpha sort:path",
		"path:workspace-alpha !parent:model_v2 sort:path",
	}
	for _, query := range cases {
		t.Run(query, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			opts := queryOptions{Query: query, Limit: 20}
			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("legacy component count declined")
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global component paths = %v, want %v", gotPaths, wantPaths)
			}

			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global component count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global component count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerComponentTypeFilterMatchesServiceVolumes(t *testing.T) {
	cases := []string{
		"path:workspace-alpha model_v2 type:file",
		"path:workspace-alpha model_v2 type:dir",
	}
	for _, query := range cases {
		t.Run(query, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			opts := queryOptions{Query: query, Limit: 5}
			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("legacy component count declined")
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global component paths = %v, want %v", gotPaths, wantPaths)
			}

			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global component count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global component count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerComponentExtMatchesServiceVolumes(t *testing.T) {
	for _, query := range []string{"path:workspace-alpha model_v2 ext:bin", "path:workspace-alpha model_v2 glob:*.bin"} {
		t.Run(query, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			opts := queryOptions{Query: query, Limit: 5}
			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				countMatches, err := searchServiceVolumes(volumes, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				wantCount = len(countMatches)
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
			}
			extSource := "global:ext:bin"
			if strings.Contains(query, "glob:") {
				extSource = "global:glob-ext:bin"
			}
			for _, want := range []traceTerm{
				{Term: "workspace-alpha", Kind: "path-substring", Source: "global:component-subtree", Exact: false},
				{Term: "model_v2", Kind: "path-substring", Source: "global:component-subtree", Exact: false},
				{Term: "bin", Kind: "extension", Source: extSource, Exact: true},
			} {
				if !traceHasTerm(trace.Terms, want) {
					t.Fatalf("trace terms = %+v, missing %+v for query %q", trace.Terms, want, query)
				}
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global component+ext paths = %v, want %v", gotPaths, wantPaths)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global component+ext count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global component+ext count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerComponentExtTraceMissingPostingVolume(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	if volumes[0].queryIndex == nil {
		volumes[0].queryIndex = &residentQueryIndex{}
	}
	volumes[0].queryIndex.ext = nil
	trace := &searchTrace{}
	opts := queryOptions{Query: "path:workspace-alpha model_v2 ext:bin", Limit: 5, Trace: trace}
	if got, handled, err := searchServiceVolumesGlobalComponentsOnly(volumes, opts, false); err != nil {
		t.Fatal(err)
	} else if handled {
		t.Fatalf("global components unexpectedly handled missing ext posting source with results=%v", got)
	}
	if trace.Decline != "global-ext:missing-posting" {
		t.Fatalf("decline = %q, want global-ext:missing-posting", trace.Decline)
	}
	if len(trace.Declines) == 0 {
		t.Fatalf("declines = %+v, want missing posting decline", trace.Declines)
	}
	last := trace.Declines[len(trace.Declines)-1]
	if last.Volume != "C:" || last.Source != "global-ext" || last.Reason != "missing-posting" {
		t.Fatalf("last decline = %+v, want C: global-ext missing-posting", last)
	}
}

func TestGlobalPlannerSupportedComponentsDefaultWithoutEnv(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	cases := []struct {
		opts queryOptions
		mode string
	}{
		{opts: queryOptions{Query: "path:workspace-alpha model_v2 ext:bin", Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "workspace-alpha model_v2 ext:bin", MatchPath: true, Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "workspace-alpha model_v2", MatchPath: true, Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "path:workspace-alpha model_v2|alpha-notes", Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "path:workspace-alpha !model_v2", Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "parent:model_v2 type:file", Limit: 20}, mode: "global-components"},
		{opts: queryOptions{Query: "dir:workspace-alpha ext:bin type:file", Limit: 20}, mode: "global-components"},
	}
	for _, tc := range cases {
		t.Run(tc.opts.Query, func(t *testing.T) {
			trace := &searchTrace{}
			opts := tc.opts
			opts.Trace = trace
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != tc.mode {
				t.Fatalf("planner mode = %q, want %q; decline=%s fallback=%s results=%v", trace.PlannerMode, tc.mode, trace.Decline, trace.Fallback, pathsOf(got))
			}
			if strings.Contains(opts.Query, "dir:workspace-alpha") && !traceHasTerm(trace.Terms, traceTerm{Term: "workspace-alpha", Kind: "directory-component", Source: "global:dir"}) {
				t.Fatalf("trace terms = %+v, missing dir source", trace.Terms)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("default global component count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
			}
			if count != len(got) {
				t.Fatalf("count = %d, search matches = %d (%v)", count, len(got), pathsOf(got))
			}
		})
	}
}

func TestGlobalPlannerVolumeAnchoredAndExplicitSinglePathTermsDefault(t *testing.T) {
	cIndex := dottedPathBenchmarkIndex(80)
	fIndex := dottedPathBenchmarkIndex(80)
	fIndex.Volume = "F:"
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("single-path-c.gsi", cIndex),
		newServiceVolumeIndex("single-path-f.gsi", fIndex),
	}
	cases := []struct {
		query      string
		searchMode string
		countMode  string
	}{
		{query: "F: fixtureproj", searchMode: "service-single-volume", countMode: "service-count-single-volume"},
		{query: "path:F: raw", searchMode: "global-components", countMode: "global-count-components"},
		{query: "path:trainingdata", searchMode: "global-components", countMode: "global-count-components"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			query := tc.query
			opts := queryOptions{Query: query, Limit: 20}
			want, err := searchAll([]*Index{cIndex, fIndex}, opts, false)
			if err != nil {
				t.Fatalf("oracle search: %v", err)
			}
			trace := &searchTrace{}
			opts.Trace = trace
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != tc.searchMode {
				t.Fatalf("planner mode = %q, want %s; source=%s decline=%s fallback=%s", trace.PlannerMode, tc.searchMode, trace.Source, trace.Decline, trace.Fallback)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("paths = %v, want %v", gotPaths, wantPaths)
			}
			countTrace := &searchTrace{}
			countWant, err := searchAll([]*Index{cIndex, fIndex}, queryOptions{Query: query}, true)
			if err != nil {
				t.Fatalf("oracle count: %v", err)
			}
			count, ok, err := countServiceVolumes(volumes, queryOptions{Query: query, Trace: countTrace})
			if err != nil || !ok || count != len(countWant) {
				t.Fatalf("count = %d, handled=%v, err=%v; want %d", count, ok, err, len(countWant))
			}
			if countTrace.PlannerMode != tc.countMode {
				t.Fatalf("count planner mode = %q, want %s; source=%s decline=%s fallback=%s", countTrace.PlannerMode, tc.countMode, countTrace.Source, countTrace.Decline, countTrace.Fallback)
			}
		})
	}

	trace := &searchTrace{}
	if _, err := searchServiceVolumes(volumes, queryOptions{Query: "fixtureproj", Limit: 20, Trace: trace}, false); err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode == "global-components" {
		t.Fatal("unanchored bare single term unexpectedly used global components")
	}
}

func TestGlobalPlannerCountVerifiesMalformedLegacyComponentCandidates(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	cases := []struct {
		name    string
		query   string
		corrupt []string
	}{
		{name: "volume-anchor", query: "path:F: workspace-alpha", corrupt: []string{"workspace-alpha"}},
		{name: "common", query: "path:workspace-alpha", corrupt: []string{"workspace-alpha"}},
		{name: "rare", query: "path:target-model-00", corrupt: []string{"target-model-00"}},
		{name: "no-hit", query: "path:missingneedle", corrupt: []string{"missingneedle"}},
		{name: "or", query: "path:workspace-alpha|missingneedle", corrupt: []string{"workspace-alpha", "missingneedle"}},
		{name: "not", query: "path:workspace-alpha !model_v2", corrupt: []string{"workspace-alpha"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			indexes := []*Index{volumes[0].index, volumes[1].index}
			want, err := searchAll(indexes, queryOptions{Query: tc.query}, true)
			if err != nil {
				t.Fatalf("oracle count: %v", err)
			}
			for _, vol := range volumes {
				// Legacy indexes can have component candidates without the
				// canonical subtree metadata needed for exact interval coverage.
				vol.subtreeOrder = nil
				vol.subtreeStart = nil
				vol.subtreeEnd = nil
				if vol.queryIndex == nil {
					vol.queryIndex = &residentQueryIndex{}
				}
				if vol.queryIndex.components == nil {
					vol.queryIndex.components = make(map[string][]uint32)
				}
				for _, term := range tc.corrupt {
					// Simulate a malformed legacy derived posting: root zero is a
					// complete candidate superset, but not an exact path predicate.
					vol.queryIndex.components[term] = []uint32{0}
				}
			}

			trace := &searchTrace{}
			got, handled, err := countServiceVolumes(volumes, queryOptions{Query: tc.query, Trace: trace})
			if err != nil || !handled {
				t.Fatalf("count handled=%v err=%v; trace=%+v", handled, err, trace)
			}
			if got != len(want) {
				t.Fatalf("count = %d, want oracle %d; source=%s", got, len(want), trace.Source)
			}
		})
	}
}

func TestGlobalPlannerComponentShortExtensionDefaultKeepsGlobalNameOrder(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("c-short-ext.gsi", singleDownloadFileCompactIndex("C:", "z-notes.md")),
		newServiceVolumeIndex("f-short-ext.gsi", singleDownloadFileCompactIndex("F:", "a-notes.md")),
	}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(volumes, queryOptions{Query: "path:Downloads md", Limit: 1, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\Downloads\a-notes.md`}) {
		t.Fatalf("paths = %v, want F volume global best promoted extension match", gotPaths)
	}
}

func TestGlobalPlannerImplicitPathSeparatorDefault(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{Query: `project-03\workspace-alpha\model_v2 type:file`, Limit: 20}
	trace := &searchTrace{}
	searchOpts := opts
	searchOpts.Trace = trace
	got, err := searchServiceVolumes(volumes, searchOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\project-03\workspace-alpha\model_v2\target-model-03.bin`}) {
		t.Fatalf("paths = %v, want F project-03 model result", gotPaths)
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes(volumes, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("countServiceVolumes declined implicit path separator query")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestGlobalPlannerImplicitPathSeparatorShortComponentsDefault(t *testing.T) {
	makeVolume := func(volume, leaf string) *serviceVolumeIndex {
		idx := &Index{
			Source:  "usn",
			Volume:  volume,
			Compact: true,
			Records: []CompactRecord{
				{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
				{FRN: 2, ParentFRN: 1, Parent: 0, Name: "x", Mode: uint32(os.ModeDir)},
				{FRN: 3, ParentFRN: 2, Parent: 1, Name: "y", Mode: uint32(os.ModeDir)},
				{FRN: 4, ParentFRN: 3, Parent: 2, Name: leaf},
			},
		}
		buildOrders(idx)
		return newServiceVolumeIndex(volume+"-short-path.gsi", idx)
	}
	volumes := []*serviceVolumeIndex{makeVolume("C:", "other.bin"), makeVolume("F:", "target.bin")}
	opts := queryOptions{Query: `x\y\target.bin type:file`, Limit: 10}
	pq := mustParseQuery(t, opts)
	if !slices.Equal(pq.ImplicitPathTerms, []string{"x", "y", "target.bin"}) {
		t.Fatalf("implicit path terms = %v", pq.ImplicitPathTerms)
	}
	bare := mustParseQuery(t, queryOptions{Query: "x y target.bin", MatchPath: true})
	if globalComponentDefaultSupported(bare, nonVolumeTerms(bare.Terms)) {
		t.Fatal("bare short terms unexpectedly enabled the default global planner")
	}

	trace := &searchTrace{}
	opts.Trace = trace
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\x\y\target.bin`}) {
		t.Fatalf("paths = %v, want short separator-derived match", gotPaths)
	}
	countTrace := &searchTrace{}
	opts.Trace = countTrace
	count, ok, err := countServiceVolumes(volumes, opts)
	if err != nil || !ok || count != 1 {
		t.Fatalf("count = %d, handled=%v, err=%v; want 1, true, nil", count, ok, err)
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
}

func TestGlobalPlannerUnderMatchesServiceVolumes(t *testing.T) {
	cases := []queryOptions{
		{Query: "ext:bin", Under: `F:\project-03\workspace-alpha`, Limit: 20},
		{Query: "path:workspace-alpha model_v2", MatchPath: true, Under: `F:\project-03`, Limit: 20},
	}
	for _, opts := range cases {
		t.Run(opts.Query+"/under", func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				countMatches, err := searchServiceVolumes(volumes, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				wantCount = len(countMatches)
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global under paths = %v, want %v", gotPaths, wantPaths)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global under count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global under count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestParentFilterSearchCountParity(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	cases := []struct {
		query string
		want  []string
	}{
		{
			query: "parent:model_v2 type:file",
			want: []string{
				`F:\project-00\workspace-alpha\model_v2\target-model-00.bin`,
				`F:\project-01\workspace-alpha\model_v2\target-model-01.bin`,
				`F:\project-02\workspace-alpha\model_v2\target-model-02.bin`,
				`F:\project-03\workspace-alpha\model_v2\target-model-03.bin`,
				`F:\project-04\workspace-alpha\model_v2\target-model-04.bin`,
				`F:\project-05\workspace-alpha\model_v2\target-model-05.bin`,
				`F:\project-06\workspace-alpha\model_v2\target-model-06.bin`,
				`F:\project-07\workspace-alpha\model_v2\target-model-07.bin`,
			},
		},
		{
			query: "parent:workspace-alpha type:dir",
			want: []string{
				`F:\project-00\workspace-alpha\model_v2`,
				`F:\project-01\workspace-alpha\model_v2`,
				`F:\project-02\workspace-alpha\model_v2`,
				`F:\project-03\workspace-alpha\model_v2`,
				`F:\project-04\workspace-alpha\model_v2`,
				`F:\project-05\workspace-alpha\model_v2`,
				`F:\project-06\workspace-alpha\model_v2`,
				`F:\project-07\workspace-alpha\model_v2`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			opts := queryOptions{Query: tc.query, Limit: 20}
			got, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if paths := pathsOf(got); !slices.Equal(paths, tc.want) {
				t.Fatalf("paths = %v, want %v", paths, tc.want)
			}
			count, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				countMatches, err := searchServiceVolumes(volumes, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				count = len(countMatches)
			}
			if count != len(tc.want) {
				t.Fatalf("count = %d, want %d", count, len(tc.want))
			}
		})
	}
}

func TestParentFilterBuildsCandidateSource(t *testing.T) {
	vol := workspaceAlphaModelVolume("F:", true)
	pq := mustParseQuery(t, queryOptions{Query: "parent:model_v2 type:file"})
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined parent filter")
	}
	if len(plan.sources) == 0 || plan.sources[0].name != "parent:model_v2" {
		t.Fatalf("plan sources = %+v, want parent:model_v2 source", plan.sources)
	}
	if got := plan.execute(); len(got) != 8 {
		t.Fatalf("parent candidate count = %d, want 8; ids=%v", len(got), got)
	}
}

func TestGlobalPlannerParentFilterMatchesServiceVolumes(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{Query: "path:workspace-alpha model_v2 parent:workspace-alpha", Limit: 20}
	want, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	wantCount, ok, err := countServiceVolumes(volumes, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		countMatches, err := searchServiceVolumes(volumes, opts, true)
		if err != nil {
			t.Fatal(err)
		}
		wantCount = len(countMatches)
	}

	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	trace := &searchTrace{}
	globalOpts := opts
	globalOpts.Trace = trace
	got, err := searchServiceVolumes(volumes, globalOpts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
	}
	if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("global parent paths = %v, want %v", gotPaths, wantPaths)
	}
	countTrace := &searchTrace{}
	countOpts := opts
	countOpts.Trace = countTrace
	count, ok, err := countServiceVolumes(volumes, countOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("global parent count declined")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
	}
	if count != wantCount {
		t.Fatalf("global parent count = %d, want %d", count, wantCount)
	}
}

func TestGlobalPlannerParentOnlyMatchesServiceVolumes(t *testing.T) {
	for _, query := range []string{"parent:model_v2", "parent:model_v2 ext:bin"} {
		t.Run(query, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				workspaceAlphaModelVolume("C:", false),
				workspaceAlphaModelVolume("F:", true),
			}
			opts := queryOptions{Query: query, Limit: 20}
			want, err := searchServiceVolumes(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, ok, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				countMatches, err := searchServiceVolumes(volumes, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				wantCount = len(countMatches)
			}

			t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
			trace := &searchTrace{}
			globalOpts := opts
			globalOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, globalOpts, false)
			if err != nil {
				t.Fatal(err)
			}
			if trace.PlannerMode != "global-components" {
				t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
			}
			if gotPaths, wantPaths := pathsOf(got), pathsOf(want); !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("global parent paths = %v, want %v", gotPaths, wantPaths)
			}
			countTrace := &searchTrace{}
			countOpts := opts
			countOpts.Trace = countTrace
			count, ok, err := countServiceVolumes(volumes, countOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("global parent count declined")
			}
			if countTrace.PlannerMode != "global-count-components" {
				t.Fatalf("count planner mode = %q, want global-count-components", countTrace.PlannerMode)
			}
			if count != wantCount {
				t.Fatalf("global parent count = %d, want %d", count, wantCount)
			}
		})
	}
}

func TestGlobalPlannerRequestSeqDoesNotStopAfterFirstHit(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", true),
		workspaceAlphaModelVolume("F:", true),
	}
	opts := queryOptions{
		Query:      "path:workspace-alpha model_v2 type:file sort:size",
		Limit:      20,
		RequestSeq: time.Now().UnixNano(),
	}
	trace := &searchTrace{}
	opts.Trace = trace
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components", trace.PlannerMode)
	}
	paths := pathsOf(got)
	var cHits, fHits int
	for _, path := range paths {
		if strings.HasPrefix(path, `C:\`) {
			cHits++
		}
		if strings.HasPrefix(path, `F:\`) {
			fHits++
		}
	}
	if cHits == 0 || fHits == 0 {
		t.Fatalf("paths = %v, want hits from both C: and F:", paths)
	}
}

func TestGeneratedImplicitPathSeparatorQueryParity(t *testing.T) {
	idx := dottedPathBenchmarkIndex(800)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		implicit string
		explicit string
	}{
		{
			implicit: `workspace\dataset-000000.nrrd\metadata-000000.json`,
			explicit: `path:workspace path:dataset-000000.nrrd metadata-000000.json`,
		},
		{
			implicit: `workspace/nrrd-cache/cache-000097.json`,
			explicit: `path:workspace path:nrrd-cache cache-000097.json`,
		},
		{
			implicit: `nrrd-cache\cache-000097.json`,
			explicit: `path:nrrd-cache cache-000097.json`,
		},
	}
	filters := []string{"", "ext:json", "type:file", "!backup", "size:>0", "dm:2026-05-01"}
	limits := []int{1, 5, 25}
	for _, tc := range cases {
		for _, filter := range filters {
			for _, limit := range limits {
				implicit := strings.TrimSpace(tc.implicit + " " + filter)
				explicit := strings.TrimSpace(tc.explicit + " " + filter)
				t.Run(fmt.Sprintf("%s/filter:%s/limit:%d", tc.implicit, filter, limit), func(t *testing.T) {
					implicitOpts := queryOptions{Query: implicit, Limit: limit}
					explicitOpts := queryOptions{Query: explicit, Limit: limit}
					pq := mustParseQuery(t, implicitOpts)
					if !pq.MatchPath {
						t.Fatalf("implicit path query %q did not infer MatchPath", implicit)
					}
					implicitFast, err := searchCompactWithCache(idx, implicitOpts, false, vol.pathCache, vol.nameTermCandidates)
					if err != nil {
						t.Fatalf("implicit search: %v", err)
					}
					explicitFast, err := searchCompactWithCache(idx, explicitOpts, false, vol.pathCache, vol.nameTermCandidates)
					if err != nil {
						t.Fatalf("explicit search: %v", err)
					}
					if got, want := pathsOf(implicitFast), pathsOf(explicitFast); !sameOrderedStrings(got, want) {
						t.Fatalf("implicit paths = %v, explicit paths = %v", got, want)
					}
					full, err := searchCompactWithCache(idx, implicitOpts, false, make(map[int]string), nil)
					if err != nil {
						t.Fatalf("full implicit search: %v", err)
					}
					if got, want := pathsOf(implicitFast), pathsOf(full); !sameOrderedStrings(got, want) {
						t.Fatalf("candidate paths = %v, full paths = %v", got, want)
					}
				})
			}
		}
	}
}

func TestDriveScopedBroadExtensionSearchRoutesToRequestedVolume(t *testing.T) {
	cIdx := dottedPathBenchmarkIndex(1000)
	cIdx.Volume = "C:"
	fIdx := dottedPathBenchmarkIndex(1000)
	fIdx.Volume = "F:"
	cVol := newServiceVolumeIndex("c.gsi", cIdx)
	fVol := newServiceVolumeIndex("f.gsi", fIdx)
	for _, ext := range []string{"nrrd", "raw", "pdf"} {
		query := "path:F: ." + ext
		t.Run(query, func(t *testing.T) {
			volumes, err := serviceVolumesForQuery([]*serviceVolumeIndex{cVol, fVol}, queryOptions{Query: query, MatchPath: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(volumes) != 1 || volumes[0] != fVol {
				t.Fatalf("volumes = %+v, want only F:", volumes)
			}
			got, err := searchServiceVolumes([]*serviceVolumeIndex{cVol, fVol}, queryOptions{Query: query, MatchPath: true, Limit: 25}, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range got {
				if !strings.HasPrefix(strings.ToUpper(entry.Path), `F:\`) {
					t.Fatalf("result %q is not on F:", entry.Path)
				}
			}
		})
	}
}

func TestGeneratedBroadPathQueryParityAcrossResidentVariants(t *testing.T) {
	idx := dottedPathBenchmarkIndex(800)
	queries := []queryOptions{
		{Query: "path:.nrrd", Limit: 25},
		{Query: "path:.nrrd ext:json", Limit: 25},
		{Query: "path:nrrd glob:*.json", Limit: 25},
		{Query: "path:dataset ext:json", Limit: 25},
		{Query: "path:.nrrd", Under: `C:\workspace\dataset-000000.nrrd`, Limit: 25},
		{Query: "path:nrrd !backup", Limit: 25},
		{Query: "path:workspace ext:nrrd|json", Limit: 25},
		{Query: `workspace\dataset-000000.nrrd\metadata-000000.json`, Limit: 25},
		{Query: `workspace/nrrd-cache/cache-000097.json`, Limit: 25},
		{Query: "path:trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:Dataset trainingdata .nrrd", Limit: 25},
		{Query: "path:workspace trainingdata Dataset .nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset ext:nrrd", Limit: 25},
		{Query: "path:trainingdata Dataset glob:*.nrrd", Limit: 25},
		{Query: "path:trainingdata missing .nrrd", Limit: 25},
	}
	variants := []struct {
		name   string
		mutate func(*serviceVolumeIndex)
	}{
		{name: "normal"},
		{name: "no-child-ranges", mutate: func(vol *serviceVolumeIndex) {
			vol.childOffsets = nil
			vol.childIDs = nil
			vol.rootIDs = nil
			vol.subtreeOrder = nil
			vol.subtreeStart = nil
			vol.subtreeEnd = nil
		}},
		{name: "no-children-map", mutate: func(vol *serviceVolumeIndex) {
			vol.children = nil
			vol.childOffsets = nil
			vol.childIDs = nil
			vol.rootIDs = nil
			vol.subtreeOrder = nil
			vol.subtreeStart = nil
			vol.subtreeEnd = nil
		}},
		{name: "no-query-index", mutate: func(vol *serviceVolumeIndex) {
			vol.queryIndex = nil
		}},
		{name: "no-exact-names", mutate: func(vol *serviceVolumeIndex) {
			vol.exactNames = nil
		}},
	}
	for _, variant := range variants {
		vol := newServiceVolumeIndex("fixture.gsi", idx)
		vol.rebuildNameTrigramsLocked()
		if variant.mutate != nil {
			variant.mutate(vol)
		}
		for _, opts := range queries {
			t.Run(variant.name+"/"+opts.Query, func(t *testing.T) {
				full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
				if err != nil {
					t.Fatalf("full search: %v", err)
				}
				fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
				if err != nil {
					t.Fatalf("candidate search: %v", err)
				}
				if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
					t.Fatalf("candidate paths = %v, full paths = %v", got, want)
				}
			})
		}
	}
}

func TestNameTrigramPathCandidatesIncludeDirectoryDescendants(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "path:C: .opencode", Limit: 20}
	trigram, ok := vol.nameTrigramCandidates(mustParseQuery(t, opts))
	if !ok {
		t.Fatal("nameTrigramCandidates declined path dotted substring query")
	}
	gotNames := namesOf(entriesForIDs(idx, trigram))
	if !sameStringSet(gotNames, []string{"ai.opencode.desktop", "settings.json"}) {
		t.Fatalf("trigram candidate names = %v, want directory and descendant", gotNames)
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
		t.Fatalf("fast names = %v, full names = %v", got, want)
	}
}

func TestLimitedPathComponentPostingMatchesFullSearch(t *testing.T) {
	idx := dottedPathBenchmarkIndex(25000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "path:C: .opencode", Limit: 20}
	pq := mustParseQuery(t, opts)
	dropSatisfiedVolumeTerms(&pq, idx.Volume)
	pq.Limit = normalizedLimit(opts.Limit, false)

	limited, ok := vol.pathPlanTermPostingLimited(".opencode", pq)
	if !ok {
		t.Fatal("pathPlanTermPostingLimited declined selective component query")
	}
	if len(limited) == 0 || len(limited) > opts.Limit {
		t.Fatalf("limited candidates = %d, want 1..%d", len(limited), opts.Limit)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok || len(plan.sources) != 1 ||
		(!strings.HasPrefix(plan.sources[0].name, "term-limited:") && !strings.HasPrefix(plan.sources[0].name, "path-term:")) {
		t.Fatalf("plan = %+v, ok=%v, want bounded path source", plan, ok)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("full search: %v", err)
	}
	fast, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatalf("service search: %v", err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("limited paths = %v, full paths = %v", got, want)
	}
}

func TestComponentMultiTermTopReturnsCompleteUnderLimitResults(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := fixtureprojTrainingdataFixture(167)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	for _, opts := range []queryOptions{
		{Query: "path:F: fixtureproj-dev trainingdata", Limit: 200},
		{Query: `path:F:\fixtureproj trainingdata`, Limit: 20},
	} {
		t.Run(opts.Query, func(t *testing.T) {
			trace := &searchTrace{}
			fast, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, Limit: opts.Limit, MatchPath: true, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatal(err)
			}
			full, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, Limit: opts.Limit, MatchPath: true}, false, make(map[int]string), nil)
			if err != nil {
				t.Fatal(err)
			}
			if trace.Source != "path-component-multi-top" {
				t.Fatalf("source = %q, want path-component-multi-top", trace.Source)
			}
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("paths = %v, want %v", got, want)
			}
			if opts.Limit > 167 && len(fast) <= 167 {
				t.Fatalf("result count = %d, want fixture files plus matching directories", len(fast))
			}
		})
	}
}

func TestBroadDownloadsPathQueriesUseBoundedDirectoryTop(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := broadDownloadsMarkdownFixture(35_000, 800, true)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	vol.rebuildNameTrigramsLocked()
	for _, opts := range []queryOptions{
		{Query: "path:Downloads", Limit: 25},
		{Query: "path:Downloads ext:md", Limit: 25},
		{Query: "Downloads md", MatchPath: true, Limit: 25},
	} {
		t.Run(opts.Query, func(t *testing.T) {
			trace := &searchTrace{}
			fast, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, MatchPath: opts.MatchPath, Limit: opts.Limit, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
			if err != nil {
				t.Fatal(err)
			}
			full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
				t.Fatalf("paths = %v, want %v", got, want)
			}
			switch opts.Query {
			case "path:Downloads":
				if trace.Source != "path-directory-term-top" && trace.Source != "path-component-direct-top" && trace.Source != "path-component-root-top" {
					t.Fatalf("source = %q, want bounded directory/component source", trace.Source)
				}
				if len(fast) != opts.Limit {
					t.Fatalf("result count = %d, want %d", len(fast), opts.Limit)
				}
			case "path:Downloads ext:md":
				if trace.Source != "path-directory-term-top" {
					t.Fatalf("source = %q, want path-directory-term-top", trace.Source)
				}
				if len(fast) != opts.Limit {
					t.Fatalf("result count = %d, want %d", len(fast), opts.Limit)
				}
			default:
				if len(fast) != opts.Limit {
					t.Fatalf("result count = %d, want %d", len(fast), opts.Limit)
				}
			}
		})
	}
}

func TestBareExtensionPathQueryUsesRealAnchorWhenAnchorLooksExtensionShaped(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := clientDvarrayFixture(30_000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.buildCompactChildren()
	vol.buildSubtreeRanges()
	vol.rebuildNameTrigramsLocked()
	opts := queryOptions{Query: "path:F: client dvarray", MatchPath: true, Limit: 20}
	trace := &searchTrace{}
	fast, err := searchCompactWithCache(idx, queryOptions{Query: opts.Query, MatchPath: opts.MatchPath, Limit: opts.Limit, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Source != "path-bare-extension-multi-top" {
		t.Fatalf("source = %q, want path-bare-extension-multi-top", trace.Source)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	if got := pathsOf(fast); !sameOrderedStrings(got, []string{`F:\Example Sync\Analysis\Projects\Example Client\PROJECT-2024-07-WELL-001\ml\876955601027075-top-thickness-ml-fixed.dvarray`}) {
		t.Fatalf("paths = %v, want example client dvarray hit", got)
	}
	if trace.Candidates > 128 {
		t.Fatalf("candidates = %d, want bounded anchor set", trace.Candidates)
	}
	pq := mustParseQuery(t, opts)
	dropSatisfiedVolumeTerms(&pq, idx.Volume)
	candidates, ok := vol.extPostingPathTermCandidates("dvarray", pq.Terms, opts.Limit)
	if !ok {
		t.Fatalf("extPostingPathTermCandidates declined %d dvarray postings; want bounded verifier route", len(vol.extPosting("dvarray")))
	}
	if got := pathsOf(entriesForIDs(idx, candidates)); !sameOrderedStrings(got, []string{`F:\Example Sync\Analysis\Projects\Example Client\PROJECT-2024-07-WELL-001\ml\876955601027075-top-thickness-ml-fixed.dvarray`}) {
		t.Fatalf("extension-posting verifier paths = %v, want example client dvarray hit", got)
	}
	countPQ := mustParseQuery(t, opts)
	countPQ.CountOnly = true
	count, ok := vol.fastPostingCount(countPQ)
	if !ok {
		t.Fatalf("fastPostingCount declined bare extension path query")
	}
	if count != 1 {
		t.Fatalf("fastPostingCount = %d, want 1", count)
	}
}

func TestComponentTrigramDeclinesBroadDirectoryExpansion(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := broadComponentExpansionIndex(serviceComponentTrigramExpansionMaxIDs + 500)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "path:C: workspace", Limit: 20}
	pq := mustParseQuery(t, opts)
	if candidates, ok := vol.componentTrigramCandidates(pq); ok {
		t.Fatalf("componentTrigramCandidates returned %d broad workspace candidates, want fallback", len(candidates))
	}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("fast paths = %v, full paths = %v", got, want)
	}
}

func TestLargePlainComponentRootUsesBoundedTopSource(t *testing.T) {
	idx := broadComponentExpansionIndex(100_100)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "path:C: workspace", Limit: 20}
	pq := mustParseQuery(t, opts)
	dropSatisfiedVolumeTerms(&pq, idx.Volume)
	pq.Limit = opts.Limit

	candidates, ok := vol.componentRootTopCandidates(pq)
	if !ok {
		t.Fatal("componentRootTopCandidates declined large plain component root")
	}
	if len(candidates) != opts.Limit {
		t.Fatalf("candidate count = %d, want %d", len(candidates), opts.Limit)
	}
	if names := namesOf(entriesForIDs(idx, candidates)); !containsString(names, "workspace") {
		t.Fatalf("candidate names = %v, want workspace root included", names)
	}
}

func TestLowMemoryDeterministicQueryMatrixUnderTarget(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	cIdx := dottedPathBenchmarkIndex(100_000)
	fIdx := dottedPathBenchmarkIndex(100_000)
	fIdx.Volume = "F:"
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("lowmem-c.gsi", cIdx),
		newServiceVolumeIndex("lowmem-f.gsi", fIdx),
	}
	for _, vol := range volumes {
		vol.rebuildNameTrigramsLocked()
	}
	queries := []string{
		".nrrd",
		".raw",
		".pdf",
		".json",
		".dll",
		".exe",
		".opencode",
		"path:F: .nrrd",
		"path:F: nrrd",
		"path:F: .raw",
		"path:F: raw",
		"path:F: .pdf",
		"path:F: pdf",
		"path:C: pvsm",
		"path:C: .pvsm",
		"path:C: .exe",
		"path:C: Users",
		"path:C: AppData",
		"path:F: workspace",
		"path:Windows",
		"path:node_modules",
		"path:Downloads .nrrd",
		"path:F: Downloads .nrrd",
		"path:C: trainingdata Dataset .nrrd",
		"path:F: trainingdata Dataset .nrrd",
		"path:C: workspace trainingdata Dataset .nrrd",
		"path:F: workspace trainingdata Dataset .nrrd",
		"path:C: trainingdata Dataset ext:nrrd",
		"path:C: trainingdata Dataset glob:*.nrrd",
		"path:C: missing Dataset .nrrd",
		"path:F: missing Dataset .nrrd",
		"path:C:.nrrd",
		"zzzz-no-hit-seekfs",
	}
	const iterations = 5
	all := make([]float64, 0, len(queries)*iterations)
	perQuery := make(map[string][]float64, len(queries))
	for i := 0; i < iterations; i++ {
		for _, query := range queries {
			start := time.Now()
			_, err := searchServiceVolumes(volumes, queryOptions{Query: query, Limit: 100}, false)
			if err != nil {
				t.Fatalf("query %q failed: %v", query, err)
			}
			ms := float64(time.Since(start).Microseconds()) / 1000
			all = append(all, ms)
			perQuery[query] = append(perQuery[query], ms)
		}
	}
	if p95 := percentile(append([]float64(nil), all...), 0.95); p95 > 100 {
		if envBool("SEEKFS_ENFORCE_LATENCY_TESTS") {
			t.Fatalf("lowmem deterministic query matrix p95 = %.3fms, want <= 100ms; per-query=%v", p95, perQuery)
		}
		t.Logf("lowmem deterministic query matrix p95 = %.3fms over soft 100ms budget; per-query=%v", p95, perQuery)
	}
}

func TestNameTrigramRecentOverlayFindsCreateWithMissingBaseGram(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)

	id := idx.appendCompactRecord(CompactRecord{FRN: 30, ParentFRN: 2, Parent: 1, Name: "zzquark-note.txt", Size: 1, ModUnix: time.Now().UnixNano()})
	vol.addFRNID(30, id)
	vol.addExactName(id)
	vol.markNameTrigramRecent(id)

	opts := queryOptions{Query: "path:C: zzquark", Limit: 20}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(fast); !sameStringSet(names, []string{"zzquark-note.txt"}) {
		t.Fatalf("recent create names = %v, want zzquark-note.txt", names)
	}
}

func TestFilenameTrigramCandidatesMatchFullSearch(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: ".opencode", Limit: 20}
	candidates, ok := vol.filenameTrigramCandidates(mustParseQuery(t, opts))
	if !ok {
		t.Fatal("filenameTrigramCandidates declined selective filename query")
	}
	if names := namesOf(entriesForIDs(idx, candidates)); !sameStringSet(names, []string{"ai.opencode.desktop"}) {
		t.Fatalf("filename trigram candidates = %v, want ai.opencode.desktop", names)
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
		t.Fatalf("fast names = %v, full names = %v", got, want)
	}
}

func TestFilenameTrigramCompletePNGRExactEmptyDeclinesNoFallback(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	trigrams := buildNameTrigramIndex(idx)
	trigrams.gramCountsComplete = true
	vol.nameTrigrams.Store(trigrams)

	trace := &searchTrace{}
	pq := mustParseQuery(t, queryOptions{Query: "zzzz-no-hit", Limit: 20, Trace: trace})
	candidates, ok := vol.filenameTrigramCandidates(pq)
	if !ok || len(candidates) != 0 {
		t.Fatalf("complete PNGR candidates=%v ok=%v, want exact empty", candidates, ok)
	}
	if trace.Source != "exact-empty" || trace.Candidates != 0 || trace.Decline != "" {
		t.Fatalf("exact-empty trace=%+v, want terminal exact-empty without decline", trace)
	}

	searchRunTrace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "zzzz-no-hit", Limit: 20, Trace: searchRunTrace,
	}, false)
	if err != nil || len(got) != 0 {
		t.Fatalf("complete PNGR search got=%v err=%v, want zero", got, err)
	}
	if searchRunTrace.Source != "exact-empty" || searchRunTrace.Candidates != 0 {
		t.Fatalf("complete PNGR search trace=%+v, want exact-empty zero", searchRunTrace)
	}

	countTrace := &searchTrace{}
	count, handled, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "zzzz-no-hit", Trace: countTrace,
	})
	if err != nil || !handled || count != 0 {
		t.Fatalf("complete PNGR count=%d handled=%v err=%v, want zero handled", count, handled, err)
	}
	if countTrace.Source != "exact-empty" || countTrace.Candidates != 0 {
		t.Fatalf("complete PNGR count trace=%+v, want exact-empty zero", countTrace)
	}

	legacy := *trigrams
	legacy.gramCountsComplete = false
	vol.nameTrigrams.Store(&legacy)
	legacyTrace := &searchTrace{}
	legacyPQ := mustParseQuery(t, queryOptions{Query: "zzzz-no-hit", Limit: 20, Trace: legacyTrace})
	if candidates, ok := vol.filenameTrigramCandidates(legacyPQ); ok {
		t.Fatalf("legacy PNGR returned %d candidates, want safe decline", len(candidates))
	}
	if legacyTrace.Decline != "name-trigram:missing-section" {
		t.Fatalf("legacy PNGR trace=%+v, want missing-section decline", legacyTrace)
	}
}

func TestFilenameTrigramRecentOverlayFindsMissingBaseGram(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	id := idx.appendCompactRecord(CompactRecord{FRN: 30, ParentFRN: 2, Parent: 1, Name: "zzquark-note.txt"})
	vol.addFRNID(30, id)
	vol.addExactName(id)
	vol.markNameTrigramRecent(id)

	opts := queryOptions{Query: "zzquark", Limit: 20}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(fast); !sameStringSet(names, []string{"zzquark-note.txt"}) {
		t.Fatalf("recent filename names = %v, want zzquark-note.txt", names)
	}
}

func TestFilenameTrigramDeclinesUnicodeNameTerm(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	records := []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}}
	for i := 0; i < 30_000; i++ {
		name := fmt.Sprintf("plain-%05d.txt", i)
		if i == 20_000 {
			name = "Übersicht.pdf"
		}
		records = append(records, CompactRecord{
			FRN:       uint64(200 + i),
			ParentFRN: 100,
			Parent:    0,
			Name:      name,
		})
	}
	idx := &Index{Source: "usn", Volume: "F:", Compact: true, Records: records}
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	opts := queryOptions{Query: "übersicht", Limit: 10}
	if candidates, ok := vol.filenameTrigramCandidates(mustParseQuery(t, opts)); ok {
		t.Fatalf("filenameTrigramCandidates returned %d unicode candidates, want scan fallback", len(candidates))
	}
	trace := &searchTrace{}
	fast, err := searchCompactWithCache(idx, queryOptions{Query: "übersicht", Limit: 10, Trace: trace}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOf(fast), pathsOf(full); !sameOrderedStrings(got, want) {
		t.Fatalf("unicode name query paths=%v want=%v source=%s", got, want, trace.Source)
	}
	if got := pathsOf(fast); !sameOrderedStrings(got, []string{`F:\Übersicht.pdf`}) {
		t.Fatalf("unicode name query paths=%v, want Übersicht.pdf source=%s", got, trace.Source)
	}
	if trace.Source == "name-trigram" {
		t.Fatalf("unicode name query used trigram source")
	}
}

func TestFilenameTrigramDeclinesCommonTerm(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := dottedPathBenchmarkIndex(100_000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)
	if candidates, ok := vol.filenameTrigramCandidates(mustParseQuery(t, queryOptions{Query: "plain", Limit: 20})); ok {
		t.Fatalf("filenameTrigramCandidates returned %d common candidates, want fallback", len(candidates))
	}
}

func TestNameTrigramRecentOverlayFindsRenamedDirectoryDescendants(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)

	dirID := 10 // ai.opencode.desktop in pathSyntaxFixture.
	rec := idx.compactRecord(dirID)
	vol.removeExactName(dirID)
	rec.Name = "zzquark-folder"
	idx.setCompactRecord(dirID, rec)
	vol.addExactName(dirID)
	vol.markNameTrigramRecent(dirID)

	opts := queryOptions{Query: "path:C: zzquark", Limit: 20}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(fast); !sameStringSet(names, []string{"settings.json", "zzquark-folder"}) {
		t.Fatalf("recent renamed directory names = %v, want directory and descendant", names)
	}
}

func TestNameTrigramRecentOverlayExcludesDeletedBaseMatch(t *testing.T) {
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "1")
	idx := pathSyntaxFixture()
	id := idx.appendCompactRecord(CompactRecord{FRN: 30, ParentFRN: 2, Parent: 1, Name: "zzstable.txt", Size: 1, ModUnix: time.Now().UnixNano()})
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	(&goSearchService{}).rebuildNameTrigramsInBackground(vol)

	rec := idx.compactRecord(id)
	rec.Deleted = true
	idx.setCompactRecord(id, rec)
	vol.markNameTrigramRecent(id)

	opts := queryOptions{Query: "path:C: zzstable", Limit: 20}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(fast); len(names) != 0 {
		t.Fatalf("deleted base match names = %v, want none", names)
	}
}

func TestLiveDottedExtensionQueryMatchesExtensionFilter(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	dotted, err := searchCompactWithCache(idx, queryOptions{Query: ".go", MatchPath: true, Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatalf("dotted extension search: %v", err)
	}
	filtered, err := searchCompactWithCache(idx, queryOptions{Query: "ext:go", MatchPath: true, Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatalf("extension filter search: %v", err)
	}
	if !sameStringSet(namesOf(dotted), namesOf(filtered)) {
		t.Fatalf(".go names = %v, ext:go names = %v", namesOf(dotted), namesOf(filtered))
	}
}

func TestLimitedSingleTermCandidatesMatchFullFirstPage(t *testing.T) {
	idx := syntheticCompactIndex(5000)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	opts := queryOptions{Query: "source", MatchPath: true, Limit: 25}
	full, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("full search: %v", err)
	}
	fast, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatalf("candidate search: %v", err)
	}
	if got, want := namesOf(fast), namesOf(full); !sameStringSet(got, want) {
		t.Fatalf("fast first page = %v, full first page = %v", got, want)
	}
}

func TestPlannerUsesSelectiveExtensionBeforePathVerification(t *testing.T) {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
		})
	}
	add(1, 1, -1, ".", uint32(os.ModeDir))
	add(2, 1, 0, "Downloads", uint32(os.ModeDir))
	add(3, 2, 1, "camera-001.raw", 0)
	add(4, 2, 1, "camera-002.raw", 0)
	add(5, 1, 0, "Lab", uint32(os.ModeDir))
	for i := 0; i < 2000; i++ {
		add(uint64(i+10), 5, 4, fmt.Sprintf("sample-%04d.raw", i), 0)
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "Downloads ext:raw", MatchPath: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined Downloads ext:raw")
	}
	sourceNames := make([]string, 0, len(plan.sources))
	for _, source := range plan.sources {
		sourceNames = append(sourceNames, source.name)
	}
	if !sameStringSet(sourceNames, []string{"ext:raw"}) && !sameStringSet(sourceNames, []string{"ext:raw", "path-term:downloads"}) {
		t.Fatalf("plan sources = %v, want ext:raw plus optional bounded Downloads path source", sourceNames)
	}
	got, err := searchCompactWithCache(idx, queryOptions{Query: "Downloads ext:raw", MatchPath: true, Limit: 20}, false, vol.pathCache, vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"camera-001.raw", "camera-002.raw"}) {
		t.Fatalf("names = %v, want Downloads raw files only", names)
	}
}

func TestDropSatisfiedVolumeTermsOnlyForMatchingVolume(t *testing.T) {
	pq := mustParseQuery(t, queryOptions{Query: "path:F: .pdf", Limit: 20})
	dropSatisfiedVolumeTerms(&pq, "F:")
	if len(pq.Terms) != 0 {
		t.Fatalf("terms after matching F: drop = %v, want no terms", pq.Terms)
	}
	if len(pq.Exts) != 1 || pq.Exts[0] != "pdf" {
		t.Fatalf("exts after matching F: drop = %v, want [pdf]", pq.Exts)
	}
	if !compactCandidateCanSkipEntryMatches(pq, true) {
		t.Fatal("matching volume ext-only path query should skip full entryMatches after candidate selection")
	}

	pq = mustParseQuery(t, queryOptions{Query: "path:F: .pdf", Limit: 20})
	dropSatisfiedVolumeTerms(&pq, "C:")
	if len(pq.Terms) != 1 || pq.Terms[0] != "f:" {
		t.Fatalf("terms after mismatched C: drop = %v, want [f:]", pq.Terms)
	}
	if compactCandidateCanSkipEntryMatches(pq, true) {
		t.Fatal("mismatched volume term must still require full entryMatches")
	}

	pq = mustParseQuery(t, queryOptions{Query: "Downloads ext:raw", MatchPath: true, Limit: 20})
	if compactCandidateCanSkipEntryMatches(pq, true) {
		t.Fatal("path term query must not skip full entryMatches")
	}
}

func TestCandidatePlanSkipsSingleCharacterPathTermWhenSelectiveTermExists(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "c main.go", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		t.Fatal("buildCandidatePlan declined query")
	}
	for _, source := range plan.sources {
		if source.name == "term:c" {
			t.Fatalf("plan sources = %+v, should not build broad single-character path term posting", plan.sources)
		}
	}
	got := plan.execute()
	if len(got) == 0 {
		t.Fatal("plan returned no candidates from the selective term")
	}
}

func TestPathPlanProbeTermsPreferSpecificFragments(t *testing.T) {
	got := pathPlanProbeTerms([]string{"f:", "repo", "tools", "fixtures", "reports", "specific_fixture_tool.py"})
	want := []string{"specific_fixture_tool.py", "fixtures", "reports", "tools", "repo"}
	if !sameStringSet(got, want) {
		t.Fatalf("probe terms = %v, want same terms as %v", got, want)
	}
	for i, term := range want {
		if got[i] != term {
			t.Fatalf("probe terms = %v, want ordered prefix %v", got, want)
		}
	}
}

func TestCandidatePlanDeclinesCaseSensitivePostings(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "case: README", MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vol.plannedCandidates(pq); ok {
		t.Fatal("plannedCandidates accepted case-sensitive query")
	}
}

func TestRegexLiteralCandidatesDeclinesAlternationLiterals(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: `regex:Assets.*\.(dat|txt)$`, MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vol.regexLiteralCandidates(pq); ok {
		t.Fatal("regexLiteralCandidates accepted ambiguous alternation literals")
	}
}

func TestGlobalPlannerRegexLiteralUsesGlobalSource(t *testing.T) {
	idx := dottedPathBenchmarkIndex(200)
	vol := newServiceVolumeIndex("regex.gsi", idx)
	trace := &searchTrace{}
	opts := queryOptions{Query: `regex:.*filtered-volume-cleaned.*`, MatchPath: true, Limit: 10, Trace: trace}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	if !traceHasTerm(trace.Terms, traceTerm{Term: "filtered-volume-cleaned", Kind: "regex-literal", Source: "global:regex-literal"}) {
		t.Fatalf("trace terms = %+v, missing regex literal source", trace.Terms)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`C:\Users\exampleuser\Downloads\filtered-volume-cleaned.nrrd`}) {
		t.Fatalf("paths = %v, want filtered-volume-cleaned.nrrd only", gotPaths)
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: opts.Query, MatchPath: true, Trace: countTrace})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("countServiceVolumes declined regex literal")
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestGlobalPlannerRegexLiteralDeclinesAmbiguousAlternation(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	trace := &searchTrace{}
	opts := queryOptions{Query: `regex:Assets.*\.(dat|txt)$`, MatchPath: true, Limit: 10, Trace: trace}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode == "global-components" {
		t.Fatalf("planner mode = %q, want legacy fallback for ambiguous regex; terms=%+v", trace.PlannerMode, trace.Terms)
	}
	if len(got) == 0 {
		t.Fatal("ambiguous regex fallback returned no matches")
	}
}

func TestGlobalPlannerRegexLiteralIntersectsExtensionSource(t *testing.T) {
	idx := dottedPathBenchmarkIndex(200)
	vol := newServiceVolumeIndex("regex-ext.gsi", idx)
	trace := &searchTrace{}
	opts := queryOptions{Query: `ext:nrrd regex:.*filtered-volume-cleaned.*`, MatchPath: true, Limit: 10, Trace: trace}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s fallback=%s", trace.PlannerMode, trace.Decline, trace.Fallback)
	}
	for _, want := range []traceTerm{
		{Term: "filtered-volume-cleaned", Kind: "regex-literal", Source: "global:regex-literal"},
		{Term: "nrrd", Kind: "extension", Source: "global:ext:nrrd", Exact: true},
	} {
		if !traceHasTerm(trace.Terms, want) {
			t.Fatalf("trace terms = %+v, missing %+v", trace.Terms, want)
		}
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`C:\Users\exampleuser\Downloads\filtered-volume-cleaned.nrrd`}) {
		t.Fatalf("paths = %v, want filtered-volume-cleaned.nrrd only", gotPaths)
	}
}

func TestGlobalPlannerRegexLiteralDeclinesCaseSensitive(t *testing.T) {
	idx := dottedPathBenchmarkIndex(200)
	vol := newServiceVolumeIndex("regex-case.gsi", idx)
	trace := &searchTrace{}
	opts := queryOptions{Query: `case: regex:.*Filtered.*`, MatchPath: true, Limit: 10, Trace: trace}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if trace.PlannerMode == "global-components" {
		t.Fatalf("planner mode = %q, want verified fallback for case-sensitive regex; terms=%+v", trace.PlannerMode, trace.Terms)
	}
	if len(got) != 0 {
		t.Fatalf("case-sensitive regex matches = %v, want none for lowercase path", pathsOf(got))
	}
}
