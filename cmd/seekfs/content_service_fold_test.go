package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// contentFoldFixture is a ready content volume backed by a real on-disk base
// sidecar and real files, so the drain can extract churned content and a fold
// can persist it.
type contentFoldFixture struct {
	s      *goSearchService
	vol    *serviceVolumeIndex
	idx    *Index
	dbPath string
	paths  map[uint64]string
}

func newContentFoldFixture(t *testing.T, files []contentFixtureFile) *contentFoldFixture {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 1_000_000})

	const journal = uint64(7)
	const baseCP = int64(100)
	dir := t.TempDir()
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: journal, Checkpoint: baseCP}
	build := make([]contentBuildDoc, 0, len(files))
	paths := make(map[uint64]string, len(files))
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, []byte(f.text), 0o644); err != nil {
			t.Fatal(err)
		}
		paths[f.frn] = p
		idx.Records = append(idx.Records, CompactRecord{FRN: f.frn, ParentFRN: 1, Parent: -1, Name: f.name, Size: int64(len(f.text))})
		build = append(build, contentBuildDoc{path: `C:\` + f.name, frn: f.frn, text: []byte(f.text)})
	}
	contentIndexFRNs(idx)

	dbPath := filepath.Join(dir, "seekfs_c.gsi")
	base, err := assembleContentIndex(build, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = journal
	base.CheckpointUSN = uint64(baseCP)
	if err := contentSaveFile(contentIndexPathForDB(dbPath), base); err != nil {
		t.Fatal(err)
	}

	vol := newServiceVolumeIndex(dbPath, idx)
	s := &goSearchService{stop: make(chan struct{})}
	t.Cleanup(func() { close(s.stop) })
	s.attachContentForVolume(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after attach = %q; want ready", got)
	}
	return &contentFoldFixture{s: s, vol: vol, idx: idx, dbPath: dbPath, paths: paths}
}

// churn rewrites an FRN's file and drives it through the coordinator (observe →
// queue → extract) exactly like a live replay + drain tick.
func (f *contentFoldFixture) churn(t *testing.T, frn uint64, usn int64, text string) {
	t.Helper()
	if err := os.WriteFile(f.paths[frn], []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	f.vol.contentCoord.observeChanges([]usnChange{{FRN: frn, USN: usn, Reason: usnReasonDataOverwrite | usnReasonClose}})
	f.vol.contentCoord.processQueue(func(frn uint64) (string, bool) {
		p, ok := f.paths[frn]
		return p, ok
	})
}

// PB2/WP1c: churn, fold, and the `.gsx` reload carries the edit and advances the
// checkpoint past the base; the delta drains.
func TestContentFoldPersistsDeltaAndAdvancesCheckpoint(t *testing.T) {
	f := newContentFoldFixture(t, []contentFixtureFile{
		{10, "alpha.txt", "alphaold base"},
		{20, "beta.txt", "betaneedle base"},
	})
	f.churn(t, 10, 105, "gammanew needlegamma")
	if got := f.vol.content.deltaView().liveCount(); got != 1 {
		t.Fatalf("delta live count before fold = %d; want 1", got)
	}

	f.s.runContentFold(f.vol)

	reloaded, err := contentLoadFile(contentIndexPathForDB(f.dbPath))
	if err != nil {
		t.Fatalf("reload folded sidecar: %v", err)
	}
	if reloaded.CheckpointUSN != 105 {
		t.Fatalf("folded CheckpointUSN = %d; want 105", reloaded.CheckpointUSN)
	}
	if got := f.vol.content.deltaView().liveCount(); got != 0 {
		t.Fatalf("delta live count after fold = %d; want 0", got)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:needlegamma", false); len(hits) != 1 {
		t.Fatalf("folded edit not findable: %v", hits)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:alphaold", false); len(hits) != 0 {
		t.Fatalf("stale base text still findable: %v", hits)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:betaneedle", false); len(hits) != 1 {
		t.Fatalf("fold lost an untouched base doc: %v", hits)
	}
}

// PB2: after a fold, a fresh attach over the persisted sidecar (a restart)
// serves the edit without any catch-up.
func TestContentFoldSurvivesRestart(t *testing.T) {
	f := newContentFoldFixture(t, []contentFixtureFile{
		{10, "alpha.txt", "alphaold base"},
		{20, "beta.txt", "betaneedle base"},
	})
	f.churn(t, 10, 105, "gammanew needlegamma")
	f.s.runContentFold(f.vol)

	idx2 := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 105}
	idx2.Records = []CompactRecord{
		{FRN: 10, ParentFRN: 1, Parent: -1, Name: "alpha.txt", Size: 10},
		{FRN: 20, ParentFRN: 1, Parent: -1, Name: "beta.txt", Size: 10},
	}
	contentIndexFRNs(idx2)
	vol2 := newServiceVolumeIndex(f.dbPath, idx2)
	s2 := &goSearchService{stop: make(chan struct{})}
	defer close(s2.stop)
	s2.attachContentForVolume(vol2)
	if got := vol2.content.stateOf(); got != contentStateReady {
		t.Fatalf("restart attach state = %q; want ready", got)
	}
	if hits, _ := contentServiceSearch(t, vol2, "content:needlegamma", false); len(hits) != 1 {
		t.Fatalf("edit not findable after restart: %v", hits)
	}
	if hits, _ := contentServiceSearch(t, vol2, "content:alphaold", false); len(hits) != 0 {
		t.Fatalf("stale content findable after restart: %v", hits)
	}
}

// A delete that lands after the fold snapshot must not be resurrected by the
// folded base: the entry differs from the snapshot, so the tombstone is kept.
func TestContentFoldDeleteRacingSnapshotIsNotResurrected(t *testing.T) {
	f := newContentFoldFixture(t, []contentFixtureFile{
		{10, "alpha.txt", "alphaold base"},
		{20, "beta.txt", "betaneedle base"},
	})
	f.churn(t, 10, 105, "gammanew needlegamma")

	restore := contentFoldSnapshotHook
	contentFoldSnapshotHook = func(vol *serviceVolumeIndex) {
		vol.contentCoord.observeChanges([]usnChange{{FRN: 10, USN: 106, Reason: usnReasonFileDelete}})
	}
	t.Cleanup(func() { contentFoldSnapshotHook = restore })

	f.s.runContentFold(f.vol)

	if hits, _ := contentServiceSearch(t, f.vol, "content:needlegamma", false); len(hits) != 0 {
		t.Fatalf("a delete racing the fold was resurrected: %v", hits)
	}
	if f.vol.content.deltaView().len() == 0 {
		t.Fatal("the racing tombstone was dropped, so a later edit/delete could be lost")
	}
}

// M1: churn far past the threshold and the live delta stays bounded because the
// fold fires from the drain path.
func TestContentFoldBoundsDeltaMemory(t *testing.T) {
	restoreDocs := contentDeltaFoldMaxDocs
	contentDeltaFoldMaxDocs = 4
	t.Cleanup(func() { contentDeltaFoldMaxDocs = restoreDocs })
	restoreBytes := contentDeltaFoldMaxBytes
	contentDeltaFoldMaxBytes = int64(1) << 40
	t.Cleanup(func() { contentDeltaFoldMaxBytes = restoreBytes })

	files := make([]contentFixtureFile, 0, 24)
	for i := 0; i < 24; i++ {
		files = append(files, contentFixtureFile{frn: uint64(1000 + i), name: fmt.Sprintf("f%02d.txt", i), text: "base"})
	}
	f := newContentFoldFixture(t, files)
	for i := range files {
		f.churn(t, files[i].frn, int64(200+i), fmt.Sprintf("changedneedle%d", i))
		f.s.maybeFoldContentDelta(f.vol)
	}
	if got := f.vol.content.deltaView().liveCount(); got >= contentDeltaFoldMaxDocs {
		t.Fatalf("delta live count %d not bounded below %d", got, contentDeltaFoldMaxDocs)
	}
	reloaded, err := contentLoadFile(contentIndexPathForDB(f.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CheckpointUSN <= 100 {
		t.Fatalf("churn+fold never advanced the checkpoint: %d", reloaded.CheckpointUSN)
	}
}

// M1 (bytes): the byte bound alone triggers the fold, so a few large edits do
// not accumulate resident text.
func TestContentFoldBoundsDeltaBytes(t *testing.T) {
	restoreDocs := contentDeltaFoldMaxDocs
	contentDeltaFoldMaxDocs = 1 << 30
	t.Cleanup(func() { contentDeltaFoldMaxDocs = restoreDocs })
	restoreBytes := contentDeltaFoldMaxBytes
	contentDeltaFoldMaxBytes = int64(1)
	t.Cleanup(func() { contentDeltaFoldMaxBytes = restoreBytes })

	f := newContentFoldFixture(t, []contentFixtureFile{
		{10, "alpha.txt", "alphaold base"},
		{20, "beta.txt", "betaneedle base"},
	})
	f.churn(t, 10, 105, "gammanew needlegamma with a large body of text")
	if got := f.vol.content.deltaView().liveBytes(); got == 0 {
		t.Fatal("delta live bytes not tracked")
	}
	f.s.maybeFoldContentDelta(f.vol)
	if got := f.vol.content.deltaView().liveBytes(); got >= contentDeltaFoldMaxBytes {
		t.Fatalf("delta live bytes %d not bounded below %d", got, contentDeltaFoldMaxBytes)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:needlegamma", false); len(hits) != 1 {
		t.Fatalf("byte-bounded fold lost the edit: %v", hits)
	}
}

// A failed fold (e.g. the sidecar could not be written) must keep the delta
// verbatim and never publish a partial base.
func TestContentFoldFailureKeepsDelta(t *testing.T) {
	f := newContentFoldFixture(t, []contentFixtureFile{
		{10, "alpha.txt", "alphaold base"},
		{20, "beta.txt", "betaneedle base"},
	})
	f.churn(t, 10, 105, "gammanew needlegamma")

	restore := contentFoldSave
	contentFoldSave = func(string, *contentIndex) error { return errors.New("injected save failure") }
	t.Cleanup(func() { contentFoldSave = restore })

	f.s.runContentFold(f.vol)

	if got := f.vol.content.deltaView().liveCount(); got != 1 {
		t.Fatalf("failed fold dropped the delta: live=%d; want 1", got)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:needlegamma", false); len(hits) != 1 {
		t.Fatalf("edit lost after a failed fold: %v", hits)
	}
	reloaded, err := contentLoadFile(contentIndexPathForDB(f.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CheckpointUSN != 100 {
		t.Fatalf("failed fold wrote the sidecar: checkpoint=%d; want 100", reloaded.CheckpointUSN)
	}
}

// PF-5a blocker (two streams): a fold must not persist observedUSN while restart
// catch-up is still pending. observedUSN is a max over the live replay and
// catch-up streams, so it can run ahead of the delta's coverage; persisting it
// would make the next restart resume past records catch-up never replayed,
// permanently missing their content.
func TestContentFoldSkippedWhileCatchUpPending(t *testing.T) {
	f := newContentFoldFixture(t, []contentFixtureFile{
		{10, "alpha.txt", "alphaold base"},
		{20, "beta.txt", "betaold base"},
	})
	// The live replay stream observes an edit at USN 300 while catch-up has not
	// finished (its stream still has to replay USN 150 for beta).
	f.vol.content.markCatchUpPending()
	f.churn(t, 10, 300, "gammanew needlegamma")

	f.s.runContentFold(f.vol)

	reloaded, err := contentLoadFile(contentIndexPathForDB(f.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CheckpointUSN != 100 {
		t.Fatalf("fold advanced the checkpoint to %d while catch-up was pending; want 100", reloaded.CheckpointUSN)
	}
	if got := f.vol.content.deltaView().liveCount(); got != 1 {
		t.Fatalf("gated fold consumed the delta: live=%d; want 1", got)
	}

	// A crash + restart: the record checkpoint replayed to 300, and beta changed
	// at USN 150 while the service was stopped. Because the gated fold did not
	// inflate the persisted content checkpoint, catch-up resumes from 100 and
	// replays the skipped change.
	if err := os.WriteFile(f.paths[20], []byte("betaNEW needlebeta2"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
		if startUSN != 100 {
			t.Fatalf("restart catch-up start = %d; want 100 (the persisted content checkpoint)", startUSN)
		}
		return 300, []usnChange{{FRN: 20, USN: 150, Reason: usnReasonDataOverwrite | usnReasonClose}}, nil
	})
	idx2 := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 7, Checkpoint: 300}
	idx2.Records = []CompactRecord{
		{FRN: 10, ParentFRN: 1, Parent: -1, Name: "alpha.txt", Size: 10},
		{FRN: 20, ParentFRN: 1, Parent: -1, Name: "beta.txt", Size: 10},
	}
	contentIndexFRNs(idx2)
	vol2 := newServiceVolumeIndex(f.dbPath, idx2)
	s2 := &goSearchService{stop: make(chan struct{})}
	defer close(s2.stop)
	s2.attachContentForVolume(vol2)
	vol2.contentCoord.processQueue(func(frn uint64) (string, bool) {
		p, ok := f.paths[frn]
		return p, ok
	})
	if hits, _ := contentServiceSearch(t, vol2, "content:needlebeta2", false); len(hits) != 1 {
		t.Fatalf("restart skipped the USN catch-up never replayed: %v", hits)
	}
}

// PF-5a blocker (pending work): a fold must not persist observedUSN while an
// eligible change has been observed but not yet extracted (dirty/queue), or the
// checkpoint would claim content the folded base does not contain.
func TestContentFoldSkippedWhileWorkPending(t *testing.T) {
	f := newContentFoldFixture(t, []contentFixtureFile{
		{10, "alpha.txt", "alphaold base"},
		{20, "beta.txt", "betaneedle base"},
	})
	f.churn(t, 10, 105, "gammanew needlegamma")
	// A second change observed but left in dirty: its USN (150) runs ahead of
	// what the delta represents.
	f.vol.contentCoord.observeChanges([]usnChange{{FRN: 20, USN: 150, Reason: usnReasonDataOverwrite}})
	if f.vol.contentCoord.dirtyCount() == 0 {
		t.Fatal("setup: expected pending dirty work")
	}

	f.s.runContentFold(f.vol)

	reloaded, err := contentLoadFile(contentIndexPathForDB(f.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CheckpointUSN != 100 {
		t.Fatalf("fold persisted observedUSN %d with unextracted work pending; want 100", reloaded.CheckpointUSN)
	}
	if got := f.vol.content.deltaView().liveCount(); got != 1 {
		t.Fatalf("gated fold consumed the delta: live=%d; want 1", got)
	}
}

// PF-5a blocker (Incomplete): a capped catch-up's Incomplete/degraded signal
// must survive a fold attempt; only a fully-caught-up volume may fold and clear
// it.
func TestContentFoldSkippedWhileCatchUpIncomplete(t *testing.T) {
	f := newContentFoldFixture(t, []contentFixtureFile{
		{10, "alpha.txt", "alphaold base"},
		{20, "beta.txt", "betaneedle base"},
	})
	f.churn(t, 10, 105, "gammanew needlegamma")
	f.vol.content.markCatchUpIncomplete()

	f.s.runContentFold(f.vol)

	if !f.vol.content.healthIncomplete() {
		t.Fatal("a fold cleared the capped catch-up's Incomplete flag")
	}
	if got := f.vol.content.stateOf(); got != contentStateDegraded {
		t.Fatalf("state after gated fold = %q; want degraded", got)
	}
	reloaded, err := contentLoadFile(contentIndexPathForDB(f.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CheckpointUSN != 100 {
		t.Fatalf("fold advanced a degraded volume's checkpoint to %d; want 100", reloaded.CheckpointUSN)
	}
}

// A rename after the fold snapshot that reuses the content hash must stay in the
// delta: removeUnchanged compares Path, not just (Deleted, Hash), so the new
// path is not dropped.
func TestContentFoldRenameAfterSnapshotRetained(t *testing.T) {
	f := newContentFoldFixture(t, []contentFixtureFile{
		{10, "alpha.txt", "alphaold base"},
		{20, "beta.txt", "betaneedle base"},
	})
	f.churn(t, 10, 105, "gammanew needlegamma")
	newPath := f.paths[10] + ".renamed"

	restore := contentFoldSnapshotHook
	contentFoldSnapshotHook = func(vol *serviceVolumeIndex) {
		for _, d := range vol.content.deltaView().live() {
			if d.FRN == 10 {
				d.Path = newPath
				vol.content.deltaView().upsert(d)
			}
		}
	}
	t.Cleanup(func() { contentFoldSnapshotHook = restore })

	f.s.runContentFold(f.vol)

	reloaded, err := contentLoadFile(contentIndexPathForDB(f.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CheckpointUSN != 105 {
		t.Fatalf("fold did not run: checkpoint=%d; want 105", reloaded.CheckpointUSN)
	}
	live := f.vol.content.deltaView().live()
	if len(live) != 1 || live[0].FRN != 10 || live[0].Path != newPath {
		t.Fatalf("rename-after-snapshot dropped from the delta: %+v (want frn 10 path %q)", live, newPath)
	}
}
