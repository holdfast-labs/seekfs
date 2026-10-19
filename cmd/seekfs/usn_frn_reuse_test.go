package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestApplyUSNStaleDeleteForReusedRecordKeepsLiveSubtree reproduces the loss of
// a long-lived directory when a stale USN DELETE for an earlier occupant of a
// reused MFT record number is applied on top of a fresh scan.
//
// The snapshot watermark S is the USN the fresh MFT scan resumed from
// (idx.Checkpoint = journal.NextUsn). The directory R ("wt") and its subtree
// are already reflected in that snapshot. The journal still holds the DELETE
// for the *previous* occupant of record R (older sequence, USN < S), which the
// record-number-only key cannot distinguish. Applying it tombstones R and
// cascades the whole subtree, detaching every descendant from its path.
func TestApplyUSNStaleDeleteForReusedRecordKeepsLiveSubtree(t *testing.T) {
	const snapshotUSN = int64(5_000_000_000)
	const dirFRN = uint64(0x15ffac) // reused MFT record: earlier occupant deleted
	const childDirFRN = uint64(0x15ffb0)
	const fileFRN = uint64(0x15ffb4)

	idx := &Index{
		Version:    indexVersion,
		Roots:      []string{`E:\`},
		Source:     "usn",
		Volume:     "E:",
		JournalID:  0xABCDEF,
		Checkpoint: snapshotUSN,
		Compact:    true,
		Records: []CompactRecord{
			{FRN: 5, ParentFRN: 5, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: dirFRN, ParentFRN: 5, Parent: 0, Name: "wt", Mode: uint32(os.ModeDir)},
			{FRN: childDirFRN, ParentFRN: dirFRN, Parent: 1, Name: "widen", Mode: uint32(os.ModeDir)},
			{FRN: fileFRN, ParentFRN: childDirFRN, Parent: 2, Name: "WORK_QUEUE.md"},
		},
	}
	buildOrders(idx)
	dbPath := filepath.Join(t.TempDir(), "seekfs_e.gsi")
	vol := newServiceVolumeIndex(dbPath, idx)

	// Precondition: the fresh snapshot serves the directory and the file.
	if got := searchPathsFor(t, vol, "WORK_QUEUE.md"); !sameStringSet(got, []string{`E:\wt\widen\WORK_QUEUE.md`}) {
		t.Fatalf("precondition: WORK_QUEUE.md paths = %v, want [E:\\wt\\widen\\WORK_QUEUE.md]", got)
	}

	// Stale journal event: DELETE for the previous occupant of record dirFRN,
	// below the snapshot watermark. The index keys by record number only, so
	// without a USN floor this deletes the live "wt" record and its subtree.
	vol.applyUSNChanges([]usnChange{{
		FRN:    dirFRN,
		USN:    snapshotUSN - 1_000_000,
		Reason: usnReasonFileDelete,
		Name:   "wt",
	}})

	if got := searchPathsFor(t, vol, "WORK_QUEUE.md"); !sameStringSet(got, []string{`E:\wt\widen\WORK_QUEUE.md`}) {
		t.Fatalf("stale delete cut the subtree: WORK_QUEUE.md paths = %v, want [E:\\wt\\widen\\WORK_QUEUE.md]", got)
	}
	found := false
	for _, p := range searchPathsFor(t, vol, "widen") {
		if p == `E:\wt\widen` {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("stale delete removed the directory: widen paths = %v, want to contain E:\\wt\\widen", searchPathsFor(t, vol, "widen"))
	}

	// The record at the watermark is the first post-scan change
	// (Checkpoint = journal.NextUsn) and must apply.
	vol.applyUSNChanges([]usnChange{{
		FRN:    fileFRN,
		USN:    snapshotUSN,
		Reason: usnReasonFileDelete,
		Name:   "WORK_QUEUE.md",
	}})
	if got := searchPathsFor(t, vol, "WORK_QUEUE.md"); len(got) != 0 {
		t.Fatalf("first post-scan delete (USN == watermark) was dropped: paths = %v", got)
	}
}

func searchPathsFor(t *testing.T, vol *serviceVolumeIndex, query string) []string {
	t.Helper()
	entries, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: query, MatchPath: true, Limit: 50}, false)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	return pathsOf(entries)
}
