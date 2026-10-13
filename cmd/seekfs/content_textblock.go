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
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
)

// contentTextBlockTarget is the uncompressed size a text block is cut at.
// Blocks hold whole documents (~8 average docs at the measured 8.5 KB
// mean). Small blocks touch ~64 KiB per cold block (~170 µs with a
// dictionary) instead of ~512 KiB (~0.9 ms without), so a scattered
// verify-heavy query's working set itself shrinks ~8x — which is what fits
// a broad compound's blocks back under the shared cache. Measured on the
// 553 MB Chromium sample (Better/BestCompression; dict rows pair Better
// with a 96 KiB trained dict): 64 KiB .203/.212, 128 KiB .199/.202,
// 256 KiB .196/.194, 512 KiB .194/.187. v8 ships 64 KiB with a per-shard
// trained dictionary.
const contentTextBlockTarget = 64 << 10

// contentBlockCacheDefaultMB sizes the shared cache when neither the
// environment nor a platform memory figure says otherwise.
const contentBlockCacheDefaultMB = 64

// contentBlockCacheMaxMB caps the shared cache: even on huge machines a
// content reader never pins more than this of decompressed blocks.
const contentBlockCacheMaxMB = 256

// contentBlockCacheMinMB floors the shared cache (and the env override), so
// a configured cache always holds several blocks.
const contentBlockCacheMinMB = 8

// contentBlockCacheKey identifies one cached block: its store (shards have
// separate stores) and the block's logical start.
type contentBlockKey struct {
	store *contentTextStore
	start uint64
}

// contentBlockCache is a bounded LRU of decompressed text blocks shared by
// every reader of one index (a composite shares one across its shards), so
// repeated and adjacent queries, typing refinements, and the snippet pass
// after a search all hit warm blocks. The cap is bytes, defaulting to
// TotalRAM/64 clamped to [32 MiB, 128 MiB] and overridable with
// SEEKFS_CONTENT_BLOCK_CACHE_MB (clamped to [8 MiB, 256 MiB]).
//
// Lifetime and pins: entries are decompressed COPIES owned by the cache, so
// they never alias the mapped section and the pin discipline
// (content_index.go pinRead/Release) is untouched — unmapping the backing
// file cannot invalidate a cached block. Dropping the owning reader drops
// the cache (evicted buffers return to the pool, the rest to the GC), which
// is what "released on index release" means here: no bytes outlive the
// reader, and Release waits out pins exactly as before.
//
// Concurrency: cache memory is only ever touched under mu. Callers copy out
// through copyBlock (under the same hold); insert transfers ownership in.
// No cache slice is ever retained past its lock hold, so an evicted buffer
// returning to the pool can never be observed mid-reuse.
type contentBlockCache struct {
	mu        sync.Mutex
	capBytes  int64
	usedBytes int64
	ll        *list.List // front = MRU; elements contentBlockKey
	ix        map[contentBlockKey]*list.Element
	data      map[contentBlockKey][]byte
	inflight  map[contentBlockKey]chan struct{}
	// Lifetime decode counters (atomics: updated on paths both inside
	// and outside mu). hits counts copyBlock hits; misses counts block
	// decodes performed (a duplicate install still decoded, so it still
	// counts); decodedBytes sums the decoded lengths. They exist so
	// query profiling can attribute "N block touches x ~M us" per
	// shape without perturbing the hot path; resetStats zeroes them.
	hits         atomic.Uint64
	misses       atomic.Uint64
	decodedBytes atomic.Uint64
}

// contentBlockCacheStats is one snapshot of a block cache's lifetime
// decode counters; see contentBlockCache.
type contentBlockCacheStats struct {
	Hits         uint64
	Misses       uint64
	DecodedBytes uint64
}

// stats snapshots the lifetime decode counters.
func (c *contentBlockCache) stats() contentBlockCacheStats {
	if c == nil {
		return contentBlockCacheStats{}
	}
	return contentBlockCacheStats{
		Hits:         c.hits.Load(),
		Misses:       c.misses.Load(),
		DecodedBytes: c.decodedBytes.Load(),
	}
}

