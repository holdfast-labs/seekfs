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
)

func writeDirectSpoolRecord(w io.Writer, rec directRecord) (int64, error) {
	if uint64(len(rec.Name)) > uint64(^uint32(0)) || uint64(len(rec.Path)) > uint64(^uint32(0)) {
		return 0, errors.New("direct record name or path too large")
	}
	var header [directSpoolHeaderBytes]byte
	binary.LittleEndian.PutUint64(header[0:8], rec.FRN)
	binary.LittleEndian.PutUint64(header[8:16], rec.ParentFRN)
	binary.LittleEndian.PutUint32(header[16:20], rec.Mode)
	binary.LittleEndian.PutUint64(header[20:28], uint64(rec.Size))
	binary.LittleEndian.PutUint64(header[28:36], uint64(rec.ModUnix))
	binary.LittleEndian.PutUint32(header[36:40], uint32(len(rec.Name)))
	binary.LittleEndian.PutUint32(header[40:44], uint32(len(rec.Path)))
	binary.LittleEndian.PutUint16(header[44:46], rec.Seq)
	n, err := w.Write(header[:])
	if err != nil {
		return int64(n), err
	}
	if n != len(header) {
		return int64(n), io.ErrShortWrite
	}
	m, err := io.WriteString(w, rec.Name)
	if err != nil {
		return int64(n + m), err
	}
	if m != len(rec.Name) {
		return int64(n + m), io.ErrShortWrite
	}
	p, err := io.WriteString(w, rec.Path)
	if err != nil {
		return int64(n + m + p), err
	}
	if p != len(rec.Path) {
		return int64(n + m + p), io.ErrShortWrite
	}
	return int64(n + m + p), nil
}

func readDirectSpoolRecord(r *bufio.Reader) (directRecord, error) {
	var header [directSpoolHeaderBytes]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return directRecord{}, err
	}
	nameLen := binary.LittleEndian.Uint32(header[36:40])
	pathLen := binary.LittleEndian.Uint32(header[40:44])
	if uint64(nameLen) > uint64(^uint(0)>>1) {
		return directRecord{}, errors.New("direct spool name too large")
	}
	name := make([]byte, int(nameLen))
	if _, err := io.ReadFull(r, name); err != nil {
		return directRecord{}, err
	}
	if uint64(pathLen) > uint64(^uint(0)>>1) {
		return directRecord{}, errors.New("direct spool path too large")
	}
	path := make([]byte, int(pathLen))
	if _, err := io.ReadFull(r, path); err != nil {
		return directRecord{}, err
	}
	return directRecord{
		FRN:       binary.LittleEndian.Uint64(header[0:8]),
		ParentFRN: binary.LittleEndian.Uint64(header[8:16]),
		Mode:      binary.LittleEndian.Uint32(header[16:20]),
		Size:      int64(binary.LittleEndian.Uint64(header[20:28])),
		ModUnix:   int64(binary.LittleEndian.Uint64(header[28:36])),
		Name:      string(name),
		Path:      string(path),
		Seq:       binary.LittleEndian.Uint16(header[44:46]),
	}, nil
}

func directRecordLess(a, b directRecord) bool {
	if a.FRN != b.FRN {
		return a.FRN < b.FRN
	}
	if a.ParentFRN != b.ParentFRN {
		return a.ParentFRN < b.ParentFRN
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.Mode != b.Mode {
		return a.Mode < b.Mode
	}
	if a.Size != b.Size {
		return a.Size < b.Size
	}
	return a.ModUnix < b.ModUnix
}

func directWriteRun(path string, records []directRecord) (int64, error) {
	sort.Slice(records, func(i, j int) bool { return directRecordLess(records[i], records[j]) })
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	bw := bufio.NewWriterSize(f, 256*1024)
	var written int64
	for _, rec := range records {
		n, err := writeDirectSpoolRecord(bw, rec)
		written += n
		if err != nil {
			return written, err
		}
	}
	if err := bw.Flush(); err != nil {
		return written, err
	}
	// The run file is owned scratch: it is merged and removed within the same
	// process, never published, so no per-run filesystem flush is needed.
	return written, f.Close()
}

func directBuildRuns(ctx context.Context, source directRecordSource, spoolDir string, maxRecords int, maxBytes int64, owned *[]string) ([]directRunFile, int64, error) {
	if maxRecords <= 0 {
		maxRecords = directDefaultRunRecords
	}
	if maxBytes <= 0 {
		maxBytes = directDefaultRunBytes
	}
	var runs []directRunFile
	chunk := make([]directRecord, 0, min(maxRecords, 4096))
	var chunkBytes int64
	var maxChunkBytes int64
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		maxChunkBytes = max(maxChunkBytes, chunkBytes)
		path := filepath.Join(spoolDir, fmt.Sprintf("direct-run-%06d.tmp", len(runs)))
		bytes, err := directWriteRun(path, chunk)
		if err != nil {
			return err
		}
		*owned = append(*owned, path)
		runs = append(runs, directRunFile{path: path, bytes: bytes})
		chunk = make([]directRecord, 0, min(maxRecords, 4096))
		chunkBytes = 0
		return nil
	}
	for {
		rec, err := source.Next(ctx)
		if err == io.EOF {
			if err := flush(); err != nil {
				return nil, 0, err
			}
			return runs, maxChunkBytes, nil
		}
		if err != nil {
			return nil, 0, err
		}
		if len(rec.Name) > int(^uint16(0)) {
			return nil, 0, errors.New("direct record name exceeds v9 token limit")
		}
		chunk = append(chunk, rec)
		chunkBytes += int64(directSpoolHeaderBytes + len(rec.Name) + len(rec.Path))
		if len(chunk) >= maxRecords || chunkBytes >= maxBytes {
			if err := flush(); err != nil {
				return nil, 0, err
			}
		}
	}
}

