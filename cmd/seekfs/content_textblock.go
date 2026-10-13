package main

// Blocked compressed text store (format v7, CXST v2).
//
// The v6 CXST was a raw concatenation of every document's case-preserving
// text: the single largest section (~1.0x the extracted text). The v7 CXST
// holds the same logical byte stream — the doc table's TextOff/TextLen still
// address it, so folds, reshards, walk volumes and snippets are unchanged —
// but physically stored as independently-decodable zstd blocks of about
// contentTextBlockTarget uncompressed bytes, cut at document boundaries (a
// document larger than the target gets its own block span).
//
// Verification and snippets decompress only the blocks covering the candidate
// docs. Candidates arrive in ascending docID order (ascending logical
// offsets), so sequential reads hit the same block repeatedly; a small LRU
// over decompressed blocks absorbs them. Integrity stays section-level: the
// section table's CRC32C covers the whole CXST payload (table + frames), and
// the doc table (eagerly verified at load) addresses only the logical
// stream, so a corrupt offset fails closed as "no text" rather than a wrong
// slice.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// contentTextBlockTarget is the uncompressed size a text block is cut at. A
// block holds whole documents (~60 average docs at the measured 8.5 KB
// mean), so one 512 KiB decompress (~1 ms) typically serves a run of
// candidates, and sequential scans (fold, regex fallback) share each block
// across its docs through the LRU. Larger blocks compress better; smaller
// ones reduce read amplification for selective queries (one cold block per
// verified doc run). Measured on the 553 MB Chromium sample (ratio at
// Better/BestCompression): 128 KiB .223/.212, 256 KiB .205/.193,
// 512 KiB .199/.186.
const contentTextBlockTarget = 512 << 10

// contentTextBlockCacheEntries bounds the reader's decompressed-block LRU:
// entries x 512 KiB ≈ 2 MiB per loaded shard, inside the per-shard budget.
// Sequential readers (candidates in docID order, fold, scan fallback) need
// only one hot block plus one for cross-block stitching; the rest absorbs
// repeated verification of popular docs.
const contentTextBlockCacheEntries = 4

// contentTextBlockMeta locates one compressed block: its start in the logical
// (decompressed) stream, its span in the frames region, and its raw length.
type contentTextBlockMeta struct {
	logicalStart uint64
	compOff      uint64 // relative to the start of the frames region
	compLen      uint32
	rawLen       uint32
}

// contentTextBlockMetaSize is logicalStart(8)+compOff(8)+compLen(4)+rawLen(4).
const contentTextBlockMetaSize = 24

// contentTextBlockHeaderSize is blockCount(4)+target(4).
const contentTextBlockHeaderSize = 8

// contentZstdDecPool recycles block decoders across reads. A pooled decoder
// is used by exactly one goroutine at a time (checked out, used, returned),
// so no read ever blocks another's decompression; only the LRU insert takes
// the store mutex.
var contentZstdDecPool = sync.Pool{New: func() any {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil
	}
	return dec
}}

// contentTextStore is a decoded CXST v2 section: the block table plus the
// frames they address. frames aliases the mapped section (or a heap copy);
// blocks are decoded on demand into a bounded LRU.
type contentTextStore struct {
	blocks []contentTextBlockMeta
	frames []byte
	mu     sync.Mutex
	lru    []contentCachedBlock
}

// contentCachedBlock is one decompressed block held by the LRU.
type contentCachedBlock struct {
	start uint64
	data  []byte
}

// openContentTextStore decodes a CXST v2 payload. A v6 (raw-concatenation)
// payload fails here, which is correct: the version gate (v7) already
// refused the whole sidecar, so a raw store can never reach this decoder.
func openContentTextStore(sec []byte) (*contentTextStore, error) {
	if len(sec) < contentTextBlockHeaderSize {
		return nil, errors.New("content text store truncated")
	}
	n := int(binary.LittleEndian.Uint32(sec[0:]))
	if n < 0 || n > (1<<30) {
		return nil, errors.New("content text store block count out of range")
	}
	tableLen := n * contentTextBlockMetaSize
	if contentTextBlockHeaderSize+tableLen > len(sec) {
		return nil, errors.New("content text store table truncated")
	}
	st := &contentTextStore{frames: sec[contentTextBlockHeaderSize+tableLen:]}
	if n > 0 {
		st.blocks = make([]contentTextBlockMeta, n)
		off := contentTextBlockHeaderSize
		var prevEnd uint64
		for i := 0; i < n; i++ {
			p := off + i*contentTextBlockMetaSize
			b := contentTextBlockMeta{
				logicalStart: binary.LittleEndian.Uint64(sec[p:]),
				compOff:      binary.LittleEndian.Uint64(sec[p+8:]),
				compLen:      binary.LittleEndian.Uint32(sec[p+16:]),
				rawLen:       binary.LittleEndian.Uint32(sec[p+20:]),
			}
			// The table must describe a partition of the logical stream in
			// order, and frames inside the section; anything else is a
			// corrupt store that fails closed.
			if i > 0 && b.logicalStart < prevEnd {
				return nil, errors.New("content text store blocks out of order")
			}
			if b.compOff+uint64(b.compLen) > uint64(len(st.frames)) {
				return nil, errors.New("content text store frame out of range")
			}
			st.blocks[i] = b
			prevEnd = b.logicalStart + uint64(b.rawLen)
		}
		if !sort.SliceIsSorted(st.blocks, func(i, j int) bool {
			return st.blocks[i].logicalStart < st.blocks[j].logicalStart
		}) {
			return nil, errors.New("content text store blocks out of order")
		}
	}
	return st, nil
}

