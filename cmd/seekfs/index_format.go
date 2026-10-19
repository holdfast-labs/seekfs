package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strings"
	"time"
)

func readIndexWithReaderAt(r io.Reader, ra io.ReaderAt, size int64) (*Index, error) {
	var header diskHeader
	if err := binary.Read(r, binary.LittleEndian, &header); err != nil {
		return nil, err
	}
	sectionTableOffset := uint64(0)
	if header.Magic != indexMagic {
		return nil, errors.New("unsupported index format: only the v9/v10 index formats are supported")
	}
	if err := binary.Read(r, binary.LittleEndian, &sectionTableOffset); err != nil {
		return nil, err
	}
	if !indexVersionSupported(int(header.Version)) {
		return nil, fmt.Errorf("unsupported index version %d: only v9/v10 indexes are supported", header.Version)
	}
	if header.EntryCount > uint64(^uint(0)>>1) || header.RootCount > uint64(^uint(0)>>1) {
		return nil, errors.New("index too large")
	}
	idx := &Index{
		Version:    int(header.Version),
		BuiltAt:    time.Unix(0, header.BuiltUnix),
		Roots:      make([]string, int(header.RootCount)),
		JournalID:  header.JournalID,
		Checkpoint: header.Checkpoint,
		Compact:    header.Compact != 0,
	}
	var err error
	if idx.Source, err = readString(r); err != nil {
		return nil, err
	}
	if idx.Volume, err = readString(r); err != nil {
		return nil, err
	}
	if idx.ContentHash, err = readString(r); err != nil {
		return nil, err
	}
	for i := range idx.Roots {
		if idx.Roots[i], err = readString(r); err != nil {
			return nil, err
		}
	}
	if idx.Compact {
		wideRefs := header.Compact&compactDiskWideRefsFlag != 0
		idx.CompactAttrs = header.Compact&compactDiskAttrsFlag != 0
		if header.NameBlobLen > uint64(^uint(0)>>1) {
			return nil, errors.New("name blob too large")
		}
		idx.NameBlob = make([]byte, int(header.NameBlobLen))
		if _, err := io.ReadFull(r, idx.NameBlob); err != nil {
			return nil, err
		}
		if header.TokenCount > uint64(^uint(0)>>1) {
			return nil, errors.New("name table too large")
		}
		nameOffs := make([]uint32, int(header.TokenCount))
		nameLens := make([]uint16, int(header.TokenCount))
		for i := range nameOffs {
			if err := binary.Read(r, binary.LittleEndian, &nameOffs[i]); err != nil {
				return nil, err
			}
			if err := binary.Read(r, binary.LittleEndian, &nameLens[i]); err != nil {
				return nil, err
			}
		}
		idx.Records = make([]CompactRecord, int(header.EntryCount))
		for i := range idx.Records {
			rec := &idx.Records[i]
			if err := binary.Read(r, binary.LittleEndian, &rec.FRN); err != nil {
				return nil, err
			}
			if err := binary.Read(r, binary.LittleEndian, &rec.ParentFRN); err != nil {
				return nil, err
			}
			parent, nameOff, err := readCompactRecordRefs(r, wideRefs)
			if err != nil {
				return nil, err
			}
			if (!wideRefs && parent == compactNarrowParentSentinel) || (wideRefs && parent == compactWideParentSentinel) {
				rec.Parent = -1
			} else {
				rec.Parent = int32(parent)
			}
			rec.NameOff = nameOff
			if int(rec.NameOff) >= len(nameOffs) {
				return nil, errors.New("invalid compact name id")
			}
			off := nameOffs[rec.NameOff]
			length := nameLens[rec.NameOff]
			end := int(off) + int(length)
			if end < int(off) || end > len(idx.NameBlob) {
				return nil, errors.New("invalid compact name reference")
			}
			rec.Name = stringView(idx.NameBlob[int(off):end])
			if err := binary.Read(r, binary.LittleEndian, &rec.Mode); err != nil {
				return nil, err
			}
			if err := binary.Read(r, binary.LittleEndian, &rec.Size); err != nil {
				return nil, err
			}
			if err := binary.Read(r, binary.LittleEndian, &rec.ModUnix); err != nil {
				return nil, err
			}
			var deleted uint8
			if err := binary.Read(r, binary.LittleEndian, &deleted); err != nil {
				return nil, err
			}
			rec.Deleted = deleted != 0
		}
		idx.CompactNameOrder = make([]int, len(idx.Records))
		for i := range idx.CompactNameOrder {
			idx.CompactNameOrder[i] = i
		}
		if sectionTableOffset != 0 {
			idx.Derived = readDerivedSectionsFromReaderAt(ra, size, sectionTableOffset, int(header.EntryCount))
		}
		// A v10 index persists the NTFS sequence per record in the SEQN
		// section; a v9 index has none and every record's Seq stays 0.
		if seqs := idx.Derived.FRNSeq; len(seqs) == len(idx.Records) {
			for i := range seqs {
				idx.Records[i].Seq = seqs[i]
			}
		}
		return idx, nil
	}
	idx.Entries = make([]Entry, int(header.EntryCount))
	for i := range idx.Entries {
		entry := &idx.Entries[i]
		if entry.Path, err = readString(r); err != nil {
			return nil, err
		}
		if entry.Name, err = readString(r); err != nil {
			return nil, err
		}
		if entry.LowerPath, err = readString(r); err != nil {
			return nil, err
		}
		if entry.LowerName, err = readString(r); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.LittleEndian, &entry.Size); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.LittleEndian, &entry.Mode); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.LittleEndian, &entry.ModUnix); err != nil {
			return nil, err
		}
	}
	if idx.NameOrder, err = readUint32Slice(r, int(header.EntryCount)); err != nil {
		return nil, err
	}
	if idx.PathOrder, err = readUint32Slice(r, int(header.EntryCount)); err != nil {
		return nil, err
	}
	return idx, nil
}

func writeString(w io.Writer, s string) error {
	if len(s) > int(^uint32(0)) {
		return errors.New("string too large")
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}

func readString(r io.Reader) (string, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	buf := make([]byte, int(n))
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func writeUint32Slice(w io.Writer, values []int) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(values))); err != nil {
		return err
	}
	tmp := make([]uint32, len(values))
	for i, value := range values {
		tmp[i] = uint32(value)
	}
	return binary.Write(w, binary.LittleEndian, tmp)
}

func readUint32Slice(r io.Reader, n int) ([]int, error) {
	validateAsPermutation := n >= 0
	if n < 0 {
		var count uint32
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return nil, err
		}
		n = int(count)
	} else {
		var count uint32
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return nil, err
		}
		if int(count) != n {
			return nil, errors.New("invalid order length")
		}
	}
	tmp := make([]uint32, n)
	if err := binary.Read(r, binary.LittleEndian, tmp); err != nil {
		return nil, err
	}
	values := make([]int, n)
	for i, value := range tmp {
		if validateAsPermutation && (int(value) < 0 || int(value) >= n) {
			return nil, errors.New("invalid order index")
		}
		values[i] = int(value)
	}
	return values, nil
}

func writeCompactRecordRefs(w io.Writer, parent, nameOff uint32, wide bool) error {
	if wide {
		if err := binary.Write(w, binary.LittleEndian, parent); err != nil {
			return err
		}
		return binary.Write(w, binary.LittleEndian, nameOff)
	}
	if parent > compactNarrowParentSentinel || nameOff >= compactNarrowParentSentinel {
		return errors.New("compact index too large for packed record format")
	}
	if err := writeUint24(w, parent); err != nil {
		return err
	}
	return writeUint24(w, nameOff)
}

func readCompactRecordRefs(r io.Reader, wide bool) (uint32, uint32, error) {
	if wide {
		var parent, nameOff uint32
		if err := binary.Read(r, binary.LittleEndian, &parent); err != nil {
			return 0, 0, err
		}
		if err := binary.Read(r, binary.LittleEndian, &nameOff); err != nil {
			return 0, 0, err
		}
		return parent, nameOff, nil
	}
	parent, err := readUint24(r)
	if err != nil {
		return 0, 0, err
	}
	nameOff, err := readUint24(r)
	if err != nil {
		return 0, 0, err
	}
	return parent, nameOff, nil
}

func writeUint24(w io.Writer, value uint32) error {
	var buf [3]byte
	buf[0] = byte(value)
	buf[1] = byte(value >> 8)
	buf[2] = byte(value >> 16)
	_, err := w.Write(buf[:])
	return err
}

