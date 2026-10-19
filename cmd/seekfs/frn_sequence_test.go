package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLegacyV1WAL writes a single v1 (pre-sequence) binary WAL frame exactly as
// the previous release would have. The frame carries no sequence field.
func writeLegacyV1WAL(t *testing.T, db string, nextUSN int64, changes []usnChange) {
	t.Helper()
	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.LittleEndian, nextUSN)
	_ = binary.Write(&payload, binary.LittleEndian, uint32(len(changes)))
	for _, c := range changes {
		_ = binary.Write(&payload, binary.LittleEndian, c.FRN)
		_ = binary.Write(&payload, binary.LittleEndian, c.ParentFRN)
		_ = binary.Write(&payload, binary.LittleEndian, c.USN)
		_ = binary.Write(&payload, binary.LittleEndian, c.Reason)
		_ = binary.Write(&payload, binary.LittleEndian, c.Attr)
		_ = binary.Write(&payload, binary.LittleEndian, uint16(len(c.Name)))
		_, _ = payload.WriteString(c.Name)
	}
	var frame bytes.Buffer
	frame.Write(walMagicV1)
	var header [8]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(payload.Len()))
	binary.LittleEndian.PutUint32(header[4:8], crc32.ChecksumIEEE(payload.Bytes()))
	frame.Write(header[:])
	frame.Write(payload.Bytes())
	if err := os.WriteFile(walPath(db), frame.Bytes(), 0o644); err != nil {
		t.Fatalf("write legacy wal: %v", err)
	}
}

