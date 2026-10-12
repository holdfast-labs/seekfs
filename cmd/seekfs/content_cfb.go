package main

// A bounded, hand-rolled OLE2/CFB (Compound File Binary) reader, used by the
// .msg extractor. It parses the 512-byte header, the DIFAT/FAT and mini-FAT
// allocation tables, the directory tree, and exposes the named streams the MSG
// format stores its MAPI properties in.
//
// Adapted from github.com/ffois/mailfmt internal/msg/msg.go (MIT License,
// Copyright (c) 2025 ffois): the sector-chain walker and the maxPrealloc clamp
// are modelled on that reader. This copy is deliberately narrower and adds a
// hard input bound: the caller wraps the underlying ReaderAt in an
// io.SectionReader limited to the extraction policy's maxRaw, so every offset
// arithmetic here is over at most maxRaw bytes and every allocation sized from
// an attacker-controlled field (stream size, sector/directory counts) is
// clamped by contentCFBMaxPrealloc or an explicit count cap.
//
// Untrusted input: a crafted compound file can loop a FAT/mini-FAT/directory
// chain forever, point a chain at a sector outside the file, or declare a
// multi-exabyte stream size. Every chain walk carries a visited set and every
// allocation is clamped, so a malformed file errors or returns a bounded prefix
// rather than panicking, looping, or exhausting memory.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
)

const (
	contentCFBSignature     = 0xE11AB1A1E011CFD0
	contentCFBMiniCutoff    = 4096
	contentCFBMaxRegular    = 0xFFFFFFFA // first special FAT value
	contentCFBNoStream      = 0xFFFFFFFF
	contentCFBDIFATInHeader = 109
	contentCFBDirEntrySize  = 128

	// contentCFBMaxPrealloc caps how many bytes a declared stream size may
	// reserve up front. Every size here is attacker-controlled; an unclamped
	// make() on a claimed 1<<62 panics with "makeslice: cap out of range" and a
	// merely large one exhausts memory before a byte is read. Streams larger
	// than the cap still read correctly: append grows as real sectors arrive,
	// which the bounded input length caps.
	contentCFBMaxPrealloc = 8 << 20
	// contentCFBMaxDirEntries and contentCFBMaxFATEntries bound the entry and
	// allocation-table slices independently of the input length, so a file with
	// an enormous but finite chain cannot turn into an enormous in-memory table.
	contentCFBMaxDirEntries = 1 << 16
	contentCFBMaxFATEntries = 1 << 22
)

// contentCFBPrealloc returns an empty slice with capacity clamped to
// contentCFBMaxPrealloc; see that constant for why the clamp is not optional.
func contentCFBPrealloc(size uint64) []byte {
	if size > contentCFBMaxPrealloc {
		size = contentCFBMaxPrealloc
	}
	return make([]byte, 0, size)
}

// contentCFBChain follows a FAT/mini-FAT/directory chain, refusing to visit a
// sector twice. A crafted file can point a chain back at itself; without this
// guard the walk never terminates. A cycle is not recoverable the way a panic
// is, so it must be prevented rather than caught.
type contentCFBChain map[uint32]bool

func (w contentCFBChain) visit(sector uint32) bool {
	if sector >= contentCFBMaxRegular || w[sector] {
		return false
	}
	w[sector] = true
	return true
}

type contentCFBHeader struct {
	Signature            uint64
	CLSID                [16]byte
	MinorVersion         uint16
	MajorVersion         uint16
	ByteOrder            uint16
	SectorShift          uint16
	MiniSectorShift      uint16
	Reserved1            [6]byte
	TotalSectors         uint32
	FATSectors           uint32
	FirstDirectorySector uint32
	TransactionSignature uint32
	MiniStreamCutoff     uint32
	FirstMiniFATSector   uint32
	MiniFATSectors       uint32
	FirstDIFATSector     uint32
	DIFATSectors         uint32
	DIFAT                [contentCFBDIFATInHeader]uint32
}

