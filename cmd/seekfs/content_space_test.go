package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
