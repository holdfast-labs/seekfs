package main

// Bounded external-merge builder for a content posting section.
//
// encodeContentPostingSection needs the whole key -> postings map resident at
// once. This builder produces byte-identical output while only ever holding an
// arena of pending records plus the merge state: Add buffers records until the
// estimated byte count exceeds arenaBytes, then sorts the buffer by (key, docID)
// and spills it to a temporary run file. Finish spills the tail and k-way merges
// the runs back in (key, docID) order, flushing 1024-posting blocks as it goes.
//
// Memory ceiling: peak transient memory is arenaBytes + the merge heap (one
// record per run) + the output being assembled. The returned section is still
// O(index) in memory because contentIndex holds sections as []byte; this builder
// removes the raw posting map transient, not the encoded output.
//
// Callers must add each (key, docID) pair at most once and add postings for a key
// in ascending docID order (the same contract encodeContentPostingSection has).
// Empty keys and keys longer than 65535 bytes are ignored, matching encode.

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

const contentExternalRecordOverhead = 10 // keyLen(2)+docID(4)+tf(4)

// contentExternalRunBufSize bounds each open run's read buffer. The merge holds
// one buffer per run, so keeping it small keeps the heap near the arena size.
const contentExternalRunBufSize = 2048

type contentExternalRecord struct {
	key   string
	docID uint32
	tf    uint32
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

func (b *contentExternalSectionBuilder) Add(key string, docID uint32, tf uint32) {
	if b.err != nil || b.closed {
		return
	}
	if key == "" || len(key) > int(^uint16(0)) {
		return
	}
	b.buf = append(b.buf, contentExternalRecord{key: key, docID: docID, tf: tf})
	b.bufBytes += len(key) + contentExternalRecordOverhead
	b.records++
	if b.bufBytes > b.arenaBytes {
		if err := b.spill(); err != nil {
			b.err = err
		}
	}
}

// spill sorts the pending buffer and writes it to a fresh run file.
func (b *contentExternalSectionBuilder) spill() error {
	if len(b.buf) == 0 {
		return nil
	}
	sort.Slice(b.buf, func(i, j int) bool {
		if b.buf[i].key != b.buf[j].key {
			return b.buf[i].key < b.buf[j].key
		}
		return b.buf[i].docID < b.buf[j].docID
	})
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
		n = binary.PutUvarint(scratch[:], uint64(r.tf))
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
	tf    uint32
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
	tf, err := binary.ReadUvarint(r.br)
	if err != nil {
		return err
	}
	r.key = key
	r.docID = uint32(docID)
	r.tf = uint32(tf)
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
	readers := make([]*contentRunReader, 0, len(b.runs))
	for _, path := range b.runs {
		r, err := newContentRunReader(path)
		if err != nil {
			for _, open := range readers {
				open.f.Close()
			}
			return nil, err
		}
		if r.done {
			r.f.Close()
			continue
		}
		readers = append(readers, r)
	}
	if len(readers) == 0 {
		return nil, nil
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

	asm := newContentExternalAssembler(b.records)
	for h.Len() > 0 {
		r := h[0]
		if !asm.hasKey || !bytes.Equal(asm.curKey, r.key) {
			asm.endKey()
			asm.startKey(r.key)
		}
		asm.addPosting(r.docID, r.tf)

		if err := r.advance(); err != nil {
			return nil, err
		}
		if r.done {
			heap.Pop(&h)
			r.f.Close()
		} else {
			heap.Fix(&h, 0)
		}
	}
	asm.endKey()
	return asm.bytes(), nil
}

// contentExternalAssembler builds the output tables in the same order as
// encodeContentPostingSection: keys ascending, each key's postings chunked into
// 1024-doc blocks.
type contentExternalAssembler struct {
	keyBlob   bytes.Buffer
	blockBlob bytes.Buffer
	entries   []contentPostingEntry
	blocks    []contentBlockMeta

	curKey   []byte
	hasKey   bool
	entry    contentPostingEntry
	inBlock  bool
	firstDoc uint32
	count    uint32
	offset   uint64
	maxTF    uint32
	prevDoc  int64
	scratch  [binary.MaxVarintLen64]byte
}

// newContentExternalAssembler seeds blockBlob from the total record count so it
// avoids most of the doubling growth at the end of the merge. Two bytes per
// posting covers the common small-delta and small-tf case; a key with huge gaps
// just makes blockBlob grow once more, still bounded by the output. keyBlob is
// left to grow on its own (distinct keys are usually far smaller than records).
func newContentExternalAssembler(records int) contentExternalAssembler {
	var a contentExternalAssembler
	if records > 0 {
		a.blockBlob.Grow(records*2 + 1024)
	}
	a.entries = make([]contentPostingEntry, 0, 64)
	a.blocks = make([]contentBlockMeta, 0, 64)
	return a
}

func (a *contentExternalAssembler) startKey(key []byte) {
	a.curKey = append(a.curKey[:0], key...)
	a.hasKey = true
	a.entry = contentPostingEntry{
		keyOff:     uint32(a.keyBlob.Len()),
		keyLen:     uint16(len(key)),
		firstBlock: uint32(len(a.blocks)),
	}
	a.keyBlob.Write(key)
}

func (a *contentExternalAssembler) addPosting(docID, tf uint32) {
	if !a.inBlock {
		a.inBlock = true
		a.firstDoc = docID
		a.count = 0
		a.offset = uint64(a.blockBlob.Len())
		a.maxTF = 0
		a.prevDoc = int64(docID) - 1
	}
	n := binary.PutUvarint(a.scratch[:], uint64(int64(docID)-a.prevDoc-1))
	a.blockBlob.Write(a.scratch[:n])
	n = binary.PutUvarint(a.scratch[:], uint64(tf))
	a.blockBlob.Write(a.scratch[:n])
	a.prevDoc = int64(docID)
	a.count++
	if tf > a.maxTF {
		a.maxTF = tf
	}
	a.entry.count++
	if a.count >= contentPostingBlockSize {
		a.flushBlock()
	}
}

func (a *contentExternalAssembler) flushBlock() {
	if !a.inBlock {
		return
	}
	a.blocks = append(a.blocks, contentBlockMeta{
		firstDocID: a.firstDoc,
		count:      a.count,
		byteOffset: a.offset,
		byteLen:    uint32(uint64(a.blockBlob.Len()) - a.offset),
		maxTF:      a.maxTF,
	})
	a.inBlock = false
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
	total := 16 + len(a.entries)*contentPostingEntrySize + len(a.blocks)*contentBlockMetaSize + a.keyBlob.Len() + a.blockBlob.Len()
	out := make([]byte, 0, total)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(a.entries)))
	out = binary.LittleEndian.AppendUint32(out, uint32(a.keyBlob.Len()))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(a.blocks)))
	out = binary.LittleEndian.AppendUint32(out, uint32(a.blockBlob.Len()))
	for _, e := range a.entries {
		out = binary.LittleEndian.AppendUint32(out, e.keyOff)
		out = binary.LittleEndian.AppendUint16(out, e.keyLen)
		out = binary.LittleEndian.AppendUint16(out, 0)
		out = binary.LittleEndian.AppendUint32(out, e.count)
		out = binary.LittleEndian.AppendUint32(out, e.firstBlock)
		out = binary.LittleEndian.AppendUint32(out, e.blockCount)
	}
	for _, b := range a.blocks {
		out = binary.LittleEndian.AppendUint32(out, b.firstDocID)
		out = binary.LittleEndian.AppendUint32(out, b.count)
		out = binary.LittleEndian.AppendUint64(out, b.byteOffset)
		out = binary.LittleEndian.AppendUint32(out, b.byteLen)
		out = binary.LittleEndian.AppendUint32(out, b.maxTF)
	}
	out = append(out, a.keyBlob.Bytes()...)
	out = append(out, a.blockBlob.Bytes()...)
	return out
}
