package main

import (
	"os"
	"path/filepath"
	"testing"
)

// scopeReconFixture is a flat single-repo USN volume (the volume directory
// itself is the git worktree) with two text files. Tests stamp a sidecar
// under one .gitignore revision and attach under another, so attach must
// reconcile the selection difference instead of discarding the base.
type scopeReconFixture struct {
	dir        string
	keepPath   string
	dropPath   string
	ignorePath string
	vol        *serviceVolumeIndex
	gsx        string
	s          *goSearchService
}

func newScopeReconFixture(t *testing.T, ignoreText string) *scopeReconFixture {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	keepPath := filepath.Join(dir, "keep.txt")
	dropPath := filepath.Join(dir, "drop.txt")
	if err := os.WriteFile(keepPath, []byte("keep needlekeep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dropPath, []byte("drop needledrop"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ignorePath := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(ignorePath, []byte(ignoreText), 0o644); err != nil {
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
	keepSize, keepMod := stat(keepPath)
	dropSize, dropMod := stat(dropPath)
	ignoreSize, ignoreMod := stat(ignorePath)

	const journal = uint64(7)
	const cp = int64(100)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "keep.txt", Size: keepSize, ModUnix: keepMod},
		{FRN: 20, ParentFRN: 20, Parent: -1, Name: "drop.txt", Size: dropSize, ModUnix: dropMod},
		{FRN: 30, ParentFRN: 30, Parent: -1, Name: ".gitignore", Size: ignoreSize, ModUnix: ignoreMod},
		{FRN: 40, ParentFRN: 40, Parent: -1, Name: ".git", Mode: uint32(os.ModeDir)},
	})
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	return &scopeReconFixture{dir: dir, keepPath: keepPath, dropPath: dropPath, ignorePath: ignorePath, vol: vol, gsx: gsx, s: s}
}

// writeScopeReconBase assembles a sidecar with the given file contents and
// stamps the scope exactly as attach would resolve it right now (same env,
// same index, same ignore files on disk), so any later scope edit is a
// genuine drift the attach must reconcile.
func (f *scopeReconFixture) writeScopeReconBase(t *testing.T, docs []contentBuildDoc) {
	t.Helper()
	base, err := assembleContentIndex(docs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = 7
	base.CheckpointUSN = 100
	resolved, ok := f.s.resolvedContentScope(f.vol)
	if !ok {
		t.Fatal("scope unresolvable for fixture volume")
	}
	base.ScopeHash = resolved.fingerprint()
	if err := contentSaveFile(f.gsx, base); err != nil {
		t.Fatal(err)
	}
}

func (f *scopeReconFixture) filePrint(t *testing.T, path string) (int64, int64) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size(), st.ModTime().UnixNano()
}