type contentCFBDirectoryEntry struct {
	Name              [64]byte
	NameLen           uint16
	ObjectType        uint8
	ColorFlag         uint8
	LeftSiblingID     uint32
	RightSiblingID    uint32
	ChildID           uint32
	CLSID             [16]byte
	StateBits         uint32
	CreationTime      uint64
	ModifiedTime      uint64
	StartingSectorLoc uint32
	StreamSize        uint64
}

type contentCFBReader struct {
	ctx        context.Context
	reader     io.ReaderAt
	header     contentCFBHeader
	sectorSize int
	fat        []uint32
	miniFAT    []uint32
	dirEntries []contentCFBDirectoryEntry
	miniStream []byte
	// props maps a MAPI (property id << 16 | type) to the directory-entry index
	// of its `__substg1.0_XXXXYYYY` stream.
	props map[uint32]int
}

// newContentCFBReader parses a compound file from r. r must already be bounded
// by the caller (an io.SectionReader at the extraction policy's maxRaw) and size
// is the real file length; this function never reads outside that bound and
// never trusts a size in the file itself to allocate more than
// contentCFBMaxPrealloc.
func newContentCFBReader(ctx context.Context, r io.ReaderAt, size int64) (*contentCFBReader, error) {
	cfb := &contentCFBReader{ctx: ctx, reader: r, props: make(map[uint32]int)}

	headerData := make([]byte, 512)
	if _, err := r.ReadAt(headerData, 0); err != nil {
		return nil, fmt.Errorf("cfb: read header: %w", err)
	}
	if err := binary.Read(bytes.NewReader(headerData), binary.LittleEndian, &cfb.header); err != nil {
		return nil, fmt.Errorf("cfb: decode header: %w", err)
	}
	if cfb.header.Signature != contentCFBSignature {
		return nil, errors.New("cfb: bad signature")
	}
	// [MS-CFB] allows two sector sizes and one mini-sector size. Rejecting
	// anything else closes a class of arithmetic bugs: a SectorShift below 2
	// gives a sector smaller than a FAT entry (negative entries-per-sector), and
	// an inflated MiniSectorShift overflows the mini-stream offset math.
	if cfb.header.SectorShift != 9 && cfb.header.SectorShift != 12 {
		return nil, fmt.Errorf("cfb: invalid sector shift %d", cfb.header.SectorShift)
	}
	if cfb.header.MiniSectorShift != 6 {
		return nil, fmt.Errorf("cfb: invalid mini sector shift %d", cfb.header.MiniSectorShift)
	}
	cfb.sectorSize = 1 << cfb.header.SectorShift

	if err := cfb.readFAT(); err != nil {
		return nil, err
	}
	if err := cfb.readDirectories(); err != nil {
		return nil, err
	}
	// A stream cannot hold more bytes than the whole file. A directory entry
	// claiming otherwise is malformed; rejecting it here keeps a bogus
	// multi-exabyte StreamSize from ever reaching the read path as a "large
	// stream" (the prealloc clamp already bounds the allocation, but the file is
	// still not a valid compound file).
	for i := range cfb.dirEntries {
		if cfb.dirEntries[i].StreamSize > uint64(size) {
			return nil, errors.New("cfb: stream size exceeds file")
		}
	}
	if cfb.header.FirstMiniFATSector < contentCFBMaxRegular {
		if err := cfb.readMiniFAT(); err != nil {
			return nil, err
		}
	}
	if len(cfb.dirEntries) > 0 && cfb.dirEntries[0].StreamSize > 0 {
		if err := cfb.readMiniStream(); err != nil {
			return nil, err
		}
	}
	cfb.indexProperties()
	return cfb, nil
}

func (cfb *contentCFBReader) sectorOffset(sector uint32) int64 {
	return int64(sector+1) * int64(cfb.sectorSize)
}

func (cfb *contentCFBReader) readSector(sector uint32) ([]byte, error) {
	data := make([]byte, cfb.sectorSize)
	_, err := cfb.reader.ReadAt(data, cfb.sectorOffset(sector))
	return data, err
}

