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

type directChildPair struct {
	Parent uint32
	Child  uint32
}

func directWriteChildRun(path string, pairs []directChildPair) (int64, error) {
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Parent != pairs[j].Parent {
			return pairs[i].Parent < pairs[j].Parent
		}
		return pairs[i].Child < pairs[j].Child
	})
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriterSize(f, 256*1024)
	var buf [8]byte
	var written int64
	for _, pair := range pairs {
		binary.LittleEndian.PutUint32(buf[0:4], pair.Parent)
		binary.LittleEndian.PutUint32(buf[4:8], pair.Child)
		if _, err := bw.Write(buf[:]); err != nil {
			_ = f.Close()
			return written, err
		}
		written += int64(len(buf))
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return written, err
	}
	// Owned scratch; merged and removed within the same process.
	return written, f.Close()
}

type directChildHead struct {
	Pair directChildPair
	Run  int
}

type directChildHeap struct{ items []directChildHead }

func (h directChildHeap) Len() int { return len(h.items) }
func (h directChildHeap) Less(i, j int) bool {
	a, b := h.items[i].Pair, h.items[j].Pair
	if a.Parent != b.Parent {
		return a.Parent < b.Parent
	}
	return a.Child < b.Child
}
func (h directChildHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *directChildHeap) Push(x any)   { h.items = append(h.items, x.(directChildHead)) }
func (h *directChildHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

func directReadChildPair(r *bufio.Reader) (directChildPair, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return directChildPair{}, err
	}
	return directChildPair{Parent: binary.LittleEndian.Uint32(buf[0:4]), Child: binary.LittleEndian.Uint32(buf[4:8])}, nil
}

func directCheckParentCycles(ctx context.Context, parentPath string, recordCount int) error {
	data, err := os.ReadFile(parentPath)
	if err != nil {
		return err
	}
	if len(data) < recordCount*4 {
		return errors.New("direct topology parents file too short")
	}
	state := make([]byte, recordCount)
	stamp := make([]uint32, recordCount)
	var walkID uint32
	readParent := func(id int) (int32, error) {
		value := binary.LittleEndian.Uint32(data[id*4:])
		if value == ^uint32(0) {
			return -1, nil
		}
		if value >= uint32(recordCount) {
			return -1, errors.New("direct topology parent ID out of range")
		}
		return int32(value), nil
	}
	for start := 0; start < recordCount; start++ {
		if state[start] != 0 {
			continue
		}
		walkID++
		if walkID == 0 {
			for i := range stamp {
				stamp[i] = 0
			}
			walkID = 1
		}
		cur := start
		for cur >= 0 && state[cur] == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if stamp[cur] == walkID {
				return errors.New("direct topology parent cycle")
			}
			stamp[cur] = walkID
			parent, err := readParent(cur)
			if err != nil {
				return err
			}
			cur = int(parent)
		}
		cur = start
		for cur >= 0 && state[cur] == 0 {
			state[cur] = 1
			parent, err := readParent(cur)
			if err != nil {
				return err
			}
			cur = int(parent)
		}
	}
	return nil
}

func directCopyFile(cw *countingWriter, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(cw, f)
	return err
}

