package main

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

func loadIndexMMap(path string) (*Index, error) {
	mapped, err := mapIndexFile(path)
	if err != nil {
		return nil, fmt.Errorf("map index %s: %w", path, err)
	}
	idx, err := readIndexMMap(mapped)
	if err != nil {
		_ = mapped.close()
		return nil, fmt.Errorf("read mapped index %s: %w", path, err)
	}
	idx.DBPath = path
	return idx, nil
}

func readIndexMMap(mapped *mappedIndexFile) (*Index, error) {
	if mapped == nil || len(mapped.data) < binary.Size(diskHeader{}) {
		return nil, errors.New("invalid mapped index")
	}
	data := mapped.data
	headerSize := binary.Size(diskHeader{})
	var magic [8]byte
	copy(magic[:], data[:8])
	header := diskHeader{
		Magic:       magic,
		Version:     binary.LittleEndian.Uint32(data[8:]),
		EntryCount:  binary.LittleEndian.Uint64(data[12:]),
		RootCount:   binary.LittleEndian.Uint64(data[20:]),
		BuiltUnix:   int64(binary.LittleEndian.Uint64(data[28:])),
		JournalID:   binary.LittleEndian.Uint64(data[36:]),
		Checkpoint:  int64(binary.LittleEndian.Uint64(data[44:])),
		Compact:     binary.LittleEndian.Uint32(data[52:]),
		NameBlobLen: binary.LittleEndian.Uint64(data[56:]),
		TokenCount:  binary.LittleEndian.Uint64(data[64:]),
	}
	sectionTableOffset := uint64(0)
	if header.Magic != indexMagic {
		return nil, errors.New("unsupported index format: only the v9 index format is supported")
	}
	if len(data) < headerSize+8 {
		return nil, errors.New("invalid mapped v9 index")
	}
	sectionTableOffset = binary.LittleEndian.Uint64(data[headerSize:])
	headerSize += 8
	if header.Version != indexVersion {
		return nil, fmt.Errorf("unsupported index version %d: only v9 indexes are supported", header.Version)
	}
	if header.Compact == 0 {
		return nil, errors.New("mmap low-memory mode requires compact index")
	}
	if header.EntryCount > uint64(^uint(0)>>1) || header.RootCount > uint64(^uint(0)>>1) ||
		header.NameBlobLen > uint64(^uint(0)>>1) || header.TokenCount > uint64(^uint(0)>>1) {
		return nil, errors.New("index too large")
	}
	off := headerSize
	idx := &Index{
		Version:    int(header.Version),
		BuiltAt:    time.Unix(0, header.BuiltUnix),
		Roots:      make([]string, int(header.RootCount)),
		JournalID:  header.JournalID,
		Checkpoint: header.Checkpoint,
		Compact:    true,
	}
	var err error
	if idx.Source, off, err = mappedReadString(data, off); err != nil {
		return nil, err
	}
	if idx.Volume, off, err = mappedReadString(data, off); err != nil {
		return nil, err
	}
	if idx.ContentHash, off, err = mappedReadString(data, off); err != nil {
		return nil, err
	}
	for i := range idx.Roots {
		if idx.Roots[i], off, err = mappedReadString(data, off); err != nil {
			return nil, err
		}
	}
	nameBlobLen := int(header.NameBlobLen)
	if off+nameBlobLen < off || off+nameBlobLen > len(data) {
		return nil, errors.New("invalid mapped name blob")
	}
	nameBlob := data[off : off+nameBlobLen]
	off += nameBlobLen
	tokenBytes := int(header.TokenCount) * 6
	if tokenBytes/6 != int(header.TokenCount) || off+tokenBytes < off || off+tokenBytes > len(data) {
		return nil, errors.New("invalid mapped name table")
	}
	tokenTable := data[off : off+tokenBytes]
	off += tokenBytes
	recordCount := int(header.EntryCount)
	wideRefs := header.Compact&compactDiskWideRefsFlag != 0
	idx.CompactAttrs = header.Compact&compactDiskAttrsFlag != 0
	recordBytes := compactDiskRecordBytesForCounts(recordCount, int(header.TokenCount))
	needRecordBytes := recordCount * recordBytes
	if recordBytes <= 0 || needRecordBytes/recordBytes != recordCount ||
		off+needRecordBytes < off || off+needRecordBytes > len(data) {
		return nil, errors.New("invalid mapped compact records")
	}
	mappedRecords := &MMapRecords{
		file:       mapped,
		wideRefs:   wideRefs,
		count:      recordCount,
		nameBlob:   nameBlob,
		tokenTable: tokenTable,
		recordData: data[off : off+needRecordBytes],
	}
	mappedRecords.scanCapabilities()
	idx.MMapRecords = mappedRecords
	if sectionTableOffset != 0 {
		idx.Derived = parseMappedDerivedSections(data, sectionTableOffset, int(header.EntryCount))
		mapped.derived = idx.Derived
	}
	return idx, nil
}

func parseMappedDerivedSections(data []byte, sectionTableOffset uint64, recordCount int) indexDerivedSections {
	var out indexDerivedSections
	if sectionTableOffset > uint64(len(data)) || sectionTableOffset+4 > uint64(len(data)) {
		return out
	}
	off := int(sectionTableOffset)
	count := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	const entrySize = 24
	if count < 0 || off+count*entrySize < off || off+count*entrySize > len(data) {
		return out
	}
	for i := 0; i < count; i++ {
		tag := binary.LittleEndian.Uint32(data[off:])
		sectionOff := binary.LittleEndian.Uint64(data[off+4:])
		length := binary.LittleEndian.Uint64(data[off+12:])
		off += entrySize
		if sectionOff > uint64(len(data)) || length > uint64(len(data)) || sectionOff+length < sectionOff || sectionOff+length > uint64(len(data)) {
			continue
		}
		section := data[int(sectionOff):int(sectionOff+length)]
		decodeDerivedSection(&out, tag, section, recordCount)
	}
	finalizeDerivedSections(&out)
	return out
}

