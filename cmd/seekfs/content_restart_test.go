package main

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// stubContentCatchUpSync makes attach run catch-up inline so tests are
// deterministic; production runs it on a background goroutine.
func stubContentCatchUpSync(t *testing.T) {
	t.Helper()
	restore := contentCatchUpAsync
	contentCatchUpAsync = func(fn func()) { fn() }
	t.Cleanup(func() { contentCatchUpAsync = restore })
}

// stubContentCatchUpJournal makes the pre-read journal-bounds validation use a
// fixed journal instead of opening the real volume.
func stubContentCatchUpJournal(t *testing.T, j usnJournalDataV0) {
	t.Helper()
	restore := contentCatchUpJournal
	contentCatchUpJournal = func(string) (usnJournalDataV0, error) { return j, nil }
	t.Cleanup(func() { contentCatchUpJournal = restore })
}

func stubContentCatchUpRead(t *testing.T, fn func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error)) {
	t.Helper()
	restore := contentCatchUpUSNRead
	contentCatchUpUSNRead = fn
	t.Cleanup(func() { contentCatchUpUSNRead = restore })
}

// stubContentBuildSchedule captures scheduled content builds without running
// them, so a test can assert that a capped/errored catch-up requested a rebuild
// while the volume's degraded/incomplete state stays deterministically
// observable.
func stubContentBuildSchedule(t *testing.T) *int {
	t.Helper()
	n := 0
	restore := contentBuildRun
	contentBuildRun = func(fn func()) { n++ }
	t.Cleanup(func() { contentBuildRun = restore })
	return &n
}

// The WP0 checkpoint must round-trip through the .gsx header so restart
// catch-up can resume from it.
func TestContentIndexCheckpointRoundTrip(t *testing.T) {
	idx := newContentIndex()
	idx.JournalID = 0x1122334455667788
	idx.CheckpointUSN = 0x99AABBCCDDEEFF00
	got, err := contentIndexDecode(contentIndexEncode(idx))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.JournalID != idx.JournalID || got.CheckpointUSN != idx.CheckpointUSN {
		t.Fatalf("checkpoint = (%d,%d); want (%d,%d)", got.JournalID, got.CheckpointUSN, idx.JournalID, idx.CheckpointUSN)
	}
}

