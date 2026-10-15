package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// Pin the fixture directory as an explicit content root: the service build
	// now derives an auto scope from the index (git worktrees + known folders),
	// and a synthetic temp volume is neither, so without this the build would
	// index nothing.
	t.Setenv("SEEKFS_CONTENT_SCOPE", "explicit")
	t.Setenv("SEEKFS_CONTENT_ROOTS", dir)
	idx := &Index{Source: "usn", Volume: dir, Compact: true, JournalID: journal, Checkpoint: int64(checkpoint)}
	idx.Records = append([]CompactRecord(nil), records...)
	contentIndexFRNs(idx)
	dbPath := filepath.Join(t.TempDir(), "seekfs_c.gsi")
	vol := newServiceVolumeIndex(dbPath, idx)
	releaseVolumeContentOnCleanup(t, vol)
	return vol, contentIndexPathForDB(dbPath)
}

// scheduleContentBuild may reuse an existing valid sidecar; scheduleContentRebuild
// exists to replace a still-valid-but-incomplete sidecar and must NOT re-attach
// it. Re-attaching would setReady, which clears the incomplete flag and skips the
// rebuild, silently publishing a base missing records. runContentBuild must
// therefore only attempt the reuse attach when the caller allows it.
func TestContentRebuildDoesNotReattachSidecar(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello needle world"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(0xABC)
	const cp = int64(120)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})

	vol, _ := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 18},
	})
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition: build state = %q; want ready", got)
	}

	// Make the build's snapshot unavailable (no watermark) so only an attach can
	// publish ready: this isolates the reuse decision from a real rebuild.
	vol.index.Checkpoint = 0
	vol.baseCheckpoint = 0

	// The reuse path (scheduleContentBuild) attaches the valid sidecar.
	vol.content.markStale("test")
	s.runContentBuild(vol, true)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("reuse=true did not attach the valid sidecar; state = %q", got)
	}

	// The rebuild path (scheduleContentRebuild) must not re-attach it.
	vol.content.markStale("test")
	s.runContentBuild(vol, false)
	if got := vol.content.stateOf(); got == contentStateReady {
		t.Fatalf("reuse=false re-attached the replaced sidecar (state=%q); the rebuild must not re-publish it", got)
	}
}

// A rebuild that aborts on a generation change retries. The retry must stay on
// the rebuild path: if it fell through to scheduleContentBuild it would attach
// the still-valid-but-incomplete sidecar, clearing health.Incomplete and
// abandoning the rebuild (the failure scheduleContentRebuild exists to prevent).
func TestContentRebuildRetryDoesNotReattachSidecar(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello needle world"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(0xABC)
	const cp = int64(120)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})

	vol, _ := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 18},
	})
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition: build state = %q; want ready", got)
	}

	// Force every rebuild attempt to abort on a generation change, so the retry
	// loop runs; it exhausts contentBuildMaxAttempts and fails the build.
	contentBuildSnapshotHook = func(_ *goSearchService, v *serviceVolumeIndex) {
		v.replayGen.Add(1)
	}

	vol.content.markCatchUpIncomplete()
	s.scheduleContentRebuild(vol)

	if got := vol.content.stateOf(); got == contentStateReady {
		t.Fatalf("rebuild retry re-attached the incomplete sidecar (state=%q)", got)
	}
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

