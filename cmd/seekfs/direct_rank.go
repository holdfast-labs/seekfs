package main

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
	"sort"
	"strings"
)

func directScanNames(finalPath, tokenPath string) (int64, int, error) {
	f, err := os.Open(finalPath)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	var tokens *os.File
	var tokenWriter *bufio.Writer
	if tokenPath != "" {
		tokens, err = os.Create(tokenPath)
		if err != nil {
			return 0, 0, err
		}
		defer tokens.Close()
		tokenWriter = bufio.NewWriterSize(tokens, 256*1024)
	}
	r := bufio.NewReaderSize(f, 256*1024)
	var offset uint64
	var count int
	for {
		rec, readErr := readDirectSpoolRecord(r)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, 0, readErr
		}
		if len(rec.Name) > int(^uint16(0)) || offset > uint64(^uint32(0))-uint64(len(rec.Name)) {
			return 0, 0, errors.New("direct name table exceeds on-disk limits")
		}
		if tokenWriter != nil {
			var entry [6]byte
			binary.LittleEndian.PutUint32(entry[:4], uint32(offset))
			binary.LittleEndian.PutUint16(entry[4:], uint16(len(rec.Name)))
			if _, err := tokenWriter.Write(entry[:]); err != nil {
				return 0, 0, err
			}
		}
		offset += uint64(len(rec.Name))
		count++
	}
	if tokenWriter != nil {
		if err := tokenWriter.Flush(); err != nil {
			return 0, 0, err
		}
	}
	// The name-table temp is owned scratch consumed later in this process.
	return int64(offset), count, nil
}

func directFoldName(rec directRecord) string { return strings.ToLower(rec.Name) }

func directSignedOrderKey(value int64) string {
	var b [8]byte
	u := uint64(value) ^ (uint64(1) << 63)
	binary.BigEndian.PutUint64(b[:], u)
	return string(b[:])
}

func directLowerExt(rec directRecord) string {
	ext := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
	return strings.ToLower(ext)
}

func directLowerPath(rec directRecord) string {
	if rec.Path != "" {
		return strings.ToLower(filepath.Clean(rec.Path))
	}
	return strings.ToLower(rec.Name)
}

func directRankSpecs() []directRankSpec {
	return []directRankSpec{
		{Tag: indexSectionRANK, Name: "RANK", Key: directFoldName},
		{Tag: indexSectionSRNK, Name: "SRNK", Key: func(rec directRecord) string {
			return directSignedOrderKey(rec.Size) + "\x00" + strings.ToLower(rec.Name)
		}},
		{Tag: indexSectionMRNK, Name: "MRNK", Key: func(rec directRecord) string {
			if rec.ModUnix == 0 {
				return "\x01" + strings.ToLower(rec.Name)
			}
			var b [8]byte
			u := uint64(rec.ModUnix) ^ (uint64(1) << 63)
			binary.BigEndian.PutUint64(b[:], ^u)
			return "\x00" + string(b[:]) + "\x00" + strings.ToLower(rec.Name)
		}},
		{Tag: indexSectionERNK, Name: "ERNK", Key: func(rec directRecord) string {
			return directLowerExt(rec) + "\x00" + strings.ToLower(rec.Name)
		}},
		{Tag: indexSectionTRNK, Name: "TRNK", Key: func(rec directRecord) string {
			kind := byte(1)
			if rec.Mode&uint32(os.ModeDir) != 0 {
				kind = 0
			}
			return string([]byte{kind}) + "\x00" + strings.ToLower(rec.Name)
		}},
		{Tag: indexSectionPRNK, Name: "PRNK", Key: func(rec directRecord) string {
			return directLowerPath(rec)
		}},
	}
}