func readUint24(r io.Reader) (uint32, error) {
	var buf [3]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16, nil
}

func blobString(blob []byte, off uint64, length uint32) (string, error) {
	end := off + uint64(length)
	if end < off || end > uint64(len(blob)) {
		return "", errors.New("invalid string reference")
	}
	return string(blob[off:end]), nil
}

func saveIndex(path string, idx *Index) error {
	tmp, err := stageIndexFile(path, idx)
	if err != nil {
		return err
	}
	return commitStageIndexFile(path, tmp)
}

// sweepStaleIndexTempFilesIfDue reaps abandoned index stage temporaries at most
// once per staleTempSweepInterval.  The startup sweep alone never runs again on
// a long-lived service, so each interrupted persist (killed mid-write, or a
// failing disk) leaves a multi-GB temp file behind that can fill the drive and
// then block every later persist, which is how a full disk turns into a
// permanent persist-failure loop.
func (s *goSearchService) sweepStaleIndexTempFilesIfDue(now time.Time) {
	if s == nil || len(s.dbs) == 0 {
		return
	}
	if now.Sub(time.Unix(0, s.lastTempSweep.Load())) < staleTempSweepInterval {
		return
	}
	s.lastTempSweep.Store(now.UnixNano())
	s.sweepStaleIndexTempFiles()
}

// sweepStaleIndexTempFiles removes leftover stageIndexFile temporaries for the
// configured databases.  These accumulate when a persist is interrupted (the
// process exits mid-write), and each one can be many GB; they are never
// reaped otherwise.  Only files older than an hour and not currently growing
// are removed, so a live persist is never disturbed.
func (s *goSearchService) sweepStaleIndexTempFiles() {
	for _, db := range s.dbs {
		dir := filepath.Dir(db)
		base := filepath.Base(db)
		matches, err := filepath.Glob(filepath.Join(dir, base+".*.tmp"))
		if err != nil {
			continue
		}
		cutoff := time.Now().Add(-time.Hour)
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				if rmErr := os.Remove(match); rmErr == nil {
					serviceLog("swept stale index temp %s", match)
				}
			}
		}
	}
}

// stageIndexFile writes idx to a fresh temporary file next to path and returns
// the temporary file name.  The caller may do expensive work between staging
// and committing; commitStageIndexFile performs the atomic rename.  This lets
// the service write a multi-GB v9 index while holding only the per-volume
// lock, so the global service lock is not blocked for the whole disk write.
func stageIndexFile(path string, idx *Index) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create index directory %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("stage index %s: %w", path, err)
	}
	tmp := f.Name()
	if !idx.Compact {
		ensureCompactIndexForService(idx)
		buildOrders(idx)
	}
	err = writeIndexFile(f, idx)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("write index %s: %w", path, err)
	}
	if syncErr != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("sync index %s: %w", path, syncErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("close staged index %s: %w", path, closeErr)
	}
	return tmp, nil
}

// commitStageIndexFile atomically replaces path with the staged temporary file
// produced by stageIndexFile.
func commitStageIndexFile(path, tmp string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		// In low-memory mode the live index is memory-mapped, so the OS
		// refuses to delete the target file.  Fall back to an in-place
		// truncate-and-rewrite: the mmap view stays valid (same file),
		// readers see a coherent file only after the rename of the tmp file
		// would have taken place, and the file is fully fsynced before the
		// old mapping is invalidated by future loads.
		if !serviceLowMemoryMode() {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace index %s: %w", path, err)
		}
		if inPlaceErr := replaceIndexFileInPlace(path, tmp); inPlaceErr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("remove %s: %w; in-place fallback: %w", path, err, inPlaceErr)
		}
		_ = os.Remove(tmp)
		return nil
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace index %s: %w", path, err)
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// replaceIndexFileInPlace rewrites an existing (possibly memory-mapped) index
// file with the freshly written temporary file's contents.  It truncates the
// target and copies the tmp payload so the mapped view remains backed by the
// same file.  On Windows this is required in low-memory mode because the mmap
// pins the file open and remove/rename fails with ERROR_SHARING_VIOLATION.
func replaceIndexFileInPlace(path, tmp string) error {
	in, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}

type indexSectionBlob struct {
	tag     uint32
	data    []byte
	subtree *subtreeSectionBlob
	flags   uint32
}

const subtreeSectionScratchBytes = 64 * 1024

type subtreeSectionBlob struct {
	parts [][]uint32
}

func writeSubtreeSection(w io.Writer, section *subtreeSectionBlob) (int64, error) {
	if section == nil {
		return 0, nil
	}
	var scratch [subtreeSectionScratchBytes]byte
	var written int64
	for _, part := range section.parts {
		var count [4]byte
		binary.LittleEndian.PutUint32(count[:], uint32(len(part)))
		n, err := w.Write(count[:])
		written += int64(n)
		if err != nil {
			return written, err
		}
		if n != len(count) {
			return written, io.ErrShortWrite
		}
		for start := 0; start < len(part); {
			chunk := min(len(part)-start, len(scratch)/4)
			for i := 0; i < chunk; i++ {
				binary.LittleEndian.PutUint32(scratch[i*4:], part[start+i])
			}
			bytes := scratch[:chunk*4]
			n, err := w.Write(bytes)
			written += int64(n)
			if err != nil {
				return written, err
			}
			if n != len(bytes) {
				return written, io.ErrShortWrite
			}
			start += chunk
		}
	}
	return written, nil
}

type indexSectionTableEntry struct {
	tag    uint32
	offset uint64
	length uint64
	flags  uint32
}