func readDerivedSectionsFromReaderAt(ra io.ReaderAt, size int64, sectionTableOffset uint64, recordCount int) indexDerivedSections {
	var out indexDerivedSections
	if ra == nil || size < 0 || sectionTableOffset == 0 {
		return out
	}
	fileSize := uint64(size)
	if sectionTableOffset > fileSize || sectionTableOffset+4 < sectionTableOffset || sectionTableOffset+4 > fileSize {
		return out
	}
	var countBuf [4]byte
	if _, err := ra.ReadAt(countBuf[:], int64(sectionTableOffset)); err != nil {
		return out
	}
	count := int(binary.LittleEndian.Uint32(countBuf[:]))
	const entrySize = 24
	tableBytes := count * entrySize
	tableOff := sectionTableOffset + 4
	if count < 0 || tableBytes/entrySize != count || tableOff+uint64(tableBytes) < tableOff || tableOff+uint64(tableBytes) > fileSize {
		return out
	}
	table := make([]byte, tableBytes)
	if _, err := ra.ReadAt(table, int64(tableOff)); err != nil {
		return out
	}
	for off := 0; off < len(table); off += entrySize {
		tag := binary.LittleEndian.Uint32(table[off:])
		sectionOff := binary.LittleEndian.Uint64(table[off+4:])
		length := binary.LittleEndian.Uint64(table[off+12:])
		if sectionOff > fileSize || length > fileSize || sectionOff+length < sectionOff || sectionOff+length > fileSize {
			continue
		}
		if length > uint64(^uint(0)>>1) {
			continue
		}
		section := make([]byte, int(length))
		if _, err := ra.ReadAt(section, int64(sectionOff)); err != nil {
			continue
		}
		decodeDerivedSection(&out, tag, section, recordCount)
	}
	finalizeDerivedSections(&out)
	return out
}

func decodeDerivedSection(out *indexDerivedSections, tag uint32, section []byte, recordCount int) {
	if out == nil {
		return
	}
	switch tag {
	case indexSectionRANK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.NameOrder, out.NameRank = parts[0], parts[1]
		}
	case indexSectionSRNK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.SizeOrder, out.SizeRank = parts[0], parts[1]
		}
	case indexSectionMRNK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.ModOrder, out.ModRank = parts[0], parts[1]
		}
	case indexSectionERNK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.ExtOrder, out.ExtRank = parts[0], parts[1]
		}
	case indexSectionTRNK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.TypeOrder, out.TypeRank = parts[0], parts[1]
		}
	case indexSectionPRNK:
		parts := decodeUint32Section(section, 2)
		if len(parts) == 2 {
			out.PathOrder, out.PathRank = parts[0], parts[1]
		}
	case indexSectionCHLD:
		parts := decodeUint32Section(section, 3)
		if len(parts) == 3 {
			out.ChildOffsets, out.ChildIDs, out.RootIDs = parts[0], parts[1], parts[2]
		}
	case indexSectionSUBT:
		parts := decodeUint32Section(section, 8)
		if len(parts) == 8 {
			out.SubtreeStart, out.SubtreeEnd, out.SubtreeOrder = parts[0], parts[1], parts[2]
			out.SubtreeSizeRank, out.SubtreeModRank = parts[3], parts[4]
			out.SubtreeExtRank, out.SubtreeTypeRank = parts[5], parts[6]
			out.SubtreePathRank = parts[7]
			break
		}
		parts = decodeUint32Section(section, 3)
		if len(parts) == 3 {
			out.SubtreeStart, out.SubtreeEnd, out.SubtreeOrder = parts[0], parts[1], parts[2]
		}
	case indexSectionSUBS:
		out.SubtreeBytes = decodeUint64Section(section, 1)
	case indexSectionFRNS:
		frns, ids := decodeFRNSection(section)
		out.FRNs, out.FRNRecordIDs = frns, ids
	case indexSectionLOWR:
		out.LowerBlob, out.LowerOffs, out.LowerLens = decodeLowerSection(section)
	case indexSectionPATR:
		out.AttrBits = decodeAttrPostingSection(section)
	case indexSectionPEXT, indexSectionPCMP, indexSectionPNGR, indexSectionPNGC:
		if out.Postings == nil {
			out.Postings = make(map[uint32]mappedPostingSection)
		}
		out.Postings[tag] = decodePostingSection(section)
		if tag == indexSectionPNGR {
			out.NameTrigrams = decodeGramPostingIndex(section, recordCount)
		} else if tag == indexSectionPNGC {
			out.SelfNameTrigrams = decodeGramPostingIndex(section, recordCount)
		}
	case indexSectionPXRB:
		if out.PostingBounds == nil {
			out.PostingBounds = make(map[uint32]postingRankBounds)
		}
		out.PostingBounds[indexSectionPEXT] = decodePostingRankBounds(section)
	case indexSectionPXRC:
		if out.PostingBounds == nil {
			out.PostingBounds = make(map[uint32]postingRankBounds)
		}
		out.PostingBounds[indexSectionPCMP] = decodePostingRankBounds(section)
	}
}

func finalizeDerivedSections(out *indexDerivedSections) {
	if out == nil {
		return
	}
	for tag, bounds := range out.PostingBounds {
		if bounds.BlockCount <= 0 || out.Postings == nil {
			continue
		}
		if posting, ok := out.Postings[tag]; ok {
			posting.RankBounds = bounds
			out.Postings[tag] = posting
		}
	}
}

