package main

import (
	"encoding/binary"
	"testing"
	"time"
	"unicode/utf16"
)

func TestParseUSNChangeBuffer(t *testing.T) {
	record := makeUSNRecordV2(0x000700000000002c, 0x0003000000000021, 1200, 0x00000100, fileAttributeDir, "created-dir")
	buffer := make([]byte, 8, 8+len(record))
	binary.LittleEndian.PutUint64(buffer[:8], 1300)
	buffer = append(buffer, record...)

	next, changes, err := parseUSNChangeBuffer(buffer)
	if err != nil {
		t.Fatalf("parseUSNChangeBuffer: %v", err)
	}
	if next != 1300 {
		t.Fatalf("next = %d, want 1300", next)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	got := changes[0]
	if got.FRN != 44 || got.ParentFRN != 33 || got.USN != 1200 || got.Reason != 0x00000100 || got.Attr != fileAttributeDir || got.Name != "created-dir" {
		t.Fatalf("change mismatch: %+v", got)
	}
}

func TestMakeReadUSNJournalRequestWaitFields(t *testing.T) {
	req := makeReadUSNJournalRequest(42, 100, 5*time.Second, 1)
	if req.StartUsn != 100 || req.UsnJournalID != 42 {
		t.Fatalf("request identity fields = %+v", req)
	}
	if req.Timeout != 5 || req.BytesToWaitFor != 1 {
		t.Fatalf("wait fields = timeout %d bytes %d, want 5/1", req.Timeout, req.BytesToWaitFor)
	}
	req = makeReadUSNJournalRequest(42, 100, 0, 0)
	if req.Timeout != 0 || req.BytesToWaitFor != 0 {
		t.Fatalf("non-wait fields = timeout %d bytes %d, want 0/0", req.Timeout, req.BytesToWaitFor)
	}
}

func TestParseUSNChangeBufferV3(t *testing.T) {
	record := makeUSNRecordV3(0x000d0000008b7674, 0x00010000008b766c, 0x382a600ac0, usnReasonFileCreate, 0x20, "specific_fixture_tool.py")
	buffer := make([]byte, 8, 8+len(record))
	binary.LittleEndian.PutUint64(buffer[:8], 0x382a600ad0)
	buffer = append(buffer, record...)

	next, changes, err := parseUSNChangeBuffer(buffer)
	if err != nil {
		t.Fatalf("parseUSNChangeBuffer: %v", err)
	}
	if next != 0x382a600ad0 {
		t.Fatalf("next = %d, want %d", next, uint64(0x382a600ad0))
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	got := changes[0]
	if got.FRN != 0x8b7674 || got.ParentFRN != 0x8b766c || got.USN != 0x382a600ac0 ||
		got.Reason != usnReasonFileCreate || got.Attr != 0x20 || got.Name != "specific_fixture_tool.py" {
		t.Fatalf("change mismatch: %+v", got)
	}
}

func TestParseUSNRecordsRejectsInvalidLength(t *testing.T) {
	record := make([]byte, 60)
	binary.LittleEndian.PutUint32(record[0:4], 59)
	if _, err := parseUSNRecords(record); err == nil {
		t.Fatal("parseUSNRecords succeeded with invalid length")
	}
}

func TestMergeUSNNodesIntoMFTAddsMissingRecords(t *testing.T) {
	entries := mftTableFromEntries(map[uint64]mftEntry{
		10: {frn: 10, parentFRN: 5, name: "existing.txt", attr: 0x20, size: 99},
	})
	nodes := map[uint64]usnNode{
		10: {frn: 10, parentFRN: 5, name: "existing-renamed.txt", attr: 0x20},
		11: {frn: 11, parentFRN: 5, name: "missing-dir", attr: fileAttributeDir},
	}
	if got := mergeUSNNodesIntoMFT(entries, nodes); got != 1 {
		t.Fatalf("added = %d, want 1", got)
	}
	if e, ok := entries.get(10); !ok || e.name != "existing.txt" || e.size != 99 {
		t.Fatalf("existing MFT entry was overwritten: %+v ok=%v", e, ok)
	}
	if e, ok := entries.get(11); !ok || e.name != "missing-dir" || e.parentFRN != 5 || !e.isDir || !e.inUse {
		t.Fatalf("merged entry = %+v ok=%v, want USN-backed dir", e, ok)
	}
}

func TestFilterMFTExclusions(t *testing.T) {
	entries := mftTableFromEntries(map[uint64]mftEntry{
		5:  {frn: 5, parentFRN: 5, name: "", attr: fileAttributeDir, isDir: true},
		6:  {frn: 6, parentFRN: 5, name: "ProgramData", attr: fileAttributeDir, isDir: true},
		7:  {frn: 7, parentFRN: 6, name: "seekfs", attr: fileAttributeDir, isDir: true},
		8:  {frn: 8, parentFRN: 7, name: "seekfs.toml", attr: 0x20},
		9:  {frn: 9, parentFRN: 7, name: "indexes", attr: fileAttributeDir, isDir: true},
		10: {frn: 10, parentFRN: 9, name: "seekfs_c.gsi", attr: 0x20},
		11: {frn: 11, parentFRN: 5, name: "keep.txt", attr: 0x20},
		12: {frn: 12, parentFRN: 5, name: "System Volume Information", attr: fileAttributeDir, isDir: true},
		13: {frn: 13, parentFRN: 12, name: "{GUID}", attr: fileAttributeDir, isDir: true},
	})
	removed := filterMFTExclusions(entries, `C:`, []string{ // C: like normalizeVolume returns
		`C:\ProgramData\seekfs`,
		`C:\System Volume Information`,
		`D:\Other-Volume\seekfs`, // not on this volume: ignored
		`C:\NoSuch\Thing`,        // unresolvable: ignored
	})
	if removed != 6 {
		t.Fatalf("removed = %d, want 6 (seekfs subtree + SVI subtree)", removed)
	}
	for frn, want := range map[uint64]bool{7: false, 8: false, 9: false, 10: false, 12: false, 13: false} {
		if _, ok := entries.get(frn); ok != want {
			t.Fatalf("frn %d present=%v want %v", frn, ok, want)
		}
	}
	for _, frn := range []uint64{5, 6, 11} {
		if _, ok := entries.get(frn); !ok {
			t.Fatalf("frn %d unexpectedly removed", frn)
		}
	}
}

func TestFilterMFTExclusionsNoMatchKeepsAll(t *testing.T) {
	entries := mftTableFromEntries(map[uint64]mftEntry{
		5:  {frn: 5, parentFRN: 5, name: "", attr: fileAttributeDir, isDir: true},
		11: {frn: 11, parentFRN: 5, name: "keep.txt", attr: 0x20},
	})
	if got := filterMFTExclusions(entries, `C:\`, []string{`C:\ProgramData\seekfs`}); got != 0 {
		t.Fatalf("removed = %d, want 0", got)
	}
	if _, ok := entries.get(11); !ok {
		t.Fatal("keep.txt removed despite no matching exclusion")
	}
}

func makeUSNRecordV2(frn, parent uint64, usn int64, reason, attr uint32, name string) []byte {
	nameUTF16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(nameUTF16)*2)
	for i, v := range nameUTF16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], v)
	}
	recordLen := uint32(60 + len(nameBytes))
	record := make([]byte, recordLen)
	binary.LittleEndian.PutUint32(record[0:4], recordLen)
	binary.LittleEndian.PutUint16(record[4:6], 2)
	binary.LittleEndian.PutUint16(record[6:8], 0)
	binary.LittleEndian.PutUint64(record[8:16], frn)
	binary.LittleEndian.PutUint64(record[16:24], parent)
	binary.LittleEndian.PutUint64(record[24:32], uint64(usn))
	binary.LittleEndian.PutUint32(record[40:44], reason)
	binary.LittleEndian.PutUint32(record[52:56], attr)
	binary.LittleEndian.PutUint16(record[56:58], uint16(len(nameBytes)))
	binary.LittleEndian.PutUint16(record[58:60], 60)
	copy(record[60:], nameBytes)
	return record
}

func makeUSNRecordV3(frn, parent uint64, usn int64, reason, attr uint32, name string) []byte {
	nameUTF16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(nameUTF16)*2)
	for i, v := range nameUTF16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], v)
	}
	recordLen := uint32(76 + len(nameBytes))
	record := make([]byte, recordLen)
	binary.LittleEndian.PutUint32(record[0:4], recordLen)
	binary.LittleEndian.PutUint16(record[4:6], 3)
	binary.LittleEndian.PutUint16(record[6:8], 0)
	binary.LittleEndian.PutUint64(record[8:16], frn)
	binary.LittleEndian.PutUint64(record[24:32], parent)
	binary.LittleEndian.PutUint64(record[40:48], uint64(usn))
	binary.LittleEndian.PutUint32(record[56:60], reason)
	binary.LittleEndian.PutUint32(record[68:72], attr)
	binary.LittleEndian.PutUint16(record[72:74], uint16(len(nameBytes)))
	binary.LittleEndian.PutUint16(record[74:76], 76)
	copy(record[76:], nameBytes)
	return record
}
