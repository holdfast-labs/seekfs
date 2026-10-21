package main

// The auxiliary direct-builder slice deliberately rebuilds one serving family
// at a time from the canonical record spool.  It is a bounded prototype path:
// no resident Index or cross-family posting map is retained between writes.

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type directAuxSection struct {
	tag  uint32
	name string
	data []byte
}

func directWriteAuxiliarySections(ctx context.Context, cw *countingWriter, finalPath string, recordCount int, runRecords int, nameRankPath string, subtreeRankPaths []string, scratchDir string, owned *[]string, scratchHigh *int64) ([]indexSectionTableEntry, []directSectionReport, error) {
	entries := make([]indexSectionTableEntry, 0, 8)
	reports := make([]directSectionReport, 0, 8)
	nameRanks, err := directReadUint32Vector(nameRankPath, recordCount)
	if err != nil {
		return nil, nil, err
	}
	emit := func(tag uint32, name string, data []byte) error {
		if err := writeAlignment(cw, 8); err != nil {
			return err
		}
		offset := uint64(cw.n)
		if len(data) > 0 {
			if _, err := cw.Write(data); err != nil {
				return err
			}
		}
		entry := indexSectionTableEntry{tag: tag, offset: offset, length: uint64(cw.n) - offset}
		entries = append(entries, entry)
		reports = append(reports, directSectionReport{Name: name, Tag: tag, Bytes: int64(entry.length)})
		directAuxMemoryTrace(scratchDir, name+"-emitted")
		if int64(len(data)) > *scratchHigh {
			*scratchHigh = int64(len(data))
		}
		return nil
	}
	if err := writeAlignment(cw, 8); err != nil {
		return nil, nil, err
	}
	lowr, err := directWriteLOWRSectionFromSpool(ctx, cw, finalPath, recordCount)
	if err != nil {
		return nil, nil, err
	}
	entries = append(entries, lowr)
	reports = append(reports, directSectionReport{Name: "LOWR", Tag: indexSectionLOWR, Bytes: int64(lowr.length)})

	// Collect every posting family in a single spool pass; the individual
	// sections below then emit from the shared maps without re-reading the
	// record spool.
	maps, err := directCollectAuxMaps(ctx, finalPath)
	if err != nil {
		return nil, nil, err
	}
	pext := encodeStringPostingSection(maps.ext, nil)
	if err := emit(indexSectionPEXT, "PEXT", pext); err != nil {
		return nil, nil, err
	}
	if err := emit(indexSectionPXRB, "PXRB", directZeroPostingBounds(pext)); err != nil {
		return nil, nil, err
	}
	maps.ext = nil
	pext = nil
	runtime.GC()
	directAuxMemoryTrace(scratchDir, "PEXT-released")

	pcmp := encodeStringPostingSection(maps.comp, nameRanks)
	if err := emit(indexSectionPCMP, "PCMP", pcmp); err != nil {
		return nil, nil, err
	}
	cmpBounds, err := directBuildComponentPostingRankBounds(maps.comp, subtreeRankPaths, recordCount)
	if err != nil {
		return nil, nil, err
	}
	if err := emit(indexSectionPXRC, "PXRC", encodePostingRankBounds(cmpBounds)); err != nil {
		return nil, nil, err
	}
	maps.comp = nil
	pcmp = nil
	runtime.GC()
	directAuxMemoryTrace(scratchDir, "PCMP-released")

	if err := emit(indexSectionPATR, "PATR", encodeAttrPostingSection(maps.attrs)); err != nil {
		return nil, nil, err
	}
	maps.attrs = nil
	runtime.GC()
	directAuxMemoryTrace(scratchDir, "PATR-released")

	stored, omitted := directPartitionNameGrams(maps.gramCnt, serviceLowMemoryTrigramStoredPostingMax())
	maps.gramCnt = nil
	// Build both gram run sets in one spool pass, then emit each section
	// serially to the shared output writer.
	runLimit := directGramRunLimit(runRecords)
	pngrRuns, pngcRuns, err := directBuildGramRunSets(ctx, finalPath, scratchDir, runLimit, stored, directGramKeys(omitted), owned)
	if err != nil {
		return nil, nil, err
	}
	gramEntry, gramReport, err := directWriteGramSectionFromRuns(ctx, cw, pngrRuns, omitted, nameRanks, gramPostingMetadataMagic, "PNGR", scratchDir, owned, scratchHigh)
	for _, run := range pngrRuns {
		_ = os.Remove(run.path)
	}
	if err != nil {
		return nil, nil, err
	}
	entries = append(entries, gramEntry)
	reports = append(reports, gramReport)
	runtime.GC()
	directAuxMemoryTrace(scratchDir, "PNGR-released")

	if len(pngcRuns) > 0 {
		gramEntry, gramReport, err = directWriteGramSectionFromRuns(ctx, cw, pngcRuns, nil, nameRanks, gramPostingUnionMetadataMagic, "PNGC", scratchDir, owned, scratchHigh)
		for _, run := range pngcRuns {
			_ = os.Remove(run.path)
		}
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, gramEntry)
		reports = append(reports, gramReport)
		runtime.GC()
		directAuxMemoryTrace(scratchDir, "PNGC-released")
	}
	_ = scratchDir
	_ = owned
	return entries, reports, nil
}

