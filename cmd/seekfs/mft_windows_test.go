//go:build windows

package main

import (
	"encoding/binary"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// synthMFTRecord builds a minimal but valid 1024-byte MFT record with a fixup
// array, a $STANDARD_INFORMATION (modified time), a $FILE_NAME (parent + name),
// and a resident or non-resident $DATA (size). Used to unit test the parser
// without a live volume.
type synthOpts struct {
	isDir       bool
	inUse       bool
	name        string
	namespace   byte
	parentFRN   uint64
	modUnixNano int64
	size        int64
	attrs       uint32
	nonResident bool
	namedData   bool // a named $DATA stream that must NOT count as file size
	// altName/altNamespace add a second $FILE_NAME attribute, modelling a hard
	// link or the DOS 8.3 alias that shares one MFT record.
	altName      string
	altNamespace byte
	// content, when non-nil, fills the resident $DATA bytes and sets size to
	// its length. seq/baseRef model the record sequence and the header's
	// base-record reference (extension records set baseRef to another record).
	content  []byte
	seq      uint16
	baseRef  uint64
	attrList bool
}

func synthMFTRecord(o synthOpts) []byte {
	const recSize = 1024
	rec := make([]byte, recSize)
	fillSynthMFTRecord(rec, o)
	return rec
}

// fillSynthMFTRecord writes a synthetic MFT record into rec, which must be at
// least 1024 bytes. It lets benchmarks reuse one buffer instead of allocating a
// kilobyte per generated record.
func fillSynthMFTRecord(rec []byte, o synthOpts) {
	const recSize = 1024
	const bps = 512
	for i := range rec {
		rec[i] = 0
	}
	binary.LittleEndian.PutUint32(rec[0:4], mftRecordMagic)
	// Update sequence array at offset 48, count = 1 + (recSize/bps) = 3.
	usaOff := 48
	usaCount := 1 + recSize/bps
	binary.LittleEndian.PutUint16(rec[4:6], uint16(usaOff))
	binary.LittleEndian.PutUint16(rec[6:8], uint16(usaCount))
	// flags
	flags := uint16(0)
	if o.inUse {
		flags |= mftFlagInUse
	}
	if o.isDir {
		flags |= mftFlagDir
	}
	binary.LittleEndian.PutUint16(rec[22:24], flags)
	binary.LittleEndian.PutUint16(rec[16:18], o.seq)
	binary.LittleEndian.PutUint64(rec[mftBaseRefOffset:mftBaseRefOffset+8], o.baseRef)
	// first attribute offset
	firstAttr := 64
	binary.LittleEndian.PutUint16(rec[20:22], uint16(firstAttr))

	// Write the USN and place it at the end of each sector (pre-fixup state),
	// stashing the "real" bytes into the USA entries.
	usn := []byte{0xAA, 0xBB}
	rec[usaOff] = usn[0]
	rec[usaOff+1] = usn[1]
	for i := 0; i < recSize/bps; i++ {
		sectorEnd := (i+1)*bps - 2
		// real bytes (arbitrary) stored in USA
		rec[usaOff+2+i*2] = byte(0x10 + i)
		rec[usaOff+2+i*2+1] = byte(0x20 + i)
		// current sector-end holds the USN
		rec[sectorEnd] = usn[0]
		rec[sectorEnd+1] = usn[1]
	}

	pos := firstAttr

	writeAttrHeader := func(attrType uint32, contentLen int, content []byte) {
		// Resident attribute header is 24 bytes; content follows.
		attrLen := 24 + contentLen
		// align to 8
		if attrLen%8 != 0 {
			attrLen += 8 - attrLen%8
		}
		binary.LittleEndian.PutUint32(rec[pos:pos+4], attrType)
		binary.LittleEndian.PutUint32(rec[pos+4:pos+8], uint32(attrLen))
		rec[pos+8] = 0 // resident
		binary.LittleEndian.PutUint32(rec[pos+16:pos+20], uint32(contentLen))
		binary.LittleEndian.PutUint16(rec[pos+20:pos+22], 24) // content offset
		copy(rec[pos+24:pos+24+contentLen], content)
		pos += attrLen
	}

	// $STANDARD_INFORMATION: modified time at content offset +8.
	si := make([]byte, 48)
	if o.modUnixNano != 0 {
		ft := uint64(o.modUnixNano/100) + filetimeToUnix100ns
		binary.LittleEndian.PutUint64(si[8:16], ft)
	}
	attrs := o.attrs
	if o.isDir {
		attrs |= fileAttributeDir
	}
	if attrs != 0 {
		binary.LittleEndian.PutUint32(si[32:36], attrs)
	}
	writeAttrHeader(attrStandardInformation, len(si), si)

	// $FILE_NAME
	nameU16 := windows.StringToUTF16(o.name)
	// drop trailing null
	if len(nameU16) > 0 && nameU16[len(nameU16)-1] == 0 {
		nameU16 = nameU16[:len(nameU16)-1]
	}
	fn := make([]byte, 66+len(nameU16)*2)
	binary.LittleEndian.PutUint64(fn[0:8], o.parentFRN)
	fn[64] = byte(len(nameU16))
	fn[65] = o.namespace
	for i, u := range nameU16 {
		binary.LittleEndian.PutUint16(fn[66+i*2:66+i*2+2], u)
	}
	writeAttrHeader(attrFileName, len(fn), fn)

	if o.altName != "" {
		altU16 := windows.StringToUTF16(o.altName)
		if len(altU16) > 0 && altU16[len(altU16)-1] == 0 {
			altU16 = altU16[:len(altU16)-1]
		}
		alt := make([]byte, 66+len(altU16)*2)
		binary.LittleEndian.PutUint64(alt[0:8], o.parentFRN)
		alt[64] = byte(len(altU16))
		alt[65] = o.altNamespace
		for i, u := range altU16 {
			binary.LittleEndian.PutUint16(alt[66+i*2:66+i*2+2], u)
		}
		writeAttrHeader(attrFileName, len(alt), alt)
	}

	if o.attrList {
		writeAttrHeader(attrAttributeList, 0, nil)
	}

	// $DATA
	if !o.isDir {
		if o.content != nil {
			o.size = int64(len(o.content))
		}
		if o.nonResident {
			// Non-resident header up to 0x38 with real size at 0x30.
			data := make([]byte, 0x40)
			data[0] = 0 // (placeholder; header byte written below)
			// We must write a full non-resident attribute manually since the
			// resident helper doesn't fit. Build it inline.
			attrLen := 0x48
			binary.LittleEndian.PutUint32(rec[pos:pos+4], attrData)
			binary.LittleEndian.PutUint32(rec[pos+4:pos+8], uint32(attrLen))
			rec[pos+8] = 1 // non-resident
			rec[pos+9] = 0 // name length 0 (unnamed)
			binary.LittleEndian.PutUint64(rec[pos+0x30:pos+0x38], uint64(o.size))
			pos += attrLen
		} else {
			nameLen := 0
			if o.namedData {
				nameLen = 4
			}
			// Resident $DATA: content length at 0x10, name length at 0x09.
			attrLen := 0x18 + int(o.size)
			if attrLen%8 != 0 {
				attrLen += 8 - attrLen%8
			}
			binary.LittleEndian.PutUint32(rec[pos:pos+4], attrData)
			binary.LittleEndian.PutUint32(rec[pos+4:pos+8], uint32(attrLen))
			rec[pos+8] = 0 // resident
			rec[pos+9] = byte(nameLen)
			binary.LittleEndian.PutUint32(rec[pos+0x10:pos+0x14], uint32(o.size))
			binary.LittleEndian.PutUint16(rec[pos+0x14:pos+0x16], 0x18)
			if o.content != nil {
				copy(rec[pos+0x18:pos+0x18+len(o.content)], o.content)
			}
			pos += attrLen
		}
	}

	// End marker
	binary.LittleEndian.PutUint32(rec[pos:pos+4], attrEnd)
}

// mftTableFromEntries builds the compact MFT table from a map, for tests that
// used to pass map[uint64]mftEntry directly.
func mftTableFromEntries(entries map[uint64]mftEntry) *mftTable {
	t := newMFTTable(0)
	for frn, e := range entries {
		e.frn = frn
		t.add(e)
	}
	return t
}

func TestParseMFTRecordFile(t *testing.T) {
	mod := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC).UnixNano()
	rec := synthMFTRecord(synthOpts{
		inUse: true, name: "report.pdf", namespace: fileNameWin32,
		parentFRN: 5, modUnixNano: mod, size: 4096, nonResident: true,
	})
	e, ok := parseMFTRecord(rec, 42, 512)
	if !ok {
		t.Fatal("expected record to parse")
	}
	if e.frn != 42 {
		t.Errorf("frn = %d, want 42", e.frn)
	}
	if e.name != "report.pdf" {
		t.Errorf("name = %q, want report.pdf", e.name)
	}
	if e.parentFRN != 5 {
		t.Errorf("parentFRN = %d, want 5", e.parentFRN)
	}
	if e.size != 4096 {
		t.Errorf("size = %d, want 4096", e.size)
	}
	if e.isDir {
		t.Error("file parsed as dir")
	}
	if e.modUnix == 0 {
		t.Error("modUnix not set")
	}
	// Allow 100ns rounding.
	if d := e.modUnix - mod; d > 100 || d < -100 {
		t.Errorf("modUnix = %d, want ~%d (delta %d)", e.modUnix, mod, d)
	}
}