func writeIndexFile(f *os.File, idx *Index) error {
	bw := bufio.NewWriterSize(f, 16*1024*1024)
	cw := &countingWriter{w: bw}
	sectionOffsetPatch := int64(binary.Size(diskHeader{}))
	header := diskHeader{
		Magic:      indexMagic,
		Version:    indexVersion,
		EntryCount: uint64(idx.compactRecordCount()),
		RootCount:  uint64(len(idx.Roots)),
		BuiltUnix:  idx.BuiltAt.UnixNano(),
		JournalID:  idx.JournalID,
		Checkpoint: idx.Checkpoint,
		Compact:    compactDiskFlag,
	}
	recordCount := idx.compactRecordCount()
	nameBlob := make([]byte, 0, recordCount*16)
	nameOffs := make([]uint32, 0, recordCount)
	nameLens := make([]uint16, 0, recordCount)
	nameTokens := make([]string, 0, recordCount)
	nameIDForRecord := make([]uint32, recordCount)
	if recs := idx.MMapRecords; recs != nil && len(recs.tokenTable) > 0 {
		// The source is already tokenized: every record references a stable name
		// token, so dedup by token id (O(1), no string hashing) and compact away
		// tokens that no longer appear.  Hashing every name string dominated the
		// record-table pass on multi-million-name volumes.
		srcToDst := make([]int32, len(recs.tokenTable)/6)
		for i := range srcToDst {
			srcToDst[i] = -1
		}
		for i := 0; i < recordCount; i++ {
			rec := idx.compactRecord(i)
			if len(rec.Name) > int(^uint16(0)) {
				return errors.New("compact name too large")
			}
			token := int(rec.NameOff)
			if token < 0 || token >= len(srcToDst) {
				return errors.New("compact name token out of range")
			}
			id := srcToDst[token]
			if id < 0 {
				id = int32(len(nameOffs))
				srcToDst[token] = id
				nameOffs = append(nameOffs, uint32(len(nameBlob)))
				nameLens = append(nameLens, uint16(len(rec.Name)))
				nameTokens = append(nameTokens, rec.Name)
				nameBlob = append(nameBlob, rec.Name...)
			}
			nameIDForRecord[i] = uint32(id)
		}
		srcToDst = nil
	} else {
		nameIDs := make(map[string]uint32, max(1, recordCount/2))
		for i := 0; i < recordCount; i++ {
			rec := idx.compactRecord(i)
			if len(rec.Name) > int(^uint16(0)) {
				return errors.New("compact name too large")
			}
			id, ok := nameIDs[rec.Name]
			if !ok {
				id = uint32(len(nameOffs))
				nameIDs[rec.Name] = id
				nameOffs = append(nameOffs, uint32(len(nameBlob)))
				nameLens = append(nameLens, uint16(len(rec.Name)))
				nameTokens = append(nameTokens, rec.Name)
				nameBlob = append(nameBlob, rec.Name...)
			}
			nameIDForRecord[i] = id
		}
		// The deduplication map is only needed while assigning record references.
		// Drop it before derived-section generation; retaining millions of string
		// keys here was the second large peak in index conversion.
		nameIDs = nil
		debug.FreeOSMemory()
	}
	header.NameBlobLen = uint64(len(nameBlob))
	header.TokenCount = uint64(len(nameOffs))
	if compactNeedsWideDiskRecords(recordCount, len(nameOffs)) {
		header.Compact |= compactDiskWideRefsFlag
	}
	if idx.CompactAttrs {
		header.Compact |= compactDiskAttrsFlag
	}
	if err := binary.Write(cw, binary.LittleEndian, header); err != nil {
		return err
	}
	if err := binary.Write(cw, binary.LittleEndian, uint64(0)); err != nil {
		return err
	}
	for _, s := range []string{idx.Source, idx.Volume, idx.ContentHash} {
		if err := writeString(cw, s); err != nil {
			return err
		}
	}
	for _, root := range idx.Roots {
		if err := writeString(cw, root); err != nil {
			return err
		}
	}
	wideRefs := header.Compact&compactDiskWideRefsFlag != 0
	if _, err := cw.Write(nameBlob); err != nil {
		return err
	}
	for i := range nameOffs {
		if err := binary.Write(cw, binary.LittleEndian, nameOffs[i]); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, nameLens[i]); err != nil {
			return err
		}
	}
	for i := 0; i < recordCount; i++ {
		rec := idx.compactRecord(i)
		rec.NameOff = nameIDForRecord[i]
		parent := uint32(compactNarrowParentSentinel)
		if wideRefs {
			parent = compactWideParentSentinel
		}
		if rec.Parent >= 0 {
			if !wideRefs && uint32(rec.Parent) >= compactNarrowParentSentinel {
				return errors.New("compact index too large for packed record format")
			}
			parent = uint32(rec.Parent)
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.FRN); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.ParentFRN); err != nil {
			return err
		}
		if err := writeCompactRecordRefs(cw, parent, rec.NameOff, wideRefs); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.Mode); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.Size); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, rec.ModUnix); err != nil {
			return err
		}
		deleted := uint8(0)
		if rec.Deleted {
			deleted = 1
		}
		if err := binary.Write(cw, binary.LittleEndian, deleted); err != nil {
			return err
		}
	}
	// The record table is now durable in the output stream.  Keep only
	// nameTokens for LOWR/PNGR generation and release the other name-table
	// working buffers before building postings.
	nameIDForRecord = nil
	nameOffs = nil
	nameLens = nil
	nameBlob = nil
	debug.FreeOSMemory()
	persistTrace("record-table")
	table, err := writeDerivedSectionStream(cw, idx, nameTokens)
	if err != nil {
		return err
	}
	persistTrace("derived-complete")
	if err := writeAlignment(cw, 8); err != nil {
		return err
	}
	sectionTableOffset := uint64(cw.n)
	if err := binary.Write(cw, binary.LittleEndian, uint32(len(table))); err != nil {
		return err
	}
	for _, entry := range table {
		if err := binary.Write(cw, binary.LittleEndian, entry.tag); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, entry.offset); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, entry.length); err != nil {
			return err
		}
		if err := binary.Write(cw, binary.LittleEndian, entry.flags); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if _, err := f.Seek(sectionOffsetPatch, io.SeekStart); err != nil {
		return err
	}
	var patch [8]byte
	binary.LittleEndian.PutUint64(patch[:], sectionTableOffset)
	if _, err := f.Write(patch[:]); err != nil {
		return err
	}
	_, err = f.Seek(0, io.SeekEnd)
	return err
}

var persistStageObserver func(string, runtime.MemStats)

func releasePersistStage() {
	if serviceLowMemoryMode() {
		runtime.GC()
		debug.FreeOSMemory()
	}
}

func persistTrace(stage string) {
	if v, _ := envFirst("SEEKFS_PERSIST_TRACE", "SEEKFS_V9_PERSIST_TRACE"); v != "1" {
		return
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	if persistStageObserver != nil {
		persistStageObserver(stage, mem)
	}
	fmt.Fprintf(os.Stderr, "persist stage=%s time=%s heap_alloc=%d heap_inuse=%d heap_objects=%d\n",
		stage, time.Now().Format(time.RFC3339Nano), mem.HeapAlloc, mem.HeapInuse, mem.HeapObjects)
	if dir, _ := envFirst("SEEKFS_PERSIST_PROFILE_DIR", "SEEKFS_V9_PERSIST_PROFILE_DIR"); dir != "" {
		if os.MkdirAll(dir, 0o755) == nil {
			name := strings.NewReplacer("\\", "_", "/", "_", ":", "_").Replace(stage)
			if f, err := os.Create(filepath.Join(dir, name+".pprof")); err == nil {
				_ = pprof.Lookup("heap").WriteTo(f, 0)
				_ = f.Close()
			}
		}
	}
}

func writeAlignment(w io.Writer, align int64) error {
	if align <= 1 {
		return nil
	}
	if cw, ok := w.(*countingWriter); ok {
		pad := int((align - (cw.n % align)) % align)
		if pad == 0 {
			return nil
		}
		_, err := cw.Write(make([]byte, pad))
		return err
	}
	return nil
}

func newDerivedSectionVolumeIndex(idx *Index) *serviceVolumeIndex {
	return newDerivedSectionVolumeIndexMode(idx, false)
}

func newStagedDerivedSectionVolumeIndex(idx *Index) *serviceVolumeIndex {
	return newDerivedSectionVolumeIndexMode(idx, true)
}

func newDerivedSectionVolumeIndexMode(idx *Index, staged bool) *serviceVolumeIndex {
	vol := &serviceVolumeIndex{
		index:       idx,
		volume:      idx.Volume,
		state:       "ready",
		pathCache:   make(map[int]string),
		lastPersist: time.Now(),
	}
	// Reuse already-persisted topology without copying it.  An index built
	// without derived topology is filled by buildDerivedSectionBlobs below.
	vol.childOffsets = idx.Derived.ChildOffsets
	vol.childIDs = idx.Derived.ChildIDs
	vol.rootIDs = idx.Derived.RootIDs
	vol.subtreeOrder = idx.Derived.SubtreeOrder
	vol.subtreeStart = idx.Derived.SubtreeStart
	vol.subtreeEnd = idx.Derived.SubtreeEnd
	if staged {
		vol.queryIndex = &residentQueryIndex{}
	} else {
		vol.queryIndex = buildResidentQueryIndexForPersistence(vol)
	}
	return vol
}

func buildPersistencePostingFamily(vol *serviceVolumeIndex, family string) {
	if vol == nil || vol.index == nil || vol.queryIndex == nil {
		return
	}
	recordCount := vol.index.compactRecordCount()
	switch family {
	case "ext":
		if postings := vol.index.Derived.Postings; postings != nil && len(postings[indexSectionPEXT].Data) > 0 {
			return
		}
		vol.queryIndex.ext = make(map[string][]uint32)
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			ext := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
			if ext != "" {
				vol.queryIndex.ext[strings.ToLower(ext)] = append(vol.queryIndex.ext[strings.ToLower(ext)], uint32(id))
			}
		}
		sortResidentPostings(vol.queryIndex.ext)
	case "components":
		if postings := vol.index.Derived.Postings; postings != nil && len(postings[indexSectionPCMP].Data) > 0 {
			return
		}
		vol.queryIndex.components = make(map[string][]uint32)
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
				continue
			}
			name := vol.index.compactLowerNameAt(id)
			if name != "" && name != "." {
				vol.queryIndex.components[name] = append(vol.queryIndex.components[name], uint32(id))
			}
		}
		sortResidentPostings(vol.queryIndex.components)
	case "attrs":
		if len(vol.index.Derived.AttrBits) > 0 {
			vol.queryIndex.attrBits = vol.index.Derived.AttrBits
			return
		}
		vol.queryIndex.attrBits = make(map[uint32][]uint32, 5)
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			for _, bit := range queryAttrBits() {
				if rec.Mode&bit == bit {
					vol.queryIndex.attrBits[bit] = append(vol.queryIndex.attrBits[bit], uint32(id))
				}
			}
		}
		sortResidentAttrPostings(vol.queryIndex.attrBits)
	}
}