func directAuxMemoryTrace(dir, phase string) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	f, err := os.OpenFile(filepath.Join(dir, "direct-aux-memory.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s heap=%d sys=%d\n", phase, mem.HeapAlloc, mem.Sys)
	_ = f.Close()
}

type directGramPair struct{ Gram, ID uint32 }

const directGramRunPairs32MiB = 4 * 1024 * 1024

func directGramRunLimit(runRecords int) int {
	if runRecords >= 64*1024 && runRecords < directGramRunPairs32MiB {
		return directGramRunPairs32MiB
	}
	return runRecords
}

func directWriteGramRun(path string, pairs []directGramPair) (int64, error) {
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Gram != pairs[j].Gram {
			return pairs[i].Gram < pairs[j].Gram
		}
		return pairs[i].ID < pairs[j].ID
	})
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriterSize(f, 256*1024)
	var b [8]byte
	var written int64
	for _, pair := range pairs {
		binary.LittleEndian.PutUint32(b[0:4], pair.Gram)
		binary.LittleEndian.PutUint32(b[4:8], pair.ID)
		if _, err := bw.Write(b[:]); err != nil {
			_ = f.Close()
			return written, err
		}
		written += 8
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return written, err
	}
	// These run files are owned scratch, never published or used for recovery;
	// the atomic target is synced after all sections and the table are complete.
	// Avoid one filesystem flush per PNGR/PNGC run (hundreds on the 1M gate).
	return written, f.Close()
}

// directBuildGramRunSets builds the PNGR and PNGC run files in a single pass
// over the record spool.  Each record's grams are emitted to the stored (PNGR)
// and omitted (PNGC) run sets according to the partition, halving the number of
// full-spool traversals versus building the sections independently.
func directBuildGramRunSets(ctx context.Context, finalPath, spoolDir string, maxPairs int, stored map[uint32]struct{}, omitted map[uint32]struct{}, owned *[]string) ([]directRunFile, []directRunFile, error) {
	if maxPairs <= 0 {
		maxPairs = directDefaultRunRecords
	}
	f, err := os.Open(finalPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 256*1024)
	storedPairs := make([]directGramPair, 0, min(maxPairs, 4096))
	omittedPairs := make([]directGramPair, 0, min(maxPairs, 4096))
	var storedRuns, omittedRuns []directRunFile
	flush := func(pairs []directGramPair, name string, runs []directRunFile) ([]directRunFile, error) {
		if len(pairs) == 0 {
			return runs, nil
		}
		path := filepath.Join(spoolDir, fmt.Sprintf("direct-%s-gram-run-%06d.tmp", strings.ToLower(name), len(runs)))
		bytes, err := directWriteGramRun(path, pairs)
		if err != nil {
			return runs, err
		}
		*owned = append(*owned, path)
		return append(runs, directRunFile{path: path, bytes: bytes}), nil
	}
	for id := 0; ; id++ {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}
		rec, readErr := readDirectSpoolRecord(r)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, nil, readErr
		}
		for _, gram := range uniqueTrigramKeys(strings.ToLower(rec.Name)) {
			if _, ok := stored[gram]; ok {
				storedPairs = append(storedPairs, directGramPair{Gram: gram, ID: uint32(id)})
				if len(storedPairs) >= maxPairs {
					storedRuns, err = flush(storedPairs, "PNGR", storedRuns)
					if err != nil {
						return nil, nil, err
					}
					storedPairs = make([]directGramPair, 0, min(maxPairs, 4096))
				}
			}
			if _, ok := omitted[gram]; ok {
				omittedPairs = append(omittedPairs, directGramPair{Gram: gram, ID: uint32(id)})
				if len(omittedPairs) >= maxPairs {
					omittedRuns, err = flush(omittedPairs, "PNGC", omittedRuns)
					if err != nil {
						return nil, nil, err
					}
					omittedPairs = make([]directGramPair, 0, min(maxPairs, 4096))
				}
			}
		}
	}
	storedRuns, err = flush(storedPairs, "PNGR", storedRuns)
	if err != nil {
		return nil, nil, err
	}
	omittedRuns, err = flush(omittedPairs, "PNGC", omittedRuns)
	if err != nil {
		return nil, nil, err
	}
	return storedRuns, omittedRuns, nil
}

