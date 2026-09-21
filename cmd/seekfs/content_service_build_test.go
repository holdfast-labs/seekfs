package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubContentBuildSync runs the service-owned content build inline and removes
// the IO governor pause/batching, so tests are deterministic.
func stubContentBuildSync(t *testing.T) {
	t.Helper()
	restoreRun := contentBuildRun
	restorePause := contentBuildBatchPause
	restoreSize := contentBuildBatchSize
	restoreHook := contentBuildSnapshotHook
	restoreBatchHook := contentBuildSnapshotBatchHook
	restoreIndexing := contentBuildIndexingHook
	restoreBackoff := contentBuildRetryBackoff
	contentBuildRun = func(fn func()) { fn() }
	contentBuildBatchPause = 0
	contentBuildBatchSize = 1
	contentBuildSnapshotHook = func(*goSearchService, *serviceVolumeIndex) {}
	contentBuildSnapshotBatchHook = func(*goSearchService, *serviceVolumeIndex) {}
	contentBuildIndexingHook = func(*contentVolumeState) {}
	contentBuildRetryBackoff = 0
	t.Cleanup(func() {
		contentBuildRun = restoreRun
		contentBuildBatchPause = restorePause
		contentBuildBatchSize = restoreSize
		contentBuildSnapshotHook = restoreHook
		contentBuildSnapshotBatchHook = restoreBatchHook
		contentBuildIndexingHook = restoreIndexing
		contentBuildRetryBackoff = restoreBackoff
	})
}

func contentTestService(t *testing.T) *goSearchService {
	t.Helper()
	s := &goSearchService{stop: make(chan struct{})}
	t.Cleanup(func() {
		if !s.serviceStopping() {
			close(s.stop)
		}
	})
	return s
}

func contentBuildTestVolume(t *testing.T, dir string, journal, checkpoint uint64, records []CompactRecord) (*serviceVolumeIndex, string) {
	t.Helper()
	idx := &Index{Source: "usn", Volume: dir, Compact: true, JournalID: journal, Checkpoint: int64(checkpoint)}
	idx.Records = append([]CompactRecord(nil), records...)
	contentIndexFRNs(idx)
	dbPath := filepath.Join(t.TempDir(), "seekfs_c.gsi")
	return newServiceVolumeIndex(dbPath, idx), contentIndexPathForDB(dbPath)
}

// M7 (WP10): a service build whose encoded sidecar exceeds the cap must refuse
// to publish and surface the size in health, never write a truncated index.
func TestContentServiceBuildOverCapRefuses(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("hello needle world"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(0xABC)
	const cp = int64(120)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})

	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 18},
	})

	restoreCap := contentGSXMaxBytes
	contentGSXMaxBytes = 64
	t.Cleanup(func() { contentGSXMaxBytes = restoreCap })

	s := contentTestService(t)
	s.ensureContentBuild(vol)

	if _, err := os.Stat(gsx); !os.IsNotExist(err) {
		t.Fatalf("over-cap build wrote a sidecar (stat err=%v)", err)
	}
	if got := vol.content.stateOf(); got != contentStateDegraded {
		t.Fatalf("state after over-cap build = %q; want degraded", got)
	}
	h := vol.content.healthSnapshot(0)
	if !strings.Contains(h.BuildError, "size cap") || h.SidecarCap != contentGSXMaxBytes {
		t.Fatalf("health after over-cap build = %+v; want a size-cap reason and cap", h)
	}
}

// A drain tick during a background build must not flip an indexing volume to
// ready: the build owns the state until it publishes or fails.
func TestContentIndexingNotClobberedByDrain(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	st := newContentVolumeState("C:")
	st.markIndexing(5)
	st.setBuildProgress(2, 5)
	st.markDrained()
	if got := st.stateOf(); got != contentStateIndexing {
		t.Fatalf("state after markDrained during build = %q; want indexing", got)
	}
	h := st.healthSnapshot(0)
	if h.State != contentStateIndexing || h.BuildDone != 2 || h.BuildTotal != 5 {
		t.Fatalf("health after markDrained = %+v; want indexing 2/5", h)
	}
}

