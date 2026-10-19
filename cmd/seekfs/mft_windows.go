//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file reads file metadata (name, parent, size, modification time,
// attributes) directly from the NTFS Master File Table, the same source
// Everything uses for its initial index. FSCTL_ENUM_USN_DATA gives names but no
// size or real timestamps; the MFT carries all of it in each record's
// $STANDARD_INFORMATION, $FILE_NAME, and $DATA attributes.
//
// The parsing core (parseMFTRecord, applyFixup) is pure and unit-tested with
// synthetic records; the I/O layer (enumMFT) reads the raw $MFT via the volume
// handle and feeds records through the core.

const fsctlGetNTFSVolumeData = 0x00090064

// NTFS on-disk constants.
const (
	mftRecordMagic = 0x454c4946 // "FILE"

	mftFlagInUse = 0x0001
	mftFlagDir   = 0x0002

	attrStandardInformation = 0x10
	attrFileName            = 0x30
	attrData                = 0x80
	attrEnd                 = 0xffffffff

	// $FILE_NAME namespaces.
	fileNamePOSIX = 0
	fileNameWin32 = 1
	fileNameDOS   = 2
	fileNameBoth  = 3

	// FILETIME epoch (1601-01-01) to Unix epoch (1970-01-01) in 100ns ticks.
	filetimeToUnix100ns = 116444736000000000
)

// ntfsVolumeDataBuffer mirrors NTFS_VOLUME_DATA_BUFFER (leading fields only).
type ntfsVolumeDataBuffer struct {
	VolumeSerialNumber    int64
	NumberSectors         int64
	TotalClusters         int64
	FreeClusters          int64
	TotalReserved         int64
	BytesPerSector        uint32
	BytesPerCluster       uint32
	BytesPerFileRecord    uint32
	ClustersPerFileRecord uint32
	MftValidDataLength    int64
	MftStartLcn           int64
	Mft2StartLcn          int64
	MftZoneStart          int64
	MftZoneEnd            int64
}

// mftEntry is the metadata extracted from one MFT record.
type mftEntry struct {
	frn       uint64
	parentFRN uint64
	name      string
	attr      uint32 // Windows FILE_ATTRIBUTE_* flags (dir bit synthesized)
	size      int64
	modUnix   int64 // unix nanoseconds, 0 if unknown
	isDir     bool
	inUse     bool
	seq       uint16 // this record's NTFS file-reference sequence number
	parentSeq uint16 // the parent reference's sequence number, when known
}

// filetimeToUnixNano converts a Windows FILETIME (100ns ticks since 1601) to
// unix nanoseconds. Returns 0 for a zero/!valid timestamp.
func filetimeToUnixNano(ft uint64) int64 {
	if ft == 0 || ft < filetimeToUnix100ns {
		return 0
	}
	return int64(ft-filetimeToUnix100ns) * 100
}

// applyFixup applies the NTFS update sequence array to an in-place record
// buffer. NTFS stores a 2-byte "update sequence number" at the end of each
// sector within a record; the real bytes live in the update sequence array in
// the record header. This must be undone before parsing. bytesPerSector is
// typically 512. Returns an error if the record is inconsistent.
func applyFixup(record []byte, bytesPerSector int) error {
	if len(record) < 8 {
		return errors.New("mft record too small for header")
	}
	usaOffset := int(binary.LittleEndian.Uint16(record[4:6]))
	usaCount := int(binary.LittleEndian.Uint16(record[6:8]))
	if usaCount == 0 {
		return errors.New("mft record has empty update sequence array")
	}
	// usaCount includes the USN itself plus one entry per sector.
	sectors := usaCount - 1
	if usaOffset+usaCount*2 > len(record) {
		return errors.New("update sequence array out of bounds")
	}
	usn := record[usaOffset : usaOffset+2]
	for i := 0; i < sectors; i++ {
		sectorEnd := (i+1)*bytesPerSector - 2
		if sectorEnd+2 > len(record) {
			return errors.New("fixup sector out of bounds")
		}
		// The last two bytes of each sector must currently equal the USN.
		if record[sectorEnd] != usn[0] || record[sectorEnd+1] != usn[1] {
			return errors.New("fixup signature mismatch")
		}
		entry := record[usaOffset+2+i*2 : usaOffset+2+i*2+2]
		record[sectorEnd] = entry[0]
		record[sectorEnd+1] = entry[1]
	}
	return nil
}

