package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

func directWriteHeaderAndBase(ctx context.Context, cw *countingWriter, finalPath, tokenPath, frnPath string, opts directBuildOptions, recordCount int, nameBlobLen int64) (int, error) {
	if err := binary.Write(cw, binary.LittleEndian, diskHeader{
		Magic:       indexMagic,
		Version:     indexVersion,
		EntryCount:  uint64(recordCount),
		RootCount:   uint64(len(opts.Roots)),
		BuiltUnix:   opts.BuiltAt.UnixNano(),
		JournalID:   opts.JournalID,
		Checkpoint:  opts.Checkpoint,
		Compact:     compactDiskFlag | compactDiskAttrsFlag | directCompactFlags(recordCount),
		NameBlobLen: uint64(nameBlobLen),
		TokenCount:  uint64(recordCount),
	}); err != nil {
		return 0, err
	}
	// The section table offset is backpatched after all families are emitted.
	if err := binary.Write(cw, binary.LittleEndian, uint64(0)); err != nil {
		return 0, err
	}
	for _, value := range []string{opts.Source, opts.Volume, ""} {
		if err := writeString(cw, value); err != nil {
			return 0, err
		}
	}
	for _, root := range opts.Roots {
		if err := writeString(cw, root); err != nil {
			return 0, err
		}
	}
	nameFile, err := os.Open(finalPath)
	if err != nil {
		return 0, err
	}
	defer nameFile.Close()
	reader := bufio.NewReaderSize(nameFile, 256*1024)
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		rec, readErr := readDirectSpoolRecord(reader)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, readErr
		}
		if _, err := io.WriteString(cw, rec.Name); err != nil {
			return 0, err
		}
	}
	tokens, err := os.Open(tokenPath)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(cw, tokens); err != nil {
		_ = tokens.Close()
		return 0, err
	}
	if err := tokens.Close(); err != nil {
		return 0, err
	}
	// Stream records a second time.  The FRN file is mapped read-only for
	// in-memory parent lookups instead of issuing a ReadAt syscall per record.
	records, err := os.Open(finalPath)
	if err != nil {
		return 0, err
	}
	defer records.Close()
	frnMap, frns, err := directMapFRNs(frnPath, recordCount)
	if err != nil {
		return 0, err
	}
	if frnMap != nil {
		defer frnMap.close()
	}
	recordReader := bufio.NewReaderSize(records, 256*1024)
	wide := directCompactFlags(recordCount)&compactDiskWideRefsFlag != 0
	for id := 0; id < recordCount; id++ {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		rec, readErr := readDirectSpoolRecord(recordReader)
		if readErr != nil {
			return 0, readErr
		}
		parent := uint32(compactNarrowParentSentinel)
		if wide {
			parent = compactWideParentSentinel
		}
		if rec.ParentFRN != 0 {
			if parentID := directLookupIDMapped(frns, rec.ParentFRN); parentID >= 0 {
				parent = uint32(parentID)
			}
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.FRN); err != nil {
			return 0, err
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.ParentFRN); err != nil {
			return 0, err
		}
		if err := writeCompactRecordRefs(cw, parent, uint32(id), wide); err != nil {
			return 0, err
		}
		for _, value := range []any{rec.Mode, rec.Size, rec.ModUnix} {
			if err := binary.Write(cw, binary.LittleEndian, value); err != nil {
				return 0, err
			}
		}
		if err := binary.Write(cw, binary.LittleEndian, uint8(0)); err != nil {
			return 0, err
		}
	}
	return int(cw.n), nil
}

func directCompactFlags(recordCount int) uint32 {
	flags := uint32(0)
	if compactNeedsWideDiskRecords(recordCount, recordCount) {
		flags |= compactDiskWideRefsFlag
	}
	return flags
}

// directRankFamilyResult is one computed (but not yet emitted) rank section.
type directRankFamilyResult struct {
	spec     directRankSpec
	runBytes int64
	report   directRankReport
	rankPath string
}

