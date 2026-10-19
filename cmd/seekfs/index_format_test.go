package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPostingRankBoundsRoundTripIncludesDefaultNameOrder(t *testing.T) {
	want := postingRankBounds{
		BlockCount: 2,
		Name:       []uint32{1, 2},
		Size:       []uint32{3, 4},
		Modified:   []uint32{5, 6},
		Extension:  []uint32{7, 8},
		Type:       []uint32{9, 10},
		Path:       []uint32{11, 12},
	}
	got := decodePostingRankBounds(encodePostingRankBounds(want))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-tripped rank bounds = %+v, want %+v", got, want)
	}
	if ranks := got.ranksForSort(""); !slices.Equal(ranks, want.Name) {
		t.Fatalf("default rank bounds = %v, want %v", ranks, want.Name)
	}
}

func TestCompactIndexRoundTripKeepsFRNMetadata(t *testing.T) {
	builtAt := time.Unix(0, 123456789)
	modified := time.Unix(0, 987654321)
	idx := &Index{
		Version:      indexVersion,
		Roots:        []string{`C:\`},
		BuiltAt:      builtAt,
		Source:       "usn",
		Volume:       "C:",
		JournalID:    42,
		Checkpoint:   99,
		Compact:      true,
		CompactAttrs: true,
		Records: []CompactRecord{
			{
				FRN:       10,
				ParentFRN: 10,
				Parent:    -1,
				Name:      ".",
				Mode:      uint32(1 << 31),
				Size:      0,
				ModUnix:   modified.UnixNano(),
			},
			{
				FRN:       11,
				ParentFRN: 10,
				Parent:    0,
				Name:      "main.go",
				Mode:      modeFromAttrs(fileAttributeHidden | fileAttributeArchive),
				Size:      1234,
				ModUnix:   modified.Add(time.Second).UnixNano(),
				Deleted:   true,
			},
		},
	}

	dir := t.TempDir()
	db := filepath.Join(dir, "test.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}
	got, err := loadIndex(db)
	if err != nil {
		t.Fatalf("loadIndex: %v", err)
	}

	if got.Version != indexVersion {
		t.Fatalf("Version = %d, want %d", got.Version, indexVersion)
	}
	if got.Source != "usn" || got.Volume != "C:" || got.JournalID != 42 || got.Checkpoint != 99 {
		t.Fatalf("index metadata was not preserved: %+v", got)
	}
	if !got.Compact || len(got.Records) != 2 {
		t.Fatalf("records = %d compact=%v, want 2 compact records", len(got.Records), got.Compact)
	}
	if !got.CompactAttrs {
		t.Fatal("compact attr capability was not preserved")
	}

	root := got.Records[0]
	if root.FRN != 10 || root.ParentFRN != 10 || root.Parent != -1 || root.Mode != uint32(1<<31) || root.ModUnix != modified.UnixNano() {
		t.Fatalf("root record metadata mismatch: %+v", root)
	}
	file := got.Records[1]
	if file.FRN != 11 || file.ParentFRN != 10 || file.Parent != 0 || file.Name != "main.go" || file.Size != 1234 || file.ModUnix != modified.Add(time.Second).UnixNano() || !file.Deleted || file.Mode&fileAttributeHidden == 0 || file.Mode&fileAttributeArchive == 0 {
		t.Fatalf("file record metadata mismatch: %+v", file)
	}
	if got.reconstructCompactPath(1) != `C:\main.go` {
		t.Fatalf("path = %q", got.reconstructCompactPath(1))
	}
}

func TestCompactIndexMMapRoundTripKeepsPathAndMetadata(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	modified := time.Unix(0, 987654321)
	idx := &Index{
		Version:      indexVersion,
		Roots:        []string{`F:\`},
		BuiltAt:      time.Unix(0, 123456789),
		Source:       "usn",
		Volume:       "F:",
		JournalID:    42,
		Checkpoint:   99,
		Compact:      true,
		CompactAttrs: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), ModUnix: modified.UnixNano()},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "Projects", Mode: uint32(os.ModeDir), ModUnix: modified.Add(time.Second).UnixNano()},
			{FRN: 12, ParentFRN: 11, Parent: 1, Name: "Scan.NRRD", Mode: modeFromAttrs(fileAttributeHidden | fileAttributeArchive), Size: 4096, ModUnix: modified.Add(2 * time.Second).UnixNano()},
		},
	}
	path := filepath.Join(t.TempDir(), "compact.gsi")
	if err := saveIndex(path, idx); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}
	got, err := loadIndexMMap(path)
	if err != nil {
		t.Fatalf("loadIndexMMap: %v", err)
	}
	t.Cleanup(func() {
		if got.MMapRecords != nil {
			_ = got.MMapRecords.file.close()
		}
	})
	if got.MMapRecords == nil || got.PackedRecords != nil || len(got.Records) != 0 {
		t.Fatalf("mmap records not active: mmap=%v packed=%v records=%d", got.MMapRecords != nil, got.PackedRecords != nil, len(got.Records))
	}
	if got.compactRecordCount() != 3 || !got.compactHasSize() || !got.compactHasModTime() || !got.compactHasAttrs() {
		t.Fatalf("mmap capabilities count=%d size=%v mod=%v attr=%v", got.compactRecordCount(), got.compactHasSize(), got.compactHasModTime(), got.compactHasAttrs())
	}
	rec := got.compactRecord(2)
	if rec.Name != "Scan.NRRD" || rec.Size != 4096 || rec.Parent != 1 || rec.ParentFRN != 11 || rec.Mode&fileAttributeHidden == 0 || rec.Mode&fileAttributeArchive == 0 {
		t.Fatalf("mmap record mismatch: %+v", rec)
	}
	if lower := got.compactLowerNameAt(2); lower != "scan.nrrd" {
		t.Fatalf("lower name = %q", lower)
	}
	if path := got.reconstructCompactPath(2); path != `F:\Projects\Scan.NRRD` {
		t.Fatalf("path = %q", path)
	}
}

func TestCompactDiskRecordRefsUseWideEncodingPastNarrowLimit(t *testing.T) {
	var buf bytes.Buffer
	parent := uint32(compactNarrowParentSentinel)
	nameOff := uint32(compactNarrowParentSentinel + 7)
	if err := writeCompactRecordRefs(&buf, parent, nameOff, true); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 8 {
		t.Fatalf("wide ref bytes = %d, want 8", buf.Len())
	}
	gotParent, gotNameOff, err := readCompactRecordRefs(bytes.NewReader(buf.Bytes()), true)
	if err != nil {
		t.Fatal(err)
	}
	if gotParent != parent || gotNameOff != nameOff {
		t.Fatalf("refs = (%d, %d), want (%d, %d)", gotParent, gotNameOff, parent, nameOff)
	}
}

func TestCompactDiskRecordRefsRejectNarrowOverflow(t *testing.T) {
	var buf bytes.Buffer
	if err := writeCompactRecordRefs(&buf, compactNarrowParentSentinel+1, 1, false); err == nil {
		t.Fatal("expected narrow parent overflow error")
	}
	if err := writeCompactRecordRefs(&buf, 1, compactNarrowParentSentinel, false); err == nil {
		t.Fatal("expected narrow name ref overflow error")
	}
}

func TestCompactDiskRecordBytesSwitchesToWideAtNarrowLimit(t *testing.T) {
	narrowMaxCount := int(compactNarrowMaxRecordRef) + 1
	if got := compactDiskRecordBytesForCounts(narrowMaxCount, 1); got != compactDiskRecordBytes {
		t.Fatalf("narrow record bytes = %d, want %d", got, compactDiskRecordBytes)
	}
	if got := compactDiskRecordBytesForCounts(narrowMaxCount+1, 1); got != compactWideDiskRecordBytes {
		t.Fatalf("wide-by-record-count bytes = %d, want %d", got, compactWideDiskRecordBytes)
	}
	if got := compactDiskRecordBytesForCounts(1, narrowMaxCount+1); got != compactWideDiskRecordBytes {
		t.Fatalf("wide-by-name-count bytes = %d, want %d", got, compactWideDiskRecordBytes)
	}
}

func TestEngineWritesAndLoadsDerivedSections(t *testing.T) {
	idx := &Index{
		Version:      indexVersion,
		Roots:        []string{`C:\`},
		BuiltAt:      time.Unix(0, 123),
		Source:       "usn",
		Volume:       "C:",
		Compact:      true,
		CompactAttrs: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: modeFromAttrs(fileAttributeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "docs", Mode: modeFromAttrs(fileAttributeDir)},
			{FRN: 12, ParentFRN: 11, Parent: 1, Name: "Alpha.TXT", Mode: modeFromAttrs(fileAttributeHidden | fileAttributeArchive)},
			{FRN: 13, ParentFRN: 11, Parent: 1, Name: "Résumé.GO"},
			{FRN: 14, ParentFRN: 11, Parent: 1, Name: "Alpha.TXT"},
			{FRN: 15, ParentFRN: 11, Parent: 1, Name: "İ.TXT"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 mmap: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	if loaded.Version != indexVersion {
		t.Fatalf("version = %d, want %d", loaded.Version, indexVersion)
	}
	if !loaded.compactHasAttrs() {
		t.Fatal("v9 compact attr capability was not preserved")
	}
	if len(loaded.Derived.NameOrder) != loaded.compactRecordCount() || len(loaded.Derived.NameRank) != loaded.compactRecordCount() {
		t.Fatalf("rank section missing: %+v", loaded.Derived)
	}
	if len(loaded.Derived.ChildOffsets) == 0 || len(loaded.Derived.ChildIDs) == 0 || len(loaded.Derived.SubtreeOrder) == 0 {
		t.Fatalf("child/subtree sections missing: %+v", loaded.Derived)
	}
	if len(loaded.Derived.FRNs) != loaded.compactRecordCount() || len(loaded.Derived.FRNRecordIDs) != loaded.compactRecordCount() {
		t.Fatalf("frn section missing: %+v", loaded.Derived)
	}
	tokenCount := len(loaded.MMapRecords.tokenTable) / 6
	if len(loaded.Derived.LowerOffs) != tokenCount || len(loaded.Derived.LowerLens) != tokenCount || len(loaded.Derived.LowerBlob) == 0 {
		t.Fatalf("lowercase section missing: %+v", loaded.Derived)
	}
	if got, want := loaded.compactLowerNameAt(2), "alpha.txt"; got != want {
		t.Fatalf("mapped lower name = %q, want %q", got, want)
	}
	if got, want := loaded.compactLowerNameAt(3), "résumé.go"; got != want {
		t.Fatalf("mapped unicode lower name = %q, want %q", got, want)
	}
	if got, want := loaded.compactLowerNameAt(4), "alpha.txt"; got != want {
		t.Fatalf("mapped duplicate lower name = %q, want %q", got, want)
	}
	if got, want := loaded.compactLowerNameAt(5), strings.ToLower("İ.TXT"); got != want {
		t.Fatalf("mapped unicode variable lower name = %q, want %q", got, want)
	}
	sections, _ := derivedSectionInfo(loaded.Derived)
	for _, want := range []string{"LOWR", "PATR", "PEXT", "PXRB", "PXRC", "PCMP", "PNGR"} {
		if !slices.Contains(sections, want) {
			t.Fatalf("sections = %v, missing %s", sections, want)
		}
	}
	vol := newServiceVolumeIndex(db, loaded)
	if vol.queryIndex == nil || len(vol.queryIndex.nameOrder) != loaded.compactRecordCount() {
		t.Fatalf("service did not wire mapped rank section")
	}
	if len(vol.queryIndex.ext) != 0 {
		t.Fatalf("mapped ext postings were eagerly materialized: %v", vol.queryIndex.ext)
	}
	if len(vol.queryIndex.components) != 0 {
		t.Fatalf("mapped component postings were eagerly materialized: %v", vol.queryIndex.components)
	}
	if vol.nameTrigramStateString() != "ready" || vol.nameTrigramIndex() == nil {
		t.Fatalf("mapped name trigram section was not marked ready")
	}
	if vol.nameTrigramIndex().mappedGrams == nil || len(vol.nameTrigramIndex().segments) != 0 {
		t.Fatalf("mapped name trigram section was materialized unexpectedly")
	}
	if ids, ok := vol.nameTrigramIndex().candidateIDs("alpha"); !ok || len(ids) == 0 {
		t.Fatalf("mapped name trigram lookup ok=%v ids=%v", ok, ids)
	}
	if got := vol.extPosting("txt"); !reflect.DeepEqual(got, []int{2, 4, 5}) {
		t.Fatalf("raw mapped ext lookup = %v", got)
	}
	if got := vol.pathComponentRootIDs("docs"); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("raw mapped component lookup = %v", got)
	}
	if len(vol.childOffsets) == 0 || len(vol.subtreeOrder) == 0 || len(vol.frns) == 0 {
		t.Fatalf("service did not wire mapped derived sections")
	}
}

func TestEngineLowmemMappedStartupSkipsResidentPathRebuilds(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 789),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "docs", Mode: uint32(os.ModeDir)},
			{FRN: 12, ParentFRN: 11, Parent: 1, Name: "Alpha.TXT"},
			{FRN: 13, ParentFRN: 11, Parent: 1, Name: "Beta.GO"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9-lowmem.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("load for service: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	vol := newServiceVolumeIndex(db, loaded)
	if vol.queryIndex == nil {
		t.Fatal("queryIndex was not initialized")
	}
	if vol.queryIndex.ext != nil || vol.queryIndex.components != nil || vol.queryIndex.pathGrams != nil || vol.queryIndex.dirsReady {
		t.Fatalf("mapped lowmem materialized resident postings: ext=%v components=%v pathGrams=%v dirsReady=%v",
			vol.queryIndex.ext != nil, vol.queryIndex.components != nil, vol.queryIndex.pathGrams != nil, vol.queryIndex.dirsReady)
	}
	if len(vol.queryIndex.nameOrder) != loaded.compactRecordCount() || len(vol.queryIndex.nameRank) != loaded.compactRecordCount() {
		t.Fatalf("mapped rank not wired: order=%d rank=%d records=%d", len(vol.queryIndex.nameOrder), len(vol.queryIndex.nameRank), loaded.compactRecordCount())
	}
	if got := vol.nameTrigramStateString(); got != "ready" {
		t.Fatalf("mapped trigram state = %q, want ready", got)
	}
	servicePostingBlockCache = postingBlockLRU{}
	t.Cleanup(func() { servicePostingBlockCache = postingBlockLRU{} })
	t.Setenv("SEEKFS_POSTING_CACHE_MB", "1")
	extCount, extOK, extErr := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "ext:txt"})
	if extErr != nil || !extOK || extCount != 1 {
		t.Fatalf("mapped ext count = %d, handled=%v, err=%v; want 1, true, nil", extCount, extOK, extErr)
	}
	servicePostingBlockCache.mu.Lock()
	cachedBlocks := len(servicePostingBlockCache.items)
	servicePostingBlockCache.mu.Unlock()
	if cachedBlocks != 0 {
		t.Fatalf("mapped ext count decoded %d posting blocks, want metadata-only count", cachedBlocks)
	}
	lazyPQ := mustParseQuery(t, queryOptions{Query: "ext:txt dir:docs alpha", MatchPath: true, Limit: 10})
	lazyPQ.Limit = normalizedLimit(10, false)
	plan, ok := vol.buildCandidatePlan(lazyPQ)
	if !ok {
		t.Fatal("buildCandidatePlan declined mapped ext+dir query")
	}
	if got := plan.sourceSummary(); got != "dir:docs+ext:txt" && got != "dir:docs+ext:txt+path-term:alpha" {
		t.Fatalf("source summary = %q, want %q or the bounded term route %q", got, "dir:docs+ext:txt", "dir:docs+ext:txt+path-term:alpha")
	}
	if vol.queryIndex.ext != nil || vol.queryIndex.components != nil || vol.queryIndex.pathGrams != nil || vol.queryIndex.dirsReady {
		t.Fatalf("mapped lowmem materialized resident postings after buildCandidatePlan: ext=%v components=%v pathGrams=%v dirsReady=%v",
			vol.queryIndex.ext != nil, vol.queryIndex.components != nil, vol.queryIndex.pathGrams != nil, vol.queryIndex.dirsReady)
	}
	for _, tc := range []struct {
		name       string
		opts       queryOptions
		want       []string
		wantSource string
	}{
		{
			name: "type-dir-falls-to-bounded-scan",
			opts: queryOptions{Query: "type:dir", Limit: 10},
			want: []string{`C:`, `C:\docs`},
		},
		{
			name: "exact-dir-uses-mapped-component",
			opts: queryOptions{Query: "dir:docs alpha", MatchPath: true, Limit: 10},
			want: []string{`C:\docs\Alpha.TXT`},
		},
		{
			name: "substring-dir-declines-to-bounded-scan",
			opts: queryOptions{Query: "dir:doc alpha", MatchPath: true, Limit: 10},
			want: []string{`C:\docs\Alpha.TXT`},
		},
		{
			name:       "mapped-ext-top-uses-posting-blocks",
			opts:       queryOptions{Query: "ext:txt", Limit: 1},
			want:       []string{`C:\docs\Alpha.TXT`},
			wantSource: "planned:ext-top",
		},
		{
			name:       "mapped-ext-dir-plan-stays-lazy",
			opts:       queryOptions{Query: "ext:txt dir:docs alpha", MatchPath: true, Limit: 10},
			want:       []string{`C:\docs\Alpha.TXT`},
			wantSource: "planned:dir:docs+ext:txt+path-term:alpha",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := &searchTrace{}
			tc.opts.Trace = trace
			got, err := searchCompactWithCache(loaded, tc.opts, false, make(map[int]string), vol.nameTermCandidates)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(pathsOf(got), tc.want) {
				t.Fatalf("paths = %v, want %v (source=%s decline=%s candidates=%d)", pathsOf(got), tc.want, trace.Source, trace.Decline, trace.Candidates)
			}
			if trace.Source == "" {
				t.Fatal("query did not report a trace source")
			}
			if tc.wantSource != "" && trace.Source != tc.wantSource {
				t.Fatalf("trace source = %q, want %q", trace.Source, tc.wantSource)
			}
		})
	}
	if vol.queryIndex.ext != nil || vol.queryIndex.components != nil || vol.queryIndex.pathGrams != nil || vol.queryIndex.dirsReady {
		t.Fatalf("mapped lowmem materialized resident postings after queries: ext=%v components=%v pathGrams=%v dirsReady=%v",
			vol.queryIndex.ext != nil, vol.queryIndex.components != nil, vol.queryIndex.pathGrams != nil, vol.queryIndex.dirsReady)
	}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "ext:txt dir:docs alpha", MatchPath: true})
	if err != nil {
		t.Fatalf("count mapped ext+dir query: %v", err)
	}
	if !ok || count != 1 {
		t.Fatalf("count mapped ext+dir query = (%d, %v), want (1, true)", count, ok)
	}
}

func TestReadIndexLoadsDerivedSectionsWithoutMMap(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "")
	idx := &Index{
		Version:      indexVersion,
		Roots:        []string{`F:\`},
		BuiltAt:      time.Unix(0, 456),
		Source:       "usn",
		Volume:       "F:",
		Compact:      true,
		CompactAttrs: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "Projects", Mode: uint32(os.ModeDir)},
			{FRN: 12, ParentFRN: 11, Parent: 1, Name: "Alpha.TXT", Mode: modeFromAttrs(fileAttributeHidden | fileAttributeArchive), Size: 100, ModUnix: 10},
			{FRN: 13, ParentFRN: 11, Parent: 1, Name: "Beta.GO", Mode: modeFromAttrs(fileAttributeArchive), Size: 200, ModUnix: 20},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9-resident.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndex(db)
	if err != nil {
		t.Fatalf("loadIndex: %v", err)
	}
	if loaded.MMapRecords != nil {
		t.Fatal("resident load unexpectedly used mmap records")
	}
	sections, _ := derivedSectionInfo(loaded.Derived)
	for _, want := range []string{"RANK", "ERNK", "TRNK", "PRNK", "CHLD", "SUBT", "FRNS", "LOWR", "PATR", "PEXT", "PXRB", "PXRC", "PCMP", "PNGR"} {
		if !slices.Contains(sections, want) {
			t.Fatalf("sections = %v, missing %s", sections, want)
		}
	}
	vol := newServiceVolumeIndex(db, loaded)
	if got := vol.nameOrderStateString(); got != "ready" {
		t.Fatalf("resident v9 name order state = %q, want ready", got)
	}
	if got := vol.nameTrigramStateString(); got != "ready" {
		t.Fatalf("resident v9 trigram state = %q, want ready", got)
	}
	if vol.nameTrigramIndex() == nil {
		t.Fatal("resident v9 trigram section was not wired")
	}
	if vol.queryIndex == nil || len(vol.queryIndex.nameOrder) != loaded.compactRecordCount() || len(vol.queryIndex.extOrder) != loaded.compactRecordCount() {
		t.Fatalf("resident v9 ranks not wired: queryIndex=%v nameOrder=%d extOrder=%d records=%d",
			vol.queryIndex != nil, len(vol.queryIndex.nameOrder), len(vol.queryIndex.extOrder), loaded.compactRecordCount())
	}
	if len(vol.queryIndex.ext) != 0 {
		t.Fatalf("resident v9 ext postings were rebuilt instead of using derived PEXT: %v", vol.queryIndex.ext)
	}
	if len(vol.queryIndex.components) != 0 {
		t.Fatalf("resident v9 component postings were rebuilt instead of using derived PCMP: %v", vol.queryIndex.components)
	}
	if got := vol.extPosting("txt"); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("resident v9 derived ext lookup = %v, want [2]", got)
	}
	if got := vol.pathComponentRootIDs("projects"); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("resident v9 derived component lookup = %v, want [1]", got)
	}
	if ids, ok := vol.nameTrigramIndex().candidateIDs("alpha"); !ok || len(ids) == 0 {
		t.Fatalf("resident v9 trigram lookup ok=%v ids=%v", ok, ids)
	}
}

func TestEngineLowmemMappedBoundedScanUsesMappedRankOrder(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 790),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "zeta", Mode: uint32(os.ModeDir)},
			{FRN: 12, ParentFRN: 10, Parent: 0, Name: "alpha", Mode: uint32(os.ModeDir)},
			{FRN: 13, ParentFRN: 12, Parent: 2, Name: "note.txt"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9-lowmem-order.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("load for service: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	vol := newServiceVolumeIndex(db, loaded)
	if vol.queryIndex == nil {
		t.Fatal("queryIndex was not initialized")
	}
	if len(vol.queryIndex.nameOrder) != loaded.compactRecordCount() {
		t.Fatalf("mapped rank not wired: order=%d records=%d", len(vol.queryIndex.nameOrder), loaded.compactRecordCount())
	}
	if vol.queryIndex.dirsReady {
		t.Fatal("mapped lowmem unexpectedly built resident directory postings")
	}

	opts := queryOptions{Query: "type:dir", Limit: 2, Trace: &searchTrace{}}
	got, err := searchCompactWithCache(loaded, opts, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`C:`, `C:\alpha`}; !reflect.DeepEqual(pathsOf(got), want) {
		t.Fatalf("paths = %v, want %v (source=%s candidates=%d)", pathsOf(got), want, opts.Trace.Source, opts.Trace.Candidates)
	}
	if opts.Trace.Source != "bounded-scan" {
		t.Fatalf("trace source = %q, want bounded-scan", opts.Trace.Source)
	}
}

func TestEngineMappedExtTopTraceReportsSkippedBlocks(t *testing.T) {
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 791),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		},
	}
	for i := 0; i < 1100; i++ {
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       uint64(11 + i),
			ParentFRN: 10,
			Parent:    0,
			Name:      fmt.Sprintf("file-%04d.txt", i),
			Size:      int64(1100 - i),
			ModUnix:   int64(i + 1),
		})
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9-lowmem-ext-blocks.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("load for service: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	vol := newServiceVolumeIndex(db, loaded)
	extPosting := loaded.Derived.Postings[indexSectionPEXT]
	if extPosting.RankBounds.BlockCount < 2 {
		t.Fatalf("PEXT rank bounds block count = %d, want at least 2", extPosting.RankBounds.BlockCount)
	}
	trace := &searchTrace{}
	opts := queryOptions{Query: "ext:txt", Limit: 1, Trace: trace}
	got, err := searchCompactWithCache(loaded, opts, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(got); !reflect.DeepEqual(paths, []string{`C:\file-0000.txt`}) {
		t.Fatalf("paths = %v, want first txt file", paths)
	}
	if trace.Source != "planned:ext-top" {
		t.Fatalf("trace source = %q, want planned:ext-top", trace.Source)
	}
	if trace.BlocksDecoded != 1 || trace.BlocksSkipped == 0 {
		t.Fatalf("blocks decoded/skipped = %d/%d, want 1/>0", trace.BlocksDecoded, trace.BlocksSkipped)
	}
	if !traceHasTerm(trace.Terms, traceTerm{Term: "txt", Kind: "extension", Source: "planned:ext-top", Exact: true}) {
		t.Fatalf("trace terms = %+v, missing planned ext-top term", trace.Terms)
	}

	globalTrace := &searchTrace{}
	global, handled, err := searchServiceVolumesGlobalExtOnly([]*serviceVolumeIndex{vol, vol}, queryOptions{Query: "ext:txt", Limit: 1, Trace: globalTrace}, false)
	if err != nil || !handled {
		t.Fatalf("global mapped ext top handled=%v err=%v", handled, err)
	}
	if paths := pathsOf(global); !reflect.DeepEqual(paths, []string{`C:\file-0000.txt`}) {
		t.Fatalf("global mapped ext paths = %v, want first txt file", paths)
	}
	if globalTrace.PlannerMode != "global-ext" || globalTrace.BlocksDecoded != 2 || globalTrace.BlocksSkipped == 0 {
		t.Fatalf("global planner/blocks = %q %d/%d, want global-ext 2/>0", globalTrace.PlannerMode, globalTrace.BlocksDecoded, globalTrace.BlocksSkipped)
	}

	sizeTrace := &searchTrace{}
	sizeOpts := queryOptions{Query: "ext:txt sort:size", Limit: 1, Trace: sizeTrace}
	sizeSorted, err := searchCompactWithCache(loaded, sizeOpts, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(sizeSorted); !reflect.DeepEqual(paths, []string{`C:\file-1099.txt`}) {
		t.Fatalf("sort:size paths = %v, want smallest txt file", paths)
	}
	if sizeTrace.Source != "planned:ext-top" {
		t.Fatalf("sort:size trace source = %q, want planned:ext-top", sizeTrace.Source)
	}
	if sizeTrace.BlocksDecoded != 1 || sizeTrace.BlocksSkipped == 0 {
		t.Fatalf("sort:size blocks decoded/skipped = %d/%d, want 1/>0 with size block bounds", sizeTrace.BlocksDecoded, sizeTrace.BlocksSkipped)
	}
	if !traceHasTerm(sizeTrace.Terms, traceTerm{Term: "txt", Kind: "extension", Source: "planned:ext-top", Exact: true}) {
		t.Fatalf("sort:size trace terms = %+v, missing planned ext-top term", sizeTrace.Terms)
	}

	modTrace := &searchTrace{}
	modOpts := queryOptions{Query: "ext:txt sort:modified", Limit: 1, Trace: modTrace}
	modSorted, err := searchCompactWithCache(loaded, modOpts, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(modSorted); !reflect.DeepEqual(paths, []string{`C:\file-1099.txt`}) {
		t.Fatalf("sort:modified paths = %v, want newest txt file", paths)
	}
	if modTrace.Source != "planned:ext-top" {
		t.Fatalf("sort:modified trace source = %q, want planned:ext-top", modTrace.Source)
	}
	if modTrace.BlocksDecoded != 1 || modTrace.BlocksSkipped == 0 {
		t.Fatalf("sort:modified blocks decoded/skipped = %d/%d, want 1/>0 with modified block bounds", modTrace.BlocksDecoded, modTrace.BlocksSkipped)
	}
	if !traceHasTerm(modTrace.Terms, traceTerm{Term: "txt", Kind: "extension", Source: "planned:ext-top", Exact: true}) {
		t.Fatalf("sort:modified trace terms = %+v, missing planned ext-top term", modTrace.Terms)
	}

	extTrace := &searchTrace{}
	extOpts := queryOptions{Query: "ext:txt sort:extension", Limit: 1, Trace: extTrace}
	extSorted, err := searchCompactWithCache(loaded, extOpts, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(extSorted); !reflect.DeepEqual(paths, []string{`C:\file-0000.txt`}) {
		t.Fatalf("sort:extension paths = %v, want first txt name", paths)
	}
	if extTrace.Source != "planned:ext-top" {
		t.Fatalf("sort:extension trace source = %q, want planned:ext-top", extTrace.Source)
	}
	if extTrace.BlocksDecoded != 1 || extTrace.BlocksSkipped == 0 {
		t.Fatalf("sort:extension blocks decoded/skipped = %d/%d, want 1/>0 with extension block bounds", extTrace.BlocksDecoded, extTrace.BlocksSkipped)
	}

	typeTrace := &searchTrace{}
	typeOpts := queryOptions{Query: "ext:txt sort:type", Limit: 1, Trace: typeTrace}
	typeSorted, err := searchCompactWithCache(loaded, typeOpts, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(typeSorted); !reflect.DeepEqual(paths, []string{`C:\file-0000.txt`}) {
		t.Fatalf("sort:type paths = %v, want first txt name", paths)
	}
	if typeTrace.Source != "planned:ext-top" {
		t.Fatalf("sort:type trace source = %q, want planned:ext-top", typeTrace.Source)
	}
	if typeTrace.BlocksDecoded != 1 || typeTrace.BlocksSkipped == 0 {
		t.Fatalf("sort:type blocks decoded/skipped = %d/%d, want 1/>0 with type block bounds", typeTrace.BlocksDecoded, typeTrace.BlocksSkipped)
	}

	pathTrace := &searchTrace{}
	pathOpts := queryOptions{Query: "ext:txt sort:path", Limit: 1, Trace: pathTrace}
	pathSorted, err := searchCompactWithCache(loaded, pathOpts, false, make(map[int]string), vol.nameTermCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if paths := pathsOf(pathSorted); !reflect.DeepEqual(paths, []string{`C:\file-0000.txt`}) {
		t.Fatalf("sort:path paths = %v, want first txt path", paths)
	}
	if pathTrace.Source != "planned:ext-top" {
		t.Fatalf("sort:path trace source = %q, want planned:ext-top", pathTrace.Source)
	}
	if pathTrace.BlocksDecoded != 1 || pathTrace.BlocksSkipped == 0 {
		t.Fatalf("sort:path blocks decoded/skipped = %d/%d, want 1/>0 with path block bounds", pathTrace.BlocksDecoded, pathTrace.BlocksSkipped)
	}
}

func TestEngineWritesAndLoadsSizeRankSection(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 124),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "large.txt", Size: 300},
			{FRN: 12, ParentFRN: 10, Parent: 0, Name: "small.txt", Size: 10},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9-size-rank.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 mmap: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	if len(loaded.Derived.SizeOrder) != loaded.compactRecordCount() || len(loaded.Derived.SizeRank) != loaded.compactRecordCount() {
		t.Fatalf("size rank section missing: %+v", loaded.Derived)
	}
	// Directories rank by their recursive total (root = 310 here), so the
	// order is small.txt (10), large.txt (300), root (310).
	if got := loaded.compactRecord(int(loaded.Derived.SizeOrder[0])).Name; got != "small.txt" {
		t.Fatalf("first size-ranked record = %q, want small.txt", got)
	}
	if got := loaded.compactRecord(int(loaded.Derived.SizeOrder[1])).Name; got != "large.txt" {
		t.Fatalf("second size-ranked record = %q, want large.txt", got)
	}
	if got := loaded.compactRecord(int(loaded.Derived.SizeOrder[2])).Name; got != "." {
		t.Fatalf("third size-ranked record = %q, want root", got)
	}
	sections, _ := derivedSectionInfo(loaded.Derived)
	if !slices.Contains(sections, "SRNK") {
		t.Fatalf("sections = %v, missing SRNK", sections)
	}
}

func TestEngineWritesAndLoadsModifiedRankSection(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 125),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), ModUnix: 1},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "old.txt", ModUnix: 10},
			{FRN: 12, ParentFRN: 10, Parent: 0, Name: "new.txt", ModUnix: 30},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9-modified-rank.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 mmap: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	if len(loaded.Derived.ModOrder) != loaded.compactRecordCount() || len(loaded.Derived.ModRank) != loaded.compactRecordCount() {
		t.Fatalf("modified rank section missing: %+v", loaded.Derived)
	}
	if got := loaded.compactRecord(int(loaded.Derived.ModOrder[0])).Name; got != "new.txt" {
		t.Fatalf("first modified-ranked record = %q, want new.txt", got)
	}
	sections, _ := derivedSectionInfo(loaded.Derived)
	if !slices.Contains(sections, "MRNK") {
		t.Fatalf("sections = %v, missing MRNK", sections)
	}
}

func TestEngineWritesAndLoadsExtensionRankSection(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 126),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "z.txt"},
			{FRN: 12, ParentFRN: 10, Parent: 0, Name: "a.go"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9-extension-rank.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 mmap: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	if len(loaded.Derived.ExtOrder) != loaded.compactRecordCount() || len(loaded.Derived.ExtRank) != loaded.compactRecordCount() {
		t.Fatalf("extension rank section missing: %+v", loaded.Derived)
	}
	if got := loaded.compactRecord(int(loaded.Derived.ExtOrder[1])).Name; got != "a.go" {
		t.Fatalf("second extension-ranked record = %q, want a.go", got)
	}
	sections, _ := derivedSectionInfo(loaded.Derived)
	if !slices.Contains(sections, "ERNK") {
		t.Fatalf("sections = %v, missing ERNK", sections)
	}
}

func TestEngineWritesAndLoadsTypeRankSection(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 127),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "z.txt"},
			{FRN: 12, ParentFRN: 10, Parent: 0, Name: "src", Mode: uint32(os.ModeDir)},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9-type-rank.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 mmap: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	if len(loaded.Derived.TypeOrder) != loaded.compactRecordCount() || len(loaded.Derived.TypeRank) != loaded.compactRecordCount() {
		t.Fatalf("type rank section missing: %+v", loaded.Derived)
	}
	if got := loaded.compactRecord(int(loaded.Derived.TypeOrder[1])).Name; got != "src" {
		t.Fatalf("second type-ranked record = %q, want src", got)
	}
	sections, _ := derivedSectionInfo(loaded.Derived)
	if !slices.Contains(sections, "TRNK") {
		t.Fatalf("sections = %v, missing TRNK", sections)
	}
}

func TestEngineWritesAndLoadsPathRankSection(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 128),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "zeta", Mode: uint32(os.ModeDir)},
			{FRN: 12, ParentFRN: 11, Parent: 1, Name: "a.txt"},
			{FRN: 13, ParentFRN: 10, Parent: 0, Name: "alpha", Mode: uint32(os.ModeDir)},
			{FRN: 14, ParentFRN: 13, Parent: 3, Name: "z.txt"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "v9-path-rank.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 mmap: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	if len(loaded.Derived.PathOrder) != loaded.compactRecordCount() || len(loaded.Derived.PathRank) != loaded.compactRecordCount() {
		t.Fatalf("path rank section missing: %+v", loaded.Derived)
	}
	if got := loaded.reconstructCompactPath(int(loaded.Derived.PathOrder[1])); got != `C:\alpha` {
		t.Fatalf("second path-ranked record = %q, want C:\\alpha", got)
	}
	sections, _ := derivedSectionInfo(loaded.Derived)
	if !slices.Contains(sections, "PRNK") {
		t.Fatalf("sections = %v, missing PRNK", sections)
	}
}

func TestEngineUpgradeIndexCommand(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		BuiltAt: time.Unix(0, 456),
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 20, ParentFRN: 20, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 21, ParentFRN: 20, Parent: 0, Name: "src", Mode: uint32(os.ModeDir)},
			{FRN: 22, ParentFRN: 21, Parent: 1, Name: "main.go"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "upgrade.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v8: %v", err)
	}
	if err := cmdUpgradeIndex([]string{"-db", db}); err != nil {
		t.Fatalf("upgrade-index: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load upgraded index: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	if loaded.Version != indexVersion {
		t.Fatalf("version = %d, want %d", loaded.Version, indexVersion)
	}
	sections, bytes := derivedSectionInfo(loaded.Derived)
	if !reflect.DeepEqual(sections, []string{"RANK", "ERNK", "TRNK", "PRNK", "CHLD", "SUBT", "SUBS", "FRNS", "LOWR", "PNGR"}) {
		t.Fatalf("sections = %v", sections)
	}
	if bytes == 0 {
		t.Fatal("derived byte count was not reported")
	}
}

func TestServiceSearchCountCompatibilityMatrixAndMissingDerived(t *testing.T) {
	base := compatibilityMatrixIndex()
	queries := []queryOptions{
		{Query: "path:workspace alpha", Limit: 20},
		{Query: "dir:workspace alpha", MatchPath: true, Limit: 20},
		{Query: "ext:txt", Limit: 20},
		{Query: "glob:*.go", Limit: 20},
		{Query: "path:workspace alpha|beta", Limit: 20},
		{Query: "path:workspace !beta", Limit: 20},
		{Query: "path:workspace regex:alpha-[0-9]+\\.txt", Limit: 20},
	}
	cases := []struct {
		name  string
		index *Index
	}{
		{name: "v9-derived", index: roundTripCompatibilityIndex(t, base, true, false)},
		{name: "v9-missing-derived", index: roundTripCompatibilityIndex(t, base, true, true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := newServiceVolumeIndex(tc.name+".gsi", tc.index)
			for _, opts := range queries {
				t.Run(opts.Query, func(t *testing.T) {
					full, err := searchCompactWithCache(tc.index, opts, false, make(map[int]string), nil)
					if err != nil {
						t.Fatalf("full search: %v", err)
					}
					got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
					if err != nil {
						t.Fatalf("service search: %v", err)
					}
					if gotPaths, wantPaths := pathsOf(got), pathsOf(full); !slices.Equal(gotPaths, wantPaths) {
						t.Fatalf("service paths = %v, want full paths %v", gotPaths, wantPaths)
					}
					countOpts := opts
					countOpts.Limit = 0
					fullCount, err := searchCompactWithCache(tc.index, countOpts, true, make(map[int]string), nil)
					if err != nil {
						t.Fatalf("full count search: %v", err)
					}
					countMatches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, countOpts, true)
					if err != nil {
						t.Fatalf("service count search: %v", err)
					}
					if len(countMatches) != len(fullCount) {
						t.Fatalf("service count-search len = %d, want %d", len(countMatches), len(fullCount))
					}
					fastCount, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, countOpts)
					if err != nil {
						t.Fatalf("fast count: %v", err)
					}
					if ok && fastCount != len(fullCount) {
						t.Fatalf("fast count = %d, want %d", fastCount, len(fullCount))
					}
				})
			}
		})
	}
}

func compatibilityMatrixIndex() *Index {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`C:\`},
		BuiltAt: time.Unix(0, 987),
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "workspace", Mode: uint32(os.ModeDir)},
			{FRN: 3, ParentFRN: 2, Parent: 1, Name: "alpha-01.txt", Size: 10, ModUnix: 10},
			{FRN: 4, ParentFRN: 2, Parent: 1, Name: "beta.go", Size: 20, ModUnix: 20},
			{FRN: 5, ParentFRN: 2, Parent: 1, Name: "gamma.md", Size: 30, ModUnix: 30},
			{FRN: 6, ParentFRN: 1, Parent: 0, Name: "outside-alpha.txt", Size: 40, ModUnix: 40},
		},
	}
	buildOrders(idx)
	return idx
}

func roundTripCompatibilityIndex(t *testing.T, idx *Index, withDerived, clearDerived bool) *Index {
	t.Helper()
	db := filepath.Join(t.TempDir(), "compat-v9.gsi")
	if err := saveIndex(db, cloneCompactIndex(idx)); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndex(db)
	if err != nil {
		t.Fatalf("load v9: %v", err)
	}
	if clearDerived {
		loaded.Derived = indexDerivedSections{}
	}
	return loaded
}

func cloneCompactIndex(idx *Index) *Index {
	if idx == nil {
		return nil
	}
	out := &Index{
		Version:          idx.Version,
		Roots:            append([]string(nil), idx.Roots...),
		BuiltAt:          idx.BuiltAt,
		Source:           idx.Source,
		Volume:           idx.Volume,
		JournalID:        idx.JournalID,
		Checkpoint:       idx.Checkpoint,
		ContentHash:      idx.ContentHash,
		Entries:          append([]Entry(nil), idx.Entries...),
		NameOrder:        append([]int(nil), idx.NameOrder...),
		PathOrder:        append([]int(nil), idx.PathOrder...),
		Compact:          idx.Compact,
		Records:          append([]CompactRecord(nil), idx.Records...),
		PackedRecords:    idx.PackedRecords,
		MMapRecords:      idx.MMapRecords,
		CompactAttrs:     idx.CompactAttrs,
		CompactNameOrder: append([]int(nil), idx.CompactNameOrder...),
		NameBlob:         idx.NameBlob,
		Derived:          idx.Derived,
		DBPath:           idx.DBPath,
	}
	return out
}

func TestEngineOverlaySnapshotScaffold(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	if vol.overlay == nil || vol.snap.Load() == nil {
		t.Fatal("engine v9 did not initialize overlay snapshot scaffold")
	}
	vol.applyUSNChanges([]usnChange{{FRN: 101, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "alpha.txt"}})
	snap := vol.snap.Load()
	if snap == nil || snap.watermark != 1 || len(snap.records) < 1 {
		t.Fatalf("snapshot = %+v, want watermark 1", snap)
	}
	if got := vol.overlay.byFRN[101]; got != 0 {
		t.Fatalf("overlay slot for frn 101 = %d, want 0", got)
	}
	vol.applyUSNChanges([]usnChange{{FRN: 101, USN: 11, Reason: usnReasonFileDelete}})
	if len(vol.overlay.records) != 2 || !vol.overlay.records[1].Deleted {
		t.Fatalf("delete did not append overlay tombstone marker: %+v", vol.overlay.records)
	}
	if len(idx.Records) != 1 || idx.Records[0].Deleted {
		t.Fatalf("v9 overlay path mutated base records: %+v", idx.Records)
	}
}

func TestEngineOverlaySnapshotIsStableAcrossLaterChanges(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	vol.applyUSNChanges([]usnChange{{FRN: 101, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "first.txt"}})
	first := vol.snap.Load()
	if first == nil || first.watermark != 1 || len(first.records) < 1 {
		t.Fatal("first snapshot missing overlay records")
	}
	vol.applyUSNChanges([]usnChange{{FRN: 101, ParentFRN: 100, USN: 11, Reason: usnReasonRenameNew, Name: "second.txt"}})

	firstRecords := first.records[:first.watermark]
	entry, ok := vol.overlayEntry(firstRecords, latestOverlaySlotsByFRN(firstRecords), 0, map[int32]struct{}{}, nil)
	if !ok {
		t.Fatal("first snapshot entry disappeared after later rename")
	}
	if entry.Name != "first.txt" {
		t.Fatalf("first snapshot entry name = %q, want first.txt", entry.Name)
	}
	latest := vol.snap.Load()
	latestRecords := latest.records[:latest.watermark]
	entry, ok = vol.overlayEntry(latestRecords, latestOverlaySlotsByFRN(latestRecords), 1, map[int32]struct{}{}, nil)
	if !ok || entry.Name != "second.txt" {
		t.Fatalf("latest snapshot entry = (%+v, %v), want second.txt", entry, ok)
	}
}

func TestEngineOverlayServiceSearchMergesCreatesAndDeletes(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		BuiltAt: time.Unix(0, 789),
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "base.txt"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "overlay.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 mmap: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	vol := newServiceVolumeIndex(db, loaded)
	vol.applyUSNChanges([]usnChange{{FRN: 102, ParentFRN: 100, USN: 20, Reason: usnReasonFileCreate, Name: "overlay.txt"}})
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "overlay", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search overlay: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\overlay.txt`}) {
		t.Fatalf("overlay search paths = %v", pathsOf(got))
	}
	vol.applyUSNChanges([]usnChange{{FRN: 101, USN: 21, Reason: usnReasonFileDelete}})
	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "base", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search tombstoned base: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("tombstoned base leaked into search: %v", pathsOf(got))
	}
	if loaded.MMapRecords == nil || len(loaded.Records) != 0 {
		t.Fatalf("v9 overlay materialized base records: mmap=%v records=%d", loaded.MMapRecords != nil, len(loaded.Records))
	}
}

