package main

import (
	"os"
	"testing"
)

// TestPersistOverlayCarryRemapsBaseIDsAfterFold reproduces the phantom-orphan
// mechanism. A background persist snapshots the overlay, then the fold removes
// pre-snapshot records while replay keeps tombstoning base records in the live
// overlay. Those tombstone IDs belong to the OLD base ID space. The swap must
// remap them onto the replacement base by FRN; carrying the raw IDs makes them
// hide unrelated records (here the innocent directory and its child), which the
// next fold then drops, leaving the children re-rooted.
func TestPersistOverlayCarryRemapsBaseIDsAfterFold(t *testing.T) {
	const dir = uint32(os.ModeDir)
	old := &Index{
		Version: indexVersion,
		Compact: true,
		Source:  "usn",
		Volume:  "C:",
		Roots:   []string{`C:\`},
		Records: []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: ".", Mode: dir},
			{FRN: 20, ParentFRN: 10, Parent: 0, Name: "tmp1"},
			{FRN: 30, ParentFRN: 10, Parent: 0, Name: "tmp2"},
			{FRN: 40, ParentFRN: 10, Parent: 0, Name: "worktree", Mode: dir},
			{FRN: 41, ParentFRN: 40, Parent: 3, Name: "go.mod"},
			{FRN: 50, ParentFRN: 10, Parent: 0, Name: "innocent", Mode: dir},
			{FRN: 51, ParentFRN: 50, Parent: 5, Name: "keep.txt"},
		},
	}
	buildOrders(old)
	vol := newServiceVolumeIndex("", old)

	// Pre-snapshot: two temporary files are already tombstoned. The fold
	// removes them, so every later record's base ID shifts down by two.
	vol.applyUSNChanges([]usnChange{
		{FRN: 20, ParentFRN: 10, USN: 10, Reason: usnReasonFileDelete},
		{FRN: 30, ParentFRN: 10, USN: 11, Reason: usnReasonFileDelete},
	})
	foldOverlay := snapshotOverlayForFold(vol.overlay)
	foldOverlay.checkpointAtRotate.Store(vol.checkpoint)

	// Post-snapshot: the worktree directory is deleted while the fold runs.
	// tombstoneBaseSubtree records old base IDs 3 and 4.
	vol.applyUSNChanges([]usnChange{{FRN: 40, ParentFRN: 10, USN: 20, Reason: usnReasonFileDelete}})

	folded := compactOverlayIndexLocked(vol, foldOverlay)
	if folded.compactRecordCount() != 5 {
		t.Fatalf("folded records = %d, want 5", folded.compactRecordCount())
	}
	replacement := newServiceVolumeIndex("", folded)
	replacement.overlay = carryOverlayDelta(vol.overlay, len(foldOverlay.records), foldOverlay, old, replacement)

	// The carried tombstone must name the same logical records in the new base
	// (worktree=1, go.mod=2), not old IDs 3/4 (innocent/keep.txt).
	second := compactOverlayIndexLocked(replacement, replacement.overlay)
	got := map[string]bool{}
	for id := 0; id < second.compactRecordCount(); id++ {
		got[second.compactRecord(id).Name] = true
	}
	if !got["innocent"] || !got["keep.txt"] {
		t.Fatalf("stale carried IDs hid unrelated records: %v", got)
	}
	if got["worktree"] || got["go.mod"] {
		t.Fatalf("deleted worktree subtree survived the fold: %v", got)
	}
}

// TestOrphanParentDoesNotResolveToVolumeRoot pins the fallback: a record whose
// nonzero parent FRN is not in the index must not be reported as a child of the
// volume root. Legitimate roots (ParentFRN 0 or self) keep their root path.
func TestOrphanParentDoesNotResolveToVolumeRoot(t *testing.T) {
	const dir = uint32(os.ModeDir)
	idx := &Index{
		Version: indexVersion,
		Compact: true,
		Source:  "usn",
		Volume:  "C:",
		Roots:   []string{`C:\`},
		Records: []CompactRecord{
			{FRN: 5, ParentFRN: 5, Parent: -1, Name: ".", Mode: dir},
			{FRN: 100, ParentFRN: 5, Parent: 0, Name: "keep", Mode: dir},
			{FRN: 101, ParentFRN: 100, Parent: 1, Name: "ok.txt"},
			{FRN: 200, ParentFRN: 999, Parent: -1, Name: "gone", Mode: dir},
			{FRN: 201, ParentFRN: 200, Parent: 3, Name: ".git"},
			{FRN: 202, ParentFRN: 0, Parent: -1, Name: "rootfile.txt"},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex("", idx)

	if got := idx.reconstructCompactPath(0); got != "C:" {
		t.Fatalf("root path = %q, want %q", got, "C:")
	}
	if got := idx.reconstructCompactPath(2); got != `C:\keep\ok.txt` {
		t.Fatalf("resolved path = %q, want %q", got, `C:\keep\ok.txt`)
	}
	if got := idx.reconstructCompactPath(5); got != `C:\rootfile.txt` {
		t.Fatalf("legitimate root-level path = %q, want %q", got, `C:\rootfile.txt`)
	}
	if got := idx.reconstructCompactPath(3); got != "" {
		t.Fatalf("orphan directory resolved to %q, want no path", got)
	}
	if got := idx.reconstructCompactPath(4); got != "" {
		t.Fatalf("orphan child resolved to %q, want no path", got)
	}

	ov := newOverlaySegment()
	ov.records = []CompactRecord{{FRN: 300, ParentFRN: 999, Name: "overlay-orphan"}}
	ov.byFRN[300] = 0
	ov.watermark.Store(1)
	if got := vol.overlayRecordPath(ov.records, latestOverlaySlotsByFRN(ov.records), 0, map[int32]struct{}{}, map[int]string{}); got != "" {
		t.Fatalf("overlay orphan path = %q, want no path", got)
	}

	results, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: ".git", MatchPath: true, Limit: 10}, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("orphan .git reported at %v, want no result", pathsOf(results))
	}
}