// A ready USN volume with no usable `.gsx` must get a service-owned build:
// persisted, attached, and searchable, with the `indexing` state (and progress)
// observable. No CLI step is involved.
func TestContentServiceBuildsMissingBase(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("hello needle world"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(0xABC)
	const cp = int64(120)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})

	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 18},
	})
	var indexing contentHealth
	contentBuildIndexingHook = func(st *contentVolumeState) { indexing = st.healthSnapshot(0) }

	s := contentTestService(t)
	s.ensureContentBuild(vol)

	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after service build = %q; want ready", got)
	}
	if indexing.State != contentStateIndexing || indexing.BuildTotal != 1 {
		t.Fatalf("indexing health = %+v; want state=indexing total=1", indexing)
	}
	if _, err := os.Stat(gsx); err != nil {
		t.Fatalf("sidecar was not persisted: %v", err)
	}
	built, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatalf("load built sidecar: %v", err)
	}
	if built.Origin != contentOriginUSN || built.JournalID != journal || built.CheckpointUSN != uint64(cp) {
		t.Fatalf("built header origin=%d journal=%d checkpoint=%d; want USN/%d/%d", built.Origin, built.JournalID, built.CheckpointUSN, journal, cp)
	}
	hits, err := contentServiceSearch(t, vol, "content:needle", false)
	if err != nil {
		t.Fatalf("search built content: %v", err)
	}
	if len(hits) != 1 || hits[0].Path != note {
		t.Fatalf("built content not searchable: %v", hits)
	}
}

// The service build defaults to an extension allowlist, not the whole volume
// (M5): an out-of-allowlist file with matching text is not indexed.
func TestContentServiceBuildDefaultScope(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	note := filepath.Join(dir, "note.txt")
	junk := filepath.Join(dir, "blob.xyz")
	if err := os.WriteFile(note, []byte("allowed needle term"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(junk, []byte("hidden uniqueterm"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(7)
	const cp = int64(50)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})

	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 19},
		{FRN: 20, ParentFRN: 20, Parent: -1, Name: "blob.xyz", Size: 16},
	})
	s := contentTestService(t)
	s.ensureContentBuild(vol)

	built, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Docs) != 1 {
		t.Fatalf("built %d docs; want 1 (only the allowlisted .txt)", len(built.Docs))
	}
	if hits, _ := contentServiceSearch(t, vol, "content:uniqueterm", false); len(hits) != 0 {
		t.Fatalf("out-of-allowlist file was indexed: %v", hits)
	}
	if hits, _ := contentServiceSearch(t, vol, "content:needle", false); len(hits) != 1 {
		t.Fatalf("allowlisted file missing from content: %v", hits)
	}
}

// After a journal reset invalidates the FRN-keyed content base, the service
// rebuilds it and re-attaches: content self-heals instead of staying stale.
func TestContentServiceBuildSelfHealsAfterJournalReset(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("selfheal needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 50})
	vol, gsx := contentBuildTestVolume(t, dir, 7, 50, []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 15},
	})

	// A valid base for journal 7 attaches without a build.
	base, err := assembleContentIndex([]contentBuildDoc{{path: note, frn: 10, text: []byte("selfheal needle"), class: contentClassText, version: 1}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = 7
	base.CheckpointUSN = 50
	if err := contentSaveFile(gsx, base); err != nil {
		t.Fatal(err)
	}
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("initial attach state = %q; want ready", got)
	}

	// Simulate the filename rebuild against a new journal generation.
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 8, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})
	reset := &Index{Source: "usn", Volume: dir, Compact: true, JournalID: 8, Checkpoint: 60}
	reset.Records = []CompactRecord{{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 15}}
	contentIndexFRNs(reset)
	replaceServiceVolumeContents(vol, newServiceVolumeIndex(vol.dbPath, reset))
	if got := vol.content.stateOf(); got != contentStateStale {
		t.Fatalf("journal-reset state = %q; want stale before self-heal", got)
	}
	if vol.content.usableForQuery() {
		t.Fatal("stale content must not be usable before self-heal")
	}

	// The rebuild hook re-attaches or reschedules; here it rebuilds.
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after self-heal = %q; want ready", got)
	}
	rebuilt, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.JournalID != 8 || rebuilt.CheckpointUSN != 60 {
		t.Fatalf("rebuilt header journal=%d checkpoint=%d; want 8/60", rebuilt.JournalID, rebuilt.CheckpointUSN)
	}
	if hits, err := contentServiceSearch(t, vol, "content:needle", false); err != nil || len(hits) != 1 {
		t.Fatalf("self-healed content not searchable: %v %v", hits, err)
	}
}