func TestEngineOverlayAttribFilterMatchesCreates(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	idx := &Index{
		Version:      indexVersion,
		Roots:        []string{`F:\`},
		BuiltAt:      time.Unix(0, 790),
		Source:       "usn",
		Volume:       "F:",
		Compact:      true,
		CompactAttrs: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: modeFromAttrs(fileAttributeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "plain.txt", Mode: modeFromAttrs(fileAttributeArchive)},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "overlay-attrs.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 mmap: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	vol := newServiceVolumeIndex(db, loaded)
	vol.applyUSNChanges([]usnChange{{
		FRN:       102,
		ParentFRN: 100,
		USN:       20,
		Reason:    usnReasonFileCreate,
		Name:      "hidden.txt",
		Attr:      fileAttributeHidden | fileAttributeArchive,
	}})

	trace := &searchTrace{}
	opts := queryOptions{Query: "attrib:H", Limit: 10, Trace: trace}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
	if err != nil {
		t.Fatalf("search overlay attrib: %v", err)
	}
	if trace.PlannerMode != "global-components" {
		t.Fatalf("planner mode = %q, want global-components; decline=%s", trace.PlannerMode, trace.Decline)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\hidden.txt`}) {
		t.Fatalf("overlay attrib paths = %v", pathsOf(got))
	}
	countTrace := &searchTrace{}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "attrib:H", Trace: countTrace})
	if err != nil {
		t.Fatalf("count overlay attrib: %v", err)
	}
	if !ok || count != 1 {
		t.Fatalf("overlay attrib count = %d ok=%v, want 1 true", count, ok)
	}
	if countTrace.PlannerMode != "global-count-components" {
		t.Fatalf("count planner mode = %q, want global-count-components; decline=%s", countTrace.PlannerMode, countTrace.Decline)
	}
}