func directWriteAtomic(ctx context.Context, opts directBuildOptions, finalPath, frnPath string, recordCount int, nameBlobLen int64, tokenPath string, rankSpecs []directRankSpec, baseScratch int64, reports *[]directRankReport, sectionReports *[]directSectionReport, scratchHigh *int64, owned *[]string) (int64, error) {
	reportPhase := func(name string, d time.Duration) {
		if opts.PhaseReporter != nil {
			opts.PhaseReporter(name, d)
		}
	}
	dir := filepath.Dir(opts.OutputPath)
	spoolDir := filepath.Dir(finalPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(dir, filepath.Base(opts.OutputPath)+".direct-*.tmp")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	*owned = append(*owned, tmp)
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	bw := bufio.NewWriterSize(f, 4*1024*1024)
	cw := &countingWriter{w: bw}
	t0 := time.Now()
	if _, err := directWriteHeaderAndBase(ctx, cw, finalPath, tokenPath, frnPath, opts, recordCount, nameBlobLen); err != nil {
		cleanup()
		return 0, err
	}
	reportPhase("header-base", time.Since(t0))
	entries := make([]indexSectionTableEntry, 0, len(rankSpecs))
	rankScratchPaths := make([]string, 0, len(rankSpecs))
	t0 = time.Now()
	sharedRankRuns, sharedRankErr := directBuildRankRunsShared(ctx, finalPath, spoolDir, opts.RunRecords, opts.RankWorkers, rankSpecs, owned)
	if sharedRankErr != nil {
		cleanup()
		return 0, sharedRankErr
	}
	reportPhase("rank-runs", time.Since(t0))
	t0 = time.Now()
	rankResults, rankErr := directComputeRankFamilies(ctx, rankSpecs, sharedRankRuns, recordCount, opts.RankWorkers, spoolDir, owned)
	reportPhase("rank-families", time.Since(t0))
	if rankErr != nil {
		cleanup()
		return 0, rankErr
	}
	for _, result := range rankResults {
		if result == nil {
			cleanup()
			return 0, errors.New("direct rank family was not computed")
		}
		spec := result.spec
		rankRuns := sharedRankRuns.Runs[spec.Name]
		sectionPath := filepath.Join(spoolDir, fmt.Sprintf("direct-rank-section-%s.tmp", spec.Name))
		entry, emitErr := directEmitRankSection(cw, spec.Tag, sectionPath)
		if emitErr != nil {
			cleanup()
			return 0, emitErr
		}
		if high := baseScratch + result.runBytes + int64(recordCount)*4; high > *scratchHigh {
			*scratchHigh = high
		}
		entries = append(entries, entry)
		retainedPath := result.rankPath
		*owned = append(*owned, retainedPath)
		rankScratchPaths = append(rankScratchPaths, retainedPath)
		if high := baseScratch + result.runBytes + int64(recordCount)*4*int64(len(rankScratchPaths)+1); high > *scratchHigh {
			*scratchHigh = high
		}
		if reports != nil {
			result.report.Bytes = int64(entry.length)
			*reports = append(*reports, result.report)
		}
		if sectionReports != nil {
			*sectionReports = append(*sectionReports, directSectionReport{Name: spec.Name, Tag: spec.Tag, Runs: len(rankRuns), Bytes: int64(entry.length), ScratchBytes: baseScratch + result.runBytes + int64(recordCount)*4})
		}
		for _, run := range rankRuns {
			_ = os.Remove(run.path)
		}
		_ = os.Remove(sectionPath)
	}
	t0 = time.Now()
	topologyEntries, topologyReports, topologyErr := directWriteTopologySections(ctx, cw, finalPath, frnPath, spoolDir, recordCount, opts.RunRecords, owned, scratchHigh)
	if topologyErr != nil {
		cleanup()
		return 0, topologyErr
	}
	reportPhase("topology", time.Since(t0))
	entries = append(entries, topologyEntries...)
	if sectionReports != nil {
		*sectionReports = append(*sectionReports, topologyReports...)
	}
	parentPath := filepath.Join(spoolDir, "direct-parents.tmp")
	sizesPath := filepath.Join(spoolDir, "direct-sizes.tmp")
	t0 = time.Now()
	subtreeEntries, subtreeReports, subtreeRankPaths, subtreeErr := directWriteSubtreeSection(ctx, cw, parentPath, sizesPath, recordCount, rankScratchPaths, owned, scratchHigh)
	if subtreeErr != nil {
		cleanup()
		return 0, subtreeErr
	}
	reportPhase("subtree", time.Since(t0))
	entries = append(entries, subtreeEntries...)
	if sectionReports != nil {
		*sectionReports = append(*sectionReports, subtreeReports...)
	}
	t0 = time.Now()
	auxEntries, auxReports, auxErr := directWriteAuxiliarySections(ctx, cw, finalPath, recordCount, opts.RunRecords, rankScratchPaths[0], subtreeRankPaths, spoolDir, owned, scratchHigh)
	if auxErr != nil {
		cleanup()
		return 0, auxErr
	}
	reportPhase("aux", time.Since(t0))
	entries = append(entries, auxEntries...)
	if sectionReports != nil {
		*sectionReports = append(*sectionReports, auxReports...)
	}
	for _, rankPath := range rankScratchPaths {
		_ = os.Remove(rankPath)
	}
	for _, rankPath := range subtreeRankPaths {
		_ = os.Remove(rankPath)
	}
	t0 = time.Now()
	if err := writeAlignment(cw, 8); err != nil {
		cleanup()
		return 0, err
	}
	tableOffset := uint64(cw.n)
	if err := directWriteSectionTable(cw, entries); err != nil {
		cleanup()
		return 0, err
	}
	if err := directFinalizeOutput(f, bw, tmp, opts.OutputPath, dir, tableOffset); err != nil {
		return 0, err
	}
	reportPhase("finalize", time.Since(t0))
	return fileSize(opts.OutputPath)
}

// directComputeRankFamilies builds every rank family concurrently with a
// bounded worker pool.  The section bytes are written to private temp files
// during the merge so the parallel phase is memory-bounded and the later serial
// emit stays byte-deterministic.
func directComputeRankFamilies(ctx context.Context, specs []directRankSpec, shared directSharedRankRuns, recordCount, workers int, spoolDir string, owned *[]string) ([]*directRankFamilyResult, error) {
	if workers < 1 {
		workers = 1
	}
	if workers > 16 {
		workers = 16
	}
	results := make([]*directRankFamilyResult, len(specs))
	ctxRanks, cancelRanks := context.WithCancel(ctx)
	defer cancelRanks()
	jobs := make(chan int, len(specs))
	var workerWG sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for worker := 0; worker < workers; worker++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for index := range jobs {
				spec := specs[index]
				runs := shared.Runs[spec.Name]
				liveCount := shared.LiveCounts[spec.Name]
				maxRunBytes := shared.MaxBytes[spec.Name]
				var runBytes int64
				for _, run := range runs {
					runBytes += run.bytes
				}
				sectionPath := filepath.Join(spoolDir, fmt.Sprintf("direct-rank-section-%s.tmp", spec.Name))
				rankPath := filepath.Join(spoolDir, fmt.Sprintf("direct-rank-by-id-%s.tmp", spec.Name))
				if _, err := directComputeRankFamily(ctxRanks, spec.Tag, runs, recordCount, liveCount, sectionPath, rankPath, owned); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
						cancelRanks()
					}
					errMu.Unlock()
					continue
				}
				results[index] = &directRankFamilyResult{
					spec:     spec,
					runBytes: runBytes,
					report:   directRankReport{Name: spec.Name, Tag: spec.Tag, Runs: len(runs), RunBytes: runBytes, MaxRunBytes: maxRunBytes},
					rankPath: rankPath,
				}
			}
		}()
	}
	// Stop feeding once the rank work is cancelled. A plain `break` would only
	// exit the select, leaving the feed to spin through the remaining specs.