// PF-4 bumps the content format version (normalized text + policy changed). An
// existing sidecar written by an older version must be rejected and rebuilt by
// the service, never attached stale.
func TestContentOldVersionSidecarRebuiltNotAttached(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("upgrade needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(0x515)
	const cp = int64(64)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 14},
	})

	// Write a well-formed sidecar, then downgrade its magic+version to the
	// previous format (a pre-PF-4 v2 file).
	old, err := assembleContentIndex([]contentBuildDoc{{path: note, frn: 10, text: []byte("upgrade needle"), class: contentClassText, version: 1}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old.Origin = contentOriginUSN
	old.JournalID = journal
	old.CheckpointUSN = uint64(cp)
	data := contentIndexEncode(old)
	binary.LittleEndian.PutUint32(data[8:], contentIndexVersion-1)
	data[7] = byte('0' + (contentIndexVersion - 1))
	if err := os.WriteFile(gsx, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := contentLoadFile(gsx); err == nil {
		t.Fatal("an old-version sidecar must be rejected on load, not attached")
	}

	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after version-invalidated rebuild = %q; want ready", got)
	}
	rebuilt, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Version != contentIndexVersion {
		t.Fatalf("rebuilt version = %d; want %d", rebuilt.Version, contentIndexVersion)
	}
	if hits, err := contentServiceSearch(t, vol, "content:needle", false); err != nil || len(hits) != 1 {
		t.Fatalf("rebuilt content not searchable: %v %v", hits, err)
	}
}

// A volume that was not ready at startup (a rebuild owned it) is skipped, then
// gets content attached/build when it becomes ready — without a restart.
func TestContentServiceBuildSchedulesAfterNotReadyVolumeBecomesReady(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("later needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(5)
	const cp = int64(70)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 13},
	})
	s := contentTestService(t)

	vol.state = "stale"
	s.ensureContentBuild(vol)
	if _, err := os.Stat(gsx); !os.IsNotExist(err) {
		t.Fatalf("build ran for a non-ready volume (sidecar err=%v)", err)
	}

	vol.state = "ready"
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after late ready = %q; want ready", got)
	}
	if hits, err := contentServiceSearch(t, vol, "content:needle", false); err != nil || len(hits) != 1 {
		t.Fatalf("late-attached content not searchable: %v %v", hits, err)
	}
}

// The build must not hold indexMu across file extraction: a persist/swap takes
// indexMu.Lock, which must succeed while the build extracts. The snapshot hook
// runs after the metadata lock is released; acquiring Lock there proves it.
func TestContentServiceBuildReleasesIndexLockDuringExtraction(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("lock needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(9)
	const cp = int64(30)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, _ := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 11},
	})
	s := contentTestService(t)

	locked := make(chan struct{})
	contentBuildSnapshotHook = func(s *goSearchService, vol *serviceVolumeIndex) {
		done := make(chan struct{})
		go func() {
			s.indexMu.Lock()
			s.indexMu.Unlock()
			close(done)
		}()
		select {
		case <-done:
			close(locked)
		case <-time.After(5 * time.Second):
			t.Error("build held indexMu across extraction (persist/swap would deadlock)")
		}
	}
	s.ensureContentBuild(vol)
	select {
	case <-locked:
	default:
		t.Fatal("snapshot hook did not acquire the released indexMu")
	}
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state = %q; want ready", got)
	}
}