func populatePersistenceFRNs(vol *serviceVolumeIndex, idx *Index) {
	if vol == nil || idx == nil || !idx.Compact || idx.Source != "usn" {
		return
	}
	recordCount := idx.compactRecordCount()
	if len(idx.Derived.FRNs) == recordCount && len(idx.Derived.FRNRecordIDs) == recordCount {
		vol.frns = idx.Derived.FRNs
		vol.frnRecordIDs = idx.Derived.FRNRecordIDs
		return
	}
	vol.frns = make([]uint64, 0, recordCount)
	vol.frnRecordIDs = make([]uint32, 0, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if rec.FRN != 0 {
			vol.frns = append(vol.frns, rec.FRN)
			vol.frnRecordIDs = append(vol.frnRecordIDs, uint32(id))
		}
	}
	sortFRNIndexEntries(vol.frns, vol.frnRecordIDs)
}

// buildPersistenceSeqs returns the per-record-id NTFS sequence column, or nil
// when no record carries a sequence (walk sources and v9-upgraded indexes), so
// the SEQN section is never written empty. A persisted FRNSeq column is
// authoritative; otherwise the in-memory records are scanned.
func buildPersistenceSeqs(idx *Index) []uint16 {
	if idx == nil || !idx.Compact {
		return nil
	}
	recordCount := idx.compactRecordCount()
	if recordCount == 0 {
		return nil
	}
	if seqs := idx.Derived.FRNSeq; len(seqs) == recordCount {
		for _, s := range seqs {
			if s != 0 {
				return seqs
			}
		}
		return nil
	}
	seqs := make([]uint16, recordCount)
	any := false
	for id := 0; id < recordCount; id++ {
		if s := idx.compactRecord(id).Seq; s != 0 {
			seqs[id] = s
			any = true
		}
	}
	if !any {
		return nil
	}
	return seqs
}

// recordSeq returns the NTFS sequence for a base record id. The persisted SEQN
// column wins (it is the only copy in low-memory/mmapped mode); otherwise the
// in-memory record carries it. 0 means unknown.
func (idx *Index) recordSeq(id int) uint16 {
	if idx == nil || id < 0 {
		return 0
	}
	if id < len(idx.Derived.FRNSeq) {
		if s := idx.Derived.FRNSeq[id]; s != 0 {
			return s
		}
	}
	if id < len(idx.Records) {
		return idx.Records[id].Seq
	}
	return 0
}

// seqColumn returns the per-record-id sequence column for joins, or nil when no
// record carries one (walk sources and v9-upgraded indexes).
func (idx *Index) seqColumn() []uint16 {
	if idx == nil {
		return nil
	}
	if len(idx.Derived.FRNSeq) > 0 {
		return idx.Derived.FRNSeq
	}
	if len(idx.Records) == 0 {
		return nil
	}
	seqs := make([]uint16, len(idx.Records))
	any := false
	for i := range idx.Records {
		if idx.Records[i].Seq != 0 {
			seqs[i] = idx.Records[i].Seq
			any = true
		}
	}
	if !any {
		return nil
	}
	return seqs
}

func buildDerivedSectionBlobs(idx *Index, nameTokens []string) []indexSectionBlob {
	out := make([]indexSectionBlob, 0, 16)
	_ = forEachDerivedSection(idx, nameTokens, func(section indexSectionBlob) error {
		if section.subtree != nil {
			section.data = encodeUint32Section(section.subtree.parts...)
			section.subtree = nil
		}
		out = append(out, section)
		return nil
	})
	return out
}