func decodeUint32Section(data []byte, parts int) [][]uint32 {
	out := make([][]uint32, 0, parts)
	off := 0
	for i := 0; i < parts; i++ {
		if off+4 > len(data) {
			return nil
		}
		n := int(binary.LittleEndian.Uint32(data[off:]))
		off += 4
		bytesLen := n * 4
		if n < 0 || bytesLen/4 != n || off+bytesLen < off || off+bytesLen > len(data) {
			return nil
		}
		values := mappedUint32Slice(data[off : off+bytesLen])
		off += bytesLen
		out = append(out, values)
	}
	if off != len(data) {
		return nil
	}
	return out
}

func decodeUint64Section(data []byte, parts int) []uint64 {
	if parts != 1 || len(data) == 0 || len(data)%8 != 0 {
		return nil
	}
	return mappedUint64Slice(data)
}

func decodeFRNSection(data []byte) ([]uint64, []uint32) {
	off := 0
	if off+4 > len(data) {
		return nil, nil
	}
	frnCount := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	frnBytes := frnCount * 8
	if frnCount < 0 || frnBytes/8 != frnCount || off+frnBytes < off || off+frnBytes > len(data) {
		return nil, nil
	}
	frns := mappedUint64Slice(data[off : off+frnBytes])
	off += frnBytes
	if off+4 > len(data) {
		return nil, nil
	}
	idCount := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	idBytes := idCount * 4
	if idCount < 0 || idBytes/4 != idCount || idCount != frnCount || off+idBytes < off || off+idBytes > len(data) {
		return nil, nil
	}
	ids := mappedUint32Slice(data[off : off+idBytes])
	return frns, ids
}

func decodeLowerSection(data []byte) ([]byte, []uint32, []uint16) {
	if len(data) < 8 {
		return nil, nil, nil
	}
	count := int(binary.LittleEndian.Uint32(data[0:]))
	blobLen := int(binary.LittleEndian.Uint32(data[4:]))
	off := 8
	offsBytes := count * 4
	if count < 0 || blobLen < 0 || offsBytes/4 != count || off+offsBytes < off || off+offsBytes > len(data) {
		return nil, nil, nil
	}
	offs := mappedUint32Slice(data[off : off+offsBytes])
	off += offsBytes
	lensBytes := count * 2
	if lensBytes/2 != count || off+lensBytes < off || off+lensBytes > len(data) {
		return nil, nil, nil
	}
	lens := mappedUint16Slice(data[off : off+lensBytes])
	off += lensBytes
	if off+blobLen < off || off+blobLen > len(data) {
		return nil, nil, nil
	}
	return data[off : off+blobLen], offs, lens
}

func mappedUint16Slice(data []byte) []uint16 {
	if len(data) == 0 {
		return nil
	}
	return unsafe.Slice((*uint16)(unsafe.Pointer(&data[0])), len(data)/2)
}

func mappedUint32Slice(data []byte) []uint32 {
	if len(data) == 0 {
		return nil
	}
	return unsafe.Slice((*uint32)(unsafe.Pointer(&data[0])), len(data)/4)
}

func mappedUint64Slice(data []byte) []uint64 {
	if len(data) == 0 {
		return nil
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&data[0])), len(data)/8)
}

func decodePostingSection(data []byte) mappedPostingSection {
	if len(data) < 16 {
		return mappedPostingSection{}
	}
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	keyBlobLen := int(binary.LittleEndian.Uint32(data[4:]))
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(data[12:]))
	if entryCount < 0 || keyBlobLen < 0 || blockCount < 0 || blockBlobLen < 0 {
		return mappedPostingSection{}
	}
	stringEntrySize := 20
	gramEntrySize := 16
	entrySize := stringEntrySize
	if keyBlobLen == 0 {
		entrySize = gramEntrySize
	}
	blockMetaSize := 28
	off := 16
	entriesBytes := entryCount * entrySize
	if entriesBytes/entrySize != entryCount || off+entriesBytes < off || off+entriesBytes > len(data) {
		return mappedPostingSection{}
	}
	off += entriesBytes
	blockMetaBytes := blockCount * blockMetaSize
	if blockMetaBytes/blockMetaSize != blockCount || off+blockMetaBytes < off || off+blockMetaBytes > len(data) {
		return mappedPostingSection{}
	}
	off += blockMetaBytes
	if off+keyBlobLen < off || off+keyBlobLen > len(data) {
		return mappedPostingSection{}
	}
	off += keyBlobLen
	if off+blockBlobLen < off || off+blockBlobLen > len(data) {
		return mappedPostingSection{}
	}
	section := mappedPostingSection{EntryCount: entryCount, BlockCount: blockCount, Bytes: len(data), Data: data}
	if keyBlobLen == 0 {
		return section
	}
	for i := 0; i < entryCount; i++ {
		entryOff := 16 + i*stringEntrySize
		keyOff := int(binary.LittleEndian.Uint32(data[entryOff:]))
		keyLen := int(binary.LittleEndian.Uint16(data[entryOff+4:]))
		firstBlock := int(binary.LittleEndian.Uint32(data[entryOff+12:]))
		entryBlockCount := int(binary.LittleEndian.Uint32(data[entryOff+16:]))
		if keyOff < 0 || keyLen < 0 || keyOff+keyLen < keyOff || keyOff+keyLen > keyBlobLen {
			return mappedPostingSection{}
		}
		if firstBlock < 0 || entryBlockCount < 0 || firstBlock+entryBlockCount < firstBlock || firstBlock+entryBlockCount > blockCount {
			return mappedPostingSection{}
		}
	}
	return section
}