// A base written at checkpoint C, with files created/modified/deleted while the
// service was stopped, must catch up after attach+drain: the changed content
// becomes findable and deleted content does not (PB3).
func TestContentRestartCatchUpAfterDrain(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	aPath := filepath.Join(dir, "alpha.txt")
	bPath := filepath.Join(dir, "beta.txt")
	if err := os.WriteFile(aPath, []byte("alpha old needlealpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, []byte("beta needlebeta"), 0o644); err != nil {
		t.Fatal(err)
	}

	const journal = uint64(0xC0FFEE)
	const baseCP = int64(100)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 200})

	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: journal, Checkpoint: baseCP}
	idx.Records = []CompactRecord{
		{FRN: 10, ParentFRN: 1, Parent: -1, Name: "alpha.txt", Size: 10},
		{FRN: 20, ParentFRN: 1, Parent: -1, Name: "beta.txt", Size: 10},
	}
	contentIndexFRNs(idx)
	dbPath := filepath.Join(dir, "seekfs_c.gsi")
	vol := newServiceVolumeIndex(dbPath, idx)

	base, err := assembleContentIndex([]contentBuildDoc{
		{path: `C:\alpha.txt`, frn: 10, text: []byte("alpha old needlealpha"), class: contentClassText, version: 1},
		{path: `C:\beta.txt`, frn: 20, text: []byte("beta needlebeta"), class: contentClassText, version: 1},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = journal
	base.CheckpointUSN = uint64(baseCP)
	stampContentTestScope(vol.volume, base)
	if err := contentSaveFile(contentIndexPathForDB(dbPath), base); err != nil {
		t.Fatal(err)
	}

	// Changes that happened while the service was stopped. applyUSNChanges is
	// exactly what startup replay does: it advances the record overlay and the
	// volume checkpoint, but content observation is a no-op until the drain is
	// enabled (the PB3 window).
	if err := os.WriteFile(aPath, []byte("gamma new needlegamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bPath); err != nil {
		t.Fatal(err)
	}
	changes := []usnChange{
		{FRN: 10, ParentFRN: 1, Name: "alpha.txt", USN: 105, Reason: usnReasonDataOverwrite | usnReasonClose},
		{FRN: 20, ParentFRN: 1, Name: "beta.txt", USN: 106, Reason: usnReasonFileDelete},
	}
	vol.applyUSNChanges(changes)
	if vol.checkpoint != 106 {
		t.Fatalf("volume checkpoint after replay = %d; want 106", vol.checkpoint)
	}

	// Drive attach's USN enumeration from the same stop-time changes.
	stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
		if journalID != journal {
			t.Errorf("catch-up journal = %d; want %d", journalID, journal)
		}
		if startUSN != baseCP {
			t.Errorf("catch-up start = %d; want %d", startUSN, baseCP)
		}
		return 106, changes, nil
	})

	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after attach = %q; want ready", got)
	}

	resolve := func(frn uint64) (string, bool) {
		switch frn {
		case 10:
			return aPath, true
		case 20:
			return bPath, true
		}
		return "", false
	}
	if n := vol.contentCoord.processQueue(resolve); n == 0 && vol.content.deltaView().len() == 0 {
		t.Fatal("restart catch-up did not enqueue the changed file")
	}

	hits, err := contentServiceSearch(t, vol, "content:needlegamma", false)
	if err != nil {
		t.Fatalf("search changed content: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("changed content not findable after restart: %v", hits)
	}
	if hits, _ := contentServiceSearch(t, vol, "content:needlealpha", false); len(hits) != 0 {
		t.Fatalf("stale content still findable after restart: %v", hits)
	}
	if hits, _ := contentServiceSearch(t, vol, "content:needlebeta", false); len(hits) != 0 {
		t.Fatalf("deleted content still findable after restart: %v", hits)
	}
}

