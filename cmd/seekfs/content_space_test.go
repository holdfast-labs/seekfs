package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContentMkdirWorkUsesSidecarDirAndClearsStale(t *testing.T) {
	dir := t.TempDir()
	gsx := filepath.Join(dir, "seekfs_e.gsx")
	stale := filepath.Join(dir, contentWorkPrefix+"old")
	if err := os.MkdirAll(filepath.Join(stale, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	work, err := contentMkdirWork(gsx)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(work) != dir || !strings.HasPrefix(filepath.Base(work), contentWorkPrefix) {
		t.Fatalf("scratch dir %s not next to the sidecar in %s", work, dir)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a stale scratch dir from a crashed build was not removed")
	}
}

func TestContentCheckDiskReserve(t *testing.T) {
	restore, restoreReserve := contentFreeBytesFn, contentDiskReserveBytes
	t.Cleanup(func() { contentFreeBytesFn, contentDiskReserveBytes = restore, restoreReserve })
	contentDiskReserveBytes = 10 << 30
	contentFreeBytesFn = func(string) (uint64, error) { return contentDiskReserveBytes - 1, nil }
	var spaceErr *contentDiskSpaceError
	if err := contentCheckDiskReserve(`E:\x`); !errors.As(err, &spaceErr) || !strings.Contains(err.Error(), "E:") {
		t.Fatalf("below reserve: err = %v; want a disk-space error naming the volume", err)
	}
	contentFreeBytesFn = func(string) (uint64, error) { return contentDiskReserveBytes, nil }
	if err := contentCheckDiskReserve(`E:\x`); err != nil {
		t.Fatalf("at reserve: err = %v; want nil", err)
	}
	contentFreeBytesFn = func(string) (uint64, error) { return 0, errors.New("unknown") }
	if err := contentCheckDiskReserve(`E:\x`); err != nil {
		t.Fatalf("unknown free space must not block: %v", err)
	}
}

// A service build that runs the disk below the reserve stops with a clear
// error, leaves no shard files behind, and never writes scratch outside the
// sidecar's directory.
func TestContentServiceBuildStopsAtDiskReserve(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	restoreFree, restoreTarget, restoreReserve := contentFreeBytesFn, contentShardTargetBytes, contentDiskReserveBytes
	t.Cleanup(func() {
		contentFreeBytesFn, contentShardTargetBytes, contentDiskReserveBytes = restoreFree, restoreTarget, restoreReserve
	})
	contentDiskReserveBytes = 10 << 30
	contentShardTargetBytes = 1 // force the sharded save path
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello needle one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hello needle two"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 100})
	vol, gsx := contentBuildTestVolume(t, dir, 7, 100, []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: 16},
		{FRN: 11, ParentFRN: 10, Parent: -1, Name: "b.txt", Size: 16},
	})
	// Plenty of space until the first shard save, then the disk is "full".
	calls := 0
	contentFreeBytesFn = func(string) (uint64, error) {
		calls++
		if calls > 1 {
			return 1 << 20, nil
		}
		return 1 << 40, nil
	}
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	health := vol.content.healthSnapshot(0)
	if !strings.Contains(health.BuildError, "free") {
		t.Fatalf("build error = %q; want a disk-space message", health.BuildError)
	}
	if entries, _ := os.ReadDir(contentShardDir(gsx)); len(entries) != 0 {
		t.Fatalf("failed build left %d shard files behind", len(entries))
	}
	entries, _ := os.ReadDir(filepath.Dir(gsx))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), contentWorkPrefix) {
			t.Fatalf("scratch dir %s left behind", e.Name())
		}
	}
}

// A content snapshot holds background persists for its duration (each persist
// swaps vol.index and would restart the copy) and releases the hold after.
func TestContentSnapshotHoldsBackgroundPersist(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 100})
	vol, _ := contentBuildTestVolume(t, dir, 7, 100, []CompactRecord{{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: 12}})
	held := false
	contentBuildSnapshotBatchHook = func(*goSearchService, *serviceVolumeIndex) {
		held = held || vol.contentSnapshotHold.Load() > 0
	}
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if !held {
		t.Fatal("snapshot did not hold background persists")
	}
	if got := vol.contentSnapshotHold.Load(); got != 0 {
		t.Fatalf("persist hold not released after the snapshot: %d", got)
	}
	vol.dirty = true
	vol.contentSnapshotHold.Add(1)
	s.persistVolumeIfDue(vol, false) // must return without persisting
	vol.contentSnapshotHold.Add(-1)
	if !vol.dirty {
		t.Fatal("background persist ran while a content snapshot held it")
	}
}