func postingBlockCacheMaxBytes() int64 {
	mb := int64(128)
	if raw := strings.TrimSpace(os.Getenv("SEEKFS_POSTING_CACHE_MB")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			mb = parsed
		}
	}
	if mb <= 0 {
		return 0
	}
	const maxReasonableMB = 16 * 1024
	if mb > maxReasonableMB {
		mb = maxReasonableMB
	}
	return mb * 1024 * 1024
}

func (c *postingBlockLRU) get(key postingBlockCacheKey) ([]uint32, bool) {
	maxBytes := postingBlockCacheMaxBytes()
	if maxBytes <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		return nil, false
	}
	elem := c.items[key]
	if elem == nil {
		return nil, false
	}
	c.ll.MoveToFront(elem)
	entry := elem.Value.(*postingBlockCacheEntry)
	return entry.ids, true
}

func (c *postingBlockLRU) add(key postingBlockCacheKey, ids []uint32) {
	maxBytes := postingBlockCacheMaxBytes()
	if maxBytes <= 0 || len(ids) == 0 {
		return
	}
	entryBytes := int64(len(ids)) * int64(unsafe.Sizeof(uint32(0)))
	if entryBytes > maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[postingBlockCacheKey]*list.Element)
	}
	if elem := c.items[key]; elem != nil {
		c.ll.MoveToFront(elem)
		entry := elem.Value.(*postingBlockCacheEntry)
		c.bytes += entryBytes - entry.bytes
		entry.ids = ids
		entry.bytes = entryBytes
	} else {
		elem := c.ll.PushFront(&postingBlockCacheEntry{key: key, ids: ids, bytes: entryBytes})
		c.items[key] = elem
		c.bytes += entryBytes
	}
	for c.bytes > maxBytes {
		elem := c.ll.Back()
		if elem == nil {
			break
		}
		entry := elem.Value.(*postingBlockCacheEntry)
		delete(c.items, entry.key)
		c.bytes -= entry.bytes
		c.ll.Remove(elem)
	}
}

func (section mappedPostingSection) postingBlockCacheKey(blockIndex int) (postingBlockCacheKey, bool) {
	if len(section.Data) == 0 || blockIndex < 0 {
		return postingBlockCacheKey{}, false
	}
	return postingBlockCacheKey{
		base:  uintptr(unsafe.Pointer(unsafe.SliceData(section.Data))),
		bytes: len(section.Data),
		block: blockIndex,
	}, true
}

func (section mappedPostingSection) decodePostingBlock(blockIndex, blockMetaStart, blockBlobStart, blockBlobLen int) ([]uint32, bool) {
	const blockMetaSize = 28
	data := section.Data
	metaOff := blockMetaStart + blockIndex*blockMetaSize
	if metaOff < blockMetaStart || metaOff+blockMetaSize < metaOff || metaOff+blockMetaSize > len(data) {
		return nil, false
	}
	blockOffset := int(binary.LittleEndian.Uint64(data[metaOff:]))
	blockLength := int(binary.LittleEndian.Uint32(data[metaOff+8:]))
	blockCountValue := int(binary.LittleEndian.Uint32(data[metaOff+12:]))
	if blockOffset < 0 || blockLength < 0 || blockOffset+blockLength < blockOffset || blockOffset+blockLength > blockBlobLen {
		return nil, false
	}
	if key, ok := section.postingBlockCacheKey(blockIndex); ok {
		if cached, ok := servicePostingBlockCache.get(key); ok {
			return cached, true
		}
		encoded := data[blockBlobStart+blockOffset : blockBlobStart+blockOffset+blockLength]
		decoded := decodeDeltaUvarint32(encoded, blockCountValue)
		servicePostingBlockCache.add(key, decoded)
		return decoded, true
	}
	encoded := data[blockBlobStart+blockOffset : blockBlobStart+blockOffset+blockLength]
	return decodeDeltaUvarint32(encoded, blockCountValue), true
}

type postingBlockIterator struct {
	section        mappedPostingSection
	next           int
	end            int
	blockMetaStart int
	blockBlobStart int
	blockBlobLen   int
}

type postingBlockRankRef struct {
	index int
	meta  postingBlockMeta
}

func (it *postingBlockIterator) nextBlock() ([]uint32, postingBlockMeta, bool) {
	if it == nil || it.next >= it.end {
		return nil, postingBlockMeta{}, false
	}
	blockIndex := it.next
	it.next++
	return it.blockAt(blockIndex)
}

func (it postingBlockIterator) blockAt(blockIndex int) ([]uint32, postingBlockMeta, bool) {
	meta, ok := it.blockMetaAt(blockIndex)
	if !ok {
		return nil, postingBlockMeta{}, false
	}
	ids, ok := it.section.decodePostingBlock(blockIndex, it.blockMetaStart, it.blockBlobStart, it.blockBlobLen)
	return ids, meta, ok
}

func (it postingBlockIterator) blockMetaAt(blockIndex int) (postingBlockMeta, bool) {
	const blockMetaSize = 28
	if blockIndex < 0 || blockIndex >= it.end {
		return postingBlockMeta{}, false
	}
	metaOff := it.blockMetaStart + blockIndex*blockMetaSize
	data := it.section.Data
	if metaOff < it.blockMetaStart || metaOff+blockMetaSize < metaOff || metaOff+blockMetaSize > len(data) {
		return postingBlockMeta{}, false
	}
	meta := postingBlockMeta{
		offset:  binary.LittleEndian.Uint64(data[metaOff:]),
		length:  binary.LittleEndian.Uint32(data[metaOff+8:]),
		count:   binary.LittleEndian.Uint32(data[metaOff+12:]),
		minID:   binary.LittleEndian.Uint32(data[metaOff+16:]),
		maxID:   binary.LittleEndian.Uint32(data[metaOff+20:]),
		minRank: binary.LittleEndian.Uint32(data[metaOff+24:]),
	}
	return meta, true
}

