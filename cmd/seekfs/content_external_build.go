package main

// Bounded external-merge builder for the content gram (CXGR) section.
//
// encodeContentPostingSection needs the whole key -> postings map resident at
// once. This builder produces byte-identical output while only ever holding an
// arena of pending records plus the merge state: Add buffers records until the
// estimated byte count exceeds arenaBytes, then sorts the buffer by (key, docID)
// and spills it to a temporary run file. Finish spills the tail and k-way merges
// the runs back in (key, docID) order, flushing contentPostingBlockSize
// blocks as it goes.
//
// Memory ceiling: peak transient memory is arenaBytes + the merge heap (one
// record per run) + the output being assembled. The returned section is still
// O(index) in memory because contentIndex holds sections as []byte; this builder
// removes the raw posting map transient, not the encoded output.
//
// Callers must add each (key, docID) pair at most once and add postings for a key
// in ascending docID order (the same contract encodeContentPostingSection has).
// Keys must be 3-byte grams; anything else is ignored, matching encode.

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sort"
)

// contentSectionFileBufSize is the buffered-I/O size for spilling a finished
// posting section to disk and for streaming it back into a `.gsx`.
const contentSectionFileBufSize = 1 << 20

const contentExternalRecordOverhead = 6 // keyLen(2)+docID(4)

// contentExternalRunBufSize bounds each open run's read buffer. The merge holds
// one buffer per run, so keeping it small keeps the heap near the arena size.
const contentExternalRunBufSize = 2048

type contentExternalRecord struct {
	key   string
	docID uint32
}

type contentExternalSectionBuilder struct {
	tmpDir     string
	arenaBytes int
	buf        []contentExternalRecord
	bufBytes   int
	runs       []string
	records    int
	err        error
	closed     bool
	// scratch is the radix-partition workspace, reused across spills so a
	// large spill does not allocate a second buffer every time.
	scratch []contentExternalRecord
}