// logicalSize is the decompressed stream length.
func (st *contentTextStore) logicalSize() uint64 {
	if len(st.blocks) == 0 {
		return 0
	}
	b := st.blocks[len(st.blocks)-1]
	return b.logicalStart + uint64(b.rawLen)
}

// blockFor returns the index of the block containing logical offset off.
func (st *contentTextStore) blockFor(off uint64) int {
	i := sort.Search(len(st.blocks), func(i int) bool { return st.blocks[i].logicalStart > off }) - 1
	return i
}

// decompressed returns the raw bytes of block i, from the LRU when hot. The
// returned slice is owned by the cache and must not be retained or mutated;
// textFor copies out of it.
func (st *contentTextStore) decompressed(i int) ([]byte, error) {
	b := st.blocks[i]
	st.mu.Lock()
	for j := range st.lru {
		if st.lru[j].start == b.logicalStart {
			data := st.lru[j].data
			// Promote to front (move-to-front on a tiny slice).
			copy(st.lru[1:j+1], st.lru[0:j])
			st.lru[0] = contentCachedBlock{start: b.logicalStart, data: data}
			st.mu.Unlock()
			return data, nil
		}
	}
	st.mu.Unlock()

	// Decompression runs without the mutex (a pooled decoder per miss), so
	// two goroutines can decompress the same cold block at once; both
	// inserts are identical and textFor copies out, so the duplicate only
	// costs CPU, never correctness.
	frame := st.frames[b.compOff : b.compOff+uint64(b.compLen)]
	dec, ok := contentZstdDecPool.Get().(*zstd.Decoder)
	if !ok || dec == nil {
		return nil, errors.New("content text store decoder unavailable")
	}
	data, err := dec.DecodeAll(frame, nil)
	contentZstdDecPool.Put(dec)
	if err != nil {
		return nil, fmt.Errorf("content text store block: %w", err)
	}
	if len(data) != int(b.rawLen) {
		return nil, errors.New("content text store block length mismatch")
	}
	st.mu.Lock()
	entry := contentCachedBlock{start: b.logicalStart, data: data}
	if len(st.lru) < contentTextBlockCacheEntries {
		st.lru = append(st.lru, contentCachedBlock{})
	}
	copy(st.lru[1:], st.lru[:len(st.lru)-1])
	st.lru[0] = entry
	st.mu.Unlock()
	return data, nil
}

// textFor returns a fresh copy of length bytes at logical offset off,
// stitching across block boundaries for documents that span them. ok is false
// when the span is out of range or a block fails to decode: callers treat it
// as absent text, never as a match.
func (st *contentTextStore) textFor(off uint64, length uint32) (out []byte, ok bool) {
	if st == nil || uint64(length) > uint64(1<<32) || off+uint64(length) > st.logicalSize() {
		return nil, false
	}
	if length == 0 {
		return []byte{}, true
	}
	out = make([]byte, 0, length)
	for len(out) < int(length) {
		i := st.blockFor(off)
		if i < 0 || i >= len(st.blocks) {
			return nil, false
		}
		data, err := st.decompressed(i)
		if err != nil {
			return nil, false
		}
		in := off - st.blocks[i].logicalStart
		if in >= uint64(len(data)) {
			return nil, false
		}
		n := copy(out[len(out):cap(out)], data[in:])
		// grow out by n without reallocating (cap was set to length).
		out = out[:len(out)+n]
		off += uint64(n)
		if n == 0 {
			return nil, false
		}
	}
	return out, true
}

// contentTextBlockWriter compresses the logical text stream into blocked CXST
// v2 during assembly. Append buffers whole documents and cuts a block at the
// first document boundary at or past the target; FinishFile writes the final
// section file (header + table + frames). Peak heap is one target block plus
// one document plus the encoder workspace.
type contentTextBlockWriter struct {
	enc     *zstd.Encoder
	frames  *os.File
	framesW *bufio.Writer
	blocks  []contentTextBlockMeta
	pending []byte
	// pendingStart/logical track the logical stream: pendingStart is the
	// logical offset of pending[0], logical the end of the stream so far.
	pendingStart uint64
	logical      uint64
	framesLen    uint64
	scratch      []byte
	closed       bool
}

