//go:build windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

var zipfCommonBases = []string{
	"index.js", "README.md", ".gitignore", "__init__.py", "package.json",
	"main.go", "utils.go", "style.css", "config.yaml", "LICENSE",
}

// zipfCommonNames returns a pool of reusable file names, like a real tree full
// of index.js, README.md and package.json files.
func zipfCommonNames(pool int) []string {
	if pool%len(zipfCommonBases) != 0 {
		pool += len(zipfCommonBases) - pool%len(zipfCommonBases)
	}
	names := make([]string, 0, pool)
	for i := 0; len(names) < pool; i++ {
		base := zipfCommonBases[i%len(zipfCommonBases)]
		ext := filepath.Ext(base)
		stem := strings.TrimSuffix(base, ext)
		names = append(names, fmt.Sprintf("%s-%03d%s", stem, i/len(zipfCommonBases), ext))
	}
	return names
}

// zipfRecord builds one synthetic record: 35% of files reuse a pool name, the
// rest are unique. Shapes match the name-index memory benchmark.
func zipfRecord(i int, common []string) synthOpts {
	parent := uint64(5)
	if i > 6 {
		parent = uint64(5 + (i-6)/8)
	}
	if parent >= uint64(i) {
		parent = 5
	}
	isDir := i%13 == 0
	var size int64
	if !isDir {
		size = int64(i % 1_000_000)
	}
	var name string
	if i%100 < 35 {
		name = common[(i*2654435761)%len(common)]
	} else {
		ext := []string{".txt", ".bin", ".go", ".dat", ".log"}[i%5]
		name = fmt.Sprintf("unique-%07d%s", i, ext)
	}
	return synthOpts{
		inUse:       true,
		isDir:       isDir,
		name:        name,
		namespace:   fileNameWin32,
		parentFRN:   parent,
		modUnixNano: int64(1_700_000_000_000_000_000 + int64(i)*1_000_000),
		size:        size,
		nonResident: true,
	}
}

// oldBuildRecordsFromMFT is the pre-arena conversion kept as a reference.
func oldBuildRecordsFromMFT(idx *Index, entries map[uint64]mftEntry) {
	idx.CompactAttrs = true
	frns := make([]uint64, 0, len(entries))
	for frn := range entries {
		frns = append(frns, frn)
	}
	sort.Slice(frns, func(i, j int) bool { return frns[i] < frns[j] })
	frnToIndex := make(map[uint64]int, len(frns))
	for i, frn := range frns {
		frnToIndex[frn] = i
	}
	idx.Records = make([]CompactRecord, 0, len(frns))
	for _, frn := range frns {
		e := entries[frn]
		parent := int32(-1)
		if p, ok := frnToIndex[e.parentFRN]; ok && p != frnToIndex[frn] {
			parent = int32(p)
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: e.parentFRN,
			Parent:    parent,
			Name:      e.name,
			Mode:      modeFromAttrs(e.attr),
			Size:      e.size,
			ModUnix:   e.modUnix,
			Seq:       e.seq,
		})
	}
}

