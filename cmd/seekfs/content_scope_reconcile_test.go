package main

import (
	"os"
	"path/filepath"
	"testing"
)

// scopeReconFixture is a flat single-repo USN volume (the volume directory
// itself is the git worktree) with three text files. Tests stamp a sidecar
// under one .gitignore revision and attach under another, so attach must
// reconcile the selection difference instead of discarding the base. Three
// keep files keep honest diffs (one file) under the rebuild fraction.
type scopeReconFixture struct {
	dir        string
	keepPaths  []string
	keepFRNs   []uint64
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
	var keepPaths []string
	var keepFRNs []uint64
	var records []CompactRecord
	stat := func(path string) (int64, int64) {
		t.Helper()
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.Size(), st.ModTime().UnixNano()
	}
	for i, frn := range []uint64{10, 11, 12} {
		path := filepath.Join(dir, "keep"+string(rune('1'+i))+".txt")
		if err := os.WriteFile(path, []byte("keep needlekeep"), 0o644); err != nil {
			t.Fatal(err)
		}
		size, mod := stat(path)
		keepPaths = append(keepPaths, path)
		keepFRNs = append(keepFRNs, frn)
		records = append(records, CompactRecord{FRN: frn, ParentFRN: frn, Parent: -1, Name: "keep" + string(rune('1'+i)) + ".txt", Size: size, ModUnix: mod})
	}
	dropPath := filepath.Join(dir, "drop.txt")
	if err := os.WriteFile(dropPath, []byte("drop needledrop"), 0o644); err != nil {
		t.Fatal(err)
	}
	dropSize, dropMod := stat(dropPath)
	records = append(records, CompactRecord{FRN: 20, ParentFRN: 20, Parent: -1, Name: "drop.txt", Size: dropSize, ModUnix: dropMod})
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
	ignoreSize, ignoreMod := stat(ignorePath)
	records = append(records,
		CompactRecord{FRN: 30, ParentFRN: 30, Parent: -1, Name: ".gitignore", Size: ignoreSize, ModUnix: ignoreMod},
		CompactRecord{FRN: 40, ParentFRN: 40, Parent: -1, Name: ".git", Mode: uint32(os.ModeDir)},
	)

	const journal = uint64(7)
	const cp = int64(100)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), records)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	return &scopeReconFixture{dir: dir, keepPaths: keepPaths, keepFRNs: keepFRNs, dropPath: dropPath, ignorePath: ignorePath, vol: vol, gsx: gsx, s: s}
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
	// Only the text files resolve. The .gitignore's own FRN (30) stays
	// unresolvable on purpose: the service drain would decline it (no text
	// extractor claims it), while the direct processQueue used here
	// tombstones it instead — a phantom the fold carries harmlessly.
	return func(frn uint64) (string, bool) {
		for i, k := range f.keepFRNs {
			if frn == k {
				return f.keepPaths[i], true
			}
		}
		if frn == 20 {
			return f.dropPath, true
		}
		return "", false
	}
}

// keepDocs builds base docs for the keep files with matching prints.
func (f *scopeReconFixture) keepDocs(t *testing.T) []contentBuildDoc {
	t.Helper()
	var docs []contentBuildDoc
	for i, k := range f.keepFRNs {
		size, mod := f.filePrint(t, f.keepPaths[i])
		docs = append(docs, contentBuildDoc{path: f.keepPaths[i], frn: k, text: []byte("keep needlekeep"), class: contentClassText, version: 1, fileSize: size, fileModNano: mod})
	}
	return docs
}

// A .gitignore edit that narrows the scope must tombstone the newly ignored
// file's docs at attach: the base stays ready and usable, no rebuild runs,
// and the fold persists the dropped doc plus the new fingerprint.
func TestContentAttachReconcilesScopeContraction(t *testing.T) {
	f := newScopeReconFixture(t, "")
	dropSize, dropMod := f.filePrint(t, f.dropPath)
	docs := f.keepDocs(t)
	docs = append(docs, contentBuildDoc{path: f.dropPath, frn: 20, text: []byte("drop needledrop"), class: contentClassText, version: 1, fileSize: dropSize, fileModNano: dropMod})
	f.writeScopeReconBase(t, docs)
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
	// Nothing enqueues: the ignored file drops via tombstone, and the
	// .gitignore itself is not selection (rules come from disk).
	if n := f.vol.contentCoord.queued(); n != 0 {
		t.Fatalf("contraction queued %d FRNs; want 0", n)
	}
	if hits, err := contentServiceSearch(t, f.vol, "content:needlekeep", false); err != nil || len(hits) != 3 {
		t.Fatalf("kept content hits = %d; want 3: %v %v", len(hits), hits, err)
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
	f.writeScopeReconBase(t, f.keepDocs(t))
	builds := stubContentBuildSchedule(t)

	f.rewriteIgnore(t, "")
	f.s.attachContentForVolume(f.vol)

	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after scope expansion = %q; want ready", got)
	}
	if *builds != 0 {
		t.Fatalf("scope expansion scheduled %d builds; want 0", *builds)
	}
	if n := f.vol.contentCoord.queued(); n != 1 {
		t.Fatalf("expansion queued %d FRNs; want 1 (the newly admitted file)", n)
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
	dropSize, dropMod := f.filePrint(t, f.dropPath)
	docs := f.keepDocs(t)
	docs = append(docs, contentBuildDoc{path: f.dropPath, frn: 20, text: []byte("drop needledrop"), class: contentClassText, version: 1, fileSize: dropSize, fileModNano: dropMod})
	f.writeScopeReconBase(t, docs)
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
	f.writeScopeReconBase(t, f.keepDocs(t))
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
