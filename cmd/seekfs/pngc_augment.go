package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	pngcAugmentChunkBytes  = 8 * 1024 * 1024
	pngcAugmentMaxSections = 1024
)

// PNGCAugmentOptions bounds the copy-on-write proof operation.  The target is
// always a new file; the source is never opened for writing.
type PNGCAugmentOptions struct {
	MaxOutputGrowth  int64
	MaxHeapBytes     uint64
	MaxScratchBytes  int64
	MinFreeDiskBytes uint64
	// FailAfterBytes is test-only failure injection for atomic-cleanup tests.
	FailAfterBytes int64
}

type PNGCAugmentResult struct {
	SourceBytes  int64
	TargetBytes  int64
	PNGCBytes    int64
	ScratchBytes int64
	OutputGrowth int64
	SourceSHA256 string
	TargetSHA256 string
	Wall         time.Duration
}

type rawSectionTable struct {
	Offset  uint64
	Entries []indexSectionTableEntry
}

func augmentPNGC(ctx context.Context, sourcePath, targetPath string, opts PNGCAugmentOptions) (result PNGCAugmentResult, err error) {
	start := time.Now()
	defer func() { result.Wall = time.Since(start) }()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return result, err
	}
	targetPath, err = filepath.Abs(targetPath)
	if err != nil {
		return result, err
	}
	if sameAbsolutePath(sourcePath, targetPath) {
		return result, errors.New("PNG C augmentation requires a distinct target")
	}
	if _, err := os.Stat(targetPath); err == nil {
		return result, errors.New("PNG C augmentation refuses to replace an existing target")
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return result, err
	}
	if info.Size() <= 0 {
		return result, errors.New("source index is empty")
	}
	result.SourceBytes = info.Size()
	if opts.MaxOutputGrowth > 0 && uint64(info.Size()) > ^uint64(0)-uint64(opts.MaxOutputGrowth) {
		return result, errors.New("output growth limit overflows")
	}
	if opts.MinFreeDiskBytes > 0 {
		free, err := freeDiskBytes(filepath.Dir(targetPath))
		if err != nil {
			return result, err
		}
		growthBudget := opts.MaxOutputGrowth
		if growthBudget <= 0 {
			growthBudget = 1
		}
		required := uint64(info.Size()) + uint64(growthBudget) + opts.MinFreeDiskBytes
		if free < required {
			return result, fmt.Errorf("insufficient free disk: have %d need %d", free, required)
		}
	}

	result.SourceSHA256, err = fileSHA256(sourcePath)
	if err != nil {
		return result, err
	}
	table, err := readRawSectionTable(sourcePath, info.Size())
	if err != nil {
		return result, err
	}
	for _, entry := range table.Entries {
		if entry.tag == indexSectionPNGC {
			return result, errors.New("source already contains PNGC")
		}
	}

	idx, err := loadIndexMMap(sourcePath)
	if err != nil {
		return result, fmt.Errorf("load v9 source: %w", err)
	}
	defer closeMappedIndex(idx)
	if idx.Version != indexVersion || idx.Derived.NameTrigrams == nil || len(idx.Derived.NameRank) == 0 {
		return result, errors.New("source lacks v9 selective PNGR/name-rank metadata")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	maxScratch := opts.MaxScratchBytes
	if maxScratch <= 0 {
		maxScratch = 1 << 30
	}
	stream, err := newPNGCStreamBuilder(ctx, idx, idx.Derived.NameTrigrams, filepath.Dir(targetPath), maxScratch)
	if err != nil {
		return result, err
	}
	defer stream.cleanup()
	streamEntries, streamBlocks, pngcBytes, err := stream.measure(idx.Derived.NameRank)
	if err != nil {
		return result, err
	}
	result.PNGCBytes = int64(pngcBytes)
	result.ScratchBytes = stream.scratch
	growthEstimate := result.PNGCBytes + 7 + 4 + int64((len(table.Entries)+1)*24)
	if opts.MaxOutputGrowth > 0 && growthEstimate > opts.MaxOutputGrowth {
		return result, fmt.Errorf("PNGC output growth %d exceeds limit %d", growthEstimate, opts.MaxOutputGrowth)
	}
	if opts.MaxHeapBytes > 0 {
		runtime.GC()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		if mem.HeapInuse > opts.MaxHeapBytes {
			return result, fmt.Errorf("PNG C build heap %d exceeds limit %d", mem.HeapInuse, opts.MaxHeapBytes)
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return result, err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(targetPath)+".*.tmp")
	if err != nil {
		return result, err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	source, err := os.Open(sourcePath)
	if err != nil {
		return result, err
	}
	defer source.Close()
	if err := copyPNGCAugmentSource(ctx, tmp, source, info.Size(), opts.FailAfterBytes); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := writeAlignmentFile(tmp, 8); err != nil {
		return result, err
	}
	pngcOffset, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		return result, err
	}
	writtenPNGC, err := stream.writePayload(ctx, tmp, idx.Derived.NameRank, streamEntries, streamBlocks)
	if err != nil {
		return result, err
	}
	if writtenPNGC != result.PNGCBytes {
		return result, fmt.Errorf("PNGC stream length mismatch: wrote %d want %d", writtenPNGC, result.PNGCBytes)
	}
	if opts.FailAfterBytes >= 0 && opts.FailAfterBytes > 0 && pngcOffset+writtenPNGC >= opts.FailAfterBytes {
		return result, errors.New("PNG C failure injection")
	}
	newTableOffset, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		return result, err
	}
	sectionEntries := append(append([]indexSectionTableEntry(nil), table.Entries...), indexSectionTableEntry{
		tag: indexSectionPNGC, offset: uint64(pngcOffset), length: uint64(writtenPNGC),
	})
	if len(sectionEntries) > pngcAugmentMaxSections {
		return result, errors.New("too many v9 sections")
	}
	var count [4]byte
	binary.LittleEndian.PutUint32(count[:], uint32(len(sectionEntries)))
	if _, err := tmp.Write(count[:]); err != nil {
		return result, err
	}
	for _, entry := range sectionEntries {
		var raw [24]byte
		binary.LittleEndian.PutUint32(raw[0:], entry.tag)
		binary.LittleEndian.PutUint64(raw[4:], entry.offset)
		binary.LittleEndian.PutUint64(raw[12:], entry.length)
		binary.LittleEndian.PutUint32(raw[20:], entry.flags)
		if _, err := tmp.Write(raw[:]); err != nil {
			return result, err
		}
	}
	if _, err := tmp.Seek(int64(binary.Size(diskHeader{})), io.SeekStart); err != nil {
		return result, err
	}
	var tablePtr [8]byte
	binary.LittleEndian.PutUint64(tablePtr[:], uint64(newTableOffset))
	if _, err := tmp.Write(tablePtr[:]); err != nil {
		return result, err
	}
	if err := tmp.Sync(); err != nil {
		return result, err
	}
	if err := tmp.Close(); err != nil {
		return result, err
	}
	if got, err := fileSHA256(sourcePath); err != nil {
		return result, err
	} else if got != result.SourceSHA256 {
		return result, errors.New("source changed during PNG C augmentation")
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return result, err
	}
	committed = true
	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		return result, err
	}
	result.TargetBytes = targetInfo.Size()
	result.OutputGrowth = result.TargetBytes - result.SourceBytes
	result.TargetSHA256, err = fileSHA256(targetPath)
	if err != nil {
		return result, err
	}
	// Reopen the committed target after releasing the source mapping.  This
	// keeps the proof atomic while ensuring the new table and PNGC payload are
	// independently decodable before the caller can use the target.
	closeMappedIndex(idx)
	idx = nil
	validated, err := loadIndexMMap(targetPath)
	if err != nil {
		_ = os.Remove(targetPath)
		committed = false
		return result, fmt.Errorf("validate augmented target: %w", err)
	}
	validPNGC := validated.Derived.SelfNameTrigrams != nil && validated.Derived.SelfNameTrigrams.gramCountsComplete
	closeMappedIndex(validated)
	if !validPNGC {
		_ = os.Remove(targetPath)
		committed = false
		return result, errors.New("validate augmented target: PNGC is missing or incomplete")
	}
	return result, nil
}