// The attach branches: content checkpoint equal to the volume checkpoint (no
// work), behind (catch up), a different journal generation (stale), a journal
// wrapped below LowestValidUsn (stale), and a zero-checkpoint USN base (stale).
func TestContentAttachCheckpointBranches(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)

	newVolume := func(t *testing.T, journal uint64, checkpoint int64) (*serviceVolumeIndex, string) {
		t.Helper()
		dir := t.TempDir()
		idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: journal, Checkpoint: checkpoint}
		idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10}}
		contentIndexFRNs(idx)
		vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)
		return vol, contentIndexPathForDB(vol.dbPath)
	}
	writeBase := func(t *testing.T, gsx string, journal, cp uint64) {
		t.Helper()
		cidx, err := assembleContentIndex([]contentBuildDoc{{path: `C:\a.txt`, frn: 10, text: []byte("alpha"), class: contentClassText, version: 1}}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		cidx.Origin = contentOriginUSN
		cidx.JournalID = journal
		cidx.CheckpointUSN = cp
		stampContentTestScope("C:", cidx)
		if err := contentSaveFile(gsx, cidx); err != nil {
			t.Fatal(err)
		}
	}
	trackCalls := func(t *testing.T) *int {
		t.Helper()
		calls := new(int)
		stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
			*calls++
			return startUSN, nil, nil
		})
		return calls
	}
	attach := func(t *testing.T, vol *serviceVolumeIndex) *goSearchService {
		t.Helper()
		s := &goSearchService{stop: make(chan struct{})}
		t.Cleanup(func() { close(s.stop) })
		s.attachContentForVolume(vol)
		releaseVolumeContentOnCleanup(t, vol)
		return s
	}

	t.Run("equal", func(t *testing.T) {
		vol, gsx := newVolume(t, 7, 50)
		writeBase(t, gsx, 7, 50)
		stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})
		calls := trackCalls(t)
		attach(t, vol)
		if got := vol.content.stateOf(); got != contentStateReady {
			t.Fatalf("state = %q; want ready", got)
		}
		if *calls != 0 {
			t.Fatalf("catch-up ran %d times; want 0 when content checkpoint is current", *calls)
		}
		if vol.contentCoord.queued() != 0 {
			t.Fatal("no work should be queued when the checkpoint is current")
		}
	})

	t.Run("behind", func(t *testing.T) {
		vol, gsx := newVolume(t, 7, 51)
		writeBase(t, gsx, 7, 50)
		stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})
		changes := []usnChange{{FRN: 10, ParentFRN: 1, Name: "a.txt", USN: 51, Reason: usnReasonDataOverwrite | usnReasonClose}}
		calls := new(int)
		stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
			*calls++
			return 51, changes, nil
		})
		attach(t, vol)
		if *calls != 1 {
			t.Fatalf("catch-up ran %d times; want 1 when the base is behind", *calls)
		}
		if vol.contentCoord.queued() == 0 && vol.content.deltaView().len() == 0 {
			t.Fatal("base-behind catch-up enqueued nothing")
		}
	})

	t.Run("journal-differs", func(t *testing.T) {
		vol, gsx := newVolume(t, 8, 50)
		writeBase(t, gsx, 7, 50)
		calls := trackCalls(t)
		attach(t, vol)
		if got := vol.content.stateOf(); got != contentStateStale {
			t.Fatalf("state = %q; want stale for a different journal generation", got)
		}
		if vol.content.usableForQuery() {
			t.Fatal("a stale generation base must not be usable")
		}
		if *calls != 0 {
			t.Fatalf("catch-up ran %d times for a foreign generation; want 0", *calls)
		}
	})

	t.Run("below-lowest-valid", func(t *testing.T) {
		vol, gsx := newVolume(t, 7, 50)
		writeBase(t, gsx, 7, 50)
		// The journal wrapped: the base checkpoint is below the first valid USN.
		stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 100, LowestValidUsn: 100, NextUsn: 200})
		calls := trackCalls(t)
		attach(t, vol)
		if got := vol.content.stateOf(); got != contentStateStale {
			t.Fatalf("state = %q; want stale for a checkpoint below LowestValidUsn", got)
		}
		if vol.content.usableForQuery() {
			t.Fatal("a wrapped-journal base must not be usable")
		}
		if *calls != 0 {
			t.Fatalf("catch-up read %d times for a wrapped journal; want 0", *calls)
		}
	})

	t.Run("zero-checkpoint", func(t *testing.T) {
		vol, gsx := newVolume(t, 7, 50)
		writeBase(t, gsx, 7, 0)
		calls := trackCalls(t)
		attach(t, vol)
		if got := vol.content.stateOf(); got == contentStateReady {
			t.Fatalf("state = %q; a zero-checkpoint USN base must not be ready", got)
		}
		if vol.content.usableForQuery() {
			t.Fatal("a zero-checkpoint USN base must not be usable")
		}
		if *calls != 0 {
			t.Fatalf("catch-up read %d times for a zero watermark; want 0", *calls)
		}
	})
}

// A base with a valid watermark must not be published over a volume a rebuild
// owns: attach skips a non-ready volume, and a later attach once it is ready
// publishes normally.
func TestContentAttachSkipsNonReadyVolume(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})

	dir := t.TempDir()
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 50}
	idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10}}
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)
	cidx, err := assembleContentIndex([]contentBuildDoc{{path: `C:\a.txt`, frn: 10, text: []byte("alpha"), class: contentClassText, version: 1}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cidx.Origin = contentOriginUSN
	cidx.JournalID = 7
	cidx.CheckpointUSN = 50
	stampContentTestScope("C:", cidx)
	if err := contentSaveFile(contentIndexPathForDB(vol.dbPath), cidx); err != nil {
		t.Fatal(err)
	}

	// Simulate a rebuild owning the volume.
	vol.state = "stale"
	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)
	if got := vol.content.stateOf(); got == contentStateReady {
		t.Fatalf("attach published %q over a rebuilding volume", got)
	}
	if vol.content.usableForQuery() {
		t.Fatal("a skipped attach must not leave a usable base")
	}

	// After the rebuild finishes the volume is ready and attach publishes.
	vol.state = "ready"
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after ready re-attach = %q; want ready", got)
	}
}

