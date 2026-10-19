package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// wrapReconFixture is a USN volume whose filename index is already past a
// journal wrap: the sidecar checkpoint (100) predates the journal's first
// valid USN (150), so attach cannot replay the range and must reconcile the
// live filename metadata against the doc table instead of rebuilding it.
type wrapReconFixture struct {
	dir   string
	aPath string
	bPath string
	cPath string
	vol   *serviceVolumeIndex
	gsx   string
	s     *goSearchService
	jwrap usnJournalDataV0
	calls *int
}

func newWrapReconFixture(t *testing.T) *wrapReconFixture {
	return newWrapReconFixturePolicy(t, contentBuildPolicy{})
}

// newWrapReconFixturePolicy is newWrapReconFixture with an explicit build
// policy on the base, so a test can stamp a reserve-truncated base
// (Policy.ReserveBytes) and exercise the reconcile budget's keep-out gate.
func newWrapReconFixturePolicy(t *testing.T, policy contentBuildPolicy) *wrapReconFixture {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.txt")
	bPath := filepath.Join(dir, "b.txt")
	cPath := filepath.Join(dir, "c.txt")
	if err := os.WriteFile(aPath, []byte("alpha new needlenew"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, []byte("beta needlebeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cPath, []byte("gamma needlegamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	stat := func(path string) (int64, int64) {
		t.Helper()
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.Size(), st.ModTime().UnixNano()
	}
	aSize, aMod := stat(aPath)
	bSize, bMod := stat(bPath)
	cSize, cMod := stat(cPath)

	const journal = uint64(7)
	// The filename side already rebuilt past the wrap: checkpoint 200, with
	// a.txt's record refreshed, b.txt gone, and c.txt picked up.
	vol, gsx := contentBuildTestVolume(t, dir, journal, 200, []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: aSize, ModUnix: aMod},
		{FRN: 30, ParentFRN: 30, Parent: -1, Name: "c.txt", Size: cSize, ModUnix: cMod},
	})
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	f := &wrapReconFixture{dir: dir, aPath: aPath, bPath: bPath, cPath: cPath, vol: vol, gsx: gsx, s: s}

	// The base predates the wrap: a.txt with old content, b.txt present,
	// c.txt unknown. Prints are distinct from the live stats so every
	// difference is a genuine reconcile case, not a stat tie.
	base, err := assembleContentIndex([]contentBuildDoc{
		{path: aPath, frn: 10, text: []byte("alpha old needleold"), class: contentClassText, version: 1, fileSize: 9, fileModNano: 1},
		{path: bPath, frn: 20, text: []byte("beta needlebeta"), class: contentClassText, version: 1, fileSize: bSize, fileModNano: bMod},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = journal
	base.CheckpointUSN = 100
	base.Policy = policy
	stampContentTestScope(dir, base)
	if err := contentSaveFile(gsx, base); err != nil {
		t.Fatal(err)
	}

	f.jwrap = usnJournalDataV0{UsnJournalID: journal, FirstUsn: 150, LowestValidUsn: 150, NextUsn: 300}
	stubContentCatchUpJournal(t, f.jwrap)
	calls := new(int)
	stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
		*calls++
		return startUSN, nil, nil
	})
	f.calls = calls
	return f
}

func (f *wrapReconFixture) resolve(t *testing.T) func(uint64) (string, bool) {
	t.Helper()
	return func(frn uint64) (string, bool) {
		switch frn {
		case 10:
			return f.aPath, true
		case 20:
			return f.bPath, true
		case 30:
			return f.cPath, true
		}
		return "", false
	}
}

// A wrap while the service is stopped must recover without a rebuild: the
// modified file re-extracts, the deleted file tombstones, the new file
// indexes, the volume stays usable, and the journal is never re-read.
func TestContentWrapReconcileRecoversChanges(t *testing.T) {
	f := newWrapReconFixture(t)
	builds := stubContentBuildSchedule(t)

	f.s.attachContentForVolume(f.vol)

	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after wrap reconcile = %q; want ready (still serving)", got)
	}
	if !f.vol.content.usableForQuery() {
		t.Fatal("a wrap-reconciled base must stay usable")
	}
	if *builds != 0 {
		t.Fatalf("wrap reconcile scheduled %d builds; want 0", *builds)
	}
	if *f.calls != 0 {
		t.Fatalf("wrap reconcile re-read the journal %d times; want 0", *f.calls)
	}
	if n := f.vol.contentCoord.processQueue(f.resolve(t)); n != 2 {
		t.Fatalf("wrap drain extracted %d docs; want 2 (changed a.txt + new c.txt)", n)
	}
	if hits, err := contentServiceSearch(t, f.vol, "content:needlenew", false); err != nil || len(hits) != 1 {
		t.Fatalf("reconciled new content not findable: %v %v", hits, err)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:needleold", false); len(hits) != 0 {
		t.Fatalf("stale pre-wrap content still findable: %v", hits)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:needlegamma", false); len(hits) != 1 {
		t.Fatalf("wrap-window created file not findable: %v", hits)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:needlebeta", false); len(hits) != 0 {
		t.Fatalf("wrap-window deleted file still findable: %v", hits)
	}

	// The fold durably claims the reconciled volume checkpoint (past the
	// lost journal range) instead of the base's stale one.
	f.s.runContentFold(f.vol)
	reloaded, err := contentLoadFile(f.gsx)
	releaseContentIndexOnCleanup(t, reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CheckpointUSN != 200 {
		t.Fatalf("fold claimed checkpoint %d; want 200 (the reconciled volume checkpoint)", reloaded.CheckpointUSN)
	}
}

// Replay without a filename rebuild leaves the wrap window's changes in the
// record overlay only. The reconcile must merge the overlay over the base
// records: an overlay-modified file re-extracts and an overlay-deleted one
// tombstones.
func TestContentWrapReconcileMergesOverlay(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.txt")
	bPath := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(aPath, []byte("alpha new needlenew"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, []byte("beta needlebeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The index is at base time; the wrap window's changes arrive as replay.
	const journal = uint64(7)
	vol, gsx := contentBuildTestVolume(t, dir, journal, 100, []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: 9, ModUnix: 1},
		{FRN: 20, ParentFRN: 20, Parent: -1, Name: "b.txt", Size: 14, ModUnix: 2},
	})
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	base, err := assembleContentIndex([]contentBuildDoc{
		{path: aPath, frn: 10, text: []byte("alpha old needleold"), class: contentClassText, version: 1, fileSize: 9, fileModNano: 1},
		{path: bPath, frn: 20, text: []byte("beta needlebeta"), class: contentClassText, version: 1, fileSize: 14, fileModNano: 2},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = journal
	base.CheckpointUSN = 100
	stampContentTestScope(dir, base)
	if err := contentSaveFile(gsx, base); err != nil {
		t.Fatal(err)
	}
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 150, LowestValidUsn: 150, NextUsn: 300})
	calls := new(int)
	stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
		*calls++
		return startUSN, nil, nil
	})

	// Exactly what startup replay does before content attaches: the overlay
	// (and checkpoint) advance while content observation is a no-op.
	vol.applyUSNChanges([]usnChange{
		{FRN: 10, ParentFRN: 10, Name: "a.txt", USN: 150, Reason: usnReasonDataOverwrite | usnReasonClose},
		{FRN: 20, ParentFRN: 20, Name: "b.txt", USN: 151, Reason: usnReasonFileDelete},
	})
	if vol.checkpoint != 151 {
		t.Fatalf("volume checkpoint after replay = %d; want 151", vol.checkpoint)
	}

	s.attachContentForVolume(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after overlay-merge reconcile = %q; want ready", got)
	}
	if *calls != 0 {
		t.Fatalf("overlay-merge reconcile re-read the journal %d times; want 0", *calls)
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
	if n := vol.contentCoord.processQueue(resolve); n != 1 {
		t.Fatalf("overlay-merge drain extracted %d docs; want 1 (modified a.txt)", n)
	}
	if hits, err := contentServiceSearch(t, vol, "content:needlenew", false); err != nil || len(hits) != 1 {
		t.Fatalf("overlay-modified content not findable: %v %v", hits, err)
	}
	if hits, _ := contentServiceSearch(t, vol, "content:needlebeta", false); len(hits) != 0 {
		t.Fatalf("overlay-deleted content still findable: %v", hits)
	}
}

// A journal-ID change is a new generation, not a wrap: FRN identity is
// unsafe, so attach must stay stale even when the range also looks wrapped.
func TestContentWrapReconcileJournalIDChangeStaysStale(t *testing.T) {
	f := newWrapReconFixture(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 999, FirstUsn: 150, LowestValidUsn: 150, NextUsn: 300})

	f.s.attachContentForVolume(f.vol)

	if got := f.vol.content.stateOf(); got != contentStateStale {
		t.Fatalf("state after journal-ID change = %q; want stale (new generation, FRNs unusable)", got)
	}
	if f.vol.content.usableForQuery() {
		t.Fatal("a new-generation base must not be usable")
	}
	if n := f.vol.contentCoord.queued(); n != 0 {
		t.Fatalf("stale generation queued %d FRNs; want 0", n)
	}
}

