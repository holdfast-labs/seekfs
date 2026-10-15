package main

import (
	"reflect"
	"testing"
)

// stubContentNoHeadroom pins the sidecar volume exactly at its reserve, the
// state a reserve-truncated build leaves behind: nothing more fits.
func stubContentNoHeadroom(t *testing.T) {
	t.Helper()
	oldFree, oldTotal := contentFreeBytesFn, contentTotalBytesFn
	t.Cleanup(func() { contentFreeBytesFn, contentTotalBytesFn = oldFree, oldTotal })
	contentTotalBytesFn = func(_ string) (uint64, error) { return 500 << 30, nil }
	contentFreeBytesFn = func(_ string) (uint64, error) { return contentScopeReserveMaxBytes, nil }
}

func TestContentReconcileTrimNew(t *testing.T) {
	rank := map[uint64]contentScopeMember{
		1: {frn: 1, path: `E:\r\a.txt`, priority: 0, size: 40},
		2: {frn: 2, path: `E:\r\b.txt`, priority: 0, size: 50},
		3: {frn: 3, path: `E:\docs\c.txt`, priority: 2, size: 5},
		4: {frn: 4, path: `E:\r\d.txt`, priority: 1, size: 10},
	}
	keys := []uint64{3, 1, 4, 2, 9}
	// Unlimited passes everything through untouched.
	if got, n, _ := contentReconcileTrimNew(keys, rank, int64(^uint64(0)>>1)); !reflect.DeepEqual(got, keys) || n != 0 {
		t.Fatalf("unlimited trim = %v (%d out); want all keys", got, n)
	}
	// Build priority order is a(40) b(50) d(10) c(5), then the unknown 9.
	// With 100 bytes, a, b and d fit exactly; c no longer fits, which latches
	// the budget, so the unknown key stays out too.
	got, out, outBytes := contentReconcileTrimNew(keys, rank, 100)
	if want := []uint64{1, 2, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("trim(100) admitted %v; want %v", got, want)
	}
	if out != 2 || outBytes != 5 {
		t.Fatalf("trim(100) left out %d files %d bytes; want 2 files 5 bytes (c + unknown)", out, outBytes)
	}
	// Once a file does not fit, later (lower-priority) files stay out even
	// if they are small: b (50) misses a 60-byte budget after a, so d and c
	// stay out too, exactly like the build's latch.
	if got, out, _ := contentReconcileTrimNew(keys, rank, 60); !reflect.DeepEqual(got, []uint64{1}) || out != 4 {
		t.Fatalf("trim(60) = %v (%d out); want [1] with 4 out", got, out)
	}
	// No allowance admits nothing.
	if got, out, _ := contentReconcileTrimNew(keys, rank, 0); len(got) != 0 || out != len(keys) {
		t.Fatalf("trim(0) = %v (%d out); want nothing admitted", got, out)
	}
}

// A reserve-truncated base must not read its own drops as scope drift: with
// no headroom left, a scope expansion enqueues nothing new and, even with a
// zero rebuild fraction, schedules no rebuild. Before the reconcile budget
// this rebuilt, truncated the same way, and looped (real E: index,
// 2026-10-13).
func TestContentScopeReconcileHonorsReserveForNewFiles(t *testing.T) {
	f := newScopeReconFixture(t, "drop.txt\n")
	f.writeScopeReconBase(t, f.keepDocs(t))
	builds := stubContentBuildSchedule(t)
	stubContentNoHeadroom(t)
	oldFrac := contentScopeReconcileRebuildFrac
	contentScopeReconcileRebuildFrac = 0
	defer func() { contentScopeReconcileRebuildFrac = oldFrac }()

	f.rewriteIgnore(t, "")
	f.s.attachContentForVolume(f.vol)

	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state = %q; want ready (reconciled within the budget)", got)
	}
	if *builds != 0 {
		t.Fatalf("scheduled %d builds; want 0 (nothing new fits, so nothing to rebuild for)", *builds)
	}
	if n := f.vol.contentCoord.queued(); n != 0 {
		t.Fatalf("queued %d FRNs past the reserve; want 0", n)
	}
}

