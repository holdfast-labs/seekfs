package main

// Content trigram (CXGR) block codec, format v9.
//
// The v9 record block format carries only []uint32, so content postings get
// their own block codec. Each posting list is split into
// contentPostingBlockSize blocks so a reader pages through a term without
// decoding the whole list, and each block records its first docID so
// intersections can skip.
//
// v9 changes (format v9 gate; v8 sections never meet this decoder):
//   - Entries drop count and firstBlock (16 -> 12 bytes); the decoder
//     rebuilds both as prefix sums over the key-ordered tables.
//   - Block metas drop byteOffset (16 -> 12 bytes); the decoder rebuilds
//     offsets as the prefix sum of byteLens.
//   - Single-document grams are singleton entries carrying the docID inline
//     with no blocks (see contentPostingEntry).
//
// v7 changes (format v7 gate; v6 sections never meet this decoder):
//   - No term frequencies: grams always had tf=1 and no query path reads tf
//     (relevance re-verifies against stored text; fuzzy scores by df), so a
//     block stores docIDs only. This alone removes ~1 byte per posting.
//   - No maxTF block metadata (it existed only for scoring).
//   - Dense blocks are bitmaps: a full 4096-posting block of a dense gram
//     (delta list ~4-8 KiB) becomes ~520 bytes. The encoder picks the smaller
//     of delta-list and bitmap per block; the iterator expands either.
//   - Fixed 3-byte gram keys sit inline in the dictionary entry (no key blob,
//     no key offsets); entries stay sorted for binary search.
//
// Block payload: kind byte 0 = delta-varint docID list (first doc stored as
// docID-firstDocID, then successive gaps of docID-prev-1, exactly the v6
// delta scheme minus tf), kind 1 = bitmap (span uvarint =
// last-first+1, then ceil(span/8) bytes, bit i = firstDocID+i present).

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
)

// contentPostingBlockSize is the posting count a block is cut at. 4096-doc
// blocks (up from v6's 1024) quarter the block-metadata overhead and amortize
// the bitmap kind/span header over four times the postings; readers still
// decode one block at a time and stop mid-block, so bounded candidate
// materialization is unchanged. The per-block firstDocID skip metadata the
// intersection relies on is kept.
const contentPostingBlockSize = 4096

// contentGramPruneDF caps a gram's document frequency in the CXGR posting
// section: a gram occurring in more documents is pruned (its postings are
// dropped and the key joins the CXGP pruned set). Dense grams dominate the
// section (measured: df >= 1000 keys are 5% of entries but 80% of blob
// bytes on the Chromium sample) while contributing almost no selectivity —
// the intersection is driven by the query's rarest gram — so pruning them
// shrinks the section and speeds selective queries. Correctness is
// structural: pruned grams are skipped as constraints (the verification
// still decides), and a query whose every gram is pruned falls back to the
// full scan like a sub-3-byte term. A degenerate term pays a scan bounded
// by the result window, which terminates fast exactly because the term is
// common. A var so tests can lower it.
var contentGramPruneDF = 25000

// contentDocFreq is one posting: a document and the term's frequency in it.
// tf is retained for API compatibility (callers build these maps) but the v7
// section encoding stores docIDs only; df for ranking comes from entry.count.
type contentDocFreq struct {
	docID uint32
	tf    uint32
}

// contentPostingEntry is the dictionary row for one gram key.
//
// v9 changes (format v9 gate; v8 sections never meet this decoder):
//   - The entry drops count and firstBlock (16 -> 12 bytes). Blocks are
//     emitted in key order, so firstBlock is the prefix sum of the preceding
//     entries' block counts, and count is the sum of the entry's block
//     counts; the decoder rebuilds both in one pass.
//   - A gram occurring in exactly one document is a singleton entry: it
//     carries the docID inline and owns no blocks (measured: ~1/3 of gram
//     keys on code corpora), saving a block meta plus a 2-byte block payload
//     per such key. The pad byte becomes a flags byte (0 = multi,
//     1 = singleton).
type contentPostingEntry struct {
	key        [3]byte
	single     bool
	count      uint32
	firstBlock uint32
	blockCount uint32
	singleDoc  uint32
}