type directGramHead struct {
	Pair directGramPair
	Run  int
}
type directGramHeap struct{ items []directGramHead }

func (h directGramHeap) Len() int { return len(h.items) }
func (h directGramHeap) Less(i, j int) bool {
	if h.items[i].Pair.Gram != h.items[j].Pair.Gram {
		return h.items[i].Pair.Gram < h.items[j].Pair.Gram
	}
	return h.items[i].Pair.ID < h.items[j].Pair.ID
}
func (h directGramHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *directGramHeap) Push(x any)   { h.items = append(h.items, x.(directGramHead)) }
func (h *directGramHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

func directWriteGramSectionFromRuns(ctx context.Context, cw *countingWriter, runs []directRunFile, omitted map[uint32]int, ranks []uint32, metadataMagic uint32, name, scratchDir string, owned *[]string, scratchHigh *int64) (indexSectionTableEntry, directSectionReport, error) {
	entryPath := filepath.Join(scratchDir, "direct-"+strings.ToLower(name)+"-entries.tmp")
	metaPath := filepath.Join(scratchDir, "direct-"+strings.ToLower(name)+"-meta.tmp")
	blobPath := filepath.Join(scratchDir, "direct-"+strings.ToLower(name)+"-blob.tmp")
	*owned = append(*owned, entryPath, metaPath, blobPath)
	ef, err := os.Create(entryPath)
	if err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	mf, err := os.Create(metaPath)
	if err != nil {
		_ = ef.Close()
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	bf, err := os.Create(blobPath)
	if err != nil {
		_ = ef.Close()
		_ = mf.Close()
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	ew := bufio.NewWriterSize(ef, 256*1024)
	mw := bufio.NewWriterSize(mf, 256*1024)
	bw := bufio.NewWriterSize(bf, 256*1024)
	files := make([]*os.File, len(runs))
	readers := make([]*bufio.Reader, len(runs))
	h := &directGramHeap{}
	heap.Init(h)
	for i, run := range runs {
		f, openErr := os.Open(run.path)
		if openErr != nil {
			return indexSectionTableEntry{}, directSectionReport{}, openErr
		}
		files[i] = f
		readers[i] = bufio.NewReaderSize(f, 256*1024)
		pair, readErr := directReadGramPair(readers[i])
		if readErr == nil {
			heap.Push(h, directGramHead{Pair: pair, Run: i})
		} else if !errors.Is(readErr, io.EOF) {
			return indexSectionTableEntry{}, directSectionReport{}, readErr
		}
	}
	defer func() {
		for _, f := range files {
			if f != nil {
				_ = f.Close()
			}
		}
	}()
	var entryCount, blockCount, blobBytes, blobOffset uint64
	var current, lastID, currentCount uint32
	var have, haveID bool
	var chunk []uint32
	var firstBlock uint64
	flushChunk := func() error {
		if len(chunk) == 0 {
			return nil
		}
		encoded := encodeDeltaUvarint32(chunk)
		if blobOffset+uint64(len(encoded)) > uint64(^uint32(0)) {
			return errors.New("direct gram blob exceeds format")
		}
		if _, err := bw.Write(encoded); err != nil {
			return err
		}
		var meta [28]byte
		binary.LittleEndian.PutUint64(meta[0:8], blobOffset)
		binary.LittleEndian.PutUint32(meta[8:12], uint32(len(encoded)))
		binary.LittleEndian.PutUint32(meta[12:16], uint32(len(chunk)))
		binary.LittleEndian.PutUint32(meta[16:20], chunk[0])
		binary.LittleEndian.PutUint32(meta[20:24], chunk[len(chunk)-1])
		minRank := uint32(^uint32(0))
		for _, id := range chunk[1:] {
			if rank := extRankOf(id, ranks); rank < minRank {
				minRank = rank
			}
		}
		if rank := extRankOf(chunk[0], ranks); rank < minRank {
			minRank = rank
		}
		if minRank == ^uint32(0) {
			minRank = 0
		}
		binary.LittleEndian.PutUint32(meta[24:28], minRank)
		if _, err := mw.Write(meta[:]); err != nil {
			return err
		}
		blobOffset += uint64(len(encoded))
		blobBytes += uint64(len(encoded))
		blockCount++
		chunk = chunk[:0]
		return nil
	}
	finish := func() error {
		if !have {
			return nil
		}
		if err := flushChunk(); err != nil {
			return err
		}
		var entry [16]byte
		binary.LittleEndian.PutUint32(entry[0:4], current)
		binary.LittleEndian.PutUint32(entry[4:8], currentCount)
		binary.LittleEndian.PutUint32(entry[8:12], uint32(firstBlock))
		binary.LittleEndian.PutUint32(entry[12:16], uint32(blockCount-firstBlock))
		if _, err := ew.Write(entry[:]); err != nil {
			return err
		}
		entryCount++
		return nil
	}
	for h.Len() > 0 {
		select {
		case <-ctx.Done():
			return indexSectionTableEntry{}, directSectionReport{}, ctx.Err()
		default:
		}
		head := heap.Pop(h).(directGramHead)
		pair := head.Pair
		next, readErr := directReadGramPair(readers[head.Run])
		if readErr == nil {
			heap.Push(h, directGramHead{Pair: next, Run: head.Run})
		} else if !errors.Is(readErr, io.EOF) {
			return indexSectionTableEntry{}, directSectionReport{}, readErr
		}
		if !have || pair.Gram != current {
			if err := finish(); err != nil {
				return indexSectionTableEntry{}, directSectionReport{}, err
			}
			current = pair.Gram
			firstBlock = blockCount
			lastID = ^uint32(0)
			currentCount = 0
			have = true
			haveID = false
		}
		if haveID && pair.ID == lastID {
			continue
		}
		lastID = pair.ID
		haveID = true
		currentCount++
		chunk = append(chunk, pair.ID)
		if len(chunk) >= 1024 {
			if err := flushChunk(); err != nil {
				return indexSectionTableEntry{}, directSectionReport{}, err
			}
		}
	}
	if err := finish(); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := ew.Flush(); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := mw.Flush(); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := bw.Flush(); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	// Owned scratch section parts; copied into the atomic target and removed.
	_ = ef.Close()
	_ = mf.Close()
	_ = bf.Close()
	if entryCount > uint64(^uint32(0)) || blockCount > uint64(^uint32(0)) || blobBytes > uint64(^uint32(0)) {
		return indexSectionTableEntry{}, directSectionReport{}, errors.New("direct gram counts exceed format")
	}
	if err := writeAlignment(cw, 8); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	sectionOffset := uint64(cw.n)
	var header [16]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(entryCount))
	binary.LittleEndian.PutUint32(header[8:12], uint32(blockCount))
	binary.LittleEndian.PutUint32(header[12:16], uint32(blobBytes))
	if _, err := cw.Write(header[:]); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := directCopyFile(cw, entryPath); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := directCopyFile(cw, metaPath); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := directCopyFile(cw, blobPath); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[0:4], metadataMagic)
	binary.LittleEndian.PutUint32(trailer[4:8], uint32(len(omitted)))
	if _, err := cw.Write(trailer[:]); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if len(omitted) > 0 {
		keys := make([]uint32, 0, len(omitted))
		for gram := range omitted {
			keys = append(keys, gram)
		}
		sortUint32s(keys)
		var pair [8]byte
		for _, gram := range keys {
			binary.LittleEndian.PutUint32(pair[0:4], gram)
			binary.LittleEndian.PutUint32(pair[4:8], uint32(omitted[gram]))
			if _, err := cw.Write(pair[:]); err != nil {
				return indexSectionTableEntry{}, directSectionReport{}, err
			}
		}
	}
	for _, run := range runs {
		_ = os.Remove(run.path)
	}
	_ = os.Remove(entryPath)
	_ = os.Remove(metaPath)
	_ = os.Remove(blobPath)
	var scratch int64
	for _, run := range runs {
		scratch += run.bytes
	}
	if info, statErr := os.Stat(entryPath); statErr == nil {
		scratch += info.Size()
	}
	if scratch > *scratchHigh {
		*scratchHigh = scratch
	}
	tag := indexSectionPNGR
	if name == "PNGC" {
		tag = indexSectionPNGC
	}
	entry := indexSectionTableEntry{tag: tag, offset: sectionOffset, length: uint64(cw.n) - sectionOffset}
	return entry, directSectionReport{Name: name, Tag: tag, Runs: len(runs), Bytes: int64(entry.length), ScratchBytes: scratch}, nil
}

func directReadGramPair(r *bufio.Reader) (directGramPair, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return directGramPair{}, err
	}
	return directGramPair{Gram: binary.LittleEndian.Uint32(b[0:4]), ID: binary.LittleEndian.Uint32(b[4:8])}, nil
}