func (cfb *contentCFBReader) readFAT() error {
	var fatSectors []uint32
	for i := 0; i < contentCFBDIFATInHeader && i < int(cfb.header.FATSectors); i++ {
		if s := cfb.header.DIFAT[i]; s < contentCFBMaxRegular {
			fatSectors = append(fatSectors, s)
		}
	}

	if cfb.header.DIFATSectors > 0 && cfb.header.FirstDIFATSector < contentCFBMaxRegular {
		difatSector := cfb.header.FirstDIFATSector
		seen := make(map[uint32]bool)
		for i := uint32(0); i < cfb.header.DIFATSectors; i++ {
			if difatSector >= contentCFBMaxRegular {
				break
			}
			if seen[difatSector] {
				return errors.New("cfb: DIFAT chain cycle")
			}
			seen[difatSector] = true
			if err := cfb.ctx.Err(); err != nil {
				return err
			}
			data, err := cfb.readSector(difatSector)
			if err != nil {
				return fmt.Errorf("cfb: read DIFAT sector: %w", err)
			}
			entries := cfb.sectorSize/4 - 1
			for j := 0; j < entries && len(fatSectors) < int(cfb.header.FATSectors); j++ {
				if s := binary.LittleEndian.Uint32(data[j*4:]); s < contentCFBMaxRegular {
					fatSectors = append(fatSectors, s)
				}
			}
			difatSector = binary.LittleEndian.Uint32(data[entries*4:])
		}
	}

	entriesPerSector := cfb.sectorSize / 4
	capHint := len(fatSectors) * entriesPerSector
	if capHint > contentCFBMaxFATEntries {
		capHint = contentCFBMaxFATEntries
	}
	cfb.fat = make([]uint32, 0, capHint)
	for _, sector := range fatSectors {
		if len(cfb.fat) >= contentCFBMaxFATEntries {
			break
		}
		data, err := cfb.readSector(sector)
		if err != nil {
			return fmt.Errorf("cfb: read FAT sector: %w", err)
		}
		for i := 0; i < entriesPerSector && len(cfb.fat) < contentCFBMaxFATEntries; i++ {
			cfb.fat = append(cfb.fat, binary.LittleEndian.Uint32(data[i*4:]))
		}
	}
	return nil
}

func (cfb *contentCFBReader) readDirectories() error {
	entriesPerSector := cfb.sectorSize / contentCFBDirEntrySize
	sector := cfb.header.FirstDirectorySector
	seen := make(map[uint32]bool)

	for len(cfb.dirEntries) < contentCFBMaxDirEntries {
		if sector >= contentCFBMaxRegular {
			return errors.New("cfb: invalid directory sector")
		}
		if seen[sector] {
			return errors.New("cfb: directory chain cycle")
		}
		seen[sector] = true
		if err := cfb.ctx.Err(); err != nil {
			return err
		}
		data, err := cfb.readSector(sector)
		if err != nil {
			return fmt.Errorf("cfb: read directory sector: %w", err)
		}
		for i := 0; i < entriesPerSector && len(cfb.dirEntries) < contentCFBMaxDirEntries; i++ {
			var entry contentCFBDirectoryEntry
			if err := binary.Read(bytes.NewReader(data[i*contentCFBDirEntrySize:]), binary.LittleEndian, &entry); err != nil {
				return fmt.Errorf("cfb: decode directory entry: %w", err)
			}
			cfb.dirEntries = append(cfb.dirEntries, entry)
		}
		next, ok := cfb.advanceFAT(sector)
		if !ok {
			break
		}
		sector = next
	}
	return nil
}

// advanceFAT returns the next sector in a regular FAT chain, or ok=false when
// the chain ends at a special value or runs past the known FAT.
func (cfb *contentCFBReader) advanceFAT(sector uint32) (uint32, bool) {
	if int(sector) >= len(cfb.fat) {
		return 0, false
	}
	next := cfb.fat[sector]
	if next >= contentCFBMaxRegular {
		return 0, false
	}
	return next, true
}