// A journal generation change under a running build aborts it before publish
// and reschedules; it must never publish a sidecar for the old generation.
func TestContentServiceBuildAbortsOnJournalChange(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("abort needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(11)
	const cp = int64(40)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 12},
	})
	s := contentTestService(t)

	contentBuildSnapshotHook = func(s *goSearchService, vol *serviceVolumeIndex) {
		s.indexMu.Lock()
		vol.journalID++
		vol.replayGen.Add(1)
		vol.state = "stale" // a rebuild owns the volume; ensure must not reschedule
		s.indexMu.Unlock()
	}
	s.ensureContentBuild(vol)

	if _, err := os.Stat(gsx); !os.IsNotExist(err) {
		t.Fatalf("aborted build published a sidecar (err=%v)", err)
	}
	if got := vol.content.stateOf(); got == contentStateReady {
		t.Fatalf("aborted build left state = %q; want not-ready", got)
	}
}

// A journal generation change under a ready volume aborts the build and the
// hook reschedules it against the new generation, which then succeeds.
func TestContentServiceBuildReschedulesAfterGenerationChange(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("resched needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(21)
	const cp = int64(33)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal + 1, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 14},
	})
	s := contentTestService(t)

	fired := false
	contentBuildSnapshotHook = func(s *goSearchService, vol *serviceVolumeIndex) {
		if fired {
			return
		}
		fired = true
		s.indexMu.Lock()
		vol.journalID++
		vol.replayGen.Add(1)
		vol.baseCheckpoint = 44
		s.indexMu.Unlock()
	}
	s.ensureContentBuild(vol)

	if !fired {
		t.Fatal("generation-change hook never ran")
	}
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after reschedule = %q; want ready", got)
	}
	rebuilt, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatalf("load rescheduled sidecar: %v", err)
	}
	if rebuilt.JournalID != journal+1 || rebuilt.CheckpointUSN != 44 {
		t.Fatalf("rescheduled header journal=%d checkpoint=%d; want %d/44", rebuilt.JournalID, rebuilt.CheckpointUSN, journal+1)
	}
	if hits, err := contentServiceSearch(t, vol, "content:needle", false); err != nil || len(hits) != 1 {
		t.Fatalf("rescheduled content not searchable: %v %v", hits, err)
	}
}

// Shutdown cancels an in-flight build; it must not publish or leave the volume
// claiming ready.
func TestContentServiceBuildCancelsOnShutdown(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("cancel needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(13)
	const cp = int64(20)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 12},
	})
	s := contentTestService(t)
	contentBuildSnapshotHook = func(s *goSearchService, vol *serviceVolumeIndex) { close(s.stop) }

	s.ensureContentBuild(vol)
	if _, err := os.Stat(gsx); !os.IsNotExist(err) {
		t.Fatalf("canceled build published a sidecar (err=%v)", err)
	}
	if got := vol.content.stateOf(); got == contentStateReady {
		t.Fatalf("canceled build left state = %q; want not-ready", got)
	}
}

// A volume that is mid-build (indexing) must not answer a content query even if
// its (retained) delta is populated: the delta alone is a partial answer and
// would otherwise be reported complete while the fresh base is still building.
func TestContentIndexingDeltaNotServed(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	st := newContentVolumeState("C:")
	st.deltaView().upsert(contentDeltaDoc{FRN: 1, Text: []byte("needle")})
	st.markIndexing(10)
	if st.usableForQuery() {
		t.Fatal("an indexing volume with a populated delta must not be usable for content queries")
	}
}