// A sidecar without file fingerprints cannot diff: the wrap must degrade
// the volume and schedule a budgeted rebuild, never guess.
func TestContentWrapReconcilePreV6Rebuilds(t *testing.T) {
	f := newWrapReconFixture(t)
	// Strip the fingerprints: zero size+mtime on every doc.
	raw, err := contentLoadFile(f.gsx)
	releaseContentIndexOnCleanup(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw.Docs {
		raw.Docs[i].FileSize = 0
		raw.Docs[i].FileModNano = 0
	}
	if err := contentSaveFile(f.gsx, raw); err != nil {
		t.Fatal(err)
	}
	builds := stubContentBuildSchedule(t)

	f.s.attachContentForVolume(f.vol)

	if *builds == 0 {
		t.Fatal("an unfingerprinted wrap base must schedule a rebuild")
	}
	if got := f.vol.content.stateOf(); got == contentStateReady && f.vol.content.usableForQuery() {
		t.Fatal("an unfingerprinted wrap base must not serve as ready")
	}
}

// A reset that lands mid-catch-up (the read fails after validation passed)
// must reconcile instead of staling: the live index is complete, only the
// journal cursor is lost.
func TestContentWrapReconcileMidLoopWrap(t *testing.T) {
	f := newWrapReconFixture(t)
	// Validation still sees a contiguous journal; the read then reports the
	// reset that happened concurrently.
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 300})
	reads := 0
	stubContentCatchUpRead(t, func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
		reads++
		if reads == 1 {
			// By the time the read runs, the journal has wrapped.
			stubContentCatchUpJournal(t, f.jwrap)
			return 0, nil, errors.New("journal reset mid-read")
		}
		return startUSN, nil, nil
	})

	f.s.attachContentForVolume(f.vol)

	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after mid-loop wrap = %q; want ready (reconciled)", got)
	}
	if !f.vol.content.usableForQuery() {
		t.Fatal("a mid-loop-wrap base must stay usable")
	}
	if n := f.vol.contentCoord.processQueue(f.resolve(t)); n != 2 {
		t.Fatalf("mid-loop-wrap drain extracted %d docs; want 2", n)
	}
	if hits, err := contentServiceSearch(t, f.vol, "content:needlenew", false); err != nil || len(hits) != 1 {
		t.Fatalf("reconciled content not findable after mid-loop wrap: %v %v", hits, err)
	}
}