func TestEngineOverlayMergeFillsLimitAndRanksCreates(t *testing.T) {
	records := []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}}
	for i := 0; i < 40; i++ {
		records = append(records, CompactRecord{
			FRN:       uint64(200 + i),
			ParentFRN: 100,
			Parent:    0,
			Name:      fmt.Sprintf("needle-%02d.txt", i),
		})
	}
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: records,
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	changes := make([]usnChange, 0, 11)
	for i := 0; i < 10; i++ {
		changes = append(changes, usnChange{FRN: uint64(200 + i), USN: int64(10 + i), Reason: usnReasonFileDelete})
	}
	changes = append(changes, usnChange{
		FRN:       999,
		ParentFRN: 100,
		USN:       30,
		Reason:    usnReasonFileCreate,
		Name:      "aaa-needle.txt",
	})
	vol.applyUSNChanges(changes)

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", Limit: 25}, false)
	if err != nil {
		t.Fatalf("search with overlay: %v", err)
	}
	paths := pathsOf(got)
	if len(paths) != 25 {
		t.Fatalf("overlay merge returned %d paths=%v, want filled limit 25", len(paths), paths)
	}
	if paths[0] != `F:\aaa-needle.txt` {
		t.Fatalf("first result = %q, want overlay create ranked first; paths=%v", paths[0], paths[:min(len(paths), 5)])
	}
	for i := 0; i < 10; i++ {
		deleted := fmt.Sprintf(`F:\needle-%02d.txt`, i)
		if slices.Contains(paths, deleted) {
			t.Fatalf("deleted base path %s leaked in %v", deleted, paths)
		}
	}
	if !slices.Contains(paths, `F:\needle-33.txt`) {
		t.Fatalf("later base matches did not backfill limit: %v", paths)
	}
}