// The journal-wrap reconcile re-extracts changed base docs regardless, but
// holds files new to the base to the reserve: with no headroom, the changed
// a.txt re-extracts and the never-indexed c.txt stays out.
func TestContentWrapReconcileHonorsReserveForNewFiles(t *testing.T) {
	f := newWrapReconFixture(t)
	builds := stubContentBuildSchedule(t)
	stubContentNoHeadroom(t)

	f.s.attachContentForVolume(f.vol)

	if *builds != 0 {
		t.Fatalf("wrap reconcile scheduled %d builds; want 0", *builds)
	}
	if n := f.vol.contentCoord.processQueue(f.resolve(t)); n != 1 {
		t.Fatalf("wrap drain extracted %d docs; want 1 (changed a.txt only)", n)
	}
	if hits, err := contentServiceSearch(t, f.vol, "content:needlenew", false); err != nil || len(hits) != 1 {
		t.Fatalf("changed base doc not re-extracted: %v %v", hits, err)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:needlegamma", false); len(hits) != 0 {
		t.Fatalf("file new to the base indexed past the reserve: %v", hits)
	}
}

// Both reconcile scans hold background persists: a persist swaps vol.index,
// which reads as a moved base, and on a busy volume persists outpace a full
// selection scan, so without the hold every metadata reconcile gave up and
// silently rebuilt (real E: index, 2026-10-13).
func TestContentReconcileScansHoldBackgroundPersists(t *testing.T) {
	restore := contentBuildSnapshotBatchHook
	t.Cleanup(func() { contentBuildSnapshotBatchHook = restore })

	t.Run("wrap", func(t *testing.T) {
		f := newWrapReconFixture(t)
		stubContentBuildSchedule(t)
		scanned, unheld := 0, 0
		contentBuildSnapshotBatchHook = func(_ *goSearchService, v *serviceVolumeIndex) {
			if v == f.vol {
				scanned++
				if v.contentSnapshotHold.Load() == 0 {
					unheld++
				}
			}
		}
		f.s.attachContentForVolume(f.vol)
		if scanned == 0 || unheld != 0 {
			t.Fatalf("metadata reconcile scanned %d batches, %d without the persist hold; want all held", scanned, unheld)
		}
		if got := f.vol.contentSnapshotHold.Load(); got != 0 {
			t.Fatalf("persist hold not released after reconcile: %d", got)
		}
	})
	t.Run("scope", func(t *testing.T) {
		f := newScopeReconFixture(t, "drop.txt\n")
		f.writeScopeReconBase(t, f.keepDocs(t))
		stubContentBuildSchedule(t)
		f.rewriteIgnore(t, "")
		scanned, unheld := 0, 0
		contentBuildSnapshotBatchHook = func(_ *goSearchService, v *serviceVolumeIndex) {
			if v == f.vol {
				scanned++
				if v.contentSnapshotHold.Load() == 0 {
					unheld++
				}
			}
		}
		f.s.attachContentForVolume(f.vol)
		if scanned == 0 || unheld != 0 {
			t.Fatalf("scope reconcile scanned %d batches, %d without the persist hold; want all held", scanned, unheld)
		}
		if got := f.vol.contentSnapshotHold.Load(); got != 0 {
			t.Fatalf("persist hold not released after reconcile: %d", got)
		}
	})
}

// An explicit budget is charged in projected index bytes, matching the build:
// with no stored text, a budget admits files whose extracted text far exceeds
// it, and the transient text allowance still applies on its own.
func TestContentReconcileTrimNewIndexLimit(t *testing.T) {
	rank := map[uint64]contentScopeMember{
		1: {frn: 1, path: `E:\r\a.txt`, priority: 0, size: 100_000},
		2: {frn: 2, path: `E:\r\b.txt`, priority: 0, size: 100_000},
		3: {frn: 3, path: `E:\r\c.txt`, priority: 0, size: 100_000},
	}
	keys := []uint64{1, 2, 3}
	two := 2 * contentIndexCostEstimate(100_000)
	if two >= 200_000 {
		t.Fatalf("index cost of 200 KB text = %d; want far below the text", two)
	}
	got, out, outBytes := contentReconcileTrimNewLimits(keys, rank, contentReconcileLimits{text: contentReconcileUnlimited, index: two})
	if want := []uint64{1, 2}; !reflect.DeepEqual(got, want) || out != 1 || outBytes != 100_000 {
		t.Fatalf("index-limited trim = %v (%d out, %d bytes); want [1 2] with c out", got, out, outBytes)
	}
	// The transient disk allowance (text bytes) still bites independently.
	got, out, _ = contentReconcileTrimNewLimits(keys, rank, contentReconcileLimits{text: 150_000, index: contentReconcileUnlimited})
	if want := []uint64{1}; !reflect.DeepEqual(got, want) || out != 2 {
		t.Fatalf("text-limited trim = %v (%d out); want [1] with 2 out", got, out)
	}
}
