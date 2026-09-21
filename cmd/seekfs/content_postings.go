package main

// Shared posting codec for content terms (CXTR) and content trigrams (CXGR).
//
// The v9 record block format carries only []uint32, so content postings — which
// need a term frequency for scoring, and string or 3-byte keys — get their own
// block codec. Each posting list is split into 1024-doc blocks so a reader can
// page through a term without decoding the whole list, and so block metadata can
// later drive block-max skipping.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
)

const contentPostingBlockSize = 1024

// contentDocFreq is one posting: a document and the term's frequency in it
// (always 1 for trigrams).
type contentDocFreq struct {
	docID uint32
	tf    uint32
}

// contentPostingEntry is the dictionary row for one key.
type contentPostingEntry struct {
	keyOff     uint32
	keyLen     uint16
	count      uint32
	firstBlock uint32
	blockCount uint32
}

// contentBlockMeta locates one encoded block and records its bounds.
type contentBlockMeta struct {
	firstDocID uint32
	count      uint32
	byteOffset uint64
	byteLen    uint32
	maxTF      uint32
}

const contentPostingEntrySize = 20 // keyOff(4)+keyLen(2)+pad(2)+count(4)+firstBlock(4)+blockCount(4)
const contentBlockMetaSize = 24    // firstDocID(4)+count(4)+byteOffset(8)+byteLen(4)+maxTF(4)

// contentPostingIndex is the decoded dictionary plus lazy block access.
type contentPostingIndex struct {
	entries   []contentPostingEntry
	keyBlob   []byte
	blocks    []contentBlockMeta
	blockBlob []byte
}