// A sharded build whose shard exceeds the per-file cap refuses to publish,
// exactly like a single-file over-cap build: no manifest is written and the
// volume surfaces degraded with the size-cap reason, never a partial corpus.
func TestContentServiceShardedBuildOverCapRefuses(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ path, body string }{
		{filepath.Join(dir, "a.txt"), "hello needle world"},
		{filepath.Join(sub, "b.txt"), "hello needle world"},
	} {
		if err := os.WriteFile(f.path, []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const journal = uint64(0xABD)
	const cp = int64(121)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})

	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: 18},
		{FRN: 11, ParentFRN: 11, Parent: -1, Name: "sub", Mode: uint32(os.ModeDir)},
		{FRN: 12, Parent: 1, ParentFRN: 11, Name: "b.txt", Size: 18},
	})

	// Force two shards (one per folder) and a per-file cap every shard breaks.
	restoreTarget := contentShardTargetBytes
	contentShardTargetBytes = 1
	t.Cleanup(func() { contentShardTargetBytes = restoreTarget })
	restoreCap := contentGSXMaxBytes
	contentGSXMaxBytes = 64
	t.Cleanup(func() { contentGSXMaxBytes = restoreCap })

	s := contentTestService(t)
	s.ensureContentBuild(vol)

	if _, err := os.Stat(gsx); !os.IsNotExist(err) {
		t.Fatalf("over-cap sharded build wrote a manifest (stat err=%v)", err)
	}
	if got := vol.content.stateOf(); got != contentStateDegraded {
		t.Fatalf("state after over-cap sharded build = %q; want degraded", got)
	}
	h := vol.content.healthSnapshot(0)
	if !strings.Contains(h.BuildError, "size cap") {
		t.Fatalf("health after over-cap sharded build = %+v; want a size-cap reason", h)
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
	releaseContentIndexOnCleanup(t, built)
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

// The service build defaults to sniff selection, not an extension allowlist:
// an unknown-extension text file is indexed, a known-binary extension is
// rejected without opening, and a NUL-sniffed file tallies as binary.
func TestContentServiceBuildDefaultScopeSniffs(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	note := filepath.Join(dir, "note.txt")
	junk := filepath.Join(dir, "blob.xyz")
	pic := filepath.Join(dir, "pic.png")
	bin := filepath.Join(dir, "junk.dat")
	if err := os.WriteFile(note, []byte("allowed needle term"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(junk, []byte("hidden uniqueterm"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pic, []byte("png words here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("bin\x00ary"), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(7)
	const cp = int64(50)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})

	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 19},
		{FRN: 20, ParentFRN: 20, Parent: -1, Name: "blob.xyz", Size: 17},
		{FRN: 30, ParentFRN: 30, Parent: -1, Name: "pic.png", Size: 14},
		{FRN: 40, ParentFRN: 40, Parent: -1, Name: "junk.dat", Size: 7},
	})
	s := contentTestService(t)
	s.ensureContentBuild(vol)

	built, err := contentLoadFile(gsx)
	releaseContentIndexOnCleanup(t, built)
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Docs) != 2 {
		t.Fatalf("built %d docs; want 2 (note.txt and the sniffed blob.xyz)", len(built.Docs))
	}
	if built.Policy.Binary != 1 {
		t.Fatalf("policy binary = %d; want 1 (junk.dat)", built.Policy.Binary)
	}
	if hits, _ := contentServiceSearch(t, vol, "content:uniqueterm", false); len(hits) != 1 {
		t.Fatalf("sniffed text file missing from content: %v", hits)
	}
	if hits, _ := contentServiceSearch(t, vol, "content:needle", false); len(hits) != 1 {
		t.Fatalf("allowlisted file missing from content: %v", hits)
	}
	if hits, _ := contentServiceSearch(t, vol, "content:\"png words\"", false); len(hits) != 0 {
		t.Fatalf("skip-listed extension was indexed: %v", hits)
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
	releaseContentIndexOnCleanup(t, rebuilt)
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
	releaseContentIndexOnCleanup(t, rebuilt)
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
	releaseContentIndexOnCleanup(t, rebuilt)
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
	idx, err := assembleContentIndexStream(source, t.TempDir(), contentFilterID{})
	if err != nil {
		t.Fatal(err)
	}
	// v11 verifies from disk: serve the synthetic docs through the stub.
	texts := make(map[string]string, len(wants))
	for _, w := range wants {
		texts[w.path] = w.term
	}
	stubContentDiskText(t, texts)
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range wants {
		hits, _ := r.search(w.term, 0)
		if len(hits) != 1 || hits[0].Path != w.path {
			t.Fatalf("term %q = %v; want exactly %q", w.term, contentPathsOf(hits), w.path)
		}
	}
}

// The assembler must stream, not buffer the whole source: when the source is
// asked for doc k, docs before k minus one read-ahead batch must already be
// normalized and indexed.
func TestContentAssembleStreamProcessesAsItPulls(t *testing.T) {
	restore := contentAssembleStreamHook
	defer func() { contentAssembleStreamHook = restore }()
	restoreDocs := contentAssembleBatchDocs
	restoreBytes := contentAssembleBatchBytes
	contentAssembleBatchDocs = 3
	contentAssembleBatchBytes = 1 << 20
	defer func() { contentAssembleBatchDocs = restoreDocs; contentAssembleBatchBytes = restoreBytes }()
	// Single tokenize worker so the hook order is deterministic.
	restoreWorkers := contentBuildWorkers
	contentBuildWorkers = 1
	defer func() { contentBuildWorkers = restoreWorkers }()
	processed := -1
	contentAssembleStreamHook = func(docID int) { processed = docID }

	const n = 8
	pulled := 0
	source := func() (contentBuildDoc, bool, error) {
		if pulled >= n {
			return contentBuildDoc{}, false, nil
		}
		if pulled-processed-1 > contentAssembleBatchDocs {
			t.Errorf("pulled doc %d but only processed through %d; assembler buffered more than one batch", pulled, processed)
		}
		doc := contentBuildDoc{
			frn:  uint64(pulled + 1),
			path: fmt.Sprintf(`C:\stream%02d.txt`, pulled),
			text: []byte(fmt.Sprintf("term%02d distinct words here", pulled)),
		}
		pulled++
		return doc, true, nil
	}
	idx, err := assembleContentIndexStream(source, t.TempDir(), contentFilterID{})
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
	b, err := assembleContentIndexStream(source, t.TempDir(), contentFilterID{})
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

func TestContentSnapshotReportsFileLimit(t *testing.T) {
	vol, _ := contentBuildTestVolume(t, t.TempDir(), 3, 10, contentManyRecords(2))
	opts := defaultContentBuildOptions()
	opts.MaxFiles = 1
	items, _, _, result := contentTestService(t).snapshotContentBuildItems(vol, opts)
	if result != contentBuildSnapshotLimited || len(items) != 1 {
		t.Fatalf("snapshot result=%d items=%d; want limited/1", result, len(items))
	}
}

func TestContentSnapshotFileLimitPrefersRepository(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	vol, _ := contentBuildTestVolume(t, dir, 3, 10, []CompactRecord{
		{FRN: 1, Parent: -1, Name: "a.txt", Size: 8},
		{FRN: 2, Parent: -1, Name: "repo", Mode: uint32(os.ModeDir)},
		{FRN: 3, Parent: 1, ParentFRN: 2, Name: ".git", Mode: uint32(os.ModeDir)},
		{FRN: 4, Parent: 1, ParentFRN: 2, Name: "z.txt", Size: 8},
	})
	s := contentTestService(t)
	scope, ok := s.resolvedContentScope(vol)
	if !ok || len(scope.Repos) != 1 || scope.Repos[0] != contentNormPath(repo) {
		t.Fatalf("detected repos = %v, ok=%v; want %q", scope.Repos, ok, repo)
	}
	opts := defaultServiceContentBuildOptions()
	opts.Scope = &scope
	opts.MaxFiles = 1
	items, _, _, result := s.snapshotContentBuildItems(vol, opts)
	if result != contentBuildSnapshotLimited || len(items) != 1 || filepath.Base(items[0].path) != "z.txt" {
		t.Fatalf("limited snapshot = %+v, result=%d; want repo file", items, result)
	}
}

func TestContentServiceBuildBudgetUsesExtractedTextAndPriority(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(dir, "a.txt"):  "rootword",
		filepath.Join(repo, "z.txt"): "repoword",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const journal = uint64(92)
	const cp = int64(100)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 1, Parent: -1, Name: "a.txt", Size: 8},
		{FRN: 2, Parent: -1, Name: "repo", Mode: uint32(os.ModeDir)},
		{FRN: 3, Parent: 1, ParentFRN: 2, Name: ".git", Mode: uint32(os.ModeDir)},
		{FRN: 4, Parent: 1, ParentFRN: 2, Name: "z.txt", Size: 8},
	})
	s := contentTestService(t)
	// Budgets are in projected index bytes (contentIndexCostEstimate).
	s.contentCfg.Content.BudgetBytes = 2 * contentIndexCostEstimate(8)
	s.ensureContentBuild(vol)
	built, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	releaseContentIndexOnCleanup(t, built)
	if len(built.Docs) != 2 || built.Policy.ScopeDropped != 0 {
		t.Fatalf("budgeted index docs=%d dropped=%d; want both short extracted texts", len(built.Docs), built.Policy.ScopeDropped)
	}
	// A single-doc allowance must choose the repo, even though the
	// explicit-root file comes first in the record index and by path.
	oneDoc := contentIndexCostEstimate(8)
	s.contentCfg.Content.BudgetBytes = oneDoc
	s.scheduleContentRebuild(vol)
	built, err = contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	releaseContentIndexOnCleanup(t, built)
	if len(built.Docs) != 1 || built.Docs[0].FRN != 4 || built.Policy.ScopeDropped != 1 {
		t.Fatalf("priority index docs=%+v dropped=%d; want only repo file", built.Docs, built.Policy.ScopeDropped)
	}
	// The enforced budget is recorded for health.
	if built.Policy.BudgetBytes != oneDoc {
		t.Fatalf("priority index policy = %+v; want budget=%d", built.Policy, oneDoc)
	}
}