// forEachDerivedSection builds independent persistence families in output
// order.  Rank orders are emitted as soon as they are built; only the rank
// vectors needed by later rank-bound sections remain live.  This keeps the
// offline writer from preparing all serving maps, order arrays, and subtree
// workspaces at the same time.
func forEachDerivedSection(idx *Index, nameTokens []string, emit func(indexSectionBlob) error) error {
	vol := newStagedDerivedSectionVolumeIndex(idx)
	if vol == nil {
		return nil
	}
	persistTrace("resident-prepared")
	emitSection := func(tag uint32, data []byte) error {
		return emit(indexSectionBlob{tag: tag, data: data})
	}
	emitSubtree := func(parts ...[]uint32) error {
		return emit(indexSectionBlob{tag: indexSectionSUBT, subtree: &subtreeSectionBlob{parts: parts}})
	}

	nameOrder, nameRank := vol.queryIndex.nameOrder, vol.queryIndex.nameRank
	if len(nameOrder) == 0 || len(nameRank) == 0 {
		nameOrder, nameRank = buildCompactNameOrderRank(idx)
	}
	vol.queryIndex.nameOrder, vol.queryIndex.nameRank = nameOrder, nameRank
	persistTrace("name-rank-ready")
	// Rank sections persist the order alone; the rank-by-id vector is the
	// exact inverse and is rebuilt at load (see decodeRankSection), halving
	// the six rank sections. The in-memory rank stays live for the posting
	// bounds built below.
	if err := emitSection(indexSectionRANK, encodeUint32Section(nameOrder)); err != nil {
		return err
	}
	vol.queryIndex.nameOrder = nil
	nameOrder = nil

	// Build the child graph and recursive directory sizes before the size rank:
	// sort:size must rank a directory by its subtree total, not its stored 0.
	if len(vol.childOffsets) == 0 || len(vol.childIDs) == 0 {
		vol.buildCompactChildren()
	}
	if len(vol.subtreeOrder) == 0 && len(vol.childOffsets) > 0 {
		vol.buildSubtreeRanges()
	}
	vol.subtreeBytes = vol.buildSubtreeBytes()
	persistTrace("children-ready")

	var sizeRank, modRank, extRank, typeRank, pathRank []uint32
	if idx.compactHasSize() {
		order, rank := buildCompactSizeOrderRankWithDirBytes(idx, vol.subtreeBytes)
		vol.queryIndex.sizeOrder, vol.queryIndex.sizeRank = order, rank
		sizeRank = rank
		persistTrace("size-rank-ready")
		if err := emitSection(indexSectionSRNK, encodeUint32Section(order)); err != nil {
			return err
		}
		vol.queryIndex.sizeOrder = nil
		vol.queryIndex.sizeRank = nil
	}
	if idx.compactHasModTime() {
		order, rank := vol.queryIndex.modOrder, vol.queryIndex.modRank
		if len(order) == 0 || len(rank) == 0 {
			order, rank = buildCompactModifiedOrderRank(idx)
		}
		vol.queryIndex.modOrder, vol.queryIndex.modRank = order, rank
		modRank = rank
		persistTrace("modified-rank-ready")
		if err := emitSection(indexSectionMRNK, encodeUint32Section(order)); err != nil {
			return err
		}
		vol.queryIndex.modOrder = nil
		vol.queryIndex.modRank = nil
	}
	{
		order, rank := vol.queryIndex.extOrder, vol.queryIndex.extRank
		if len(order) == 0 || len(rank) == 0 {
			order, rank = buildCompactExtensionOrderRank(idx)
		}
		vol.queryIndex.extOrder, vol.queryIndex.extRank = order, rank
		extRank = rank
		persistTrace("extension-rank-ready")
		if err := emitSection(indexSectionERNK, encodeUint32Section(order)); err != nil {
			return err
		}
		vol.queryIndex.extOrder = nil
	}
	{
		order, rank := vol.queryIndex.typeOrder, vol.queryIndex.typeRank
		if len(order) == 0 || len(rank) == 0 {
			order, rank = buildCompactTypeOrderRank(idx)
		}
		vol.queryIndex.typeOrder, vol.queryIndex.typeRank = order, rank
		typeRank = rank
		persistTrace("type-rank-ready")
		if err := emitSection(indexSectionTRNK, encodeUint32Section(order)); err != nil {
			return err
		}
		vol.queryIndex.typeOrder = nil
	}
	{
		order, rank := vol.queryIndex.pathOrder, vol.queryIndex.pathRank
		if len(order) == 0 || len(rank) == 0 {
			order, rank = buildCompactPathOrderRank(idx)
		}
		vol.queryIndex.pathOrder, vol.queryIndex.pathRank = order, rank
		pathRank = rank
		persistTrace("path-rank-ready")
		if err := emitSection(indexSectionPRNK, encodeUint32Section(order)); err != nil {
			return err
		}
		vol.queryIndex.pathOrder = nil
	}
	// The path rank's lowercased full-path keys are the build's largest transient
	// by an order of magnitude (multi-GiB on multi-million-record volumes).
	// Release them before the derived posting families and subtree minima
	// allocate on top, so peak stays bounded instead of riding on GC timing.
	runtime.GC()
	// Extension postings only need the raw rank vectors. Emit this family
	// before subtree minima are prepared, so those raw vectors can be released
	// as each corresponding subtree-minimum vector is built.
	buildPersistencePostingFamily(vol, "ext")
	if err := emitSection(indexSectionPEXT, encodeStringPostingSection(vol.queryIndex.ext, nameRank)); err != nil {
		return err
	}
	if err := emitSection(indexSectionPXRB, encodePostingRankBounds(buildStringPostingRankBounds(
		vol.queryIndex.ext, nameRank, sizeRank, modRank, extRank, typeRank, pathRank,
	))); err != nil {
		return err
	}
	vol.queryIndex.ext = nil

	if len(vol.childOffsets) == 0 || len(vol.childIDs) == 0 {
		vol.buildCompactChildren()
	}
	persistTrace("children-ready")
	if len(vol.subtreeOrder) == 0 && len(vol.childOffsets) > 0 {
		vol.buildSubtreeRanges()
	}
	persistTrace("subtree-ready")
	if len(sizeRank) == 0 {
		sizeRank = nameRank
	}
	if len(modRank) == 0 {
		modRank = nameRank
	}
	vol.subtreeSizeRank = vol.buildSubtreeMinRanks(sizeRank)
	sizeRank = nil
	vol.subtreeModRank = vol.buildSubtreeMinRanks(modRank)
	modRank = nil
	vol.subtreeExtRank = vol.buildSubtreeMinRanks(extRank)
	extRank = nil
	vol.queryIndex.extRank = nil
	vol.subtreeTypeRank = vol.buildSubtreeMinRanks(typeRank)
	typeRank = nil
	vol.queryIndex.typeRank = nil
	vol.subtreePathRank = vol.buildSubtreeMinRanks(pathRank)
	pathRank = nil
	vol.queryIndex.pathRank = nil
	releasePersistStage()
	persistTrace("derived-prepared")
	if err := emitSection(indexSectionCHLD, encodeUint32Section(vol.childOffsets, vol.childIDs, vol.rootIDs)); err != nil {
		return err
	}
	// The subtree start array is the exact inverse of the subtree order
	// and is rebuilt at load (see decodeDerivedSection); persisting it
	// would store the same permutation twice.
	if err := emitSubtree(
		vol.subtreeEnd, vol.subtreeOrder,
		vol.subtreeSizeRank, vol.subtreeModRank, vol.subtreeExtRank,
		vol.subtreeTypeRank, vol.subtreePathRank,
	); err != nil {
		return err
	}
	if len(vol.subtreeBytes) > 0 {
		if err := emitSection(indexSectionSUBS, encodeUint64Section(vol.subtreeBytes)); err != nil {
			return err
		}
	}
	populatePersistenceFRNs(vol, idx)
	if err := emitSection(indexSectionFRNS, encodeFRNSection(vol.frns, vol.frnRecordIDs)); err != nil {
		return err
	}
	vol.frns, vol.frnRecordIDs = nil, nil
	if seqs := buildPersistenceSeqs(idx); len(seqs) > 0 {
		if err := emitSection(indexSectionSEQN, encodeSeqSection(seqs)); err != nil {
			return err
		}
	}
	if err := emitSection(indexSectionLOWR, encodeLowerSection(nameTokens)); err != nil {
		return err
	}
	buildPersistencePostingFamily(vol, "attrs")
	if err := emitSection(indexSectionPATR, encodeAttrPostingSection(vol.queryIndex.attrBits)); err != nil {
		return err
	}
	vol.queryIndex.attrBits = nil

	buildPersistencePostingFamily(vol, "components")
	if err := emitSection(indexSectionPCMP, encodeStringPostingSection(vol.queryIndex.components, nameRank)); err != nil {
		return err
	}
	if err := emitSection(indexSectionPXRC, encodePostingRankBounds(buildComponentPostingRankBounds(vol.queryIndex.components, vol))); err != nil {
		return err
	}
	vol.queryIndex.components = nil
	vol.childOffsets, vol.childIDs, vol.rootIDs = nil, nil, nil
	vol.subtreeStart, vol.subtreeEnd, vol.subtreeOrder = nil, nil, nil
	vol.subtreeSizeRank, vol.subtreeModRank, vol.subtreeExtRank = nil, nil, nil
	vol.subtreeTypeRank, vol.subtreePathRank = nil, nil

	if shouldUseExternalNameGram(idx.compactRecordCount()) {
		pngr, pngc, gramErr := buildNameGramIndexExternal(context.Background(), idx, 3, serviceLowMemoryTrigramStoredPostingMax(), nameGramSpoolDir())
		if gramErr != nil {
			return gramErr
		}
		if err := emitSection(indexSectionPNGR, encodeGramPostingSection(pngr, nameRank)); err != nil {
			return err
		}
		if pngc != nil && selfNameGramSectionsEnabled() {
			if err := emitSection(indexSectionPNGC, encodeGramPostingSection(pngc, nameRank)); err != nil {
				return err
			}
		}
		return nil
	}

	nameGrams, nameGramsCompanion := buildSelectiveNameGramIndexWithCompanion(idx, serviceLowMemoryTrigramStoredPostingMax())
	gramData := encodeGramPostingSection(nameGrams, nameRank)
	var selfNameGramData []byte
	if nameGramsCompanion != nil && selfNameGramSectionsEnabled() {
		selfNameGramData = encodeGramPostingSection(nameGramsCompanion, nameRank)
	}
	nameGrams = nil
	nameGramsCompanion = nil
	if err := emitSection(indexSectionPNGR, gramData); err != nil {
		return err
	}
	if len(selfNameGramData) > 0 {
		if err := emitSection(indexSectionPNGC, selfNameGramData); err != nil {
			return err
		}
	}
	return nil
}

func writeDerivedSectionStream(cw *countingWriter, idx *Index, nameTokens []string) ([]indexSectionTableEntry, error) {
	return writeDerivedSectionStreamObserved(cw, idx, nameTokens, nil)
}

// writeDerivedSectionStreamObserved is also used by the bounded writer
// benchmark.  The callback observes the one section buffer currently held by
// the stream; it must return to zero before the next section is built.
func writeDerivedSectionStreamObserved(cw *countingWriter, idx *Index, nameTokens []string, observe func(int)) ([]indexSectionTableEntry, error) {
	table := make([]indexSectionTableEntry, 0, 16)
	err := forEachDerivedSection(idx, nameTokens, func(section indexSectionBlob) error {
		if len(section.data) == 0 && section.subtree == nil {
			return nil
		}
		persistTrace(fmt.Sprintf("section-%08x", section.tag))
		if err := writeAlignment(cw, 8); err != nil {
			return err
		}
		offset := uint64(cw.n)
		before := cw.n
		if section.subtree != nil {
			if _, err := writeSubtreeSection(cw, section.subtree); err != nil {
				return err
			}
		} else {
			if observe != nil {
				observe(len(section.data))
			}
			if _, err := cw.Write(section.data); err != nil {
				return err
			}
		}
		length := uint64(cw.n - before)
		if observe != nil {
			observe(int(length))
		}
		table = append(table, indexSectionTableEntry{tag: section.tag, offset: offset, length: length, flags: section.flags})
		persistTrace(fmt.Sprintf("after-%08x", section.tag))
		if observe != nil {
			observe(0)
		}
		return nil
	})
	return table, err
}