func directMergeChildRuns(ctx context.Context, cw *countingWriter, runs []directRunFile, expected int) error {
	files := make([]*os.File, len(runs))
	readers := make([]*bufio.Reader, len(runs))
	h := &directChildHeap{}
	heap.Init(h)
	for i, run := range runs {
		f, err := os.Open(run.path)
		if err != nil {
			for _, opened := range files {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return err
		}
		files[i] = f
		readers[i] = bufio.NewReaderSize(f, 256*1024)
		pair, readErr := directReadChildPair(readers[i])
		if readErr == nil {
			heap.Push(h, directChildHead{Pair: pair, Run: i})
		} else if !errors.Is(readErr, io.EOF) {
			for _, opened := range files {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return readErr
		}
	}
	defer func() {
		for _, f := range files {
			if f != nil {
				_ = f.Close()
			}
		}
	}()
	var written int
	var buf [4]byte
	for h.Len() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		head := heap.Pop(h).(directChildHead)
		binary.LittleEndian.PutUint32(buf[:], head.Pair.Child)
		if _, err := cw.Write(buf[:]); err != nil {
			return err
		}
		written++
		next, readErr := directReadChildPair(readers[head.Run])
		if readErr == nil {
			heap.Push(h, directChildHead{Pair: next, Run: head.Run})
		} else if !errors.Is(readErr, io.EOF) {
			return readErr
		}
	}
	if written != expected {
		return fmt.Errorf("direct topology child count mismatch: wrote %d want %d", written, expected)
	}
	return nil
}

func directWriteTopologySections(ctx context.Context, cw *countingWriter, finalPath, frnPath, spoolDir string, recordCount, maxRecords int, owned *[]string, scratchHigh *int64) ([]indexSectionTableEntry, []directSectionReport, error) {
	scan, err := directTopologyScanSpool(ctx, finalPath, frnPath, spoolDir, recordCount, owned)
	if err != nil {
		return nil, nil, err
	}
	childCount, err := directTopologyWriteChildOffsets(scan.offsetsPath, scan.counts, recordCount)
	if err != nil {
		return nil, nil, err
	}
	if maxRecords <= 0 {
		maxRecords = directDefaultRunRecords
	}
	runs, runBytes, err := directTopologyBuildChildRuns(spoolDir, scan.parentPath, recordCount, maxRecords, owned)
	if err != nil {
		return nil, nil, err
	}
	topoScratch, rootCount, err := directTopologyScratch(scan, runBytes)
	if err != nil {
		return nil, nil, err
	}
	if topoScratch > *scratchHigh {
		*scratchHigh = topoScratch
	}
	chldEntry, chldReport, err := directTopologyEmitCHLD(ctx, cw, scan, runs, childCount, rootCount, recordCount, topoScratch)
	if err != nil {
		return nil, nil, err
	}
	frnsEntry, frnsReport, err := directTopologyEmitFRNS(cw, frnPath, recordCount)
	if err != nil {
		return nil, nil, err
	}
	entries := []indexSectionTableEntry{chldEntry, frnsEntry}
	reports := []directSectionReport{chldReport, frnsReport}
	seqEntry, seqReport, seqPresent, err := directWriteSEQNSectionFromSpool(ctx, cw, finalPath, recordCount)
	if err != nil {
		return nil, nil, err
	}
	if seqPresent {
		entries = append(entries, seqEntry)
		reports = append(reports, seqReport)
	}
	return entries, reports, nil
}

// directWriteSEQNSectionFromSpool writes the SEQN section from the canonical
// record spool. It reuses the shared encoder so direct builds persist the
// per-record NTFS sequence column in exactly the normal save format. Records
// whose source has no sequence (walk and synthetic) stay 0; the section is
// omitted when every sequence is unknown, matching the normal save path.
func directWriteSEQNSectionFromSpool(ctx context.Context, cw *countingWriter, finalPath string, recordCount int) (indexSectionTableEntry, directSectionReport, bool, error) {
	if recordCount <= 0 {
		return indexSectionTableEntry{}, directSectionReport{}, false, nil
	}
	f, err := os.Open(finalPath)
	if err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, false, err
	}
	seqs := make([]uint16, recordCount)
	r := bufio.NewReaderSize(f, 256*1024)
	any := false
	for id := 0; id < recordCount; id++ {
		select {
		case <-ctx.Done():
			_ = f.Close()
			return indexSectionTableEntry{}, directSectionReport{}, false, ctx.Err()
		default:
		}
		rec, readErr := readDirectSpoolRecord(r)
		if readErr != nil {
			_ = f.Close()
			return indexSectionTableEntry{}, directSectionReport{}, false, readErr
		}
		seqs[id] = rec.Seq
		if rec.Seq != 0 {
			any = true
		}
	}
	_ = f.Close()
	if !any {
		return indexSectionTableEntry{}, directSectionReport{}, false, nil
	}
	data := encodeSeqSection(seqs)
	if data == nil {
		return indexSectionTableEntry{}, directSectionReport{}, false, errors.New("direct SEQN encoder returned no data")
	}
	if err := writeAlignment(cw, 8); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, false, err
	}
	offset := uint64(cw.n)
	if _, err := cw.Write(data); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, false, err
	}
	entry := indexSectionTableEntry{tag: indexSectionSEQN, offset: offset, length: uint64(cw.n) - offset}
	report := directSectionReport{Name: "SEQN", Tag: indexSectionSEQN, Bytes: int64(entry.length), ScratchBytes: int64(recordCount) * 2}
	return entry, report, true, nil
}