// The blocker race: a journal-reset base swap that lands while attach is in
// flight must not be overwritten by attach publishing the old-generation base.
// The swap runs under indexMu.Lock; attach's validate+publish runs under
// indexMu.RLock, so the ordering here (swap first) is the one that used to
// resurrect `ready` from `stale`.
func TestContentAttachRacesJournalReset(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seekfs_c.gsi")
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 50}
	idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10}}
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(dbPath, idx)
	cidx, err := assembleContentIndex([]contentBuildDoc{{path: `C:\a.txt`, frn: 10, text: []byte("alpha"), class: contentClassText, version: 1}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cidx.Origin = contentOriginUSN
	cidx.JournalID = 7
	cidx.CheckpointUSN = 50
	stampContentTestScope("C:", cidx)
	if err := contentSaveFile(contentIndexPathForDB(dbPath), cidx); err != nil {
		t.Fatal(err)
	}

	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)

	reset := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 8, Checkpoint: 50}
	reset.Records = []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10}}
	reset.Derived.FRNs = []uint64{10}
	reset.Derived.FRNRecordIDs = []uint32{0}

	// Hold indexMu while starting attach, then complete the reset swap before
	// releasing, so attach's critical section always observes the new journal.
	s.indexMu.Lock()
	done := make(chan struct{})
	go func() {
		s.attachContentForVolume(vol)
		releaseVolumeContentOnCleanup(t, vol)
		close(done)
	}()
	replaceServiceVolumeContents(vol, newServiceVolumeIndex(dbPath, reset))
	s.indexMu.Unlock()
	<-done

	if got := vol.content.stateOf(); got == contentStateReady {
		t.Fatalf("attach published %q over a journal-reset swap", got)
	}
	if vol.content.usableForQuery() {
		t.Fatal("a base from a reset journal generation must not be usable")
	}
}

// Hitting the record cap or the byte cap leaves the volume degraded and
// incomplete instead of silently serving a partial delta.
func TestContentCatchUpCapsDegrade(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)

	newVolume := func(t *testing.T) *serviceVolumeIndex {
		t.Helper()
		dir := t.TempDir()
		idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 100}
		idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10}}
		contentIndexFRNs(idx)
		vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)
		cidx, err := assembleContentIndex([]contentBuildDoc{{path: `C:\a.txt`, frn: 10, text: []byte("alpha"), class: contentClassText, version: 1}}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		cidx.Origin = contentOriginUSN
		cidx.JournalID = 7
		cidx.CheckpointUSN = 50
		stampContentTestScope("C:", cidx)
		if err := contentSaveFile(contentIndexPathForDB(vol.dbPath), cidx); err != nil {
			t.Fatal(err)
		}
		stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 100})
		stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
			return 100, []usnChange{
				{FRN: 10, Reason: usnReasonDataOverwrite},
				{FRN: 11, Reason: usnReasonDataOverwrite},
			}, nil
		})
		return vol
	}

	t.Run("records", func(t *testing.T) {
		restore := contentCatchUpMaxChanges
		contentCatchUpMaxChanges = 1
		t.Cleanup(func() { contentCatchUpMaxChanges = restore })
		scheduled := stubContentBuildSchedule(t)
		vol := newVolume(t)
		s := &goSearchService{stop: make(chan struct{})}
		defer close(s.stop)
		s.attachContentForVolume(vol)
		releaseVolumeContentOnCleanup(t, vol)
		if !vol.content.healthIncomplete() {
			t.Fatal("hitting the record cap must mark the volume incomplete")
		}
		if got := vol.content.stateOf(); got != contentStateDegraded {
			t.Fatalf("state = %q; want degraded", got)
		}
		if *scheduled == 0 {
			t.Fatal("a capped catch-up must schedule a rebuild")
		}
	})

	t.Run("bytes", func(t *testing.T) {
		restore := contentCatchUpMaxBytes
		contentCatchUpMaxBytes = 0
		t.Cleanup(func() { contentCatchUpMaxBytes = restore })
		scheduled := stubContentBuildSchedule(t)
		vol := newVolume(t)
		s := &goSearchService{stop: make(chan struct{})}
		defer close(s.stop)
		s.attachContentForVolume(vol)
		releaseVolumeContentOnCleanup(t, vol)
		if !vol.content.healthIncomplete() {
			t.Fatal("hitting the byte cap must mark the volume incomplete")
		}
		if *scheduled == 0 {
			t.Fatal("a capped catch-up must schedule a rebuild")
		}
	})
}