func TestEngineOverlaySortPathRanksCreateBeforeBaseInsertionPoint(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "needle-b.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	vol.applyUSNChanges([]usnChange{{
		FRN:       999,
		ParentFRN: 100,
		USN:       30,
		Reason:    usnReasonFileCreate,
		Name:      "needle-a.txt",
	}})

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle sort:path", Limit: 2}, false)
	if err != nil {
		t.Fatalf("search with overlay sort:path: %v", err)
	}
	if paths := pathsOf(got); !reflect.DeepEqual(paths, []string{`F:\needle-a.txt`, `F:\needle-b.txt`}) {
		t.Fatalf("sort:path overlay paths = %v, want overlay create before base", paths)
	}
}

func TestEngineOverlayCountOnlySearchIncludesCreatesAndExcludesTombstones(t *testing.T) {
	vol := engineOverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{
		{FRN: 102, ParentFRN: 100, USN: 20, Reason: usnReasonFileCreate, Name: "needle-overlay.txt"},
		{FRN: 101, USN: 21, Reason: usnReasonFileDelete},
	})

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true}, true)
	if err != nil {
		t.Fatalf("count-only overlay search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("count-only needle matches = %d paths=%v, want only overlay-created record", len(got), pathsOf(got))
	}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true})
	if err != nil {
		t.Fatalf("overlay-aware fast count: %v", err)
	}
	if !ok || count != 1 {
		t.Fatalf("overlay-aware fast count = (%d, %v), want (1, true)", count, ok)
	}
}