func derivedSectionInfo(derived indexDerivedSections) ([]string, int) {
	sections := make([]string, 0, 4)
	bytes := 0
	if len(derived.NameOrder) > 0 || len(derived.NameRank) > 0 {
		sections = append(sections, "RANK")
		bytes += 4 * (len(derived.NameOrder) + len(derived.NameRank))
	}
	if len(derived.SizeOrder) > 0 || len(derived.SizeRank) > 0 {
		sections = append(sections, "SRNK")
		bytes += 4 * (len(derived.SizeOrder) + len(derived.SizeRank))
	}
	if len(derived.ModOrder) > 0 || len(derived.ModRank) > 0 {
		sections = append(sections, "MRNK")
		bytes += 4 * (len(derived.ModOrder) + len(derived.ModRank))
	}
	if len(derived.ExtOrder) > 0 || len(derived.ExtRank) > 0 {
		sections = append(sections, "ERNK")
		bytes += 4 * (len(derived.ExtOrder) + len(derived.ExtRank))
	}
	if len(derived.TypeOrder) > 0 || len(derived.TypeRank) > 0 {
		sections = append(sections, "TRNK")
		bytes += 4 * (len(derived.TypeOrder) + len(derived.TypeRank))
	}
	if len(derived.PathOrder) > 0 || len(derived.PathRank) > 0 {
		sections = append(sections, "PRNK")
		bytes += 4 * (len(derived.PathOrder) + len(derived.PathRank))
	}
	if len(derived.ChildOffsets) > 0 || len(derived.ChildIDs) > 0 || len(derived.RootIDs) > 0 {
		sections = append(sections, "CHLD")
		bytes += 4 * (len(derived.ChildOffsets) + len(derived.ChildIDs) + len(derived.RootIDs))
	}
	if len(derived.SubtreeStart) > 0 || len(derived.SubtreeEnd) > 0 || len(derived.SubtreeOrder) > 0 {
		sections = append(sections, "SUBT")
		bytes += 4 * (len(derived.SubtreeStart) + len(derived.SubtreeEnd) + len(derived.SubtreeOrder) +
			len(derived.SubtreeSizeRank) + len(derived.SubtreeModRank) + len(derived.SubtreeExtRank) +
			len(derived.SubtreeTypeRank) + len(derived.SubtreePathRank))
	}
	if len(derived.SubtreeBytes) > 0 {
		sections = append(sections, "SUBS")
		bytes += 8 * len(derived.SubtreeBytes)
	}
	if len(derived.FRNs) > 0 || len(derived.FRNRecordIDs) > 0 {
		sections = append(sections, "FRNS")
		bytes += 8*len(derived.FRNs) + 4*len(derived.FRNRecordIDs)
	}
	if len(derived.FRNSeq) > 0 {
		sections = append(sections, "SEQN")
		bytes += 2 * len(derived.FRNSeq)
	}
	if len(derived.LowerOffs) > 0 || len(derived.LowerBlob) > 0 {
		sections = append(sections, "LOWR")
		bytes += 6*len(derived.LowerOffs) + len(derived.LowerBlob)
	}
	for _, item := range []struct {
		tag  uint32
		name string
	}{
		{indexSectionPATR, "PATR"},
		{indexSectionPEXT, "PEXT"},
		{indexSectionPXRB, "PXRB"},
		{indexSectionPXRC, "PXRC"},
		{indexSectionPCMP, "PCMP"},
		{indexSectionPNGR, "PNGR"},
		{indexSectionPNGC, "PNGC"},
	} {
		if item.tag == indexSectionPATR {
			if len(derived.AttrBits) > 0 {
				count := 0
				for _, ids := range derived.AttrBits {
					count += len(ids)
				}
				sections = append(sections, item.name)
				bytes += 4 * (len(queryAttrBits()) + count)
			}
			continue
		}
		if item.tag == indexSectionPXRB || item.tag == indexSectionPXRC {
			boundsTag := indexSectionPEXT
			if item.tag == indexSectionPXRC {
				boundsTag = indexSectionPCMP
			}
			if bounds, ok := derived.PostingBounds[boundsTag]; ok && bounds.BlockCount > 0 {
				sections = append(sections, item.name)
				bytes += 4 * (5 + len(bounds.Size) + len(bounds.Modified) + len(bounds.Extension) + len(bounds.Type) + len(bounds.Path))
			}
			continue
		}
		if posting, ok := derived.Postings[item.tag]; ok && posting.EntryCount > 0 {
			sections = append(sections, item.name)
			bytes += posting.Bytes
		}
	}
	return sections, bytes
}

func encodeUint32Section(parts ...[]uint32) []byte {
	var buf bytes.Buffer
	for _, part := range parts {
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(part)))
		_ = binary.Write(&buf, binary.LittleEndian, part)
	}
	return buf.Bytes()
}

func encodeUint64Section(values []uint64) []byte {
	return uint64SliceBytes(values)
}

func encodeFRNSection(frns []uint64, ids []uint32) []byte {
	if len(frns) == 0 || len(frns) != len(ids) {
		return nil
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(frns)))
	_ = binary.Write(&buf, binary.LittleEndian, frns)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(ids)))
	_ = binary.Write(&buf, binary.LittleEndian, ids)
	return buf.Bytes()
}

// encodeSeqSection writes the per-record-id NTFS sequence column: a uint32
// count followed by that many little-endian uint16 values.
func encodeSeqSection(seqs []uint16) []byte {
	if len(seqs) == 0 {
		return nil
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(seqs)))
	_ = binary.Write(&buf, binary.LittleEndian, seqs)
	return buf.Bytes()
}

func encodeLowerSection(nameTokens []string) []byte {
	if len(nameTokens) == 0 {
		return nil
	}
	offs := make([]uint32, len(nameTokens))
	lens := make([]uint16, len(nameTokens))
	blob := make([]byte, 0, len(nameTokens)*8)
	refs := make(map[string]uint32, max(1, len(nameTokens)/2))
	lengths := make(map[string]uint16, max(1, len(nameTokens)/2))
	for i, name := range nameTokens {
		lower := strings.ToLower(name)
		if len(lower) > int(^uint16(0)) {
			lower = lower[:int(^uint16(0))]
		}
		lens[i] = uint16(len(lower))
		if lower == name {
			offs[i] = packedLowerSameAsName
			continue
		}
		if off, ok := refs[lower]; ok {
			offs[i] = off
			lens[i] = lengths[lower]
			continue
		}
		off := uint32(len(blob))
		refs[lower] = off
		lengths[lower] = uint16(len(lower))
		offs[i] = off
		blob = append(blob, lower...)
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(nameTokens)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(blob)))
	_ = binary.Write(&buf, binary.LittleEndian, offs)
	_ = binary.Write(&buf, binary.LittleEndian, lens)
	_, _ = buf.Write(blob)
	return buf.Bytes()
}

// attrPostingMagic marks the delta-varint PATR layout ("PTR2"). A legacy
// raw-u32 section never starts with this word (it would be a 1.3-billion
// entry first part), so the decoder tells the two framings apart.
const attrPostingMagic = 0x50545232

// encodeAttrPostingSection serializes the per-attribute-bit record-ID lists.
// Attribute lists are sorted record IDs with small gaps (an attribute covers
// a large slice of the corpus), so delta-varint encoding roughly halves the
// section versus the legacy raw-u32 parts. Layout: magic u32, part count
// u32, then per part {count u32, encoded length u32, delta bytes}. Empty
// input still encodes as nil (the section is omitted), matching history.
func encodeAttrPostingSection(attrBits map[uint32][]uint32) []byte {
	if len(attrBits) == 0 {
		return nil
	}
	bits := queryAttrBits()
	parts := make([][]uint32, 0, len(bits))
	hasAny := false
	for _, bit := range bits {
		ids := uniqueSortedUint32s(append([]uint32(nil), attrBits[bit]...))
		if len(ids) > 0 {
			hasAny = true
		}
		parts = append(parts, ids)
	}
	if !hasAny {
		return nil
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(attrPostingMagic))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(parts)))
	for _, ids := range parts {
		enc := encodeDeltaUvarint32(ids)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(ids)))
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(enc)))
		buf.Write(enc)
	}
	return buf.Bytes()
}