// A biting byte budget records EXACT dropped files and extracted-text bytes:
// the ordered stream is consumed to the end, tallying each excluded file's
// real extracted size instead of the remaining-item estimate.
func TestContentServiceBuildBudgetDropsExactFilesAndBytes(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	dir := t.TempDir()
	bodies := map[string]string{
		"a.txt": "alpha",
		"b.txt": "bravo!",
		"c.txt": "charlie?",
	}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const journal = uint64(93)
	const cp = int64(101)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 1, Parent: -1, Name: "a.txt", Size: 5},
		{FRN: 2, Parent: -1, Name: "b.txt", Size: 6},
		{FRN: 3, Parent: -1, Name: "c.txt", Size: 8},
	})
	s := contentTestService(t)
	// Fits a.txt + b.txt in projected index bytes, drops c.txt (8 bytes of
	// extracted text).
	budget := contentIndexCostEstimate(5) + contentIndexCostEstimate(6)
	s.contentCfg.Content.BudgetBytes = budget
	s.ensureContentBuild(vol)
	built, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	releaseContentIndexOnCleanup(t, built)
	if len(built.Docs) != 2 || built.Policy.ScopeDropped != 1 {
		t.Fatalf("budgeted index docs=%d dropped=%d; want 2 docs, 1 dropped", len(built.Docs), built.Policy.ScopeDropped)
	}
	if built.Policy.ScopeDroppedBytes != int64(len("charlie?")) || built.Policy.BudgetBytes != budget {
		t.Fatalf("budgeted index policy = %+v; want dropped_bytes=%d budget=%d", built.Policy, len("charlie?"), budget)
	}
	h := vol.content.healthSnapshot(0)
	if h.ScopeDropped != 1 || h.ScopeDroppedBytes != int64(len("charlie?")) || h.BudgetBytes != budget {
		t.Fatalf("budget health = %+v; want exact dropped files/bytes/budget", h)
	}
	if !h.Incomplete {
		t.Fatal("budget-truncated sidecar must report incomplete")
	}
}

