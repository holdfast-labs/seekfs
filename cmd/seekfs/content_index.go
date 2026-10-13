package main

// The `.gsx` content index: a sidecar next to the v9 `.gsi`. It is deliberately
// separate from the record index because content is keyed on FRN (stable across
// record-ID churn) and rebuilds on a different cadence. This file defines the
// container (header + section table) and the document table; P1 adds the term,
// trigram, text-store, rank and delta sections over the same container.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

// contentSectionFlagCRC marks a section-table entry whose trailing word holds
// a CRC32C (Castagnoli) of the section payload. The flag rides the entry's
// 4-byte flags field and the CRC its 4-byte padding word, so the format version
// stays 4: older binaries ignore both words and keep loading, while a new
// binary treats a missing flag as a legacy (pre-checksum) section that loads
// and is upgraded on the next build.
const contentSectionFlagCRC uint32 = 1

// contentSectionCRCModel is the Castagnoli polynomial: hardware-accelerated on
// modern CPUs via hash/crc32.
var contentSectionCRCModel = crc32.MakeTable(crc32.Castagnoli)

// contentSectionCRC returns the integrity checksum of one section payload.
func contentSectionCRC(data []byte) uint32 {
	return crc32.Checksum(data, contentSectionCRCModel)
}

// contentSectionCRCError reports a section payload whose bytes do not match
// the checksum in the section table: a flipped bit (or a torn write the
// atomic rename did not prevent) rather than a structural fault.
type contentSectionCRCError struct {
	tag  uint32
	want uint32
	got  uint32
}

func (e *contentSectionCRCError) Error() string {
	return fmt.Sprintf("content section %s checksum mismatch (want %08x, got %08x)", contentSectionTagName(e.tag), e.want, e.got)
}

// contentSectionTagName renders a section tag for diagnostics.
func contentSectionTagName(tag uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], tag)
	for i := range b {
		if b[i] < 32 || b[i] > 126 {
			return fmt.Sprintf("%08x", tag)
		}
	}
	return string(b[:])
}

// contentSectionEagerVerify reports whether a section's checksum is verified
// immediately on load. Small sections (doc table, paths, policy, scope,
// manifest) are hashed at decode; the large text/term/gram sections are
// verified by a background pass after attach so startup does not page in
// gigabytes. Any other checksummed section is small by construction and
// verifies eagerly.
func contentSectionEagerVerify(tag uint32) bool {
	switch tag {
	case contentSectionText, contentSectionTerms, contentSectionGrams:
		return false
	default:
		return true
	}
}

// contentIndexMagic identifies a `.gsx` file. The trailing digits are the
// format version encoded in the magic as well as the header, matching the v9
// habit. The 02 bump added the content USN checkpoint (WP0); the 03 bump is
// PF-4's encoding breadth + size policy, which changes normalized text and the
// policy section; the 04 bump (PF-6a) makes the text store case-preserving (the
// index stays case-folded), so a v3 text store must not be read as if it were
// case-preserving; the 06 bump (walk volumes) widens each doc record with the
// source file's size and nanosecond mtime, which the walk reconcile diffs
// against the filename index for change detection without a journal; the 07
// bump (compact index) replaces the raw CXST text store with blocked zstd,
// drops the CXTR term postings to a front-coded dictionary, and stores CXGR
// gram postings as docIDs only with bitmap dense blocks (see
// content_textblock.go, content_termsdict.go and content_postings.go); the
// 08 bump shrinks text blocks to 64 KiB and gives each shard a trained
// zstd dictionary (CXTD, raw content; see content_textdict.go) that its
// frames reference. A sidecar whose version does not match is rejected on
// load, so it can never be attached stale: the service sees no usable base
// and its PF-3 build path rebuilds it. (05 is the sharded-manifest version,
// which carries no doc table; manifests are unaffected by the 06/07/08
// bumps: a manifest names shard files, and each shard carries the doc-table
// version itself.)
var contentIndexMagic = [8]byte{'G', 'O', 'S', 'C', 'X', '0', '0', '4'}