func directBuildRankRuns(ctx context.Context, finalPath, spoolDir string, maxRecords int, spec directRankSpec, owned *[]string) ([]directRunFile, int, int64, error) {
	f, err := os.Open(finalPath)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	if maxRecords <= 0 {
		maxRecords = directDefaultRunRecords
	}
	r := bufio.NewReaderSize(f, 256*1024)
	chunk := make([]directRankItem, 0, min(maxRecords, 4096))
	var runs []directRunFile
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		sort.Slice(chunk, func(i, j int) bool {
			if chunk[i].Key != chunk[j].Key {
				return chunk[i].Key < chunk[j].Key
			}
			return chunk[i].ID < chunk[j].ID
		})
		path := filepath.Join(spoolDir, fmt.Sprintf("direct-rank-%s-%06d.tmp", spec.Name, len(runs)))
		run, err := os.Create(path)
		if err != nil {
			return err
		}
		bw := bufio.NewWriterSize(run, 256*1024)
		var bytesWritten int64
		for _, item := range chunk {
			var id [4]byte
			binary.LittleEndian.PutUint32(id[:], item.ID)
			if _, err := bw.Write(id[:]); err != nil {
				_ = run.Close()
				return err
			}
			bytesWritten += 4
			if uint64(len(item.Key)) > uint64(^uint32(0)) {
				_ = run.Close()
				return errors.New("direct rank key too large")
			}
			var n [4]byte
			binary.LittleEndian.PutUint32(n[:], uint32(len(item.Key)))
			if _, err := bw.Write(n[:]); err != nil {
				_ = run.Close()
				return err
			}
			if _, err := io.WriteString(bw, item.Key); err != nil {
				_ = run.Close()
				return err
			}
			bytesWritten += int64(4 + len(item.Key))
		}
		if err := bw.Flush(); err != nil {
			_ = run.Close()
			return err
		}
		if err := run.Close(); err != nil {
			return err
		}
		*owned = append(*owned, path)
		runs = append(runs, directRunFile{path: path, bytes: bytesWritten})
		chunk = make([]directRankItem, 0, min(maxRecords, 4096))
		return nil
	}
	var id uint32
	var count int
	for {
		select {
		case <-ctx.Done():
			return nil, 0, 0, ctx.Err()
		default:
		}
		rec, readErr := readDirectSpoolRecord(r)
		if errors.Is(readErr, io.EOF) {
			if err := flush(); err != nil {
				return nil, 0, 0, err
			}
			var maxBytes int64
			for _, run := range runs {
				maxBytes = max(maxBytes, run.bytes)
			}
			return runs, count, maxBytes, nil
		}
		if readErr != nil {
			return nil, 0, 0, readErr
		}
		if !rec.Deleted() {
			chunk = append(chunk, directRankItem{Key: spec.Key(rec), ID: id})
			count++
		}
		id++
		if len(chunk) >= maxRecords {
			if err := flush(); err != nil {
				return nil, 0, 0, err
			}
		}
	}
}

// directRecord has no deletion field in the first direct slice.  Keeping
// this method makes the rank writer's contract explicit for the next source
// slice, which will carry tombstones from USN/MFT.
func (r directRecord) Deleted() bool { return false }

func directReadRankItem(r *bufio.Reader) (directRankItem, error) {
	var idBytes [4]byte
	if _, err := io.ReadFull(r, idBytes[:]); err != nil {
		return directRankItem{}, err
	}
	var lenBytes [4]byte
	if _, err := io.ReadFull(r, lenBytes[:]); err != nil {
		return directRankItem{}, err
	}
	keyLen := binary.LittleEndian.Uint32(lenBytes[:])
	if uint64(keyLen) > uint64(^uint(0)>>1) {
		return directRankItem{}, errors.New("direct rank key too large")
	}
	key := make([]byte, int(keyLen))
	if _, err := io.ReadFull(r, key); err != nil {
		return directRankItem{}, err
	}
	return directRankItem{ID: binary.LittleEndian.Uint32(idBytes[:]), Key: string(key)}, nil
}