// A biting free-space reserve records exact dropped files and bytes like an
// explicit budget, but names the reserve instead: the default scope indexes
// everything until the projected index would breach the volume's reserve.
func TestContentServiceBuildReserveDropsExactFilesAndBytes(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	oldFree, oldTotal := contentFreeBytesFn, contentTotalBytesFn
	defer func() { contentFreeBytesFn, contentTotalBytesFn = oldFree, oldTotal }()
	// 10 GiB reserve (5% of 500 GiB, capped) plus the in-flight shard
	// scratch, and 9 bytes of headroom: the build peak (compressed spool +
	// output, ×0.8) allows 11 extracted bytes.
	contentTotalBytesFn = func(_ string) (uint64, error) { return 500 << 30, nil }
	contentFreeBytesFn = func(_ string) (uint64, error) { return contentScopeReserveMaxBytes + contentScopeScratchSlack() + 9, nil }
	dir := t.TempDir()
	bodies := map[string]string{
		"a.txt": "alpha",
		"b.txt": "bravo!",
		"c.txt": "charlie?",
	}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const journal = uint64(94)
	const cp = int64(102)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 1, Parent: -1, Name: "a.txt", Size: 5},
		{FRN: 2, Parent: -1, Name: "b.txt", Size: 6},
		{FRN: 3, Parent: -1, Name: "c.txt", Size: 8},
	})
	s := contentTestService(t)
	// No explicit budget: the reserve governs. Fits a.txt + b.txt (5+6=11
	// of 11 allowed), drops c.txt (8 bytes of extracted text).
	s.ensureContentBuild(vol)
	built, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	releaseContentIndexOnCleanup(t, built)
	if len(built.Docs) != 2 || built.Policy.ScopeDropped != 1 {
		t.Fatalf("reserve index docs=%d dropped=%d; want 2 docs, 1 dropped", len(built.Docs), built.Policy.ScopeDropped)
	}
	if built.Policy.ScopeDroppedBytes != int64(len("charlie?")) {
		t.Fatalf("reserve index policy = %+v; want dropped_bytes=%d", built.Policy, len("charlie?"))
	}
	if built.Policy.BudgetBytes != 0 || built.Policy.ReserveBytes != contentScopeReserveMaxBytes {
		t.Fatalf("reserve index policy = %+v; want budget=0 reserve=20GiB", built.Policy)
	}
	h := vol.content.healthSnapshot(0)
	if h.ScopeDropped != 1 || h.ReserveBytes != contentScopeReserveMaxBytes || h.BudgetBytes != 0 {
		t.Fatalf("reserve health = %+v; want exact dropped files and the reserve", h)
	}
	if !h.Incomplete {
		t.Fatal("reserve-truncated sidecar must report incomplete")
	}
}

// A sharded build removes each shard's scratch once the shard is saved, so the
// volume never holds a second copy of every saved shard next to the spool.
func TestContentServiceBuildRemovesShardScratchAsItGoes(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	restoreTarget, restoreConc := contentShardTargetBytes, contentShardBuildConcurrency
	contentShardTargetBytes, contentShardBuildConcurrency = 1, 1 // one shard per file, sequential
	t.Cleanup(func() { contentShardTargetBytes, contentShardBuildConcurrency = restoreTarget, restoreConc })
	dir := t.TempDir()
	names := []string{"a.txt", "b.txt", "c.txt", "d.txt"}
	var recs []CompactRecord
	for i, name := range names {
		body := "body of " + name
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, CompactRecord{FRN: uint64(i + 1), Parent: -1, Name: name, Size: int64(len(body))})
	}
	const journal = uint64(95)
	const cp = int64(103)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), recs)
	// The write guard checks free space right before each shard save; count
	// the shard scratch directories alive at that moment.
	oldFree := contentFreeBytesFn
	t.Cleanup(func() { contentFreeBytesFn = oldFree })
	maxScratch, checks := 0, 0
	contentFreeBytesFn = func(path string) (uint64, error) {
		if path == contentShardDir(gsx) {
			checks++
			live, _ := filepath.Glob(filepath.Join(filepath.Dir(gsx), contentWorkPrefix+"*", "shard-*"))
			if len(live) > maxScratch {
				maxScratch = len(live)
			}
		}
		return 0, errTestReserve
	}
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	built, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	releaseContentIndexOnCleanup(t, built)
	if len(built.Docs) != len(names) {
		t.Fatalf("sharded build docs=%d; want %d", len(built.Docs), len(names))
	}
	if checks < 2 {
		t.Fatalf("saw %d shard save checks; want one per shard (sharded path not taken)", checks)
	}
	if maxScratch > 1 {
		t.Fatalf("%d shard scratch directories alive at a save; want at most the in-flight one", maxScratch)
	}
}