feed:
	for index := range specs {
		select {
		case jobs <- index:
		case <-ctxRanks.Done():
			break feed
		}
	}
	close(jobs)
	workerWG.Wait()
	return results, firstErr
}

// directWriteSectionTable writes the counted section-table entries.
func directWriteSectionTable(cw *countingWriter, entries []indexSectionTableEntry) error {
	if err := binary.Write(cw, binary.LittleEndian, uint32(len(entries))); err != nil {
		return err
	}
	for _, entry := range entries {
		for _, value := range []any{entry.tag, entry.offset, entry.length, entry.flags} {
			if err := binary.Write(cw, binary.LittleEndian, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// directFinalizeOutput flushes the buffered writer, patches the section-table
// offset into the header, fsyncs, and atomically renames the temp file onto the
// output path.
func directFinalizeOutput(f *os.File, bw *bufio.Writer, tmp, outputPath, dir string, tableOffset uint64) error {
	if err := bw.Flush(); err != nil {
		closeAll(f)
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.Seek(int64(binary.Size(diskHeader{})), io.SeekStart); err != nil {
		closeAll(f)
		_ = os.Remove(tmp)
		return err
	}
	var patch [8]byte
	binary.LittleEndian.PutUint64(patch[:], tableOffset)
	if _, err := f.Write(patch[:]); err != nil {
		closeAll(f)
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		closeAll(f)
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, outputPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if dirFile, openErr := os.Open(dir); openErr == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func directRemoveOwned(paths []string) {
	for i := len(paths) - 1; i >= 0; i-- {
		_ = os.Remove(paths[i])
	}
}

func buildDirect(ctx context.Context, opts directBuildOptions) (stats directBuildStats, err error) {
	start := time.Now()
	report := func(name string, d time.Duration) {
		if opts.PhaseReporter != nil {
			opts.PhaseReporter(name, d)
		}
	}
	if opts.OutputPath == "" || opts.Records == nil {
		return stats, errors.New("direct build requires output path and record source")
	}
	if opts.BuiltAt.IsZero() {
		opts.BuiltAt = time.Unix(0, 0)
	}
	if opts.Source == "" {
		opts.Source = "direct"
	}
	if opts.SpoolDir == "" {
		opts.SpoolDir = filepath.Join(filepath.Dir(opts.OutputPath), ".direct-spool")
	}
	if opts.RunRecords <= 0 {
		opts.RunRecords = directDefaultRunRecords
	}
	if opts.RunBytes <= 0 {
		opts.RunBytes = directDefaultRunBytes
	}
	if opts.RankWorkers <= 0 {
		opts.RankWorkers = 1
	}
	if err := os.MkdirAll(opts.SpoolDir, 0o700); err != nil {
		return stats, err
	}
	owned := make([]string, 0, 32)
	defer func() {
		directRemoveOwned(owned)
		stats.Duration = time.Since(start)
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		stats.RuntimeHeap = mem.HeapAlloc
	}()

	if opts.MaxInaccessible < 0 {
		return stats, errors.New("direct max inaccessible must be non-negative")
	}
	t0 := time.Now()
	runs, maxRunBytes, err := directBuildRuns(ctx, opts.Records, opts.SpoolDir, opts.RunRecords, opts.RunBytes, &owned)
	if err != nil {
		return stats, err
	}
	report("runs", time.Since(t0))
	stats.Runs = len(runs)
	stats.MaxRunBytes = maxRunBytes
	var runBytes int64
	for _, run := range runs {
		runBytes += run.bytes
	}
	if opts.WalkReport != nil && !opts.WalkReport.SourceComplete {
		if opts.WalkReport.Inaccessible > 0 && opts.WalkReport.Inaccessible <= opts.MaxInaccessible {
			stats.SourceDegraded = true
		} else {
			return stats, fmt.Errorf("direct source incomplete: skipped=%d inaccessible=%d reparse=%d max-inaccessible=%d examples=%v", opts.WalkReport.Skipped, opts.WalkReport.Inaccessible, opts.WalkReport.ReparseSkipped, opts.MaxInaccessible, opts.WalkReport.SkipExamples)
		}
	}
	t0 = time.Now()
	finalPath := filepath.Join(opts.SpoolDir, "direct-records.final.tmp")
	frnPath := filepath.Join(opts.SpoolDir, "direct-frns.tmp")
	recordCount, spoolBytes, err := directMergeRuns(ctx, runs, finalPath, frnPath, &owned)
	if err != nil {
		return stats, err
	}
	report("merge-runs", time.Since(t0))
	stats.Records = recordCount
	stats.SpoolBytes = spoolBytes
	stats.FinalIDRule = "ascending-frn; duplicate-frn-rejected"
	stats.SpoolSchema = "u64 frn,parent_frn; u32 mode; i64 size,mod_unix; u32 name_bytes,path_bytes; u16 seq; utf8 name,path"
	tokenPath := filepath.Join(opts.SpoolDir, "direct-name-table.tmp")
	owned = append(owned, tokenPath)
	t0 = time.Now()
	nameBlobLen, tokenCount, err := directScanNames(finalPath, tokenPath)
	if err != nil {
		return stats, err
	}
	report("scan-names", time.Since(t0))
	if tokenCount != recordCount {
		return stats, errors.New("direct token count mismatch")
	}
	stats.NameBlobBytes = nameBlobLen
	stats.TokenBytes = int64(tokenCount) * 6
	if directCompactFlags(recordCount)&compactDiskWideRefsFlag != 0 {
		stats.RecordBytes = int64(recordCount) * compactWideDiskRecordBytes
	} else {
		stats.RecordBytes = int64(recordCount) * compactDiskRecordBytes
	}
	rankSpecs := directRankSpecs()
	// Give the size rank the recursive directory totals so a directory is
	// ordered by its subtree size, matching the persisted SUBS column.
	if dirBytes, err := directComputeDirBytes(ctx, finalPath, frnPath, recordCount); err != nil {
		return stats, err
	} else if len(dirBytes) == recordCount {
		for i := range rankSpecs {
			if rankSpecs[i].Tag != indexSectionSRNK {
				continue
			}
			bytes := dirBytes
			rankSpecs[i].KeyWithID = func(id uint32, rec directRecord) string {
				if rec.Mode&uint32(os.ModeDir) != 0 && int(id) < len(bytes) {
					return directSignedOrderKey(int64(bytes[id])) + "\x00" + strings.ToLower(rec.Name)
				}
				return directSignedOrderKey(rec.Size) + "\x00" + strings.ToLower(rec.Name)
			}
		}
	}
	stats.RankRecords = recordCount
	stats.RankBytes = int64(len(rankSpecs)) * (8 + int64(recordCount)*8)
	finalSpoolBytes := spoolBytes
	frnInfo, _ := os.Stat(frnPath)
	finalInfo, _ := os.Stat(finalPath)
	frnBytes := int64(0)
	if frnInfo != nil {
		frnBytes = frnInfo.Size()
	}
	if finalInfo != nil {
		finalSpoolBytes = finalInfo.Size()
	}
	// Each rank family is built, emitted, and released before the next one.
	// The initial merge peak and the one-family output peak are tracked
	// separately; no full rank vector is retained in Go memory.
	stats.ScratchBytes = runBytes + finalSpoolBytes + frnBytes
	baseRankScratch := finalSpoolBytes + frnBytes + stats.TokenBytes
	if baseRankScratch > stats.ScratchBytes {
		stats.ScratchBytes = baseRankScratch
	}
	stats.RankFamilies = nil
	stats.SectionReports = nil
	t0 = time.Now()
	outputBytes, err := directWriteAtomic(ctx, opts, finalPath, frnPath, recordCount, nameBlobLen, tokenPath, rankSpecs, baseRankScratch, &stats.RankFamilies, &stats.SectionReports, &stats.ScratchBytes, &owned)
	if err != nil {
		return stats, err
	}
	report("write-atomic", time.Since(t0))
	stats.OutputBytes = outputBytes
	stats.Sections = make([]string, 0, len(stats.SectionReports))
	for _, section := range stats.SectionReports {
		stats.Sections = append(stats.Sections, section.Name)
	}
	if opts.WalkReport != nil {
		stats.SourceComplete = opts.WalkReport.SourceComplete
		stats.SourceSkipped = opts.WalkReport.Skipped
		stats.SourceInaccessible = opts.WalkReport.Inaccessible
		stats.SourceExcluded = opts.WalkReport.Excluded
		stats.SourceReparseSkipped = opts.WalkReport.ReparseSkipped
		stats.SourceReparseNotFollowed = opts.WalkReport.ReparseNotFollowed
		stats.SourceExclusions = append([]string(nil), opts.WalkReport.Exclusions...)
		stats.SourceSkipExamples = append([]string(nil), opts.WalkReport.SkipExamples...)
		stats.SourceMaxInaccessible = opts.MaxInaccessible
		if stats.SourceDegraded && stats.SourceComplete {
			stats.SourceComplete = false
		}
	}
	return stats, nil
}

type directWalkPreflight struct {
	SourceRoot      string   `json:"source_root"`
	Target          string   `json:"target"`
	Spool           string   `json:"spool"`
	ExclusionRoots  []string `json:"effective_exclusion_roots"`
	ExclusionSuffix []string `json:"effective_exclusion_suffixes"`
}

var directArtifactSuffixes = []string{
	".gsi", ".gsi.tok", ".tok", ".seekfs-dogfood.jsonl", ".seekfs-agent-findings.jsonl",
}

func directCanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if canonical, canonicalErr := filepath.EvalSymlinks(abs); canonicalErr == nil {
		return filepath.Clean(canonical), nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func directWalkPreflightFor(root, output, spool string) (directWalkPreflight, error) {
	var preflight directWalkPreflight
	rootAbs, err := directCanonicalPath(root)
	if err != nil {
		return preflight, err
	}
	outAbs, err := directCanonicalPath(output)
	if err != nil {
		return preflight, err
	}
	if spool == "" {
		spool = filepath.Join(filepath.Dir(outAbs), ".direct-spool")
	}
	spoolAbs, err := directCanonicalPath(spool)
	if err != nil {
		return preflight, err
	}
	runRoot := filepath.Dir(outAbs)
	exclusions := []string{
		runRoot,
		filepath.Join(rootAbs, ".r5tmp"),
		filepath.Join(rootAbs, ".seekfs"),
		filepath.Join(rootAbs, ".seekfs-db"),
		filepath.Join(rootAbs, ".seekfs-data"),
		filepath.Join(rootAbs, ".seekfs-cache"),
		filepath.Join(rootAbs, "$Recycle.Bin"),
		filepath.Join(rootAbs, "System Volume Information"),
	}
	exclusions = append(exclusions, seekFSExclusionDirsUnder(rootAbs)...)
	for parent := runRoot; parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		if strings.EqualFold(filepath.Base(parent), ".r5tmp") {
			exclusions = append(exclusions, parent)
		}
	}
	canonicalExclusions := make([]string, 0, len(exclusions))
	seen := make(map[string]struct{}, len(exclusions))
	for _, exclusion := range exclusions {
		canonical, canonicalErr := directCanonicalPath(exclusion)
		if canonicalErr != nil {
			return preflight, canonicalErr
		}
		if strings.EqualFold(canonical, rootAbs) || !directPathUnderAny(canonical, []string{rootAbs}) {
			return preflight, fmt.Errorf("direct exclusion escapes or aliases source root: %s", canonical)
		}
		key := strings.ToLower(canonical)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		canonicalExclusions = append(canonicalExclusions, canonical)
	}
	if !directPathUnderAny(outAbs, canonicalExclusions) || !directPathUnderAny(spoolAbs, canonicalExclusions) {
		return preflight, errors.New("direct target/spool is not covered by the effective exclusions")
	}
	preflight = directWalkPreflight{
		SourceRoot:      rootAbs,
		Target:          outAbs,
		Spool:           spoolAbs,
		ExclusionRoots:  canonicalExclusions,
		ExclusionSuffix: append([]string(nil), directArtifactSuffixes...),
	}
	return preflight, nil
}

func directWriteWalkPreflight(runRoot string, preflight directWalkPreflight) error {
	data, err := json.MarshalIndent(preflight, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(runRoot, "direct-walk-preflight.json"), append(data, '\n'), 0o600); err != nil {
		return err
	}
	var log strings.Builder
	log.WriteString("preflight_complete=true\nsource_root=" + preflight.SourceRoot + "\nexclusion_roots:\n")
	for _, root := range preflight.ExclusionRoots {
		log.WriteString("  " + root + "\n")
	}
	log.WriteString("exclusion_suffixes:\n")
	for _, suffix := range preflight.ExclusionSuffix {
		log.WriteString("  " + suffix + "\n")
	}
	return os.WriteFile(filepath.Join(runRoot, "direct-startup.log"), []byte(log.String()), 0o600)
}

// directVolumeExclusions returns the paths on vol that a raw-MFT build must
// drop: the seekfs dir plus the standard system folders the walk preflight also
// excludes. Paths are left as canonical absolute paths; filterMFTExclusions
// resolves them to FRN subtrees and skips any not present on the volume.
func directVolumeExclusions(vol string) []string {
	volRoot := strings.ToUpper(strings.TrimRight(vol, `\`)) + `\`
	exclusions := seekFSExclusionDirsUnder(volRoot)
	exclusions = append(exclusions,
		filepath.Join(volRoot, "$Recycle.Bin"),
		filepath.Join(volRoot, "System Volume Information"),
	)
	return exclusions
}

// cmdDirect exposes only the bounded prototype sources.  It deliberately
// has no service, elevation, compactor, or existing-index input path.
func cmdDirect(args []string) error {
	fs := flag.NewFlagSet("direct", flag.ContinueOnError)
	out := fs.String("out", "", "new v9 output path")
	root := fs.String("root", "", "read-only filesystem root for the walk fallback")
	volume := fs.String("volume", "", "NTFS volume to build from the raw MFT/USN journal (elevated, Everything-style fast build)")
	records := fs.Int("records", 0, "deterministic synthetic record count")
	spool := fs.String("spool-dir", "", "owned scratch directory")
	runRecords := fs.Int("run-records", directDefaultRunRecords, "records per external-sort run")
	runBytes := fs.Int64("run-bytes", directDefaultRunBytes, "bytes per external-sort run")
	rankWorkers := fs.Int("rank-workers", 1, "bounded parallel rank-run sort workers (1-16)")
	walkWorkers := fs.Int("walk-workers", directConcurrentDefaultWorkers, "bounded filesystem metadata workers (1-16)")
	walkQueue := fs.Int("walk-queue", 0, "bounded filesystem walk queue (default workers*2)")
	maxInaccessible := fs.Int("max-inaccessible", directDefaultMaxInaccessible, "maximum inaccessible paths to skip before refusing to publish")
	timeout := fs.Duration("timeout", 30*time.Minute, "prototype timeout")
	jsonOut := fs.Bool("json", false, "write JSON stats")
	dryRun := fs.Bool("dry-run", false, "validate and write the filesystem-walk preflight without traversing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("direct requires -out")
	}
	provided := 0
	if *root != "" {
		provided++
	}
	if *volume != "" {
		provided++
	}
	if *records != 0 {
		provided++
	}
	if provided != 1 {
		return errors.New("direct requires exactly one of -root, -volume, or -records")
	}
	if *dryRun {
		if *root == "" {
			return errors.New("direct -dry-run requires -root")
		}
		preflight, err := directWalkPreflightFor(*root, *out, *spool)
		if err != nil {
			return err
		}
		if err := directWriteWalkPreflight(filepath.Dir(preflight.Target), preflight); err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(os.Stdout).Encode(preflight)
		}
		fmt.Printf("direct walk preflight source=%s target=%s spool=%s exclusions=%d\n", preflight.SourceRoot, preflight.Target, preflight.Spool, len(preflight.ExclusionRoots))
		return nil
	}
	var source directRecordSource
	var roots []string
	var sourceName, volName string
	var closeSource func()
	var walkReport *directWalkReport
	var journalID uint64
	var checkpoint int64
	builtAt := time.Unix(0, 0)
	if *volume != "" {
		src, vols, jid, cp, err := directVolumeSource(*volume, directVolumeExclusions(normalizeVolume(*volume)))
		if err != nil {
			return err
		}
		source = src
		roots = vols
		sourceName = "usn"
		volName = normalizeVolume(*volume)
		journalID = jid
		checkpoint = cp
		builtAt = time.Now()
	} else if *root != "" {
		preflight, err := directWalkPreflightFor(*root, *out, *spool)
		if err != nil {
			return err
		}
		if err := directWriteWalkPreflight(filepath.Dir(preflight.Target), preflight); err != nil {
			return err
		}
		rootAbs := preflight.SourceRoot
		walkReport = &directWalkReport{}
		walk, err := newDirectConcurrentWalkSourceWithExclusions(rootAbs, preflight.ExclusionRoots, preflight.ExclusionSuffix, walkReport, *walkWorkers, *walkQueue)
		if err != nil {
			return err
		}
		source = walk
		roots = []string{rootAbs}
		sourceName = "direct-walk"
		volName = filepath.VolumeName(rootAbs)
		closeSource = walk.(interface{ Close() }).Close
	} else {
		if *records < 0 {
			return errors.New("direct -records must be non-negative")
		}
		source = &directSyntheticSource{remaining: *records}
		roots = []string{"synthetic:\\"}
		sourceName = "direct-synthetic"
	}
	if closeSource != nil {
		defer closeSource()
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var phaseMillis []string
	var phaseMu sync.Mutex
	stats, err := buildDirect(ctx, directBuildOptions{
		OutputPath:      *out,
		SpoolDir:        *spool,
		Roots:           roots,
		Volume:          volName,
		Source:          sourceName,
		BuiltAt:         builtAt,
		JournalID:       journalID,
		Checkpoint:      checkpoint,
		Records:         source,
		RunRecords:      *runRecords,
		RunBytes:        *runBytes,
		RankWorkers:     *rankWorkers,
		WalkWorkers:     *walkWorkers,
		WalkQueue:       *walkQueue,
		WalkReport:      walkReport,
		MaxInaccessible: *maxInaccessible,
		PhaseReporter: func(name string, d time.Duration) {
			phaseMu.Lock()
			phaseMillis = append(phaseMillis, fmt.Sprintf("%s=%dms", name, d.Milliseconds()))
			phaseMu.Unlock()
		},
	})
	if err != nil {
		return err
	}
	stats.PhaseMillis = phaseMillis
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(os.Stdout).Encode(struct {
			OK bool `json:"ok"`
			directBuildStats
		}{OK: true, directBuildStats: stats})
	}
	fmt.Printf("direct records=%d runs=%d output=%d scratch=%d duration=%s\n", stats.Records, stats.Runs, stats.OutputBytes, stats.ScratchBytes, stats.Duration.Round(time.Millisecond))
	return nil
}