// resetStats zeroes the lifetime decode counters (drop does not: entries
// are released but the counts describe the reader's lifetime).
func (c *contentBlockCache) resetStats() {
	if c == nil {
		return
	}
	c.hits.Store(0)
	c.misses.Store(0)
	c.decodedBytes.Store(0)
}

// contentBlockCacheCap returns the shared-cache byte cap: TotalRAM/64
// clamped to [32 MiB, 128 MiB], overridden by
// SEEKFS_CONTENT_BLOCK_CACHE_MB (megabytes, clamped to [8 MiB, 256 MiB]).
func contentBlockCacheCap() int64 {
	capBytes := int64(contentBlockCacheDefaultMB) << 20
	if total, err := contentTotalRamBytes(); err == nil && total > 0 {
		capBytes = int64(uint64(total) / 64)
		if capBytes < 32<<20 {
			capBytes = 32 << 20
		}
		if capBytes > 128<<20 {
			capBytes = 128 << 20
		}
	}
	if v := os.Getenv("SEEKFS_CONTENT_BLOCK_CACHE_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			capBytes = int64(n) << 20
		}
	}
	if capBytes < contentBlockCacheMinMB<<20 {
		capBytes = contentBlockCacheMinMB << 20
	}
	if capBytes > contentBlockCacheMaxMB<<20 {
		capBytes = contentBlockCacheMaxMB << 20
	}
	return capBytes
}

func newContentBlockCache() *contentBlockCache {
	return &contentBlockCache{
		capBytes: contentBlockCacheCap(),
		ll:       list.New(),
		ix:       make(map[contentBlockKey]*list.Element),
		data:     make(map[contentBlockKey][]byte),
		inflight: make(map[contentBlockKey]chan struct{}),
	}
}

// copyBlock copies up to len(dst) decompressed bytes of the block at start,
// beginning at block offset off, into dst. found=false is a miss. The copy
// happens under the cache lock, so the bytes are stable.
func (c *contentBlockCache) copyBlock(store *contentTextStore, start uint64, off int, dst []byte) (n int, found bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := contentBlockKey{store: store, start: start}
	el, ok := c.ix[key]
	if !ok {
		return 0, false
	}
	c.ll.MoveToFront(el)
	data := c.data[key]
	if off < 0 || off >= len(data) {
		return 0, false
	}
	n = copy(dst, data[off:])
	c.hits.Add(1)
	return n, true
}

func (c *contentBlockCache) evict(el *list.Element) {
	key := el.Value.(contentBlockKey)
	if data, ok := c.data[key]; ok {
		delete(c.data, key)
		c.usedBytes -= int64(len(data))
		contentBlockBufPut(data)
	}
	delete(c.ix, key)
	c.ll.Remove(el)
}

// drop releases every entry (pooled buffers return to the pool). Dropping
// the owning reader drops the cache the same way; this is the explicit
// release for paths that want it prompt.
func (c *contentBlockCache) drop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, data := range c.data {
		contentBlockBufPut(data)
		delete(c.data, key)
	}
	// Release singleflight waiters too: with no entries installed they
	// decode for themselves instead of hanging on a dropped cache.
	for key, ch := range c.inflight {
		delete(c.inflight, key)
		close(ch)
	}
	c.ix = make(map[contentBlockKey]*list.Element)
	c.ll.Init()
	c.usedBytes = 0
}

// contentBlockBufPool recycles decode-output buffers: DecodeAll into a
// pooled buffer skips the ~150 µs fresh 512 KiB allocation measured on this
// box. Only buffers up to 2x the block target are pooled; bigger ones (a
// lone huge document's block) allocate fresh and are dropped, never hoarded.
var contentBlockBufPool = sync.Pool{New: func() any {
	b := make([]byte, 0, contentTextBlockTarget)
	return &b
}}