// The streaming assembler must not retain the caller's text slice: a source
// that reuses one backing buffer for every doc is only safe because each doc is
// normalized and copied into the text store as it arrives.
func TestContentAssembleStreamConsumesSourceBuffer(t *testing.T) {
	wants := []struct {
		frn  uint64
		path string
		term string
	}{
		{1, `C:\alpha.txt`, "alphauniqueterm"},
		{2, `C:\beta.txt`, "betauniqueterm"},
		{3, `C:\gamma.txt`, "gammauniqueterm"},
	}
	scratch := make([]byte, 0, 64)
	i := 0
	source := func() (contentBuildDoc, bool, error) {
		if i >= len(wants) {
			return contentBuildDoc{}, false, nil
		}
		w := wants[i]
		i++
		scratch = append(scratch[:0], w.term...)
		return contentBuildDoc{frn: w.frn, path: w.path, text: scratch}, true, nil
	}
	idx, err := assembleContentIndexStream(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range wants {
		hits := r.search(w.term, 0)
		if len(hits) != 1 || hits[0].Path != w.path {
			t.Fatalf("term %q = %v; want exactly %q", w.term, contentPathsOf(hits), w.path)
		}
	}
}

// The assembler must pull and process one document at a time, not buffer the
// whole source: when the source is asked for doc k, docs < k must already be
// normalized and indexed.
func TestContentAssembleStreamProcessesAsItPulls(t *testing.T) {
	restore := contentAssembleStreamHook
	defer func() { contentAssembleStreamHook = restore }()
	processed := -1
	contentAssembleStreamHook = func(docID int) { processed = docID }

	const n = 8
	pulled := 0
	source := func() (contentBuildDoc, bool, error) {
		if pulled >= n {
			return contentBuildDoc{}, false, nil
		}
		if processed != pulled-1 {
			t.Errorf("pulled doc %d but only processed through %d; assembler is buffering the source", pulled, processed)
		}
		doc := contentBuildDoc{
			frn:  uint64(pulled + 1),
			path: fmt.Sprintf(`C:\stream%02d.txt`, pulled),
			text: []byte(fmt.Sprintf("term%02d distinct words here", pulled)),
		}
		pulled++
		return doc, true, nil
	}
	idx, err := assembleContentIndexStream(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Docs) != n || pulled != n {
		t.Fatalf("docs=%d pulled=%d; want %d/%d", len(idx.Docs), pulled, n, n)
	}
}

// The streaming assembler must produce byte-identical output to the slice
// assembler for the same (already sorted) input.
func TestContentAssembleStreamMatchesSlice(t *testing.T) {
	build := []contentBuildDoc{
		{frn: 10, path: `C:\a.txt`, text: []byte("first needle")},
		{frn: 20, path: `C:\b.txt`, text: []byte("second needle")},
	}
	a, err := assembleContentIndex(append([]contentBuildDoc(nil), build...), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	source := func() (contentBuildDoc, bool, error) {
		if i >= len(build) {
			return contentBuildDoc{}, false, nil
		}
		d := build[i]
		i++
		return d, true, nil
	}
	b, err := assembleContentIndexStream(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b.BuiltAt = a.BuiltAt
	if !bytes.Equal(contentIndexEncode(a), contentIndexEncode(b)) {
		t.Fatal("streamed assembly differs from slice assembly")
	}
}

func contentManyRecords(n int) []CompactRecord {
	records := make([]CompactRecord, n)
	for i := range records {
		records[i] = CompactRecord{FRN: uint64(i + 1), ParentFRN: 1, Parent: -1, Name: fmt.Sprintf("f%06d.txt", i), Size: 4}
	}
	return records
}

// The metadata snapshot must not hold indexMu.RLock across its whole pass: a
// writer (persist/rebuild) must be able to take indexMu.Lock between batches.
func TestContentSnapshotReleasesIndexLockBetweenBatches(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	dir := t.TempDir()
	records := contentManyRecords(contentBuildSnapshotBatch + 8)
	vol, _ := contentBuildTestVolume(t, dir, 3, 10, records)
	s := contentTestService(t)

	restore := contentBuildSnapshotBatchHook
	defer func() { contentBuildSnapshotBatchHook = restore }()
	batches := 0
	contentBuildSnapshotBatchHook = func(s *goSearchService, vol *serviceVolumeIndex) {
		batches++
		done := make(chan struct{})
		go func() {
			s.indexMu.Lock()
			s.indexMu.Unlock()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("snapshot held indexMu.RLock across the whole scan")
		}
	}

	items, _, _, result := s.snapshotContentBuildItems(vol, defaultContentBuildOptions())
	if result != contentBuildSnapshotOK {
		t.Fatalf("snapshot result = %d; want OK", result)
	}
	if batches < 2 {
		t.Fatalf("snapshot batches = %d; want >= 2", batches)
	}
	if len(items) != len(records) {
		t.Fatalf("snapshot items = %d; want %d", len(items), len(records))
	}
}

// A generation change mid-snapshot must abort the copy so the build rebases
// against the new generation instead of mixing records from two generations.
func TestContentSnapshotAbortsOnGenerationChange(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	dir := t.TempDir()
	records := contentManyRecords(contentBuildSnapshotBatch + 8)
	vol, _ := contentBuildTestVolume(t, dir, 3, 10, records)
	s := contentTestService(t)

	restore := contentBuildSnapshotBatchHook
	defer func() { contentBuildSnapshotBatchHook = restore }()
	contentBuildSnapshotBatchHook = func(s *goSearchService, vol *serviceVolumeIndex) {
		vol.replayGen.Add(1)
	}

	_, _, _, result := s.snapshotContentBuildItems(vol, defaultContentBuildOptions())
	if result != contentBuildSnapshotChanged {
		t.Fatalf("snapshot result = %d; want Changed", result)
	}
}

// A volume whose generation keeps changing must not spin builds forever: after
// contentBuildMaxAttempts retries the build gives up and surfaces degraded with
// a BuildError in health.
func TestContentServiceBuildGivesUpAfterRepeatedGenerationChanges(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("spin needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(31)
	const cp = int64(12)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 11},
	})
	s := contentTestService(t)
	contentBuildSnapshotHook = func(s *goSearchService, vol *serviceVolumeIndex) {
		s.indexMu.Lock()
		vol.journalID++
		vol.replayGen.Add(1)
		s.indexMu.Unlock()
	}

	s.ensureContentBuild(vol)

	if _, err := os.Stat(gsx); !os.IsNotExist(err) {
		t.Fatalf("gave-up build published a sidecar (err=%v)", err)
	}
	h := vol.content.healthSnapshot(0)
	if h.State != contentStateDegraded || h.BuildError == "" {
		t.Fatalf("health after give-up = %+v; want degraded with BuildError", h)
	}
}

// The service build must isolate a panicking extractor (skip, no crash) and a
// ctx-ignoring hanging extractor (deadline, no wedge): it still publishes the
// good document and records both skips in the policy.
func TestContentServiceBuildPanicAndTimeoutIsolated(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	restoreTimeout := contentExtractDocTimeout
	contentExtractDocTimeout = 50 * time.Millisecond
	t.Cleanup(func() { contentExtractDocTimeout = restoreTimeout })
	restoreExtractors := contentExtractors
	contentExtractors = append(append([]contentExtractor(nil), contentExtractors...), contentPanicExtractor{}, contentSpinExtractor{})
	t.Cleanup(func() { contentExtractors = restoreExtractors })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.panic"), []byte("boom"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.spin"), []byte("hang"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(77)
	const cp = int64(90)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.panic", Size: 4},
		{FRN: 20, ParentFRN: 10, Parent: -1, Name: "b.spin", Size: 4},
		{FRN: 30, ParentFRN: 10, Parent: -1, Name: "c.txt", Size: 6},
	})
	s := contentTestService(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ensureContentBuild(vol)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("service build wedged on a panicking or hanging extractor")
	}

	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after build = %q; want ready", got)
	}
	built, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatalf("load built sidecar: %v", err)
	}
	if len(built.Docs) != 1 {
		t.Fatalf("built %d docs; want 1 (only c.txt)", len(built.Docs))
	}
	if built.Policy.Skipped < 2 {
		t.Fatalf("policy skipped = %d; want the panic and the hang both skipped", built.Policy.Skipped)
	}
}