// containsID is a bounded SeekGE-style membership check.  Gram postings are
// stored in record-ID order, so binary-searching block bounds decodes at most
// one block and never materializes the posting or its intersection.
func (it postingBlockIterator) containsID(target uint32) (found, valid bool, blockIndex int, decoded bool) {
	if it.next >= it.end {
		return false, true, -1, false
	}
	lo, hi := it.next, it.end
	for lo < hi {
		mid := lo + (hi-lo)/2
		meta, ok := it.blockMetaAt(mid)
		if !ok {
			return false, false, -1, false
		}
		if target <= meta.maxID {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	if lo >= it.end {
		return false, true, -1, false
	}
	meta, ok := it.blockMetaAt(lo)
	if !ok {
		return false, false, -1, false
	}
	if target < meta.minID || target > meta.maxID {
		return false, true, lo, false
	}
	ids, _, ok := it.blockAt(lo)
	if !ok {
		return false, false, lo, false
	}
	pos := sort.Search(len(ids), func(i int) bool { return ids[i] >= target })
	return pos < len(ids) && ids[pos] == target, true, lo, true
}

type postingPrefetchRange struct {
	start int
	end   int
}

func queryPostingPrefetchBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("SEEKFS_QUERY_POSTING_PREFETCH_BYTES"))
	if raw == "" {
		return defaultQueryPostingPrefetchBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return defaultQueryPostingPrefetchBytes
	}
	return value
}

// prefetchPostingBlockRefs touches only the selected posting payload ranges,
// in physical order, before rank-ordered evaluation.  It intentionally has a
// hard byte cap and no allocation proportional to the mapped section.
func prefetchPostingBlockRefs(it postingBlockIterator, refs []postingBlockRankRef, maxBytes int64, canceled func() bool) (bytes, ranges, pages int, stopped bool) {
	if maxBytes <= 0 || len(refs) == 0 || len(it.section.Data) == 0 {
		return 0, 0, 0, false
	}
	physical := make([]postingPrefetchRange, 0, len(refs))
	for _, ref := range refs {
		meta, ok := it.blockMetaAt(ref.index)
		if !ok || meta.length == 0 {
			continue
		}
		if meta.offset > uint64(it.blockBlobLen) {
			return 0, 0, 0, true
		}
		start := it.blockBlobStart + int(meta.offset)
		end := start + int(meta.length)
		if start < it.blockBlobStart || end < start || end > it.blockBlobStart+it.blockBlobLen || end > len(it.section.Data) {
			return 0, 0, 0, true
		}
		physical = append(physical, postingPrefetchRange{start: start, end: end})
	}
	sort.Slice(physical, func(i, j int) bool {
		if physical[i].start == physical[j].start {
			return physical[i].end < physical[j].end
		}
		return physical[i].start < physical[j].start
	})
	for _, r := range physical {
		if canceled != nil && canceled() {
			return bytes, ranges, pages, true
		}
		if r.end <= r.start || int64(bytes) >= maxBytes {
			break
		}
		end := r.end
		remaining := maxBytes - int64(bytes)
		if int64(end-r.start) > remaining {
			end = r.start + int(remaining)
		}
		if end <= r.start {
			break
		}
		ranges++
		for off, touched := r.start, 0; off < end; off, touched = off+4096, touched+1 {
			_ = it.section.Data[off]
			pages++
			if touched&255 == 255 && canceled != nil && canceled() {
				return bytes + end - r.start, ranges, pages, true
			}
		}
		bytes += end - r.start
	}
	return bytes, ranges, pages, false
}

func (it postingBlockIterator) rankOrderedBlockRefs() []postingBlockRankRef {
	if it.next >= it.end {
		return nil
	}
	refs := make([]postingBlockRankRef, 0, it.end-it.next)
	for blockIndex := it.next; blockIndex < it.end; blockIndex++ {
		meta, ok := it.blockMetaAt(blockIndex)
		if !ok {
			return nil
		}
		refs = append(refs, postingBlockRankRef{index: blockIndex, meta: meta})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].meta.minRank == refs[j].meta.minRank {
			return refs[i].meta.minID < refs[j].meta.minID
		}
		return refs[i].meta.minRank < refs[j].meta.minRank
	})
	return refs
}

func (it postingBlockIterator) rankOrderedBlockRefsForSort(sortColumn string) ([]postingBlockRankRef, bool) {
	if sortColumn == "" {
		bounds := it.section.RankBounds.ranksForSort("")
		if len(bounds) >= it.end {
			refs := make([]postingBlockRankRef, 0, it.end-it.next)
			for blockIndex := it.next; blockIndex < it.end; blockIndex++ {
				meta, ok := it.blockMetaAt(blockIndex)
				if !ok {
					return nil, false
				}
				meta.minRank = bounds[blockIndex]
				refs = append(refs, postingBlockRankRef{index: blockIndex, meta: meta})
			}
			sort.Slice(refs, func(i, j int) bool {
				if refs[i].meta.minRank == refs[j].meta.minRank {
					return refs[i].meta.minID < refs[j].meta.minID
				}
				return refs[i].meta.minRank < refs[j].meta.minRank
			})
			return refs, true
		}
		return it.rankOrderedBlockRefs(), true
	}
	bounds := it.section.RankBounds.ranksForSort(sortColumn)
	if len(bounds) < it.end {
		return it.rankOrderedBlockRefs(), false
	}
	refs := make([]postingBlockRankRef, 0, it.end-it.next)
	for blockIndex := it.next; blockIndex < it.end; blockIndex++ {
		meta, ok := it.blockMetaAt(blockIndex)
		if !ok {
			return nil, false
		}
		meta.minRank = bounds[blockIndex]
		refs = append(refs, postingBlockRankRef{index: blockIndex, meta: meta})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].meta.minRank == refs[j].meta.minRank {
			return refs[i].meta.minID < refs[j].meta.minID
		}
		return refs[i].meta.minRank < refs[j].meta.minRank
	})
	return refs, true
}