// rewriteIgnore replaces the .gitignore text and refreshes its index record
// so the record stat matches the file on disk (no spurious print mismatch).
func (f *scopeReconFixture) rewriteIgnore(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(f.ignorePath, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	size, mod := f.filePrint(t, f.ignorePath)
	for i := range f.vol.index.Records {
		if f.vol.index.Records[i].FRN == 30 {
			f.vol.index.Records[i].Size = size
			f.vol.index.Records[i].ModUnix = mod
		}
	}
}

func (f *scopeReconFixture) resolveForDrain(t *testing.T) func(uint64) (string, bool) {
	t.Helper()
	// Only the two text files resolve. The .gitignore's own FRN (30) stays
	// unresolvable on purpose: the service drain would decline it (no text
	// extractor claims it), while the direct processQueue used here
	// tombstones it instead — a phantom the fold carries harmlessly.
	return func(frn uint64) (string, bool) {
		switch frn {
		case 10:
			return f.keepPath, true
		case 20:
			return f.dropPath, true
		}
		return "", false
	}
}

// A .gitignore edit that narrows the scope must tombstone the newly ignored
// file's docs at attach: the base stays ready and usable, no rebuild runs,
// and the fold persists the dropped doc plus the new fingerprint.
func TestContentAttachReconcilesScopeContraction(t *testing.T) {
	f := newScopeReconFixture(t, "")
	keepSize, keepMod := f.filePrint(t, f.keepPath)
	dropSize, dropMod := f.filePrint(t, f.dropPath)
	f.writeScopeReconBase(t, []contentBuildDoc{
		{path: f.keepPath, frn: 10, text: []byte("keep needlekeep"), class: contentClassText, version: 1, fileSize: keepSize, fileModNano: keepMod},
		{path: f.dropPath, frn: 20, text: []byte("drop needledrop"), class: contentClassText, version: 1, fileSize: dropSize, fileModNano: dropMod},
	})
	builds := stubContentBuildSchedule(t)

	// The ignore edit lands while the service is stopped.
	f.rewriteIgnore(t, "drop.txt\n")
	f.s.attachContentForVolume(f.vol)

	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after scope contraction = %q; want ready (reconciled, not stale)", got)
	}
	if !f.vol.content.usableForQuery() {
		t.Fatal("a scope-reconciled base must stay usable")
	}
	if *builds != 0 {
		t.Fatalf("scope contraction scheduled %d builds; want 0", *builds)
	}
	if n := f.vol.contentCoord.queued(); n != 1 {
		t.Fatalf("contraction queued %d FRNs; want 1 (the .gitignore itself, which the drain declines; the ignored file drops, nothing new appears)", n)
	}
	if n := f.vol.contentCoord.processQueue(f.resolveForDrain(t)); n != 0 {
		t.Fatalf("contraction drain extracted %d docs; want 0", n)
	}
	if hits, err := contentServiceSearch(t, f.vol, "content:needlekeep", false); err != nil || len(hits) != 1 {
		t.Fatalf("kept content not findable after reconcile: %v %v", hits, err)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:needledrop", false); len(hits) != 0 {
		t.Fatalf("newly ignored content still findable after reconcile: %v", hits)
	}

	f.s.runContentFold(f.vol)
	reloaded, err := contentLoadFile(f.gsx)
	releaseContentIndexOnCleanup(t, reloaded)
	if err != nil {
		t.Fatal(err)
	}
	for i := range reloaded.Docs {
		if reloaded.Docs[i].FRN == 20 {
			t.Fatal("fold did not persist the scope-drop: the ignored file's doc is still in the base")
		}
	}
	resolved, ok := f.s.resolvedContentScope(f.vol)
	if !ok {
		t.Fatal("scope unresolvable after reconcile")
	}
	if reloaded.ScopeHash != resolved.fingerprint() {
		t.Fatal("fold did not persist the reconciled scope fingerprint")
	}
}

// A .gitignore edit that widens the scope must enqueue the newly admitted
// file at attach; after the drain its content is findable.
func TestContentAttachReconcilesScopeExpansion(t *testing.T) {
	f := newScopeReconFixture(t, "drop.txt\n")
	keepSize, keepMod := f.filePrint(t, f.keepPath)
	f.writeScopeReconBase(t, []contentBuildDoc{
		{path: f.keepPath, frn: 10, text: []byte("keep needlekeep"), class: contentClassText, version: 1, fileSize: keepSize, fileModNano: keepMod},
	})
	builds := stubContentBuildSchedule(t)

	f.rewriteIgnore(t, "")
	f.s.attachContentForVolume(f.vol)

	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after scope expansion = %q; want ready", got)
	}
	if *builds != 0 {
		t.Fatalf("scope expansion scheduled %d builds; want 0", *builds)
	}
	if n := f.vol.contentCoord.queued(); n != 2 {
		t.Fatalf("expansion queued %d FRNs; want 2 (the newly admitted file plus the .gitignore itself)", n)
	}
	if n := f.vol.contentCoord.processQueue(f.resolveForDrain(t)); n != 1 {
		t.Fatalf("expansion drain extracted %d docs; want 1 (the newly admitted file)", n)
	}
	if hits, err := contentServiceSearch(t, f.vol, "content:needledrop", false); err != nil || len(hits) != 1 {
		t.Fatalf("newly admitted content not findable after drain: %v %v", hits, err)
	}
}

