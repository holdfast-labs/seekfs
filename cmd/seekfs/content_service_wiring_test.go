package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestContentIndexPathForDB(t *testing.T) {
	cases := map[string]string{
		`C:\ProgramData\seekfs\indexes\seekfs_c.gsi`: `C:\ProgramData\seekfs\indexes\seekfs_c.gsx`,
		`F:\idx.gsi`: `F:\idx.gsx`,
		`plain`:      `plain.gsx`,
	}
	for in, want := range cases {
		if got := contentIndexPathForDB(in); got != want {
			t.Errorf("contentIndexPathForDB(%q) = %q; want %q", in, got, want)
		}
	}
	if contentIndexPathForDB("") != "" {
		t.Fatal("empty dbPath must map to empty")
	}
}

func TestContentBaseFRNColumns(t *testing.T) {
	idx := &Index{Derived: indexDerivedSections{
		FRNs:         []uint64{100, 200},
		FRNRecordIDs: []uint32{0, 1},
	}}
	frns, ids, ok := contentBaseFRNColumns(idx)
	if !ok || len(frns) != 2 || len(ids) != 2 {
		t.Fatalf("columns = %v %v %v", frns, ids, ok)
	}
	bad := &Index{Derived: indexDerivedSections{FRNs: []uint64{1}, FRNRecordIDs: nil}}
	if _, _, ok := contentBaseFRNColumns(bad); ok {
		t.Fatal("mismatched FRN columns must not be usable")
	}
	if _, _, ok := contentBaseFRNColumns(nil); ok {
		t.Fatal("nil index must not be usable")
	}
}

// P6-6: contentResolvePath end-to-end against a compact index with a real
// parent chain, not only the persisted-FRN-column fallback. A drain's resolver
// must walk the record Parent links, prefer a fresh overlay path, and still
// resolve from the FRN column when the resident array is absent (low-memory).
func TestContentResolvePathParentChain(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 5, Checkpoint: 50}
	idx.Records = []CompactRecord{
		{FRN: 1, ParentFRN: 1, Parent: -1, Name: "."},
		{FRN: 2, ParentFRN: 1, Parent: 0, Name: "docs"},
		{FRN: 3, ParentFRN: 2, Parent: 1, Name: "sub"},
		{FRN: 10, ParentFRN: 3, Parent: 2, Name: "note.txt"},
	}
	contentIndexFRNs(idx)
	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)

	const want = `C:\docs\sub\note.txt`
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_c.gsi"), idx)
	if got, ok := s.contentResolvePath(vol, 10, map[int]string{}); !ok || got != want {
		t.Fatalf("resident resolve = %q, %v; want %q", got, ok, want)
	}
	if _, ok := s.contentResolvePath(vol, 99, map[int]string{}); ok {
		t.Fatal("an unknown FRN must not resolve")
	}

	// A pending overlay rename for the same FRN wins over the base path and
	// still joins to the base parent chain.
	vol.overlay.records = append(vol.overlay.records, CompactRecord{FRN: 10, ParentFRN: 3, Parent: -1, Name: "renamed.txt"})
	vol.overlay.byFRN[10] = 0
	vol.overlay.watermark.Store(1)
	if got, ok := s.contentResolvePath(vol, 10, map[int]string{}); !ok || got != `C:\docs\sub\renamed.txt` {
		t.Fatalf("overlay resolve = %q, %v; want the renamed overlay path", got, ok)
	}

	// Low-memory shape: the resident FRN array is absent, so resolution falls
	// back to the persisted FRN column and must reconstruct the same chain.
	lowmem := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_c.gsi"), idx)
	lowmem.frns, lowmem.frnRecordIDs = nil, nil
	if got, ok := s.contentResolvePath(lowmem, 10, map[int]string{}); !ok || got != want {
		t.Fatalf("FRN-column resolve = %q, %v; want %q", got, ok, want)
	}
}

// P6-4: the drain loop is retired by the per-volume cancel, not only by s.stop.
func TestContentDrainLoopStopsPerVolume(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_c.gsi"),
		&Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 5, Checkpoint: 50})
	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)

	done := make(chan struct{})
	go func() { s.contentDrainLoop(vol); close(done) }()
	vol.stopContentDrain()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("per-volume drain did not stop after stopContentDrain")
	}
}

// The per-volume cancel must not replace s.stop: service shutdown still stops
// every drain loop.
func TestContentDrainLoopStillStopsOnServiceStop(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	vol := newServiceVolumeIndex(filepath.Join(t.TempDir(), "seekfs_c.gsi"),
		&Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 5, Checkpoint: 50})
	s := &goSearchService{stop: make(chan struct{})}

	done := make(chan struct{})
	go func() { s.contentDrainLoop(vol); close(done) }()
	close(s.stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not stop on service stop")
	}
}

// P6-1 (M10): a service that loses the startup `.gsx.lock` race must re-acquire
// the lock on a later bounded attempt, so a concurrent CLI build cannot race it
// after the first holder releases.
func TestContentServiceReacquiresVolumeLockAfterContention(t *testing.T) {
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

	// A CLI build holds the lock when the service attaches.
	cli, err := acquireContentVolumeLock(gsx)
	if err != nil {
		t.Fatalf("cli acquire: %v", err)
	}
	s := contentTestService(t)
	s.attachContentForVolume(vol)
	s.retryContentVolumeLock(vol) // still contended; must not acquire
	if _, err := acquireContentVolumeLock(gsx); !errors.Is(err, errContentVolumeLocked) {
		t.Fatalf("service acquired while the CLI held the lock: %v", err)
	}

	// The CLI releases; the next bounded attempt takes ownership.
	cli.release()
	s.retryContentVolumeLock(vol)
	if _, err := acquireContentVolumeLock(gsx); !errors.Is(err, errContentVolumeLocked) {
		t.Fatalf("service did not re-acquire the released lock: %v", err)
	}
}

func TestRebindContentAfterBaseSwap(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	idx := &Index{Volume: "C:", Derived: indexDerivedSections{
		FRNs:         []uint64{100, 200},
		FRNRecordIDs: []uint32{3, 4},
	}}
	vol := newServiceVolumeIndex(`C:\seekfs_c.gsi`, idx)
	if vol.content == nil {
		t.Fatal("content state should exist when the flag is on")
	}
	cidx := newContentIndex()
	cidx.Docs = []contentDoc{{DocID: 0, FRN: 100}, {DocID: 1, FRN: 200}}
	reader, err := openContentReader(cidx)
	if err != nil {
		t.Fatal(err)
	}
	vol.content.setReady(cidx, reader, nil)

	rebindContentAfterBaseSwap(vol)
	if id, ok := vol.content.resolver.recordID(0); !ok || id != 3 {
		t.Fatalf("doc 0 -> %d, %v; want 3", id, ok)
	}
	if id, ok := vol.content.resolver.recordID(1); !ok || id != 4 {
		t.Fatalf("doc 1 -> %d, %v; want 4", id, ok)
	}
}