func (bounds postingRankBounds) ranksForSort(sortColumn string) []uint32 {
	if bounds.BlockCount <= 0 {
		return nil
	}
	switch sortColumn {
	case "size":
		return bounds.Size
	case "modified":
		return bounds.Modified
	case "extension":
		return bounds.Extension
	case "type":
		return bounds.Type
	case "path":
		return bounds.Path
	default:
		return bounds.Name
	}
}

func (section mappedPostingSection) stringPosting(key string) []uint32 {
	it, count, ok := section.stringPostingIterator(key)
	if !ok {
		return nil
	}
	return materializePostingBlockIterator(it, count)
}

// matchingStringPostingKeys scans the complete sorted key dictionary without
// touching any posting blocks.  Callers can then decode only the postings for
// keys whose component names contain the requested substring.
func (section mappedPostingSection) matchingStringPostingKeys(term string) ([]string, bool) {
	data := section.Data
	if len(data) < 16 || term == "" {
		return nil, false
	}
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	keyBlobLen := int(binary.LittleEndian.Uint32(data[4:]))
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(data[12:]))
	const stringEntrySize = 20
	const blockMetaSize = 28
	if entryCount <= 0 || keyBlobLen <= 0 || blockCount < 0 || blockBlobLen < 0 {
		return nil, false
	}
	entriesStart := 16
	entriesBytes := entryCount * stringEntrySize
	if entriesBytes/stringEntrySize != entryCount || entriesStart+entriesBytes < entriesStart || entriesStart+entriesBytes > len(data) {
		return nil, false
	}
	blockMetaStart := entriesStart + entriesBytes
	blockMetaBytes := blockCount * blockMetaSize
	if blockMetaBytes/blockMetaSize != blockCount || blockMetaStart+blockMetaBytes < blockMetaStart || blockMetaStart+blockMetaBytes > len(data) {
		return nil, false
	}
	keyBlobStart := blockMetaStart + blockMetaBytes
	blockBlobStart := keyBlobStart + keyBlobLen
	if blockBlobStart < keyBlobStart || blockBlobStart > len(data) || blockBlobStart+blockBlobLen < blockBlobStart || blockBlobStart+blockBlobLen > len(data) {
		return nil, false
	}
	term = strings.ToLower(term)
	keys := make([]string, 0, 8)
	for i := 0; i < entryCount; i++ {
		entryOff := entriesStart + i*stringEntrySize
		keyOff := int(binary.LittleEndian.Uint32(data[entryOff:]))
		keyLen := int(binary.LittleEndian.Uint16(data[entryOff+4:]))
		if keyOff < 0 || keyLen < 0 || keyOff+keyLen < keyOff || keyOff+keyLen > keyBlobLen {
			return nil, false
		}
		key := stringView(data[keyBlobStart+keyOff : keyBlobStart+keyOff+keyLen])
		if !strings.Contains(key, term) {
			continue
		}
		if _, _, ok := section.stringPostingIterator(key); !ok {
			return nil, false
		}
		keys = append(keys, key)
	}
	return keys, true
}

func (section mappedPostingSection) stringPostingIterator(key string) (postingBlockIterator, int, bool) {
	data := section.Data
	if key == "" || len(data) < 16 {
		return postingBlockIterator{}, 0, false
	}
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	keyBlobLen := int(binary.LittleEndian.Uint32(data[4:]))
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(data[12:]))
	if entryCount <= 0 || keyBlobLen <= 0 || blockCount < 0 || blockBlobLen < 0 {
		return postingBlockIterator{}, 0, false
	}
	const stringEntrySize = 20
	const blockMetaSize = 28
	entriesStart := 16
	entriesBytes := entryCount * stringEntrySize
	if entriesBytes/stringEntrySize != entryCount || entriesStart+entriesBytes < entriesStart || entriesStart+entriesBytes > len(data) {
		return postingBlockIterator{}, 0, false
	}
	blockMetaStart := entriesStart + entriesBytes
	blockMetaBytes := blockCount * blockMetaSize
	if blockMetaBytes/blockMetaSize != blockCount || blockMetaStart+blockMetaBytes < blockMetaStart || blockMetaStart+blockMetaBytes > len(data) {
		return postingBlockIterator{}, 0, false
	}
	keyBlobStart := blockMetaStart + blockMetaBytes
	blockBlobStart := keyBlobStart + keyBlobLen
	if blockBlobStart < keyBlobStart || blockBlobStart > len(data) || blockBlobStart+blockBlobLen < blockBlobStart || blockBlobStart+blockBlobLen > len(data) {
		return postingBlockIterator{}, 0, false
	}
	i := sort.Search(entryCount, func(i int) bool {
		entryOff := entriesStart + i*stringEntrySize
		keyOff := int(binary.LittleEndian.Uint32(data[entryOff:]))
		keyLen := int(binary.LittleEndian.Uint16(data[entryOff+4:]))
		if keyOff < 0 || keyLen < 0 || keyOff+keyLen < keyOff || keyOff+keyLen > keyBlobLen {
			return true
		}
		entryKey := stringView(data[keyBlobStart+keyOff : keyBlobStart+keyOff+keyLen])
		return entryKey >= key
	})
	if i >= entryCount {
		return postingBlockIterator{}, 0, false
	}
	entryOff := entriesStart + i*stringEntrySize
	keyOff := int(binary.LittleEndian.Uint32(data[entryOff:]))
	keyLen := int(binary.LittleEndian.Uint16(data[entryOff+4:]))
	if keyOff < 0 || keyLen < 0 || keyOff+keyLen < keyOff || keyOff+keyLen > keyBlobLen {
		return postingBlockIterator{}, 0, false
	}
	if stringView(data[keyBlobStart+keyOff:keyBlobStart+keyOff+keyLen]) != key {
		return postingBlockIterator{}, 0, false
	}
	count := int(binary.LittleEndian.Uint32(data[entryOff+8:]))
	firstBlock := int(binary.LittleEndian.Uint32(data[entryOff+12:]))
	entryBlockCount := int(binary.LittleEndian.Uint32(data[entryOff+16:]))
	if count <= 0 || firstBlock < 0 || entryBlockCount <= 0 || firstBlock+entryBlockCount < firstBlock || firstBlock+entryBlockCount > blockCount {
		return postingBlockIterator{}, 0, false
	}
	it := postingBlockIterator{
		section:        section,
		next:           firstBlock,
		end:            firstBlock + entryBlockCount,
		blockMetaStart: blockMetaStart,
		blockBlobStart: blockBlobStart,
		blockBlobLen:   blockBlobLen,
	}
	return it, count, true
}