// F1: an all-create catch-up (files the base predates, so they have no base
// record) must still be bounded by the byte cap. Before the fix every created
// FRN sized as 0, so only the 1M record cap applied and the delta could retain
// up to 1M documents. An FRN with no base or overlay record is charged the
// conservative per-file byte bound, so the cap trips and the volume is marked
// incomplete/degraded with no work enqueued.
func TestContentCatchUpAllCreateByteCapDegrade(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 100})

	restore := contentCatchUpMaxBytes
	contentCatchUpMaxBytes = 1
	t.Cleanup(func() { contentCatchUpMaxBytes = restore })
	scheduled := stubContentBuildSchedule(t)

	dir := t.TempDir()
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 100}
	// One existing base file; every created FRN below has no base record.
	idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "base.txt", Size: 10}}
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)
	cidx, err := assembleContentIndex([]contentBuildDoc{{path: `C:\base.txt`, frn: 10, text: []byte("base"), class: contentClassText, version: 1}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cidx.Origin = contentOriginUSN
	cidx.JournalID = 7
	cidx.CheckpointUSN = 50
	stampContentTestScope("C:", cidx)
	if err := contentSaveFile(contentIndexPathForDB(vol.dbPath), cidx); err != nil {
		t.Fatal(err)
	}

	changes := make([]usnChange, 0, 64)
	for i := 0; i < 64; i++ {
		changes = append(changes, usnChange{FRN: uint64(1000 + i), ParentFRN: 1, Name: "created.txt", Reason: usnReasonFileCreate})
	}
	stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
		return 100, changes, nil
	})

	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)

	if !vol.content.healthIncomplete() {
		t.Fatal("an all-create catch-up must trip the byte cap and mark the volume incomplete")
	}
	if got := vol.content.stateOf(); got != contentStateDegraded {
		t.Fatalf("state = %q; want degraded", got)
	}
	if q, d := vol.contentCoord.queued(), vol.content.deltaView().len(); q != 0 || d != 0 {
		t.Fatalf("cap tripped but work was enqueued: queue=%d delta=%d", q, d)
	}
	if *scheduled == 0 {
		t.Fatal("a capped catch-up must schedule a rebuild")
	}
}

// A USN read error during catch-up is a degraded (incomplete) volume, never a
// silent partial delta or a wrong `ready`.
func TestContentCatchUpReadErrorDegraded(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 100})

	dir := t.TempDir()
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 100}
	idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10}}
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)
	cidx, err := assembleContentIndex([]contentBuildDoc{{path: `C:\a.txt`, frn: 10, text: []byte("alpha"), class: contentClassText, version: 1}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cidx.Origin = contentOriginUSN
	cidx.JournalID = 7
	cidx.CheckpointUSN = 50
	stampContentTestScope("C:", cidx)
	if err := contentSaveFile(contentIndexPathForDB(vol.dbPath), cidx); err != nil {
		t.Fatal(err)
	}
	stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
		return startUSN, nil, errors.New("journal read failed")
	})
	scheduled := stubContentBuildSchedule(t)

	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)
	if !vol.content.healthIncomplete() {
		t.Fatal("a catch-up read error must mark the volume incomplete")
	}
	if got := vol.content.stateOf(); got != contentStateDegraded {
		t.Fatalf("state = %q; want degraded", got)
	}
	if *scheduled == 0 {
		t.Fatal("an errored catch-up must schedule a rebuild")
	}
}