// directTopologyScan holds the per-record parent/root/size sidecars produced by
// the topology scan pass.
type directTopologyScan struct {
	counts      []uint32
	parentPath  string
	offsetsPath string
	rootsPath   string
	sizesPath   string
}

// directTopologyScanSpool streams the final spool once, writing per-record
// directory sizes, parent ids, and root records to sidecar files, then verifies
// the parent links are acyclic.
func directTopologyScanSpool(ctx context.Context, finalPath, frnPath, spoolDir string, recordCount int, owned *[]string) (*directTopologyScan, error) {
	parentPath := filepath.Join(spoolDir, "direct-parents.tmp")
	offsetsPath := filepath.Join(spoolDir, "direct-child-offsets.tmp")
	rootsPath := filepath.Join(spoolDir, "direct-roots.tmp")
	sizesPath := filepath.Join(spoolDir, "direct-sizes.tmp")
	*owned = append(*owned, parentPath, offsetsPath, rootsPath, sizesPath)

	parents, err := os.Create(parentPath)
	if err != nil {
		return nil, err
	}
	roots, err := os.Create(rootsPath)
	if err != nil {
		closeAll(parents)
		return nil, err
	}
	sizes, err := os.Create(sizesPath)
	if err != nil {
		closeAll(parents, roots)
		return nil, err
	}
	parentWriter := bufio.NewWriterSize(parents, 256*1024)
	rootWriter := bufio.NewWriterSize(roots, 256*1024)
	sizeWriter := bufio.NewWriterSize(sizes, 256*1024)

	final, err := os.Open(finalPath)
	if err != nil {
		closeAll(parents, roots, sizes)
		return nil, err
	}
	frnMap, frns, err := directMapFRNs(frnPath, recordCount)
	if err != nil {
		closeAll(final, parents, roots, sizes)
		return nil, err
	}
	if frnMap != nil {
		defer frnMap.close()
	}

	counts := make([]uint32, recordCount)
	r := bufio.NewReaderSize(final, 256*1024)
	for id := 0; id < recordCount; id++ {
		select {
		case <-ctx.Done():
			closeAll(final, parents, roots, sizes)
			return nil, ctx.Err()
		default:
		}
		rec, readErr := readDirectSpoolRecord(r)
		if readErr != nil {
			closeAll(final, parents, roots, sizes)
			return nil, readErr
		}
		// Directory aggregate sizes sum descendant file sizes; a directory's
		// own entry contributes nothing so the subtree post-order sum is exact.
		var size uint64
		if rec.Mode&uint32(os.ModeDir) == 0 && rec.Size > 0 {
			size = uint64(rec.Size)
		}
		var sz [8]byte
		binary.LittleEndian.PutUint64(sz[:], size)
		if _, err := sizeWriter.Write(sz[:]); err != nil {
			closeAll(final, parents, roots, sizes)
			return nil, err
		}
		parentID := int32(-1)
		if rec.ParentFRN != 0 {
			parentID = directLookupIDMapped(frns, rec.ParentFRN)
		}
		if parentID == int32(id) {
			closeAll(final, parents, roots, sizes)
			return nil, errors.New("direct topology self-parent")
		}
		var b [4]byte
		if parentID < 0 {
			binary.LittleEndian.PutUint32(b[:], ^uint32(0))
			var rootID [4]byte
			binary.LittleEndian.PutUint32(rootID[:], uint32(id))
			if _, err := rootWriter.Write(rootID[:]); err != nil {
				closeAll(final, parents, roots, sizes)
				return nil, err
			}
		} else {
			binary.LittleEndian.PutUint32(b[:], uint32(parentID))
			counts[parentID]++
		}
		if _, err := parentWriter.Write(b[:]); err != nil {
			closeAll(final, parents, roots, sizes)
			return nil, err
		}
	}
	_ = final.Close()
	if err := parentWriter.Flush(); err != nil {
		closeAll(parents, roots, sizes)
		return nil, err
	}
	if err := rootWriter.Flush(); err != nil {
		closeAll(parents, roots, sizes)
		return nil, err
	}
	if err := sizeWriter.Flush(); err != nil {
		closeAll(parents, roots, sizes)
		return nil, err
	}
	closeAll(parents, roots, sizes)
	if err := directCheckParentCycles(ctx, parentPath, recordCount); err != nil {
		return nil, err
	}
	return &directTopologyScan{counts: counts, parentPath: parentPath, offsetsPath: offsetsPath, rootsPath: rootsPath, sizesPath: sizesPath}, nil
}