func (section mappedPostingSection) gramPosting(gram uint32) []uint32 {
	it, count, ok := section.gramPostingIterator(gram)
	if !ok {
		return nil
	}
	return materializePostingBlockIterator(it, count)
}

func (section mappedPostingSection) gramPostingIterator(gram uint32) (postingBlockIterator, int, bool) {
	data := section.Data
	if len(data) < 16 {
		return postingBlockIterator{}, 0, false
	}
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	keyBlobLen := int(binary.LittleEndian.Uint32(data[4:]))
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(data[12:]))
	if entryCount <= 0 || keyBlobLen != 0 || blockCount < 0 || blockBlobLen < 0 {
		return postingBlockIterator{}, 0, false
	}
	const entrySize = 16
	const blockMetaSize = 28
	entriesStart := 16
	entriesBytes := entryCount * entrySize
	if entriesBytes/entrySize != entryCount || entriesStart+entriesBytes < entriesStart || entriesStart+entriesBytes > len(data) {
		return postingBlockIterator{}, 0, false
	}
	blockMetaStart := entriesStart + entriesBytes
	blockMetaBytes := blockCount * blockMetaSize
	if blockMetaBytes/blockMetaSize != blockCount || blockMetaStart+blockMetaBytes < blockMetaStart || blockMetaStart+blockMetaBytes > len(data) {
		return postingBlockIterator{}, 0, false
	}
	blockBlobStart := blockMetaStart + blockMetaBytes
	if blockBlobStart < blockMetaStart || blockBlobStart > len(data) || blockBlobStart+blockBlobLen < blockBlobStart || blockBlobStart+blockBlobLen > len(data) {
		return postingBlockIterator{}, 0, false
	}
	i := sort.Search(entryCount, func(i int) bool {
		entryOff := entriesStart + i*entrySize
		return binary.LittleEndian.Uint32(data[entryOff:]) >= gram
	})
	if i >= entryCount {
		return postingBlockIterator{}, 0, false
	}
	entryOff := entriesStart + i*entrySize
	if binary.LittleEndian.Uint32(data[entryOff:]) != gram {
		return postingBlockIterator{}, 0, false
	}
	count := int(binary.LittleEndian.Uint32(data[entryOff+4:]))
	firstBlock := int(binary.LittleEndian.Uint32(data[entryOff+8:]))
	entryBlockCount := int(binary.LittleEndian.Uint32(data[entryOff+12:]))
	if count <= 0 || firstBlock < 0 || entryBlockCount <= 0 || firstBlock+entryBlockCount < firstBlock || firstBlock+entryBlockCount > blockCount {
		return postingBlockIterator{}, 0, false
	}
	it := postingBlockIterator{
		section:        section,
		next:           firstBlock,
		end:            firstBlock + entryBlockCount,
		blockMetaStart: blockMetaStart,
		blockBlobStart: blockBlobStart,
		blockBlobLen:   blockBlobLen,
	}
	return it, count, true
}

