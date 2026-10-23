//go:build windows

package main

// FSCTL_GET_NTFS_FILE_RECORD reader for the small resident-file optimization.
// It requests only the file's MFT record; data clusters are never read.

import (
	"encoding/binary"
	"sync"

	"golang.org/x/sys/windows"
)

const fsctlGetNTFSFileRecord = 0x00090068

// NTFS record/attribute flags relevant to resident extraction. The record
// header's base-reference field at 0x20 is non-zero (and not this record) for
// an extension record; 0x20 is the $ATTRIBUTE_LIST type.
const (
	attrAttributeList       = 0x20
	ntfsAttrSparse          = 0x0200
	ntfsAttrReparsePoint    = 0x0400
	ntfsAttrCompressed      = 0x0800
	ntfsAttrEncrypted       = 0x4000
	mftBaseRefOffset        = 0x20
	mftSequenceOffset       = 16
	ntfsRecordRefMask       = 0x0000FFFFFFFFFFFF
	ntfsRecordHeaderMinSize = 48
)

// contentResidentRecord is one parsed resident $DATA result. data aliases the
// caller's record buffer.
type contentResidentRecord struct {
	data []byte
	seq  uint16
}

// parseMFTResidentRecord applies the update-sequence fixups and returns the
// unnamed $DATA bytes when they are resident and the file is safe to read from
// the MFT. reason names why a record is unusable (stable health codes).
func parseMFTResidentRecord(record []byte, recordNumber uint64, bytesPerSector int) (contentResidentRecord, string, bool) {
	if len(record) < ntfsRecordHeaderMinSize {
		return contentResidentRecord{}, contentResidentReasonParse, false
	}
	if binary.LittleEndian.Uint32(record[0:4]) != mftRecordMagic {
		return contentResidentRecord{}, contentResidentReasonParse, false
	}
	baseRef := binary.LittleEndian.Uint64(record[mftBaseRefOffset : mftBaseRefOffset+8])
	if baseRef != 0 && baseRef&ntfsRecordRefMask != recordNumber&ntfsRecordRefMask {
		return contentResidentRecord{}, contentResidentReasonExtension, false
	}
	seq := binary.LittleEndian.Uint16(record[mftSequenceOffset : mftSequenceOffset+2])
	if err := applyFixup(record, bytesPerSector); err != nil {
		return contentResidentRecord{}, contentResidentReasonParse, false
	}
	flags := binary.LittleEndian.Uint16(record[22:24])
	if flags&mftFlagInUse == 0 {
		return contentResidentRecord{}, contentResidentReasonNotInUse, false
	}
	if flags&mftFlagDir != 0 {
		return contentResidentRecord{}, contentResidentReasonDirectory, false
	}

	firstAttrOff := int(binary.LittleEndian.Uint16(record[20:22]))
	pos := firstAttrOff
	attrs := uint32(0)
	var data []byte
	haveData := false
	for pos+8 <= len(record) {
		attrType := binary.LittleEndian.Uint32(record[pos : pos+4])
		if attrType == attrEnd {
			break
		}
		attrLen := int(binary.LittleEndian.Uint32(record[pos+4 : pos+8]))
		if attrLen < 8 || pos+attrLen > len(record) {
			return contentResidentRecord{}, contentResidentReasonParse, false
		}
		attr := record[pos : pos+attrLen]
		nonResident := len(attr) > 8 && attr[8] != 0
		nameLen := 0
		if len(attr) > 9 {
			nameLen = int(attr[9])
		}
		switch attrType {
		case attrAttributeList:
			return contentResidentRecord{}, contentResidentReasonAttributeList, false
		case attrStandardInformation:
			// $STANDARD_INFORMATION: file attributes at content offset +32.
			if len(attr) >= 22 {
				if contentOff := int(binary.LittleEndian.Uint16(attr[20:22])); contentOff >= 0 && contentOff+36 <= len(attr) {
					attrs = binary.LittleEndian.Uint32(attr[contentOff+32 : contentOff+36])
				}
			}
		case attrFileName:
			// $FILE_NAME also carries the file attributes at content offset +56.
			if len(attr) >= 22 {
				if contentOff := int(binary.LittleEndian.Uint16(attr[20:22])); contentOff >= 0 && contentOff+60 <= len(attr) {
					attrs |= binary.LittleEndian.Uint32(attr[contentOff+56 : contentOff+60])
				}
			}
		case attrData:
			if nameLen != 0 {
				break // named stream: never the file content
			}
			if nonResident {
				return contentResidentRecord{}, contentResidentReasonNonResident, false
			}
			if haveData {
				return contentResidentRecord{}, contentResidentReasonParse, false
			}
			if len(attr) < 0x18 {
				return contentResidentRecord{}, contentResidentReasonParse, false
			}
			contentLen := int(binary.LittleEndian.Uint32(attr[0x10:0x14]))
			contentOff := int(binary.LittleEndian.Uint16(attr[0x14:0x16]))
			if contentLen < 0 || contentOff < 0x18 || contentOff+contentLen > len(attr) {
				return contentResidentRecord{}, contentResidentReasonParse, false
			}
			data = attr[contentOff : contentOff+contentLen]
			haveData = true
		}
		pos += attrLen
	}
	switch {
	case attrs&ntfsAttrEncrypted != 0:
		return contentResidentRecord{}, contentResidentReasonEncrypted, false
	case attrs&ntfsAttrCompressed != 0:
		return contentResidentRecord{}, contentResidentReasonCompressed, false
	case attrs&ntfsAttrSparse != 0:
		return contentResidentRecord{}, contentResidentReasonSparse, false
	case attrs&ntfsAttrReparsePoint != 0:
		return contentResidentRecord{}, contentResidentReasonReparse, false
	case !haveData:
		return contentResidentRecord{}, contentResidentReasonNoData, false
	}
	return contentResidentRecord{data: data, seq: seq}, "", true
}