// directWriteGramSectionStreaming preserves the existing gram wire format
// while keeping encoded posting bytes off the Go heap.  The gram dictionary is
// already bounded by the current family; entries, block metadata, and delta
// payloads are staged in owned files and copied to the atomic target only
// after their counts and offsets are known.
func directWriteGramSectionStreaming(ctx context.Context, cw *countingWriter, postings map[uint32][]uint32, recordCount int, metadataMagic uint32, name, scratchDir string, owned *[]string, scratchHigh *int64) (indexSectionTableEntry, directSectionReport, error) {
	keys := make([]uint32, 0, len(postings))
	for gram, ids := range postings {
		if len(ids) > 0 {
			keys = append(keys, gram)
		}
	}
	sortUint32s(keys)
	entriesPath := filepath.Join(scratchDir, "direct-"+strings.ToLower(name)+"-entries.tmp")
	metaPath := filepath.Join(scratchDir, "direct-"+strings.ToLower(name)+"-meta.tmp")
	blobPath := filepath.Join(scratchDir, "direct-"+strings.ToLower(name)+"-blob.tmp")
	*owned = append(*owned, entriesPath, metaPath, blobPath)
	entriesFile, err := os.Create(entriesPath)
	if err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	metaFile, err := os.Create(metaPath)
	if err != nil {
		_ = entriesFile.Close()
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	blobFile, err := os.Create(blobPath)
	if err != nil {
		_ = entriesFile.Close()
		_ = metaFile.Close()
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	entryWriter := bufio.NewWriterSize(entriesFile, 256*1024)
	metaWriter := bufio.NewWriterSize(metaFile, 256*1024)
	blobWriter := bufio.NewWriterSize(blobFile, 256*1024)
	var blockCount uint64
	var blobBytes uint64
	var entryCount uint64
	var offset uint64
	for _, gram := range keys {
		select {
		case <-ctx.Done():
			_ = entriesFile.Close()
			_ = metaFile.Close()
			_ = blobFile.Close()
			return indexSectionTableEntry{}, directSectionReport{}, ctx.Err()
		default:
		}
		ids := uniqueSortedUint32s(postings[gram])
		if len(ids) == 0 {
			continue
		}
		firstBlock := blockCount
		for start := 0; start < len(ids); start += 1024 {
			end := min(len(ids), start+1024)
			chunk := ids[start:end]
			encoded := encodeDeltaUvarint32(chunk)
			if offset+uint64(len(encoded)) > uint64(^uint32(0)) || blockCount >= uint64(^uint32(0)) {
				_ = entriesFile.Close()
				_ = metaFile.Close()
				_ = blobFile.Close()
				return indexSectionTableEntry{}, directSectionReport{}, errors.New("direct gram spool exceeds format")
			}
			if _, err := blobWriter.Write(encoded); err != nil {
				_ = entriesFile.Close()
				_ = metaFile.Close()
				_ = blobFile.Close()
				return indexSectionTableEntry{}, directSectionReport{}, err
			}
			minRank := chunk[0]
			for _, id := range chunk[1:] {
				if id < minRank {
					minRank = id
				}
			}
			var meta [28]byte
			binary.LittleEndian.PutUint64(meta[0:8], offset)
			binary.LittleEndian.PutUint32(meta[8:12], uint32(len(encoded)))
			binary.LittleEndian.PutUint32(meta[12:16], uint32(len(chunk)))
			binary.LittleEndian.PutUint32(meta[16:20], chunk[0])
			binary.LittleEndian.PutUint32(meta[20:24], chunk[len(chunk)-1])
			binary.LittleEndian.PutUint32(meta[24:28], minRank)
			if _, err := metaWriter.Write(meta[:]); err != nil {
				_ = entriesFile.Close()
				_ = metaFile.Close()
				_ = blobFile.Close()
				return indexSectionTableEntry{}, directSectionReport{}, err
			}
			offset += uint64(len(encoded))
			blobBytes += uint64(len(encoded))
			blockCount++
		}
		var entry [16]byte
		binary.LittleEndian.PutUint32(entry[0:4], gram)
		binary.LittleEndian.PutUint32(entry[4:8], uint32(len(ids)))
		binary.LittleEndian.PutUint32(entry[8:12], uint32(firstBlock))
		binary.LittleEndian.PutUint32(entry[12:16], uint32(blockCount-firstBlock))
		if _, err := entryWriter.Write(entry[:]); err != nil {
			_ = entriesFile.Close()
			_ = metaFile.Close()
			_ = blobFile.Close()
			return indexSectionTableEntry{}, directSectionReport{}, err
		}
		entryCount++
	}
	if entryCount > uint64(^uint32(0)) || blockCount > uint64(^uint32(0)) || blobBytes > uint64(^uint32(0)) {
		_ = entriesFile.Close()
		_ = metaFile.Close()
		_ = blobFile.Close()
		return indexSectionTableEntry{}, directSectionReport{}, errors.New("direct gram counts exceed format")
	}
	if err := entryWriter.Flush(); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := metaWriter.Flush(); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := blobWriter.Flush(); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	_ = entriesFile.Close()
	_ = metaFile.Close()
	_ = blobFile.Close()
	if err := writeAlignment(cw, 8); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	sectionOffset := uint64(cw.n)
	var header [16]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(entryCount))
	binary.LittleEndian.PutUint32(header[4:8], 0)
	binary.LittleEndian.PutUint32(header[8:12], uint32(blockCount))
	binary.LittleEndian.PutUint32(header[12:16], uint32(blobBytes))
	if _, err := cw.Write(header[:]); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := directCopyFile(cw, entriesPath); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := directCopyFile(cw, metaPath); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := directCopyFile(cw, blobPath); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[0:4], metadataMagic)
	binary.LittleEndian.PutUint32(trailer[4:8], 0)
	if _, err := cw.Write(trailer[:]); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	entry := indexSectionTableEntry{tag: map[string]uint32{"PNGR": indexSectionPNGR, "PNGC": indexSectionPNGC}[name], offset: sectionOffset, length: uint64(cw.n) - sectionOffset}
	scratch := int64(blobBytes) + int64(blockCount*28) + int64(entryCount*16)
	if scratch > *scratchHigh {
		*scratchHigh = scratch
	}
	return entry, directSectionReport{Name: name, Tag: entry.tag, Runs: 0, Bytes: int64(entry.length), ScratchBytes: scratch}, nil
}