// buildLegacyZipfIndex writes the same records through the old map/string
// accumulation and the old string-dedup name table.
func buildLegacyZipfIndex(t *testing.T, n int, dir string) (string, indexNameStats) {
	t.Helper()
	common := zipfCommonNames(2048)
	entries := make(map[uint64]mftEntry, n)
	for i := 0; i < n; i++ {
		rec := synthMFTRecord(zipfRecord(i, common))
		if e, ok := parseMFTRecord(rec, uint64(i), 512); ok {
			entries[e.frn] = e
		}
	}
	idx := &Index{
		Version: indexVersion, Roots: []string{`X:\`}, BuiltAt: time.Now(),
		Source: "usn", Volume: "X:", Compact: true, CompactAttrs: true,
	}
	oldBuildRecordsFromMFT(idx, entries)
	buildOrders(idx)
	path := filepath.Join(dir, "zipf-legacy.gsi")
	tmp, err := stageIndexFile(path, idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitStageIndexFile(path, tmp); err != nil {
		t.Fatal(err)
	}
	return path, readIndexNameStats(t, path)
}

// buildCompactZipfIndex writes the same records through the deduplicating
// arena accumulator and the arena token table.
func buildCompactZipfIndex(t *testing.T, n int, dir string) (string, indexNameStats) {
	t.Helper()
	common := zipfCommonNames(2048)
	table := newMFTTable(uint64(n))
	buf := make([]byte, 1024)
	for i := 0; i < n; i++ {
		fillSynthMFTRecord(buf, zipfRecord(i, common))
		parseMFTRecordInto(buf, uint64(i), 512, table)
	}
	idx := &Index{
		Version: indexVersion, Roots: []string{`X:\`}, BuiltAt: time.Now(),
		Source: "usn", Volume: "X:", Compact: true, CompactAttrs: true,
	}
	buildRecordsFromMFT(idx, table)
	buildOrders(idx)
	path := filepath.Join(dir, "zipf-compact.gsi")
	tmp, err := stageIndexFile(path, idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitStageIndexFile(path, tmp); err != nil {
		t.Fatal(err)
	}
	return path, readIndexNameStats(t, path)
}

type indexNameStats struct {
	fileSize        int64
	tokens          int
	tokenTableBytes int64
	lowerBytes      int64
	pngrBytes       int64
	pngcBytes       int64
}

func readIndexNameStats(t *testing.T, path string) indexNameStats {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stats := indexNameStats{fileSize: info.Size()}
	idx, err := loadIndexMMap(path)
	if err != nil {
		t.Fatal(err)
	}
	if idx.MMapRecords != nil {
		stats.tokens = len(idx.MMapRecords.tokenTable) / 6
		stats.tokenTableBytes = int64(len(idx.MMapRecords.tokenTable) + len(idx.MMapRecords.nameBlob))
		_ = idx.MMapRecords.file.close()
	}
	table, err := readRawSectionTable(path, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range table.Entries {
		switch entry.tag {
		case indexSectionLOWR:
			stats.lowerBytes = int64(entry.length)
		case indexSectionPNGR:
			stats.pngrBytes = int64(entry.length)
		case indexSectionPNGC:
			stats.pngcBytes = int64(entry.length)
		}
	}
	return stats
}

func loadIndexNameBytes(t *testing.T, path string) (blob, tokens, lower []byte) {
	t.Helper()
	idx, err := loadIndexMMap(path)
	if err != nil {
		t.Fatal(err)
	}
	if idx.MMapRecords != nil {
		blob = append([]byte(nil), idx.MMapRecords.nameBlob...)
		tokens = append([]byte(nil), idx.MMapRecords.tokenTable...)
		_ = idx.MMapRecords.file.close()
	}
	lower = append([]byte(nil), directTestSectionData(t, path, indexSectionLOWR)...)
	return blob, tokens, lower
}

func TestZipfNameDedupMatchesLegacy(t *testing.T) {
	dir := t.TempDir()
	const records = 20_000
	legacyPath, legacy := buildLegacyZipfIndex(t, records, dir)
	compactPath, compact := buildCompactZipfIndex(t, records, dir)

	if legacy.tokens != compact.tokens {
		t.Fatalf("token count legacy=%d compact=%d", legacy.tokens, compact.tokens)
	}
	if compact.tokens >= records {
		t.Fatalf("token count %d did not dedup %d records", compact.tokens, records)
	}
	if legacy.fileSize != compact.fileSize {
		t.Fatalf("file size legacy=%d compact=%d", legacy.fileSize, compact.fileSize)
	}
	oldBlob, oldTokens, oldLower := loadIndexNameBytes(t, legacyPath)
	newBlob, newTokens, newLower := loadIndexNameBytes(t, compactPath)
	if !bytes.Equal(oldBlob, newBlob) {
		t.Fatalf("name blob differs: legacy=%d bytes compact=%d bytes", len(oldBlob), len(newBlob))
	}
	if !bytes.Equal(oldTokens, newTokens) {
		t.Fatalf("token table differs: legacy=%d bytes compact=%d bytes", len(oldTokens), len(newTokens))
	}
	if !bytes.Equal(oldLower, newLower) {
		t.Fatalf("lower-name section differs: legacy=%d bytes compact=%d bytes", len(oldLower), len(newLower))
	}
}

// runLegacyZipfPipeline measures the old map accumulation, old record build,
// and old name-table write.
func runLegacyZipfPipeline(n int, tmpPath string) []mftBenchStage {
	common := zipfCommonNames(2048)
	stages := make([]mftBenchStage, 0, 4)
	entries := make(map[uint64]mftEntry, n)
	src := make([]byte, 1024)
	stages = append(stages, measureMFTStage("accumulate", n, func() {
		for i := 0; i < n; i++ {
			fillSynthMFTRecord(src, zipfRecord(i, common))
			rec := make([]byte, len(src))
			copy(rec, src)
			if e, ok := parseMFTRecord(rec, uint64(i), 512); ok {
				entries[e.frn] = e
			}
		}
	}))
	idx := &Index{Version: indexVersion, Roots: []string{`X:\`}, Source: "usn", Volume: "X:", Compact: true, CompactAttrs: true}
	stages = append(stages, measureMFTStage("records", len(entries), func() {
		oldBuildRecordsFromMFT(idx, entries)
	}))
	stages = append(stages, measureMFTStage("orders", len(idx.Records), func() {
		buildOrders(idx)
	}))
	stages = append(stages, measureMFTStage("stage", len(idx.Records), func() {
		tmp, err := stageIndexFile(tmpPath, idx)
		if err != nil {
			panic(err)
		}
		_ = os.Remove(tmp)
	}))
	entries = nil
	idx = nil
	src = nil
	runtime.GC()
	return stages
}

// runCompactZipfPipeline measures the deduplicating arena path.
func runCompactZipfPipeline(n int, tmpPath string) []mftBenchStage {
	common := zipfCommonNames(2048)
	stages := make([]mftBenchStage, 0, 4)
	table := newMFTTable(uint64(n))
	buf := make([]byte, 1024)
	stages = append(stages, measureMFTStage("accumulate", n, func() {
		for i := 0; i < n; i++ {
			fillSynthMFTRecord(buf, zipfRecord(i, common))
			parseMFTRecordInto(buf, uint64(i), 512, table)
		}
	}))
	idx := &Index{Version: indexVersion, Roots: []string{`X:\`}, Source: "usn", Volume: "X:", Compact: true, CompactAttrs: true}
	stages = append(stages, measureMFTStage("records", table.count, func() {
		buildRecordsFromMFT(idx, table)
	}))
	stages = append(stages, measureMFTStage("orders", len(idx.Records), func() {
		buildOrders(idx)
	}))
	stages = append(stages, measureMFTStage("stage", len(idx.Records), func() {
		tmp, err := stageIndexFile(tmpPath, idx)
		if err != nil {
			panic(err)
		}
		_ = os.Remove(tmp)
	}))
	table = nil
	idx = nil
	buf = nil
	runtime.GC()
	return stages
}

// TestZipfIndexSizeReport is a measurement tool, skipped unless
// SEEKFS_ZIPF_REPORT=1. It builds both pipelines over the same corpus.
func TestZipfIndexSizeReport(t *testing.T) {
	if os.Getenv("SEEKFS_ZIPF_REPORT") != "1" {
		t.Skip("set SEEKFS_ZIPF_REPORT=1 to run the 1M index-size comparison")
	}
	n := 1_000_000
	if v := os.Getenv("SEEKFS_ZIPF_N"); v != "" {
		if parsed, err := fmt.Sscanf(v, "%d", &n); parsed != 1 || err != nil || n <= 0 {
			n = 1_000_000
		}
	}
	dir := t.TempDir()
	legacyPath, legacy := buildLegacyZipfIndex(t, n, dir)
	legacyStages := runLegacyZipfPipeline(n, filepath.Join(dir, "legacy-pipeline.gsi"))
	compactPath, compact := buildCompactZipfIndex(t, n, dir)
	compactStages := runCompactZipfPipeline(n, filepath.Join(dir, "compact-pipeline.gsi"))

	peak := func(stages []mftBenchStage) float64 {
		var max uint64
		for _, s := range stages {
			if s.peakHeap > max {
				max = s.peakHeap
			}
		}
		return float64(max) / float64(n)
	}
	t.Logf("legacy  tokens=%d file=%d name_table=%d lower=%d pngr=%d pngc=%d peak_B_record=%.1f",
		legacy.tokens, legacy.fileSize, legacy.tokenTableBytes, legacy.lowerBytes, legacy.pngrBytes, legacy.pngcBytes, peak(legacyStages))
	t.Logf("compact tokens=%d file=%d name_table=%d lower=%d pngr=%d pngc=%d peak_B_record=%.1f",
		compact.tokens, compact.fileSize, compact.tokenTableBytes, compact.lowerBytes, compact.pngrBytes, compact.pngcBytes, peak(compactStages))
	for _, s := range legacyStages {
		s.report(t)
	}
	for _, s := range compactStages {
		s.report(t)
	}
	_ = legacyPath
	_ = compactPath
}