func newContentExternalSectionBuilder(tmpDir string, arenaBytes int) (*contentExternalSectionBuilder, error) {
	if arenaBytes <= 0 {
		arenaBytes = 1 << 20
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	return &contentExternalSectionBuilder{tmpDir: tmpDir, arenaBytes: arenaBytes}, nil
}

func (b *contentExternalSectionBuilder) Add(key string, docID uint32) {
	if b.err != nil || b.closed {
		return
	}
	if len(key) != 3 {
		return
	}
	b.buf = append(b.buf, contentExternalRecord{key: key, docID: docID})
	b.bufBytes += len(key) + contentExternalRecordOverhead
	b.records++
	if b.bufBytes > b.arenaBytes {
		if err := b.spill(); err != nil {
			b.err = err
		}
	}
}

// contentExternalRecordSlice sorts records by (key, docID) without
// reflection (sort.Slice's func adapter costs ~2x here on millions of
// records).
type contentExternalRecordSlice []contentExternalRecord

func (s contentExternalRecordSlice) Len() int { return len(s) }
func (s contentExternalRecordSlice) Less(i, j int) bool {
	if s[i].key != s[j].key {
		return s[i].key < s[j].key
	}
	return s[i].docID < s[j].docID
}
func (s contentExternalRecordSlice) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// contentExternalSortWorkers sizes the spill-run parallel sort. Small buffers
// sort faster serially; the assembler reuses the adaptive build count so an
// active user or a battery still gets the serial sort.
func contentExternalSortWorkers(n int) int {
	if n < 1<<18 {
		return 1
	}
	w := contentBuildWorkerCount()
	if w < 1 {
		w = 1
	}
	if w > n/(1<<16) {
		w = n / (1 << 16)
		if w < 1 {
			w = 1
		}
	}
	return w
}

// spill sorts the pending buffer and writes it to a fresh run file.
func (b *contentExternalSectionBuilder) spill() error {
	if len(b.buf) == 0 {
		return nil
	}
	if contentExternalSortWorkers(len(b.buf)) < 2 {
		sort.Sort(contentExternalRecordSlice(b.buf))
		return b.spillSorted()
	}
	return b.spillRadix()
}

// spillRadix sorts a large pending buffer with one MSD radix step on the
// first key byte plus a counting sort on the 2-byte suffix: records are
// stable-partitioned into 256 first-byte groups, each group is
// counting-sorted with no comparisons at all, and groups stream into the run
// file in byte order. First-byte groups are key-disjoint ranges, so
// concatenation is exactly the fully-sorted order — no cross-group merge.
// Stability preserves the input's ascending-docID order within equal keys
// (documents arrive in docID order and contribute each key once), so the
// (key, docID) contract holds without an explicit tiebreak. All keys are
// 3-byte grams (the builder's contract since v7), so there is no general
// fallback: a non-3-byte key would already have been rejected by Add.
func (b *contentExternalSectionBuilder) spillRadix() error {
	buf := b.buf
	if cap(b.scratch) < len(buf) {
		b.scratch = make([]contentExternalRecord, len(buf))
	}
	scratch := b.scratch[:len(buf)]
	var counts [256]int
	for i := range buf {
		counts[buf[i].key[0]]++
	}
	// Stable partition buf -> scratch by first key byte.
	var off [256]int
	for c := 1; c < 256; c++ {
		off[c] = off[c-1] + counts[c-1]
	}
	pos := off
	for i := range buf {
		c := buf[i].key[0]
		scratch[pos[c]] = buf[i]
		pos[c]++
	}
	rw, err := createRunFileWriter(b.tmpDir)
	if err != nil {
		return err
	}
	// Counting path: per-group counting sort on the 2-byte suffix, stable
	// so equal keys keep ascending docIDs. Groups stream into the run file
	// in first-byte order.
	var c2 [65536]int
	var start [65536]int
	for c := 0; c < 256; c++ {
		lo := off[c]
		g := scratch[lo : lo+counts[c]]
		if len(g) == 0 {
			continue
		}
		for i := range g {
			k := g[i].key
			c2[int(k[1])<<8|int(k[2])]++
		}
		s := 0
		for v := 0; v < 65536; v++ {
			start[v] = s
			s += c2[v]
		}
		dst := buf[lo : lo+len(g)]
		for i := range g {
			k := g[i].key
			v := int(k[1])<<8 | int(k[2])
			dst[start[v]] = g[i]
			start[v]++
		}
		for i := range g {
			k := g[i].key
			c2[int(k[1])<<8|int(k[2])] = 0
		}
		if err := rw.writeRecords(dst); err != nil {
			return rw.abort(err)
		}
	}
	path, err := rw.commit()
	if err != nil {
		return err
	}
	b.runs = append(b.runs, path)
	b.buf = b.buf[:0]
	b.bufBytes = 0
	return nil
}

// runFileWriter streams sorted records into one run file with the exact
// historical encoding (uvarint keyLen, key bytes, uvarint docID).
type runFileWriter struct {
	f       *os.File
	bw      *bufio.Writer
	path    string
	scratch [binary.MaxVarintLen64]byte
}

func createRunFileWriter(tmpDir string) (*runFileWriter, error) {
	f, err := os.CreateTemp(tmpDir, "cxext-*.run")
	if err != nil {
		return nil, err
	}
	return &runFileWriter{f: f, bw: bufio.NewWriterSize(f, 1<<16), path: f.Name()}, nil
}

func (w *runFileWriter) writeOne(r *contentExternalRecord) error {
	n := binary.PutUvarint(w.scratch[:], uint64(len(r.key)))
	if _, err := w.bw.Write(w.scratch[:n]); err != nil {
		return err
	}
	if _, err := w.bw.WriteString(r.key); err != nil {
		return err
	}
	n = binary.PutUvarint(w.scratch[:], uint64(r.docID))
	if _, err := w.bw.Write(w.scratch[:n]); err != nil {
		return err
	}
	return nil
}

func (w *runFileWriter) writeRecords(recs []contentExternalRecord) error {
	for i := range recs {
		if err := w.writeOne(&recs[i]); err != nil {
			return err
		}
	}
	return nil
}

// commit flushes, closes and keeps the run file, returning its path.
func (w *runFileWriter) commit() (string, error) {
	if err := w.bw.Flush(); err != nil {
		_ = w.f.Close()
		_ = os.Remove(w.path)
		return "", err
	}
	if err := w.f.Close(); err != nil {
		_ = os.Remove(w.path)
		return "", err
	}
	return w.path, nil
}

// abort discards the partial run file.
func (w *runFileWriter) abort(err error) error {
	_ = w.bw.Flush()
	_ = w.f.Close()
	_ = os.Remove(w.path)
	return err
}

// spillSorted writes the (already sorted) pending buffer to a fresh run file.
func (b *contentExternalSectionBuilder) spillSorted() error {
	f, err := os.CreateTemp(b.tmpDir, "cxext-*.run")
	if err != nil {
		return err
	}
	path := f.Name()
	bw := bufio.NewWriterSize(f, 1<<16)
	var scratch [binary.MaxVarintLen64]byte
	for i := range b.buf {
		r := &b.buf[i]
		n := binary.PutUvarint(scratch[:], uint64(len(r.key)))
		if _, err := bw.Write(scratch[:n]); err != nil {
			bw.Flush()
			f.Close()
			os.Remove(path)
			return err
		}
		if _, err := bw.WriteString(r.key); err != nil {
			bw.Flush()
			f.Close()
			os.Remove(path)
			return err
		}
		n = binary.PutUvarint(scratch[:], uint64(r.docID))
		if _, err := bw.Write(scratch[:n]); err != nil {
			bw.Flush()
			f.Close()
			os.Remove(path)
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	b.runs = append(b.runs, path)
	b.buf = b.buf[:0]
	b.bufBytes = 0
	return nil
}

func (b *contentExternalSectionBuilder) Finish() ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	if b.closed {
		return nil, errors.New("content external builder closed")
	}
	if err := b.spill(); err != nil {
		return nil, err
	}
	if len(b.runs) == 0 {
		return nil, nil
	}
	out, err := b.merge()
	if err != nil {
		return nil, err
	}
	b.removeRuns()
	return out, nil
}

func (b *contentExternalSectionBuilder) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	b.buf = nil
	b.scratch = nil
	return b.removeRuns()
}

// removeRuns deletes every spilled run file, keeping any that could not be
// removed so a later Close can retry. Safe to call more than once.
func (b *contentExternalSectionBuilder) removeRuns() error {
	var firstErr error
	kept := b.runs[:0]
	for _, path := range b.runs {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
			kept = append(kept, path)
		}
	}
	b.runs = kept
	return firstErr
}

