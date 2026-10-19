package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// identityFixture builds a small USN base whose records carry NTFS sequences.
// The snapshot is already reflected at snapshotUSN.
type identityFixture struct {
	vol *serviceVolumeIndex
}

func newIdentityFixture(t *testing.T, records []CompactRecord, snapshotUSN int64) *identityFixture {
	t.Helper()
	idx := &Index{
		Version:    indexVersion,
		Roots:      []string{`E:\`},
		Source:     "usn",
		Volume:     "E:",
		JournalID:  0xABCDEF,
		Checkpoint: snapshotUSN,
		Compact:    true,
		Records:    records,
	}
	buildOrders(idx)
	dbPath := filepath.Join(t.TempDir(), "seekfs_e.gsi")
	return &identityFixture{vol: newServiceVolumeIndex(dbPath, idx)}
}

func TestApplyUSNReusedRecordStaleDeleteAboveWatermarkIgnored(t *testing.T) {
	const snapshotUSN = int64(5_000_000_000)
	const dirFRN = uint64(0x15ffac)
	const childDirFRN = uint64(0x15ffb0)
	const fileFRN = uint64(0x15ffb4)
	f := newIdentityFixture(t, []CompactRecord{
		{FRN: 5, ParentFRN: 5, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), Seq: 1},
		{FRN: dirFRN, ParentFRN: 5, Parent: 0, Name: "wt", Mode: uint32(os.ModeDir), Seq: 7},
		{FRN: childDirFRN, ParentFRN: dirFRN, Parent: 1, Name: "widen", Mode: uint32(os.ModeDir), Seq: 3},
		{FRN: fileFRN, ParentFRN: childDirFRN, Parent: 2, Name: "WORK_QUEUE.md", Seq: 4},
	}, snapshotUSN)

	// A DELETE for the previous occupant (Seq 6) of the reused record arrives
	// with USN ABOVE the watermark, so the baseCheckpoint floor cannot catch it.
	f.vol.applyUSNChanges([]usnChange{{
		FRN: dirFRN, ParentFRN: 5, USN: snapshotUSN + 100,
		Reason: usnReasonFileDelete, Name: "wt", Seq: 6,
	}})
	if got := searchPathsFor(t, f.vol, "WORK_QUEUE.md"); !sameStringSet(got, []string{`E:\wt\widen\WORK_QUEUE.md`}) {
		t.Fatalf("stale reuse delete cut the subtree: paths = %v", got)
	}

	// A DELETE carrying the current occupant's sequence still applies.
	f.vol.applyUSNChanges([]usnChange{{
		FRN: dirFRN, ParentFRN: 5, USN: snapshotUSN + 200,
		Reason: usnReasonFileDelete, Name: "wt", Seq: 7,
	}})
	if got := searchPathsFor(t, f.vol, "WORK_QUEUE.md"); len(got) != 0 {
		t.Fatalf("current-occupant delete did not apply: paths = %v", got)
	}
}

func TestApplyUSNReusedRecordCreateReplacesOccupant(t *testing.T) {
	const snapshotUSN = int64(5_000_000_000)
	const recFRN = uint64(0x2a0000)
	f := newIdentityFixture(t, []CompactRecord{
		{FRN: 5, ParentFRN: 5, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), Seq: 1},
		{FRN: recFRN, ParentFRN: 5, Parent: 0, Name: "old.txt", Seq: 7},
	}, snapshotUSN)

	if got := searchPathsFor(t, f.vol, "old.txt"); !sameStringSet(got, []string{`E:\old.txt`}) {
		t.Fatalf("precondition old.txt paths = %v", got)
	}

	// A CREATE with a different sequence is the new occupant of the record.
	f.vol.applyUSNChanges([]usnChange{{
		FRN: recFRN, ParentFRN: 5, USN: snapshotUSN + 100,
		Reason: usnReasonFileCreate, Attr: fileAttributeArchive, Name: "new.txt", Seq: 8,
	}})
	if got := searchPathsFor(t, f.vol, "old.txt"); len(got) != 0 {
		t.Fatalf("previous occupant still visible: %v", got)
	}
	if got := searchPathsFor(t, f.vol, "new.txt"); !sameStringSet(got, []string{`E:\new.txt`}) {
		t.Fatalf("new occupant paths = %v, want E:\\new.txt", got)
	}
}

func TestApplyUSNSeqZeroKeepsLegacyBehavior(t *testing.T) {
	const snapshotUSN = int64(5_000_000_000)
	const fileFRN = uint64(0x15ffb4)
	f := newIdentityFixture(t, []CompactRecord{
		{FRN: 5, ParentFRN: 5, Parent: -1, Name: ".", Mode: uint32(os.ModeDir), Seq: 1},
		{FRN: fileFRN, ParentFRN: 5, Parent: 0, Name: "gone.txt", Seq: 4},
	}, snapshotUSN)

	// Seq 0 on the event means unknown: apply exactly as before.
	f.vol.applyUSNChanges([]usnChange{{
		FRN: fileFRN, ParentFRN: 5, USN: snapshotUSN + 100,
		Reason: usnReasonFileDelete, Name: "gone.txt",
	}})
	if got := searchPathsFor(t, f.vol, "gone.txt"); len(got) != 0 {
		t.Fatalf("Seq-0 delete did not apply: paths = %v", got)
	}
}

func TestOldV9IndexWithV1WALReplaysSeqUnknown(t *testing.T) {
	idx := &Index{
		Version:    legacyIndexVersion,
		Roots:      []string{`F:\`},
		Source:     "usn",
		Volume:     "F:",
		JournalID:  7,
		Checkpoint: 10,
		Compact:    true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: "."},
		},
	}
	db := filepath.Join(t.TempDir(), "legacy.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}
	data, err := os.ReadFile(db)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	binary.LittleEndian.PutUint32(data[8:12], legacyIndexVersion) // header version -> v9
	if err := os.WriteFile(db, data, 0o644); err != nil {
		t.Fatalf("write v9 index: %v", err)
	}
	loaded, err := loadIndex(db)
	if err != nil {
		t.Fatalf("load v9 index: %v", err)
	}
	writeLegacyV1WAL(t, db, 12, []usnChange{{
		FRN: 102, ParentFRN: 100, USN: 12, Reason: usnReasonFileCreate,
		Name: "legacy-created.txt", Seq: 77, // v1 frames carry no sequence
	}})
	vol := newServiceVolumeIndex(db, loaded)
	if err := vol.replayWAL(); err != nil {
		t.Fatalf("replay v1 wal over v9 index: %v", err)
	}
	if vol.checkpoint != 12 || vol.overlay == nil || len(vol.overlay.records) != 1 {
		t.Fatalf("v1 replay state: checkpoint=%d overlay=%+v", vol.checkpoint, vol.overlay)
	}
	if got := vol.overlay.records[0].Seq; got != 0 {
		t.Fatalf("v1 replay Seq = %d, want 0 (unknown)", got)
	}
}

func TestContentDocFRNSeqRejectsReusedOccupant(t *testing.T) {
	docs := []contentDoc{{DocID: 0, FRN: 10, FRNSeq: 7}}
	if r := buildContentResolverSeq(docs, []uint64{10}, []uint32{0}, []uint16{8}); r.liveDocCount() != 0 {
		t.Fatalf("mismatched doc joined a reused record: id=%d live=%d", 0, r.liveDocCount())
	}
	// Unknown on either side still joins (legacy docs and v9 indexes).
	if id, ok := buildContentResolverSeq([]contentDoc{{DocID: 0, FRN: 10}}, []uint64{10}, []uint32{0}, []uint16{8}).recordID(0); !ok || id != 0 {
		t.Fatalf("unknown doc seq did not join: id=%d ok=%v", id, ok)
	}
	if id, ok := buildContentResolverSeq(docs, []uint64{10}, []uint32{0}, []uint16{0}).recordID(0); !ok || id != 0 {
		t.Fatalf("unknown base seq did not join: id=%d ok=%v", id, ok)
	}
	// The same occupant still joins.
	if id, ok := buildContentResolverSeq(docs, []uint64{10}, []uint32{0}, []uint16{7}).recordID(0); !ok || id != 0 {
		t.Fatalf("matching doc seq did not join: id=%d ok=%v", id, ok)
	}
	// The sequence is persisted in the doc table.
	got, err := contentDecodeDocTable(contentDocTableBytes(docs))
	if err != nil || len(got) != 1 || got[0].FRNSeq != 7 {
		t.Fatalf("doc table FRNSeq round-trip = %+v err=%v", got, err)
	}
}