// parseMFTRecord parses one fixed-up MFT record at recordNumber into an
// mftEntry. ok is false when the record is not a valid in-use file/dir record
// that should be indexed. bytesPerSector is needed for the fixup.
func parseMFTRecord(record []byte, recordNumber uint64, bytesPerSector int) (mftEntry, bool) {
	if len(record) < 48 {
		return mftEntry{}, false
	}
	if binary.LittleEndian.Uint32(record[0:4]) != mftRecordMagic {
		return mftEntry{}, false
	}
	if err := applyFixup(record, bytesPerSector); err != nil {
		return mftEntry{}, false
	}
	flags := binary.LittleEndian.Uint16(record[22:24])
	inUse := flags&mftFlagInUse != 0
	if !inUse {
		return mftEntry{}, false
	}
	isDir := flags&mftFlagDir != 0

	// The MFT record's own number forms the FRN (low 48 bits); the sequence
	// number occupies the high 16, but USN/journal FRNs the rest of seekfs uses
	// are the 48-bit base record number, so we key on recordNumber while
	// retaining the sequence separately to disambiguate record reuse.
	entry := mftEntry{
		frn:   recordNumber,
		isDir: isDir,
		inUse: true,
		attr:  fileAttributeFromDir(isDir),
		seq:   binary.LittleEndian.Uint16(record[16:18]),
	}

	firstAttrOff := int(binary.LittleEndian.Uint16(record[20:22]))
	pos := firstAttrOff
	bestNameSpace := -1
	for pos+8 <= len(record) {
		attrType := binary.LittleEndian.Uint32(record[pos : pos+4])
		if attrType == attrEnd {
			break
		}
		attrLen := int(binary.LittleEndian.Uint32(record[pos+4 : pos+8]))
		if attrLen < 8 || pos+attrLen > len(record) {
			break
		}
		nonResident := record[pos+8] != 0

		switch attrType {
		case attrStandardInformation:
			parseStandardInformation(record[pos:pos+attrLen], &entry)
		case attrFileName:
			parseFileName(record[pos:pos+attrLen], &entry, &bestNameSpace)
		case attrData:
			parseDataSize(record[pos:pos+attrLen], nonResident, isDir, &entry)
		}
		pos += attrLen
	}

	if entry.name == "" {
		return mftEntry{}, false
	}
	return entry, true
}

func fileAttributeFromDir(isDir bool) uint32 {
	if isDir {
		return fileAttributeDir
	}
	return 0
}

// parseStandardInformation reads the modification time from a resident
// $STANDARD_INFORMATION attribute.
func parseStandardInformation(attr []byte, entry *mftEntry) {
	if len(attr) < 22 {
		return
	}
	contentOff := int(binary.LittleEndian.Uint16(attr[20:22]))
	// $STANDARD_INFORMATION layout: 0 created, 8 modified, 32 file attributes.
	if contentOff+16 > len(attr) {
		return
	}
	modified := binary.LittleEndian.Uint64(attr[contentOff+8 : contentOff+16])
	if t := filetimeToUnixNano(modified); t != 0 {
		entry.modUnix = t
	}
	if contentOff+36 <= len(attr) {
		attrs := binary.LittleEndian.Uint32(attr[contentOff+32 : contentOff+36])
		if attrs != 0 {
			entry.attr = attrs
			entry.isDir = attrs&fileAttributeDir != 0
		}
	}
}

// parseFileName reads the parent FRN and name from a resident $FILE_NAME
// attribute, preferring the Win32 (long) name over DOS (8.3). bestNameSpace
// tracks which namespace the current entry.name came from.
func parseFileName(attr []byte, entry *mftEntry, bestNameSpace *int) {
	contentOff := int(binary.LittleEndian.Uint16(attr[20:22]))
	if contentOff+66 > len(attr) {
		return
	}
	c := attr[contentOff:]
	parentRef := binary.LittleEndian.Uint64(c[0:8])
	parentFRN := parentRef & 0x0000FFFFFFFFFFFF // low 48 bits
	parentSeq := uint16(parentRef >> 48)        // high 16 bits: parent reference generation
	nameLen := int(c[64])
	namespace := int(c[65])
	nameStart := 66
	if nameStart+nameLen*2 > len(c) {
		return
	}
	// Prefer Win32/Both/POSIX over DOS. Higher preference wins.
	pref := nameSpacePreference(namespace)
	if pref <= *bestNameSpace {
		// Still record the parent even if we keep an existing better name.
		if entry.parentFRN == 0 {
			entry.parentFRN = parentFRN
			entry.parentSeq = parentSeq
		}
		return
	}
	u16 := make([]uint16, nameLen)
	for i := 0; i < nameLen; i++ {
		u16[i] = binary.LittleEndian.Uint16(c[nameStart+i*2 : nameStart+i*2+2])
	}
	entry.name = windows.UTF16ToString(u16)
	entry.parentFRN = parentFRN
	entry.parentSeq = parentSeq
	*bestNameSpace = pref
}