// contentRunReader streams records from one spilled run, one at a time.
type contentRunReader struct {
	f     *os.File
	br    *bufio.Reader
	key   []byte
	docID uint32
	done  bool
}

func newContentRunReader(path string) (*contentRunReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &contentRunReader{f: f, br: bufio.NewReaderSize(f, contentExternalRunBufSize)}
	if err := r.advance(); err != nil {
		f.Close()
		return nil, err
	}
	return r, nil
}

// advance loads the next record. io.EOF at a record boundary means the run is
// exhausted; any other read error is returned.
func (r *contentRunReader) advance() error {
	keyLen, err := binary.ReadUvarint(r.br)
	if err == io.EOF {
		r.done = true
		return nil
	}
	if err != nil {
		return err
	}
	key := r.key[:0]
	if cap(key) < int(keyLen) {
		key = make([]byte, keyLen)
	} else {
		key = key[:keyLen]
	}
	if _, err := io.ReadFull(r.br, key); err != nil {
		return err
	}
	docID, err := binary.ReadUvarint(r.br)
	if err != nil {
		return err
	}
	r.key = key
	r.docID = uint32(docID)
	return nil
}

type contentMergeHeap []*contentRunReader

func (h contentMergeHeap) Len() int { return len(h) }
func (h contentMergeHeap) Less(i, j int) bool {
	if c := bytes.Compare(h[i].key, h[j].key); c != 0 {
		return c < 0
	}
	return h[i].docID < h[j].docID
}
func (h contentMergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *contentMergeHeap) Push(x any)   { *h = append(*h, x.(*contentRunReader)) }
func (h *contentMergeHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return x
}

