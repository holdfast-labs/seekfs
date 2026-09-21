package main

// The `.gsx` content index: a sidecar next to the v9 `.gsi`. It is deliberately
// separate from the record index because content is keyed on FRN (stable across
// record-ID churn) and rebuilds on a different cadence. This file defines the
// container (header + section table) and the document table; P1 adds the term,
// trigram, text-store, rank and delta sections over the same container.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// contentIndexMagic identifies a `.gsx` file. The trailing digits are the
// format version encoded in the magic as well as the header, matching the v9
// habit. The 02 bump added the content USN checkpoint (WP0); the 03 bump is
// PF-4's encoding breadth + size policy, which changes normalized text and the
// policy section. A sidecar whose version does not match is rejected on load,
// so it can never be attached stale: the service sees no usable base and its
// PF-3 build path rebuilds it.
var contentIndexMagic = [8]byte{'G', 'O', 'S', 'C', 'X', '0', '0', '3'}

const contentIndexVersion = 3

// contentOrigin records what the doc keys are, so a `.gsx` can never be
// attached to a key space it does not join.
const (
	contentOriginUnknown uint32 = 0
	contentOriginWalk    uint32 = 1 // doc key = FNV-1a hash of the relative path
	contentOriginUSN     uint32 = 2 // doc key = the NTFS FRN of the record
)

// contentIndexMaxSection bounds one decoded section allocation.
const contentIndexMaxSection = 8 << 30

// contentGSXMaxBytes caps the encoded `.gsx` sidecar (WP10/M7). It is generous
// so it does not bite in normal use; tests lower it. A var so it can be tuned
// without a format change. An over-cap index is refused, never truncated.
var contentGSXMaxBytes int64 = 4 << 30

// contentGSXSizeError is returned by contentSaveFile when an index's encoded
// size exceeds contentGSXMaxBytes. Callers surface it as degraded and keep the
// previous base (and delta) instead of publishing a truncated sidecar, so no
// acknowledged content is lost and the condition is never silent.
type contentGSXSizeError struct {
	size int64
	cap  int64
}

func (e *contentGSXSizeError) Error() string {
	return fmt.Sprintf("content index exceeds size cap (%d bytes > %d)", e.size, e.cap)
}

// Content section tags. Same 4-byte tag discipline as the v9 index.
const (
	contentSectionDocTable uint32 = 'C'<<24 | 'X'<<16 | 'D'<<8 | 'T'
	contentSectionTerms    uint32 = 'C'<<24 | 'X'<<16 | 'T'<<8 | 'R'
	contentSectionGrams    uint32 = 'C'<<24 | 'X'<<16 | 'G'<<8 | 'R'
	contentSectionText     uint32 = 'C'<<24 | 'X'<<16 | 'S'<<8 | 'T'
	contentSectionRank     uint32 = 'C'<<24 | 'X'<<16 | 'R'<<8 | 'N'
	contentSectionDelta    uint32 = 'C'<<24 | 'X'<<16 | 'D'<<8 | 'X'
	contentSectionPaths    uint32 = 'C'<<24 | 'X'<<16 | 'P'<<8 | 'T'
	contentSectionPolicy   uint32 = 'C'<<24 | 'X'<<16 | 'P'<<8 | 'L'
)

// contentBuildPolicy is the extraction policy a `.gsx` was built with and the
// build's skip/truncation counts, stored in the CXPL section. It makes the
// size/encoding policy visible to the service's content health instead of a
// silent drop.
type contentBuildPolicy struct {
	MaxRaw    int64
	MaxText   int64
	Skipped   int64
	Truncated int64
}

const contentPolicySize = 32

func (p contentBuildPolicy) encode() []byte {
	b := make([]byte, contentPolicySize)
	binary.LittleEndian.PutUint64(b[0:], uint64(p.MaxRaw))
	binary.LittleEndian.PutUint64(b[8:], uint64(p.MaxText))
	binary.LittleEndian.PutUint64(b[16:], uint64(p.Skipped))
	binary.LittleEndian.PutUint64(b[24:], uint64(p.Truncated))
	return b
}

func decodeContentPolicy(data []byte) (contentBuildPolicy, bool) {
	if len(data) < contentPolicySize {
		return contentBuildPolicy{}, false
	}
	return contentBuildPolicy{
		MaxRaw:    int64(binary.LittleEndian.Uint64(data[0:])),
		MaxText:   int64(binary.LittleEndian.Uint64(data[8:])),
		Skipped:   int64(binary.LittleEndian.Uint64(data[16:])),
		Truncated: int64(binary.LittleEndian.Uint64(data[24:])),
	}, true
}