func directMergeRuns(ctx context.Context, runs []directRunFile, finalPath, frnPath string, owned *[]string) (int, int64, error) {
	if len(runs) == 0 {
		f, err := os.Create(finalPath)
		if err != nil {
			return 0, 0, err
		}
		_ = f.Close()
		f, err = os.Create(frnPath)
		if err == nil {
			_ = f.Close()
		}
		*owned = append(*owned, finalPath, frnPath)
		return 0, 0, err
	}
	files := make([]*os.File, len(runs))
	readers := make([]*bufio.Reader, len(runs))
	for i, run := range runs {
		f, err := os.Open(run.path)
		if err != nil {
			for _, opened := range files {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return 0, 0, err
		}
		files[i] = f
		readers[i] = bufio.NewReaderSize(f, 256*1024)
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()

	final, err := os.Create(finalPath)
	if err != nil {
		return 0, 0, err
	}
	frns, err := os.Create(frnPath)
	if err != nil {
		_ = final.Close()
		return 0, 0, err
	}
	*owned = append(*owned, finalPath, frnPath)
	bw := bufio.NewWriterSize(final, 256*1024)
	frnWriter := bufio.NewWriterSize(frns, 256*1024)
	h := &directRunHeap{}
	heap.Init(h)
	for i, reader := range readers {
		rec, readErr := readDirectSpoolRecord(reader)
		if readErr == nil {
			heap.Push(h, directRunHead{rec: rec, run: i})
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, readErr
		}
	}
	var lastFRN uint64
	var haveLast bool
	var count int
	var written int64
	for h.Len() > 0 {
		select {
		case <-ctx.Done():
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, ctx.Err()
		default:
		}
		head := heap.Pop(h).(directRunHead)
		if haveLast && head.rec.FRN == lastFRN {
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, errDirectDuplicateFRN
		}
		haveLast = true
		lastFRN = head.rec.FRN
		n, writeErr := writeDirectSpoolRecord(bw, head.rec)
		written += n
		if writeErr != nil {
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, writeErr
		}
		var frnBytes [8]byte
		binary.LittleEndian.PutUint64(frnBytes[:], head.rec.FRN)
		if _, writeErr = frnWriter.Write(frnBytes[:]); writeErr != nil {
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, writeErr
		}
		count++
		next, readErr := readDirectSpoolRecord(readers[head.run])
		if readErr == nil {
			heap.Push(h, directRunHead{rec: next, run: head.run})
		} else if !errors.Is(readErr, io.EOF) {
			_ = final.Close()
			_ = frns.Close()
			return 0, 0, readErr
		}
	}
	if err := bw.Flush(); err != nil {
		_ = final.Close()
		_ = frns.Close()
		return 0, 0, err
	}
	if err := frnWriter.Flush(); err != nil {
		_ = final.Close()
		_ = frns.Close()
		return 0, 0, err
	}
	if err := final.Close(); err != nil {
		_ = frns.Close()
		return 0, 0, err
	}
	if err := frns.Close(); err != nil {
		return 0, 0, err
	}
	return count, written, nil
}

func directLookupIDMapped(frns []uint64, frn uint64) int32 {
	if frn == 0 || len(frns) == 0 {
		return -1
	}
	lo, hi := 0, len(frns)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if frns[mid] < frn {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= len(frns) || frns[lo] != frn {
		return -1
	}
	return int32(lo)
}

// directMapFRNs maps the merged FRN file for in-memory parent lookups.  The
// caller must close the returned mapped file.  A nil mapping is returned when
// the file is empty (no records).
func directMapFRNs(path string, count int) (*mappedIndexFile, []uint64, error) {
	if count == 0 {
		return nil, nil, nil
	}
	m, err := mapIndexFile(path)
	if err != nil {
		return nil, nil, err
	}
	frns := mappedUint64Slice(m.data)
	if len(frns) < count {
		_ = m.close()
		return nil, nil, fmt.Errorf("direct FRN map bytes=%d want=%d", len(frns), count)
	}
	frns = frns[:count]
	return m, frns, nil
}
