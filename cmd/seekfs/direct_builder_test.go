package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDirectBuilderIsSourceOrderIndependentAndResolvesParents(t *testing.T) {
	records := []directRecord{
		{FRN: 40, ParentFRN: 99, Name: "orphan.txt"},
		{FRN: 30, ParentFRN: 20, Name: "child.txt"},
		{FRN: 10, Name: "root"},
		{FRN: 20, ParentFRN: 10, Name: "parent"},
	}
	build := func(name string, source []directRecord, runRecords int) (string, directBuildStats) {
		dir := t.TempDir()
		path := filepath.Join(dir, name+".gsi")
		stats, err := buildDirect(context.Background(), directBuildOptions{
			OutputPath: path,
			SpoolDir:   filepath.Join(dir, "spool"),
			Roots:      []string{"X:\\"},
			Volume:     "X:",
			Source:     "direct-test",
			BuiltAt:    time.Unix(123, 0),
			Records:    newDirectSliceSource(source),
			RunRecords: runRecords,
			RunBytes:   4096,
		})
		if err != nil {
			t.Fatal(err)
		}
		return path, stats
	}
	first, firstStats := build("first", records, 2)
	secondRecords := []directRecord{records[2], records[0], records[3], records[1]}
	second, secondStats := build("second", secondRecords, 3)
	firstHash := directFileHash(t, first)
	secondHash := directFileHash(t, second)
	if firstHash != secondHash {
		t.Fatalf("source order changed direct output: %s != %s", firstHash, secondHash)
	}
	if firstStats.FinalIDRule != "ascending-frn; duplicate-frn-rejected" || len(secondStats.Sections) != 17 || secondStats.Sections[0] != "RANK" {
		t.Fatalf("unexpected build stats: %#v / %#v", firstStats, secondStats)
	}
	idx, err := loadIndexMMap(first)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })
	if idx.Version != indexVersion || idx.compactRecordCount() != 4 {
		t.Fatalf("loaded direct index = version %d records %d", idx.Version, idx.compactRecordCount())
	}
	wantParents := []int32{-1, 0, 1, -1}
	for id, want := range wantParents {
		if got := idx.compactRecord(id).Parent; got != want {
			t.Fatalf("record %d parent=%d, want %d", id, got, want)
		}
	}
	if len(idx.Derived.NameOrder) != 4 || len(idx.Derived.NameRank) != 4 || len(idx.Derived.SizeOrder) != 4 || len(idx.Derived.ModOrder) != 4 || len(idx.Derived.ExtOrder) != 4 || len(idx.Derived.TypeOrder) != 4 || len(idx.Derived.PathOrder) != 4 {
		t.Fatalf("rank families not decoded: name=%d size=%d mod=%d ext=%d type=%d path=%d", len(idx.Derived.NameOrder), len(idx.Derived.SizeOrder), len(idx.Derived.ModOrder), len(idx.Derived.ExtOrder), len(idx.Derived.TypeOrder), len(idx.Derived.PathOrder))
	}
	if got, want := idx.Derived.ChildOffsets, []uint32{0, 1, 2, 2, 2}; !equalUint32s(got, want) {
		t.Fatalf("child offsets=%v want=%v", got, want)
	}
	if got, want := idx.Derived.ChildIDs, []uint32{1, 2}; !equalUint32s(got, want) {
		t.Fatalf("child IDs=%v want=%v", got, want)
	}
	if got, want := idx.Derived.RootIDs, []uint32{0, 3}; !equalUint32s(got, want) {
		t.Fatalf("root IDs=%v want=%v", got, want)
	}
	if got, want := idx.Derived.FRNs, []uint64{10, 20, 30, 40}; !equalUint64s(got, want) || !equalUint32s(idx.Derived.FRNRecordIDs, []uint32{0, 1, 2, 3}) {
		t.Fatalf("FRNS mismatch frns=%v ids=%v", got, idx.Derived.FRNRecordIDs)
	}
}