func (cfb *contentCFBReader) readMiniFAT() error {
	entriesPerSector := cfb.sectorSize / 4
	sector := cfb.header.FirstMiniFATSector
	seen := make(map[uint32]bool)
	for len(cfb.miniFAT) < contentCFBMaxFATEntries {
		if sector >= contentCFBMaxRegular {
			break
		}
		if seen[sector] {
			return errors.New("cfb: mini-FAT chain cycle")
		}
		seen[sector] = true
		if err := cfb.ctx.Err(); err != nil {
			return err
		}
		data, err := cfb.readSector(sector)
		if err != nil {
			return fmt.Errorf("cfb: read mini-FAT sector: %w", err)
		}
		for i := 0; i < entriesPerSector && len(cfb.miniFAT) < contentCFBMaxFATEntries; i++ {
			cfb.miniFAT = append(cfb.miniFAT, binary.LittleEndian.Uint32(data[i*4:]))
		}
		next, ok := cfb.advanceFAT(sector)
		if !ok {
			break
		}
		sector = next
	}
	return nil
}

func (cfb *contentCFBReader) readMiniStream() error {
	root := cfb.dirEntries[0]
	cfb.miniStream = contentCFBPrealloc(root.StreamSize)
	sector := root.StartingSectorLoc
	remaining := int64(root.StreamSize)
	chain := contentCFBChain{}

	for remaining > 0 && chain.visit(sector) {
		data, err := cfb.readSector(sector)
		if err != nil {
			break
		}
		toRead := int64(cfb.sectorSize)
		if toRead > remaining {
			toRead = remaining
		}
		cfb.miniStream = append(cfb.miniStream, data[:toRead]...)
		remaining -= toRead
		if int(sector) >= len(cfb.fat) {
			break
		}
		sector = cfb.fat[sector]
	}
	return nil
}

// rootChildren returns the directory-entry indices of the root storage's direct
// children, in the tree's in-order sequence.
//
// The walk is iterative and keeps a visited set on purpose. An entry in a
// crafted file can name itself as its own sibling, or two entries can point at
// each other; recursion then descends until the goroutine stack is exhausted,
// which no recover catches.
func (cfb *contentCFBReader) rootChildren() []int {
	if len(cfb.dirEntries) == 0 {
		return nil
	}
	start := cfb.dirEntries[0].ChildID
	if start == contentCFBNoStream {
		return nil
	}
	type frame struct {
		idx  int
		emit bool
	}
	visited := make(map[int]bool, len(cfb.dirEntries))
	stack := []frame{{idx: int(start)}}
	var out []int
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.idx < 0 || f.idx >= len(cfb.dirEntries) || uint32(f.idx) == contentCFBNoStream {
			continue
		}
		if f.emit {
			out = append(out, f.idx)
			continue
		}
		if visited[f.idx] {
			continue
		}
		visited[f.idx] = true
		e := &cfb.dirEntries[f.idx]
		if e.RightSiblingID != contentCFBNoStream {
			stack = append(stack, frame{idx: int(e.RightSiblingID)})
		}
		stack = append(stack, frame{idx: f.idx, emit: true})
		if e.LeftSiblingID != contentCFBNoStream {
			stack = append(stack, frame{idx: int(e.LeftSiblingID)})
		}
	}
	return out
}

// indexProperties records the `__substg1.0_XXXXYYYY` property streams of the
// root storage, keyed by property id and type.
func (cfb *contentCFBReader) indexProperties() {
	for _, idx := range cfb.rootChildren() {
		e := &cfb.dirEntries[idx]
		if e.ObjectType != 2 {
			continue
		}
		id, typ, ok := contentCFBParsePropertyName(contentCFBEntryName(e))
		if !ok {
			continue
		}
		cfb.props[id<<16|typ] = idx
	}
}

// streamNames returns the names of the root storage's stream children,
// optionally filtered by prefix. It exists so callers can enumerate the
// compound file without reaching into the directory table.
func (cfb *contentCFBReader) streamNames() []string {
	var names []string
	for _, idx := range cfb.rootChildren() {
		if e := &cfb.dirEntries[idx]; e.ObjectType == 2 {
			names = append(names, contentCFBEntryName(e))
		}
	}
	return names
}