// nameSpacePreference ranks $FILE_NAME namespaces: prefer a real long name.
func nameSpacePreference(ns int) int {
	switch ns {
	case fileNameWin32, fileNameBoth:
		return 3
	case fileNamePOSIX:
		return 2
	case fileNameDOS:
		return 1
	default:
		return 0
	}
}

// parseDataSize reads the logical file size from the unnamed $DATA attribute.
// For resident data the size is the content length; for non-resident data it is
// the "real size" (data length) field. Directories have no $DATA size.
func parseDataSize(attr []byte, nonResident, isDir bool, entry *mftEntry) {
	if isDir {
		return
	}
	// Only the unnamed $DATA stream counts as the file size. Named streams have
	// a non-zero name length at offset 9.
	nameLen := int(attr[9])
	if nameLen != 0 {
		return
	}
	if nonResident {
		// Non-resident header: real size (data length) at offset 0x30.
		if len(attr) >= 0x38 {
			entry.size = int64(binary.LittleEndian.Uint64(attr[0x30:0x38]))
		}
		return
	}
	// Resident: content length at 0x10, content offset at 0x14.
	if len(attr) >= 0x18 {
		entry.size = int64(binary.LittleEndian.Uint32(attr[0x10:0x14]))
	}
}

// queryNTFSVolumeData returns NTFS geometry for the open volume handle.
func queryNTFSVolumeData(handle windows.Handle) (ntfsVolumeDataBuffer, error) {
	var data ntfsVolumeDataBuffer
	var ret uint32
	err := windows.DeviceIoControl(
		handle,
		fsctlGetNTFSVolumeData,
		nil, 0,
		(*byte)(unsafe.Pointer(&data)),
		uint32(unsafe.Sizeof(data)),
		&ret, nil,
	)
	if err != nil {
		return data, fmt.Errorf("FSCTL_GET_NTFS_VOLUME_DATA: %w", err)
	}
	return data, nil
}