const contentIndexVersion = 8

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
	contentSectionScope    uint32 = 'C'<<24 | 'X'<<16 | 'S'<<8 | 'C'
)

// contentBuildPolicy is the extraction policy a `.gsx` was built with and the
// build's skip/truncation counts, stored in the CXPL section. It makes the
// size/encoding policy visible to the service's content health instead of a
// silent drop. ScopeDropped/ScopeDroppedBytes are the exact files and
// extracted-text bytes the scope budget excluded; BudgetBytes is the explicit
// budget that was enforced (0 when the default reserve-governed policy
// applied), and ReserveBytes is the free-space reserve that bit instead (0
// when an explicit budget or nothing bit), so health can name the operator
// action either way.
type contentBuildPolicy struct {
	MaxRaw            int64
	MaxText           int64
	Skipped           int64
	Truncated         int64
	ScopeDropped      int64
	ScopeDroppedBytes int64
	BudgetBytes       int64
	// Binary counts files that sniffed as binary (NUL in the head window):
	// expected, ripgrep-agreeing rejections, tallied apart from Skipped.
	Binary int64
	// ReserveBytes is the volume free-space reserve enforced when the
	// default policy stopped selection (see contentScopeReserveFor). v7
	// sections carry 72 bytes; older 64-byte sections decode with 0.
	ReserveBytes int64
}

const contentPolicySize = 72

func (p contentBuildPolicy) encode() []byte {
	b := make([]byte, contentPolicySize)
	binary.LittleEndian.PutUint64(b[0:], uint64(p.MaxRaw))
	binary.LittleEndian.PutUint64(b[8:], uint64(p.MaxText))
	binary.LittleEndian.PutUint64(b[16:], uint64(p.Skipped))
	binary.LittleEndian.PutUint64(b[24:], uint64(p.Truncated))
	binary.LittleEndian.PutUint64(b[32:], uint64(p.ScopeDropped))
	binary.LittleEndian.PutUint64(b[40:], uint64(p.ScopeDroppedBytes))
	binary.LittleEndian.PutUint64(b[48:], uint64(p.BudgetBytes))
	binary.LittleEndian.PutUint64(b[56:], uint64(p.Binary))
	binary.LittleEndian.PutUint64(b[64:], uint64(p.ReserveBytes))
	return b
}