// merge k-way merges the runs and assembles the section. Run files are left in
// place so a failed merge can still be cleaned up by Close.
func (b *contentExternalSectionBuilder) merge() ([]byte, error) {
	asm, cleanup, err := b.mergeInto(newContentMemoryBlobSink())
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return nil, err
	}
	if asm == nil {
		return nil, nil
	}
	return asm.bytes(), nil
}

// FinishFile spills the tail and k-way merges the runs straight into the
// section file at path, without ever assembling the section in memory. The
// file holds byte-identical bytes to what Finish would return. When the
// builder saw no records, no file is created and nil is returned, mirroring
// Finish's nil section. Run files are removed on success; a failure leaves
// them for Close.
func (b *contentExternalSectionBuilder) FinishFile(path string) error {
	if b.err != nil {
		return b.err
	}
	if b.closed {
		return errors.New("content external builder closed")
	}
	if err := b.spill(); err != nil {
		return err
	}
	if len(b.runs) == 0 {
		_ = os.Remove(path)
		return nil
	}
	blocks, err := newContentFileBlobSink(b.tmpDir)
	if err != nil {
		return err
	}
	asm, cleanup, err := b.mergeInto(blocks)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return err
	}
	if asm == nil {
		return nil
	}
	if err := asm.writeFile(path); err != nil {
		return err
	}
	b.removeRuns()
	return nil
}

// mergeInto k-way merges the runs into an assembler whose block blob streams
// to the given sink. It returns (nil, nil, nil) when every run is empty.
// Callers must run cleanup (which releases the sink's temp resources) and
// must not use the assembler afterwards.
func (b *contentExternalSectionBuilder) mergeInto(blocks contentBlobSink) (*contentExternalAssembler, func(), error) {
	cleanup := func() {
		blocks.abort()
	}
	readers := make([]*contentRunReader, 0, len(b.runs))
	for _, path := range b.runs {
		r, err := newContentRunReader(path)
		if err != nil {
			for _, open := range readers {
				open.f.Close()
			}
			return nil, cleanup, err
		}
		if r.done {
			r.f.Close()
			continue
		}
		readers = append(readers, r)
	}
	if len(readers) == 0 {
		return nil, cleanup, nil
	}
	h := contentMergeHeap(readers)
	heap.Init(&h)
	defer func() {
		for _, r := range h {
			if r != nil {
				r.f.Close()
			}
		}
	}()

	asm := newContentExternalAssembler(b.records, blocks)
	// The assembler owns the sink from here; success or merge error both run
	// the sink's abort through cleanup, which is a no-op after finalize.
	for h.Len() > 0 {
		r := h[0]
		if !asm.hasKey || !bytes.Equal(asm.curKey[:], r.key) {
			asm.endKey()
			asm.startKey(r.key)
		}
		asm.addPosting(r.docID)

		if err := r.advance(); err != nil {
			return nil, cleanup, err
		}
		if r.done {
			heap.Pop(&h)
			r.f.Close()
		} else {
			heap.Fix(&h, 0)
		}
	}
	asm.endKey()
	return asm, cleanup, nil
}