// directTopologyWriteChildOffsets writes the prefix-sum child offset table and
// returns the total child count.
func directTopologyWriteChildOffsets(offsetsPath string, counts []uint32, recordCount int) (uint64, error) {
	offsets, err := os.Create(offsetsPath)
	if err != nil {
		return 0, err
	}
	offsetWriter := bufio.NewWriterSize(offsets, 256*1024)
	var childCount uint64
	for _, count := range counts {
		childCount += uint64(count)
	}
	var running uint32
	var off [4]byte
	for i := 0; i <= recordCount; i++ {
		if i > 0 {
			running += counts[i-1]
		}
		binary.LittleEndian.PutUint32(off[:], running)
		if _, err := offsetWriter.Write(off[:]); err != nil {
			_ = offsets.Close()
			return 0, err
		}
	}
	if err := offsetWriter.Flush(); err != nil {
		_ = offsets.Close()
		return 0, err
	}
	_ = offsets.Close()
	if childCount > uint64(^uint32(0)) {
		return 0, errors.New("direct topology child count exceeds format")
	}
	return childCount, nil
}

// directTopologyBuildChildRuns partitions the parent sidecar into sorted,
// bounded child runs for the external merge.
func directTopologyBuildChildRuns(spoolDir, parentPath string, recordCount, maxRecords int, owned *[]string) ([]directRunFile, int64, error) {
	pfile, err := os.Open(parentPath)
	if err != nil {
		return nil, 0, err
	}
	reader := bufio.NewReaderSize(pfile, 256*1024)
	pairs := make([]directChildPair, 0, min(maxRecords, 4096))
	runs := make([]directRunFile, 0)
	flush := func() error {
		if len(pairs) == 0 {
			return nil
		}
		path := filepath.Join(spoolDir, fmt.Sprintf("direct-child-%06d.tmp", len(runs)))
		bytes, err := directWriteChildRun(path, pairs)
		if err != nil {
			return err
		}
		*owned = append(*owned, path)
		runs = append(runs, directRunFile{path: path, bytes: bytes})
		pairs = make([]directChildPair, 0, min(maxRecords, 4096))
		return nil
	}
	for id := 0; id < recordCount; id++ {
		var b [4]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			_ = pfile.Close()
			return nil, 0, err
		}
		parent := binary.LittleEndian.Uint32(b[:])
		if parent != ^uint32(0) {
			pairs = append(pairs, directChildPair{Parent: parent, Child: uint32(id)})
		}
		if len(pairs) >= maxRecords {
			if err := flush(); err != nil {
				_ = pfile.Close()
				return nil, 0, err
			}
		}
	}
	if err := flush(); err != nil {
		_ = pfile.Close()
		return nil, 0, err
	}
	_ = pfile.Close()
	var runBytes int64
	for _, run := range runs {
		runBytes += run.bytes
	}
	return runs, runBytes, nil
}

