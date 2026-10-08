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

func TestGlobalBooleanIteratorsDeduplicateAndExcludeLazily(t *testing.T) {
	a := newGlobalIDSliceIterator([]globalRecordID{{volume: 0, local: 1}, {volume: 0, local: 3}, {volume: 1, local: 1}})
	b := newGlobalIDSliceIterator([]globalRecordID{{volume: 0, local: 3}, {volume: 1, local: 0}, {volume: 1, local: 1}})
	union := newGlobalMergeIterator(&a, &b)
	if got := collectGlobalIterator(union, 0); !slices.Equal(got, []globalRecordID{{volume: 0, local: 1}, {volume: 0, local: 3}, {volume: 1, local: 0}, {volume: 1, local: 1}}) {
		t.Fatalf("union = %v", got)
	}
	include := newGlobalIDSliceIterator([]globalRecordID{{volume: 0, local: 1}, {volume: 0, local: 3}, {volume: 1, local: 1}})
	exclude := newGlobalIDSliceIterator([]globalRecordID{{volume: 0, local: 1}, {volume: 1, local: 1}})
	countingExclude := &countingSeekGlobalIterator{globalIDIterator: &exclude}
	without := newGlobalExclusionIterator(&include, countingExclude)
	if got := collectGlobalIterator(without, 0); !slices.Equal(got, []globalRecordID{{volume: 0, local: 3}}) {
		t.Fatalf("exclusion = %v", got)
	}
	if countingExclude.seeks == 0 {
		t.Fatal("exclusion did not use SeekGE on the broad negative iterator")
	}
}

type countingSeekGlobalIterator struct {
	globalIDIterator
	seeks int
}

func (it *countingSeekGlobalIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	it.seeks++
	return it.globalIDIterator.SeekGE(target)
}