// contentBlobSink streams one posting blob (the block blob) during the merge.
// The in-memory sink preserves the historical merge behavior for Finish; the
// file sink lets FinishFile merge multi-gigabyte sections with O(metadata)
// heap. Lengths are exact byte counts; copyTo replays the bytes in order.
type contentBlobSink interface {
	io.Writer
	Len() int64
	copyTo(w io.Writer) error
	abort()
}

// contentMemoryBlobSink is the in-memory blob sink.
type contentMemoryBlobSink struct {
	buf bytes.Buffer
}

func newContentMemoryBlobSink() *contentMemoryBlobSink { return &contentMemoryBlobSink{} }

func (s *contentMemoryBlobSink) Write(p []byte) (int, error) { return s.buf.Write(p) }
func (s *contentMemoryBlobSink) Len() int64                  { return int64(s.buf.Len()) }
func (s *contentMemoryBlobSink) copyTo(w io.Writer) error {
	_, err := w.Write(s.buf.Bytes())
	return err
}
func (s *contentMemoryBlobSink) abort() {}

// contentFileBlobSink spills one posting blob to a temp file.
type contentFileBlobSink struct {
	f    *os.File
	bw   *bufio.Writer
	path string
	n    int64
}

func newContentFileBlobSink(tmpDir string) (*contentFileBlobSink, error) {
	f, err := os.CreateTemp(tmpDir, "cxsec-*.blob")
	if err != nil {
		return nil, err
	}
	return &contentFileBlobSink{f: f, bw: bufio.NewWriterSize(f, contentSectionFileBufSize), path: f.Name()}, nil
}

func (s *contentFileBlobSink) Write(p []byte) (int, error) {
	n, err := s.bw.Write(p)
	s.n += int64(n)
	return n, err
}

func (s *contentFileBlobSink) Len() int64 { return s.n }

func (s *contentFileBlobSink) copyTo(w io.Writer) error {
	if err := s.bw.Flush(); err != nil {
		return err
	}
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := io.CopyBuffer(w, s.f, make([]byte, contentSectionFileBufSize))
	return err
}

// abort releases the sink's temp file. After a successful writeFile the blob
// file is already removed and abort is a no-op.
func (s *contentFileBlobSink) abort() {
	if s.bw != nil {
		_ = s.bw.Flush()
		s.bw = nil
	}
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	if s.path != "" {
		_ = os.Remove(s.path)
		s.path = ""
	}
}

// contentExternalAssembler builds the output tables in the same order as
// encodeContentPostingSection: keys ascending, each key's postings chunked into
// contentPostingBlockSize blocks. Gram keys are fixed 3-byte strings stored inline in the
// entry; the block blob streams to the sink (memory or temp file) while the
// entry/block metadata stays resident; finalize (bytes or writeFile) emits
// header + entries + blocks + block blob in section order.
type contentExternalAssembler struct {
	blockBlob contentBlobSink
	entries   []contentPostingEntry
	blocks    []contentBlockMeta

	curKey   [3]byte
	hasKey   bool
	entry    contentPostingEntry
	block    []uint32 // current block's docIDs, buffered for the list/bitmap choice
	firstDoc uint32
}

// newContentExternalAssembler seeds an in-memory blockBlob from the total
// record count so it avoids most of the doubling growth at the end of the
// merge. One byte per posting covers the common small-delta case; a key with
// huge gaps just makes blockBlob grow once more, still bounded by the output.
// File sinks ignore the seed.
func newContentExternalAssembler(records int, blocks contentBlobSink) *contentExternalAssembler {
	a := &contentExternalAssembler{blockBlob: blocks}
	if mem, ok := blocks.(*contentMemoryBlobSink); ok && records > 0 {
		mem.buf.Grow(records + 1024)
	}
	a.entries = make([]contentPostingEntry, 0, 64)
	a.blocks = make([]contentBlockMeta, 0, 64)
	return a
}

