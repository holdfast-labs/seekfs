package main

// Content trigram (CXGR) block codec, format v7.
//
// The v9 record block format carries only []uint32, so content postings get
// their own block codec. Each posting list is split into
// contentPostingBlockSize blocks so a reader pages through a term without
// decoding the whole list, and each block records its first docID so
// intersections can skip.
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

// contentDocFreq is one posting: a document and the term's frequency in it.
// tf is retained for API compatibility (callers build these maps) but the v7
// section encoding stores docIDs only; df for ranking comes from entry.count.
type contentDocFreq struct {
	docID uint32
	tf    uint32
}

// contentPostingEntry is the dictionary row for one gram key.
type contentPostingEntry struct {
	key        [3]byte
	count      uint32
	firstBlock uint32
	blockCount uint32
}

// contentBlockMeta locates one encoded block and records its bounds.
// byteOffset is u32: a section never exceeds the 4 GiB sidecar cap, so every
// block address fits.
type contentBlockMeta struct {
	firstDocID uint32
	count      uint32
	byteOffset uint32
	byteLen    uint32
}

const contentPostingEntrySize = 16 // key(3)+pad(1)+count(4)+firstBlock(4)+blockCount(4)
const contentBlockMetaSize = 16    // firstDocID(4)+count(4)+byteOffset(4)+byteLen(4)

// contentPostingBlockList and contentPostingBlockBitmap are the per-block
// payload kinds.
const (
	contentPostingBlockList   = 0
	contentPostingBlockBitmap = 1
)

// contentPostingIndex is the decoded dictionary plus lazy block access.
type contentPostingIndex struct {
	entries   []contentPostingEntry
	blocks    []contentBlockMeta
	blockBlob []byte
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
		if len(k) == 3 && len(v) > 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	entries := make([]contentPostingEntry, 0, len(keys))
	blocks := make([]contentBlockMeta, 0, len(keys))
	var blockBlob bytes.Buffer

	for _, key := range keys {
		ids := postings[key]
		entry := contentPostingEntry{
			count:      uint32(len(ids)),
			firstBlock: uint32(len(blocks)),
		}
		copy(entry.key[:], key)

		docIDs := make([]uint32, len(ids))
		for i, p := range ids {
			docIDs[i] = p.docID
		}
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
		_ = binary.Write(&buf, binary.LittleEndian, uint8(0))
		_ = binary.Write(&buf, binary.LittleEndian, e.count)
		_ = binary.Write(&buf, binary.LittleEndian, e.firstBlock)
		_ = binary.Write(&buf, binary.LittleEndian, e.blockCount)
	}
	for _, b := range blocks {
		_ = binary.Write(&buf, binary.LittleEndian, b.firstDocID)
		_ = binary.Write(&buf, binary.LittleEndian, b.count)
		_ = binary.Write(&buf, binary.LittleEndian, b.byteOffset)
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
	if off+entryCount*contentPostingEntrySize+blockCount*contentBlockMetaSize+keyBlobLen+blockBlobLen > len(data) {
		return nil, errors.New("content postings length mismatch")
	}
	idx := &contentPostingIndex{
		entries: make([]contentPostingEntry, entryCount),
		blocks:  make([]contentBlockMeta, blockCount),
	}
	for i := 0; i < entryCount; i++ {
		p := off + i*contentPostingEntrySize
		copy(idx.entries[i].key[:], data[p:p+3])
		idx.entries[i].count = binary.LittleEndian.Uint32(data[p+4:])
		idx.entries[i].firstBlock = binary.LittleEndian.Uint32(data[p+8:])
		idx.entries[i].blockCount = binary.LittleEndian.Uint32(data[p+12:])
		if i > 0 && bytes.Compare(idx.entries[i-1].key[:], idx.entries[i].key[:]) >= 0 {
			return nil, errors.New("content postings keys out of order")
		}
	}
	off += entryCount * contentPostingEntrySize
	for i := 0; i < blockCount; i++ {
		p := off + i*contentBlockMetaSize
		idx.blocks[i] = contentBlockMeta{
			firstDocID: binary.LittleEndian.Uint32(data[p:]),
			count:      binary.LittleEndian.Uint32(data[p+4:]),
			byteOffset: binary.LittleEndian.Uint32(data[p+8:]),
			byteLen:    binary.LittleEndian.Uint32(data[p+12:]),
		}
		if idx.blocks[i].count == 0 || idx.blocks[i].count > contentPostingBlockSize {
			return nil, errors.New("content postings bad block count")
		}
		if uint64(idx.blocks[i].byteOffset)+uint64(idx.blocks[i].byteLen) > uint64(blockBlobLen) {
			return nil, errors.New("content postings block out of range")
		}
	}
	off += blockCount * contentBlockMetaSize
	idx.blockBlob = data[off : off+blockBlobLen]
	return idx, nil
}

// key returns the dictionary key for an entry.
func (idx *contentPostingIndex) key(e contentPostingEntry) string {
	return string(e.key[:])
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
	i := sort.Search(len(idx.entries), func(i int) bool {
		return bytes.Compare(idx.entries[i].key[:], want[:]) >= 0
	})
	if i >= len(idx.entries) || idx.entries[i].key != want {
		return nil, false
	}
	e := idx.entries[i]
	if int64(e.firstBlock)+int64(e.blockCount) > int64(len(idx.blocks)) {
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
		meta := idx.blocks[block]
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