func TestEngineDirectoryDeleteTombstonesBaseDescendants(t *testing.T) {
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		BuiltAt: time.Unix(0, 789),
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 200, ParentFRN: 100, Parent: 0, Name: "doomed", Mode: uint32(os.ModeDir)},
			{FRN: 201, ParentFRN: 200, Parent: 1, Name: "child.txt"},
			{FRN: 202, ParentFRN: 201, Parent: 2, Name: "grandchild.log"},
			{FRN: 300, ParentFRN: 100, Parent: 0, Name: "survivor.txt"},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "dir-delete.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save fixture: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	defer loaded.MMapRecords.file.close()
	vol := newServiceVolumeIndex(db, loaded)
	vol.applyUSNChanges([]usnChange{{FRN: 200, USN: 20, Reason: usnReasonFileDelete}})

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "child", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search deleted child: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted directory descendants leaked into search: %v", pathsOf(got))
	}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "child", MatchPath: true}, true)
	if err != nil {
		t.Fatalf("count deleted child: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted directory descendants leaked into count: %v", pathsOf(got))
	}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "survivor", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search survivor: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\survivor.txt`}) {
		t.Fatalf("survivor paths = %v", pathsOf(got))
	}
}

func TestEngineOverlayRenameSearchHidesOldPathAndShowsNewPath(t *testing.T) {
	vol := engineOverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{
		{FRN: 101, ParentFRN: 100, USN: 20, Reason: usnReasonRenameOld, Name: "needle-base.txt"},
		{FRN: 101, ParentFRN: 100, USN: 21, Reason: usnReasonRenameNew, Name: "needle-renamed.txt"},
	})

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search renamed overlay: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\needle-renamed.txt`}) {
		t.Fatalf("rename overlay paths = %v, want only renamed path", pathsOf(got))
	}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "needle-base", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search old renamed path: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("old base path leaked after rename: %v", pathsOf(got))
	}
}