func decodeAttrPostingSection(data []byte) map[uint32][]uint32 {
	if len(data) >= 8 && binary.LittleEndian.Uint32(data[:]) == attrPostingMagic {
		return decodeAttrPostingSectionDelta(data)
	}
	parts := decodeUint32Section(data, len(queryAttrBits()))
	if len(parts) != len(queryAttrBits()) {
		return nil
	}
	out := make(map[uint32][]uint32, len(parts))
	for i, bit := range queryAttrBits() {
		if len(parts[i]) > 0 {
			out[bit] = parts[i]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeAttrPostingSectionDelta decodes the magic-marked delta-varint
// layout, yielding the same sorted unique ID lists the legacy form holds.
// Any framing error yields nil (treated as a missing section, exactly like
// a legacy section that fails its part-count check today).
func decodeAttrPostingSectionDelta(data []byte) map[uint32][]uint32 {
	bits := queryAttrBits()
	off := 8
	partCount := int(binary.LittleEndian.Uint32(data[4:]))
	if partCount != len(bits) {
		return nil
	}
	out := make(map[uint32][]uint32, partCount)
	for _, bit := range bits {
		if off+8 > len(data) {
			return nil
		}
		count := int(binary.LittleEndian.Uint32(data[off:]))
		encLen := int(binary.LittleEndian.Uint32(data[off+4:]))
		off += 8
		if count < 0 || encLen < 0 || off+encLen < off || off+encLen > len(data) {
			return nil
		}
		if count == 0 {
			if encLen != 0 {
				return nil
			}
			continue
		}
		ids := decodeDeltaUvarint32(data[off:off+encLen], count)
		if len(ids) != count {
			return nil
		}
		for j := 1; j < len(ids); j++ {
			if ids[j] <= ids[j-1] {
				return nil
			}
		}
		off += encLen
		out[bit] = ids
	}
	if off != len(data) || len(out) == 0 {
		return nil
	}
	return out
}

func buildStringPostingRankBounds(postings map[string][]uint32, nameRank, sizeRank, modRank, extRank, typeRank, pathRank []uint32) postingRankBounds {
	if len(sizeRank) == 0 {
		sizeRank = nameRank
	}
	if len(modRank) == 0 {
		modRank = nameRank
	}
	if len(extRank) == 0 {
		extRank = nameRank
	}
	if len(typeRank) == 0 {
		typeRank = nameRank
	}
	if len(pathRank) == 0 {
		pathRank = nameRank
	}
	if len(postings) == 0 || len(nameRank) == 0 || len(sizeRank) == 0 || len(modRank) == 0 || len(extRank) == 0 || len(typeRank) == 0 || len(pathRank) == 0 {
		return postingRankBounds{}
	}
	keys := make([]string, 0, len(postings))
	for key, ids := range postings {
		if key != "" && len(ids) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var bounds postingRankBounds
	for _, key := range keys {
		if len(key) > int(^uint16(0)) {
			continue
		}
		ids := uniqueSortedUint32s(append([]uint32(nil), postings[key]...))
		if len(ids) == 0 {
			continue
		}
		appendPostingRankBounds(&bounds, ids, nameRank, sizeRank, modRank, extRank, typeRank, pathRank)
	}
	bounds.BlockCount = len(bounds.Size)
	if bounds.BlockCount == 0 {
		return postingRankBounds{}
	}
	return bounds
}

func appendPostingRankBounds(bounds *postingRankBounds, ids []uint32, nameRank, sizeRank, modRank, extRank, typeRank, pathRank []uint32) {
	const blockSize = 1024
	for start := 0; start < len(ids); start += blockSize {
		end := min(len(ids), start+blockSize)
		chunk := ids[start:end]
		bounds.Name = append(bounds.Name, minRankForIDs(chunk, nameRank))
		bounds.Size = append(bounds.Size, minRankForIDs(chunk, sizeRank))
		bounds.Modified = append(bounds.Modified, minRankForIDs(chunk, modRank))
		bounds.Extension = append(bounds.Extension, minRankForIDs(chunk, extRank))
		bounds.Type = append(bounds.Type, minRankForIDs(chunk, typeRank))
		bounds.Path = append(bounds.Path, minRankForIDs(chunk, pathRank))
	}
}

// buildComponentPostingRankBounds stores the best possible descendant rank
// for each PCMP block. A component posting contains directory roots, so using
// the root's own rank would be unsound; the subtree minima computed from SUBT
// are the conservative bounds required for safe block skipping.
func buildComponentPostingRankBounds(postings map[string][]uint32, vol *serviceVolumeIndex) postingRankBounds {
	if vol == nil || len(postings) == 0 || len(vol.subtreeSizeRank) == 0 || len(vol.subtreeModRank) == 0 ||
		len(vol.subtreeExtRank) == 0 || len(vol.subtreeTypeRank) == 0 || len(vol.subtreePathRank) == 0 {
		return postingRankBounds{}
	}
	keys := make([]string, 0, len(postings))
	for key, ids := range postings {
		if key != "" && len(ids) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var bounds postingRankBounds
	for _, key := range keys {
		ids := uniqueSortedUint32s(append([]uint32(nil), postings[key]...))
		for start := 0; start < len(ids); start += 1024 {
			end := min(len(ids), start+1024)
			chunk := ids[start:end]
			bounds.Name = append(bounds.Name, vol.minSubtreeRankForRoots(chunk, vol.queryIndex.nameRank))
			bounds.Size = append(bounds.Size, minRankForIDs(chunk, vol.subtreeSizeRank))
			bounds.Modified = append(bounds.Modified, minRankForIDs(chunk, vol.subtreeModRank))
			bounds.Extension = append(bounds.Extension, minRankForIDs(chunk, vol.subtreeExtRank))
			bounds.Type = append(bounds.Type, minRankForIDs(chunk, vol.subtreeTypeRank))
			bounds.Path = append(bounds.Path, minRankForIDs(chunk, vol.subtreePathRank))
		}
	}
	bounds.BlockCount = len(bounds.Size)
	if bounds.BlockCount == 0 {
		return postingRankBounds{}
	}
	return bounds
}

func (vol *serviceVolumeIndex) minSubtreeRankForRoots(roots []uint32, ranks []uint32) uint32 {
	best := uint32(^uint32(0))
	if vol == nil || len(vol.subtreeOrder) == 0 {
		return best
	}
	for _, root := range roots {
		if int(root) >= len(vol.subtreeStart) || int(root) >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[root], vol.subtreeEnd[root]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		if rank := minRankForIDs(vol.subtreeOrder[start:end], ranks); rank < best {
			best = rank
		}
	}
	if best == uint32(^uint32(0)) {
		return 0
	}
	return best
}

func minRankForIDs(ids []uint32, ranks []uint32) uint32 {
	minRank := uint32(^uint32(0))
	for _, id := range ids {
		rank := extRankOf(id, ranks)
		if rank < minRank {
			minRank = rank
		}
	}
	if minRank == uint32(^uint32(0)) {
		return 0
	}
	return minRank
}

func encodePostingRankBounds(bounds postingRankBounds) []byte {
	if bounds.BlockCount <= 0 || len(bounds.Name) != bounds.BlockCount || len(bounds.Size) != bounds.BlockCount || len(bounds.Modified) != bounds.BlockCount ||
		len(bounds.Extension) != bounds.BlockCount || len(bounds.Type) != bounds.BlockCount || len(bounds.Path) != bounds.BlockCount {
		return nil
	}
	return encodeUint32Section(bounds.Name, bounds.Size, bounds.Modified, bounds.Extension, bounds.Type, bounds.Path)
}

func decodePostingRankBounds(data []byte) postingRankBounds {
	parts := decodeUint32Section(data, 6)
	if len(parts) != 6 || len(parts[0]) == 0 {
		return postingRankBounds{}
	}
	blockCount := len(parts[0])
	for _, part := range parts[1:] {
		if len(part) != blockCount {
			return postingRankBounds{}
		}
	}
	return postingRankBounds{BlockCount: blockCount, Name: parts[0], Size: parts[1], Modified: parts[2], Extension: parts[3], Type: parts[4], Path: parts[5]}
}

type postingBlockMeta struct {
	offset  uint64
	length  uint32
	count   uint32
	minID   uint32
	maxID   uint32
	minRank uint32
}

type stringPostingEntry struct {
	keyOff     uint32
	keyLen     uint16
	count      uint32
	firstBlock uint32
	blockCount uint32
}

type gramPostingEntry struct {
	key        uint32
	count      uint32
	firstBlock uint32
	blockCount uint32
}

func encodeStringPostingSection(postings map[string][]uint32, ranks []uint32) []byte {
	if len(postings) == 0 {
		return nil
	}
	keys := make([]string, 0, len(postings))
	for key, ids := range postings {
		if key != "" && len(ids) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	entries := make([]stringPostingEntry, 0, len(keys))
	var keyBlob bytes.Buffer
	var blockBlob bytes.Buffer
	blocks := make([]postingBlockMeta, 0, len(keys))
	for _, key := range keys {
		if len(key) > int(^uint16(0)) {
			continue
		}
		ids := uniqueSortedUint32s(append([]uint32(nil), postings[key]...))
		if len(ids) == 0 {
			continue
		}
		entry := stringPostingEntry{
			keyOff:     uint32(keyBlob.Len()),
			keyLen:     uint16(len(key)),
			count:      uint32(len(ids)),
			firstBlock: uint32(len(blocks)),
		}
		_, _ = keyBlob.WriteString(key)
		blocks = appendPostingBlocks(blocks, &blockBlob, ids, ranks)
		entry.blockCount = uint32(len(blocks)) - entry.firstBlock
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(entries)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(keyBlob.Len()))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(blocks)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(blockBlob.Len()))
	for _, entry := range entries {
		_ = binary.Write(&buf, binary.LittleEndian, entry.keyOff)
		_ = binary.Write(&buf, binary.LittleEndian, entry.keyLen)
		_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
		_ = binary.Write(&buf, binary.LittleEndian, entry.count)
		_ = binary.Write(&buf, binary.LittleEndian, entry.firstBlock)
		_ = binary.Write(&buf, binary.LittleEndian, entry.blockCount)
	}
	writePostingBlockMetas(&buf, blocks)
	_, _ = buf.Write(keyBlob.Bytes())
	_, _ = buf.Write(blockBlob.Bytes())
	return buf.Bytes()
}

func encodeGramPostingSection(ti *compressedTrigramIndex, ranks []uint32) []byte {
	return encodeGramPostingSectionWithMetadata(ti, ranks, gramPostingMetadataMagic)
}

func encodeGramPostingSectionWithMetadata(ti *compressedTrigramIndex, ranks []uint32, metadataMagic uint32) []byte {
	if ti == nil {
		return nil
	}
	keys := make([]uint32, 0)
	ti.forEachCount(func(gram uint32, count int) {
		if count > 0 {
			keys = append(keys, gram)
		}
	})
	sortUint32s(keys)
	entries := make([]gramPostingEntry, 0, len(keys))
	var blockBlob bytes.Buffer
	blocks := make([]postingBlockMeta, 0, len(keys))
	for _, key := range keys {
		ids := trigramPostingIDs(ti, key)
		if len(ids) == 0 {
			continue
		}
		entry := gramPostingEntry{
			key:        key,
			count:      uint32(len(ids)),
			firstBlock: uint32(len(blocks)),
		}
		blocks = appendPostingBlocks(blocks, &blockBlob, ids, ranks)
		entry.blockCount = uint32(len(blocks)) - entry.firstBlock
		entries = append(entries, entry)
	}
	omitted := make([]uint32, 0, len(ti.omitted))
	for gram := range ti.omitted {
		omitted = append(omitted, gram)
	}
	sortUint32s(omitted)
	if len(entries) == 0 && len(omitted) == 0 {
		return nil
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(entries)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(blocks)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(blockBlob.Len()))
	for _, entry := range entries {
		_ = binary.Write(&buf, binary.LittleEndian, entry.key)
		_ = binary.Write(&buf, binary.LittleEndian, entry.count)
		_ = binary.Write(&buf, binary.LittleEndian, entry.firstBlock)
		_ = binary.Write(&buf, binary.LittleEndian, entry.blockCount)
	}
	writePostingBlockMetas(&buf, blocks)
	_, _ = buf.Write(blockBlob.Bytes())
	if ti.gramCountsComplete {
		_ = binary.Write(&buf, binary.LittleEndian, metadataMagic)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(omitted)))
		for _, gram := range omitted {
			_ = binary.Write(&buf, binary.LittleEndian, gram)
			_ = binary.Write(&buf, binary.LittleEndian, uint32(ti.countForGram(gram)))
		}
	}
	return buf.Bytes()
}

func appendPostingBlocks(blocks []postingBlockMeta, blob *bytes.Buffer, ids []uint32, ranks []uint32) []postingBlockMeta {
	const blockSize = 1024
	for start := 0; start < len(ids); start += blockSize {
		end := min(len(ids), start+blockSize)
		chunk := ids[start:end]
		encoded := encodeDeltaUvarint32(chunk)
		offset := uint64(blob.Len())
		_, _ = blob.Write(encoded)
		minRank := uint32(^uint32(0))
		for _, id := range chunk {
			rank := extRankOf(id, ranks)
			if rank < minRank {
				minRank = rank
			}
		}
		if minRank == uint32(^uint32(0)) {
			minRank = 0
		}
		blocks = append(blocks, postingBlockMeta{
			offset:  offset,
			length:  uint32(len(encoded)),
			count:   uint32(len(chunk)),
			minID:   chunk[0],
			maxID:   chunk[len(chunk)-1],
			minRank: minRank,
		})
	}
	return blocks
}

func writePostingBlockMetas(buf *bytes.Buffer, blocks []postingBlockMeta) {
	for _, block := range blocks {
		_ = binary.Write(buf, binary.LittleEndian, block.offset)
		_ = binary.Write(buf, binary.LittleEndian, block.length)
		_ = binary.Write(buf, binary.LittleEndian, block.count)
		_ = binary.Write(buf, binary.LittleEndian, block.minID)
		_ = binary.Write(buf, binary.LittleEndian, block.maxID)
		_ = binary.Write(buf, binary.LittleEndian, block.minRank)
	}
}

func trigramPostingIDs(ti *compressedTrigramIndex, gram uint32) []uint32 {
	if ti == nil {
		return nil
	}
	var out []uint32
	for _, segment := range ti.segments {
		posting := segment.postingForGram(gram)
		if posting.count == 0 {
			continue
		}
		out = append(out, decodeDeltaUvarint32(ti.postingData(posting), posting.count)...)
	}
	if len(out) == 0 {
		return nil
	}
	sortUint32s(out)
	return uniqueSortedUint32s(out)
}

func (vol *serviceVolumeIndex) buildSubtreeRanges() {
	if vol == nil || vol.index == nil || len(vol.childOffsets) == 0 {
		return
	}
	recordCount := vol.index.compactRecordCount()
	start := make([]uint32, recordCount)
	end := make([]uint32, recordCount)
	for i := range start {
		start[i] = ^uint32(0)
	}
	order := make([]uint32, 0, recordCount)
	seen := make([]bool, recordCount)
	var walk func(int)
	walk = func(id int) {
		if id < 0 || id >= recordCount || seen[id] {
			return
		}
		seen[id] = true
		start[id] = uint32(len(order))
		order = append(order, uint32(id))
		for _, childID := range vol.childIDsForRecord(id) {
			walk(int(childID))
		}
		end[id] = uint32(len(order))
	}
	for _, rootID := range vol.rootIDs {
		walk(int(rootID))
	}
	for id := 0; id < recordCount; id++ {
		if !seen[id] {
			walk(id)
		}
	}
	vol.subtreeOrder = order
	vol.subtreeStart = start
	vol.subtreeEnd = end
}

// buildSubtreeMinRanks computes a conservative best rank for every subtree.
// The persisted subtree order is depth-first, so children are visited before
// their parent when walking it backwards. Deleted records contribute no rank;
// this keeps block-max skips safe when a subtree contains tombstoned records.
func (vol *serviceVolumeIndex) buildSubtreeMinRanks(ranks []uint32) []uint32 {
	if vol == nil || vol.index == nil || len(ranks) < vol.index.compactRecordCount() ||
		len(vol.subtreeOrder) == 0 {
		return nil
	}
	best := make([]uint32, vol.index.compactRecordCount())
	for i := range best {
		best[i] = ^uint32(0)
	}
	for pos := len(vol.subtreeOrder) - 1; pos >= 0; pos-- {
		id := int(vol.subtreeOrder[pos])
		if id < 0 || id >= len(best) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted {
			best[id] = ranks[id]
		}
		for _, childID32 := range vol.childIDsForRecord(id) {
			childID := int(childID32)
			if childID >= 0 && childID < len(best) && best[childID] < best[id] {
				best[id] = best[childID]
			}
		}
	}
	return best
}

// buildSubtreeBytes computes the recursive file-byte total for every record
// (0 for plain files, which report their own size directly).  The subtree
// order is depth-first, so a reverse walk sees every child before its parent.
// Deleted records contribute nothing.
func (vol *serviceVolumeIndex) buildSubtreeBytes() []uint64 {
	if vol == nil || vol.index == nil || len(vol.subtreeOrder) == 0 {
		return nil
	}
	recordCount := vol.index.compactRecordCount()
	bytes := make([]uint64, recordCount)
	for pos := len(vol.subtreeOrder) - 1; pos >= 0; pos-- {
		id := int(vol.subtreeOrder[pos])
		if id < 0 || id >= recordCount {
			continue
		}
		rec := vol.index.compactRecord(id)
		var sum uint64
		if !rec.Deleted && rec.Mode&uint32(os.ModeDir) == 0 && rec.Size > 0 {
			sum = uint64(rec.Size)
		}
		for _, childID32 := range vol.childIDsForRecord(id) {
			childID := int(childID32)
			if childID >= 0 && childID < recordCount {
				sum += bytes[childID]
			}
		}
		bytes[id] = sum
	}
	return bytes
}

func loadIndex(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open index %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat index %s: %w", path, err)
	}
	idx, err := readIndexWithReaderAt(bufio.NewReaderSize(f, 16*1024*1024), f, info.Size())
	if err != nil {
		return nil, fmt.Errorf("read index %s: %w", path, err)
	}
	return idx, nil
}