// buildAugmentSelfNameGramIndex fills the grams absent from the retained PNGR
// source.  New v9 files with complete omission metadata can use that compact
// list directly; older v9 files without omission metadata are made complete
// by deriving only the missing grams from a temporary full name index.
func buildAugmentSelfNameGramIndex(idx *Index, selective *compressedTrigramIndex) *compressedTrigramIndex {
	if idx == nil || selective == nil {
		return nil
	}
	if len(selective.omitted) > 0 {
		return optionalSelfNameGramIndex(idx, selective)
	}
	full := buildNameTrigramIndex(idx)
	if full == nil {
		return nil
	}
	out := &compressedTrigramIndex{
		counts:             make(map[uint32]int),
		gramCountsComplete: true,
		gramSize:           3,
		recordCount:        idx.compactRecordCount(),
		segments:           []trigramSegment{{start: 0, end: idx.compactRecordCount(), postings: make(map[uint32]compressedPosting)}},
	}
	full.forEachCount(func(gram uint32, count int) {
		if count <= 0 || selective.hasStoredPosting(gram) {
			return
		}
		ids := trigramPostingIDs(full, gram)
		if len(ids) == 0 {
			return
		}
		encoded := encodeDeltaUvarint32(ids)
		out.segments[0].postings[gram] = compressedPosting{count: len(ids), data: encoded}
		out.counts[gram] = len(ids)
		out.postingBytes += len(encoded)
		out.segments[0].postingBytes += len(encoded)
	})
	if len(out.counts) == 0 {
		return nil
	}
	return out
}

