package main

// Bounded external builder for the dictionary-only terms (CXTR v2) section.
//
// The assembler adds each document's distinct terms once, so a key's document
// frequency is exactly its Add count. This builder buffers keys until the
// arena fills, sorts and spills key-only runs to disk, then k-way merges the
// runs counting equal keys into (term -> df) and encodes the front-coded
// dictionary. Peak heap is the arena plus the merge heap (one key per run)
// plus the encoded dictionary's key list; the docID lists that made v6 CXTR
// 4x larger than its dictionary are never stored.

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sort"
	"time"
)

// contentDictRecordOverhead estimates a buffered key's resident cost.
const contentDictRecordOverhead = 16 // string header

type contentExternalDictBuilder struct {
	tmpDir     string
	arenaBytes int
	buf        []string
	bufBytes   int
	runs       []string
	keys       int
	err        error
	closed     bool
}

func newContentExternalDictBuilder(tmpDir string, arenaBytes int) (*contentExternalDictBuilder, error) {
	if arenaBytes <= 0 {
		arenaBytes = 1 << 20
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	return &contentExternalDictBuilder{tmpDir: tmpDir, arenaBytes: arenaBytes}, nil
}

// Add records one document's use of key. Empty and over-long keys are
// ignored, matching the posting builder's contract.
func (b *contentExternalDictBuilder) Add(key string) {
	if b.err != nil || b.closed {
		return
	}
	if key == "" || len(key) > int(^uint16(0)) {
		return
	}
	b.buf = append(b.buf, key)
	b.bufBytes += len(key) + contentDictRecordOverhead
	b.keys++
	if b.bufBytes > b.arenaBytes {
		if err := b.spill(); err != nil {
			b.err = err
		}
	}
}

// spill sorts the pending keys and writes them to a fresh run file as
// uvarint-len + bytes records.
func (b *contentExternalDictBuilder) spill() error {
	if len(b.buf) == 0 {
		return nil
	}
	t0 := time.Now()
	defer func() {
		if pc := contentAssemblePhaseCapture; pc != nil {
			contentPhaseAdd(&pc.spillSort, time.Since(t0))
			contentPhaseAdd(&pc.spills, time.Duration(1))
		}
	}()
	sort.Strings(b.buf)
	f, err := os.CreateTemp(b.tmpDir, "cxdict-*.run")
	if err != nil {
		return err
	}
	path := f.Name()
	bw := bufio.NewWriterSize(f, 1<<16)
	var scratch [binary.MaxVarintLen64]byte
	for _, k := range b.buf {
		n := binary.PutUvarint(scratch[:], uint64(len(k)))
		if _, err := bw.Write(scratch[:n]); err != nil {
			return abortTempFile(f, bw, path, err)
		}
		if _, err := bw.WriteString(k); err != nil {
			return abortTempFile(f, bw, path, err)
		}
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	b.runs = append(b.runs, path)
	b.buf = b.buf[:0]
	b.bufBytes = 0
	return nil
}

func abortTempFile(f *os.File, bw *bufio.Writer, path string, err error) error {
	_ = bw.Flush()
	_ = f.Close()
	_ = os.Remove(path)
	return err
}

func (b *contentExternalDictBuilder) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	b.buf = nil
	return b.removeRuns()
}

func (b *contentExternalDictBuilder) removeRuns() error {
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

// contentDictRunReader streams sorted keys from one run, one at a time.
type contentDictRunReader struct {
	f    *os.File
	br   *bufio.Reader
	key  []byte
	done bool
}

func newContentDictRunReader(path string) (*contentDictRunReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &contentDictRunReader{f: f, br: bufio.NewReaderSize(f, contentExternalRunBufSize)}
	if err := r.advance(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

func (r *contentDictRunReader) advance() error {
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
	r.key = key
	return nil
}

type contentDictMergeHeap []*contentDictRunReader

func (h contentDictMergeHeap) Len() int           { return len(h) }
func (h contentDictMergeHeap) Less(i, j int) bool { return bytes.Compare(h[i].key, h[j].key) < 0 }
func (h contentDictMergeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *contentDictMergeHeap) Push(x any)        { *h = append(*h, x.(*contentDictRunReader)) }
func (h *contentDictMergeHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return x
}

// FinishFile spills the tail and merges every key (buffered and spilled)
// into the front-coded dictionary file at path. Keys arrive from the merge
// in ascending order with duplicates adjacent, so df counting is a single
// linear pass holding one key. When the builder saw no keys, no file is
// created, mirroring the posting builder's nil section.
func (b *contentExternalDictBuilder) FinishFile(path string) error {
	if b.err != nil {
		return b.err
	}
	if b.closed {
		return errors.New("content external dict builder closed")
	}
	if err := b.spill(); err != nil {
		return err
	}
	if b.keys == 0 {
		_ = os.Remove(path)
		return nil
	}
	tMerge := time.Now()
	sort.Strings(b.buf)
	readers := make([]*contentDictRunReader, 0, len(b.runs))
	for _, rp := range b.runs {
		r, err := newContentDictRunReader(rp)
		if err != nil {
			for _, open := range readers {
				_ = open.f.Close()
			}
			return err
		}
		if r.done {
			_ = r.f.Close()
			continue
		}
		readers = append(readers, r)
	}
	defer func() {
		for _, r := range readers {
			if r != nil {
				_ = r.f.Close()
			}
		}
	}()

	// Merge the sorted buffer and the run heap into one ascending stream.
	// The buffer is consumed by index; runs advance through the heap. The
	// heap gets its own slice: heap.Pop nils popped slots in the backing
	// array, which must not clobber readers for the deferred close above.
	h := append(contentDictMergeHeap(nil), readers...)
	heap.Init(&h)
	bi := 0
	nextBuf := func() ([]byte, bool) {
		if bi >= len(b.buf) {
			return nil, false
		}
		k := b.buf[bi]
		bi++
		return []byte(k), true
	}
	// Peekable two-source merge without materializing: compare buffer head
	// against heap head.
	var bufHead []byte
	bufLive := false
	if k, ok := nextBuf(); ok {
		bufHead, bufLive = k, true
	}
	var sortedKeys []string
	df := make(map[string]uint32, 1024)
	var cur string
	var curCount uint32
	haveCur := false
	flush := func() {
		if haveCur {
			sortedKeys = append(sortedKeys, cur)
			df[cur] = curCount
			haveCur = false
		}
	}
	add := func(key string) {
		if haveCur && key == cur {
			curCount++
			return
		}
		flush()
		cur, curCount, haveCur = key, 1, true
	}
	for bufLive || h.Len() > 0 {
		takeBuf := false
		if bufLive {
			if h.Len() == 0 || bytes.Compare(bufHead, h[0].key) <= 0 {
				takeBuf = true
			}
		}
		if takeBuf {
			add(string(bufHead))
			if k, ok := nextBuf(); ok {
				bufHead = k
			} else {
				bufLive = false
			}
		} else {
			r := h[0]
			add(string(r.key))
			if err := r.advance(); err != nil {
				return err
			}
			if r.done {
				heap.Pop(&h)
				_ = r.f.Close()
			} else {
				heap.Fix(&h, 0)
			}
		}
	}
	flush()

	// Duplicate Adds within one document would inflate df; the assembler adds
	// each term once per doc, so counts are exact document frequencies.
	enc := encodeContentTermDict(sortedKeys, df)
	if pc := contentAssemblePhaseCapture; pc != nil {
		contentPhaseAdd(&pc.mergeTerm, time.Since(tMerge))
	}
	if len(enc) == 0 {
		_ = os.Remove(path)
		b.removeRuns()
		return nil
	}
	tWrite := time.Now()
	err := os.WriteFile(path, enc, 0o600)
	if pc := contentAssemblePhaseCapture; pc != nil {
		contentPhaseAdd(&pc.writeTerm, time.Since(tWrite))
	}
	if err != nil {
		return err
	}
	b.removeRuns()
	return nil
}