func decodeGramPostingIndex(data []byte, recordCount int) *compressedTrigramIndex {
	if len(data) < 16 {
		return nil
	}
	entryCount := int(binary.LittleEndian.Uint32(data[0:]))
	rawKeyBlobLen := binary.LittleEndian.Uint32(data[4:])
	omittedCount := 0
	keyBlobLen := int(rawKeyBlobLen)
	hasMetadata := false
	unionComplete := false
	blockCount := int(binary.LittleEndian.Uint32(data[8:]))
	blockBlobLen := int(binary.LittleEndian.Uint32(data[12:]))
	if keyBlobLen != 0 || blockCount < 0 || blockBlobLen < 0 {
		return nil
	}
	const entrySize = 16
	const blockMetaSize = 28
	entriesStart := 16
	entriesBytes := entryCount * entrySize
	if entriesBytes/entrySize != entryCount || entriesStart+entriesBytes > len(data) {
		return nil
	}
	blockMetaStart := entriesStart + entriesBytes
	blockMetaBytes := blockCount * blockMetaSize
	if blockMetaBytes/blockMetaSize != blockCount || blockMetaStart+blockMetaBytes < blockMetaStart || blockMetaStart+blockMetaBytes > len(data) {
		return nil
	}
	blockBlobStart := blockMetaStart + blockMetaBytes
	if blockBlobStart+blockBlobLen < blockBlobStart || blockBlobStart+blockBlobLen > len(data) {
		return nil
	}
	metadataStart := blockBlobStart + blockBlobLen
	if len(data)-metadataStart >= 8 && (binary.LittleEndian.Uint32(data[metadataStart:]) == gramPostingMetadataMagic || binary.LittleEndian.Uint32(data[metadataStart:]) == gramPostingUnionMetadataMagic) {
		hasMetadata = true
		unionComplete = binary.LittleEndian.Uint32(data[metadataStart:]) == gramPostingUnionMetadataMagic
		omittedCount = int(binary.LittleEndian.Uint32(data[metadataStart+4:]))
	}
	metadataBytes := 0
	if hasMetadata {
		if omittedCount < 0 || omittedCount > (len(data)-metadataStart-8)/8 {
			return nil
		}
		metadataBytes = 8 + omittedCount*8
	}
	if !hasMetadata && entryCount <= 0 {
		return nil
	}
	if metadataStart+metadataBytes < metadataStart || metadataStart+metadataBytes > len(data) || (hasMetadata && metadataStart+metadataBytes != len(data)) {
		return nil
	}
	ti := &compressedTrigramIndex{
		counts:             make(map[uint32]int, entryCount),
		gramCountsComplete: hasMetadata,
		gramUnionComplete:  unionComplete,
		gramSize:           3,
		recordCount:        recordCount,
		mappedGrams:        &mappedPostingSection{EntryCount: entryCount, BlockCount: blockCount, Bytes: len(data), Data: data},
	}
	if omittedCount > 0 {
		ti.omitted = make(map[uint32]struct{}, omittedCount)
		for i := 0; i < omittedCount; i++ {
			off := metadataStart + 8 + i*8
			gram := binary.LittleEndian.Uint32(data[off:])
			if i > 0 && gram <= binary.LittleEndian.Uint32(data[off-8:]) {
				return nil
			}
			ti.omitted[gram] = struct{}{}
			ti.counts[gram] = int(binary.LittleEndian.Uint32(data[off+4:]))
		}
	}
	for i := 0; i < entryCount; i++ {
		entryOff := entriesStart + i*entrySize
		gram := binary.LittleEndian.Uint32(data[entryOff:])
		count := int(binary.LittleEndian.Uint32(data[entryOff+4:]))
		firstBlock := int(binary.LittleEndian.Uint32(data[entryOff+8:]))
		entryBlockCount := int(binary.LittleEndian.Uint32(data[entryOff+12:]))
		if count <= 0 || firstBlock < 0 || entryBlockCount <= 0 || firstBlock+entryBlockCount < firstBlock || firstBlock+entryBlockCount > blockCount {
			continue
		}
		for blockIndex := firstBlock; blockIndex < firstBlock+entryBlockCount; blockIndex++ {
			metaOff := blockMetaStart + blockIndex*blockMetaSize
			blockOffset := int(binary.LittleEndian.Uint64(data[metaOff:]))
			blockLength := int(binary.LittleEndian.Uint32(data[metaOff+8:]))
			if blockOffset < 0 || blockLength < 0 || blockOffset+blockLength < blockOffset || blockOffset+blockLength > blockBlobLen {
				return nil
			}
		}
		ti.counts[gram] = count
	}
	if len(ti.counts) == 0 && len(ti.omitted) == 0 {
		return nil
	}
	return ti
}

func mappedReadString(data []byte, off int) (string, int, error) {
	if off < 0 || off+4 < off || off+4 > len(data) {
		return "", off, errors.New("invalid mapped string length")
	}
	n := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	if off+n < off || off+n > len(data) {
		return "", off, errors.New("invalid mapped string")
	}
	return string(data[off : off+n]), off + n, nil
}

func loadConfig(path string) (appConfig, error) {
	if path == "" {
		path = findDefaultConfig()
	}
	if path == "" {
		return appConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return appConfig{}, err
	}
	cfg := appConfig{}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = parseContentConfigSection(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if section != "" {
			if section != "ignore" {
				applyContentScopeConfig(&cfg, section, key, value)
			}
			continue
		}
		switch key {
		case "db", "db_path":
			if s := parseTOMLString(value); s != "" {
				cfg.DBs = append(cfg.DBs, s)
			}
		case "dbs", "db_paths":
			cfg.DBs = append(cfg.DBs, parseTOMLStringArray(value)...)
		case "volume":
			if s := parseTOMLString(value); s != "" {
				cfg.Volumes = append(cfg.Volumes, s)
			}
		case "volumes":
			cfg.Volumes = append(cfg.Volumes, parseTOMLStringArray(value)...)
		case "service_pipe":
			cfg.ServicePipe = parseTOMLString(value)
		case "output_format":
			cfg.OutputFormat = strings.ToLower(parseTOMLString(value))
		case "default_limit":
			var n int
			if _, err := fmt.Sscanf(value, "%d", &n); err == nil {
				cfg.DefaultLimit = n
			}
		case "seekfs_dir":
			cfg.SeekFSDir = parseTOMLString(value)
		case "remote_addr":
			cfg.RemoteAddr = parseTOMLString(value)
		case "content_scope":
			cfg.Content.Mode = parseTOMLString(value)
		case "content_roots":
			cfg.Content.Roots = append(cfg.Content.Roots, parseTOMLStringArray(value)...)
		case "content_exclude":
			cfg.Content.Exclude = append(cfg.Content.Exclude, parseTOMLStringArray(value)...)
		case "content_budget_bytes":
			var n int64
			if _, err := fmt.Sscanf(value, "%d", &n); err == nil {
				cfg.Content.BudgetBytes = n
			}
		}
	}
	return cfg, nil
}