// contentHashLen is the width of the content hash stored per document.
const contentHashLen = 16

// contentDoc is one indexed document: the extracted text of a file, keyed on a
// stable identity (FRN for USN volumes, a path hash for walk volumes).
//
// v1 keeps exactly one document per FRN. DocID must equal the document's index
// in the FRN-sorted table; decode enforces it. Cross-file content dedup is
// deferred (see the plan), so ContentHash is change detection, not a shared-doc
// key, and the resolver's docID -> recordID map is unambiguous.
type contentDoc struct {
	DocID            uint32
	FRN              uint64
	ContentType      uint16 // contentClass* discriminator
	ExtractorVersion uint16
	ContentHash      [contentHashLen]byte
	RawSize          int64
	DocLen           uint32 // token count, for scoring
	ModUnix          int64
	TextOff          uint64 // offset into the CXST text store
	TextLen          uint32
}

// contentIndex is the in-memory form of a `.gsx`. Sections map tag -> payload.
// P1 populates terms/grams/text/rank/delta; P0 round-trips the container and
// doc table.
type contentIndex struct {
	Version int
	Origin  uint32
	// JournalID and CheckpointUSN are the content USN watermark (WP0): the
	// NTFS journal generation and the next-USN the base is known complete at.
	// A zero CheckpointUSN means the base is not USN-valid (a walk/path-keyed
	// base); the service never attaches such a base as FRN-joinable. Restart
	// catch-up resumes from CheckpointUSN, so a base ahead of the volume
	// checkpoint needs no work.
	JournalID     uint64
	CheckpointUSN uint64
	BuiltAt       time.Time
	Docs          []contentDoc
	Sections      map[uint32][]byte
	// Policy is the build's extraction policy + skip/truncation counts (CXPL).
	Policy contentBuildPolicy
	// EncodedSize is the encoded `.gsx` byte size: set by decode from the file
	// length and by encode from the produced bytes. Not persisted; surfaced in
	// content health so sidecar growth is observable (WP10/M7).
	EncodedSize int64
}

func newContentIndex() *contentIndex {
	return &contentIndex{
		Version:  contentIndexVersion,
		BuiltAt:  time.Now(),
		Sections: make(map[uint32][]byte),
	}
}

// contentDocSortByFRN orders the doc table by FRN, which is the order the
// resolver join and bisection rely on.
func contentDocSortByFRN(docs []contentDoc) {
	sort.Slice(docs, func(i, j int) bool { return docs[i].FRN < docs[j].FRN })
}

// contentDocTableBytes encodes the doc table as CXDT: a count followed by the
// fixed-width records, in FRN order.
func contentDocTableBytes(docs []contentDoc) []byte {
	var buf bytes.Buffer
	buf.Grow(4 + len(docs)*contentDocFixedSize)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(docs)))
	for i := range docs {
		d := &docs[i]
		_ = binary.Write(&buf, binary.LittleEndian, d.DocID)
		_ = binary.Write(&buf, binary.LittleEndian, d.FRN)
		_ = binary.Write(&buf, binary.LittleEndian, d.ContentType)
		_ = binary.Write(&buf, binary.LittleEndian, d.ExtractorVersion)
		buf.Write(d.ContentHash[:])
		_ = binary.Write(&buf, binary.LittleEndian, d.RawSize)
		_ = binary.Write(&buf, binary.LittleEndian, d.DocLen)
		_ = binary.Write(&buf, binary.LittleEndian, d.ModUnix)
		_ = binary.Write(&buf, binary.LittleEndian, d.TextOff)
		_ = binary.Write(&buf, binary.LittleEndian, d.TextLen)
	}
	return buf.Bytes()
}

// contentDocFixedSize is the encoded width of one doc record:
// docID(4)+frn(8)+type(2)+extractor(2)+hash(16)+rawSize(8)+docLen(4)+modUnix(8)+textOff(8)+textLen(4)
const contentDocFixedSize = 64