// contentPostingEntryMulti and contentPostingEntrySingle are the flags-byte
// values distinguishing the two v9 entry kinds.
const (
	contentPostingEntryMulti  = 0
	contentPostingEntrySingle = 1
)

// contentBlockMeta locates one encoded block and records its bounds.
// byteOffset is u32: a section never exceeds the 4 GiB sidecar cap, so every
// block address fits.
//
// v9 drops the stored byteOffset (16 -> 12 bytes): blocks are laid out in
// order, so each offset is the prefix sum of the preceding blocks' byteLens;
// the decoder rebuilds them in one pass and rejects a section whose running
// sum does not land exactly on the blob length.
type contentBlockMeta struct {
	firstDocID uint32
	count      uint32
	byteOffset uint32
	byteLen    uint32
}

const contentPostingEntrySize = 12 // key(3)+flags(1)+a(4)+b(4): multi=(firstBlock,blockCount), single=(docID,0)
const contentBlockMetaSize = 12    // firstDocID(4)+count(4)+byteLen(4)

// contentPostingBlockList and contentPostingBlockBitmap are the per-block
// payload kinds.
const (
	contentPostingBlockList   = 0
	contentPostingBlockBitmap = 1
)

// contentPostingIndex is the decoded dictionary plus lazy block access.
type contentPostingIndex struct {
	entryData []byte
	blockData []byte
	blockBlob []byte
	blockOff  []uint32
}

func (idx *contentPostingIndex) entryCount() int { return len(idx.entryData) / contentPostingEntrySize }
func (idx *contentPostingIndex) blockCount() int { return len(idx.blockData) / contentBlockMetaSize }
func (idx *contentPostingIndex) entryAt(i int) contentPostingEntry {
	p := idx.entryData[i*contentPostingEntrySize:]
	e := contentPostingEntry{}
	copy(e.key[:], p[:3])
	e.single = p[3] == contentPostingEntrySingle
	if e.single {
		e.count, e.singleDoc = 1, binary.LittleEndian.Uint32(p[4:])
	} else {
		e.firstBlock, e.blockCount = binary.LittleEndian.Uint32(p[4:]), binary.LittleEndian.Uint32(p[8:])
		if uint64(e.firstBlock)+uint64(e.blockCount) <= uint64(idx.blockCount()) {
			for b := e.firstBlock; b < e.firstBlock+e.blockCount; b++ {
				e.count += idx.blockAt(int(b)).count
			}
		}
	}
	return e
}
func (idx *contentPostingIndex) blockAt(i int) contentBlockMeta {
	p := idx.blockData[i*contentBlockMetaSize:]
	return contentBlockMeta{firstDocID: binary.LittleEndian.Uint32(p), count: binary.LittleEndian.Uint32(p[4:]), byteOffset: idx.blockOff[i], byteLen: binary.LittleEndian.Uint32(p[8:])}
}

// encodeContentPostingBlock encodes one block's docIDs (ascending),
// choosing the smaller of delta-list and bitmap.
func encodeContentPostingBlock(docIDs []uint32, firstDocID uint32) []byte {
	var scratch [binary.MaxVarintLen64]byte
	// Delta-list size: first as docID-firstDocID, then gaps.
	listLen := 1 // kind byte
	prev := int64(firstDocID)
	for _, id := range docIDs {
		n := binary.PutUvarint(scratch[:], uint64(int64(id)-prev))
		listLen += n
		prev = int64(id)
	}
	// Bitmap size: kind + span + bits.
	span := uint64(1)
	if len(docIDs) > 0 {
		span = uint64(docIDs[len(docIDs)-1]-firstDocID) + 1
	}
	spanLen := 0
	{
		n := binary.PutUvarint(scratch[:], span)
		spanLen = n
	}
	bitmapLen := 1 + spanLen + int((span+7)/8)
	if bitmapLen < listLen {
		out := make([]byte, 0, bitmapLen)
		out = append(out, contentPostingBlockBitmap)
		out = binary.AppendUvarint(out, span)
		bits := make([]byte, (span+7)/8)
		for _, id := range docIDs {
			i := id - firstDocID
			bits[i/8] |= 1 << (i % 8)
		}
		out = append(out, bits...)
		return out
	}
	out := make([]byte, 0, listLen)
	out = append(out, contentPostingBlockList)
	prev = int64(firstDocID)
	for _, id := range docIDs {
		n := binary.PutUvarint(scratch[:], uint64(int64(id)-prev))
		out = append(out, scratch[:n]...)
		prev = int64(id)
	}
	return out
}