// directAuxMaps carries every posting family collected in one spool pass so
// the PEXT/PCMP/PATR/PNGR sections no longer each re-read the record spool.
type directAuxMaps struct {
	ext     map[string][]uint32
	comp    map[string][]uint32
	attrs   map[uint32][]uint32
	gramCnt map[uint32]int
}

// directCollectAuxMaps reads the final spool once and produces the extension,
// component, attribute, and filename-gram count maps together.  The per-key
// posting lists are sorted and deduplicated, matching the individual
// collectors' output exactly.
func directCollectAuxMaps(ctx context.Context, finalPath string) (*directAuxMaps, error) {
	f, err := os.Open(finalPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := &directAuxMaps{
		ext:     make(map[string][]uint32),
		comp:    make(map[string][]uint32),
		attrs:   make(map[uint32][]uint32),
		gramCnt: make(map[uint32]int),
	}
	r := bufio.NewReaderSize(f, 256*1024)
	for id := 0; ; id++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		rec, readErr := readDirectSpoolRecord(r)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		if ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(rec.Name), ".")); ext != "" {
			out.ext[ext] = append(out.ext[ext], uint32(id))
		}
		if rec.Mode&uint32(os.ModeDir) != 0 {
			out.attrs[0x10] = append(out.attrs[0x10], uint32(id))
			name := strings.ToLower(rec.Name)
			if name != "" && name != "." {
				out.comp[name] = append(out.comp[name], uint32(id))
			}
		}
		for _, gram := range uniqueTrigramKeys(strings.ToLower(rec.Name)) {
			out.gramCnt[gram]++
		}
	}
	for key, ids := range out.ext {
		out.ext[key] = uniqueSortedUint32s(ids)
	}
	for key, ids := range out.comp {
		out.comp[key] = uniqueSortedUint32s(ids)
	}
	return out, nil
}