func contentDecodeDocTable(data []byte) ([]contentDoc, error) {
	if len(data) < 4 {
		return nil, errors.New("content doc table truncated")
	}
	n := int(binary.LittleEndian.Uint32(data[:4]))
	if n < 0 || 4+n*contentDocFixedSize > len(data) {
		return nil, errors.New("content doc table length mismatch")
	}
	docs := make([]contentDoc, n)
	off := 4
	for i := 0; i < n; i++ {
		d := &docs[i]
		d.DocID = binary.LittleEndian.Uint32(data[off:])
		d.FRN = binary.LittleEndian.Uint64(data[off+4:])
		d.ContentType = binary.LittleEndian.Uint16(data[off+12:])
		d.ExtractorVersion = binary.LittleEndian.Uint16(data[off+14:])
		copy(d.ContentHash[:], data[off+16:off+16+contentHashLen])
		p := off + 16 + contentHashLen
		d.RawSize = int64(binary.LittleEndian.Uint64(data[p:]))
		d.DocLen = binary.LittleEndian.Uint32(data[p+8:])
		d.ModUnix = int64(binary.LittleEndian.Uint64(data[p+12:]))
		d.TextOff = binary.LittleEndian.Uint64(data[p+20:])
		d.TextLen = binary.LittleEndian.Uint32(data[p+28:])
		off += contentDocFixedSize
	}
	return docs, nil
}

// contentEncode serializes the whole `.gsx`: header, section payloads, then the
// section table. Offsets are relative to the start of the file.
func contentIndexEncode(idx *contentIndex) []byte {
	idx.Sections[contentSectionDocTable] = contentDocTableBytes(idx.Docs)
	if idx.Policy != (contentBuildPolicy{}) {
		idx.Sections[contentSectionPolicy] = idx.Policy.encode()
	}

	var body bytes.Buffer
	headerSize := contentHeaderSize()
	body.Grow(headerSize + 64)
	// Reserve the header; patched below once the table offset is known.
	body.Write(make([]byte, headerSize))
	for _, tag := range contentSectionOrder(idx.Sections) {
		body.Write(idx.Sections[tag])
	}
	tableOffset := uint64(body.Len())

	tags := contentSectionOrder(idx.Sections)
	table := make([]byte, 4+len(tags)*contentSectionEntrySize)
	binary.LittleEndian.PutUint32(table[:4], uint32(len(tags)))
	entries := make([]contentSectionEntry, 0, len(tags))
	off := uint64(headerSize)
	for _, tag := range tags {
		data := idx.Sections[tag]
		entries = append(entries, contentSectionEntry{tag: tag, offset: off, length: uint64(len(data))})
		off += uint64(len(data))
	}
	for i, e := range entries {
		p := 4 + i*contentSectionEntrySize
		binary.LittleEndian.PutUint32(table[p:], e.tag)
		binary.LittleEndian.PutUint64(table[p+4:], e.offset)
		binary.LittleEndian.PutUint64(table[p+12:], e.length)
		binary.LittleEndian.PutUint32(table[p+20:], e.flags)
	}
	body.Write(table)

	out := body.Bytes()
	contentWriteHeader(out[:headerSize], idx, tableOffset, len(tags))
	idx.EncodedSize = int64(len(out))
	return out
}

type contentSectionEntry struct {
	tag    uint32
	offset uint64
	length uint64
	flags  uint32
}

const contentSectionEntrySize = 28 // tag(4)+offset(8)+length(8)+flags(4)+pad(4)

// contentHeaderSize is magic(8)+version(4)+flags(4)+docCount(4)+pad(4)+built(8)+tableOffset(8)+sectionCount(4)+journalID(8)+checkpointUSN(8) = 60.
func contentHeaderSize() int { return 60 }

func contentWriteHeader(b []byte, idx *contentIndex, tableOffset uint64, sectionCount int) {
	copy(b[0:8], contentIndexMagic[:])
	binary.LittleEndian.PutUint32(b[8:], uint32(idx.Version))
	binary.LittleEndian.PutUint32(b[12:], idx.Origin)
	binary.LittleEndian.PutUint32(b[16:], uint32(len(idx.Docs)))
	binary.LittleEndian.PutUint32(b[20:], 0) // pad
	binary.LittleEndian.PutUint64(b[24:], uint64(idx.BuiltAt.UnixNano()))
	binary.LittleEndian.PutUint64(b[32:], tableOffset)
	binary.LittleEndian.PutUint32(b[40:], uint32(sectionCount))
	binary.LittleEndian.PutUint64(b[44:], idx.JournalID)
	binary.LittleEndian.PutUint64(b[52:], idx.CheckpointUSN)
}