// decodeContentPostingBlockDocIDs expands one block payload into docIDs in
// order, calling emit for each. It never materializes the list.
func decodeContentPostingBlockDocIDs(payload []byte, firstDocID uint32, count uint32, emit func(docID uint32) bool) bool {
	if len(payload) == 0 {
		return false
	}
	switch payload[0] {
	case contentPostingBlockList:
		buf := payload[1:]
		prev := int64(firstDocID)
		for n := uint32(0); n < count; n++ {
			d, m := binary.Uvarint(buf)
			if m <= 0 {
				return false
			}
			buf = buf[m:]
			docID := uint32(prev + int64(d))
			prev = int64(docID)
			if !emit(docID) {
				return true
			}
		}
		return true
	case contentPostingBlockBitmap:
		span, m := binary.Uvarint(payload[1:])
		if m <= 0 {
			return false
		}
		bits := payload[1+m:]
		if uint64(len(bits)) < (span+7)/8 {
			return false
		}
		seen := uint32(0)
		for i := uint64(0); i < span && seen < count; i++ {
			if bits[i/8]&(1<<(i%8)) != 0 {
				if !emit(firstDocID + uint32(i)) {
					return true
				}
				seen++
			}
		}
		return seen == count
	default:
		return false
	}
}

// encodeContentPostingSection encodes key -> sorted postings. The input postings
// for each key must already be sorted by docID ascending. tf is ignored: the
// v7 section stores docIDs only (see the file header).
func encodeContentPostingSection(postings map[string][]contentDocFreq) []byte {
	if len(postings) == 0 {
		return nil
	}
	keys := make([]string, 0, len(postings))
	for k, v := range postings {
		if len(k) == 3 && len(v) > 0 && len(v) <= contentGramPruneDF {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	entries := make([]contentPostingEntry, 0, len(keys))
	blocks := make([]contentBlockMeta, 0, len(keys))
	var blockBlob bytes.Buffer

	for _, key := range keys {
		ids := postings[key]
		entry := contentPostingEntry{}
		copy(entry.key[:], key)

		docIDs := make([]uint32, len(ids))
		for i, p := range ids {
			docIDs[i] = p.docID
		}
		if len(docIDs) == 1 {
			// Singleton: the docID rides inline; no blocks are emitted.
			entry.single = true
			entry.count = 1
			entry.singleDoc = docIDs[0]
			entries = append(entries, entry)
			continue
		}
		entry.firstBlock = uint32(len(blocks))
		entry.count = uint32(len(docIDs))
		for start := 0; start < len(docIDs); start += contentPostingBlockSize {
			end := min(len(docIDs), start+contentPostingBlockSize)
			chunk := docIDs[start:end]
			meta := contentBlockMeta{
				firstDocID: chunk[0],
				count:      uint32(len(chunk)),
				byteOffset: uint32(blockBlob.Len()),
			}
			enc := encodeContentPostingBlock(chunk, chunk[0])
			blockBlob.Write(enc)
			meta.byteLen = uint32(uint64(blockBlob.Len()) - uint64(meta.byteOffset))
			blocks = append(blocks, meta)
		}
		entry.blockCount = uint32(len(blocks)) - entry.firstBlock
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil
	}

	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(entries)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0)) // keyBlobLen: v7 grams carry no key blob
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(blocks)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(blockBlob.Len()))
	for _, e := range entries {
		buf.Write(e.key[:])
		if e.single {
			_ = binary.Write(&buf, binary.LittleEndian, uint8(contentPostingEntrySingle))
			_ = binary.Write(&buf, binary.LittleEndian, e.singleDoc)
			_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
		} else {
			_ = binary.Write(&buf, binary.LittleEndian, uint8(contentPostingEntryMulti))
			_ = binary.Write(&buf, binary.LittleEndian, e.firstBlock)
			_ = binary.Write(&buf, binary.LittleEndian, e.blockCount)
		}
	}
	for _, b := range blocks {
		_ = binary.Write(&buf, binary.LittleEndian, b.firstDocID)
		_ = binary.Write(&buf, binary.LittleEndian, b.count)
		_ = binary.Write(&buf, binary.LittleEndian, b.byteLen)
	}
	buf.Write(blockBlob.Bytes())
	return buf.Bytes()
}