// propertyStream returns the directory index of a property stream, if present.
func (cfb *contentCFBReader) propertyStream(id, typ uint32) (int, bool) {
	idx, ok := cfb.props[id<<16|typ]
	return idx, ok
}

// readStreamBounded returns at most limit bytes of the stream at dirEntries idx
// and whether the whole declared stream was read. Short stream sizes still read
// in full; a declared size above limit is a bounded prefix (complete=false).
func (cfb *contentCFBReader) readStreamBounded(idx int, limit int64) ([]byte, bool) {
	if idx < 0 || idx >= len(cfb.dirEntries) || limit <= 0 {
		return nil, false
	}
	e := &cfb.dirEntries[idx]
	if e.StreamSize == 0 {
		return nil, true
	}
	want := int64(e.StreamSize)
	complete := true
	if want > limit {
		want = limit
		complete = false
	}
	if e.StreamSize < contentCFBMiniCutoff {
		return cfb.readMiniStreamData(e, want), complete
	}
	return cfb.readRegularStreamData(e, want), complete
}

func (cfb *contentCFBReader) readMiniStreamData(e *contentCFBDirectoryEntry, want int64) []byte {
	miniSectorSize := int64(1) << cfb.header.MiniSectorShift
	data := contentCFBPrealloc(uint64(want))
	sector := e.StartingSectorLoc
	remaining := want
	chain := contentCFBChain{}

	for remaining > 0 && chain.visit(sector) {
		offset := int64(sector) * miniSectorSize
		if offset >= int64(len(cfb.miniStream)) {
			break
		}
		n := miniSectorSize
		if n > remaining {
			n = remaining
		}
		end := offset + n
		if end > int64(len(cfb.miniStream)) {
			end = int64(len(cfb.miniStream))
		}
		data = append(data, cfb.miniStream[offset:end]...)
		remaining -= end - offset
		if int(sector) >= len(cfb.miniFAT) {
			break
		}
		sector = cfb.miniFAT[sector]
	}
	return data
}

func (cfb *contentCFBReader) readRegularStreamData(e *contentCFBDirectoryEntry, want int64) []byte {
	data := contentCFBPrealloc(uint64(want))
	sector := e.StartingSectorLoc
	remaining := want
	chain := contentCFBChain{}

	for remaining > 0 && chain.visit(sector) {
		sectorData, err := cfb.readSector(sector)
		if err != nil {
			break
		}
		toRead := int64(cfb.sectorSize)
		if toRead > remaining {
			toRead = remaining
		}
		data = append(data, sectorData[:toRead]...)
		remaining -= toRead
		if int(sector) >= len(cfb.fat) {
			break
		}
		sector = cfb.fat[sector]
	}
	return data
}

// contentCFBEntryName decodes a directory entry's UTF-16LE name, stripping the
// trailing NUL the format requires.
func contentCFBEntryName(e *contentCFBDirectoryEntry) string {
	if e.NameLen <= 2 || int(e.NameLen) > len(e.Name) {
		return ""
	}
	nb := e.Name[:e.NameLen-2]
	units := make([]uint16, 0, len(nb)/2)
	for i := 0; i+1 < len(nb); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(nb[i:]))
	}
	return string(utf16.Decode(units))
}

// contentCFBParsePropertyName parses `__substg1.0_XXXXYYYY`, returning the
// property id and type. Anything else reports false.
func contentCFBParsePropertyName(name string) (uint32, uint32, bool) {
	const prefix = "__substg1.0_"
	if len(name) < len(prefix)+8 || name[:len(prefix)] != prefix {
		return 0, 0, false
	}
	id, ok1 := contentCFBHex16(name[len(prefix) : len(prefix)+4])
	typ, ok2 := contentCFBHex16(name[len(prefix)+4 : len(prefix)+8])
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return id, typ, true
}

func contentCFBHex16(s string) (uint32, bool) {
	var v uint32
	for i := 0; i < len(s); i++ {
		v <<= 4
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			v |= uint32(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= uint32(c-'A') + 10
		default:
			return 0, false
		}
	}
	return v, true
}