// A snapshot waits for a background persist already in flight (it would swap
// vol.index mid-copy) and a persist backs off while a snapshot holds.
func TestContentSnapshotWaitsForInFlightPersist(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 100})
	vol, _ := contentBuildTestVolume(t, dir, 7, 100, []CompactRecord{{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: 12}})
	vol.persistActive.Add(1) // a persist is mid-flight
	var sawActive bool
	contentBuildSnapshotBatchHook = func(*goSearchService, *serviceVolumeIndex) {
		sawActive = sawActive || vol.persistActive.Load() > 0
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		vol.persistActive.Add(-1) // the persist finishes
	}()
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if sawActive {
		t.Fatal("snapshot copied while a persist was still in flight")
	}
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state = %q; want ready once the in-flight persist finished", got)
	}
}

// The effective peak multiplier adapts to the observed spool ratio but only
// ever tightens within a build and never drops below the measured-normal
// floor: normal text stays at 0.8 (the 1.875x coverage gain), dense text
// budgets its real peak (spool + output) instead of tripping the guard.
func TestContentEffectivePeakMultiplier(t *testing.T) {
	oldTrust := contentSpoolTrustBytes
	contentSpoolTrustBytes = 1 << 10
	t.Cleanup(func() { contentSpoolTrustBytes = oldTrust })
	const floor = contentScopePeakMultiplier // 0.8
	eq := func(got, want float64) bool {
		d := got - want
		return d < 1e-6 && d > -1e-6
	}
	// Below the trust threshold the floor governs alone.
	if got := contentEffectivePeakMultiplier(floor, 512, 100); !eq(got, floor) {
		t.Fatalf("untrusted mult = %v; want floor %v", got, floor)
	}
	const used = int64(10 << 20)
	// Normal text (r~0.265, split runs): twice the staged ratio plus margin
	// stays under the floor.
	if got := contentEffectivePeakMultiplier(floor, used, 2778726); !eq(got, floor) {
		t.Fatalf("normal-text mult = %v; want floor %v", got, floor)
	}
	// Just under the split cap the split-spike term binds slightly above it.
	if got, want := contentEffectivePeakMultiplier(floor, used, 3565158), 2*0.34+contentAdaptiveMargin; !eq(got, want) {
		t.Fatalf("near-cap mult = %v; want %v", got, want)
	}
	// Past the cap the split is skipped: staged ratio plus the encoded
	// output plus margin.
	if got, want := contentEffectivePeakMultiplier(floor, used, 3774873), 0.36+contentScopeEncodedMultiplier+contentAdaptiveMargin; !eq(got, want) {
		t.Fatalf("dense mult = %v; want %v", got, want)
	}
	if got, want := contentEffectivePeakMultiplier(floor, used, 6291456), 0.6+contentScopeEncodedMultiplier+contentAdaptiveMargin; !eq(got, want) {
		t.Fatalf("minified mult = %v; want %v", got, want)
	}
	// A pinned multiplier never loosens when the ratio improves later.
	if got := contentEffectivePeakMultiplier(1.25, used, 2778726); !eq(got, 1.25) {
		t.Fatalf("pinned mult = %v; want 1.25 (only tightens)", got)
	}
	if got := contentEffectivePeakMultiplier(floor, used, 6291456); !eq(got, 1.25) {
		t.Fatalf("rising mult = %v; want 1.25", got)
	}
}

// The split test matches the save-time decision: the projection and the
// reframe agree on which side of the cap a spool falls.
func TestContentSpoolWillSplit(t *testing.T) {
	if !contentSpoolWillSplit(1000, 340) {
		t.Fatal("0.34x spool must split")
	}
	if contentSpoolWillSplit(1000, 360) {
		t.Fatal("0.36x spool must skip the split")
	}
	if !contentSpoolWillSplit(600<<20, 158<<20) {
		t.Fatal("Chromium-like 0.26x spool must split")
	}
}