func decodeContentPostingIndex(data []byte) (*contentPostingIndex, error) {
	if len(data) < 16 {
		return nil, errors.New("content postings truncated")
	}
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	keyBlobLen := int(binary.LittleEndian.Uint32(data[4:]))
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(data[12:]))
	off := 16
	if entryCount < 0 || blockCount < 0 {
		return nil, errors.New("content postings negative count")
	}
	// v7 grams carry no key blob; a nonzero keyBlobLen is a corrupt (or v6)
	// section, which the version gate should already have refused.
	if keyBlobLen != 0 {
		return nil, errors.New("content postings carry an unexpected key blob")
	}
	if entryCount > (len(data)-off)/contentPostingEntrySize || blockCount > (len(data)-off-entryCount*contentPostingEntrySize)/contentBlockMetaSize || uint64(off)+uint64(entryCount)*contentPostingEntrySize+uint64(blockCount)*contentBlockMetaSize+uint64(keyBlobLen)+uint64(blockBlobLen) > uint64(len(data)) {
		return nil, errors.New("content postings length mismatch")
	}
	entryData := data[off : off+entryCount*contentPostingEntrySize]
	idx := &contentPostingIndex{entryData: entryData}
	for i := 0; i < entryCount; i++ {
		p := i * contentPostingEntrySize
		prev := (i - 1) * contentPostingEntrySize
		switch entryData[p+3] {
		case contentPostingEntryMulti:
			if binary.LittleEndian.Uint32(entryData[p+8:]) == 0 {
				return nil, errors.New("content postings multi entry without blocks")
			}
		case contentPostingEntrySingle:
			if binary.LittleEndian.Uint32(entryData[p+8:]) != 0 {
				return nil, errors.New("content postings singleton reserved word nonzero")
			}
		default:
			return nil, errors.New("content postings bad entry flags")
		}
		if i > 0 && bytes.Compare(entryData[prev:prev+3], entryData[p:p+3]) >= 0 {
			return nil, errors.New("content postings keys out of order")
		}
	}
	off += entryCount * contentPostingEntrySize
	// Rebuild the dropped firstBlock/count/byteOffset fields in one pass:
	// blocks are laid out in key order, so offsets prefix-sum the lengths
	// and entries prefix-sum the block counts.
	idx.blockData = data[off : off+blockCount*contentBlockMetaSize]
	idx.blockOff = make([]uint32, blockCount+1)
	var blobOff uint64
	for i := 0; i < blockCount; i++ {
		p := i * contentBlockMetaSize
		count, byteLen := binary.LittleEndian.Uint32(idx.blockData[p+4:]), binary.LittleEndian.Uint32(idx.blockData[p+8:])
		if count == 0 || count > contentPostingBlockSize {
			return nil, errors.New("content postings bad block count")
		}
		idx.blockOff[i] = uint32(blobOff)
		blobOff += uint64(byteLen)
		if blobOff > uint64(blockBlobLen) {
			return nil, errors.New("content postings block out of range")
		}
	}
	if blobOff != uint64(blockBlobLen) {
		return nil, errors.New("content postings blob length mismatch")
	}
	var nextFirst uint32
	for i := 0; i < idx.entryCount(); i++ {
		p := entryData[i*contentPostingEntrySize:]
		if p[3] == contentPostingEntrySingle {
			continue
		}
		// The stored firstBlock must match the prefix sum: entries own
		// contiguous block runs, so any gap or overlap is corruption.
		firstBlock, blocks := binary.LittleEndian.Uint32(p[4:]), binary.LittleEndian.Uint32(p[8:])
		if firstBlock != nextFirst {
			return nil, errors.New("content postings first block mismatch")
		}
		if uint64(firstBlock)+uint64(blocks) > uint64(idx.blockCount()) {
			return nil, errors.New("content postings entry out of range")
		}
		var sum uint32
		for b := firstBlock; b < firstBlock+blocks; b++ {
			sum += idx.blockAt(int(b)).count
		}
		_ = sum
		nextFirst += blocks
	}
	if int(nextFirst) != idx.blockCount() {
		return nil, errors.New("content postings trailing blocks")
	}
	off += blockCount * contentBlockMetaSize
	idx.blockBlob = data[off : off+blockBlobLen]
	idx.blockOff[blockCount] = uint32(blobOff)
	return idx, nil
}