// A scope diff past the rebuild fraction must NOT reconcile: the base is
// too far gone to trust, so attach stays stale and a rebuild is scheduled.
func TestContentAttachRebuildsOnScopeFlood(t *testing.T) {
	f := newScopeReconFixture(t, "")
	keepSize, keepMod := f.filePrint(t, f.keepPath)
	dropSize, dropMod := f.filePrint(t, f.dropPath)
	f.writeScopeReconBase(t, []contentBuildDoc{
		{path: f.keepPath, frn: 10, text: []byte("keep needlekeep"), class: contentClassText, version: 1, fileSize: keepSize, fileModNano: keepMod},
		{path: f.dropPath, frn: 20, text: []byte("drop needledrop"), class: contentClassText, version: 1, fileSize: dropSize, fileModNano: dropMod},
	})
	builds := stubContentBuildSchedule(t)

	// Shrink the rebuild fraction to zero: any nonzero diff rebuilds.
	oldFrac := contentScopeReconcileRebuildFrac
	contentScopeReconcileRebuildFrac = 0
	defer func() { contentScopeReconcileRebuildFrac = oldFrac }()

	f.rewriteIgnore(t, "drop.txt\n")
	f.s.ensureContentBuild(f.vol)

	if got := f.vol.content.stateOf(); got == contentStateReady && f.vol.content.usableForQuery() {
		t.Fatal("a past-threshold scope diff must not attach as ready")
	}
	if *builds == 0 {
		t.Fatal("a past-threshold scope diff must schedule a rebuild")
	}
}

// After a clean attach, a scope edit must surface through the periodic
// drift check: the differing selection reconciles without a rebuild.
func TestContentScopeDriftCheckReconciles(t *testing.T) {
	f := newScopeReconFixture(t, "drop.txt\n")
	keepSize, keepMod := f.filePrint(t, f.keepPath)
	f.writeScopeReconBase(t, []contentBuildDoc{
		{path: f.keepPath, frn: 10, text: []byte("keep needlekeep"), class: contentClassText, version: 1, fileSize: keepSize, fileModNano: keepMod},
	})
	f.s.attachContentForVolume(f.vol)
	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition state = %q; want ready", got)
	}
	if f.vol.content.scopePendingNow() {
		t.Fatal("precondition: scope reconcile pending before any drift")
	}

	oldPeriod := contentScopeDriftCheckPeriod
	contentScopeDriftCheckPeriod = 0
	defer func() { contentScopeDriftCheckPeriod = oldPeriod }()

	f.rewriteIgnore(t, "")
	f.s.contentScopeDriftCheck(f.vol, true)

	if !f.vol.content.scopePendingNow() {
		t.Fatal("drift check did not reconcile the widened scope")
	}
	if n := f.vol.contentCoord.queued(); n == 0 && f.vol.content.deltaView().len() == 0 {
		t.Fatal("drift reconcile enqueued nothing for the newly admitted file")
	}
	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after drift reconcile = %q; want ready", got)
	}
	if !f.vol.content.usableForQuery() {
		t.Fatal("content must stay usable through a drift reconcile")
	}
}

// A service-owned build must report the scanning phase (with the record
// total) while selecting files and move to extracting before assembly, so
// the multi-minute scan stops looking stuck at 0%.
func TestContentBuildPhaseScanningObserved(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("phase needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 50})
	vol, _ := contentBuildTestVolume(t, dir, 7, 50, []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 12},
	})

	var sawScanning, sawExtracting bool
	contentBuildSnapshotBatchHook = func(s *goSearchService, v *serviceVolumeIndex) {
		h := v.content.healthSnapshot(0)
		if v.content.stateOf() == contentStateIndexing && h.BuildPhase == contentBuildPhaseScanning && h.BuildTotal == 1 {
			sawScanning = true
		}
	}
	contentBuildIndexingHook = func(v *contentVolumeState) {
		if h := v.healthSnapshot(0); h.BuildPhase == contentBuildPhaseExtracting {
			sawExtracting = true
		}
	}

	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after build = %q; want ready", got)
	}
	if !sawScanning {
		t.Fatal("build never reported the scanning phase with the record total during selection")
	}
	if !sawExtracting {
		t.Fatal("build never moved to the extracting phase before assembly")
	}
}