// contentWindowsResidentSource reads resident $DATA through one volume handle,
// opened once per build and closed at the end.
type contentWindowsResidentSource struct {
	handle      windows.Handle
	bytesPerSec int
	recordSize  int
	mu          sync.Mutex
	stats       contentResidentStats
}

func contentOpenResidentSourceWindows(volume string) (contentResidentSource, error) {
	handle, err := openVolume(volume)
	if err != nil {
		return nil, err
	}
	vol, err := queryNTFSVolumeData(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	recSize := int(vol.BytesPerFileRecord)
	if recSize <= 0 || recSize > 64<<10 {
		recSize = 1024
	}
	bps := int(vol.BytesPerSector)
	if bps <= 0 {
		bps = 512
	}
	return &contentWindowsResidentSource{handle: handle, bytesPerSec: bps, recordSize: recSize}, nil
}

func (s *contentWindowsResidentSource) ResidentText(frn uint64, seq uint16, size int64) ([]byte, string, bool) {
	src := s
	if src == nil || src.handle == 0 {
		return nil, contentResidentReasonUnavailable, false
	}
	if frn == 0 {
		src.stats.fallback(contentResidentReasonParse)
		return nil, contentResidentReasonParse, false
	}
	ref := (uint64(seq) << 48) | (frn & ntfsRecordRefMask)
	var in [8]byte
	binary.LittleEndian.PutUint64(in[:], ref)
	out := make([]byte, 12+src.recordSize+64)

	src.mu.Lock()
	var ret uint32
	err := windows.DeviceIoControl(
		src.handle,
		fsctlGetNTFSFileRecord,
		&in[0], uint32(len(in)),
		&out[0], uint32(len(out)),
		&ret, nil,
	)
	src.mu.Unlock()
	if err != nil {
		src.stats.fallback(contentResidentReasonFSCTL)
		return nil, contentResidentReasonFSCTL, false
	}
	data, reason, ok := parseNTFSFileRecordOutput(out, ret, frn, seq, size, src.bytesPerSec)
	if !ok {
		src.stats.fallback(reason)
		return nil, reason, false
	}
	src.stats.hit()
	return data, "", true
}

// parseNTFSFileRecordOutput decodes one FSCTL_GET_NTFS_FILE_RECORD output and
// validates the file reference, record sequence, and data length against the
// name index. It returns a private copy of the resident bytes.
func parseNTFSFileRecordOutput(out []byte, ret uint32, frn uint64, seq uint16, size int64, bytesPerSec int) ([]byte, string, bool) {
	if ret < 12 || len(out) < 12 {
		return nil, contentResidentReasonParse, false
	}
	outRef := binary.LittleEndian.Uint64(out[0:8])
	outLen := int(binary.LittleEndian.Uint32(out[8:12]))
	if outLen <= 0 || 12+outLen > len(out) || int(ret) > len(out) || 12+outLen > int(ret) {
		return nil, contentResidentReasonParse, false
	}
	if outRef&ntfsRecordRefMask != frn&ntfsRecordRefMask {
		return nil, contentResidentReasonSequence, false
	}
	if seq != 0 && uint16(outRef>>48) != seq {
		return nil, contentResidentReasonSequence, false
	}
	// parseMFTResidentRecord applies fixups in place; keep the caller's
	// output intact so repeated parses of the same buffer stay valid.
	record := append([]byte(nil), out[12:12+outLen]...)
	parsed, reason, ok := parseMFTResidentRecord(record, frn, bytesPerSec)
	if !ok {
		return nil, reason, false
	}
	if seq != 0 && parsed.seq != seq {
		return nil, contentResidentReasonSequence, false
	}
	if size >= 0 && int64(len(parsed.data)) != size {
		return nil, contentResidentReasonSize, false
	}
	return append([]byte(nil), parsed.data...), "", true
}

func (s *contentWindowsResidentSource) Counters() contentResidentCounters {
	if s == nil {
		return contentResidentCounters{}
	}
	return s.stats.snapshot()
}

func (s *contentWindowsResidentSource) Close() {
	if s == nil || s.handle == 0 {
		return
	}
	_ = windows.CloseHandle(s.handle)
	s.handle = 0
}

// contentOpenResidentSource is the per-build volume-handle opener. Tests
// replace it to serve synthetic MFT records without a volume.
var contentOpenResidentSource = contentOpenResidentSourceWindows

// contentResidentSourceUnavailable builds a source that reports why every
// candidate fell back, for a setting that is on but the volume handle could
// not be opened (rarely: the service lacks the needed access).
func contentResidentSourceUnavailable(err error) contentResidentSource {
	return &contentUnavailableResidentSource{reason: contentResidentReasonUnavailable, err: err}
}

type contentUnavailableResidentSource struct {
	reason string
	err    error
	stats  contentResidentStats
}

func (s *contentUnavailableResidentSource) ResidentText(uint64, uint16, int64) ([]byte, string, bool) {
	s.stats.fallback(s.reason)
	return nil, s.reason, false
}

func (s *contentUnavailableResidentSource) Counters() contentResidentCounters {
	return s.stats.snapshot()
}

func (s *contentUnavailableResidentSource) Close() {}