// key returns the dictionary key for an entry.
func (idx *contentPostingIndex) key(e contentPostingEntry) string {
	return string(e.key[:])
}

// encodeContentPrunedGrams serializes the CXGP pruned-gram set: the pruned
// 3-byte keys in ascending order, concatenated (length/3 keys). Nil when
// nothing was pruned (the section is then omitted).
func encodeContentPrunedGrams(keys []string) []byte {
	keep := keys[:0:0]
	for _, k := range keys {
		if len(k) == 3 {
			keep = append(keep, k)
		}
	}
	if len(keep) == 0 {
		return nil
	}
	sort.Strings(keep)
	var out []byte
	for _, k := range keep {
		out = append(out, k[0], k[1], k[2])
	}
	return out
}

// decodeContentPrunedGrams parses a CXGP section into the gram set the query
// path skips as constraints. Keys are uint32 trigrams for O(1) lookup. Any
// framing error fails the load: like a corrupt gram section, a corrupt
// pruned set must rebuild, never serve results from the wrong constraint
// set (ignoring it would read pruned grams as absent and return empty).
func decodeContentPrunedGrams(data []byte) (map[uint32]struct{}, error) {
	if len(data)%3 != 0 {
		return nil, errors.New("content pruned grams length mismatch")
	}
	out := make(map[uint32]struct{}, len(data)/3)
	var prev uint32
	for i := 0; i < len(data); i += 3 {
		g := uint32(data[i])<<16 | uint32(data[i+1])<<8 | uint32(data[i+2])
		if i > 0 && g <= prev {
			return nil, errors.New("content pruned grams out of order")
		}
		prev = g
		out[g] = struct{}{}
	}
	return out, nil
}

// contentGramU32Of renders a 3-byte gram string as its uint32 key.
func contentGramU32Of(gram string) uint32 {
	return uint32(gram[0])<<16 | uint32(gram[1])<<8 | uint32(gram[2])
}

// contentPostingDecodeHook, when non-nil, is called for every posting the
// streaming iterator decodes. Tests set it to assert a bounded decode stops
// early instead of materializing the whole list.
var contentPostingDecodeHook func()