func TestBooleanLocalPlanUsesBoundedTopN(t *testing.T) {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Records: []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "nrrd-z.txt"},
		{FRN: 3, ParentFRN: 1, Parent: 0, Name: "raw-a.txt"},
		{FRN: 4, ParentFRN: 1, Parent: 0, Name: "nrrd-a.txt"},
		{FRN: 5, ParentFRN: 1, Parent: 0, Name: "other.txt"},
	}}
	buildOrders(idx)
	idx.packCompactRecords(true)
	vol := newServiceVolumeIndex("boolean-local.gsi", idx)
	pq, err := parseQuery(queryOptions{Query: "nrrd|raw", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	pq.Limit = 1
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok || len(plan.sources) != 1 || len(plan.sources[0].union) != 2 {
		t.Fatalf("plan = %+v, want one two-way union", plan)
	}
	got, scanned, ok := plan.executeTop(pq)
	if !ok || scanned == 0 || scanned > 2 || len(got) != 1 {
		t.Fatalf("bounded top = ids=%v scanned=%d ok=%v, want bounded two-branch top", got, scanned, ok)
	}
}

func TestGlobalBooleanPersistedTopAndCountMatchOracle(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	volumes := deterministicMultiVolumeCorpus(t, 0x0b001)
	queries := []string{
		"path:C: nrrd|raw",
		"path:C: raw|nrrd",
		"path:C: nrrd|nrrd",
		"path:C: zzzz-no-hit|no-such-term",
		"path:C: a|raw",
		"path:C: zzzz-no-hit|raw sort:path",
		"path:C: nrrd|raw sort:modified",
		"path:C: nrrd|raw sort:extension",
		"path:C: nrrd|raw sort:type",
		"path:F: nrrd|raw",
		"path:C: nrrd !raw sort:size",
	}
	for _, query := range queries {
		for _, limit := range []int{1, 20, 100} {
			t.Run(fmt.Sprintf("%s-limit-%d", query, limit), func(t *testing.T) {
				opts := queryOptions{Query: query, Limit: limit}
				want, err := r5ExhaustivePlannerOracle(volumes, opts, false)
				if err != nil {
					t.Fatal(err)
				}
				trace := &searchTrace{}
				got, err := searchServiceVolumes(volumes, queryOptions{Query: query, Limit: limit, Trace: trace}, false)
				if err != nil {
					t.Fatalf("search: %v trace=%+v", err, *trace)
				}
				if !slices.Equal(pathsOf(got), pathsOf(want)) {
					t.Fatalf("search paths=%v want=%v trace=%+v", pathsOf(got), pathsOf(want), *trace)
				}
				wantCount, err := r5ExhaustivePlannerOracle(volumes, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				countTrace := &searchTrace{}
				gotCount, _, err := countServiceVolumes(volumes, queryOptions{Query: query, Limit: limit, Trace: countTrace})
				if err != nil {
					t.Fatalf("count: %v trace=%+v", err, *countTrace)
				}
				if gotCount != len(wantCount) {
					t.Fatalf("count=%d want=%d trace=%+v", gotCount, len(wantCount), *countTrace)
				}
				if strings.Contains(query, "!") {
					if trace.Source != "global:boolean-iterator" || countTrace.Source != "global:boolean-iterator" {
						t.Fatalf("NOT did not use lazy iterator: search=%+v count=%+v", *trace, *countTrace)
					}
				} else if trace.Source != "global:boolean-persisted-top" || countTrace.Source != "global:boolean-persisted-count" {
					t.Fatalf("OR did not use persisted top/count: search=%+v count=%+v", *trace, *countTrace)
				}
			})
		}
	}
}

func TestGlobalBooleanCancellationUsesSafeFallback(t *testing.T) {
	volumes := deterministicMultiVolumeCorpus(t, 0x0b002)
	canceled := func() bool { return true }
	for _, countOnly := range []bool{false, true} {
		trace := &searchTrace{}
		opts := queryOptions{Query: "path:C: nrrd|raw", Limit: 20, Cancel: canceled, Trace: trace}
		if countOnly {
			_, _, err := countServiceVolumes(volumes, opts)
			if !errors.Is(err, errQueryCanceled) {
				t.Fatalf("count cancellation error=%v trace=%+v, want %v", err, *trace, errQueryCanceled)
			}
			continue
		}
		_, err := searchServiceVolumes(volumes, opts, false)
		if !errors.Is(err, errQueryCanceled) {
			t.Fatalf("search cancellation error=%v trace=%+v, want %v", err, *trace, errQueryCanceled)
		}
	}
}

func TestSearchAllMultiIndexLimitDoesNotHideLaterBetterMatch(t *testing.T) {
	cIdx := singleFileCompactIndex("C:", "z-match.txt")
	fIdx := singleFileCompactIndex("F:", "a-match.txt")

	got, err := searchAll([]*Index{cIdx, fIdx}, queryOptions{Query: "match", Limit: 1}, false)
	if err != nil {
		t.Fatalf("searchAll: %v", err)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\workspace\a-match.txt`}) {
		t.Fatalf("paths = %v, want F volume global best match", gotPaths)
	}
}

func TestSearchAllPromotedShortExtensionDoesNotWaterfall(t *testing.T) {
	cIdx := singleDownloadFileCompactIndex("C:", "z-notes.md")
	fIdx := singleDownloadFileCompactIndex("F:", "a-notes.md")

	got, err := searchAll([]*Index{cIdx, fIdx}, queryOptions{Query: "path:Downloads md", Limit: 1}, false)
	if err != nil {
		t.Fatalf("searchAll: %v", err)
	}
	if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\Downloads\a-notes.md`}) {
		t.Fatalf("paths = %v, want F volume global best promoted extension match", gotPaths)
	}
}

func TestServiceVolumesPromotedShortExtensionDoesNotWaterfall(t *testing.T) {
	for _, lowmem := range []bool{false, true} {
		t.Run(fmt.Sprintf("lowmem=%v", lowmem), func(t *testing.T) {
			if lowmem {
				t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
			}
			volumes := []*serviceVolumeIndex{
				newServiceVolumeIndex("c-short-ext.gsi", singleDownloadFileCompactIndex("C:", "z-notes.md")),
				newServiceVolumeIndex("f-short-ext.gsi", singleDownloadFileCompactIndex("F:", "a-notes.md")),
			}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: "path:Downloads md", Limit: 1}, false)
			if err != nil {
				t.Fatalf("searchServiceVolumes: %v", err)
			}
			if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\Downloads\a-notes.md`}) {
				t.Fatalf("paths = %v, want F volume global best promoted extension match", gotPaths)
			}
		})
	}
}

func TestMultiVolumePlannerDeclineDoesNotUsePerVolumeTerminal(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("c-declined.gsi", singleFileCompactIndex("C:", "needle.txt")),
		newServiceVolumeIndex("f-declined.gsi", singleFileCompactIndex("F:", "needle.txt")),
	}
	// Model the only unsafe global-planner decline that must not fall through:
	// an active overlay whose snapshot has not been published yet.
	volumes[0].overlay.watermark.Store(1)
	volumes[0].snap.Store(nil)

	trace := &searchTrace{}
	_, err := searchServiceVolumes(volumes, queryOptions{Query: "needle", Trace: trace}, false)
	if !errors.Is(err, errGlobalMultiVolumePlannerDeclined) {
		t.Fatalf("search error = %v, want global multi-volume planner decline", err)
	}
	if trace.PlannerMode == "service-per-volume" || trace.Fallback == "service-per-volume" {
		t.Fatalf("search trace used removed per-volume terminal: %+v", trace)
	}

	countTrace := &searchTrace{}
	_, handled, err := countServiceVolumes(volumes, queryOptions{Query: "needle", Trace: countTrace})
	if !handled || !errors.Is(err, errGlobalMultiVolumePlannerDeclined) {
		t.Fatalf("count = (handled=%v, err=%v), want handled decline error", handled, err)
	}
	if countTrace.PlannerMode == "service-count-per-volume" || countTrace.Fallback == "service-count-per-volume" {
		t.Fatalf("count trace used removed per-volume terminal: %+v", countTrace)
	}
}

// TestMultiVolumeDeclineErrorNamesScope pins the actionable decline: it must
// keep wrapping the sentinel (callers match on it) while naming the volumes,
// the record scale, and how to narrow the query.
func TestMultiVolumeDeclineErrorNamesScope(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		newServiceVolumeIndex("c-decline-msg.gsi", singleFileCompactIndex("C:", "needle.txt")),
		newServiceVolumeIndex("f-decline-msg.gsi", singleFileCompactIndex("F:", "needle.txt")),
	}
	volumes[0].overlay.watermark.Store(1)
	volumes[0].snap.Store(nil)

	_, err := searchServiceVolumes(volumes, queryOptions{Query: "needle", Trace: &searchTrace{}}, false)
	if !errors.Is(err, errGlobalMultiVolumePlannerDeclined) {
		t.Fatalf("search error = %v, want decline sentinel", err)
	}
	for _, want := range []string{"C:", "F:", "--under", "records"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("decline error = %q, want it to mention %q", err.Error(), want)
		}
	}
	_, _, err = countServiceVolumes(volumes, queryOptions{Query: "needle", Trace: &searchTrace{}})
	if !errors.Is(err, errGlobalMultiVolumePlannerDeclined) {
		t.Fatalf("count error = %v, want decline sentinel", err)
	}
	if !strings.Contains(err.Error(), "--under") {
		t.Fatalf("count decline error = %q, want scoping hint", err.Error())
	}
}

func TestBareShortNameSubstringKeepsSubstringSemantics(t *testing.T) {
	idx := shortSubstringCompactIndex()
	got, err := searchCompactWithCache(idx, queryOptions{Query: "md", Limit: 10}, false, make(map[int]string), nil)
	if err != nil {
		t.Fatalf("search md: %v", err)
	}
	if names := namesOf(got); !sameStringSet(names, []string{"cmd.exe", "readme.md"}) {
		t.Fatalf("names = %v, want substring match not extension rewrite", names)
	}
}

func TestExplicitTwoCharExtUsesGlobalMerge(t *testing.T) {
	for _, query := range []string{"ext:md", "glob:*.md"} {
		t.Run(query, func(t *testing.T) {
			volumes := []*serviceVolumeIndex{
				newServiceVolumeIndex("c-explicit-md.gsi", singleDownloadFileCompactIndex("C:", "z-notes.md")),
				newServiceVolumeIndex("f-explicit-md.gsi", singleDownloadFileCompactIndex("F:", "a-notes.md")),
			}
			got, err := searchServiceVolumes(volumes, queryOptions{Query: query, Limit: 1}, false)
			if err != nil {
				t.Fatalf("searchServiceVolumes: %v", err)
			}
			if gotPaths := pathsOf(got); !sameOrderedStrings(gotPaths, []string{`F:\Downloads\a-notes.md`}) {
				t.Fatalf("paths = %v, want F volume global best explicit extension match", gotPaths)
			}
		})
	}
}

func singleFileCompactIndex(volume, name string) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  volume,
		Compact: true,
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) int32 {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      1024,
			ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
		return int32(len(idx.Records) - 1)
	}
	root := add(1, 1, -1, ".", uint32(os.ModeDir))
	workspace := add(2, 1, root, "workspace", uint32(os.ModeDir))
	add(3, 2, workspace, name, 0)
	buildOrders(idx)
	return idx
}

func singleDownloadFileCompactIndex(volume, name string) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  volume,
		Compact: true,
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) int32 {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      1024,
			ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
		return int32(len(idx.Records) - 1)
	}
	root := add(1, 1, -1, ".", uint32(os.ModeDir))
	downloads := add(2, 1, root, "Downloads", uint32(os.ModeDir))
	add(3, 2, downloads, name, 0)
	buildOrders(idx)
	return idx
}

func shortSubstringCompactIndex() *Index {
	idx := singleDownloadFileCompactIndex("C:", "readme.md")
	idx.Records = append(idx.Records, CompactRecord{
		FRN:       4,
		ParentFRN: 2,
		Parent:    1,
		Name:      "cmd.exe",
		Mode:      0,
		Size:      1024,
		ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
	})
	buildOrders(idx)
	return idx
}

func TestGlobalRecordIteratorNextSeekGE(t *testing.T) {
	it := newGlobalRecordIterator(2, []int{1, 4, 9, 15})
	var _ globalIDIterator = &it
	if got := it.CountHint(); got != 4 {
		t.Fatalf("initial CountHint = %d, want 4", got)
	}
	if got, ok := it.Next(); !ok || got != (globalRecordID{volume: 2, local: 1}) {
		t.Fatalf("Next = %+v/%v, want volume 2 local 1", got, ok)
	}
	if got := it.CountHint(); got != 3 {
		t.Fatalf("CountHint after Next = %d, want 3", got)
	}
	if got, ok := it.SeekGE(globalRecordID{volume: 2, local: 8}); !ok || got != (globalRecordID{volume: 2, local: 9}) {
		t.Fatalf("SeekGE local 8 = %+v/%v, want volume 2 local 9", got, ok)
	}
	if got, ok := it.SeekGE(globalRecordID{volume: 1, local: 99}); !ok || got != (globalRecordID{volume: 2, local: 15}) {
		t.Fatalf("SeekGE prior volume = %+v/%v, want volume 2 local 15", got, ok)
	}
	if got, ok := it.SeekGE(globalRecordID{volume: 3, local: 0}); ok {
		t.Fatalf("SeekGE later volume = %+v/%v, want exhausted", got, ok)
	}
	if got, ok := it.Next(); ok {
		t.Fatalf("Next after exhausted = %+v/%v, want exhausted", got, ok)
	}
	if got := it.CountHint(); got != 0 {
		t.Fatalf("CountHint after exhausted = %d, want 0", got)
	}
}

func TestGlobalPostingIteratorAdaptsPostingCandidate(t *testing.T) {
	it := newGlobalPostingIterator(3, postingCountCandidate{ids: []uint32{2, 5, 11}})
	if got := it.CountHint(); got != 3 {
		t.Fatalf("posting CountHint = %d, want 3", got)
	}
	if got, ok := it.SeekGE(globalRecordID{volume: 3, local: 4}); !ok || got != (globalRecordID{volume: 3, local: 5}) {
		t.Fatalf("posting SeekGE = %+v/%v, want volume 3 local 5", got, ok)
	}
	if got, ok := it.Next(); !ok || got != (globalRecordID{volume: 3, local: 11}) {
		t.Fatalf("posting Next = %+v/%v, want volume 3 local 11", got, ok)
	}
}

func TestGlobalExtPostingIDsMatchPerVolumePostings(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	got, ok := globalExtPostingIDs(volumes, "bin", 0, nil)
	if !ok {
		t.Fatal("globalExtPostingIDs declined ext source")
	}
	want := make([]globalRecordID, 0, 8)
	for _, id := range volumes[1].extPosting("bin") {
		want = append(want, globalRecordID{volume: 1, local: id})
	}
	if !slices.Equal(got, want) {
		t.Fatalf("global ext ids = %+v, want %+v", got, want)
	}
}

func TestGlobalExtPostingIDsRespectsLimit(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	got, ok := globalExtPostingIDs(volumes, "bin", 3, nil)
	if !ok {
		t.Fatal("globalExtPostingIDs declined ext source")
	}
	want := []globalRecordID{
		{volume: 1, local: volumes[1].extPosting("bin")[0]},
		{volume: 1, local: volumes[1].extPosting("bin")[1]},
		{volume: 1, local: volumes[1].extPosting("bin")[2]},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("limited global ext ids = %+v, want %+v", got, want)
	}
}

func TestGlobalExtPostingIDsTraceMissingPostingVolume(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	if volumes[0].queryIndex == nil {
		volumes[0].queryIndex = &residentQueryIndex{}
	}
	volumes[0].queryIndex.ext = nil
	trace := &searchTrace{}
	if _, ok := globalExtPostingIDs(volumes, "bin", 0, trace); ok {
		t.Fatal("globalExtPostingIDs unexpectedly handled missing ext source")
	}
	if trace.Decline != "global-ext:missing-posting" {
		t.Fatalf("decline = %q, want global-ext:missing-posting", trace.Decline)
	}
	if len(trace.Declines) != 1 || trace.Declines[0].Volume != "C:" || trace.Declines[0].Source != "global-ext" || trace.Declines[0].Reason != "missing-posting" {
		t.Fatalf("declines = %+v, want C: missing-posting", trace.Declines)
	}
}

func TestGlobalPlannerExtRankedPathTraceMissingPostingVolume(t *testing.T) {
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
	opts := queryOptions{Query: "ext:bin", Limit: 5, Trace: trace}
	if got, handled, err := searchServiceVolumesGlobalExtOnly(volumes, opts, false); err != nil {
		t.Fatal(err)
	} else if handled {
		t.Fatalf("global ext unexpectedly handled missing posting source with results=%v", got)
	}
	if trace.Decline != "global-ext:missing-posting" {
		t.Fatalf("decline = %q, want global-ext:missing-posting", trace.Decline)
	}
	if len(trace.Declines) != 1 || trace.Declines[0].Volume != "C:" || trace.Declines[0].Source != "global-ext" || trace.Declines[0].Reason != "missing-posting" {
		t.Fatalf("declines = %+v, want C: missing-posting", trace.Declines)
	}
}

func TestGlobalComponentRootIDsMatchPerVolumeRoots(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	got, ok := globalComponentRootIDs(volumes, "workspace-alpha", 0)
	if !ok {
		t.Fatal("globalComponentRootIDs declined component source")
	}
	want := make([]globalRecordID, 0, 9)
	for volumeIndex, vol := range volumes {
		for _, id := range vol.pathComponentRootIDs("workspace-alpha") {
			want = append(want, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("global component roots = %+v, want %+v", got, want)
	}
}

func TestGlobalComponentRootIDsRespectsLimit(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	got, ok := globalComponentRootIDs(volumes, "workspace-alpha", 2)
	if !ok {
		t.Fatal("globalComponentRootIDs declined component source")
	}
	want := []globalRecordID{
		{volume: 0, local: volumes[0].pathComponentRootIDs("workspace-alpha")[0]},
		{volume: 1, local: volumes[1].pathComponentRootIDs("workspace-alpha")[0]},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("limited global component roots = %+v, want %+v", got, want)
	}
}

func TestGlobalSubtreeIDsExpandsAndDedupesRoots(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	roots, ok := globalComponentRootIDs(volumes, "workspace-alpha", 0)
	if !ok {
		t.Fatal("globalComponentRootIDs declined component source")
	}
	roots = append(roots, roots[0])
	got, ok := globalSubtreeIDs(volumes, roots, 0)
	if !ok {
		t.Fatal("globalSubtreeIDs declined subtree source")
	}
	want := make([]globalRecordID, 0)
	seen := make(map[globalRecordID]struct{})
	for _, root := range roots {
		for _, id := range volumes[root.volume].underDescendants(root.local) {
			globalID := globalRecordID{volume: root.volume, local: id}
			if _, exists := seen[globalID]; exists {
				continue
			}
			seen[globalID] = struct{}{}
			want = append(want, globalID)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("global subtree ids = %+v, want %+v", got, want)
	}
}

func TestGlobalSubtreeIDsRespectsLimit(t *testing.T) {
	volumes := []*serviceVolumeIndex{
		workspaceAlphaModelVolume("C:", false),
		workspaceAlphaModelVolume("F:", true),
	}
	roots, ok := globalComponentRootIDs(volumes, "workspace-alpha", 0)
	if !ok {
		t.Fatal("globalComponentRootIDs declined component source")
	}
	got, ok := globalSubtreeIDs(volumes, roots, 3)
	if !ok {
		t.Fatal("globalSubtreeIDs declined subtree source")
	}
	if len(got) != 3 {
		t.Fatalf("limited global subtree ids len = %d, want 3; ids=%+v", len(got), got)
	}
}

func TestGlobalSubtreeLimitDoesNotMaterializeBroadRoot(t *testing.T) {
	vol := broadUnderTestVolume(2050)
	roots := vol.underRootIDs(`C:\broad`)
	if len(roots) != 1 {
		t.Fatalf("under roots = %v, want one broad root", roots)
	}
	got, ok := globalSubtreeIDs([]*serviceVolumeIndex{vol}, []globalRecordID{{volume: 0, local: roots[0]}}, 3)
	if !ok || len(got) != 3 {
		t.Fatalf("limited subtree = (%+v, %v), want three ids", got, ok)
	}
	if _, cached := vol.underCache[roots[0]]; cached {
		t.Fatal("limited global subtree materialized and cached the full broad root")
	}
}

func TestGlobalUnderIntersectionFiltersSelectivePostingFirst(t *testing.T) {
	vol := broadUnderTestVolume(2050)
	roots := vol.underRootIDs(`C:\broad`)
	if len(roots) != 1 {
		t.Fatalf("under roots = %v, want one broad root", roots)
	}
	pq := mustParseQuery(t, queryOptions{Query: "path:target.bin", MatchPath: true, Under: `C:\broad`, Limit: 10})
	ids, ok := globalComponentQueryIDs([]*serviceVolumeIndex{vol}, pq, &searchTrace{})
	if !ok || len(ids) != 1 || vol.index.compactRecord(ids[0].local).Name != "target.bin" {
		t.Fatalf("global under ids = (%+v, %v), want target.bin", ids, ok)
	}
	if _, cached := vol.underCache[roots[0]]; cached {
		t.Fatal("global under intersection materialized the broad subtree before filtering the selective posting")
	}
}

func TestGlobalComponentPathIDsMaterializesOnlySelectiveProbe(t *testing.T) {
	vol := broadUnderTestVolume(2050)
	ids, ok := globalComponentPathIDs([]*serviceVolumeIndex{vol}, []string{"broad", "target.bin"})
	if !ok || len(ids) != 1 || vol.index.compactRecord(ids[0].local).Name != "target.bin" {
		t.Fatalf("component ids = (%+v, %v), want target.bin", ids, ok)
	}
	if _, cached := vol.pathTermCache["broad"]; cached {
		t.Fatal("multi-term global component query materialized the broad path posting before intersection")
	}
	if _, cached := vol.underCache[1]; cached {
		t.Fatal("multi-term global component query materialized the broad directory subtree")
	}
}

func broadUnderTestVolume(children int) *serviceVolumeIndex {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true}
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 2, ParentFRN: 1, Parent: 0, Name: "broad", Mode: uint32(os.ModeDir)},
	)
	for i := 0; i < children; i++ {
		name := fmt.Sprintf("noise-%04d.dat", i)
		if i == children-1 {
			name = "target.bin"
		}
		idx.Records = append(idx.Records, CompactRecord{FRN: uint64(i + 3), ParentFRN: 2, Parent: 1, Name: name})
	}
	buildOrders(idx)
	return newServiceVolumeIndex("broad-under.gsi", idx)
}

func TestGlobalIteratorSetOperations(t *testing.T) {
	left := newGlobalRecordIterator(0, []int{1, 3, 5, 9})
	right := newGlobalRecordIterator(0, []int{3, 4, 5, 10})
	if got, want := intersectGlobalIterators(&left, &right, 0), []globalRecordID{
		{volume: 0, local: 3},
		{volume: 0, local: 5},
	}; !slices.Equal(got, want) {
		t.Fatalf("intersect = %+v, want %+v", got, want)
	}

	left = newGlobalRecordIterator(0, []int{1, 3, 5})
	right = newGlobalRecordIterator(0, []int{3, 4, 5})
	if got, want := unionGlobalIterators(&left, &right, 0), []globalRecordID{
		{volume: 0, local: 1},
		{volume: 0, local: 3},
		{volume: 0, local: 4},
		{volume: 0, local: 5},
	}; !slices.Equal(got, want) {
		t.Fatalf("union = %+v, want %+v", got, want)
	}

	include := newGlobalRecordIterator(1, []int{1, 2, 3, 4})
	exclude := newGlobalRecordIterator(1, []int{2, 4})
	if got, want := excludeGlobalIterator(&include, &exclude, 0), []globalRecordID{
		{volume: 1, local: 1},
		{volume: 1, local: 3},
	}; !slices.Equal(got, want) {
		t.Fatalf("exclude = %+v, want %+v", got, want)
	}
}

func TestGlobalIteratorSetOperationsRespectVolumeAndLimit(t *testing.T) {
	left := newGlobalRecordIterator(0, []int{1, 2, 3})
	right := newGlobalRecordIterator(1, []int{1, 2, 3})
	if got := intersectGlobalIterators(&left, &right, 0); len(got) != 0 {
		t.Fatalf("cross-volume intersect = %+v, want empty", got)
	}

	left = newGlobalRecordIterator(0, []int{1, 2, 3})
	right = newGlobalRecordIterator(1, []int{1, 2, 3})
	if got, want := unionGlobalIterators(&left, &right, 4), []globalRecordID{
		{volume: 0, local: 1},
		{volume: 0, local: 2},
		{volume: 0, local: 3},
		{volume: 1, local: 1},
	}; !slices.Equal(got, want) {
		t.Fatalf("limited cross-volume union = %+v, want %+v", got, want)
	}
}

func TestCollectGlobalTopNOrdersByRankThenGlobalID(t *testing.T) {
	left := newGlobalRecordIterator(0, []int{1, 2, 3})
	right := newGlobalRecordIterator(1, []int{1, 2, 3})
	ranks := map[globalRecordID]int{
		{volume: 0, local: 1}: 50,
		{volume: 0, local: 2}: 10,
		{volume: 0, local: 3}: 20,
		{volume: 1, local: 1}: 10,
		{volume: 1, local: 2}: 40,
		{volume: 1, local: 3}: 20,
	}
	got := collectGlobalTopN([]globalIDIterator{&left, &right}, 4, func(id globalRecordID) int {
		return ranks[id]
	})
	want := []globalRecordID{
		{volume: 0, local: 2},
		{volume: 1, local: 1},
		{volume: 0, local: 3},
		{volume: 1, local: 3},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("global top-n = %+v, want %+v", got, want)
	}
}

func TestCollectGlobalTopNHandlesEmptyInputs(t *testing.T) {
	got := collectGlobalTopN(nil, 10, func(id globalRecordID) int { return 0 })
	if len(got) != 0 {
		t.Fatalf("nil iterator top-n = %+v, want empty", got)
	}
	it := newGlobalRecordIterator(0, []int{1})
	got = collectGlobalTopN([]globalIDIterator{&it}, 0, func(id globalRecordID) int { return 0 })
	if len(got) != 0 {
		t.Fatalf("zero limit top-n = %+v, want empty", got)
	}
}

func TestSortIDsByRankOrdersByRankThenID(t *testing.T) {
	ids := []int{9, 2, 8, 5, 3, 1}
	ranks := map[int]int{
		1: 10,
		2: 20,
		3: 10,
		5: 30,
		8: 20,
		9: 10,
	}
	sortIDsByRank(ids, func(id int) int {
		return ranks[id]
	})
	want := []int{1, 3, 9, 2, 8, 5}
	if !slices.Equal(ids, want) {
		t.Fatalf("sorted ids = %v, want %v", ids, want)
	}
}
