package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"
)

// TestTotalRamBytesProbesPhysicalMemory pins the MEMORYSTATUSEX interop the
// content block-cache sizing depends on: the struct must be 64 bytes (a
// padded 72-byte struct makes GlobalMemoryStatusEx fail, silently dropping
// every default cache cap to the 64 MiB fallback) and the probe must report
// a plausible physical size on a real machine.
func TestTotalRamBytesProbesPhysicalMemory(t *testing.T) {
	if got := unsafe.Sizeof(memoryStatusEx{}); got != 64 {
		t.Fatalf("sizeof(memoryStatusEx) = %d; want 64 (Win32 MEMORYSTATUSEX)", got)
	}
	total, err := totalRamBytes()
	if err != nil {
		t.Fatalf("totalRamBytes: %v", err)
	}
	const (
		minPlausible = 512 << 20
		maxPlausible = 16 << 50
	)
	if total < minPlausible || total > maxPlausible {
		t.Fatalf("totalRamBytes = %d; want within [512 MiB, 16 PiB]", total)
	}
}

func TestPNGCAugmentPreservesSectionsAndQueryParity(t *testing.T) {
	t.Setenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING", "1")
	t.Setenv("SEEKFS_SELF_NAME_GRAMS", "0")
	dir := t.TempDir()
	sourceBase := filepath.Join(dir, "source-base.gsi")
	source := filepath.Join(dir, "source.gsi")
	target := filepath.Join(dir, "target.gsi")
	idx := dottedPathBenchmarkIndex(5000)
	if err := saveIndex(sourceBase, idx); err != nil {
		t.Fatal(err)
	}
	if err := addUnknownSectionForTest(sourceBase, source); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	oldTable, err := readRawSectionTable(source, int64(len(before)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := augmentPNGC(context.Background(), source, target, PNGCAugmentOptions{
		MaxOutputGrowth:  2 << 20,
		MinFreeDiskBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.PNGCBytes == 0 || result.OutputGrowth <= result.PNGCBytes {
		t.Fatalf("unexpected augmentation sizes: %+v", result)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fileSHA256(source); got != result.SourceSHA256 {
		t.Fatalf("source hash changed: got %s want %s", got, result.SourceSHA256)
	}
	newTable, err := readRawSectionTable(target, int64(len(after)))
	if err != nil {
		t.Fatal(err)
	}
	if len(newTable.Entries) != len(oldTable.Entries)+1 {
		t.Fatalf("section count = %d, want %d", len(newTable.Entries), len(oldTable.Entries)+1)
	}
	var pngc indexSectionTableEntry
	for _, entry := range newTable.Entries {
		if entry.tag == indexSectionPNGC {
			pngc = entry
		}
	}
	if pngc.length == 0 || pngc.offset+pngc.length > uint64(len(after)) {
		t.Fatalf("invalid PNGC entry: %+v", pngc)
	}
	for _, old := range oldTable.Entries {
		oldBytes := before[old.offset : old.offset+old.length]
		newBytes := after[old.offset : old.offset+old.length]
		if !bytes.Equal(oldBytes, newBytes) {
			t.Fatalf("raw section %08x changed", old.tag)
		}
	}
	loaded := mustLoadMappedIndex(t, target)
	defer closeMappedIndex(loaded)
	if loaded.Derived.SelfNameTrigrams == nil {
		t.Fatal("target PNGC did not decode")
	}
	for _, query := range []string{"nrrd", ".json", "path:workspace nrrd", "nrrd|raw"} {
		opts := queryOptions{Query: query, MatchPath: true, Limit: 20}
		want, err := searchCompactWithCache(mustLoadIndex(t, source), opts, false, make(map[int]string), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := searchCompactWithCache(loaded, opts, false, make(map[int]string), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !sameEntryResults(want, got) {
			t.Fatalf("query %q changed across augmentation: want=%v got=%v", query, want, got)
		}
	}
}

func TestPNGCAugmentRejectsCorruptionDuplicateAndCleansUp(t *testing.T) {
	t.Setenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING", "1")
	t.Setenv("SEEKFS_SELF_NAME_GRAMS", "0")
	dir := t.TempDir()
	source := filepath.Join(dir, "source.gsi")
	if err := saveIndex(source, dottedPathBenchmarkIndex(1000)); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "corrupt.gsi")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	var header diskHeader
	if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &header); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(data[binary.Size(diskHeader{}):], uint64(len(data)-2))
	if err := os.WriteFile(corrupt, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := augmentPNGC(context.Background(), corrupt, filepath.Join(dir, "bad-target.gsi"), PNGCAugmentOptions{}); err == nil {
		t.Fatal("corrupt table unexpectedly accepted")
	}

	target := filepath.Join(dir, "target.gsi")
	if _, err := augmentPNGC(context.Background(), source, target, PNGCAugmentOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := augmentPNGC(context.Background(), target, filepath.Join(dir, "duplicate.gsi"), PNGCAugmentOptions{}); err == nil {
		t.Fatal("duplicate PNGC unexpectedly accepted")
	}
	failed := filepath.Join(dir, "failed.gsi")
	if _, err := augmentPNGC(context.Background(), source, failed, PNGCAugmentOptions{FailAfterBytes: 1}); err == nil {
		t.Fatal("failure injection unexpectedly succeeded")
	}
	assertNoPNGCTempFiles(t, dir)
	canceled := filepath.Join(dir, "canceled.gsi")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := augmentPNGC(ctx, source, canceled, PNGCAugmentOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	assertNoPNGCTempFiles(t, dir)
}

func TestPNGCAugmentFixtureMeasurement(t *testing.T) {
	t.Setenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING", "1")
	t.Setenv("SEEKFS_SELF_NAME_GRAMS", "0")
	// The 200k/500k sizes measure the same properties as 50k at scale;
	// they run in the soak tier (CI) while the default tier keeps 50k.
	sizes := []int{50_000}
	if testSoakEnabled() {
		sizes = []int{50_000, 200_000, 500_000}
	} else {
		t.Logf("soak sizes skipped (SEEKFS_TEST_SOAK=1 runs 200k/500k in CI)")
	}
	for _, records := range sizes {
		t.Run(fmt.Sprintf("records-%d", records), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.gsi")
			target := filepath.Join(dir, "target.gsi")
			if err := saveIndex(source, dottedPathBenchmarkIndex(records)); err != nil {
				t.Fatal(err)
			}
			if records == 500_000 {
				if err := stripPNGRMetadataForTest(source); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			result, err := augmentPNGC(context.Background(), source, target, PNGCAugmentOptions{
				MaxOutputGrowth:  32 << 20,
				MinFreeDiskBytes: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			metric := struct {
				Records      int     `json:"records"`
				SourceBytes  int64   `json:"source_bytes"`
				TargetBytes  int64   `json:"target_bytes"`
				PNGCBytes    int64   `json:"pngc_bytes"`
				ScratchBytes int64   `json:"scratch_bytes"`
				OutputGrowth int64   `json:"output_growth"`
				WallMS       float64 `json:"wall_ms"`
				HeapInuse    uint64  `json:"heap_inuse"`
				SourceSHA256 string  `json:"source_sha256"`
				TargetSHA256 string  `json:"target_sha256"`
			}{records, before.Size(), result.TargetBytes, result.PNGCBytes, result.ScratchBytes, result.OutputGrowth, float64(result.Wall.Microseconds()) / 1000, mem.HeapInuse, result.SourceSHA256, result.TargetSHA256}
			encoded, _ := json.Marshal(metric)
			t.Logf("PNGCAUGMENT_METRIC %s", encoded)
		})
	}
}

func TestPNGCAugmentStreamingMatchesInMemoryBuilder(t *testing.T) {
	t.Setenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING", "1")
	idx := dottedPathBenchmarkIndex(5000)
	selective := buildSelectiveNameTrigramIndex(idx, 1)
	stream, err := newPNGCStreamBuilder(context.Background(), idx, selective, t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.cleanup()
	entries, blocks, size, err := stream.measure(nil)
	if err != nil {
		t.Fatal(err)
	}
	var streamed bytes.Buffer
	if got, err := stream.writePayload(context.Background(), &streamed, nil, entries, blocks); err != nil {
		t.Fatal(err)
	} else if got != int64(streamed.Len()) || got != int64(size) {
		t.Fatalf("stream size = %d/%d/%d", got, streamed.Len(), size)
	}
	want := encodeGramPostingSection(optionalSelfNameGramIndex(idx, selective), nil)
	if !bytes.Equal(streamed.Bytes(), want) {
		t.Fatalf("streamed PNGC differs from in-memory encoding: got=%d want=%d", streamed.Len(), len(want))
	}
}

func TestPNGCAugmentStreamingRejectsSpoolCorruptionAndWriteFailure(t *testing.T) {
	idx := dottedPathBenchmarkIndex(1000)
	selective := buildSelectiveNameTrigramIndex(idx, 1)
	stream, err := newPNGCStreamBuilder(context.Background(), idx, selective, t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.cleanup()
	if len(stream.grams) == 0 {
		t.Fatal("expected streamed grams")
	}
	var bad [4]byte
	binary.LittleEndian.PutUint32(bad[:], ^uint32(0))
	if _, err := stream.file.WriteAt(bad[:], stream.grams[0].offset); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := stream.measure(nil); err == nil {
		t.Fatal("corrupt spool unexpectedly measured")
	}

	stream2, err := newPNGCStreamBuilder(context.Background(), idx, selective, t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer stream2.cleanup()
	entries, blocks, _, err := stream2.measure(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream2.writePayload(context.Background(), &failAfterWriter{limit: 8}, nil, entries, blocks); err == nil {
		t.Fatal("write failure unexpectedly succeeded")
	}
	if _, err := stream2.writePayload(canceledContext(), io.Discard, nil, entries, blocks); err == nil {
		t.Fatal("canceled encode unexpectedly succeeded")
	}
}

func TestPNGCAugmentLegacyNoMetadataUsesBoundedDiscovery(t *testing.T) {
	t.Setenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING", "1")
	t.Setenv("SEEKFS_SELF_NAME_GRAMS", "0")
	// MAX_POSTING=1 forces the bounded discovery path at any scale, so a
	// 100k index asserts the same properties as 500k; the full 500k runs
	// in the soak tier (CI).
	legacyRecords := 100_000
	if testSoakEnabled() {
		legacyRecords = 500_000
	}
	t.Logf("legacy augment fixture: %d records (soak=%v)", legacyRecords, testSoakEnabled())
	dir := t.TempDir()
	source := filepath.Join(dir, "legacy.gsi")
	if err := saveIndex(source, dottedPathBenchmarkIndex(legacyRecords)); err != nil {
		t.Fatal(err)
	}
	if err := stripPNGRMetadataForTest(source); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "legacy-pngc.gsi")
	result, err := augmentPNGC(context.Background(), source, target, PNGCAugmentOptions{MaxScratchBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.ScratchBytes == 0 || result.ScratchBytes > 64<<20 {
		t.Fatalf("unexpected legacy scratch: %d", result.ScratchBytes)
	}
	loaded := mustLoadMappedIndex(t, target)
	defer closeMappedIndex(loaded)
	if loaded.Derived.NameTrigrams == nil || loaded.Derived.NameTrigrams.gramCountsComplete {
		t.Fatal("legacy PNGR unexpectedly retained complete metadata")
	}
	if loaded.Derived.SelfNameTrigrams == nil || !loaded.Derived.SelfNameTrigrams.gramUnionComplete {
		t.Fatal("legacy PNGC did not declare a complete union")
	}
	its, _, exactZero, complete := completeSelfNameGramIterators(loaded, "zzzz-no-hit")
	if !complete || !exactZero || len(its) != 0 {
		t.Fatalf("legacy exact-zero = complete:%v exact:%v iterators:%d", complete, exactZero, len(its))
	}
	for _, query := range []string{"nrrd", ".json", "zzzz-no-hit", "path:workspace nrrd", "nrrd|raw"} {
		opts := queryOptions{Query: query, MatchPath: true, Limit: 20}
		want, err := searchCompactWithCache(mustLoadIndex(t, source), opts, false, make(map[int]string), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := searchCompactWithCache(loaded, opts, false, make(map[int]string), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !sameEntryResults(want, got) {
			t.Fatalf("legacy query %q changed across augmentation", query)
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	table, err := readRawSectionTable(target, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var pngc indexSectionTableEntry
	for _, entry := range table.Entries {
		if entry.tag == indexSectionPNGC {
			pngc = entry
		}
	}
	gotPNGC := data[pngc.offset : pngc.offset+pngc.length]
	idx := dottedPathBenchmarkIndex(legacyRecords)
	selective := buildSelectiveNameTrigramIndex(idx, 1)
	rankedSource := mustLoadMappedIndex(t, source)
	defer closeMappedIndex(rankedSource)
	wantPNGC := encodeGramPostingSectionWithMetadata(optionalSelfNameGramIndex(idx, selective), rankedSource.Derived.NameRank, gramPostingUnionMetadataMagic)
	if !bytes.Equal(gotPNGC, wantPNGC) {
		first := -1
		for i := range gotPNGC {
			if gotPNGC[i] != wantPNGC[i] {
				first = i
				break
			}
		}
		entryCount := int(binary.LittleEndian.Uint32(gotPNGC))
		blockStart := 16 + entryCount*16
		blockIndex := (first - blockStart) / 28
		if first >= blockStart && blockIndex >= 0 && blockStart+blockIndex*28+28 <= len(gotPNGC) {
			t.Logf("first block diff=%d got=%x want=%x", blockIndex, gotPNGC[blockStart+blockIndex*28:blockStart+blockIndex*28+28], wantPNGC[blockStart+blockIndex*28:blockStart+blockIndex*28+28])
		}
		t.Fatalf("legacy streamed PNGC differs from metadata-backed complete build: got=%d want=%d first_diff=%d got_byte=%02x want_byte=%02x", len(gotPNGC), len(wantPNGC), first, gotPNGC[first], wantPNGC[first])
	}
}

type failAfterWriter struct{ written, limit int }

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.written >= w.limit {
		return 0, errors.New("injected write failure")
	}
	n := min(len(p), w.limit-w.written)
	w.written += n
	return n, io.ErrShortWrite
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func mustLoadMappedIndex(t *testing.T, path string) *Index {
	t.Helper()
	idx, err := loadIndexMMap(path)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func mustLoadIndex(t *testing.T, path string) *Index {
	t.Helper()
	idx, err := loadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func sameEntryResults(a, b []Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Name != b[i].Name || a[i].Size != b[i].Size || a[i].Mode != b[i].Mode {
			return false
		}
	}
	return true
}

func assertNoPNGCTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Fatalf("temporary augmentation artifact survived: %s", entry.Name())
		}
	}
}

func addUnknownSectionForTest(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	table, err := readRawSectionTable(source, int64(len(data)))
	if err != nil {
		return err
	}
	for len(data)%8 != 0 {
		data = append(data, 0)
	}
	unknown := []byte("unknown-section-payload")
	unknownOffset := uint64(len(data))
	data = append(data, unknown...)
	for len(data)%8 != 0 {
		data = append(data, 0)
	}
	tableOffset := uint64(len(data))
	entries := append(append([]indexSectionTableEntry(nil), table.Entries...), indexSectionTableEntry{
		tag: 0x58585858, offset: unknownOffset, length: uint64(len(unknown)), flags: 7,
	})
	var count [4]byte
	binary.LittleEndian.PutUint32(count[:], uint32(len(entries)))
	data = append(data, count[:]...)
	for _, entry := range entries {
		var raw [24]byte
		binary.LittleEndian.PutUint32(raw[0:], entry.tag)
		binary.LittleEndian.PutUint64(raw[4:], entry.offset)
		binary.LittleEndian.PutUint64(raw[12:], entry.length)
		binary.LittleEndian.PutUint32(raw[20:], entry.flags)
		data = append(data, raw[:]...)
	}
	binary.LittleEndian.PutUint64(data[binary.Size(diskHeader{}):], tableOffset)
	return os.WriteFile(target, data, 0o600)
}

func stripPNGRMetadataForTest(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	table, err := readRawSectionTable(path, int64(len(data)))
	if err != nil {
		return err
	}
	pngr := -1
	var pngrEntry indexSectionTableEntry
	maxOtherEnd := uint64(0)
	for i, entry := range table.Entries {
		if entry.tag == indexSectionPNGR {
			pngr = i
			pngrEntry = entry
			continue
		}
		if entry.offset+entry.length > maxOtherEnd {
			maxOtherEnd = entry.offset + entry.length
		}
	}
	if pngr < 0 {
		return errors.New("PNGR missing")
	}
	section := data[pngrEntry.offset : pngrEntry.offset+pngrEntry.length]
	if len(section) < 16 {
		return errors.New("PNGR truncated")
	}
	entryCount := int(binary.LittleEndian.Uint32(section[0:]))
	blockCount := int(binary.LittleEndian.Uint32(section[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(section[12:]))
	metadata := 16 + entryCount*16 + blockCount*28 + blockBlobLen
	if metadata < 16 || metadata > len(section) || maxOtherEnd > pngrEntry.offset+uint64(metadata) {
		return errors.New("PNGR metadata layout is not terminal")
	}
	if metadata+8 > len(section) || binary.LittleEndian.Uint32(section[metadata:]) != gramPostingMetadataMagic {
		return errors.New("PNGR GRM1 metadata missing")
	}
	data = data[:pngrEntry.offset+uint64(metadata)]
	for len(data)%8 != 0 {
		data = append(data, 0)
	}
	tableOffset := uint64(len(data))
	table.Entries[pngr].length = uint64(metadata)
	var count [4]byte
	binary.LittleEndian.PutUint32(count[:], uint32(len(table.Entries)))
	data = append(data, count[:]...)
	for _, entry := range table.Entries {
		var raw [24]byte
		binary.LittleEndian.PutUint32(raw[0:], entry.tag)
		binary.LittleEndian.PutUint64(raw[4:], entry.offset)
		binary.LittleEndian.PutUint64(raw[12:], entry.length)
		binary.LittleEndian.PutUint32(raw[20:], entry.flags)
		data = append(data, raw[:]...)
	}
	binary.LittleEndian.PutUint64(data[binary.Size(diskHeader{}):], tableOffset)
	return os.WriteFile(path, data, 0o600)
}