func decodeContentPolicy(data []byte) (contentBuildPolicy, bool) {
	if len(data) < 32 {
		return contentBuildPolicy{}, false
	}
	p := contentBuildPolicy{
		MaxRaw:    int64(binary.LittleEndian.Uint64(data[0:])),
		MaxText:   int64(binary.LittleEndian.Uint64(data[8:])),
		Skipped:   int64(binary.LittleEndian.Uint64(data[16:])),
		Truncated: int64(binary.LittleEndian.Uint64(data[24:])),
	}
	if len(data) >= 40 {
		p.ScopeDropped = int64(binary.LittleEndian.Uint64(data[32:]))
	}
	if len(data) >= 56 {
		p.ScopeDroppedBytes = int64(binary.LittleEndian.Uint64(data[40:]))
		p.BudgetBytes = int64(binary.LittleEndian.Uint64(data[48:]))
	}
	// Binary rides the pre-v7 64-byte tail; ReserveBytes the v7 72-byte
	// tail. Either length decodes; a shorter section leaves zeros.
	if len(data) >= 64 {
		p.Binary = int64(binary.LittleEndian.Uint64(data[56:]))
	}
	if len(data) >= contentPolicySize {
		p.ReserveBytes = int64(binary.LittleEndian.Uint64(data[64:]))
	}
	return p, true
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
	DocLen           uint32 // byte length of the case-folded text, for scoring
	ModUnix          int64
	TextOff          uint64 // offset into the case-preserving CXST text store
	TextLen          uint32
	// FileSize and FileModNano are the source file's byte size and nanosecond
	// mtime at extraction time. The walk reconcile (no USN journal) diffs them
	// against the filename index for change detection; USN volumes stamp them
	// too so folds stay uniform. Zero on sidecars predating v6, which never
	// attach (version gate) and are rebuilt instead.
	FileSize    int64
	FileModNano int64
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
	// ScopeHash identifies the resolved scope. Zero means this sidecar predates
	// scope identity and is not safe for service attachment.
	ScopeHash [32]byte
	// EncodedSize is the encoded `.gsx` byte size: set by decode from the file
	// length and by encode from the produced bytes. Not persisted; surfaced in
	// content health so sidecar growth is observable (WP10/M7).
	EncodedSize int64
	// Scanned and Declined are the walk build's file accounting beyond Policy:
	// how many eligible files were seen, and how many produced no text (a
	// binary/unsupported file has no extractor; an empty or text-free file's
	// extractor yields nothing). Not persisted; the offline CLI reports them so
	// the counters reconcile against the file count.
	Scanned  int64
	Declined int64
	// SectionCRC holds the expected CRC32C per section tag for sections whose
	// table entry carries contentSectionFlagCRC. Sections absent from the map
	// are legacy (pre-checksum) and load without verification; the next build
	// rewrites them with checksums.
	SectionCRC map[uint32]uint32
	// release unmaps the file backing Sections when the index was loaded from a
	// memory-mapped `.gsx` (contentLoadFileMapped); nil for a heap-decoded
	// index. Only Sections (and the reader's text/posting views derived from
	// them) alias the mapping; Docs and Policy are decoded copies.
	release func()
	// pin guards the mapping's lifetime against readers that run outside
	// indexMu (the background integrity pass): a reader holds one pin
	// reference while touching Sections, and Release waits for every
	// reference to drop before unmapping. pinMu serializes pinRefuse against
	// pin acquisition only; pins themselves are a WaitGroup so a pinned
	// reader never blocks another pinned reader. Release is once-only
	// (releaseOnce), and attachRelease also arms a finalizer, so an index
	// whose owner merely drops it is still unmapped once no query references
	// it and no pin is held. The finalizer runs only on an unreachable index,
	// but reachability alone does not stop an explicit Release — the pin does.
	pin         sync.WaitGroup
	pinMu       sync.RWMutex
	pinRefuse   bool
	releaseOnce sync.Once
	// shards is set on a composite index loaded from a manifest (see
	// content_shards.go); Docs then spans every shard and Sections holds only
	// the manifest's own sections.
	shards []*contentShard
}

// contentMappingReleaseHook, when non-nil, runs once each time a mapped `.gsx`
// backing is unmapped. Tests use it to assert a replaced mapping is released.
// It may be read from a finalizer goroutine, so access is mutex-guarded.
var (
	contentMappingHookMu      sync.Mutex
	contentMappingReleaseHook func()
)

func setContentMappingReleaseHook(fn func()) {
	contentMappingHookMu.Lock()
	contentMappingReleaseHook = fn
	contentMappingHookMu.Unlock()
}

func fireContentMappingReleaseHook() {
	contentMappingHookMu.Lock()
	fn := contentMappingReleaseHook
	contentMappingHookMu.Unlock()
	if fn != nil {
		fn()
	}
}

// attachRelease records how to unmap this index's backing file and arms a
// finalizer so the mapping is released even when the owner only drops the
// index. Only the mmap load path calls it; a heap index leaves release nil.
// The finalizer calls Release, which takes the pin's write side, so it waits
// out any in-flight pinned section read before unmapping.
func (idx *contentIndex) attachRelease(release func()) {
	if idx == nil || release == nil {
		return
	}
	idx.release = release
	runtime.SetFinalizer(idx, (*contentIndex).Release)
}