func TestDirectTopologyRejectsParentCycleAndCleans(t *testing.T) {
	dir := t.TempDir()
	_, err := buildDirect(context.Background(), directBuildOptions{
		OutputPath: filepath.Join(dir, "cycle.gsi"),
		SpoolDir:   filepath.Join(dir, "spool"),
		Records: newDirectSliceSource([]directRecord{
			{FRN: 1, ParentFRN: 2, Name: "a"},
			{FRN: 2, ParentFRN: 1, Name: "b"},
		}),
		RunRecords: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "parent cycle") {
		t.Fatalf("cycle error=%v", err)
	}
	if leftovers, globErr := filepath.Glob(filepath.Join(dir, "spool", "direct-*.tmp")); globErr != nil || len(leftovers) != 0 {
		t.Fatalf("cycle scratch survived: %v (%v)", leftovers, globErr)
	}
}

func equalUint32s(a, b []uint32) bool {
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

func equalUint64s(a, b []uint64) bool {
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

func TestDirectRankFamiliesMatchComparator(t *testing.T) {
	records := []directRecord{
		{FRN: 40, Name: "zeta.TXT", Path: `X:\zeta.TXT`, Size: 0, ModUnix: 0},
		{FRN: 10, Name: "dir", Path: `X:\dir`, Mode: uint32(os.ModeDir), Size: 0, ModUnix: 20},
		{FRN: 30, Name: "a.go", Path: `X:\dir\a.go`, Size: 5, ModUnix: 10},
		{FRN: 20, Name: ".profile", Path: `X:\.profile`, Size: 2, ModUnix: 30},
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "ranks.gsi")
	stats, err := buildDirect(context.Background(), directBuildOptions{OutputPath: out, SpoolDir: filepath.Join(dir, "spool"), Records: newDirectSliceSource(records), RunRecords: 2, RunBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.RankFamilies) != 6 {
		t.Fatalf("rank family reports=%d: %#v", len(stats.RankFamilies), stats.RankFamilies)
	}
	idx, err := loadIndexMMap(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })
	ordered := append([]directRecord(nil), records...)
	sort.Slice(ordered, func(i, j int) bool { return directRecordLess(ordered[i], ordered[j]) })
	for _, spec := range directRankSpecs() {
		want := make([]uint32, len(ordered))
		for i := range want {
			want[i] = uint32(i)
		}
		sort.Slice(want, func(i, j int) bool {
			ki, kj := spec.Key(ordered[want[i]]), spec.Key(ordered[want[j]])
			if ki != kj {
				return ki < kj
			}
			return want[i] < want[j]
		})
		gotOrder, gotRank := directDerivedRank(idx, spec.Tag)
		if len(gotOrder) != len(want) || len(gotRank) != len(want) {
			t.Fatalf("%s lengths=%d/%d want=%d", spec.Name, len(gotOrder), len(gotRank), len(want))
		}
		for pos, id := range want {
			if gotOrder[pos] != id || gotRank[id] != uint32(pos) {
				t.Fatalf("%s mismatch at pos=%d got=%d/%d want=%d", spec.Name, pos, gotOrder[pos], gotRank[id], id)
			}
		}
	}
}

// Rank sections persist the order alone; the rank-by-id vector is rebuilt
// at load as its exact inverse. Both on-disk forms decode identically.
func TestDecodeRankSectionCompactAndLegacy(t *testing.T) {
	order := []uint32{3, 1, 0, 2}
	legacy := encodeUint32Section(order, []uint32{2, 1, 3, 0})
	compact := encodeUint32Section(order)
	gotOrder, gotRank := decodeRankSection(legacy, 4)
	if !equalUint32s(gotOrder, order) || !equalUint32s(gotRank, []uint32{2, 1, 3, 0}) {
		t.Fatalf("legacy decode = %v/%v", gotOrder, gotRank)
	}
	gotOrder, gotRank = decodeRankSection(compact, 4)
	if !equalUint32s(gotOrder, order) || !equalUint32s(gotRank, []uint32{2, 1, 3, 0}) {
		t.Fatalf("compact decode = %v/%v; want order %v with inverted rank", gotOrder, gotRank, order)
	}
	// Compact form is one part (4 + 4/live entries), not two.
	if parts := directTestUint32Parts(t, compact); len(parts) != 1 {
		t.Fatalf("compact section has %d parts; want 1", len(parts))
	}
	// Corrupt sections yield nils, never a panic or a wrong rank.
	if o, r := decodeRankSection([]byte{9, 0, 0, 0, 1, 2, 3}, 4); o != nil || r != nil {
		t.Fatal("truncated section must decode to nils")
	}
	badOrder := append([]uint32(nil), order...)
	badOrder[0] = 99
	if o, r := decodeRankSection(encodeUint32Section(badOrder), 4); o != nil || r != nil {
		t.Fatal("out-of-range order value must decode to nils")
	}
	// Deleted records keep rank 0: the order covers only live IDs.
	gotOrder, gotRank = decodeRankSection(encodeUint32Section([]uint32{2, 0}), 3)
	if !equalUint32s(gotOrder, []uint32{2, 0}) || !equalUint32s(gotRank, []uint32{1, 0, 0}) {
		t.Fatalf("sparse decode = %v/%v; want [2 0]/[1 0 0]", gotOrder, gotRank)
	}
}

// The direct builder writes order-only rank sections while the loaded index
// still exposes both arrays with the exact inverse relationship.
func TestDirectRankSectionsPersistOrderOnly(t *testing.T) {
	records := []directRecord{
		{FRN: 40, Name: "zeta.TXT", Path: `X:\zeta.TXT`, Size: 0, ModUnix: 0},
		{FRN: 10, Name: "dir", Path: `X:\dir`, Mode: uint32(os.ModeDir), Size: 0, ModUnix: 20},
		{FRN: 30, Name: "a.go", Path: `X:\dir\a.go`, Size: 5, ModUnix: 10},
		{FRN: 20, Name: ".profile", Path: `X:\.profile`, Size: 2, ModUnix: 30},
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "ranks-compact.gsi")
	if _, err := buildDirect(context.Background(), directBuildOptions{OutputPath: out, SpoolDir: filepath.Join(dir, "spool"), Records: newDirectSliceSource(records), RunRecords: 2, RunBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []uint32{indexSectionRANK, indexSectionSRNK, indexSectionMRNK, indexSectionERNK, indexSectionTRNK, indexSectionPRNK} {
		data := directTestSectionData(t, out, tag)
		parts := directTestUint32Parts(t, data)
		if len(parts) != 1 || len(parts[0]) != len(records) {
			t.Fatalf("section %08x has %d parts x %d entries; want 1 x %d", tag, len(parts), len(parts[0]), len(records))
		}
	}
	idx, err := loadIndexMMap(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })
	for _, tc := range []struct {
		name  string
		order []uint32
		rank  []uint32
	}{
		{"RANK", idx.Derived.NameOrder, idx.Derived.NameRank},
		{"SRNK", idx.Derived.SizeOrder, idx.Derived.SizeRank},
		{"MRNK", idx.Derived.ModOrder, idx.Derived.ModRank},
		{"ERNK", idx.Derived.ExtOrder, idx.Derived.ExtRank},
		{"TRNK", idx.Derived.TypeOrder, idx.Derived.TypeRank},
		{"PRNK", idx.Derived.PathOrder, idx.Derived.PathRank},
	} {
		if len(tc.order) != len(records) || len(tc.rank) != len(records) {
			t.Fatalf("%s resident lengths=%d/%d want=%d", tc.name, len(tc.order), len(tc.rank), len(records))
		}
		if !equalUint32s(tc.rank, invertRankOrder(tc.order, len(records))) {
			t.Fatalf("%s resident rank is not the inverse of the order", tc.name)
		}
	}
}

func TestDirectRankWorkerCountsPreserveOutput(t *testing.T) {
	records := make([]directRecord, 0, 256)
	for i := 0; i < 256; i++ {
		records = append(records, directSyntheticRecord(i))
	}
	var wantHash string
	for _, workers := range []int{1, 4, 8, 16} {
		dir := t.TempDir()
		out := filepath.Join(dir, fmt.Sprintf("workers-%d.gsi", workers))
		stats, err := buildDirect(context.Background(), directBuildOptions{
			OutputPath:  out,
			SpoolDir:    filepath.Join(dir, "spool"),
			Records:     newDirectSliceSource(records),
			RunRecords:  32,
			RankWorkers: workers,
		})
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		gotHash := directFileHash(t, out)
		if workers == 1 {
			wantHash = gotHash
		} else if gotHash != wantHash {
			t.Fatalf("workers=%d output hash=%s want=%s stats=%+v", workers, gotHash, wantHash, stats)
		}
	}
}

func TestDirectSharedRankPassCancellationCleansOwnedRuns(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "records.final.tmp")
	f, err := os.Create(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriter(f)
	if _, err := writeDirectSpoolRecord(bw, directSyntheticRecord(0)); err != nil {
		t.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	owned := make([]string, 0)
	_, err = directBuildRankRunsShared(ctx, finalPath, dir, 32, 4, directRankSpecs(), &owned)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shared rank cancellation error=%v", err)
	}
	directRemoveOwned(owned)
	if leftovers, globErr := filepath.Glob(filepath.Join(dir, "direct-rank-*.tmp")); globErr != nil || len(leftovers) != 0 {
		t.Fatalf("shared rank cancellation leftovers=%v err=%v", leftovers, globErr)
	}
}

func directDerivedRank(idx *Index, tag uint32) ([]uint32, []uint32) {
	switch tag {
	case indexSectionRANK:
		return idx.Derived.NameOrder, idx.Derived.NameRank
	case indexSectionSRNK:
		return idx.Derived.SizeOrder, idx.Derived.SizeRank
	case indexSectionMRNK:
		return idx.Derived.ModOrder, idx.Derived.ModRank
	case indexSectionERNK:
		return idx.Derived.ExtOrder, idx.Derived.ExtRank
	case indexSectionTRNK:
		return idx.Derived.TypeOrder, idx.Derived.TypeRank
	case indexSectionPRNK:
		return idx.Derived.PathOrder, idx.Derived.PathRank
	default:
		return nil, nil
	}
}

func TestDirectBuilderRejectsDuplicateFRNAndCleansOwnedScratch(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "duplicate.gsi")
	_, err := buildDirect(context.Background(), directBuildOptions{
		OutputPath: out,
		SpoolDir:   filepath.Join(dir, "owned-spool"),
		BuiltAt:    time.Unix(123, 0),
		Records: newDirectSliceSource([]directRecord{
			{FRN: 1, Name: "a"},
			{FRN: 1, Name: "b"},
		}),
		RunRecords: 1,
	})
	if !errors.Is(err, errDirectDuplicateFRN) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("duplicate build left output: %v", statErr)
	}
	if leftovers, globErr := filepath.Glob(filepath.Join(dir, "owned-spool", "direct-*.tmp")); globErr != nil || len(leftovers) != 0 {
		t.Fatalf("owned scratch survived cleanup: %v (%v)", leftovers, globErr)
	}
}

func TestDirectMFTAndUSNAdaptersAreElevationFree(t *testing.T) {
	mft := newDirectMFTSource(map[uint64]mftEntry{
		20: {frn: 20, parentFRN: 10, name: "child.bin", size: 7},
		10: {frn: 10, name: "root", attr: fileAttributeDir, isDir: true},
	})
	first, err := mft.Next(context.Background())
	if err != nil || first.FRN != 10 || first.Name != "root" {
		t.Fatalf("MFT adapter first=%+v err=%v", first, err)
	}
	second, err := mft.Next(context.Background())
	if err != nil || second.FRN != 20 || second.ParentFRN != 10 || second.Size != 7 {
		t.Fatalf("MFT adapter second=%+v err=%v", second, err)
	}
}

func TestDirectMFTSourceNormalizesRootSelfParent(t *testing.T) {
	mft := newDirectMFTSource(map[uint64]mftEntry{
		5:  {frn: 5, parentFRN: 5, name: "root", attr: fileAttributeDir, isDir: true},
		10: {frn: 10, parentFRN: 5, name: "child.bin", size: 3},
	})
	root, err := mft.Next(context.Background())
	if err != nil || root.FRN != 5 || root.ParentFRN != 0 {
		t.Fatalf("MFT root self-parent not normalized: root=%+v err=%v", root, err)
	}
	child, err := mft.Next(context.Background())
	if err != nil || child.FRN != 10 || child.ParentFRN != 5 {
		t.Fatalf("MFT child=%+v err=%v", child, err)
	}
}

func TestDirectUSNAdapterIsElevationFree(t *testing.T) {
	usn := newDirectUSNSource(map[uint64]usnNode{
		2: {frn: 2, parentFRN: 1, name: "child.bin"},
		1: {frn: 1, name: "root", attr: fileAttributeDir},
	})
	first, err := usn.Next(context.Background())
	if err != nil || first.FRN != 1 || first.Mode&uint32(os.ModeDir) == 0 {
		t.Fatalf("USN adapter first=%+v err=%v", first, err)
	}
	second, err := usn.Next(context.Background())
	if err != nil || second.FRN != 2 || second.ParentFRN != 1 {
		t.Fatalf("USN adapter second=%+v err=%v", second, err)
	}
}

func TestDirectWalkSourceFeedsTheSameBuilder(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "beta.bin"), []byte("beta"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := newDirectWalkSource(root)
	if err != nil {
		t.Fatal(err)
	}
	defer source.(*directWalkSource).Close()
	dir := t.TempDir()
	out := filepath.Join(dir, "walk.gsi")
	stats, err := buildDirect(context.Background(), directBuildOptions{
		OutputPath: out,
		SpoolDir:   filepath.Join(dir, "spool"),
		Roots:      []string{root},
		Source:     "direct-walk",
		BuiltAt:    time.Unix(123, 0),
		Records:    source,
		RunRecords: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 4 || stats.Sections[0] != "RANK" {
		t.Fatalf("walk build stats=%+v", stats)
	}
	idx, err := loadIndexMMap(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })
	if idx.compactRecordCount() != 4 || idx.Source != "direct-walk" {
		t.Fatalf("walk index records/source=%d/%q", idx.compactRecordCount(), idx.Source)
	}
}

func TestDirectWalkSourceExcludesOwnedArtifactsBeforeTraversal(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "owned-run")
	if err := os.MkdirAll(filepath.Join(root, "included"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(owned, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(root, "included", "ok.txt"): "ok",
		filepath.Join(root, "ignored.gsi"):        "database",
		filepath.Join(owned, "nested", "new.tmp"): "builder output",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	report := &directWalkReport{}
	source, err := newDirectWalkSourceWithExclusions(root, []string{owned}, []string{".gsi"}, report)
	if err != nil {
		t.Fatal(err)
	}
	walk := source.(*directWalkSource)
	defer walk.Close()
	count := 0
	for {
		_, nextErr := source.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		count++
	}
	// Root, included directory, and included file are the only records.
	if count != 3 || report.Excluded < 2 || !report.SourceComplete {
		t.Fatalf("walk count/report=%d/%+v", count, report)
	}
}

func TestDirectConcurrentWalkBuilderOutputIsWorkerIndependent(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 32; i++ {
		dir := filepath.Join(root, fmt.Sprintf("d-%02d", i%4))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%02d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var want string
	for _, workers := range []int{1, 4, 8, 16} {
		dir := t.TempDir()
		report := &directWalkReport{}
		source, err := newDirectConcurrentWalkSourceWithOptions(root, nil, nil, report, directConcurrentWalkOptions{Workers: workers, Queue: workers * 2})
		if err != nil {
			t.Fatal(err)
		}
		stats, err := buildDirect(context.Background(), directBuildOptions{
			OutputPath:  filepath.Join(dir, "walk.gsi"),
			SpoolDir:    filepath.Join(dir, "spool"),
			Records:     source,
			RunRecords:  8,
			RankWorkers: workers,
			WalkReport:  report,
			Source:      "direct-walk",
		})
		if err != nil {
			t.Fatal(err)
		}
		got := directFileHash(t, filepath.Join(dir, "walk.gsi"))
		if workers == 1 {
			want = got
		} else if got != want {
			idx, loadErr := loadIndexMMap(filepath.Join(dir, "walk.gsi"))
			if loadErr != nil {
				t.Fatalf("workers=%d hash=%s want=%s load=%v stats=%+v", workers, got, want, loadErr, stats)
			}
			idxHash := directOrderHash(idx.Derived.NameOrder)
			_ = idx.MMapRecords.file.close()
			t.Fatalf("workers=%d hash=%s want=%s order_hash=%s stats=%+v", workers, got, want, idxHash, stats)
		}
	}
}

func TestDirectWalkPreflightUsesStableArtifactRootAcrossFreshRuns(t *testing.T) {
	// The preflight canonicalizes paths (EvalSymlinks), so compare in canonical
	// space; a symlinked or short-named temp root would otherwise alias.
	root, err := directCanonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(root, ".r5tmp")
	if err := os.MkdirAll(stable, 0o700); err != nil {
		t.Fatal(err)
	}
	runOne := filepath.Join(stable, "run-a-7f6e")
	runTwo := filepath.Join(stable, "run-b-92c1")
	for _, run := range []string{runOne, runTwo} {
		if err := os.MkdirAll(filepath.Join(run, "spool"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	preOne, err := directWalkPreflightFor(root, filepath.Join(runOne, "one.gsi"), filepath.Join(runOne, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	preTwo, err := directWalkPreflightFor(root, filepath.Join(runTwo, "two.gsi"), filepath.Join(runTwo, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	if !directPathUnderAny(preOne.Target, preOne.ExclusionRoots) || !directPathUnderAny(preOne.Spool, preOne.ExclusionRoots) ||
		!directPathUnderAny(preTwo.Target, preTwo.ExclusionRoots) || !directPathUnderAny(preTwo.Spool, preTwo.ExclusionRoots) {
		t.Fatalf("target/spool escaped stable exclusion: one=%+v two=%+v", preOne, preTwo)
	}
	if !directPathUnderAny(stable, preTwo.ExclusionRoots) || directPathUnderAny(root, preTwo.ExclusionRoots) {
		t.Fatalf("stable root/source alias check failed: %+v", preTwo.ExclusionRoots)
	}
	if directPathUnderAny(preTwo.Target, []string{runOne}) {
		t.Fatalf("fresh run target was covered only by stale prior-run root")
	}
	if !directPathUnderAny(preTwo.Target, preOne.ExclusionRoots) {
		t.Fatalf("stable root should cover a future run without stale variables")
	}
}

func TestDirectBuilderCancellationDoesNotPublishTarget(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := buildDirect(ctx, directBuildOptions{
		OutputPath: filepath.Join(dir, "cancelled.gsi"),
		SpoolDir:   filepath.Join(dir, "owned-spool"),
		Records:    newDirectSliceSource([]directRecord{{FRN: 1, Name: "a"}}),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "cancelled.gsi")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled build published target: %v", statErr)
	}
}

func TestDirectInaccessibleBoundDegradesWithinLimit(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "degraded.gsi")
	report := &directWalkReport{SourceComplete: false}
	report.note("inaccessible", "X:\\protected\\dir-a")
	report.note("inaccessible", "X:\\protected\\dir-b")
	stats, err := buildDirect(context.Background(), directBuildOptions{
		OutputPath:      out,
		SpoolDir:        filepath.Join(dir, "spool"),
		Roots:           []string{`X:\`},
		Volume:          "X:",
		Source:          "direct-walk",
		BuiltAt:         time.Unix(0, 0),
		Records:         newDirectSliceSource([]directRecord{{FRN: 1, Name: "."}}),
		WalkReport:      report,
		MaxInaccessible: 64,
	})
	if err != nil {
		t.Fatalf("bounded inaccessible build failed: %v", err)
	}
	if !stats.SourceDegraded {
		t.Fatalf("bounded inaccessible build did not report degraded: %+v", stats)
	}
	if stats.SourceComplete {
		t.Fatalf("degraded build must not report source complete")
	}
	if stats.SourceInaccessible != 2 {
		t.Fatalf("source inaccessible = %d, want 2", stats.SourceInaccessible)
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Fatalf("degraded build did not publish target: %v", statErr)
	}
}

func TestDirectInaccessibleBoundRefusesAboveLimit(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "refused.gsi")
	report := &directWalkReport{SourceComplete: false, Inaccessible: 2}
	_, err := buildDirect(context.Background(), directBuildOptions{
		OutputPath:      out,
		SpoolDir:        filepath.Join(dir, "spool"),
		Roots:           []string{`X:\`},
		Volume:          "X:",
		Source:          "direct-walk",
		BuiltAt:         time.Unix(0, 0),
		Records:         newDirectSliceSource([]directRecord{{FRN: 1, Name: "."}}),
		WalkReport:      report,
		MaxInaccessible: 1,
	})
	if err == nil {
		t.Fatal("build with inaccessible above bound must fail")
	}
	if !strings.Contains(err.Error(), "source incomplete") {
		t.Fatalf("error = %v, want source-incomplete refusal", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused build published target: %v", statErr)
	}
}

func TestDirectInaccessibleZeroRequiresCleanSource(t *testing.T) {
	dir := t.TempDir()
	report := &directWalkReport{SourceComplete: true, Inaccessible: 0}
	out := filepath.Join(dir, "clean.gsi")
	if _, err := buildDirect(context.Background(), directBuildOptions{
		OutputPath:      out,
		SpoolDir:        filepath.Join(dir, "spool"),
		Roots:           []string{`X:\`},
		Volume:          "X:",
		Source:          "direct-walk",
		BuiltAt:         time.Unix(0, 0),
		Records:         newDirectSliceSource([]directRecord{{FRN: 1, Name: "."}}),
		WalkReport:      report,
		MaxInaccessible: 0,
	}); err != nil {
		t.Fatalf("clean build failed: %v", err)
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Fatalf("clean build did not publish target: %v", statErr)
	}
}

func TestDirectCanonicalDerivedParity(t *testing.T) {
	t.Setenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING", "2")
	t.Setenv("SEEKFS_SELF_NAME_GRAMS", "1")
	records := []directRecord{
		{FRN: 1, Name: ".", Path: `X:\`, Mode: uint32(os.ModeDir)},
		{FRN: 2, ParentFRN: 1, Name: "common", Path: `X:\common`, Mode: uint32(os.ModeDir)},
		{FRN: 3, ParentFRN: 1, Name: "rare", Path: `X:\rare`, Mode: uint32(os.ModeDir)},
		{FRN: 4, ParentFRN: 2, Name: "common-alpha-nrrd.txt", Path: `X:\common\common-alpha-nrrd.txt`, Size: 30, ModUnix: 300},
		{FRN: 5, ParentFRN: 2, Name: "common-beta.txt", Path: `X:\common\common-beta.txt`, Size: 10, ModUnix: 100},
		{FRN: 6, ParentFRN: 2, Name: "common-gamma.go", Path: `X:\common\common-gamma.go`, Size: 20, ModUnix: 200},
		{FRN: 7, ParentFRN: 3, Name: "zqx-rare-nrrd.bin", Path: `X:\rare\zqx-rare-nrrd.bin`, Size: 40, ModUnix: 400},
		{FRN: 8, ParentFRN: 3, Name: "neutral.txt", Path: `X:\rare\neutral.txt`, Size: 50, ModUnix: 500},
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "canonical-parity.gsi")
	if _, err := buildDirect(context.Background(), directBuildOptions{
		OutputPath: out,
		SpoolDir:   filepath.Join(dir, "spool"),
		Roots:      []string{`X:\`},
		Volume:     "X:",
		Source:     "usn",
		BuiltAt:    time.Unix(123, 0),
		Records:    newDirectSliceSource(records),
		RunRecords: 2,
		RunBytes:   4096,
	}); err != nil {
		t.Fatal(err)
	}
	idx, err := loadIndexMMap(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })
	sizeRank := append([]uint32(nil), idx.Derived.SizeRank...)
	modRank := append([]uint32(nil), idx.Derived.ModRank...)
	extRank := append([]uint32(nil), idx.Derived.ExtRank...)
	typeRank := append([]uint32(nil), idx.Derived.TypeRank...)
	pathRank := append([]uint32(nil), idx.Derived.PathRank...)
	vol := newServiceVolumeIndex(out, idx)

	subt := directTestSectionData(t, out, indexSectionSUBT)
	if parts := directTestUint32Parts(t, subt); len(parts) != 8 {
		t.Fatalf("SUBT parts=%d, want canonical 8", len(parts))
	}
	for _, tc := range []struct {
		name string
		got  []uint32
		rank []uint32
	}{
		{name: "size", got: vol.subtreeSizeRank, rank: sizeRank},
		{name: "modified", got: vol.subtreeModRank, rank: modRank},
		{name: "extension", got: vol.subtreeExtRank, rank: extRank},
		{name: "type", got: vol.subtreeTypeRank, rank: typeRank},
		{name: "path", got: vol.subtreePathRank, rank: pathRank},
	} {
		if want := vol.buildSubtreeMinRanks(tc.rank); !equalUint32s(tc.got, want) {
			t.Fatalf("SUBT %s minima=%v, want %v", tc.name, tc.got, want)
		}
	}

	pcmp := idx.Derived.Postings[indexSectionPCMP]
	componentPostings := map[string][]uint32{"common": {1}, "rare": {2}}
	for key, want := range componentPostings {
		if got := pcmp.stringPosting(key); !equalUint32s(got, want) {
			t.Fatalf("PCMP %q=%v, want own-directory roots %v", key, got, want)
		}
	}
	pcmpData := directTestSectionData(t, out, indexSectionPCMP)
	firstBlock, blockCount := directTestStringPostingBlocks(t, pcmpData, "common")
	if blockCount != 1 {
		t.Fatalf("PCMP common blocks=%d, want 1", blockCount)
	}
	if got, want := directTestBlockMinRank(pcmpData, firstBlock), vol.queryIndex.nameRank[1]; got != want {
		t.Fatalf("PCMP common minRank=%d, want name rank %d", got, want)
	}
	if got, want := pcmp.RankBounds, buildComponentPostingRankBounds(componentPostings, vol); !reflect.DeepEqual(got, want) {
		t.Fatalf("PXRC bounds=%+v, want canonical descendant bounds %+v", got, want)
	}

	commonGram, rareGram := trigramKey("com"), trigramKey("zqx")
	if got := idx.Derived.NameTrigrams.countForGram(commonGram); got != 4 {
		t.Fatalf("PNGR common count=%d, want omitted count 4", got)
	}
	if _, omitted := idx.Derived.NameTrigrams.omitted[commonGram]; !omitted {
		t.Fatal("PNGR did not mark common filename gram omitted")
	}
	if got := idx.Derived.NameTrigrams.countForGram(rareGram); got != 1 {
		t.Fatalf("PNGR rare count=%d, want stored count 1", got)
	}
	if idx.Derived.SelfNameTrigrams == nil || !idx.Derived.SelfNameTrigrams.gramUnionComplete {
		t.Fatal("PNGC missing complete-union metadata")
	}
	if got := idx.Derived.SelfNameTrigrams.countForGram(commonGram); got != 4 {
		t.Fatalf("PNGC common count=%d, want omitted companion count 4", got)
	}
	if got := idx.Derived.SelfNameTrigrams.countForGram(rareGram); got != 0 {
		t.Fatalf("PNGC rare count=%d, want selective PNGR to own it", got)
	}
	pngcData := directTestSectionData(t, out, indexSectionPNGC)
	firstBlock, blockCount = directTestGramPostingBlocks(t, pngcData, commonGram)
	if blockCount != 1 {
		t.Fatalf("PNGC common blocks=%d, want 1", blockCount)
	}
	wantCommonRank := minRankForIDs([]uint32{1, 3, 4, 5}, vol.queryIndex.nameRank)
	if got := directTestBlockMinRank(pngcData, firstBlock); got != wantCommonRank {
		t.Fatalf("PNGC common minRank=%d, want name rank %d", got, wantCommonRank)
	}

	queries := []string{
		"path:common", "path:rare", "path:no-such-component",
		"nrrd", "zqx", "path:common|rare", "path:common !beta",
		"path:common sort:size", "path:common sort:modified",
		"path:common sort:extension", "path:common sort:type", "path:common sort:path",
	}
	fastCount := map[string]bool{"path:common": true, "path:rare": true, "path:no-such-component": true, "nrrd": true, "zqx": true}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			opts := queryOptions{Query: query, Limit: 100, Trace: &searchTrace{}}
			want, err := searchCompactWithCache(idx, opts, false, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("oracle search: %v", err)
			}
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
			if err != nil {
				t.Fatalf("mapped search: %v", err)
			}
			gotPaths, wantPaths := directTestPaths(got), directTestPaths(want)
			if !strings.Contains(query, "sort:") {
				sort.Strings(gotPaths)
				sort.Strings(wantPaths)
			}
			if !reflect.DeepEqual(gotPaths, wantPaths) {
				t.Fatalf("mapped paths=%v, want exhaustive %v", gotPaths, wantPaths)
			}
			countOpts := queryOptions{Query: query, Trace: &searchTrace{}}
			wantCount, err := searchCompactWithCache(idx, countOpts, true, make(map[int]string), nil)
			if err != nil {
				t.Fatalf("oracle count: %v", err)
			}
			gotCount, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, countOpts, true)
			if err != nil || len(gotCount) != len(wantCount) {
				t.Fatalf("mapped count-search=%d err=%v, want %d", len(gotCount), err, len(wantCount))
			}
			count, handled, err := countServiceVolumes([]*serviceVolumeIndex{vol}, countOpts)
			if err != nil {
				t.Fatalf("fast count: %v", err)
			}
			if fastCount[query] && !handled {
				t.Fatal("fast count declined canonical direct fixture")
			}
			if handled && count != len(wantCount) {
				t.Fatalf("fast count=%d, want %d", count, len(wantCount))
			}
		})
	}
}

func TestDecodeUint32SectionRejectsTrailingSUBTPart(t *testing.T) {
	parts := make([][]uint32, 9)
	for i := range parts {
		parts[i] = []uint32{uint32(i)}
	}
	if got := decodeUint32Section(encodeUint32Section(parts...), 8); got != nil {
		t.Fatalf("decoded %d SUBT parts from a nine-part payload; want strict rejection", len(got))
	}
}

func directTestSectionData(t *testing.T, path string, tag uint32) []byte {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	table, err := readRawSectionTable(path, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range table.Entries {
		if entry.tag == tag {
			return data[int(entry.offset):int(entry.offset+entry.length)]
		}
	}
	t.Fatalf("section %08x not found", tag)
	return nil
}

func directTestUint32Parts(t *testing.T, data []byte) [][]uint32 {
	t.Helper()
	parts := make([][]uint32, 0, 8)
	for off := 0; off < len(data); {
		if off+4 > len(data) {
			t.Fatalf("truncated uint32 part header at %d", off)
		}
		n := int(binary.LittleEndian.Uint32(data[off:]))
		off += 4
		bytesLen := n * 4
		if n < 0 || bytesLen/4 != n || off+bytesLen < off || off+bytesLen > len(data) {
			t.Fatalf("invalid uint32 part at %d count=%d", off-4, n)
		}
		part := make([]uint32, n)
		for i := range part {
			part[i] = binary.LittleEndian.Uint32(data[off+i*4:])
		}
		parts = append(parts, part)
		off += bytesLen
	}
	return parts
}

func directTestStringPostingBlocks(t *testing.T, data []byte, key string) (int, int) {
	t.Helper()
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	keyBlobLen := int(binary.LittleEndian.Uint32(data[4:]))
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	keyStart := 16 + entryCount*20 + blockCount*28
	if keyStart+keyBlobLen > len(data) {
		t.Fatal("invalid string posting layout")
	}
	for i := 0; i < entryCount; i++ {
		off := 16 + i*20
		keyOff := int(binary.LittleEndian.Uint32(data[off:]))
		keyLen := int(binary.LittleEndian.Uint16(data[off+4:]))
		if keyOff+keyLen <= keyBlobLen && string(data[keyStart+keyOff:keyStart+keyOff+keyLen]) == key {
			return int(binary.LittleEndian.Uint32(data[off+12:])), int(binary.LittleEndian.Uint32(data[off+16:]))
		}
	}
	t.Fatalf("string posting %q not found", key)
	return 0, 0
}

func directTestGramPostingBlocks(t *testing.T, data []byte, gram uint32) (int, int) {
	t.Helper()
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	for i := 0; i < entryCount; i++ {
		off := 16 + i*16
		if binary.LittleEndian.Uint32(data[off:]) == gram {
			return int(binary.LittleEndian.Uint32(data[off+8:])), int(binary.LittleEndian.Uint32(data[off+12:]))
		}
	}
	t.Fatalf("gram posting %08x not found", gram)
	return 0, 0
}

func directTestBlockMinRank(data []byte, block int) uint32 {
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	entrySize := 20
	if binary.LittleEndian.Uint32(data[4:]) == 0 {
		entrySize = 16
	}
	return binary.LittleEndian.Uint32(data[16+entryCount*entrySize+block*28+24:])
}

func directTestPaths(entries []Entry) []string {
	paths := make([]string, len(entries))
	for i := range entries {
		paths[i] = entries[i].Path
	}
	return paths
}

func TestDirectPrototypeGate(t *testing.T) {
	if v, _ := envFirst("SEEKFS_DIRECT_PROTOTYPE", "SEEKFS_DIRECT_V9_PROTOTYPE"); v != "1" {
		t.Skip("set SEEKFS_DIRECT_PROTOTYPE=1 to run the owned 500k/1M prototype gate")
	}
	records := 500_000
	if value, _ := envFirst("SEEKFS_DIRECT_RECORDS", "SEEKFS_DIRECT_V9_RECORDS"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			t.Fatalf("invalid SEEKFS_DIRECT_RECORDS=%q", value)
		}
		records = parsed
	}
	dir := t.TempDir()
	stats, err := buildDirect(context.Background(), directBuildOptions{
		OutputPath: filepath.Join(dir, "prototype.gsi"),
		SpoolDir:   filepath.Join(dir, "spool"),
		Roots:      []string{"X:\\"},
		Volume:     "X:",
		Source:     "synthetic-direct",
		BuiltAt:    time.Unix(123, 0),
		Records:    &directSyntheticSource{remaining: records},
		RunRecords: 64 * 1024,
		RunBytes:   64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadIndexMMap(filepath.Join(dir, "prototype.gsi"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loaded.MMapRecords.file.close() })
	if loaded.compactRecordCount() != records || len(loaded.Derived.NameOrder) != records {
		t.Fatalf("prototype loaded records/rank = %d/%d, want %d/%d", loaded.compactRecordCount(), len(loaded.Derived.NameOrder), records, records)
	}
	encoded, _ := json.Marshal(stats)
	t.Logf("direct prototype stats=%s output_sha256=%s", encoded, directFileHash(t, filepath.Join(dir, "prototype.gsi")))
}

func TestDirectExternalPrototypeValidation(t *testing.T) {
	path, _ := envFirst("SEEKFS_DIRECT_VALIDATE_PATH", "SEEKFS_DIRECT_V9_VALIDATE_PATH")
	if path == "" {
		t.Skip("set SEEKFS_DIRECT_VALIDATE_PATH to validate an owned prototype target")
	}
	idx, err := loadIndexMMap(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.MMapRecords.file.close() })
	n := idx.compactRecordCount()
	if idx.Version != indexVersion || n == 0 || len(idx.Derived.NameOrder) != n || len(idx.Derived.NameRank) != n {
		t.Fatalf("invalid direct prototype reader state: version=%d records=%d order=%d rank=%d", idx.Version, n, len(idx.Derived.NameOrder), len(idx.Derived.NameRank))
	}
	want := make([]uint32, n)
	for i := range want {
		want[i] = uint32(i)
	}
	sort.Slice(want, func(i, j int) bool {
		a, b := int(want[i]), int(want[j])
		ak, bk := a%100000, b%100000
		if ak != bk {
			return ak < bk
		}
		return a < b
	})
	for pos, id := range want {
		if got := idx.Derived.NameOrder[pos]; got != id {
			t.Fatalf("default-order oracle mismatch at %d: got %d want %d", pos, got, id)
		}
		if got := idx.Derived.NameRank[id]; int(got) != pos {
			t.Fatalf("rank oracle mismatch for id %d: got %d want %d", id, got, pos)
		}
		if pos < 3 || pos == len(want)-1 {
			name := idx.compactRecord(int(id)).Name
			if name != fmt.Sprintf("prototype-%05d.txt", int(id)%100000) {
				t.Fatalf("record name mismatch for id %d: %q", id, name)
			}
		}
	}
	for id := 0; id < min(n, 4096); id++ {
		wantParent := int32(id - 1)
		if id == 0 {
			wantParent = -1
		}
		if got := idx.compactRecord(id).Parent; got != wantParent {
			t.Fatalf("parent oracle mismatch for id %d: got %d want %d", id, got, wantParent)
		}
	}
	wantHash := directOrderHash(want)
	gotHash := directOrderHash(idx.Derived.NameOrder)
	if gotHash != wantHash {
		t.Fatalf("default-order search hash mismatch: got %s want %s", gotHash, wantHash)
	}
	for _, spec := range directRankSpecs() {
		if spec.Tag == indexSectionRANK {
			continue
		}
		wantFamily := make([]uint32, n)
		for i := range wantFamily {
			wantFamily[i] = uint32(i)
		}
		sort.Slice(wantFamily, func(i, j int) bool {
			ki := spec.Key(directSyntheticRecord(int(wantFamily[i])))
			kj := spec.Key(directSyntheticRecord(int(wantFamily[j])))
			if ki != kj {
				return ki < kj
			}
			return wantFamily[i] < wantFamily[j]
		})
		gotOrder, gotRank := directDerivedRank(idx, spec.Tag)
		if len(gotOrder) != n || len(gotRank) != n {
			t.Fatalf("%s lengths=%d/%d want=%d", spec.Name, len(gotOrder), len(gotRank), n)
		}
		for pos, id := range wantFamily {
			if gotOrder[pos] != id || gotRank[id] != uint32(pos) {
				t.Fatalf("%s oracle mismatch at %d: got id/rank=%d/%d want=%d", spec.Name, pos, gotOrder[pos], gotRank[id], id)
			}
		}
		t.Logf("direct %s hash=%s", spec.Name, directOrderHash(gotOrder))
	}
	t.Logf("direct reader/oracle valid records=%d default_order_hash=%s top20_hash=%s", n, gotHash, directOrderHash(idx.Derived.NameOrder[:min(20, n)]))
}

func directSyntheticRecord(id int) directRecord {
	return directRecord{
		FRN:       uint64(id + 1),
		ParentFRN: uint64(id),
		Size:      int64(id % 100000),
		ModUnix:   int64(1_700_000_000_000_000_000 + id),
		Name:      fmt.Sprintf("prototype-%05d.txt", id%100000),
		Path:      fmt.Sprintf(`synthetic:\prototype-%05d.txt`, id%100000),
	}
}

func directOrderHash(order []uint32) string {
	h := sha256.New()
	var buf [4]byte
	for _, id := range order {
		binary.LittleEndian.PutUint32(buf[:], id)
		_, _ = h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func directFileHash(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