// contentBlockBufGet returns a buffer with at least n bytes of capacity:
// pooled when n fits the pool discipline, fresh otherwise.
func contentBlockBufGet(n int) []byte {
	if n <= 2*contentTextBlockTarget {
		if v := contentBlockBufPool.Get(); v != nil {
			if bp, ok := v.(*[]byte); ok && bp != nil && cap(*bp) >= n {
				return (*bp)[:0]
			}
		}
	}
	return make([]byte, 0, n)
}

// contentBlockBufPut returns a buffer to the pool when it fits the
// discipline; anything else is left for the GC.
func contentBlockBufPut(b []byte) {
	if cap(b) <= 2*contentTextBlockTarget {
		nb := b[:0]
		contentBlockBufPool.Put(&nb)
	}
}

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

// contentTextStore is a decoded CXST section: the block table plus the
// frames they address. frames aliases the mapped section (or a heap copy);
// blocks decode on demand through the owning reader's shared block cache
// (nil cache decodes direct, for store-level tests). dict, when present, is
// the shard's CXTD content the frames were encoded against (v8); pooled
// decoders each own a private copy, so there is no lifetime coupling to the
// mapped section beyond the section CRC that verified it.
type contentTextStore struct {
	blocks   []contentTextBlockMeta
	frames   []byte
	cache    *contentBlockCache
	dictID   uint32
	dict     []byte
	dictPool sync.Pool
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

// copySpan copies the block span [off, off+len(dst)) of data into dst.
func copySpan(data []byte, off int, dst []byte) int {
	if off < 0 || off >= len(data) {
		return 0
	}
	return copy(dst, data[off:])
}

// fetchMiss serves one block-cache miss: join an in-flight decode when
// another goroutine is already decoding the block (parallel verifiers
// racing the same block decode it once), else decode outside the lock and
// install. At most one redundant decode happens (a joiner woken onto an
// instantly-evicted entry decodes itself); every buffer's ownership transfers
// exactly once.
func (st *contentTextStore) fetchMiss(b contentTextBlockMeta, in int, room []byte) (int, bool) {
	c := st.cache
	frame := st.frames[b.compOff : b.compOff+uint64(b.compLen)]
	for attempt := 0; attempt < 2; attempt++ {
		wait, mine := c.claimSingleflight(st, b.logicalStart)
		if !mine {
			if wait != nil {
				<-wait
			}
			if n, ok := c.copyBlock(st, b.logicalStart, in, room); ok {
				return n, true
			}
			continue
		}
		data, err := st.decodeBlock(frame, int(b.rawLen))
		if err != nil {
			// Release joiners on failure: remove the claim without an
			// install so they decode themselves instead of hanging.
			c.mu.Lock()
			if ch, ok := c.inflight[contentBlockKey{store: st, start: b.logicalStart}]; ok {
				delete(c.inflight, contentBlockKey{store: st, start: b.logicalStart})
				close(ch)
			}
			c.mu.Unlock()
			return 0, false
		}
		return c.installAndCopy(st, b.logicalStart, in, data, room), true
	}
	// Pathological churn: decode privately, cache nothing.
	data, err := st.decodeBlock(frame, int(b.rawLen))
	if err != nil {
		return 0, false
	}
	c.misses.Add(1)
	c.decodedBytes.Add(uint64(len(data)))
	n := copySpan(data, in, room)
	contentBlockBufPut(data)
	return n, n > 0
}

// installAndCopy inserts data (taking ownership in all cases: a duplicate
// insert recycles it to the pool) and copies its [off, off+len(dst)) span
// into dst — all under one lock hold, so the copied bytes are stable and no
// caller ever retains cache memory past the hold. It also closes the block's
// singleflight channel, releasing waiters onto the fresh entry.
func (c *contentBlockCache) installAndCopy(store *contentTextStore, start uint64, off int, data []byte, dst []byte) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The caller decoded this block (a duplicate install still paid the
	// decode; only the buffer is recycled), so it counts as a miss with
	// its decoded bytes either way.
	c.misses.Add(1)
	c.decodedBytes.Add(uint64(len(data)))
	key := contentBlockKey{store: store, start: start}
	if _, dup := c.ix[key]; dup {
		n := copySpan(c.data[key], off, dst)
		contentBlockBufPut(data)
		return n
	}
	c.ix[key] = c.ll.PushFront(key)
	c.data[key] = data
	c.usedBytes += int64(len(data))
	for c.usedBytes > c.capBytes {
		back := c.ll.Back()
		if back == nil || back == c.ll.Front() {
			break
		}
		c.evict(back)
	}
	n := copySpan(data, off, dst)
	if ch, ok := c.inflight[key]; ok {
		delete(c.inflight, key)
		close(ch)
	}
	return n
}

// claimSingleflight registers the caller as the decoder of an absent block,
// or joins an in-flight decode. mine=false returns a channel closed when the
// winner installs; the joiner then retries copyBlock (a hit, short of
// instant eviction under cap pressure). mine=true means decode and
// installAndCopy (which releases the joiners).
func (c *contentBlockCache) claimSingleflight(store *contentTextStore, start uint64) (wait <-chan struct{}, mine bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := contentBlockKey{store: store, start: start}
	if ch, ok := c.inflight[key]; ok {
		return ch, false
	}
	// Re-check under the same hold: a winner may have installed between the
	// caller's copyBlock miss and this claim.
	if _, ok := c.ix[key]; ok {
		return nil, false
	}
	ch := make(chan struct{})
	c.inflight[key] = ch
	return ch, true
}

// contentDecodeBlock decodes one zstd frame into a pooled buffer, verifying
// the raw length. The caller owns the buffer and must pool-put or transfer
// it exactly once.
func contentDecodeBlock(frame []byte, rawLen int) ([]byte, error) {
	dec, ok := contentZstdDecPool.Get().(*zstd.Decoder)
	if !ok || dec == nil {
		return nil, errors.New("content text store decoder unavailable")
	}
	out, err := dec.DecodeAll(frame, contentBlockBufGet(rawLen))
	contentZstdDecPool.Put(dec)
	if err != nil {
		return nil, fmt.Errorf("content text store block: %w", err)
	}
	if len(out) != rawLen {
		return nil, errors.New("content text store block length mismatch")
	}
	return out, nil
}

// decodeBlock decodes one frame with the store's dictionary when it has one
// (v8, from the pooled dict decoders) or plain otherwise. Ownership rules
// match contentDecodeBlock.
func (st *contentTextStore) decodeBlock(frame []byte, rawLen int) ([]byte, error) {
	if len(st.dict) == 0 || st.dictID == 0 {
		return contentDecodeBlock(frame, rawLen)
	}
	dec, ok := st.dictPool.Get().(*zstd.Decoder)
	if !ok || dec == nil {
		// First use (or a zero pool in store-level tests): build a decoder
		// over a private copy of the dict content, so pooled decoders never
		// alias the mapped CXTD section.
		dictCopy := append([]byte(nil), st.dict...)
		var err error
		dec, err = zstd.NewReader(nil, zstd.WithDecoderDictRaw(st.dictID, dictCopy))
		if err != nil {
			return nil, fmt.Errorf("content text store dict decoder: %w", err)
		}
	}
	out, err := dec.DecodeAll(frame, contentBlockBufGet(rawLen))
	st.dictPool.Put(dec)
	if err != nil {
		return nil, fmt.Errorf("content text store block: %w", err)
	}
	if len(out) != rawLen {
		return nil, errors.New("content text store block length mismatch")
	}
	return out, nil
}

// textFor returns a fresh copy of length bytes at logical offset off,
// stitching across block boundaries for documents that span them. ok is false
// when the span is out of range or a block fails to decode: callers treat it
// as absent text, never as a match.
//
// Every block comes from the shared cache when one is attached: a hit copies
// out (no decode), a miss decodes outside the lock into a pooled buffer and
// installs it, copying out under the same hold. Decompression runs without
// the mutex, so concurrent misses on distinct blocks proceed in parallel;
// duplicate misses decode twice and the loser recycles (CPU only, never
// correctness). A nil cache decodes direct per block (store-level tests).
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
		b := st.blocks[i]
		in := off - b.logicalStart
		if in >= uint64(b.rawLen) {
			return nil, false
		}
		room := out[len(out):cap(out)]
		var n int
		if st.cache != nil {
			var ok bool
			if n, ok = st.cache.copyBlock(st, b.logicalStart, int(in), room); !ok {
				if n, ok = st.fetchMiss(b, int(in), room); !ok {
					return nil, false
				}
			}
		} else {
			frame := st.frames[b.compOff : b.compOff+uint64(b.compLen)]
			data, err := st.decodeBlock(frame, int(b.rawLen))
			if err != nil {
				return nil, false
			}
			n = copySpan(data, int(in), room)
			contentBlockBufPut(data)
		}
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
// during assembly. Append buffers whole documents and cuts a block at the
// first document boundary at or past the target; FinishFile writes the final
// section file (header + table + frames). Peak heap is one target block plus
// one document plus the encoder workspace.
//
// The optional dictionary (v8, setDict anytime before FinishFile) is trained
// per shard from the shard head and stored as its own CXTD section: frames
// cut after it reference it. Without a dictionary the writer encodes plain
// at BestCompression; with one it encodes at BetterCompression (Best ignores
// dictionaries), which the dictionary more than repays on small blocks. The
// encoder is built lazily at the first flush so the level follows the dict.
type contentTextBlockWriter struct {
	enc     *zstd.Encoder
	dict    []byte
	dictID  uint32
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
	f, err := os.CreateTemp(tmpDir, "cxframes-*.bin")
	if err != nil {
		return nil, err
	}
	return &contentTextBlockWriter{
		frames:  f,
		framesW: bufio.NewWriterSize(f, contentSectionFileBufSize),
	}, nil
}

// setDict installs the shard's trained dictionary, anytime before FinishFile.
// Blocks cut before it stay plain, later ones reference it; frames are
// self-describing per dict ID so a mixed store decodes either way, and the
// same input always trains the same dict at the same point, so output stays
// byte-identical. (Training mid-stream keeps the assembler single-pass: the
// first ~sample bytes encode plain, the rest with the dictionary.)
func (w *contentTextBlockWriter) setDict(id uint32, dict []byte) error {
	if w.closed {
		return errors.New("content text block writer closed")
	}
	if len(dict) == 0 || id == 0 {
		return nil
	}
	// A plain encoder may already exist (blocks cut before training); swap
	// it so later blocks use the dictionary. Frames are independent, so
	// mixing is safe and deterministic.
	if w.enc != nil {
		_ = w.enc.Close()
		w.enc = nil
	}
	w.dict = dict
	w.dictID = id
	return nil
}

// ensureEncoder builds the frame encoder for the configured mode: plain at
// BestCompression (~20 s per 553 MB single-threaded, noise next to
// extraction), or at BetterCompression with the dictionary and a 1 MiB
// window covering dict plus block (BestCompression has no dict path).
func (w *contentTextBlockWriter) ensureEncoder() error {
	if w.enc != nil {
		return nil
	}
	var enc *zstd.Encoder
	var err error
	if len(w.dict) > 0 {
		enc, err = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
			zstd.WithEncoderDictRaw(w.dictID, w.dict),
			zstd.WithWindowSize(1<<20))
	} else {
		enc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	}
	if err != nil {
		return err
	}
	w.enc = enc
	return nil
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
	if err := w.ensureEncoder(); err != nil {
		return err
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
	if w.enc != nil {
		_ = w.enc.Close()
		w.enc = nil
	}
}

// FinishFile flushes the tail block and writes the final CXST section file at
// path: header + block table + the spooled frames. The frames spool is
// removed.
func (w *contentTextBlockWriter) FinishFile(path string) error {
	if w.closed {
		return errors.New("content text block writer closed")
	}
	w.closed = true
	// The encoder is lazy (nil on an empty corpus); flush builds it only
	// when a block actually needs encoding.
	defer func() {
		if w.enc != nil {
			w.enc.Close()
		}
	}()
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
	if w.enc != nil {
		_ = w.enc.Close()
		w.enc = nil
	}
}