func copyPNGCAugmentSource(ctx context.Context, dst io.Writer, src io.Reader, size, failAfter int64) error {
	buf := make([]byte, pngcAugmentChunkBytes)
	var written int64
	for written < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := int64(len(buf))
		if remain := size - written; remain < want {
			want = remain
		}
		n, err := io.CopyN(dst, src, want)
		written += n
		if failAfter > 0 && written >= failAfter {
			return errors.New("PNG C failure injection")
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
	return nil
}

func readRawSectionTable(path string, size int64) (rawSectionTable, error) {
	f, err := os.Open(path)
	if err != nil {
		return rawSectionTable{}, err
	}
	defer f.Close()
	headerSize := int64(binary.Size(diskHeader{}))
	var header diskHeader
	if err := binary.Read(f, binary.LittleEndian, &header); err != nil {
		return rawSectionTable{}, err
	}
	if header.Magic != indexMagic || header.Version != indexVersion {
		return rawSectionTable{}, errors.New("source is not a v9 index")
	}
	var ptr [8]byte
	if _, err := io.ReadFull(f, ptr[:]); err != nil {
		return rawSectionTable{}, err
	}
	tableOffset := binary.LittleEndian.Uint64(ptr[:])
	if tableOffset < uint64(headerSize+8) || tableOffset+4 > uint64(size) {
		return rawSectionTable{}, errors.New("invalid v9 section table offset")
	}
	var countBuf [4]byte
	if _, err := f.ReadAt(countBuf[:], int64(tableOffset)); err != nil {
		return rawSectionTable{}, err
	}
	count := int(binary.LittleEndian.Uint32(countBuf[:]))
	if count <= 0 || count > pngcAugmentMaxSections {
		return rawSectionTable{}, errors.New("invalid v9 section count")
	}
	tableBytes := int64(count) * 24
	if tableOffset+4+uint64(tableBytes) < tableOffset || tableOffset+4+uint64(tableBytes) > uint64(size) {
		return rawSectionTable{}, errors.New("truncated v9 section table")
	}
	raw := make([]byte, tableBytes)
	if _, err := f.ReadAt(raw, int64(tableOffset+4)); err != nil {
		return rawSectionTable{}, err
	}
	entries := make([]indexSectionTableEntry, 0, count)
	seen := make(map[uint32]struct{}, count)
	for off := 0; off < len(raw); off += 24 {
		entry := indexSectionTableEntry{
			tag:    binary.LittleEndian.Uint32(raw[off:]),
			offset: binary.LittleEndian.Uint64(raw[off+4:]),
			length: binary.LittleEndian.Uint64(raw[off+12:]),
			flags:  binary.LittleEndian.Uint32(raw[off+20:]),
		}
		if _, ok := seen[entry.tag]; ok {
			return rawSectionTable{}, errors.New("duplicate v9 section tag")
		}
		seen[entry.tag] = struct{}{}
		if entry.offset > uint64(size) || entry.length > uint64(size) || entry.offset+entry.length < entry.offset || entry.offset+entry.length > uint64(size) {
			return rawSectionTable{}, errors.New("truncated v9 section payload")
		}
		entries = append(entries, entry)
	}
	return rawSectionTable{Offset: tableOffset, Entries: entries}, nil
}

func writeAlignmentFile(f *os.File, alignment int64) error {
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	pad := (alignment - (pos % alignment)) % alignment
	if pad == 0 {
		return nil
	}
	_, err = f.Write(make([]byte, pad))
	return err
}

func sameAbsolutePath(a, b string) bool {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, pngcAugmentChunkBytes)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func freeDiskBytes(path string) (uint64, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	for {
		if _, err := os.Stat(path); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, nil, nil); err != nil {
		return 0, err
	}
	return free, nil
}

// memoryStatusEx mirrors Win32 MEMORYSTATUSEX: two DWORDs followed by seven
// DWORDLONGs, 64 bytes total (package scope so tests can pin the size: a
// wrong size makes GlobalMemoryStatusEx fail and every consumer silently
// fall back).
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// totalRamBytes reports total physical memory via kernel32's
// GlobalMemoryStatusEx (x/sys has no wrapper for it in this version). Only
// TotalPhys is read; an error means unknown.
func totalRamBytes() (uint64, error) {
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
	var st memoryStatusEx
	// MEMORYSTATUSEX is 64 bytes (two DWORDs + seven DWORDLONGs); the
	// length field must say so or the call fails. There is no padding
	// between dwMemoryLoad and ullTotalPhys: 4+4 already aligns the
	// 8-byte fields.
	st.length = uint32(unsafe.Sizeof(st))
	r1, _, _ := proc.Call(uintptr(unsafe.Pointer(&st)))
	if r1 == 0 {
		return 0, errors.New("GlobalMemoryStatusEx failed")
	}
	if st.totalPhys == 0 {
		return 0, errors.New("no physical memory reported")
	}
	return st.totalPhys, nil
}

// totalDiskBytes reports the total capacity bytes of the volume holding path,
// resolving through non-existent parents to the enclosing volume like
// freeDiskBytes.
func totalDiskBytes(path string) (uint64, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	for {
		if _, err := os.Stat(path); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var total uint64
	if err := windows.GetDiskFreeSpaceEx(p, nil, &total, nil); err != nil {
		return 0, err
	}
	return total, nil
}

func closeMappedIndex(idx *Index) {
	if idx != nil && idx.MMapRecords != nil && idx.MMapRecords.file != nil {
		_ = idx.MMapRecords.file.close()
	}
}
