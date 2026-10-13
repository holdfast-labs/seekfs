package main

import (
	"os"
	"sync"
	"testing"
)

// Cross-test shared content fixtures.
//
// Several differential tests each build an identical multi-thousand-document
// content index (the expensive step is assembleContentIndex, not the record
// list) and then only read from it. Building one shared pair per fixture
// shape and reusing it across those tests removes the redundant builds with
// byte-identical volumes: same constructor, same records, same content.
//
// Safety contract, read it before touching this file:
//   - Shared volumes are READ-ONLY. Any test that mutates a volume
//     (applyUSNChanges, delta upserts) must build its own via
//     newContentRecordVolume, never take a shared one.
//   - Tests in this package run sequentially (no package-level t.Parallel),
//     so the lazily built fixture needs no locking beyond the Once.
//   - The fixture outlives the first test that builds it, so it cannot use
//     t.TempDir (removed when that test ends); it uses os.MkdirTemp and
//     TestMain removes the dirs after the suite (best effort).

var sharedContentTmpDirs struct {
	mu   sync.Mutex
	dirs []string
}

// sharedContentTmpDir mints a suite-lived scratch dir for a shared fixture.
// Call only for fixtures shared across tests; per-test volumes keep using
// t.TempDir through newContentRecordVolume.
func sharedContentTmpDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "seekfs-shared-vol-*")
	if err != nil {
		t.Fatal(err)
	}
	sharedContentTmpDirs.mu.Lock()
	sharedContentTmpDirs.dirs = append(sharedContentTmpDirs.dirs, dir)
	sharedContentTmpDirs.mu.Unlock()
	return dir
}

// removeSharedContentTmpDirs drops every suite-lived fixture dir. Called from
// TestMain after m.Run; failures are ignored (stale scratch, not coverage).
func removeSharedContentTmpDirs() {
	sharedContentTmpDirs.mu.Lock()
	dirs := sharedContentTmpDirs.dirs
	sharedContentTmpDirs.dirs = nil
	sharedContentTmpDirs.mu.Unlock()
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
}

var sharedBoundedFlatOnce struct {
	sync.Once
	c, f *serviceVolumeIndex
}

// sharedBoundedFlatVolumes returns the C:/F: pair over the flat 4300-doc
// fixture (deliberately larger than contentRelevanceWindow so the broad path
// is exercised). Same records and constructor as the per-test builds it
// replaces; read-only sharing, see the contract above.
func sharedBoundedFlatVolumes(t *testing.T) (c, f *serviceVolumeIndex) {
	t.Helper()
	sharedBoundedFlatOnce.Once.Do(func() {
		dir := sharedContentTmpDir(t)
		sharedBoundedFlatOnce.c = newContentRecordVolumeInDir(t, "C:", contentBoundedDifferentialRecords("C:", 4_300, 1_000_000), dir)
		sharedBoundedFlatOnce.f = newContentRecordVolumeInDir(t, "F:", contentBoundedDifferentialRecords("F:", 4_300, 2_000_000), dir)
	})
	if sharedBoundedFlatOnce.c == nil || sharedBoundedFlatOnce.f == nil {
		t.Fatal("shared flat fixture failed to build")
	}
	return sharedBoundedFlatOnce.c, sharedBoundedFlatOnce.f
}

var sharedBoundedNestedOnce struct {
	sync.Once
	c, f *serviceVolumeIndex
}

// sharedBoundedNestedVolumes returns the C:/F: pair over the nested fixture
// (real <volume>\<dir>\<name> paths so bias roots actually reorder). Same
// records and constructor as the per-test builds it replaces; read-only
// sharing, see the contract above.
func sharedBoundedNestedVolumes(t *testing.T) (c, f *serviceVolumeIndex) {
	t.Helper()
	sharedBoundedNestedOnce.Once.Do(func() {
		dir := sharedContentTmpDir(t)
		sharedBoundedNestedOnce.c = newContentRecordVolumeInDir(t, "C:", contentBiasedNestedRecords("C:", 1_000_000, 1_100), dir)
		sharedBoundedNestedOnce.f = newContentRecordVolumeInDir(t, "F:", contentBiasedNestedRecords("F:", 2_000_000, 1_100), dir)
	})
	if sharedBoundedNestedOnce.c == nil || sharedBoundedNestedOnce.f == nil {
		t.Fatal("shared nested fixture failed to build")
	}
	return sharedBoundedNestedOnce.c, sharedBoundedNestedOnce.f
}