func (a *contentExternalAssembler) startKey(key []byte) {
	copy(a.curKey[:], key)
	a.hasKey = true
	a.entry = contentPostingEntry{
		firstBlock: uint32(len(a.blocks)),
	}
	a.entry.key = a.curKey
}

func (a *contentExternalAssembler) addPosting(docID uint32) {
	if len(a.block) == 0 {
		a.firstDoc = docID
	}
	a.block = append(a.block, docID)
	a.entry.count++
	if len(a.block) >= contentPostingBlockSize {
		a.flushBlock()
	}
}

func (a *contentExternalAssembler) flushBlock() {
	if len(a.block) == 0 {
		return
	}
	enc := encodeContentPostingBlock(a.block, a.firstDoc)
	off := uint32(a.blockBlob.Len())
	a.blockBlob.Write(enc)
	a.blocks = append(a.blocks, contentBlockMeta{
		firstDocID: a.firstDoc,
		count:      uint32(len(a.block)),
		byteOffset: off,
		byteLen:    uint32(uint64(a.blockBlob.Len()) - uint64(off)),
	})
	a.block = a.block[:0]
}

func (a *contentExternalAssembler) endKey() {
	if !a.hasKey {
		return
	}
	a.flushBlock()
	a.entry.blockCount = uint32(len(a.blocks)) - a.entry.firstBlock
	a.entries = append(a.entries, a.entry)
	a.hasKey = false
}

func (a *contentExternalAssembler) bytes() []byte {
	if len(a.entries) == 0 {
		return nil
	}
	total := 16 + len(a.entries)*contentPostingEntrySize + len(a.blocks)*contentBlockMetaSize + int(a.blockBlob.Len())
	out := make([]byte, 0, total)
	out = a.appendTables(out)
	if err := a.blockBlob.copyTo(sliceWriter{&out}); err != nil {
		return nil
	}
	return out
}

// appendTables emits the section header plus the entry and block tables: the
// byte prefix shared by the in-memory and file final forms. keyBlobLen is 0:
// v7 gram keys sit inline in the entries.
func (a *contentExternalAssembler) appendTables(out []byte) []byte {
	out = binary.LittleEndian.AppendUint32(out, uint32(len(a.entries)))
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(a.blocks)))
	out = binary.LittleEndian.AppendUint32(out, uint32(a.blockBlob.Len()))
	for _, e := range a.entries {
		out = append(out, e.key[0], e.key[1], e.key[2], 0)
		out = binary.LittleEndian.AppendUint32(out, e.count)
		out = binary.LittleEndian.AppendUint32(out, e.firstBlock)
		out = binary.LittleEndian.AppendUint32(out, e.blockCount)
	}
	for _, b := range a.blocks {
		out = binary.LittleEndian.AppendUint32(out, b.firstDocID)
		out = binary.LittleEndian.AppendUint32(out, b.count)
		out = binary.LittleEndian.AppendUint32(out, b.byteOffset)
		out = binary.LittleEndian.AppendUint32(out, b.byteLen)
	}
	return out
}

// writeFile finalizes the section into the file at path: header + tables,
// then the streamed block blob. The bytes are identical to bytes().
// The blob sink's temp file is removed.
func (a *contentExternalAssembler) writeFile(path string) error {
	if len(a.entries) == 0 {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, contentSectionFileBufSize)
	_, err = bw.Write(a.appendTables(nil))
	if err == nil {
		err = a.blockBlob.copyTo(bw)
	}
	if err == nil {
		err = bw.Flush()
	}
	cerr := f.Close()
	a.blockBlob.abort()
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	if cerr != nil {
		_ = os.Remove(path)
		return cerr
	}
	return nil
}

// sliceWriter appends to a byte slice through the io.Writer interface.
type sliceWriter struct {
	out *[]byte
}

func (w sliceWriter) Write(p []byte) (int, error) {
	*w.out = append(*w.out, p...)
	return len(p), nil
}