// pinRead holds a read pin on the index's mapping while the caller touches
// Sections. It reports false when the index is already released: the caller
// must stop without reading. A composite pins only its manifest mapping here;
// shards are pinned individually through their own indexes.
// Every pinRead must be paired with pinDone, promptly: Release blocks until
// the last pin drops.
func (idx *contentIndex) pinRead() bool {
	if idx == nil {
		return false
	}
	idx.pinMu.RLock()
	defer idx.pinMu.RUnlock()
	if idx.pinRefuse {
		return false
	}
	idx.pin.Add(1)
	return true
}

// pinDone releases one pinRead hold.
func (idx *contentIndex) pinDone() {
	if idx == nil {
		return
	}
	idx.pin.Done()
}

// released reports whether Release has run. Readers outside indexMu (the
// background integrity pass) check it up front so they stop early instead of
// hashing a mapping that is already gone.
func (idx *contentIndex) released() bool {
	if idx == nil {
		return true
	}
	idx.pinMu.RLock()
	defer idx.pinMu.RUnlock()
	return idx.pinRefuse
}

// Release unmaps the index's backing file exactly once. It refuses new pins
// and waits for every out-of-lock section reader (a background integrity pass
// holding a pin), so the unmap can never race an in-flight section read even
// when the releaser does not hold indexMu. Shard mappings are released
// through the shard indexes, which carry their own pins. It is a no-op on a
// heap-decoded index. The finalizer backstops a dropped index the same way:
// it runs only once the index is unreachable, then pins-out and unmaps like
// any Release.
func (idx *contentIndex) Release() {
	if idx == nil {
		return
	}
	idx.releaseOnce.Do(func() {
		for _, sh := range idx.shards {
			sh.idx.Release()
		}
		idx.pinMu.Lock()
		idx.pinRefuse = true
		release := idx.release
		idx.release = nil
		idx.pinMu.Unlock()
		// Wait out every pinned section read before unmapping: a background
		// integrity pass parked mid-hash holds a pin, so the unmap cannot
		// race its reads. A heap index has no mapping; the wait is still
		// harmless (no pin is ever held without a mapping behind it).
		idx.pin.Wait()
		if release != nil {
			release()
		}
	})
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
		_ = binary.Write(&buf, binary.LittleEndian, d.FileSize)
		_ = binary.Write(&buf, binary.LittleEndian, d.FileModNano)
	}
	return buf.Bytes()
}

