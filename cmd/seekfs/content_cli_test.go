package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M5: `content-index -db` with no scope must refuse rather than silently index
// the whole volume.
func TestContentIndexDBWholeVolumeRequiresOptIn(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	err := cmdContentIndex([]string{"-db", "does-not-matter.gsi"})
	if err == nil || !strings.Contains(err.Error(), "whole volume") {
		t.Fatalf("err = %v; want a whole-volume opt-in refusal", err)
	}
}

// M5: `-all` opts in and builds the sidecar for the whole volume.
func TestContentIndexDBAllOptInBuildsSidecar(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	dir := t.TempDir()
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := &Index{Source: "usn", Volume: dir, Compact: true, JournalID: 7, Checkpoint: 100}
	idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 6}}
	db := filepath.Join(dir, "seekfs_c.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatal(err)
	}
	if err := cmdContentIndex([]string{"-db", db, "-all"}); err != nil {
		t.Fatalf("content-index -all: %v", err)
	}
	built, err := contentLoadFile(contentIndexPathForDB(db))
	if err != nil {
		t.Fatalf("load built sidecar: %v", err)
	}
	if len(built.Docs) != 1 {
		t.Fatalf("built %d docs; want 1", len(built.Docs))
	}
}

// M10: two holders of the same sidecar lock must contend, so the CLI fails fast
// instead of racing a live service.
func TestContentVolumeLockContends(t *testing.T) {
	gsx := filepath.Join(t.TempDir(), "v.gsx")
	lk, err := acquireContentVolumeLock(gsx)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lk.release()
	if _, err := acquireContentVolumeLock(gsx); !errors.Is(err, errContentVolumeLocked) {
		t.Fatalf("second acquire err = %v; want errContentVolumeLocked", err)
	}
}

// M10: releasing the lock lets the next holder acquire it.
func TestContentVolumeLockReacquireAfterRelease(t *testing.T) {
	gsx := filepath.Join(t.TempDir(), "v.gsx")
	lk, err := acquireContentVolumeLock(gsx)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	lk.release()
	lk2, err := acquireContentVolumeLock(gsx)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	lk2.release()
}

// M10: when a live service holds the sidecar lock, `content-index -db` must
// fail fast with a clear error instead of clobbering it.
func TestContentIndexCLIRefusesLiveServiceLock(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	db := filepath.Join(t.TempDir(), "seekfs_c.gsi")
	lk, err := acquireContentVolumeLock(contentIndexPathForDB(db))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lk.release()
	err = cmdContentIndex([]string{"-db", db, "-all"})
	if !errors.Is(err, errContentVolumeLocked) {
		t.Fatalf("err = %v; want errContentVolumeLocked", err)
	}
}

// M10: attaching a volume makes the service the lock owner, so a concurrent CLI
// build of the same sidecar is refused.
func TestContentServiceOwnsVolumeLockAfterAttach(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 5, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})

	dir := t.TempDir()
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 5, Checkpoint: 50}
	idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10}}
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)
	gsx := contentIndexPathForDB(vol.dbPath)
	usn := newContentIndex()
	usn.Origin = contentOriginUSN
	usn.JournalID = 5
	usn.CheckpointUSN = 50
	usn.Docs = []contentDoc{{DocID: 0, FRN: 10, ContentType: contentClassText, ExtractorVersion: 1}}
	if err := contentSaveFile(gsx, usn); err != nil {
		t.Fatal(err)
	}

	s := contentTestService(t)
	s.attachContentForVolume(vol)
	if _, err := acquireContentVolumeLock(gsx); !errors.Is(err, errContentVolumeLocked) {
		t.Fatalf("service did not hold the sidecar lock; acquire err = %v", err)
	}
}