// A truncated catch-up must reach the query signal: the search trace reports
// incomplete and a count refuses, even after a completed drain pass.
func TestContentCatchUpIncompleteSurfacesAtQueryLevel(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{
		{2, "notes.txt", "alpha needle beta"},
	})
	vol.content.markCatchUpIncomplete()
	// A drain pass must not clear the truncation.
	vol.content.markDrained()
	if !vol.content.healthIncomplete() {
		t.Fatal("markDrained cleared the incomplete flag")
	}

	_, trace, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:needle", 100)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !trace.ContentIncomplete {
		t.Fatal("search did not surface the truncated catch-up as ContentIncomplete")
	}
	if trace.Complete == nil || *trace.Complete {
		t.Fatalf("trace Complete = %v; want false for a truncated catch-up", trace.Complete)
	}

	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:needle", Trace: &searchTrace{}})
	if !ok {
		t.Fatal("count was not handled")
	}
	if count != 0 || !errors.Is(err, errContentIncomplete) {
		t.Fatalf("count = %d, err = %v; want refused with errContentIncomplete", count, err)
	}
}

// A restart-window create and a move-in rename-new are enqueued after attach;
// an intra-volume rename (FRN already has a doc) is not.
func TestContentRestartCatchUpCreateAndRename(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 200})

	dir := t.TempDir()
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 102}
	idx.Records = []CompactRecord{
		{FRN: 10, ParentFRN: 1, Parent: -1, Name: "existing.txt", Size: 10},
		{FRN: 30, ParentFRN: 1, Parent: -1, Name: "created.txt", Size: 10},
		{FRN: 40, ParentFRN: 1, Parent: -1, Name: "movedin.txt", Size: 10},
	}
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)
	base, err := assembleContentIndex([]contentBuildDoc{{path: `C:\existing.txt`, frn: 10, text: []byte("existing"), class: contentClassText, version: 1}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = 7
	base.CheckpointUSN = 50
	stampContentTestScope("C:", base)
	if err := contentSaveFile(contentIndexPathForDB(vol.dbPath), base); err != nil {
		t.Fatal(err)
	}
	stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
		return 102, []usnChange{
			{FRN: 30, Reason: usnReasonFileCreate},
			{FRN: 40, Reason: usnReasonRenameNew},
			{FRN: 10, Reason: usnReasonRenameNew},
		}, nil
	})

	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)

	// The create and the move-in are queued; the intra-volume rename is not.
	q := vol.contentCoord.takeQueued()
	if len(q) != 2 {
		t.Fatalf("queued = %v; want the created and moved-in FRNs only", q)
	}
	got := map[uint64]bool{}
	for _, frn := range q {
		got[frn] = true
	}
	if !got[30] || !got[40] || got[10] {
		t.Fatalf("queued = %v; want {30,40}", q)
	}
}

// A USN-origin sidecar with no watermark must be treated as stale and never
// attached ready.
func TestContentAttachRejectsZeroCheckpoint(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	dir := t.TempDir()
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 5, Checkpoint: 50}
	idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10}}
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)

	cidx := newContentIndex()
	cidx.Origin = contentOriginUSN
	cidx.JournalID = 5
	cidx.CheckpointUSN = 0
	stampContentTestScope("C:", cidx)
	if err := contentSaveFile(contentIndexPathForDB(vol.dbPath), cidx); err != nil {
		t.Fatal(err)
	}
	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)
	if got := vol.content.stateOf(); got != contentStateStale {
		t.Fatalf("state = %q; want stale", got)
	}
	if vol.content.usableForQuery() {
		t.Fatal("a zero-checkpoint base must not be usable")
	}
}