// A sharded build reframes its spool into one file per shard group and
// deletes each file as soon as its shard is saved, so the assembly peak is
// one group's spool plus the shards saved so far. The per-group spool files
// alive at each shard save must strictly decrease: with N groups the saves
// observe N, N-1, ..., 1 files.
func TestContentServiceBuildDeletesGroupSpoolsAsItGoes(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	restoreTarget, restoreConc := contentShardTargetBytes, contentShardBuildConcurrency
	contentShardTargetBytes, contentShardBuildConcurrency = 1, 1 // one shard per file, sequential
	t.Cleanup(func() { contentShardTargetBytes, contentShardBuildConcurrency = restoreTarget, restoreConc })
	dir := t.TempDir()
	names := []string{"a.txt", "b.txt", "c.txt", "d.txt"}
	var recs []CompactRecord
	for i, name := range names {
		// Repetitive bodies stage far below the spool-cap share, so the
		// build takes the split path.
		body := strings.Repeat("needle haystack "+name+" ", 30)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, CompactRecord{FRN: uint64(i + 1), Parent: -1, Name: name, Size: int64(len(body))})
	}
	const journal = uint64(96)
	const cp = int64(104)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), recs)
	// At each write-guard check before a shard save, count the per-group
	// spool files alive in the build work dir.
	oldFree := contentFreeBytesFn
	t.Cleanup(func() { contentFreeBytesFn = oldFree })
	var alive []int
	contentFreeBytesFn = func(path string) (uint64, error) {
		if path == contentShardDir(gsx) {
			files, _ := filepath.Glob(filepath.Join(filepath.Dir(gsx), contentWorkPrefix+"*", "spool-*"))
			alive = append(alive, len(files))
		}
		return 1 << 40, nil
	}
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	built, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	releaseContentIndexOnCleanup(t, built)
	if len(built.Docs) != len(names) {
		t.Fatalf("sharded build docs=%d; want %d", len(built.Docs), len(names))
	}
	// Every save must observe one fewer group spool than the previous one,
	// ending with the last group's own file: nothing lingers past its shard.
	if len(alive) < 2 {
		t.Fatalf("saw %d shard saves (%v); want at least 2 (sharded path not taken)", len(alive), alive)
	}
	for i := 1; i < len(alive); i++ {
		if alive[i] != alive[i-1]-1 {
			t.Fatalf("group spools alive at saves = %v; want strictly decreasing (split path not taken?)", alive)
		}
	}
	if alive[len(alive)-1] != 1 {
		t.Fatalf("group spools alive at saves = %v; want the last group freed by its own save", alive)
	}
}

// A sharded build whose spool stages past its cap share skips the reframe:
// copying it would double an already-large spool. Assembly reads the shared
// spool as before and still publishes every doc.
func TestContentServiceBuildSkipsSplitPastSpoolCap(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	restoreTarget, restoreConc := contentShardTargetBytes, contentShardBuildConcurrency
	contentShardTargetBytes, contentShardBuildConcurrency = 1, 1 // one shard per file, sequential
	t.Cleanup(func() { contentShardTargetBytes, contentShardBuildConcurrency = restoreTarget, restoreConc })
	dir := t.TempDir()
	names := []string{"a.txt", "b.txt"}
	var recs []CompactRecord
	for i, name := range names {
		// Tiny docs stage more than the 0.35 cap share: each frame carries
		// fixed overhead larger than the text.
		body := "needle " + name
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, CompactRecord{FRN: uint64(i + 1), Parent: -1, Name: name, Size: int64(len(body))})
	}
	const journal = uint64(97)
	const cp = int64(105)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), recs)
	oldFree := contentFreeBytesFn
	t.Cleanup(func() { contentFreeBytesFn = oldFree })
	splitSeen := false
	contentFreeBytesFn = func(path string) (uint64, error) {
		if path == contentShardDir(gsx) {
			if files, _ := filepath.Glob(filepath.Join(filepath.Dir(gsx), contentWorkPrefix+"*", "spool-*")); len(files) > 0 {
				splitSeen = true
			}
		}
		return 1 << 40, nil
	}
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	built, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	releaseContentIndexOnCleanup(t, built)
	if len(built.Docs) != len(names) {
		t.Fatalf("sharded build docs=%d; want %d", len(built.Docs), len(names))
	}
	if splitSeen {
		t.Fatal("per-group spool files staged past the spool cap; want the shared spool")
	}
}