func TestFRNSeqSectionSaveLoadRoundTrip(t *testing.T) {
	idx := &Index{
		Version:      indexVersion,
		Roots:        []string{`C:\`},
		BuiltAt:      time.Unix(0, 1),
		Source:       "usn",
		Volume:       "C:",
		JournalID:    42,
		Checkpoint:   99,
		Compact:      true,
		CompactAttrs: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), Seq: 7},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "main.go", Mode: modeFromAttrs(fileAttributeArchive), Size: 5, Seq: 42},
		},
	}
	db := filepath.Join(t.TempDir(), "seq.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}
	got, err := loadIndex(db)
	if err != nil {
		t.Fatalf("loadIndex: %v", err)
	}
	if len(got.Derived.FRNSeq) != 2 || got.Derived.FRNSeq[0] != 7 || got.Derived.FRNSeq[1] != 42 {
		t.Fatalf("derived FRNSeq = %v, want [7 42]", got.Derived.FRNSeq)
	}
	if got.Records[0].Seq != 7 || got.Records[1].Seq != 42 {
		t.Fatalf("record Seq = [%d %d], want [7 42]", got.Records[0].Seq, got.Records[1].Seq)
	}
	sections, _ := derivedSectionInfo(got.Derived)
	found := false
	for _, s := range sections {
		if s == "SEQN" {
			found = true
		}
	}
	if !found {
		t.Fatalf("SEQN section missing from %v", sections)
	}
}

func TestFRNSeqOldIndexWithoutSeqLoadsAsZero(t *testing.T) {
	// A legacy index has no SEQN section; save a sequence-free index (nothing
	// is emitted) and stamp the header version back to v9 to model one exactly.
	idx := &Index{
		Version:      legacyIndexVersion,
		Roots:        []string{`C:\`},
		BuiltAt:      time.Unix(0, 1),
		Source:       "usn",
		Volume:       "C:",
		Checkpoint:   0,
		Compact:      true,
		CompactAttrs: true,
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 11, ParentFRN: 10, Parent: 0, Name: "main.go", Size: 5},
		},
	}
	db := filepath.Join(t.TempDir(), "legacy.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}
	data, err := os.ReadFile(db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	binary.LittleEndian.PutUint32(data[8:12], legacyIndexVersion) // header version -> v9
	if err := os.WriteFile(db, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := loadIndex(db)
	if err != nil {
		t.Fatalf("loadIndex of v9 index: %v", err)
	}
	if got.Version != legacyIndexVersion {
		t.Fatalf("version = %d, want %d", got.Version, legacyIndexVersion)
	}
	if len(got.Derived.FRNSeq) != 0 {
		t.Fatalf("legacy index produced FRNSeq = %v, want none", got.Derived.FRNSeq)
	}
	for i, rec := range got.Records {
		if rec.Seq != 0 {
			t.Fatalf("record %d Seq = %d, want 0 (unknown)", i, rec.Seq)
		}
	}
}

func TestWALFrameSeqRoundTrip(t *testing.T) {
	changes := []usnChange{
		{FRN: 102, ParentFRN: 100, USN: 12, Reason: usnReasonFileCreate, Attr: fileAttributeArchive, Name: "created.txt", Seq: 9},
		{FRN: 103, ParentFRN: 100, USN: 13, Reason: usnReasonFileDelete, Name: "gone.txt", Seq: 11},
	}
	payload, err := encodeBinaryWALBatch(14, changes)
	if err != nil {
		t.Fatalf("encodeBinaryWALBatch: %v", err)
	}
	batch, err := decodeBinaryWALBatch(payload, true)
	if err != nil {
		t.Fatalf("decodeBinaryWALBatch: %v", err)
	}
	if batch.NextUSN != 14 || len(batch.Changes) != 2 {
		t.Fatalf("batch = %+v", batch)
	}
	if batch.Changes[0].Seq != 9 || batch.Changes[1].Seq != 11 {
		t.Fatalf("decoded seq = [%d %d], want [9 11]", batch.Changes[0].Seq, batch.Changes[1].Seq)
	}
	if batch.Changes[0].Name != "created.txt" || batch.Changes[1].FRN != 103 {
		t.Fatalf("decoded change fields = %+v", batch.Changes)
	}
}

func TestWALOldMagicFrameDecodesAsSeqZero(t *testing.T) {
	db := filepath.Join(t.TempDir(), "legacy.gsi")
	writeLegacyV1WAL(t, db, 12, []usnChange{{
		FRN:       102,
		ParentFRN: 100,
		USN:       12,
		Reason:    usnReasonFileCreate,
		Name:      "legacy-created.txt",
		Seq:       77, // must NOT survive: v1 frames carry no sequence
	}})
	base := func() *Index {
		return &Index{
			Source:     "usn",
			Volume:     "F:",
			Compact:    true,
			Checkpoint: 10,
			Records:    []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: "."}},
		}
	}
	first := newServiceVolumeIndex(db, base())
	if err := first.replayWAL(); err != nil {
		t.Fatalf("replay legacy wal: %v", err)
	}
	if first.checkpoint != 12 {
		t.Fatalf("checkpoint = %d, want 12", first.checkpoint)
	}
	if first.overlay == nil || len(first.overlay.records) != 1 {
		t.Fatalf("legacy frame did not replay: %+v", first.overlay)
	}
	if got := first.overlay.records[0].Seq; got != 0 {
		t.Fatalf("legacy frame Seq = %d, want 0 (unknown)", got)
	}
	// The legacy WAL is upgraded in place to v2 so later appends stay one
	// version, and its (sequence-free) frame still replays on the next start.
	data, err := os.ReadFile(walPath(db))
	if err != nil {
		t.Fatalf("read upgraded wal: %v", err)
	}
	if !bytes.HasPrefix(data, walMagicV2) {
		t.Fatalf("upgraded wal prefix = %q, want %q", data[:min(len(data), len(walMagicV2))], walMagicV2)
	}
	second := newServiceVolumeIndex(db, base())
	if err := second.replayWAL(); err != nil {
		t.Fatalf("replay upgraded wal: %v", err)
	}
	if second.overlay == nil || len(second.overlay.records) != 1 {
		t.Fatalf("upgraded frame did not replay: %+v", second.overlay)
	}
}

func TestParseUSNRecordFieldsReturnsSequence(t *testing.T) {
	v2 := makeUSNRecordV2(0x000700000000002c, 0x0003000000000021, 1200, usnReasonFileCreate, fileAttributeArchive, "created.txt")
	frn, parent, _, _, _, frnSeq, parentSeq, ok := parseUSNRecordFields(v2, 2)
	if !ok || frn != 0x2c || parent != 0x21 {
		t.Fatalf("v2 fields frn=%#x parent=%#x ok=%v", frn, parent, ok)
	}
	if frnSeq != 0x0007 || parentSeq != 0x0003 {
		t.Fatalf("v2 seq = (%#x,%#x), want (0x7,0x3)", frnSeq, parentSeq)
	}

	v3 := makeUSNRecordV3(0x000d0000008b7674, 0x00010000008b766c, 0x382a600ac0, usnReasonFileCreate, 0x20, "orig.py")
	frn3, parent3, _, _, _, frnSeq3, parentSeq3, ok3 := parseUSNRecordFields(v3, 3)
	if !ok3 || frn3 != 0x8b7674 || parent3 != 0x8b766c {
		t.Fatalf("v3 fields frn=%#x parent=%#x ok=%v", frn3, parent3, ok3)
	}
	if frnSeq3 != 0x000d || parentSeq3 != 0x0001 {
		t.Fatalf("v3 seq = (%#x,%#x), want (0xd,0x1)", frnSeq3, parentSeq3)
	}
}