func TestEngineOverlaySearchReconstructsChildPathThroughBaseParent(t *testing.T) {
	vol := engineOverlaySearchTestVolume(t)
	vol.applyUSNChanges([]usnChange{{
		FRN:       201,
		ParentFRN: 200,
		USN:       20,
		Reason:    usnReasonFileCreate,
		Name:      "overlay-child.txt",
	}})

	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "overlay-child", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search overlay child: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\base-parent\overlay-child.txt`}) {
		t.Fatalf("overlay child paths = %v", pathsOf(got))
	}
}

func TestEngineCompactOverlayIndexMergesBaseAndOverlay(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "delete-me.txt"},
			{FRN: 102, ParentFRN: 100, Parent: 0, Name: "keep-me.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	vol.applyUSNChanges([]usnChange{
		{FRN: 101, USN: 30, Reason: usnReasonFileDelete},
		{FRN: 103, ParentFRN: 100, USN: 31, Reason: usnReasonFileCreate, Name: "created.txt"},
	})
	compacted := compactOverlayIndex(vol)
	if compacted.Checkpoint != 31 {
		t.Fatalf("checkpoint = %d, want 31", compacted.Checkpoint)
	}
	names := make([]string, 0, len(compacted.Records))
	for _, rec := range compacted.Records {
		names = append(names, rec.Name)
		if rec.Name == "delete-me.txt" {
			t.Fatalf("deleted base record persisted: %+v", compacted.Records)
		}
	}
	if !sameStringSet(names, []string{".", "keep-me.txt", "created.txt"}) {
		t.Fatalf("compacted names = %v", names)
	}
	created := compacted.Records[2]
	if created.Parent != 0 || created.ParentFRN != 100 {
		t.Fatalf("created parent = %+v", created)
	}
}

func TestEnginePersistVolumeCompactsOverlayToMappedBase(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.gsi")
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		BuiltAt: time.Unix(0, 123),
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "delete-me.txt"},
		},
	}
	buildOrders(idx)
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save fixture: %v", err)
	}
	loaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	vol := newServiceVolumeIndex(db, loaded)
	vol.applyUSNChanges([]usnChange{
		{FRN: 101, USN: 40, Reason: usnReasonFileDelete},
		{FRN: 102, ParentFRN: 100, USN: 41, Reason: usnReasonFileCreate, Name: "created.txt"},
	})
	vol.dirty = true
	vol.nameOrderState.Store(nameTrigramStatePending)
	vol.nameOrderMillis.Store(123)
	svc := &goSearchService{
		indexes: []*Index{vol.index},
		volumes: []*serviceVolumeIndex{vol},
	}
	svc.persistVolumeIfDue(vol, true)
	if vol.dirty {
		t.Fatal("volume stayed dirty after forced compaction")
	}
	if vol.overlay != nil && vol.overlay.watermark.Load() != 0 {
		t.Fatalf("overlay survived compaction with watermark %d", vol.overlay.watermark.Load())
	}
	if got := vol.nameOrderStateString(); got != "ready" {
		t.Fatalf("compacted resident name order state = %q, want ready", got)
	}
	if got := vol.nameTrigramStateString(); got != "ready" {
		t.Fatalf("compacted resident trigram state = %q, want ready", got)
	}
	if vol.nameTrigramIndex() == nil {
		t.Fatal("compacted resident trigram section was not wired")
	}
	if vol.queryIndex == nil || len(vol.queryIndex.nameOrder) != vol.index.compactRecordCount() {
		t.Fatalf("compacted resident rank not wired: queryIndex=%v nameOrder=%d records=%d",
			vol.queryIndex != nil, len(vol.queryIndex.nameOrder), vol.index.compactRecordCount())
	}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "created", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search compacted overlay: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\created.txt`}) {
		t.Fatalf("compacted overlay paths = %v", pathsOf(got))
	}
	reloaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("reload compacted fixture: %v", err)
	}
	sections, _ := derivedSectionInfo(reloaded.Derived)
	for _, want := range []string{"RANK", "PEXT", "PNGR"} {
		if !slices.Contains(sections, want) {
			t.Fatalf("reloaded compacted sections = %v, missing %s", sections, want)
		}
	}
	reloadedVol := newServiceVolumeIndex(db, reloaded)
	if got := reloadedVol.nameOrderStateString(); got != "ready" {
		t.Fatalf("reloaded compacted name order state = %q, want ready", got)
	}
	if got := reloadedVol.nameTrigramStateString(); got != "ready" {
		t.Fatalf("reloaded compacted trigram state = %q, want ready", got)
	}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{reloadedVol}, queryOptions{Query: "created", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search reloaded compacted overlay: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\created.txt`}) {
		t.Fatalf("reloaded compacted overlay paths = %v", pathsOf(got))
	}
	if got, err := searchServiceVolumes([]*serviceVolumeIndex{reloadedVol}, queryOptions{Query: "delete-me", MatchPath: true, Limit: 10}, false); err != nil {
		t.Fatalf("search deleted compacted overlay: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("deleted path survived compaction: %v", pathsOf(got))
	}
}

func TestMappedPostingSectionUsesDecodedBlockCache(t *testing.T) {
	postings := map[string][]uint32{"go": make([]uint32, 0, 1500)}
	for i := 0; i < 1500; i++ {
		postings["go"] = append(postings["go"], uint32(i*2))
	}
	section := decodePostingSection(encodeStringPostingSection(postings, nil))
	if section.BlockCount < 2 {
		t.Fatalf("block count = %d, want at least 2", section.BlockCount)
	}

	servicePostingBlockCache = postingBlockLRU{}
	t.Setenv("SEEKFS_POSTING_CACHE_MB", "0")
	if got := section.stringPosting("go"); !reflect.DeepEqual(got, postings["go"]) {
		t.Fatalf("posting with disabled cache mismatch")
	}
	servicePostingBlockCache.mu.Lock()
	disabledItems := len(servicePostingBlockCache.items)
	servicePostingBlockCache.mu.Unlock()
	if disabledItems != 0 {
		t.Fatalf("disabled cache item count = %d, want 0", disabledItems)
	}

	servicePostingBlockCache = postingBlockLRU{}
	t.Setenv("SEEKFS_POSTING_CACHE_MB", "1")
	if got := section.stringPosting("go"); !reflect.DeepEqual(got, postings["go"]) {
		t.Fatalf("posting with enabled cache mismatch")
	}
	servicePostingBlockCache.mu.Lock()
	enabledItems := len(servicePostingBlockCache.items)
	enabledBytes := servicePostingBlockCache.bytes
	servicePostingBlockCache.mu.Unlock()
	if enabledItems != section.BlockCount {
		t.Fatalf("enabled cache item count = %d, want %d", enabledItems, section.BlockCount)
	}
	if enabledBytes <= 0 {
		t.Fatalf("enabled cache bytes = %d, want > 0", enabledBytes)
	}
}

func TestMappedPostingSectionIteratorParityAndIntersection(t *testing.T) {
	postings := map[string][]uint32{"go": make([]uint32, 0, 2500)}
	for i := 0; i < 2500; i++ {
		postings["go"] = append(postings["go"], uint32(i*2))
	}
	section := decodePostingSection(encodeStringPostingSection(postings, nil))
	it, count, ok := section.stringPostingIterator("go")
	if !ok {
		t.Fatal("stringPostingIterator declined mapped posting")
	}
	if count != len(postings["go"]) {
		t.Fatalf("iterator count = %d, want %d", count, len(postings["go"]))
	}
	if got := materializePostingBlockIterator(it, count); !reflect.DeepEqual(got, postings["go"]) {
		t.Fatalf("iterator materialization mismatch")
	}

	it, _, ok = section.stringPostingIterator("go")
	if !ok {
		t.Fatal("stringPostingIterator declined mapped posting for intersection")
	}
	seed := []uint32{1, 4, 8, 1998, 2000, 4998, 5001}
	got := intersectSortedUint32sWithPostingIterator(seed, it)
	want := []uint32{4, 8, 1998, 2000, 4998}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("block iterator intersection = %v, want %v", got, want)
	}
}

func TestWalkWatcherRebuildPathRefreshesWalkIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatalf("write old fixture: %v", err)
	}
	db := filepath.Join(t.TempDir(), "walk.gsi")
	idx := &Index{Version: indexVersion, Roots: []string{root}, BuiltAt: time.Now(), Source: "walk"}
	if err := walkRoot(root, idx); err != nil {
		t.Fatalf("walk fixture: %v", err)
	}
	buildOrders(idx)
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save walk fixture: %v", err)
	}
	loaded, err := loadIndexForService(db)
	if err != nil {
		t.Fatalf("load walk fixture: %v", err)
	}
	vol := newServiceVolumeIndex(db, loaded)
	svc := &goSearchService{
		indexes: []*Index{vol.index},
		volumes: []*serviceVolumeIndex{vol},
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatalf("write new fixture: %v", err)
	}
	if err := svc.rebuildWalkVolumeInPlace(vol, "test"); err != nil {
		t.Fatalf("rebuild walk index: %v", err)
	}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "new", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search rebuilt walk volume: %v", err)
	}
	if len(got) != 1 || got[0].Name != "new.txt" {
		t.Fatalf("rebuilt walk search results = %+v", got)
	}
	if len(svc.indexes) != 1 || svc.indexes[0] != vol.index {
		t.Fatal("service index slice was not updated to the rebuilt live index")
	}
}

func TestEngineOverlayCompactionDueTriggers(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "base.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "state.gsi"), idx)
	now := time.Unix(1_000, 0)
	vol.lastPersist = now
	if vol.compactionDue(now.Add(time.Minute)) {
		t.Fatal("fresh empty overlay triggered compaction")
	}
	vol.overlay.watermark.Store(overlayCompactionMaxSlots)
	if !vol.compactionDue(now.Add(time.Minute)) {
		t.Fatal("overlay slot threshold did not trigger compaction")
	}

	vol.overlay = newOverlaySegment()
	vol.overlay.tombstone.add(1)
	vol.overlay.watermark.Store(1)
	if !vol.compactionDue(now.Add(time.Minute)) {
		t.Fatal("tombstone ratio threshold did not trigger compaction")
	}

	vol.overlay = newOverlaySegment()
	if !vol.compactionDue(now.Add(overlayCompactionDirtyAge)) {
		t.Fatal("dirty age threshold did not trigger compaction")
	}

	wal := walPath(vol.dbPath)
	f, err := os.Create(wal)
	if err != nil {
		t.Fatalf("create wal: %v", err)
	}
	if err := f.Truncate(overlayCompactionMaxWAL); err != nil {
		t.Fatalf("truncate wal: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close wal: %v", err)
	}
	vol.lastPersist = now
	if !vol.compactionDue(now.Add(time.Minute)) {
		t.Fatal("wal size threshold did not trigger compaction")
	}
}

func TestEngineLockVolumeSearchDoesNotSerializeOnSearchMu(t *testing.T) {
	vol := &serviceVolumeIndex{}
	vol.searchMu.Lock()
	defer vol.searchMu.Unlock()

	locked, ok := lockVolumeSearch(vol, queryOptions{})
	if !ok {
		t.Fatal("v9 lockVolumeSearch rejected uncanceled query")
	}
	if locked {
		t.Fatal("v9 lockVolumeSearch acquired searchMu")
	}
}

func TestEngineSnapshotServiceVolumeForSearchUsesStableBase(t *testing.T) {
	oldIdx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "old-base.txt"},
		},
	}
	buildOrders(oldIdx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, oldIdx)
	view := snapshotServiceVolumeForSearch(vol)

	newIdx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 200, ParentFRN: 200, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 201, ParentFRN: 200, Parent: 0, Name: "new-base.txt"},
		},
	}
	buildOrders(newIdx)
	replaceServiceVolumeContents(vol, newServiceVolumeIndex(`F:\state.gsi`, newIdx))

	got, err := searchServiceVolumes([]*serviceVolumeIndex{view}, queryOptions{Query: "old-base", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search read view: %v", err)
	}
	if !sameStringSet(pathsOf(got), []string{`F:\old-base.txt`}) {
		t.Fatalf("read view paths = %v, want old base", pathsOf(got))
	}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{view}, queryOptions{Query: "new-base", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search read view new base: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("read view saw replaced live base: %v", pathsOf(got))
	}
}

func TestEngineReadViewCachesUseSnapshotGeneration(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "dir", Mode: uint32(os.ModeDir)},
			{FRN: 102, ParentFRN: 101, Parent: 1, Name: "alpha-cache.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	view := snapshotServiceVolumeForSearch(vol)
	gen := view.cacheGeneration()
	if gen == 0 {
		t.Fatal("read view cache generation was zero")
	}

	got := view.nameTermPosting("alpha")
	if !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("nameTermPosting = %v, want [2]", got)
	}
	if stamp := view.termCache["alpha"].gen; stamp != gen {
		t.Fatalf("term cache stamp = %d, want snapshot gen %d", stamp, gen)
	}
	view.termCache["alpha"] = postingCacheEntry{ids: []int{99}, gen: gen - 1}
	if got := view.nameTermPosting("alpha"); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("stale term cache was not ignored: %v", got)
	}
	view.underRootCache[strings.ToLower(filepath.Clean(`F:\dir`))] = postingCacheEntry{ids: []int{99}, gen: gen - 1}
	if got := view.underRootIDs(`F:\dir`); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("stale under-root cache was not ignored: %v", got)
	}

	vol.applyUSNChanges([]usnChange{{FRN: 102, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "beta-cache.txt"}})
	next := snapshotServiceVolumeForSearch(vol)
	if next.cacheGeneration() == gen {
		t.Fatalf("cache generation did not advance after publish: %d", gen)
	}
	if _, ok := next.termCache["alpha"]; ok {
		t.Fatal("new read view reused prior query cache")
	}
}

func engineOverlaySearchTestVolume(t *testing.T) *serviceVolumeIndex {
	t.Helper()
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		BuiltAt: time.Unix(0, 789),
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "needle-base.txt"},
			{FRN: 200, ParentFRN: 100, Parent: 0, Name: "base-parent", Mode: uint32(os.ModeDir)},
		},
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "overlay-search.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save v9 overlay search fixture: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load v9 overlay search fixture: %v", err)
	}
	t.Cleanup(func() {
		if loaded.MMapRecords != nil {
			_ = loaded.MMapRecords.file.close()
		}
	})
	return newServiceVolumeIndex(db, loaded)
}

func TestEngineOverlayMutationStateMachineMatchesFreshOracle(t *testing.T) {
	logical := map[uint64]CompactRecord{
		100: {FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), Size: 0},
	}
	idx := freshIndexFromLogicalRecords("F:", logical)
	vol := newServiceVolumeIndex(`F:\seekfs_state.gsi`, idx)
	steps := [][]usnChange{
		{{FRN: 101, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "alpha-needle.txt"}},
		{{FRN: 102, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "beta-needle.go"}},
		{{FRN: 102, ParentFRN: 100, USN: 13, Reason: usnReasonRenameOld, Name: "beta-needle.go"}, {FRN: 102, ParentFRN: 100, USN: 14, Reason: usnReasonRenameNew, Name: "gamma-needle.go"}},
		{{FRN: 103, ParentFRN: 100, USN: 15, Reason: usnReasonFileCreate, Name: "subdir", Attr: fileAttributeDir}},
		{{FRN: 104, ParentFRN: 103, USN: 16, Reason: usnReasonFileCreate, Name: "delta-needle.txt"}},
		{{FRN: 103, USN: 17, Reason: usnReasonFileDelete}},
		{{FRN: 101, USN: 18, Reason: usnReasonFileDelete}},
		{{FRN: 101, ParentFRN: 100, USN: 19, Reason: usnReasonFileCreate, Name: "aaa-needle.md"}},
		// (a) directory rename to a NEW parent: create two sibling directories,
		// then move "movable-dir" (and its child) from under "parent-a" to
		// under "parent-b" via rename-old/rename-new carrying a different
		// ParentFRN, not just a name change in place.
		{{FRN: 105, ParentFRN: 100, USN: 20, Reason: usnReasonFileCreate, Name: "parent-a", Attr: fileAttributeDir}},
		{{FRN: 106, ParentFRN: 100, USN: 21, Reason: usnReasonFileCreate, Name: "parent-b", Attr: fileAttributeDir}},
		{{FRN: 107, ParentFRN: 105, USN: 22, Reason: usnReasonFileCreate, Name: "movable-dir", Attr: fileAttributeDir}},
		{{FRN: 108, ParentFRN: 107, USN: 23, Reason: usnReasonFileCreate, Name: "epsilon-needle.txt"}},
		{
			{FRN: 107, ParentFRN: 105, USN: 24, Reason: usnReasonRenameOld, Name: "movable-dir"},
			{FRN: 107, ParentFRN: 106, USN: 25, Reason: usnReasonRenameNew, Name: "movable-dir"},
		},
		// (c) delete-then-recreate at the SAME path (same name+parent): delete
		// "gamma-needle.go" (FRN 102, currently under the root) and recreate a
		// file at the identical name+parent with a brand-new FRN.
		{{FRN: 102, USN: 26, Reason: usnReasonFileDelete}},
		{{FRN: 109, ParentFRN: 100, USN: 27, Reason: usnReasonFileCreate, Name: "gamma-needle.go"}},
	}
	queries := []queryOptions{
		{Query: "needle", MatchPath: true, Limit: 20},
		{Query: "needle", Limit: 20},
		{Query: "ext:go", MatchPath: true, Limit: 20},
		{Query: "type:dir", MatchPath: true, Limit: 20},
	}
	for step, changes := range steps {
		vol.applyUSNChanges(changes)
		applyLogicalUSNChanges(logical, changes)
		fresh := freshIndexFromLogicalRecords("F:", logical)
		freshVol := newServiceVolumeIndex(`F:\fresh_state.gsi`, fresh)
		for _, opts := range queries {
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
			if err != nil {
				t.Fatalf("step %d query %q v9 overlay search: %v", step, opts.Query, err)
			}
			want, err := searchCompactWithCache(fresh, opts, false, freshVol.pathCache, freshVol.nameTermCandidates)
			if err != nil {
				t.Fatalf("step %d query %q fresh search: %v", step, opts.Query, err)
			}
			if !sameOrderedStrings(pathsOf(got), pathsOf(want)) {
				t.Fatalf("step %d query %q paths=%v want=%v", step, opts.Query, pathsOf(got), pathsOf(want))
			}
		}
	}
}

func TestEngineConcurrentChurnQueryStress(t *testing.T) {
	idx := freshIndexFromLogicalRecords("F:", map[uint64]CompactRecord{
		100: {FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
	})
	vol := newServiceVolumeIndex(`F:\seekfs_churn_stress.gsi`, idx)
	queries := []queryOptions{
		{Query: "needle", MatchPath: true, Limit: 20},
		{Query: "ext:txt", MatchPath: true, Limit: 20},
		{Query: "type:dir", MatchPath: true, Limit: 20},
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 8)
	for worker := 0; worker < 2; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 80; i++ {
				for _, opts := range queries {
					if _, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false); err != nil {
						errs <- fmt.Errorf("search %q: %w", opts.Query, err)
						return
					}
					if _, _, err := countServiceVolumes([]*serviceVolumeIndex{vol}, opts); err != nil {
						errs <- fmt.Errorf("count %q: %w", opts.Query, err)
						return
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 80; i++ {
			frn := uint64(1000 + i)
			vol.applyUSNChanges([]usnChange{{
				FRN:       frn,
				ParentFRN: 100,
				USN:       int64(10 + i*3),
				Reason:    usnReasonFileCreate,
				Name:      fmt.Sprintf("needle-%03d.txt", i),
			}})
			if i%3 == 0 {
				vol.applyUSNChanges([]usnChange{{
					FRN:    frn,
					USN:    int64(11 + i*3),
					Reason: usnReasonFileDelete,
				}})
			}
		}
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func applyLogicalUSNChanges(logical map[uint64]CompactRecord, changes []usnChange) {
	for _, change := range changes {
		rec := logical[change.FRN]
		switch {
		case change.Reason&usnReasonFileDelete != 0:
			markLogicalDeleted(logical, change.FRN)
		case change.Reason&usnReasonRenameNew != 0 || change.Reason&usnReasonFileCreate != 0:
			rec.FRN = change.FRN
			rec.ParentFRN = change.ParentFRN
			rec.Name = change.Name
			rec.Mode = modeFromAttrs(change.Attr)
			rec.Deleted = false
			rec.Size = 1024
			rec.ModUnix = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
			logical[change.FRN] = rec
		}
	}
}

func markLogicalDeleted(logical map[uint64]CompactRecord, frn uint64) {
	rec := logical[frn]
	rec.Deleted = true
	logical[frn] = rec
	for childFRN, child := range logical {
		if childFRN == frn || child.Deleted || child.ParentFRN != frn {
			continue
		}
		markLogicalDeleted(logical, childFRN)
	}
}

func freshIndexFromLogicalRecords(volume string, logical map[uint64]CompactRecord) *Index {
	frns := make([]uint64, 0, len(logical))
	for frn := range logical {
		frns = append(frns, frn)
	}
	sort.Slice(frns, func(i, j int) bool { return frns[i] < frns[j] })
	idx := &Index{Source: "usn", Volume: volume, Compact: true, Records: make([]CompactRecord, 0, len(frns))}
	idByFRN := make(map[uint64]int32, len(frns))
	for _, frn := range frns {
		rec := logical[frn]
		if rec.Deleted {
			continue
		}
		idByFRN[frn] = int32(len(idx.Records))
		idx.Records = append(idx.Records, rec)
	}
	for i := range idx.Records {
		parentFRN := idx.Records[i].ParentFRN
		if idx.Records[i].FRN == parentFRN {
			idx.Records[i].Parent = -1
			continue
		}
		parent, ok := idByFRN[parentFRN]
		if !ok {
			idx.Records[i].Parent = -1
			continue
		}
		idx.Records[i].Parent = parent
	}
	buildOrders(idx)
	return idx
}

func TestServiceVolumeIndexReplaysBinaryWAL(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "test.gsi")
	if err := appendWAL(db, 12, []usnChange{{
		FRN:       102,
		ParentFRN: 100,
		USN:       12,
		Reason:    usnReasonFileCreate,
		Name:      "binary-wal-created.txt",
	}}); err != nil {
		t.Fatalf("append binary WAL: %v", err)
	}
	data, err := os.ReadFile(walPath(db))
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if !bytes.HasPrefix(data, walMagicV2) {
		t.Fatalf("wal prefix = %q, want %q", data[:min(len(data), len(walMagicV2))], walMagicV2)
	}
	reloaded := newServiceVolumeIndex(db, &Index{
		Source:     "usn",
		Volume:     "F:",
		Compact:    true,
		Checkpoint: 10,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: "."},
		},
	})
	if err := reloaded.replayWAL(); err != nil {
		t.Fatalf("replay binary WAL: %v", err)
	}
	if reloaded.checkpoint != 12 || len(reloaded.index.Records) != 1 {
		t.Fatalf("binary wal mutated base unexpectedly: checkpoint=%d records=%+v", reloaded.checkpoint, reloaded.index.Records)
	}
	if reloaded.overlay == nil || len(reloaded.overlay.records) != 1 || reloaded.overlay.records[0].Name != "binary-wal-created.txt" {
		t.Fatalf("binary wal not replayed into overlay: %+v", reloaded.overlay)
	}
}

func TestValidateUSNCheckpoint(t *testing.T) {
	vol := &serviceVolumeIndex{journalID: 10, checkpoint: 50}
	journal := usnJournalDataV0{UsnJournalID: 10, FirstUsn: 20, LowestValidUsn: 30, NextUsn: 100}
	if err := validateUSNCheckpoint(vol, journal); err != nil {
		t.Fatalf("valid checkpoint rejected: %v", err)
	}

	vol.checkpoint = 29
	if err := validateUSNCheckpoint(vol, journal); err == nil {
		t.Fatal("checkpoint before lowest valid USN was accepted")
	} else {
		var staleErr staleIndexError
		if !errors.As(err, &staleErr) || !staleErr.rebuild {
			t.Fatalf("error = %T %v, want rebuildable staleIndexError", err, err)
		}
	}

	vol.checkpoint = 101
	if err := validateUSNCheckpoint(vol, journal); err == nil {
		t.Fatal("checkpoint after next USN was accepted")
	}

	vol.checkpoint = 50
	vol.journalID = 11
	if err := validateUSNCheckpoint(vol, journal); err == nil {
		t.Fatal("changed journal id was accepted")
	}
}

func TestSearchCompactSkipsDeletedRecords(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: "."},
			{FRN: 101, ParentFRN: 100, Parent: 0, Name: "gone.txt", Deleted: true},
			{FRN: 102, ParentFRN: 100, Parent: 0, Name: "live.txt"},
		},
	}
	buildOrders(idx)

	matches, err := searchCompact(idx, queryOptions{Query: "txt", MatchPath: false, Limit: 10}, false)
	if err != nil {
		t.Fatalf("searchCompact: %v", err)
	}
	if len(matches) != 1 || matches[0].Name != "live.txt" {
		t.Fatalf("matches = %+v, want only live.txt", matches)
	}
}

func TestServiceVolumeIndexBuildsFRNMap(t *testing.T) {
	idx := &Index{
		Source:     "usn",
		Volume:     "F:",
		JournalID:  7,
		Checkpoint: 12,
		Compact:    true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Name: ".", Parent: -1},
			{FRN: 101, ParentFRN: 100, Name: "child.txt", Parent: 0},
		},
	}

	vol := newServiceVolumeIndex(`F:\seekfs_f.gsi`, idx)
	if vol.dbPath != `F:\seekfs_f.gsi` || vol.volume != "F:" || vol.journalID != 7 || vol.checkpoint != 12 || vol.state != "ready" {
		t.Fatalf("volume metadata mismatch: %+v", vol)
	}
	if vol.frnRecordCount() != 2 {
		t.Fatalf("frn record count = %d, want 2", vol.frnRecordCount())
	}
	if got, ok := vol.idForFRN(101); !ok || got != 1 {
		t.Fatalf("idForFRN(101) = %d, %v; want 1, true", got, ok)
	}
}

func TestCommonSearchQuerySemantics(t *testing.T) {
	idx := commonSearchFixture()
	cases := []struct {
		name      string
		opts      queryOptions
		wantNames []string
	}{
		{
			name:      "filename term",
			opts:      queryOptions{Query: "main", Limit: 20},
			wantNames: []string{"main.go"},
		},
		{
			name:      "full path terms",
			opts:      queryOptions{Query: "assets dat", MatchPath: true, Limit: 20},
			wantNames: []string{"sample.dat"},
		},
		{
			name:      "path substring and dotted filename substring",
			opts:      queryOptions{Query: "project .bin", MatchPath: true, Limit: 20},
			wantNames: []string{"volume.bin"},
		},
		{
			name:      "drive token",
			opts:      queryOptions{Query: "c: .bin", MatchPath: true, Limit: 20},
			wantNames: []string{"volume.bin"},
		},
		{
			name:      "compound dotted suffix",
			opts:      queryOptions{Query: ".tar.gz", MatchPath: true, Limit: 20},
			wantNames: []string{"archive.tar.gz"},
		},
		{
			name:      "extension filter",
			opts:      queryOptions{Query: "ext:go", MatchPath: true, Limit: 20},
			wantNames: []string{"main.go", "search_test.go"},
		},
		{
			name:      "directory filter",
			opts:      queryOptions{Query: "dir:src ext:go", MatchPath: true, Limit: 20},
			wantNames: []string{"main.go", "search_test.go"},
		},
		{
			name:      "path term plus extension",
			opts:      queryOptions{Query: "workspace ext:go", MatchPath: true, Limit: 20},
			wantNames: []string{"main.go", "search_test.go"},
		},
		{
			name:      "path term plus dotted literal",
			opts:      queryOptions{Query: "src main.go", MatchPath: true, Limit: 20},
			wantNames: []string{"main.go"},
		},
		{
			name:      "path term plus extension and dotted literal",
			opts:      queryOptions{Query: "src ext:go search_test.go", MatchPath: true, Limit: 20},
			wantNames: []string{"search_test.go"},
		},
		{
			name:      "sibling path term plus extension",
			opts:      queryOptions{Query: "downstream ext:dat", MatchPath: true, Limit: 20},
			wantNames: []string{"sibling.dat"},
		},
		{
			name:      "glob filter",
			opts:      queryOptions{Query: "glob:*_test.go", MatchPath: true, Limit: 20},
			wantNames: []string{"search_test.go"},
		},
		{
			name:      "regex filter",
			opts:      queryOptions{Query: `regex:Assets.*\.dat$`, MatchPath: true, Limit: 20},
			wantNames: []string{"sample.dat"},
		},
		{
			name:      "type file",
			opts:      queryOptions{Query: "type:file assets", MatchPath: true, Limit: 20},
			wantNames: []string{"sample.dat", "notes.txt"},
		},
		{
			name:      "type directory",
			opts:      queryOptions{Query: "type:dir assets", MatchPath: true, Limit: 20},
			wantNames: []string{"Assets"},
		},
		{
			name:      "under filter",
			opts:      queryOptions{Query: "ext:dat", MatchPath: true, Under: `C:\fixture\workspace\Assets`, Limit: 20},
			wantNames: []string{"sample.dat"},
		},
		{
			name:      "under excludes sibling prefix",
			opts:      queryOptions{Query: "ext:dat", MatchPath: true, Under: `C:\fixture\workspace\Down`, Limit: 20},
			wantNames: nil,
		},
		{
			name:      "case sensitive",
			opts:      queryOptions{Query: "case: README", Limit: 20},
			wantNames: []string{"README.md"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches, err := searchCompact(idx, tc.opts, false)
			if err != nil {
				t.Fatalf("searchCompact: %v", err)
			}
			got := namesOf(matches)
			if !sameStringSet(got, tc.wantNames) {
				t.Fatalf("names = %v, want %v", got, tc.wantNames)
			}
		})
	}
}

func TestServiceCandidatesMatchFullCompactSearchForCommonQueries(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []queryOptions{
		{Query: "assets dat", MatchPath: true, Limit: 20},
		{Query: "project .bin", MatchPath: true, Limit: 20},
		{Query: "c: .bin", MatchPath: true, Limit: 20},
		{Query: ".tar.gz", MatchPath: true, Limit: 20},
		{Query: "ext:dat", MatchPath: true, Under: `C:\fixture\workspace\Assets`, Limit: 20},
		{Query: "src go", MatchPath: true, Limit: 20},
		{Query: "ext:go dir:src", MatchPath: true, Limit: 20},
		{Query: "workspace ext:go", MatchPath: true, Limit: 20},
		{Query: "src main.go", MatchPath: true, Limit: 20},
		{Query: "src ext:go search_test.go", MatchPath: true, Limit: 20},
		{Query: "downstream ext:dat", MatchPath: true, Limit: 20},
		{Query: "glob:*_test.go", MatchPath: true, Limit: 20},
		{Query: `regex:Assets.*\.dat$`, MatchPath: true, Limit: 20},
		{Query: "type:dir Assets", MatchPath: true, Limit: 20},
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
			if !sameStringSet(namesOf(fast), namesOf(full)) {
				t.Fatalf("candidate names = %v, full names = %v", namesOf(fast), namesOf(full))
			}
		})
	}
}

func TestServiceCandidatesMatchFullSearchWithoutResidentSortedViews(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.queryIndex.nameOrder = nil
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil

	cases := []queryOptions{
		{Query: "assets dat", MatchPath: true, Limit: 20},
		{Query: "ext:dat", MatchPath: true, Under: `C:\fixture\workspace\Assets`, Limit: 20},
		{Query: "type:file glob:*.go", MatchPath: true, Under: `C:\fixture\workspace`, Limit: 20},
		{Query: "glob:*test*.go", MatchPath: true, Under: `C:\fixture\workspace`, Limit: 20},
		{Query: "src go", MatchPath: true, Limit: 20},
		{Query: "workspace ext:go", MatchPath: true, Limit: 20},
		{Query: "src main.go", MatchPath: true, Limit: 20},
		{Query: "src ext:go search_test.go", MatchPath: true, Limit: 20},
		{Query: "downstream ext:dat", MatchPath: true, Limit: 20},
		{Query: `src\main.go`, MatchPath: true, Under: `C:\fixture\workspace`, Limit: 20},
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
			if !sameStringSet(namesOf(fast), namesOf(full)) {
				t.Fatalf("candidate names = %v, full names = %v", namesOf(fast), namesOf(full))
			}
		})
	}
}

func TestServiceVolumeIndexUnderRootIDsWithoutResidentChildRanges(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.queryIndex.nameOrder = nil
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil

	got := vol.underRootIDs(`C:\fixture\workspace\Assets`)
	if len(got) != 1 {
		t.Fatalf("underRootIDs len = %d, ids = %v; want one root", len(got), got)
	}
	if name := idx.compactRecord(got[0]).Name; name != "Assets" {
		t.Fatalf("underRootIDs root name = %q, want Assets", name)
	}
	if got := vol.underRootIDs(`C:\fixture\workspace\Missing`); len(got) != 0 {
		t.Fatalf("missing underRootIDs = %v, want none", got)
	}
}

func TestServiceVolumeIndexUnderRootIDsUsesBasenameFallback(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.queryIndex.nameOrder = nil
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil
	vol.termCache = make(map[string]postingCacheEntry)
	vol.termCache["assets"] = postingCacheEntry{ids: []int{4}, gen: vol.cacheGeneration()}

	got := vol.underRootIDs(`C:\fixture\workspace\Assets`)
	if len(got) != 1 || got[0] != 3 {
		t.Fatalf("basename fallback root IDs = %v, want [3]", got)
	}
}

func TestServiceVolumeIndexUnderDescendantsCachesWithoutChildRanges(t *testing.T) {
	idx := commonSearchFixture()
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.queryIndex.nameOrder = nil
	vol.children = nil
	vol.childOffsets = nil
	vol.childIDs = nil
	root := vol.underRootIDs(`C:\fixture\workspace`)[0]

	first := vol.underDescendants(root)
	if len(first) == 0 {
		t.Fatal("underDescendants returned no descendants")
	}
	if len(vol.underCache[root].ids) == 0 {
		t.Fatal("underDescendants did not cache the root")
	}
	second := vol.underDescendants(root)
	if !sameIntSet(first, second) {
		t.Fatalf("cached descendants = %v, want %v", second, first)
	}
}

func TestParseQuerySplitsPathLikeTermsForPathSearch(t *testing.T) {
	pq, err := parseQuery(queryOptions{Query: `cmd\seekfs\main.go`, MatchPath: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cmd", "seekfs", "main.go"}
	if !sameStringSet(pq.Terms, want) {
		t.Fatalf("terms = %v, want %v", pq.Terms, want)
	}

	pq, err = parseQuery(queryOptions{Query: `cmd\seekfs\main.go`})
	if err != nil {
		t.Fatal(err)
	}
	if !pq.MatchPath {
		t.Fatal("path-like query did not infer path mode")
	}
	if !sameStringSet(pq.Terms, want) {
		t.Fatalf("inferred path-mode terms = %v, want %v", pq.Terms, want)
	}
}

func TestExtractUnderPathArgTreatsDriveTokenAsRoot(t *testing.T) {
	args, under := extractUnderPathArg([]string{"C:", "needle"})
	if under != `C:\` {
		t.Fatalf("under = %q, want C:\\", under)
	}
	if !sameStringSet(args, []string{"needle"}) {
		t.Fatalf("args = %v, want [needle]", args)
	}
}

func TestSortCandidateIDsPrefersExactFilename(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "WidgetObserver.cpp"},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "Widget.cpp"},
		},
	}
	buildOrders(idx)
	pq, err := parseQuery(queryOptions{Query: "Widget.cpp"})
	if err != nil {
		t.Fatal(err)
	}
	ids := []int{1, 2}
	sortCandidateIDs(ids, pq, idx, nil)
	if ids[0] != 2 {
		t.Fatalf("sorted candidates = %v, want exact Widget.cpp first", ids)
	}
}

func TestSortCandidateIDsCachedRanksMatchesFallback(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "zeta.txt"},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "alpha.txt"},
			{FRN: 4, ParentFRN: 1, Parent: 0, Name: "middle.txt"},
		},
	}
	buildOrders(idx)
	ranks := make([]uint32, idx.compactRecordCount())
	for i := range ranks {
		ranks[i] = uint32(i)
	}
	for pos := 0; pos < compactOrderLen(idx.CompactNameOrder, idx.compactRecordCount()); pos++ {
		id := compactOrderAt(idx.CompactNameOrder, pos)
		if id >= 0 && id < len(ranks) {
			ranks[id] = uint32(pos)
		}
	}
	pq, err := parseQuery(queryOptions{Query: "txt"})
	if err != nil {
		t.Fatal(err)
	}

	fallback := []int{1, 2, -1, 3, 99}
	cached := append([]int(nil), fallback...)
	sortCandidateIDs(fallback, pq, idx, nil)
	sortCandidateIDs(cached, pq, idx, ranks)
	if !reflect.DeepEqual(cached, fallback) {
		t.Fatalf("cached sort = %v, fallback = %v", cached, fallback)
	}
}

func TestServiceVolumeIndexExactNameIDsScansWhenNameOrderSkipped(t *testing.T) {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 2, ParentFRN: 1, Parent: 0, Name: "WidgetObserver.cpp"},
			{FRN: 3, ParentFRN: 1, Parent: 0, Name: "Widget.cpp"},
		},
	}
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	vol.queryIndex.nameOrder = nil
	vol.exactNames = nil

	got := vol.exactNameIDs("widget.cpp")
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("exactNameIDs = %v, want [2]", got)
	}
	if _, ok := vol.termCache["\x00exact:widget.cpp"]; !ok {
		t.Fatal("exactNameIDs did not cache scanned exact result")
	}
	if got := vol.exactNameIDs("widgetobserver.cpp"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("cached extension exactNameIDs = %v, want [1]", got)
	}
}

func BenchmarkSearchCompactBroadPathQuery(b *testing.B) {
	idx := syntheticCompactIndex(100_000)
	opts := queryOptions{Query: "needle", MatchPath: true, Limit: 20}
	cache := make(map[int]string)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		matches, err := searchCompactWithCache(idx, opts, false, cache, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(matches) != 20 {
			b.Fatalf("matches = %d, want 20", len(matches))
		}
	}
}

func BenchmarkSearchCompactExtPrecheck(b *testing.B) {
	idx := syntheticCompactIndex(100_000)
	opts := queryOptions{Query: "ext:go", MatchPath: true, Limit: 20}
	cache := make(map[int]string)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		matches, err := searchCompactWithCache(idx, opts, false, cache, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(matches) != 20 {
			b.Fatalf("matches = %d, want 20", len(matches))
		}
	}
}

func BenchmarkSearchCompactNameTokenPathQuery(b *testing.B) {
	idx := syntheticCompactIndex(100_000)
	vol := newServiceVolumeIndex("bench.gsi", idx)
	opts := queryOptions{Query: "source", MatchPath: true, Limit: 20}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		matches, err := searchCompactWithCache(idx, opts, false, vol.pathCache, vol.nameTermCandidates)
		if err != nil {
			b.Fatal(err)
		}
		if len(matches) != 20 {
			b.Fatalf("matches = %d, want 20", len(matches))
		}
	}
}

func syntheticCompactIndex(n int) *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: make([]CompactRecord, 0, n+2),
	}
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 1, ParentFRN: 1, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 2, ParentFRN: 1, Parent: 0, Name: "needle-root", Mode: uint32(os.ModeDir)},
	)
	for i := 0; i < n; i++ {
		parent := int32(0)
		parentFRN := uint64(1)
		if i%10 == 0 {
			parent = 1
			parentFRN = 2
		}
		name := fmt.Sprintf("file-%06d.txt", i)
		if i%37 == 0 {
			name = fmt.Sprintf("source-%06d.go", i)
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       uint64(i + 10),
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
		})
	}
	buildOrders(idx)
	return idx
}

func BenchmarkDerivedSectionStreaming(b *testing.B) {
	idx := syntheticCompactIndex(20_000)
	nameTokens := make([]string, len(idx.Records))
	for i := range idx.Records {
		nameTokens[i] = idx.Records[i].Name
	}
	maxSectionBytes := 0
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		maxSectionBytes = 0
		_, err := writeDerivedSectionStreamObserved(&countingWriter{w: io.Discard}, idx, nameTokens, func(inFlight int) {
			if inFlight > maxSectionBytes {
				maxSectionBytes = inFlight
			}
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(maxSectionBytes), "max_section_bytes")
}

func TestDerivedSectionLifetimeBound(t *testing.T) {
	idx := syntheticCompactIndex(20_000)
	nameTokens := make([]string, len(idx.Records))
	for i := range idx.Records {
		nameTokens[i] = idx.Records[i].Name
	}
	retained := buildDerivedSectionBlobs(idx, nameTokens)
	retainedBytes := 0
	for _, section := range retained {
		retainedBytes += len(section.data)
	}
	maxInFlight := 0
	if _, err := writeDerivedSectionStreamObserved(&countingWriter{w: io.Discard}, idx, nameTokens, func(inFlight int) {
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
	}); err != nil {
		t.Fatal(err)
	}
	if maxInFlight <= 0 || maxInFlight >= retainedBytes {
		t.Fatalf("derived section lifetime max=%d retained=%d; want one-section streaming bound", maxInFlight, retainedBytes)
	}
	t.Logf("derived section bytes retained-all=%d streamed-max=%d", retainedBytes, maxInFlight)
}

func TestSubtreeSectionStreamingGoldenAndScratchCap(t *testing.T) {
	if subtreeSectionScratchBytes > 64*1024 {
		t.Fatalf("SUBT scratch cap=%d; want <= 65536", subtreeSectionScratchBytes)
	}
	idx := syntheticCompactIndex(20_000)
	nameTokens := make([]string, len(idx.Records))
	for i := range idx.Records {
		nameTokens[i] = idx.Records[i].Name
	}
	var compared bool
	err := forEachDerivedSection(idx, nameTokens, func(section indexSectionBlob) error {
		if section.tag != indexSectionSUBT || section.subtree == nil {
			return nil
		}
		legacy := encodeUint32Section(section.subtree.parts...)
		var streamed bytes.Buffer
		written, err := writeSubtreeSection(&streamed, section.subtree)
		if err != nil {
			return err
		}
		if written != int64(len(legacy)) || !bytes.Equal(streamed.Bytes(), legacy) {
			return fmt.Errorf("streamed SUBT differs: streamed=%d legacy=%d", written, len(legacy))
		}
		if sha256.Sum256(streamed.Bytes()) != sha256.Sum256(legacy) {
			return errors.New("streamed SUBT hash differs from legacy encoding")
		}
		compared = true
		t.Logf("SUBT encoded_bytes=%d scratch_cap=%d sha256=%x", written, subtreeSectionScratchBytes, sha256.Sum256(streamed.Bytes()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !compared {
		t.Fatal("staged derived sections did not emit SUBT")
	}
}

type partialWriteErrorWriter struct {
	limit int
	wrote int
}

func (w *partialWriteErrorWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.wrote
	if remaining <= 0 {
		return 0, errors.New("injected subtree write failure")
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	w.wrote += len(p)
	if w.wrote >= w.limit {
		return len(p), errors.New("injected subtree write failure")
	}
	return len(p), nil
}

func TestSubtreeSectionWriteFailureReportsPartialWrite(t *testing.T) {
	section := &subtreeSectionBlob{parts: [][]uint32{make([]uint32, 10_000)}}
	w := &partialWriteErrorWriter{limit: 97}
	written, err := writeSubtreeSection(w, section)
	if err == nil {
		t.Fatal("writeSubtreeSection unexpectedly succeeded after injected partial write")
	}
	if written != int64(w.wrote) || written <= 0 || written >= int64(4+len(section.parts[0])*4) {
		t.Fatalf("partial SUBT write=%d writer=%d; want bounded partial output", written, w.wrote)
	}
}

func TestSaveIndexFailureDoesNotReplaceSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atomic.gsi")
	valid := syntheticCompactIndex(32)
	if err := saveIndex(path, valid); err != nil {
		t.Fatalf("save valid source: %v", err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := cloneCompactIndex(valid)
	invalid.Records[1].Parent = int32(compactNarrowParentSentinel)
	if err := saveIndex(path, invalid); err == nil {
		t.Fatal("save invalid index unexpectedly succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("partial failed write replaced the existing source index")
	}
	tmp, err := filepath.Glob(filepath.Join(dir, "atomic.gsi.*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmp) != 0 {
		t.Fatalf("failed save left temporary outputs: %v", tmp)
	}
}

func TestPersistencePreparationStagesBoundLiveHeap(t *testing.T) {
	idx := syntheticCompactIndex(100_000)
	nameTokens := make([]string, len(idx.Records))
	for i := range idx.Records {
		nameTokens[i] = idx.Records[i].Name
	}
	stages := make(map[string]uint64)
	persistStageObserver = func(stage string, _ runtime.MemStats) {
		runtime.GC()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		if mem.HeapAlloc > stages[stage] {
			stages[stage] = mem.HeapAlloc
		}
	}
	defer func() { persistStageObserver = nil }()
	t.Setenv("SEEKFS_PERSIST_TRACE", "1")
	runtime.GC()
	if _, err := writeDerivedSectionStreamObserved(&countingWriter{w: io.Discard}, idx, nameTokens, nil); err != nil {
		t.Fatal(err)
	}
	peak := uint64(0)
	for _, bytes := range stages {
		if bytes > peak {
			peak = bytes
		}
	}
	for _, stage := range []string{"resident-prepared", "name-rank-ready", "extension-rank-ready", "derived-prepared"} {
		t.Logf("stage=%s heap=%d", stage, stages[stage])
	}
	t.Logf("bounded preparation heap peak=%d", peak)
	if stages["derived-prepared"] == 0 {
		t.Fatal("derived-prepared stage was not observed")
	}
}

func TestPersistenceBoundedChildMode(t *testing.T) {
	mode, _ := envFirst("SEEKFS_PERSIST_MODE", "SEEKFS_V9_PERSIST_MODE")
	if mode != "staged" {
		t.Skip("set SEEKFS_PERSIST_MODE=staged for child-process measurement")
	}
	idx := syntheticCompactIndex(100_000)
	nameTokens := make([]string, len(idx.Records))
	for i := range idx.Records {
		nameTokens[i] = idx.Records[i].Name
	}
	runtime.GC()
	start := time.Now()
	if _, err := writeDerivedSectionStreamObserved(&countingWriter{w: io.Discard}, idx, nameTokens, nil); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	t.Logf("bounded child mode=%s duration=%s heap_alloc=%d heap_inuse=%d", mode, time.Since(start), mem.HeapAlloc, mem.HeapInuse)
}

func commonSearchFixture() *Index {
	idx := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
	}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) {
		// Give files a non-zero size so the index advertises size capability;
		// directories keep size 0. The exact value scales with the record index
		// so size: filters have something to discriminate on.
		size := int64(0)
		if mode&uint32(os.ModeDir) == 0 {
			size = int64(len(idx.Records)+1) * 1024
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      name,
			Mode:      mode,
			Size:      size,
			ModUnix:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		})
	}
	add(1, 1, -1, ".", uint32(os.ModeDir))
	add(2, 1, 0, "fixture", uint32(os.ModeDir))
	add(3, 2, 1, "workspace", uint32(os.ModeDir))
	add(4, 3, 2, "Assets", uint32(os.ModeDir))
	add(5, 4, 3, "sample.dat", 0)
	add(6, 4, 3, "notes.txt", 0)
	add(7, 3, 2, "src", uint32(os.ModeDir))
	add(8, 7, 6, "main.go", 0)
	add(9, 7, 6, "search_test.go", 0)
	add(10, 3, 2, "README.md", 0)
	add(11, 3, 2, "readme.md", 0)
	add(12, 3, 2, "project-data-worktree", uint32(os.ModeDir))
	add(13, 12, 11, "volume.bin", 0)
	add(14, 3, 2, "archive.tar.gz", 0)
	add(15, 3, 2, "Downstream", uint32(os.ModeDir))
	add(16, 15, 14, "sibling.dat", 0)
	buildOrders(idx)
	return idx
}

func namesOf(entries []Entry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	return names
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameIntSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	a = append([]int(nil), a...)
	b = append([]int(nil), b...)
	sort.Ints(a)
	sort.Ints(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