// denseHexFixtureBody is one deterministic high-entropy document: 76-char
// hex lines (PEM-style) after a marker line. The fastest spool level stores
// it at ~1.0x (pure LZ, no entropy coding) while the trigram space stays
// bounded (16^3), so it exercises the past-cap path without exploding the
// posting builders: the minified/bundled-JS regime from the E: guard trip.
func denseHexFixtureBody(t *testing.T, seed int64, marker string) []byte {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	raw := make([]byte, 16<<10)
	if _, err := r.Read(raw); err != nil {
		t.Fatal(err)
	}
	enc := fmt.Sprintf("%x", raw)
	var sb strings.Builder
	sb.WriteString(marker)
	sb.WriteByte('\n')
	for i := 0; i < len(enc); i += 76 {
		end := i + 76
		if end > len(enc) {
			end = len(enc)
		}
		sb.WriteString(enc[i:end])
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}

type denseBuildResult struct {
	docs     int
	dropped  int64
	maxPeak  uint64
	hits     int
	count    int
	absentQ  int   // queried absent marker index, or -1 when none was provable
	splitSeq []int // per-group spool files alive at each shard save
	incompl  bool
	state    string
	buildErr string
}

// On an incompressible corpus the adaptive allowance stops selection earlier
// than on compressible text of the same sizes, the measured peak stays within
// headroom, and the build publishes instead of tripping the write-time guard
// (the E: failure: a guard trip after selection spent the headroom left no
// index at all). Both arms share one headroom H sized so the 0.8 floor admits
// everything but the adapted ~1.65x multiplier on the dense arm does not.
func TestContentServiceBuildAdaptsToIncompressibleSpool(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	oldTrust, oldCadence := contentSpoolTrustBytes, contentAllowanceCheckEvery
	oldTarget, oldConc := contentShardTargetBytes, contentShardBuildConcurrency
	contentSpoolTrustBytes = 256 << 10
	contentAllowanceCheckEvery = 1 << 20
	contentShardTargetBytes, contentShardBuildConcurrency = 4<<20, 1
	t.Cleanup(func() {
		contentSpoolTrustBytes, contentAllowanceCheckEvery = oldTrust, oldCadence
		contentShardTargetBytes, contentShardBuildConcurrency = oldTarget, oldConc
	})
	oldFree, oldTotal := contentFreeBytesFn, contentTotalBytesFn
	t.Cleanup(func() { contentFreeBytesFn, contentTotalBytesFn = oldFree, oldTotal })

	const ndocs = 1280
	body := denseHexFixtureBody(t, 1, "needle-marker-0")
	// Fixture precondition: the body must stage past the split cap (the
	// past-cap path) but well inside the adaptive budget. Fail loud if a
	// future encoder changes the ratio instead of silently testing nothing.
	var rBody float64
	{
		f, err := os.Create(filepath.Join(t.TempDir(), "r.spool"))
		if err != nil {
			t.Fatal(err)
		}
		w, err := newContentSpoolWriter(f)
		if err != nil {
			t.Fatal(err)
		}
		_, comp, err := w.Append(body)
		if err != nil {
			t.Fatal(err)
		}
		rBody = float64(comp) / float64(len(body))
		_ = w.Close()
		_ = f.Close()
	}
	if rBody < 0.5 || rBody > 1.1 {
		t.Fatalf("fixture spool ratio = %.3f; want in [0.5, 1.1] (past the %.2f cap, inside the budget)", rBody, contentScopeSpoolMultiplier)
	}

	// One headroom for both arms: the floor admits the whole ~40 MB corpus
	// but the adapted multiplier on the dense arm stops at ~60% of it.
	// Calibrated: dense peak all-admitted is ~1.32x plus ~11 MB of
	// per-shard spill runs (gram + dict-builder runs scale with records, so
	// the 40 MB scale keeps them diluted), so H + slack must sit below that
	// (a fixed 0.8 would trip) and above the adapted publish.
	const headroom = 50 << 20
	slack := contentScopeScratchSlack()

	run := func(name string, bodies [][]byte) denseBuildResult {
		t.Helper()
		dir := t.TempDir()
		var recs []CompactRecord
		var frn uint64 = 1000
		for i, b := range bodies {
			sub := fmt.Sprintf("g%03d", i)
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
			fname := fmt.Sprintf("f%03d.txt", i)
			if err := os.WriteFile(filepath.Join(dir, sub, fname), b, 0o644); err != nil {
				t.Fatal(err)
			}
			frn++
			drec := CompactRecord{FRN: frn, Parent: -1, Name: sub, Mode: uint32(os.ModeDir)}
			recs = append(recs, drec)
			frn++
			recs = append(recs, CompactRecord{FRN: frn, Parent: int32(len(recs) - 1), ParentFRN: drec.FRN, Name: fname, Size: int64(len(b))})
		}
		const journal = uint64(98)
		const cp = int64(106)
		stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
		vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), recs)
		reserve := uint64(contentScopeReserveMaxBytes)
		contentTotalBytesFn = func(_ string) (uint64, error) { return 500 << 30, nil }
		free0 := reserve + slack + headroom
		// Transient + output bytes this build holds on the sidecar volume:
		// the work trees (spool, scratch, group files) plus the published
		// output next to them.
		sidecarTreeBytes := func() uint64 {
			var tot uint64
			matches, _ := filepath.Glob(filepath.Join(filepath.Dir(gsx), contentWorkPrefix+"*"))
			for _, m := range matches {
				_ = filepath.Walk(m, func(path string, info os.FileInfo, err error) error {
					if err == nil && !info.IsDir() {
						tot += uint64(info.Size())
					}
					return nil
				})
			}
			if fi, err := os.Stat(gsx); err == nil {
				tot += uint64(fi.Size())
			}
			entries, _ := os.ReadDir(contentShardDir(gsx))
			for _, e := range entries {
				if fi, err := e.Info(); err == nil && !fi.IsDir() {
					tot += uint64(fi.Size())
				}
			}
			return tot
		}
		var res denseBuildResult
		contentFreeBytesFn = func(path string) (uint64, error) {
			// Deterministic split observation: the write guard runs right
			// before each shard save, when the alive group files are exact.
			if path == contentShardDir(gsx) {
				files, _ := filepath.Glob(filepath.Join(filepath.Dir(gsx), contentWorkPrefix+"*", "spool-*"))
				res.splitSeq = append(res.splitSeq, len(files))
			}
			// Model depletion like a real disk: the stubbed free space falls
			// as the build stages bytes, so the spool credit in the allowance
			// cancels exactly as in production. A constant stub would count
			// staged bytes twice (once missing from free, once credited) and
			// inflate the allowance without bound.
			if used := sidecarTreeBytes(); used < free0 {
				return free0 - used, nil
			}
			return 0, nil
		}
		// Peak sampler over the sidecar trees (the stubbed free space cannot
		// deplete, so only real bytes prove the peak): every 50 ms plus a
		// final sample after publish.
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		sample := func() {
			tot := sidecarTreeBytes()
			mu.Lock()
			if tot > res.maxPeak {
				res.maxPeak = tot
			}
			mu.Unlock()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					sample()
				}
			}
		}()
		s := contentTestService(t)
		s.ensureContentBuild(vol)
		close(stop)
		wg.Wait()
		sample()
		h0 := vol.content.healthSnapshot(0)
		// A truncated-but-published base is degraded with Incomplete set
		// (setReady); only a guard trip fails the build with a disk-space
		// BuildError and publishes nothing. The dense arm must land in the
		// former state, never the latter.
		if strings.Contains(h0.BuildError, "free") {
			t.Fatalf("%s: build error %q; want no guard trip", name, h0.BuildError)
		}
		res.state = vol.content.stateOf()
		res.buildErr = h0.BuildError
		res.incompl = h0.Incomplete
		built, err := contentLoadFile(gsx)
		if err != nil {
			t.Fatal(err)
		}
		res.docs = len(built.Docs)
		res.dropped = built.Policy.ScopeDropped
		if built.Policy.ScopeDroppedBytes <= 0 && res.dropped > 0 {
			t.Fatalf("%s: dropped %d files with no dropped bytes", name, res.dropped)
		}
		releaseContentIndexOnCleanup(t, built)
		// Directed queryability from index ground truth (selection order is
		// scope-priority, not path order, so the admitted set is read back,
		// not assumed): an admitted doc's marker must hit, and a marker
		// absent from the admitted set must miss. Markers cannot hide in hex
		// bodies (hex has no '-'/'m'/'k'/'t'), and an absent 4-digit marker
		// cannot prefix-collide with an admitted one, so both are exact.
		admitted := map[int]bool{}
		for _, d := range built.Docs {
			if d.FRN >= 1002 {
				if i := (int(d.FRN) - 1002) / 2; i >= 0 && i < ndocs && (int(d.FRN)-1002)%2 == 0 {
					admitted[i] = true
				}
			}
		}
		present, absent := -1, -1
		for i := 0; i < ndocs; i++ {
			if admitted[i] && present < 0 {
				present = i
			}
		}
		// An absent marker proves the tail is gone only when no admitted
		// marker extends it as a decimal prefix (marker-12 is a substring
		// of marker-127's line) and it is not the embedded marker-0 every
		// dense body carries. A 4-digit absent index always qualifies.
		for j := 1; j < ndocs && absent < 0; j++ {
			if admitted[j] {
				continue
			}
			safe := true
			js := fmt.Sprintf("%d", j)
			for k := 0; k < ndocs; k++ {
				if k != j && admitted[k] && strings.HasPrefix(fmt.Sprintf("%d", k), js) {
					safe = false
					break
				}
			}
			if safe {
				absent = j
			}
		}
		if present < 0 {
			t.Fatalf("%s: no admitted docs to query", name)
		}
		mid, err := contentServiceSearch(t, vol, fmt.Sprintf("content:needle-marker-%d", present), false)
		if err != nil {
			t.Fatal(err)
		}
		res.hits = len(mid)
		absentQ := absent
		if absentQ < 0 {
			absentQ = ndocs - 1 // all admitted: prove the tail is findable
		}
		last, err := contentServiceSearch(t, vol, fmt.Sprintf("content:needle-marker-%d", absentQ), false)
		if err != nil {
			t.Fatal(err)
		}
		res.count = len(last)
		res.absentQ = absent
		return res
	}

	dense := make([][]byte, 0, ndocs)
	comp := make([][]byte, 0, ndocs)
	for i := 0; i < ndocs; i++ {
		marker := fmt.Sprintf("needle-marker-%d", i)
		dense = append(dense, append([]byte(marker+"\n"), body...))
		// Compressible arm: same byte length, trivially compressible. Short
		// lines, like the dense arm's 76-char ones: one ~33 KB line would
		// read as minified content and be skipped by the inclusion policy
		// (contentLooksMinified, §6z).
		var cb strings.Builder
		cb.WriteString(marker)
		cb.WriteByte('\n')
		for cb.Len() < len(dense[i]) {
			cb.WriteString("needle haystack bundle chunk pack\n")
		}
		c := []byte(cb.String())
		comp = append(comp, c[:len(dense[i])])
	}

	incomp := run("dense", dense)
	complete := run("compressible", comp)

	// (1) The dense arm stops earlier: same sizes, same headroom, but the
	// adapted multiplier drops files the floor would admit.
	if complete.dropped != 0 || complete.docs != ndocs {
		t.Fatalf("compressible: docs=%d dropped=%d; want %d docs, nothing dropped", complete.docs, complete.dropped, ndocs)
	}
	if incomp.docs+int(incomp.dropped) != ndocs || incomp.dropped < 200 || incomp.docs > ndocs-200 {
		t.Fatalf("dense: docs=%d dropped=%d; want a substantial early stop of %d files", incomp.docs, incomp.dropped, ndocs)
	}
	if !incomp.incompl || complete.incompl {
		t.Fatalf("incomplete flags: dense=%v compressible=%v; want true/false", incomp.incompl, complete.incompl)
	}
	if incomp.state != contentStateDegraded || incomp.buildErr != "" {
		t.Fatalf("dense: state=%q buildErr=%q; want degraded-with-Incomplete (published partial, no guard trip)", incomp.state, incomp.buildErr)
	}
	if complete.state != contentStateReady {
		t.Fatalf("compressible: state=%q; want ready (complete publish)", complete.state)
	}
	// (2) The measured peak stays within headroom + scratch in both arms...
	if incomp.maxPeak > headroom+slack {
		t.Fatalf("dense peak %d exceeds headroom+slack %d", incomp.maxPeak, headroom+slack)
	}
	if complete.maxPeak > headroom+slack {
		t.Fatalf("compressible peak %d exceeds headroom+slack %d", complete.maxPeak, headroom+slack)
	}
	// ...and the split decision matches the projection in both arms: the
	// dense arm never stages per-group files (split skipped past the cap)
	// while the compressible arm deletes them as its shards save.
	for _, n := range incomp.splitSeq {
		if n != 0 {
			t.Fatalf("dense split observations = %v; want no per-group spool (split skipped)", incomp.splitSeq)
		}
	}
	if len(incomp.splitSeq) == 0 {
		t.Fatal("dense arm saw no shard saves; want at least one (sharded path not taken)")
	}
	for i := 1; i < len(complete.splitSeq); i++ {
		if complete.splitSeq[i] != complete.splitSeq[i-1]-1 {
			t.Fatalf("compressible split observations = %v; want strictly decreasing", complete.splitSeq)
		}
	}
	if n := len(complete.splitSeq); n == 0 || complete.splitSeq[n-1] != 1 {
		t.Fatalf("compressible split observations = %v; want the last group freed by its own save", complete.splitSeq)
	}
	// ...and the dense peak proves the adapted stop did real work: a fixed
	// 0.8 would have admitted everything and peaked past the budget.
	t.Logf("dense: docs=%d dropped=%d peak=%.2fMB r=%.3f; compressible: docs=%d dropped=%d peak=%.2fMB",
		incomp.docs, incomp.dropped, float64(incomp.maxPeak)/(1<<20), rBody,
		complete.docs, complete.dropped, float64(complete.maxPeak)/(1<<20))
	// (3) Published results are queryable in both arms: an admitted doc is
	// found, and the dense arm no longer contains a provably dropped tail.
	if incomp.hits < 1 {
		t.Fatalf("dense: present-marker hits=%d; want >=1 (admitted doc findable)", incomp.hits)
	}
	if incomp.absentQ < 0 {
		t.Fatal("dense: no safely-unfindable dropped marker; cannot prove the tail is gone")
	}
	if incomp.count != 0 {
		t.Fatalf("dense: absent-marker hits=%d; want 0 (dropped doc unfindable)", incomp.count)
	}
	if complete.hits < 1 || complete.count < 1 {
		t.Fatalf("compressible: present hits=%d tail hits=%d; want >=1 (all admitted)",
			complete.hits, complete.count)
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
	releaseContentIndexOnCleanup(t, built)
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