// contentDocFixedSize is the encoded width of one doc record:
// docID(4)+frn(8)+type(2)+extractor(2)+hash(16)+rawSize(8)+docLen(4)+modUnix(8)+textOff(8)+textLen(4)+fileSize(8)+fileModNano(8)
const contentDocFixedSize = 80

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
		d.FileSize = int64(binary.LittleEndian.Uint64(data[p+32:]))
		d.FileModNano = int64(binary.LittleEndian.Uint64(data[p+40:]))
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
	if idx.ScopeHash != ([32]byte{}) {
		idx.Sections[contentSectionScope] = idx.ScopeHash[:]
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
		entries = append(entries, contentSectionEntry{tag: tag, offset: off, length: uint64(len(data)), flags: contentSectionFlagCRC, crc: contentSectionCRC(data), hasCRC: true})
		off += uint64(len(data))
	}
	for i, e := range entries {
		p := 4 + i*contentSectionEntrySize
		binary.LittleEndian.PutUint32(table[p:], e.tag)
		binary.LittleEndian.PutUint64(table[p+4:], e.offset)
		binary.LittleEndian.PutUint64(table[p+12:], e.length)
		binary.LittleEndian.PutUint32(table[p+20:], e.flags)
		binary.LittleEndian.PutUint32(table[p+24:], e.crc)
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
	// crc/hasCRC carry the section-table checksum: crc is the entry's trailing
	// word and hasCRC whether contentSectionFlagCRC was set.
	crc    uint32
	hasCRC bool
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

// contentDecode parses a `.gsx`. The doc table is decoded eagerly into heap
// copies; other sections are handed back as slices into data. When data is a
// mapping, those section slices alias the mapping and the index must be
// Released before the mapping goes away (see attachRelease).
func contentIndexDecode(data []byte) (*contentIndex, error) {
	if len(data) < contentHeaderSize() {
		return nil, errors.New("content index truncated")
	}
	if !bytes.Equal(data[0:8], contentIndexMagic[:]) {
		return nil, errors.New("unsupported content index format")
	}
	version := int(binary.LittleEndian.Uint32(data[8:]))
	if version != contentIndexVersion && version != contentManifestVersion {
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
		flags := binary.LittleEndian.Uint32(data[p+20:])
		crc := binary.LittleEndian.Uint32(data[p+24:])
		if length > contentIndexMaxSection || off+length > uint64(len(data)) {
			return nil, fmt.Errorf("content section %08x out of range", tag)
		}
		payload := data[off : off+length]
		idx.Sections[tag] = payload
		if flags&contentSectionFlagCRC != 0 {
			if idx.SectionCRC == nil {
				idx.SectionCRC = make(map[uint32]uint32, sectionCount)
			}
			idx.SectionCRC[tag] = crc
			if contentSectionEagerVerify(tag) {
				if got := contentSectionCRC(payload); got != crc {
					return nil, &contentSectionCRCError{tag: tag, want: crc, got: got}
				}
			}
		}
	}
	if sec, ok := idx.Sections[contentSectionPolicy]; ok {
		if p, ok := decodeContentPolicy(sec); ok {
			idx.Policy = p
		}
	}
	if sec, ok := idx.Sections[contentSectionScope]; ok {
		if len(sec) != len(idx.ScopeHash) {
			return nil, errors.New("content index invalid scope identity")
		}
		copy(idx.ScopeHash[:], sec)
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
	if _, ok := idx.Sections[contentSectionShards]; (version == contentManifestVersion) != ok || ok && len(docs) != 0 {
		return nil, errors.New("content index manifest is malformed")
	}
	return idx, nil
}

// contentIndexExtractorMismatch reports whether a decoded index contains a doc
// whose extractor identity no longer matches the registry. A doc with
// ContentType==0 is an older `.gsx` written before these fields were populated.
// A class whose registered version differs means that extractor's Version() was
// bumped. A mismatch is resolved by contentPlanStaleExtractorRefresh: a
// registered-class version bump re-extracts just the stale docs in place, while
// an unregistered class or an over-large stale set falls back to a full rebuild.
func contentIndexExtractorMismatch(idx *contentIndex) (string, bool) {
	if idx == nil {
		return "nil content index", true
	}
	for i := range idx.Docs {
		d := &idx.Docs[i]
		if d.ContentType == 0 {
			return "content doc has no content type", true
		}
		want, ok := contentExtractorVersionForClass(d.ContentType)
		if !ok {
			return fmt.Sprintf("content doc class %d has no registered extractor", d.ContentType), true
		}
		if d.ExtractorVersion != want {
			return fmt.Sprintf("content doc class %d version %d != %d", d.ContentType, d.ExtractorVersion, want), true
		}
	}
	return "", false
}

// contentRefreshMaxDocs bounds a targeted extractor-version refresh: a stale
// set larger than this falls back to a full rebuild, which is the right tool
// for a wholesale invalidation and bounds the coordinator queue the refresh
// admits. A var so tests can lower it.
var contentRefreshMaxDocs = 32768

// contentStaleExtractorPlan is the outcome of partitioning a mismatched content
// index. A non-rebuild plan carries the FRNs to re-extract in place; a rebuild
// plan carries the reason the full-rebuild path must run instead.
type contentStaleExtractorPlan struct {
	refreshFRNs []uint64
	rebuild     bool
	reason      string
}

// contentPlanStaleExtractorRefresh partitions an index whose extractor identity
// no longer matches the registry into docs that can be re-extracted in place:
//
//   - a registered class whose Version() changed: the FRN is re-extracted by the
//     drain with the current extractor, which re-stamps the delta doc's class
//     and version; the fold then carries it into the base;
//   - an old class==0 doc whose file still resolves and whose extension a
//     registered extractor (or the service text allowlist) claims: same refresh;
//   - an old class==0 doc whose file no longer resolves is still enqueued so the
//     drain tombstones it through the normal delete path.
//
// A class==0 doc whose extension is no longer extractable, a doc whose class has
// no registered extractor, or a stale set over contentRefreshMaxDocs cannot be
// refreshed and forces rebuild=true. resolvePath may be nil only when the index
// has no class==0 docs.
func contentPlanStaleExtractorRefresh(idx *contentIndex, resolvePath func(uint64) (string, bool)) contentStaleExtractorPlan {
	if idx == nil {
		return contentStaleExtractorPlan{rebuild: true, reason: "nil content index"}
	}
	var plan contentStaleExtractorPlan
	for i := range idx.Docs {
		if len(plan.refreshFRNs) > contentRefreshMaxDocs {
			return contentStaleExtractorPlan{rebuild: true, reason: fmt.Sprintf("more than %d stale content docs", contentRefreshMaxDocs)}
		}
		d := &idx.Docs[i]
		if d.ContentType == 0 {
			path, resolved := "", false
			if resolvePath != nil {
				path, resolved = resolvePath(d.FRN)
			}
			if !resolved || contentPathExtractableByRegistry(path) {
				plan.refreshFRNs = append(plan.refreshFRNs, d.FRN)
				continue
			}
			return contentStaleExtractorPlan{rebuild: true, reason: fmt.Sprintf("content doc FRN %d has no content type and its extension is not extractable", d.FRN)}
		}
		want, ok := contentExtractorVersionForClass(d.ContentType)
		if !ok {
			return contentStaleExtractorPlan{rebuild: true, reason: fmt.Sprintf("content doc class %d has no registered extractor", d.ContentType)}
		}
		if d.ExtractorVersion != want {
			plan.refreshFRNs = append(plan.refreshFRNs, d.FRN)
		}
	}
	if len(plan.refreshFRNs) > contentRefreshMaxDocs {
		return contentStaleExtractorPlan{rebuild: true, reason: fmt.Sprintf("%d stale content docs exceed the targeted-refresh cap (%d)", len(plan.refreshFRNs), contentRefreshMaxDocs)}
	}
	return plan
}

// contentPathExtractableByRegistry reports whether a path could be extracted
// without opening it: anything but a known-binary extension (which the sniff
// would reject) is a text candidate, including extensionless files. It is the
// extension gate for refreshing an old class==0 doc and for catch-up byte
// budgeting: a doc nothing could extract now forces a rebuild instead of a
// targeted refresh.
func contentPathExtractableByRegistry(path string) bool {
	return !contentBinaryExtension(contentPathExtension(path))
}

// contentStaleExtractorDocCount counts the docs in an attached base whose
// (class, version) no longer matches the registry. It drives the health refresh
// signal: it stays non-zero (degraded) while stale text is served and returns to
// zero once a fold publishes a base re-extracted at the current versions.
func contentStaleExtractorDocCount(idx *contentIndex) int {
	if idx == nil {
		return 0
	}
	n := 0
	for i := range idx.Docs {
		d := &idx.Docs[i]
		if d.ContentType == 0 {
			n++
			continue
		}
		want, ok := contentExtractorVersionForClass(d.ContentType)
		if !ok || d.ExtractorVersion != want {
			n++
		}
	}
	return n
}

// contentLookupFRN binary-searches the FRN-sorted doc table.
func contentLookupFRN(docs []contentDoc, frn uint64) (int, bool) {
	i := sort.Search(len(docs), func(i int) bool { return docs[i].FRN >= frn })
	if i < len(docs) && docs[i].FRN == frn {
		return i, true
	}
	return 0, false
}

// contentSectionPayload is one section headed for a `.gsx`: either resident
// bytes or a temp file holding the exact section bytes (text store and posting
// sections spooled during assembly).
type contentSectionPayload struct {
	tag  uint32
	data []byte
	file string
	size uint64
}

// contentCollectPayloads gathers the sections contentIndexEncode and
// contentSaveAssembled both write: the doc table, policy and scope derived
// from idx exactly as the encoder derives them, the resident small sections,
// and the file-backed large sections. Both writers share it so the streaming
// save is byte-identical to the in-memory encode for the same input.
func contentCollectPayloads(idx *contentIndex, files map[uint32]string) []contentSectionPayload {
	sections := make(map[uint32][]byte, len(idx.Sections)+3)
	for tag, data := range idx.Sections {
		if tag == contentSectionDocTable {
			continue
		}
		sections[tag] = data
	}
	sections[contentSectionDocTable] = contentDocTableBytes(idx.Docs)
	if idx.Policy != (contentBuildPolicy{}) {
		sections[contentSectionPolicy] = idx.Policy.encode()
	}
	if idx.ScopeHash != ([32]byte{}) {
		sections[contentSectionScope] = idx.ScopeHash[:]
	}
	out := make([]contentSectionPayload, 0, len(sections)+len(files))
	for tag, data := range sections {
		if path, ok := files[tag]; ok {
			if st, err := os.Stat(path); err == nil {
				out = append(out, contentSectionPayload{tag: tag, file: path, size: uint64(st.Size())})
				continue
			}
		}
		out = append(out, contentSectionPayload{tag: tag, data: data, size: uint64(len(data))})
	}
	for tag, path := range files {
		known := false
		for _, p := range out {
			if p.tag == tag {
				known = true
				break
			}
		}
		if known {
			continue
		}
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			out = append(out, contentSectionPayload{tag: tag, file: path, size: uint64(st.Size())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].tag < out[j].tag })
	return out
}

// contentPayloadSize totals a payload set's encoded size: header + payloads +
// the section table. Both the cap check and the header's table offset derive
// from it, so an over-cap save is refused before anything is written and the
// table offset is exact.
func contentPayloadSize(payloads []contentSectionPayload) int64 {
	total := int64(contentHeaderSize()) + 4 + int64(len(payloads))*contentSectionEntrySize
	for _, p := range payloads {
		total += int64(p.size)
	}
	return total
}

// contentWritePayload copies one payload to w while accumulating its CRC32C.
// Exactly one of p.data/p.file carries the bytes; a missing file reads as
// empty (a section FinishFile declined to create). h is an extra sink (the
// caller's own hasher, or io.Discard).
func contentWritePayload(w io.Writer, h io.Writer, p contentSectionPayload) (uint32, error) {
	crc := crc32.New(contentSectionCRCModel)
	mw := io.MultiWriter(w, h, crc)
	if p.file != "" {
		f, err := os.Open(p.file)
		if err != nil {
			if os.IsNotExist(err) {
				return crc.Sum32(), nil
			}
			return 0, err
		}
		_, err = io.CopyBuffer(mw, f, make([]byte, contentSectionFileBufSize))
		cerr := f.Close()
		if err != nil {
			return 0, err
		}
		if cerr != nil {
			return 0, cerr
		}
		return crc.Sum32(), nil
	}
	if _, err := mw.Write(p.data); err != nil {
		return 0, err
	}
	return crc.Sum32(), nil
}

// contentSaveAssembled streams one shard to path: the header placeholder, each
// section payload from memory or its spool file (hashing the CRC32C as it
// goes), then the section table with checksums, then the patched header. The
// temp-then-rename atomicity, the contentGSXSizeError cap (checked before
// anything is written), the deterministic section order and the byte-identical
// output for the same input all match contentSaveFile.
func contentSaveAssembled(path string, idx *contentIndex, files map[uint32]string) error {
	payloads := contentCollectPayloads(idx, files)
	if size := contentPayloadSize(payloads); contentGSXMaxBytes > 0 && size > contentGSXMaxBytes {
		return &contentGSXSizeError{size: size, cap: contentGSXMaxBytes}
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
	bw := bufio.NewWriterSize(tmp, contentSectionFileBufSize)
	fail := func(err error) error {
		_ = bw.Flush()
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := bw.Write(make([]byte, contentHeaderSize())); err != nil {
		return fail(err)
	}
	entries := make([]contentSectionEntry, 0, len(payloads))
	off := uint64(contentHeaderSize())
	for _, p := range payloads {
		crc, err := contentWritePayload(bw, io.Discard, p)
		if err != nil {
			return fail(err)
		}
		entries = append(entries, contentSectionEntry{tag: p.tag, offset: off, length: p.size, flags: contentSectionFlagCRC, crc: crc, hasCRC: true})
		off += p.size
	}
	tableOffset := off
	table := make([]byte, 4+len(entries)*contentSectionEntrySize)
	binary.LittleEndian.PutUint32(table[:4], uint32(len(entries)))
	for i, e := range entries {
		q := 4 + i*contentSectionEntrySize
		binary.LittleEndian.PutUint32(table[q:], e.tag)
		binary.LittleEndian.PutUint64(table[q+4:], e.offset)
		binary.LittleEndian.PutUint64(table[q+12:], e.length)
		binary.LittleEndian.PutUint32(table[q+20:], e.flags)
		binary.LittleEndian.PutUint32(table[q+24:], e.crc)
	}
	if _, err := bw.Write(table); err != nil {
		return fail(err)
	}
	if err := bw.Flush(); err != nil {
		return fail(err)
	}
	header := make([]byte, contentHeaderSize())
	contentWriteHeader(header, idx, tableOffset, len(entries))
	if _, err := tmp.WriteAt(header, 0); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	idx.EncodedSize = int64(tableOffset) + int64(len(table))
	return os.Rename(name, path)
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

// contentMapFileFn is the mmap primitive contentLoadFileMapped uses. A var so
// tests can force the heap fallback.
var contentMapFileFn = contentMapFile

// contentLoadFileMapped loads a `.gsx` by mapping it read-only and decoding the
// blob sections zero-copy: Sections (and the reader's text/posting views) alias
// the mapping until the index is Released. If mapping fails for any reason it
// falls back to reading the file into the heap (release nil), so a load that
// could have succeeded never fails for lack of a mapping.
//
// The index owns the mapping: attachRelease arms a finalizer and the returned
// index's Release unmaps it exactly once.
func contentLoadFileMapped(path string) (*contentIndex, error) {
	data, release, err := contentMapFileFn(path)
	if err != nil {
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		release = nil
	}
	idx, err := contentIndexDecode(data)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	idx.attachRelease(release)
	if idx.Version == contentManifestVersion {
		if err := contentComposeShards(idx, path); err != nil {
			idx.Release()
			return nil, err
		}
	}
	return idx, nil
}

// contentLoadFileHeap is the pre-mmap path: read the whole `.gsx` into the heap
// and decode it. Tests use it to compare a mapped load against the read path;
// production goes through contentLoadFileMapped.
func contentLoadFileHeap(path string) (*contentIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return contentIndexDecode(data)
}

// contentLoadFile reads and decodes a `.gsx`. It maps the file (zero-copy
// sections) and falls back to a heap read, so its contract is unchanged.
func contentLoadFile(path string) (*contentIndex, error) {
	return contentLoadFileMapped(path)
}
