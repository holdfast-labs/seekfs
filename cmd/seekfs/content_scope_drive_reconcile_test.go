package main

import (
	"os"
	"path/filepath"
	"testing"
)

// driveReconFixture is a USN volume whose index lives under a real temp repo,
// but whose volume string is the drive itself, so the auto scope resolves the
// whole-drive root (the pre-merge selection had only repos + known folders).
// It holds two repo text files and one text file under a sibling directory
// that no worktree or known folder covers: exactly the files the whole-volume
// auto root admits.
type driveReconFixture struct {
	dir        string
	repoA      string
	outsideDir string
	outside    string
	vol        *serviceVolumeIndex
	gsx        string
	s          *goSearchService
}

func newDriveReconFixture(t *testing.T) *driveReconFixture {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	t.Setenv("SEEKFS_CONTENT_SCOPE", "auto")
	t.Setenv("SEEKFS_CONTENT_ROOTS", "")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	// The live E: volume sat at its free-space reserve when the whole-volume
	// auto root was added, so the reconcile's reserve allowance was tiny. Pin
	// the same condition: the base below was built WITHOUT reserve truncation,
	// so the reconcile must still admit the newly selected outside-repo file.
	stubContentNoHeadroom(t)

	dir := t.TempDir()
	repoA := filepath.Join(dir, "repo")
	outsideDir := filepath.Join(dir, "wisp-memory")
	if err := os.MkdirAll(filepath.Join(repoA, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoA, ".git", "HEAD"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aPath := filepath.Join(repoA, "a.txt")
	bPath := filepath.Join(repoA, "b.txt")
	outside := filepath.Join(outsideDir, "note.txt")
	for _, p := range []string{aPath, bPath} {
		if err := os.WriteFile(p, []byte("repo needlerepo"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outside, []byte("outside needleoutside"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := uint32(os.ModeDir)
	records := []CompactRecord{
		{FRN: 1, Parent: -1, ParentFRN: 1, Mode: d, Name: "."},
		{FRN: 2, Parent: 0, ParentFRN: 1, Mode: d, Name: "repo"},
		{FRN: 3, Parent: 1, ParentFRN: 2, Mode: d, Name: ".git"},
		{FRN: 4, Parent: 1, ParentFRN: 2, Name: "a.txt", Size: 15},
		{FRN: 5, Parent: 1, ParentFRN: 2, Name: "b.txt", Size: 15},
		{FRN: 6, Parent: 0, ParentFRN: 1, Mode: d, Name: "wisp-memory"},
		{FRN: 7, Parent: 5, ParentFRN: 6, Name: "note.txt", Size: 20},
	}
	// Hermetic disk: a 500 GiB volume with the 10 GiB reserve plus 1 GiB free,
	// so the reserve allowance is ~1 GiB (minus the shard scratch slack), not
	// whatever the host disk happens to hold.
	oldFree, oldTotal := contentFreeBytesFn, contentTotalBytesFn
	t.Cleanup(func() { contentFreeBytesFn, contentTotalBytesFn = oldFree, oldTotal })
	contentTotalBytesFn = func(string) (uint64, error) { return 500 << 30, nil }
	contentFreeBytesFn = func(string) (uint64, error) { return contentScopeReserveMaxBytes + 1<<30, nil }
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 100})
	stubContentCatchUpRead(t, func(string, uint64, int64, []byte) (int64, []usnChange, error) {
		return 0, nil, nil
	})
	idx := &Index{Source: "usn", Volume: dir, Compact: true, JournalID: 7, Checkpoint: 100}
	idx.Records = append([]CompactRecord(nil), records...)
	contentIndexFRNs(idx)
	dbPath := filepath.Join(t.TempDir(), "seekfs_c.gsi")
	vol := newServiceVolumeIndex(dbPath, idx)
	releaseVolumeContentOnCleanup(t, vol)
	// The service volume names the drive, so auto scope adds the volume root;
	// the index paths are the temp repo under that drive.
	vol.volume = filepath.VolumeName(dir) + string(filepath.Separator)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	return &driveReconFixture{dir: dir, repoA: repoA, outsideDir: outsideDir, outside: outside, vol: vol, gsx: contentIndexPathForDB(dbPath), s: s}
}

// writeDriveBase assembles a sidecar with the given docs and stamps the OLD
// (pre-merge) scope fingerprint: the current resolution with the whole-volume
// root stripped, exactly what the previous binary hashed.
func (f *driveReconFixture) writeDriveBase(t *testing.T, docs []contentBuildDoc) {
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
	old := resolved
	old.DriveRoot = ""
	roots := old.Roots[:0]
	for _, r := range resolved.Roots {
		if r != contentNormPath(f.vol.volume) {
			roots = append(roots, r)
		}
	}
	old.Roots = roots
	old.buildScopePrefixes()
	base.ScopeHash = old.fingerprint()
	if resolved.fingerprint() == base.ScopeHash {
		t.Fatal("precondition: stripping the drive root did not move the scope fingerprint")
	}
	if err := contentSaveFile(f.gsx, base); err != nil {
		t.Fatal(err)
	}
}

func (f *driveReconFixture) repoDocs(t *testing.T) []contentBuildDoc {
	t.Helper()
	var docs []contentBuildDoc
	for _, spec := range []struct {
		frn  uint64
		path string
	}{{4, filepath.Join(f.repoA, "a.txt")}, {5, filepath.Join(f.repoA, "b.txt")}} {
		st, err := os.Stat(spec.path)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, contentBuildDoc{path: spec.path, frn: spec.frn, text: []byte("repo needlerepo"), class: contentClassText, version: 1, fileSize: st.Size(), fileModNano: st.ModTime().UnixNano()})
	}
	return docs
}

// A sidecar built under the repo-only auto scope (the pre-merge rules) must,
// after the scope gains the whole-volume root, reconcile the newly admitted
// non-repo file at attach: the base stays ready, no rebuild runs, the outside
// FRN is enqueued, and after the drain its content is findable.
func TestContentAttachReconcilesDriveRootScope(t *testing.T) {
	f := newDriveReconFixture(t)
	f.writeDriveBase(t, f.repoDocs(t))
	builds := stubContentBuildSchedule(t)

	f.s.attachContentForVolume(f.vol)

	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after drive-root scope expansion = %q; want ready (reconciled, not stale)", got)
	}
	if *builds != 0 {
		t.Fatalf("drive-root expansion scheduled %d builds; want 0", *builds)
	}
	if n := f.vol.contentCoord.queued(); n != 1 {
		t.Fatalf("expansion queued %d FRNs; want 1 (the outside-repo file)", n)
	}
	n := f.vol.contentCoord.processQueue(func(frn uint64) (string, bool) {
		if frn == 7 {
			return f.outside, true
		}
		return "", false
	})
	if n != 1 {
		t.Fatalf("expansion drain extracted %d docs; want 1 (the outside-repo file)", n)
	}
	if hits, err := contentServiceSearch(t, f.vol, "content:needleoutside", false); err != nil || len(hits) != 1 {
		t.Fatalf("outside-repo content not findable after drain: %v %v", hits, err)
	}
}