// encodeContentPostingSection encodes key -> sorted postings. The input postings
// for each key must already be sorted by docID ascending and tf non-zero.
func encodeContentPostingSection(postings map[string][]contentDocFreq) []byte {
	if len(postings) == 0 {
		return nil
	}
	keys := make([]string, 0, len(postings))
	for k, v := range postings {
		if k != "" && len(v) > 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var keyBlob bytes.Buffer
	entries := make([]contentPostingEntry, 0, len(keys))
	blocks := make([]contentBlockMeta, 0, len(keys))
	var blockBlob bytes.Buffer
	var scratch [binary.MaxVarintLen64]byte

	for _, key := range keys {
		if len(key) > int(^uint16(0)) {
			continue
		}
		ids := postings[key]
		entry := contentPostingEntry{
			keyOff:     uint32(keyBlob.Len()),
			keyLen:     uint16(len(key)),
			count:      uint32(len(ids)),
			firstBlock: uint32(len(blocks)),
		}
		keyBlob.WriteString(key)

		for start := 0; start < len(ids); start += contentPostingBlockSize {
			end := min(len(ids), start+contentPostingBlockSize)
			chunk := ids[start:end]
			meta := contentBlockMeta{
				firstDocID: chunk[0].docID,
				count:      uint32(len(chunk)),
				byteOffset: uint64(blockBlob.Len()),
			}
			prev := int64(chunk[0].docID) - 1
			for _, p := range chunk {
				n := binary.PutUvarint(scratch[:], uint64(int64(p.docID)-prev-1))
				blockBlob.Write(scratch[:n])
				n = binary.PutUvarint(scratch[:], uint64(p.tf))
				blockBlob.Write(scratch[:n])
				prev = int64(p.docID)
				if p.tf > meta.maxTF {
					meta.maxTF = p.tf
				}
			}
			meta.byteLen = uint32(uint64(blockBlob.Len()) - meta.byteOffset)
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
	_ = binary.Write(&buf, binary.LittleEndian, uint32(keyBlob.Len()))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(blocks)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(blockBlob.Len()))
	for _, e := range entries {
		_ = binary.Write(&buf, binary.LittleEndian, e.keyOff)
		_ = binary.Write(&buf, binary.LittleEndian, e.keyLen)
		_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
		_ = binary.Write(&buf, binary.LittleEndian, e.count)
		_ = binary.Write(&buf, binary.LittleEndian, e.firstBlock)
		_ = binary.Write(&buf, binary.LittleEndian, e.blockCount)
	}
	for _, b := range blocks {
		_ = binary.Write(&buf, binary.LittleEndian, b.firstDocID)
		_ = binary.Write(&buf, binary.LittleEndian, b.count)
		_ = binary.Write(&buf, binary.LittleEndian, b.byteOffset)
		_ = binary.Write(&buf, binary.LittleEndian, b.byteLen)
		_ = binary.Write(&buf, binary.LittleEndian, b.maxTF)
	}
	buf.Write(keyBlob.Bytes())
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
	if off+entryCount*contentPostingEntrySize+blockCount*contentBlockMetaSize+keyBlobLen+blockBlobLen > len(data) {
		return nil, errors.New("content postings length mismatch")
	}
	idx := &contentPostingIndex{
		entries: make([]contentPostingEntry, entryCount),
		blocks:  make([]contentBlockMeta, blockCount),
	}
	for i := 0; i < entryCount; i++ {
		p := off + i*contentPostingEntrySize
		idx.entries[i] = contentPostingEntry{
			keyOff:     binary.LittleEndian.Uint32(data[p:]),
			keyLen:     binary.LittleEndian.Uint16(data[p+4:]),
			count:      binary.LittleEndian.Uint32(data[p+8:]),
			firstBlock: binary.LittleEndian.Uint32(data[p+12:]),
			blockCount: binary.LittleEndian.Uint32(data[p+16:]),
		}
	}
	off += entryCount * contentPostingEntrySize
	for i := 0; i < blockCount; i++ {
		p := off + i*contentBlockMetaSize
		idx.blocks[i] = contentBlockMeta{
			firstDocID: binary.LittleEndian.Uint32(data[p:]),
			count:      binary.LittleEndian.Uint32(data[p+4:]),
			byteOffset: binary.LittleEndian.Uint64(data[p+8:]),
			byteLen:    binary.LittleEndian.Uint32(data[p+16:]),
			maxTF:      binary.LittleEndian.Uint32(data[p+20:]),
		}
	}
	off += blockCount * contentBlockMetaSize
	idx.keyBlob = data[off : off+keyBlobLen]
	off += keyBlobLen
	idx.blockBlob = data[off : off+blockBlobLen]
	return idx, nil
}

// key returns the dictionary key for an entry.
func (idx *contentPostingIndex) key(e contentPostingEntry) string {
	end := int(e.keyOff) + int(e.keyLen)
	if end > len(idx.keyBlob) {
		return ""
	}
	return string(idx.keyBlob[e.keyOff:end])
}

// contentPostingDecodeHook, when non-nil, is called for every posting the
// streaming iterator decodes. Tests set it to assert a bounded decode stops
// early instead of materializing the whole list.
var contentPostingDecodeHook func()

// postings returns a forward iterator over key's postings in docID order. found
// is false when the key is absent or its block range is out of bounds, matching
// lookup. The iterator decodes one block at a time; callers that stop early
// never touch the remaining blocks, so a broad term's list is never fully
// materialized.
func (idx *contentPostingIndex) postings(key string) (next func() (uint32, uint32, bool), found bool) {
	i := sort.Search(len(idx.entries), func(i int) bool { return idx.key(idx.entries[i]) >= key })
	if i >= len(idx.entries) || idx.key(idx.entries[i]) != key {
		return nil, false
	}
	e := idx.entries[i]
	if int64(e.firstBlock)+int64(e.blockCount) > int64(len(idx.blocks)) {
		return nil, false
	}
	block, endBlock := e.firstBlock, e.firstBlock+e.blockCount
	left := e.count
	var buf []byte
	var prev int64
	next = func() (uint32, uint32, bool) {
		for left > 0 {
			if len(buf) == 0 {
				if block >= endBlock {
					return 0, 0, false
				}
				meta := idx.blocks[block]
				block++
				if meta.byteOffset+uint64(meta.byteLen) > uint64(len(idx.blockBlob)) {
					return 0, 0, false
				}
				buf = idx.blockBlob[meta.byteOffset : meta.byteOffset+uint64(meta.byteLen)]
				prev = int64(meta.firstDocID) - 1
			}
			d, n := binary.Uvarint(buf)
			if n <= 0 {
				return 0, 0, false
			}
			buf = buf[n:]
			tf, n2 := binary.Uvarint(buf)
			if n2 <= 0 {
				return 0, 0, false
			}
			buf = buf[n2:]
			docID := uint32(prev + 1 + int64(d))
			prev = int64(docID)
			left--
			if contentPostingDecodeHook != nil {
				contentPostingDecodeHook()
			}
			return docID, uint32(tf), true
		}
		return 0, 0, false
	}
	return next, true
}

// forEach invokes fn for each posting of key in docID order, stopping early when
// fn returns false. found reports whether the key exists (a truncated/corrupt
// list still reports found). It never allocates the full posting list.
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
func (idx *contentPostingIndex) lookup(key string) ([]contentDocFreq, bool) {
	var out []contentDocFreq
	found := idx.forEach(key, func(docID, tf uint32) bool {
		out = append(out, contentDocFreq{docID: docID, tf: tf})
		return true
	})
	return out, found
}