func directReadUint32Vector(path string, expected int) ([]uint32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) != expected*4 {
		return nil, fmt.Errorf("direct rank vector bytes=%d want=%d", len(data), expected*4)
	}
	return mappedUint32Slice(data), nil
}

func directBuildComponentPostingRankBounds(postings map[string][]uint32, rankPaths []string, recordCount int) (postingRankBounds, error) {
	if len(rankPaths) != 6 {
		return postingRankBounds{}, fmt.Errorf("direct component bounds rank families=%d want=6", len(rankPaths))
	}
	keys := make([]string, 0, len(postings))
	for key, ids := range postings {
		if key != "" && len(ids) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([][]uint32, len(rankPaths))
	for family, path := range rankPaths {
		ranks, err := directReadUint32Vector(path, recordCount)
		if err != nil {
			return postingRankBounds{}, err
		}
		for _, key := range keys {
			ids := uniqueSortedUint32s(append([]uint32(nil), postings[key]...))
			for start := 0; start < len(ids); start += 1024 {
				parts[family] = append(parts[family], minRankForIDs(ids[start:min(len(ids), start+1024)], ranks))
			}
		}
	}
	if len(parts[0]) == 0 {
		return postingRankBounds{}, nil
	}
	return postingRankBounds{BlockCount: len(parts[0]), Name: parts[0], Size: parts[1], Modified: parts[2], Extension: parts[3], Type: parts[4], Path: parts[5]}, nil
}

func directPartitionNameGrams(counts map[uint32]int, maxPostingCount int) (map[uint32]struct{}, map[uint32]int) {
	stored := make(map[uint32]struct{}, len(counts))
	omitted := make(map[uint32]int)
	for gram, count := range counts {
		if count > maxPostingCount {
			omitted[gram] = count
		} else if count > 0 {
			stored[gram] = struct{}{}
		}
	}
	return stored, omitted
}

func directGramKeys(counts map[uint32]int) map[uint32]struct{} {
	keys := make(map[uint32]struct{}, len(counts))
	for gram := range counts {
		keys[gram] = struct{}{}
	}
	return keys
}

func directMakeGramIndex(postings map[uint32][]uint32, recordCount int, union bool) *compressedTrigramIndex {
	segment := trigramSegment{postings: make(map[uint32]compressedPosting)}
	counts := make(map[uint32]int, len(postings))
	for gram, ids := range postings {
		ids = uniqueSortedUint32s(ids)
		segment.postings[gram] = compressedPosting{count: len(ids), data: encodeDeltaUvarint32(ids)}
		counts[gram] = len(ids)
	}
	return &compressedTrigramIndex{segments: []trigramSegment{segment}, counts: counts, gramCountsComplete: true, gramUnionComplete: union, gramSize: 3, recordCount: recordCount}
}

func directZeroPostingBounds(data []byte) []byte {
	if len(data) < 16 {
		return nil
	}
	blocks := int(binary.LittleEndian.Uint32(data[8:]))
	if blocks <= 0 {
		return nil
	}
	zeros := make([]uint32, blocks)
	return encodeUint32Section(zeros, zeros, zeros, zeros, zeros, zeros)
}