// An old-format .gsx is rejected on load and never attached ready.
func TestContentAttachRejectsOldFormat(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	dir := t.TempDir()
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 5, Checkpoint: 50}
	idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10}}
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)

	cidx := newContentIndex()
	cidx.Origin = contentOriginUSN
	cidx.JournalID = 5
	cidx.CheckpointUSN = 50
	data := contentIndexEncode(cidx)

	// A v1 sidecar (old magic + version) must not decode.
	old := append([]byte(nil), data...)
	copy(old[0:8], []byte("GOSCX001"))
	binary.LittleEndian.PutUint32(old[8:], 1)
	if _, err := contentIndexDecode(old); err == nil {
		t.Fatal("old-format .gsx must be rejected")
	}
	// The version gate alone rejects a mismatched version.
	badVersion := append([]byte(nil), data...)
	binary.LittleEndian.PutUint32(badVersion[8:], 1)
	if _, err := contentIndexDecode(badVersion); err == nil {
		t.Fatal("unsupported .gsx version must be rejected")
	}

	gsx := contentIndexPathForDB(vol.dbPath)
	if err := os.WriteFile(gsx, old, 0o644); err != nil {
		t.Fatal(err)
	}
	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)
	if got := vol.content.stateOf(); got == contentStateReady {
		t.Fatal("old-format sidecar was attached ready")
	}
	if vol.content.usableForQuery() {
		t.Fatal("old-format sidecar must not be usable")
	}
}

// A capped restart catch-up must schedule a rebuild that advances the base to
// the live checkpoint, so the volume self-heals instead of staying permanently
// degraded with a gated fold and an ever-growing delta.
func TestContentCatchUpCapSelfHealsRebuild(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(0xABC)
	const live = uint64(120)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: int64(live)})

	vol, _ := contentBuildTestVolume(t, dir, journal, live, []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 6},
	})
	base := newContentIndex()
	base.Origin = contentOriginUSN
	base.JournalID = journal
	base.CheckpointUSN = 50
	base.Docs = []contentDoc{{DocID: 0, FRN: 10, ContentType: contentClassText, ExtractorVersion: 1}}
	stampContentTestScope(vol.volume, base)
	if err := contentSaveFile(contentIndexPathForDB(vol.dbPath), base); err != nil {
		t.Fatal(err)
	}

	restore := contentCatchUpMaxChanges
	contentCatchUpMaxChanges = 1
	t.Cleanup(func() { contentCatchUpMaxChanges = restore })
	stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
		return int64(live), []usnChange{
			{FRN: 10, Reason: usnReasonDataOverwrite},
			{FRN: 11, Reason: usnReasonDataOverwrite},
		}, nil
	})

	s := contentTestService(t)
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)

	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after capped catch-up self-heal = %q; want ready", got)
	}
	if vol.content.healthIncomplete() {
		t.Fatal("a completed rebuild must clear the incomplete flag")
	}
	// PF-5b/P6-3: the rebuild's attach re-ran catch-up to the live checkpoint,
	// which must clear catchUpPending too, or the fold stays gated forever.
	if vol.content.catchUpPendingNow() {
		t.Fatal("a completed rebuild's catch-up must clear catchUpPending")
	}
	if vol.content.catchUpIncomplete() {
		t.Fatal("a completed rebuild must leave no catch-up gate")
	}
	rebuilt, err := contentLoadFile(contentIndexPathForDB(vol.dbPath))
	releaseContentIndexOnCleanup(t, rebuilt)
	if err != nil {
		t.Fatalf("load rebuilt sidecar: %v", err)
	}
	if rebuilt.CheckpointUSN != live {
		t.Fatalf("rebuilt checkpoint = %d; want %d", rebuilt.CheckpointUSN, live)
	}
}