// directComputeRankFamily merges one rank family's external-sort runs into
// its section bytes plus a rank-by-id array, writing both to private temp
// files.  It is safe to run for multiple families concurrently: each family
// touches only its own runs and its own temp paths, and the output file order
// is preserved by the caller's serial emission phase.
func directComputeRankFamily(ctx context.Context, tag uint32, runs []directRunFile, recordCount, liveCount int, sectionPath, rankPath string, owned *[]string) (int64, error) {
	section, err := os.Create(sectionPath)
	if err != nil {
		return 0, err
	}
	*owned = append(*owned, sectionPath)
	sw := bufio.NewWriterSize(section, 256*1024)
	written := int64(0)
	if err := binary.Write(sw, binary.LittleEndian, uint32(liveCount)); err != nil {
		_ = section.Close()
		return 0, err
	}
	written += 4
	// The rank-by-id array is filled in memory during the merge and written
	// once per family; the array is bounded by recordCount*4 and released when
	// the family completes.  Multiple families may be in flight, so the caller
	// bounds the worker pool.
	ranks := make([]byte, recordCount*4)
	readers := make([]*bufio.Reader, len(runs))
	files := make([]*os.File, len(runs))
	h := &directRankHeap{}
	heap.Init(h)
	for i, run := range runs {
		f, openErr := os.Open(run.path)
		if openErr != nil {
			_ = section.Close()
			return 0, openErr
		}
		files[i] = f
		readers[i] = bufio.NewReaderSize(f, 256*1024)
		item, readErr := directReadRankItem(readers[i])
		if readErr == nil {
			heap.Push(h, itemWithRun{item: item, run: i})
		} else if !errors.Is(readErr, io.EOF) {
			_ = section.Close()
			return 0, readErr
		}
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	for rank := 0; h.Len() > 0; rank++ {
		select {
		case <-ctx.Done():
			_ = section.Close()
			return 0, ctx.Err()
		default:
		}
		head := heap.Pop(h).(itemWithRun)
		if err := binary.Write(sw, binary.LittleEndian, head.item.ID); err != nil {
			_ = section.Close()
			return 0, err
		}
		written += 4
		binary.LittleEndian.PutUint32(ranks[head.item.ID*4:head.item.ID*4+4], uint32(rank))
		next, readErr := directReadRankItem(readers[head.run])
		if readErr == nil {
			heap.Push(h, itemWithRun{item: next, run: head.run})
		} else if !errors.Is(readErr, io.EOF) {
			_ = section.Close()
			return 0, readErr
		}
	}
	// The section carries the order alone: the rank-by-id vector is the
	// exact inverse and is rebuilt at load (see decodeRankSection), halving
	// the six rank sections. The rank-by-id array still goes to rankPath
	// for the subtree minima and posting bounds built later.
	if err := sw.Flush(); err != nil {
		_ = section.Close()
		return 0, err
	}
	if err := section.Close(); err != nil {
		return 0, err
	}
	// Persist the rank-by-id array for the retained copy the caller needs for
	// subtree/bounds.  One sequential write replaces N random WriteAt calls.
	rankFile, err := os.Create(rankPath)
	if err != nil {
		return 0, err
	}
	*owned = append(*owned, rankPath)
	if _, err := rankFile.Write(ranks); err != nil {
		_ = rankFile.Close()
		return 0, err
	}
	if err := rankFile.Close(); err != nil {
		return 0, err
	}
	ranks = nil
	return written, nil
}

// directEmitRankSection streams a computed rank-family section into the
// output writer with alignment, preserving the section byte layout exactly.
func directEmitRankSection(cw *countingWriter, tag uint32, sectionPath string) (indexSectionTableEntry, error) {
	if err := writeAlignment(cw, 8); err != nil {
		return indexSectionTableEntry{}, err
	}
	offset := uint64(cw.n)
	if err := copyFileToWriter(cw, sectionPath); err != nil {
		return indexSectionTableEntry{}, err
	}
	return indexSectionTableEntry{tag: tag, offset: offset, length: uint64(cw.n) - offset}, nil
}

type itemWithRun struct {
	item directRankItem
	run  int
}

func (h itemWithRun) less(other itemWithRun) bool {
	if h.item.Key != other.item.Key {
		return h.item.Key < other.item.Key
	}
	return h.item.ID < other.item.ID
}