// directTopologyScratch returns the peak scratch footprint and root count for
// the CHLD section.
func directTopologyScratch(scan *directTopologyScan, runBytes int64) (int64, int, error) {
	scratch := runBytes
	if info, err := os.Stat(scan.parentPath); err == nil {
		scratch += info.Size()
	}
	if info, err := os.Stat(scan.offsetsPath); err == nil {
		scratch += info.Size()
	}
	info, err := os.Stat(scan.rootsPath)
	if err != nil {
		return 0, 0, fmt.Errorf("stat topology roots: %w", err)
	}
	scratch += info.Size()
	return scratch, int(info.Size() / 4), nil
}

// directTopologyEmitCHLD writes the CHLD section and removes its runs.
func directTopologyEmitCHLD(ctx context.Context, cw *countingWriter, scan *directTopologyScan, runs []directRunFile, childCount uint64, rootCount, recordCount int, topoScratch int64) (indexSectionTableEntry, directSectionReport, error) {
	if err := writeAlignment(cw, 8); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	offset := uint64(cw.n)
	if err := binary.Write(cw, binary.LittleEndian, uint32(recordCount+1)); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := directCopyFile(cw, scan.offsetsPath); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := binary.Write(cw, binary.LittleEndian, uint32(childCount)); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := directMergeChildRuns(ctx, cw, runs, int(childCount)); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := binary.Write(cw, binary.LittleEndian, uint32(rootCount)); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := directCopyFile(cw, scan.rootsPath); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	for _, run := range runs {
		_ = os.Remove(run.path)
	}
	entry := indexSectionTableEntry{tag: indexSectionCHLD, offset: offset, length: uint64(cw.n) - offset}
	report := directSectionReport{Name: "CHLD", Tag: indexSectionCHLD, Runs: len(runs), Bytes: int64(cw.n) - int64(offset), ScratchBytes: topoScratch}
	return entry, report, nil
}

// directTopologyEmitFRNS writes the FRNS section: FRN sidecar plus the identity
// id vector.
func directTopologyEmitFRNS(cw *countingWriter, frnPath string, recordCount int) (indexSectionTableEntry, directSectionReport, error) {
	if err := writeAlignment(cw, 8); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	offset := uint64(cw.n)
	if err := binary.Write(cw, binary.LittleEndian, uint32(recordCount)); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := directCopyFile(cw, frnPath); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	if err := binary.Write(cw, binary.LittleEndian, uint32(recordCount)); err != nil {
		return indexSectionTableEntry{}, directSectionReport{}, err
	}
	var ids [256]byte
	for start := 0; start < recordCount; {
		count := min(recordCount-start, len(ids)/4)
		for i := 0; i < count; i++ {
			binary.LittleEndian.PutUint32(ids[i*4:], uint32(start+i))
		}
		if _, err := cw.Write(ids[:count*4]); err != nil {
			return indexSectionTableEntry{}, directSectionReport{}, err
		}
		start += count
	}
	entry := indexSectionTableEntry{tag: indexSectionFRNS, offset: offset, length: uint64(cw.n) - offset}
	report := directSectionReport{Name: "FRNS", Tag: indexSectionFRNS, Runs: 0, Bytes: int64(cw.n) - int64(offset), ScratchBytes: 0}
	return entry, report, nil
}

func closeAll(closers ...io.Closer) {
	for _, c := range closers {
		if c != nil {
			_ = c.Close()
		}
	}
}