func newContentTextBlockWriter(tmpDir string) (*contentTextBlockWriter, error) {
	// BestCompression costs ~20 s per 553 MB single-threaded (noise next
	// to extraction) for ~1.5 points of ratio over BetterCompression.
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(tmpDir, "cxframes-*.bin")
	if err != nil {
		_ = enc.Close()
		return nil, err
	}
	return &contentTextBlockWriter{
		enc:     enc,
		frames:  f,
		framesW: bufio.NewWriterSize(f, contentSectionFileBufSize),
	}, nil
}

// logicalSize reports the logical stream length so far: the doc table's
// TextOff for the next document.
func (w *contentTextBlockWriter) logicalSize() uint64 { return w.logical }

// Append adds one whole document to the logical stream.
func (w *contentTextBlockWriter) Append(doc []byte) error {
	if w.closed {
		return errors.New("content text block writer closed")
	}
	if len(w.pending) == 0 {
		w.pendingStart = w.logical
	}
	w.pending = append(w.pending, doc...)
	w.logical += uint64(len(doc))
	if uint64(len(w.pending)) >= contentTextBlockTarget {
		return w.flush()
	}
	return nil
}

func (w *contentTextBlockWriter) flush() error {
	if len(w.pending) == 0 {
		return nil
	}
	w.scratch = w.enc.EncodeAll(w.pending, w.scratch[:0])
	n, err := w.framesW.Write(w.scratch)
	if err != nil {
		return err
	}
	w.blocks = append(w.blocks, contentTextBlockMeta{
		logicalStart: w.pendingStart,
		compOff:      w.framesLen,
		compLen:      uint32(n),
		rawLen:       uint32(len(w.pending)),
	})
	w.framesLen += uint64(n)
	w.pending = w.pending[:0]
	return nil
}

// abort releases the writer's temp resources without a final file.
func (w *contentTextBlockWriter) abort() {
	if w.closed {
		return
	}
	w.closed = true
	if w.framesW != nil {
		_ = w.framesW.Flush()
	}
	if w.frames != nil {
		name := w.frames.Name()
		_ = w.frames.Close()
		_ = os.Remove(name)
		w.frames = nil
	}
	_ = w.enc.Close()
}

// FinishFile flushes the tail block and writes the final CXST section file at
// path: header + block table + the spooled frames. The frames spool is
// removed.
func (w *contentTextBlockWriter) FinishFile(path string) error {
	if w.closed {
		return errors.New("content text block writer closed")
	}
	w.closed = true
	defer w.enc.Close()
	if err := w.flush(); err != nil {
		w.abortFiles()
		return err
	}
	if err := w.framesW.Flush(); err != nil {
		w.abortFiles()
		return err
	}
	framesName := w.frames.Name()
	if err := w.frames.Close(); err != nil {
		_ = os.Remove(framesName)
		return err
	}
	w.frames = nil

	f, err := os.Create(path)
	if err != nil {
		_ = os.Remove(framesName)
		return err
	}
	bw := bufio.NewWriterSize(f, contentSectionFileBufSize)
	fail := func(err error) error {
		_ = bw.Flush()
		_ = f.Close()
		_ = os.Remove(path)
		_ = os.Remove(framesName)
		return err
	}
	var hdr [contentTextBlockHeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[0:], uint32(len(w.blocks)))
	binary.LittleEndian.PutUint32(hdr[4:], uint32(contentTextBlockTarget))
	if _, err := bw.Write(hdr[:]); err != nil {
		return fail(err)
	}
	var meta [contentTextBlockMetaSize]byte
	for _, b := range w.blocks {
		binary.LittleEndian.PutUint64(meta[0:], b.logicalStart)
		binary.LittleEndian.PutUint64(meta[8:], b.compOff)
		binary.LittleEndian.PutUint32(meta[16:], b.compLen)
		binary.LittleEndian.PutUint32(meta[20:], b.rawLen)
		if _, err := bw.Write(meta[:]); err != nil {
			return fail(err)
		}
	}
	rf, err := os.Open(framesName)
	if err != nil {
		return fail(err)
	}
	if _, err := io.CopyBuffer(bw, rf, make([]byte, contentSectionFileBufSize)); err != nil {
		_ = rf.Close()
		return fail(err)
	}
	_ = rf.Close()
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		_ = os.Remove(framesName)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		_ = os.Remove(framesName)
		return err
	}
	_ = os.Remove(framesName)
	// An empty corpus still writes the 8-byte header so the section decodes
	// as an empty store instead of a missing one.
	return nil
}

func (w *contentTextBlockWriter) abortFiles() {
	if w.frames != nil {
		name := w.frames.Name()
		_ = w.frames.Close()
		_ = os.Remove(name)
		w.frames = nil
	}
}