// directVolumeSource builds a record source for one NTFS volume from the raw
// $MFT, merging FSCTL_ENUM_USN_DATA names for any FRNs the MFT read missed.
// This is the Everything-style fast build path (no directory walk), and like
// Everything it requires an elevated raw-volume handle.  It returns the source,
// the volume root string, and the USN journal identity/checkpoint so the
// published v9 index can resume incremental replay from exactly this point.
// exclusions are paths (on this volume) whose whole subtrees are dropped from
// the result; the seekfs dir and standard system folders are passed in.
func directVolumeSource(volume string, exclusions []string) (directRecordSource, []string, uint64, int64, error) {
	vol := normalizeVolume(volume)
	handle, err := openVolume(vol)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	defer windows.CloseHandle(handle)
	journal, err := queryUSNJournal(handle)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	entries, err := enumMFT(handle)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	if nodes, usnErr := enumUSN(handle, journal.NextUsn); usnErr == nil {
		if added := mergeUSNNodesIntoMFT(entries, nodes); added > 0 {
			serviceLog("direct volume=%s mft=%d usn-merged=%d", vol, len(entries), added)
		}
	} else {
		serviceLog("direct volume=%s usn merge skipped err=%v", vol, usnErr)
	}
	if filtered := filterMFTExclusions(entries, vol, exclusions); filtered > 0 {
		serviceLog("direct volume=%s excluded=%d remaining=%d", vol, filtered, len(entries))
	}
	return newDirectMFTSource(entries), []string{vol + `\`}, journal.UsnJournalID, journal.NextUsn, nil
}

// mftRootFRN is the MFT record number of the NTFS root directory.
const mftRootFRN = 5

// filterMFTExclusions removes every entry under any exclusion path that lives on
// this volume. Each exclusion is resolved to a directory FRN by descending the
// parent links from the volume root, then the whole subtree under that FRN is
// dropped from entries. It returns the number of entries removed.
func filterMFTExclusions(entries map[uint64]mftEntry, vol string, exclusions []string) int {
	if len(exclusions) == 0 || len(entries) == 0 {
		return 0
	}
	children := make(map[uint64][]uint64, len(entries)/2)
	for frn, e := range entries {
		if e.parentFRN != 0 && e.parentFRN != frn {
			children[e.parentFRN] = append(children[e.parentFRN], frn)
		}
	}
	var roots []uint64
	volRoot := strings.ToUpper(strings.TrimRight(vol, `\`)) + `\`
	for _, exclusion := range exclusions {
		canonical, err := filepath.Abs(exclusion)
		if err != nil {
			continue
		}
		canonical = filepath.Clean(canonical)
		rel, err := filepath.Rel(volRoot, canonical)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		components := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
		cur := uint64(mftRootFRN)
		resolved := true
		for _, component := range components {
			var next uint64
			for _, frn := range children[cur] {
				if strings.EqualFold(entries[frn].name, component) {
					next = frn
					break
				}
			}
			if next == 0 {
				resolved = false
				break
			}
			cur = next
		}
		if resolved {
			roots = append(roots, cur)
		}
	}
	if len(roots) == 0 {
		return 0
	}
	excluded := make(map[uint64]bool, len(roots))
	queue := roots
	for len(queue) > 0 {
		frn := queue[0]
		queue = queue[1:]
		if excluded[frn] {
			continue
		}
		excluded[frn] = true
		queue = append(queue, children[frn]...)
	}
	removed := 0
	for frn := range excluded {
		if _, ok := entries[frn]; ok {
			delete(entries, frn)
			removed++
		}
	}
	return removed
}

// enumMFT reads every in-use file/dir record from the volume's $MFT and returns
// them keyed by FRN. It reads the MFT through the $DATA attribute of MFT record
// 0 so a fragmented MFT is handled. modUnix and size are populated.
func enumMFT(handle windows.Handle) (map[uint64]mftEntry, error) {
	vol, err := queryNTFSVolumeData(handle)
	if err != nil {
		return nil, err
	}
	recSize := int(vol.BytesPerFileRecord)
	if recSize <= 0 || recSize > 1<<20 {
		return nil, fmt.Errorf("implausible MFT record size %d", recSize)
	}
	bytesPerSector := int(vol.BytesPerSector)
	if bytesPerSector <= 0 {
		bytesPerSector = 512
	}

	runs, err := readMFTDataRuns(handle, vol, recSize, bytesPerSector)
	if err != nil {
		return nil, err
	}

	nodes := make(map[uint64]mftEntry, 1<<20)
	bytesPerCluster := int64(vol.BytesPerCluster)

	// Read in chunks that are a whole number of MFT records AND a multiple of the
	// cluster size, so every raw-volume read is properly aligned (unaligned reads
	// on a volume handle fail with ERROR_INVALID_PARAMETER). lcm of recSize and
	// cluster size, scaled up toward ~1 MiB.
	chunkUnit := lcmInt(int64(recSize), bytesPerCluster)
	chunkSize := chunkUnit
	for chunkSize < (1 << 20) {
		chunkSize += chunkUnit
	}
	buf := make([]byte, chunkSize)

	// The MFT's valid data length bounds how far the data runs are actually
	// populated; reading past it returns zeroed/garbage records (harmless, but we
	// stop to avoid wasted reads).
	mftValid := vol.MftValidDataLength

	var recordNumber uint64
	var consumed int64
	readErrors := 0
	for _, run := range runs {
		runBytes := run.clusters * bytesPerCluster
		offset := run.startLCN * bytesPerCluster
		remaining := runBytes
		for remaining > 0 {
			chunk := int64(len(buf))
			if chunk > remaining {
				chunk = remaining // still cluster-aligned: remaining is a cluster multiple
			}
			n, rerr := readVolumeAt(handle, offset, buf[:chunk])
			if rerr != nil {
				// Skip this chunk rather than aborting the whole volume: a single
				// bad run or transient read shouldn't lose every record's size.
				readErrors++
				if readErrors > 64 {
					return nil, fmt.Errorf("too many MFT read errors (last at offset %d): %w", offset, rerr)
				}
				recordNumber += uint64(chunk / int64(recSize))
				offset += chunk
				remaining -= chunk
				continue
			}
			if n == 0 {
				break
			}
			for off := 0; off+recSize <= n; off += recSize {
				parseMFTRecordInto(buf[off:off+recSize], recordNumber, bytesPerSector, nodes)
				recordNumber++
			}
			offset += int64(n)
			remaining -= int64(n)
			consumed += int64(n)
		}
		if mftValid > 0 && consumed >= mftValid {
			break
		}
	}
	if len(nodes) == 0 {
		return nil, errors.New("MFT enumeration produced no records")
	}
	return nodes, nil
}

// parseMFTRecordInto parses a record and inserts it into nodes if valid. It
// copies the record before fixup so the shared read buffer is not mutated.
func parseMFTRecordInto(src []byte, recordNumber uint64, bytesPerSector int, nodes map[uint64]mftEntry) {
	rec := make([]byte, len(src))
	copy(rec, src)
	if entry, ok := parseMFTRecord(rec, recordNumber, bytesPerSector); ok {
		nodes[entry.frn] = entry
	}
}

// lcmInt returns the least common multiple of a and b.
func lcmInt(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	return a / gcdInt(a, b) * b
}

func gcdInt(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

// mftRun is one extent of the $MFT on disk.
type mftRun struct {
	startLCN int64
	clusters int64
}

// readMFTDataRuns reads MFT record 0 ($MFT itself) and decodes the data run
// list of its unnamed $DATA attribute, yielding where the MFT lives on disk.
func readMFTDataRuns(handle windows.Handle, vol ntfsVolumeDataBuffer, recSize, bytesPerSector int) ([]mftRun, error) {
	bytesPerCluster := int64(vol.BytesPerCluster)
	mftOffset := vol.MftStartLcn * bytesPerCluster
	rec := make([]byte, recSize)
	if _, err := readVolumeAt(handle, mftOffset, rec); err != nil {
		return nil, fmt.Errorf("read $MFT record 0: %w", err)
	}
	if binary.LittleEndian.Uint32(rec[0:4]) != mftRecordMagic {
		return nil, errors.New("$MFT record 0 missing FILE signature")
	}
	if err := applyFixup(rec, bytesPerSector); err != nil {
		return nil, fmt.Errorf("$MFT record 0 fixup: %w", err)
	}
	pos := int(binary.LittleEndian.Uint16(rec[20:22]))
	for pos+8 <= len(rec) {
		attrType := binary.LittleEndian.Uint32(rec[pos : pos+4])
		if attrType == attrEnd {
			break
		}
		attrLen := int(binary.LittleEndian.Uint32(rec[pos+4 : pos+8]))
		if attrLen < 8 || pos+attrLen > len(rec) {
			break
		}
		if attrType == attrData && rec[pos+9] == 0 && rec[pos+8] != 0 {
			// Non-resident unnamed $DATA: data runs start at the run offset.
			runOff := int(binary.LittleEndian.Uint16(rec[pos+0x20 : pos+0x22]))
			return decodeDataRuns(rec[pos+runOff : pos+attrLen])
		}
		pos += attrLen
	}
	return nil, errors.New("$MFT $DATA runlist not found")
}

// decodeDataRuns decodes an NTFS data run list into absolute-LCN runs.
func decodeDataRuns(runs []byte) ([]mftRun, error) {
	out := make([]mftRun, 0, 8)
	var prevLCN int64
	i := 0
	for i < len(runs) {
		header := runs[i]
		if header == 0 {
			break
		}
		lenBytes := int(header & 0x0f)
		offBytes := int(header >> 4)
		i++
		if lenBytes == 0 || i+lenBytes+offBytes > len(runs) {
			break
		}
		length := decodeRunInt(runs[i:i+lenBytes], false)
		i += lenBytes
		var lcnDelta int64
		if offBytes > 0 {
			lcnDelta = decodeRunInt(runs[i:i+offBytes], true)
			i += offBytes
		}
		prevLCN += lcnDelta
		if length > 0 && prevLCN >= 0 {
			out = append(out, mftRun{startLCN: prevLCN, clusters: length})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("empty $MFT data run list")
	}
	return out, nil
}

// decodeRunInt decodes a little-endian variable-length integer used in NTFS data
// runs. When signed, the value is sign-extended (run offsets can be negative).
func decodeRunInt(b []byte, signed bool) int64 {
	var v int64
	for i := len(b) - 1; i >= 0; i-- {
		v = (v << 8) | int64(b[i])
	}
	if signed && len(b) > 0 && b[len(b)-1]&0x80 != 0 {
		// Sign-extend.
		shift := uint(8 * len(b))
		v -= 1 << shift
	}
	return v
}

// readVolumeAt reads len(buf) bytes from the volume at byteOffset using an
// overlapped (positioned) read, which is required for raw volume access.
func readVolumeAt(handle windows.Handle, byteOffset int64, buf []byte) (int, error) {
	var overlapped windows.Overlapped
	overlapped.Offset = uint32(byteOffset & 0xffffffff)
	overlapped.OffsetHigh = uint32(uint64(byteOffset) >> 32)
	var read uint32
	err := windows.ReadFile(handle, buf, &read, &overlapped)
	if err != nil && err != windows.ERROR_HANDLE_EOF {
		return int(read), fmt.Errorf("read volume at %d: %w", byteOffset, err)
	}
	return int(read), nil
}

// mftEntryModTime is a small helper for tests/log formatting.
func mftEntryModTime(e mftEntry) time.Time {
	if e.modUnix == 0 {
		return time.Time{}
	}
	return time.Unix(0, e.modUnix)
}