func TestParseMFTRecordResidentSizeAndDir(t *testing.T) {
	rec := synthMFTRecord(synthOpts{
		inUse: true, name: "small.txt", namespace: fileNameWin32,
		parentFRN: 7, size: 123, nonResident: false,
	})
	e, ok := parseMFTRecord(rec, 9, 512)
	if !ok {
		t.Fatal("expected parse")
	}
	if e.size != 123 {
		t.Errorf("resident size = %d, want 123", e.size)
	}

	dir := synthMFTRecord(synthOpts{
		inUse: true, isDir: true, name: "src", namespace: fileNameWin32, parentFRN: 3,
	})
	de, ok := parseMFTRecord(dir, 11, 512)
	if !ok {
		t.Fatal("expected dir parse")
	}
	if !de.isDir {
		t.Error("dir not flagged")
	}
	if de.size != 0 {
		t.Errorf("dir size = %d, want 0", de.size)
	}
	if modeFromAttrs(de.attr) == 0 {
		t.Error("dir mode should have ModeDir bit")
	}
}

func TestParseMFTRecordReadsStandardInformationAttributes(t *testing.T) {
	rec := synthMFTRecord(synthOpts{
		inUse: true, name: "secret.txt", namespace: fileNameWin32,
		parentFRN: 7, attrs: fileAttributeHidden | fileAttributeSystem | fileAttributeArchive,
	})
	e, ok := parseMFTRecord(rec, 12, 512)
	if !ok {
		t.Fatal("expected parse")
	}
	want := uint32(fileAttributeHidden | fileAttributeSystem | fileAttributeArchive)
	if e.attr&want != want {
		t.Fatalf("attrs = %#x, want %#x", e.attr, want)
	}
	if modeFromAttrs(e.attr)&want != want {
		t.Fatalf("mode did not preserve attrs: %#x", modeFromAttrs(e.attr))
	}
}