// postings returns a forward iterator over key's postings in docID order. found
// is false when the key is absent or its block range is out of bounds, matching
// lookup. The iterator decodes one block at a time; callers that stop early
// never touch the remaining blocks, so a broad term's list is never fully
// materialized. The second value is always 0: v7 stores docIDs only.
func (idx *contentPostingIndex) postings(key string) (next func() (uint32, uint32, bool), found bool) {
	if len(key) != 3 {
		return nil, false
	}
	var want [3]byte
	copy(want[:], key)
	i := sort.Search(idx.entryCount(), func(i int) bool {
		p := idx.entryData[i*contentPostingEntrySize:]
		return bytes.Compare(p[:3], want[:]) >= 0
	})
	if i >= idx.entryCount() || !bytes.Equal(idx.entryData[i*contentPostingEntrySize:i*contentPostingEntrySize+3], want[:]) {
		return nil, false
	}
	e := idx.entryAt(i)
	if e.single {
		doc := e.singleDoc
		done := false
		return func() (uint32, uint32, bool) {
			if done {
				return 0, 0, false
			}
			done = true
			if contentPostingDecodeHook != nil {
				contentPostingDecodeHook()
			}
			return doc, 0, true
		}, true
	}
	if int64(e.firstBlock)+int64(e.blockCount) > int64(idx.blockCount()) {
		return nil, false
	}
	block, endBlock := e.firstBlock, e.firstBlock+e.blockCount
	left := e.count
	// blockLeft is the postings remaining in the current block; when it
	// reaches zero the next block loads. Bounding bitmap scans by it (not by
	// the span end) keeps a multi-block key streaming: trailing zeros after
	// the block's last set bit must load the next block, not end the key.
	var blockLeft uint32
	var buf []byte // remainder of a list block (nil unless in a list block)
	var bits []byte
	var span, pos uint64
	var base uint32 // firstDocID of the current block
	var prev int64  // previous docID within a list block
	inList := false
	load := func() bool {
		if block >= endBlock {
			return false
		}
		meta := idx.blockAt(int(block))
		block++
		if uint64(meta.byteOffset)+uint64(meta.byteLen) > uint64(len(idx.blockBlob)) {
			return false
		}
		payload := idx.blockBlob[meta.byteOffset : meta.byteOffset+meta.byteLen]
		if len(payload) == 0 {
			return false
		}
		base = meta.firstDocID
		blockLeft = meta.count
		switch payload[0] {
		case contentPostingBlockList:
			buf = payload[1:]
			prev = int64(base)
			inList = true
		case contentPostingBlockBitmap:
			s, m := binary.Uvarint(payload[1:])
			if m <= 0 {
				return false
			}
			bits = payload[1+m:]
			span, pos = s, 0
			inList = false
		default:
			return false
		}
		return true
	}
	next = func() (uint32, uint32, bool) {
		for left > 0 {
			if blockLeft == 0 {
				if !load() {
					return 0, 0, false
				}
			}
			var docID uint32
			if inList {
				d, n := binary.Uvarint(buf)
				if n <= 0 {
					return 0, 0, false
				}
				buf = buf[n:]
				docID = uint32(prev + int64(d))
				prev = int64(docID)
			} else {
				found := false
				for pos < span {
					if pos/8 >= uint64(len(bits)) {
						return 0, 0, false
					}
					if bits[pos/8]&(1<<(pos%8)) != 0 {
						docID = base + uint32(pos)
						pos++
						found = true
						break
					}
					pos++
				}
				if !found {
					return 0, 0, false
				}
			}
			blockLeft--
			left--
			if contentPostingDecodeHook != nil {
				contentPostingDecodeHook()
			}
			return docID, 0, true
		}
		return 0, 0, false
	}
	return next, true
}

// forEach invokes fn for each posting of key in docID order, stopping early when
// fn returns false. found reports whether the key exists (a truncated/corrupt
// list still reports found). It never allocates the full posting list.
// test-only: driven by the content postings tests through lookup.
func (idx *contentPostingIndex) forEach(key string, fn func(docID, tf uint32) bool) bool {
	next, found := idx.postings(key)
	if !found {
		return false
	}
	for {
		docID, tf, ok := next()
		if !ok {
			return true
		}
		if !fn(docID, tf) {
			return true
		}
	}
}

// lookup returns the postings for key, decoded, in docID order.
// test-only: no production caller.
func (idx *contentPostingIndex) lookup(key string) ([]contentDocFreq, bool) {
	var out []contentDocFreq
	found := idx.forEach(key, func(docID, tf uint32) bool {
		out = append(out, contentDocFreq{docID: docID, tf: tf})
		return true
	})
	return out, found
}