// contentSectionOrder returns a deterministic tag order (sorted), so encoding
// the same index twice yields identical bytes.
func contentSectionOrder(sections map[uint32][]byte) []uint32 {
	tags := make([]uint32, 0, len(sections))
	for tag := range sections {
		tags = append(tags, tag)
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i] < tags[j] })
	return tags
}

// contentDecode parses a `.gsx`. The doc table is decoded eagerly; other
// sections are handed back as slices into a private copy.
func contentIndexDecode(data []byte) (*contentIndex, error) {
	if len(data) < contentHeaderSize() {
		return nil, errors.New("content index truncated")
	}
	if !bytes.Equal(data[0:8], contentIndexMagic[:]) {
		return nil, errors.New("unsupported content index format")
	}
	version := int(binary.LittleEndian.Uint32(data[8:]))
	if version != contentIndexVersion {
		return nil, fmt.Errorf("unsupported content index version %d", version)
	}
	docCount := int(binary.LittleEndian.Uint32(data[16:]))
	built := int64(binary.LittleEndian.Uint64(data[24:]))
	tableOffset := binary.LittleEndian.Uint64(data[32:])
	sectionCount := int(binary.LittleEndian.Uint32(data[40:]))
	journalID := binary.LittleEndian.Uint64(data[44:])
	checkpointUSN := binary.LittleEndian.Uint64(data[52:])
	if tableOffset > uint64(len(data)) || sectionCount < 0 {
		return nil, errors.New("content index section table out of range")
	}
	tableLen := 4 + sectionCount*contentSectionEntrySize
	if tableOffset+uint64(tableLen) > uint64(len(data)) {
		return nil, errors.New("content index section table truncated")
	}
	idx := &contentIndex{
		Version:       version,
		Origin:        binary.LittleEndian.Uint32(data[12:]),
		JournalID:     journalID,
		CheckpointUSN: checkpointUSN,
		BuiltAt:       time.Unix(0, built),
		Sections:      make(map[uint32][]byte, sectionCount),
		EncodedSize:   int64(len(data)),
	}
	for i := 0; i < sectionCount; i++ {
		p := int(tableOffset) + 4 + i*contentSectionEntrySize
		tag := binary.LittleEndian.Uint32(data[p:])
		off := binary.LittleEndian.Uint64(data[p+4:])
		length := binary.LittleEndian.Uint64(data[p+12:])
		if length > contentIndexMaxSection || off+length > uint64(len(data)) {
			return nil, fmt.Errorf("content section %08x out of range", tag)
		}
		idx.Sections[tag] = data[off : off+length]
	}
	if sec, ok := idx.Sections[contentSectionPolicy]; ok {
		if p, ok := decodeContentPolicy(sec); ok {
			idx.Policy = p
		}
	}
	table, ok := idx.Sections[contentSectionDocTable]
	if !ok {
		return nil, errors.New("content index missing doc table")
	}
	docs, err := contentDecodeDocTable(table)
	if err != nil {
		return nil, err
	}
	if docCount != len(docs) {
		return nil, errors.New("content index doc count mismatch")
	}
	for i := range docs {
		if docs[i].DocID != uint32(i) {
			return nil, errors.New("content index doc ID does not match its position")
		}
	}
	idx.Docs = docs
	return idx, nil
}

// contentLookupFRN binary-searches the FRN-sorted doc table.
func contentLookupFRN(docs []contentDoc, frn uint64) (int, bool) {
	i := sort.Search(len(docs), func(i int) bool { return docs[i].FRN >= frn })
	if i < len(docs) && docs[i].FRN == frn {
		return i, true
	}
	return 0, false
}

// contentSaveFile writes the index to path atomically (temp + fsync + rename),
// so a crash never leaves a half-written sidecar. An encoded index over
// contentGSXMaxBytes is refused with contentGSXSizeError: nothing is written and
// the caller keeps the previous base (and delta) rather than a truncated sidecar.
func contentSaveFile(path string, idx *contentIndex) error {
	encoded := contentIndexEncode(idx)
	if contentGSXMaxBytes > 0 && int64(len(encoded)) > contentGSXMaxBytes {
		return &contentGSXSizeError{size: int64(len(encoded)), cap: contentGSXMaxBytes}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// contentLoadFile reads and decodes a `.gsx`.
func contentLoadFile(path string) (*contentIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return contentIndexDecode(data)
}