func TestParseStandardInformationShortAttributeDoesNotPanic(t *testing.T) {
	var entry mftEntry
	parseStandardInformation([]byte{1, 2, 3}, &entry)
	if entry.modUnix != 0 || entry.attr != 0 {
		t.Fatalf("short standard information mutated entry: %+v", entry)
	}
}

func TestParseMFTRecordSkipsNotInUse(t *testing.T) {
	rec := synthMFTRecord(synthOpts{inUse: false, name: "deleted.txt", namespace: fileNameWin32})
	if _, ok := parseMFTRecord(rec, 1, 512); ok {
		t.Fatal("deleted record should be skipped")
	}
}

func TestParseMFTRecordPrefersWin32Name(t *testing.T) {
	// A DOS short name should not overwrite a Win32 long name regardless of order.
	rec := synthMFTRecord(synthOpts{
		inUse: true, name: "LongName.txt", namespace: fileNameWin32, parentFRN: 2, size: 1,
	})
	e, _ := parseMFTRecord(rec, 3, 512)
	if e.name != "LongName.txt" {
		t.Errorf("name = %q, want LongName.txt", e.name)
	}
}

func TestApplyFixupDetectsMismatch(t *testing.T) {
	rec := synthMFTRecord(synthOpts{inUse: true, name: "x", namespace: fileNameWin32})
	// Corrupt a sector-end signature.
	rec[512-2] = 0x00
	if err := applyFixup(append([]byte(nil), rec...), 512); err == nil {
		t.Error("expected fixup signature mismatch")
	}
}

func TestDecodeRunInt(t *testing.T) {
	if got := decodeRunInt([]byte{0x00, 0x01}, false); got != 256 {
		t.Errorf("unsigned = %d, want 256", got)
	}
	// signed negative: 0xFF = -1
	if got := decodeRunInt([]byte{0xFF}, true); got != -1 {
		t.Errorf("signed 0xFF = %d, want -1", got)
	}
}

func TestDecodeDataRuns(t *testing.T) {
	// One run: length 0x34 clusters at LCN 0x0123 (relative). Header 0x21 means
	// 1 length byte (0x34) and 2 offset bytes (0x23 0x01 -> 0x0123).
	runs := []byte{0x21, 0x34, 0x23, 0x01, 0x00}
	out, err := decodeDataRuns(runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].clusters != 0x34 || out[0].startLCN != 0x0123 {
		t.Fatalf("runs = %+v", out)
	}
}
