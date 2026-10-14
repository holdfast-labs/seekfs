package main

// Reconcile + service-level tests for the inclusion policy (§6z): linked
// worktrees appearing/disappearing or changing policy reconcile through
// the §6p path (no rebuild), duplicates diverging re-resolve at fold, and
// per-file caps truncate by worktree membership.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inclusionWorktreeFixture is an auto-scope USN volume with a main checkout
// (a real `.git` directory) and a linked worktree (a real `.git` FILE
// pointing at the main's worktrees dir), each holding one text file. The
// real on-disk markers drive the real worktree probe: no stubs.
type inclusionWorktreeFixture struct {
	dir        string
	mainPaths  []string
	linkedPath string
	vol        *serviceVolumeIndex
	gsx        string
	s          *goSearchService
}

func newInclusionWorktreeFixture(t *testing.T) *inclusionWorktreeFixture {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	t.Setenv("SEEKFS_CONTENT_SCOPE", "auto")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	mainDir := filepath.Join(dir, "main")
	if err := os.MkdirAll(filepath.Join(mainDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Three keep files: a one-file policy drift is past the rebuild
	// fraction on a tiny base, like the scope fixture's three keeps.
	var mainPaths []string
	var records []CompactRecord
	stat := func(path string) (int64, int64) {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.Size(), st.ModTime().UnixNano()
	}
	records = append(records,
		CompactRecord{FRN: 1, Parent: -1, ParentFRN: 1, Mode: uint32(os.ModeDir), Name: "main"},
		CompactRecord{FRN: 2, Parent: 0, ParentFRN: 1, Mode: uint32(os.ModeDir), Name: ".git"},
	)
	for i, frn := range []uint64{10, 11, 12} {
		name := "main" + string(rune('1'+i)) + ".txt"
		p := filepath.Join(mainDir, name)
		if err := os.WriteFile(p, []byte("main needlemain"), 0o644); err != nil {
			t.Fatal(err)
		}
		mainPaths = append(mainPaths, p)
		size, mod := stat(p)
		records = append(records, CompactRecord{FRN: frn, Parent: 0, ParentFRN: 1, Name: name, Size: size, ModUnix: mod})
	}
	linkedDir := filepath.Join(dir, "linked")
	if err := os.MkdirAll(linkedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linkedDir, ".git"),
		[]byte("gitdir: "+filepath.Join(mainDir, ".git", "worktrees", "linked")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkedPath := filepath.Join(linkedDir, "linked.txt")
	if err := os.WriteFile(linkedPath, []byte("linked needlelinked"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkedSize, linkedMod := stat(linkedPath)
	gitMod := linkedMod
	linkedDirID := int32(len(records))
	records = append(records,
		CompactRecord{FRN: 3, Parent: -1, ParentFRN: 3, Mode: uint32(os.ModeDir), Name: "linked"},
		CompactRecord{FRN: 4, Parent: linkedDirID, ParentFRN: 3, Name: ".git", Size: 60, ModUnix: gitMod},
		CompactRecord{FRN: 20, Parent: linkedDirID, ParentFRN: 3, Name: "linked.txt", Size: linkedSize, ModUnix: linkedMod},
	)
	const journal = uint64(7)
	const cp = int64(100)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	idx := &Index{Source: "usn", Volume: dir, Compact: true, JournalID: journal, Checkpoint: cp}
	idx.Records = append([]CompactRecord(nil), records...)
	contentIndexFRNs(idx)
	dbPath := filepath.Join(t.TempDir(), "seekfs_c.gsi")
	vol := newServiceVolumeIndex(dbPath, idx)
	releaseVolumeContentOnCleanup(t, vol)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	gsx := contentIndexPathForDB(dbPath)
	return &inclusionWorktreeFixture{dir: dir, mainPaths: mainPaths, linkedPath: linkedPath, vol: vol, gsx: gsx, s: s}
}

// stampWorktreeBase assembles a sidecar with the named docs (main, linked
// or both) and stamps the scope resolved under the given worktrees mode,
// so attach under the other mode is a genuine policy drift to reconcile.
// The doc set must match the mode's selection: both under all, main only
// under main.
func (f *inclusionWorktreeFixture) stampWorktreeBase(t *testing.T, worktrees string, which ...string) {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_WORKTREES", worktrees)
	want := make(map[string]bool)
	for _, w := range which {
		want[w] = true
	}
	var docs []contentBuildDoc
	addDoc := func(name, path, text string, frn uint64) {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, contentBuildDoc{path: path, frn: frn, text: []byte(text),
			class: contentClassText, version: 1, fileSize: st.Size(), fileModNano: st.ModTime().UnixNano()})
	}
	if want["main"] {
		for i, p := range f.mainPaths {
			addDoc("main", p, "main needlemain", uint64(10+i))
		}
	}
	if want["linked"] {
		addDoc("linked", f.linkedPath, "linked needlelinked", 20)
	}
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

// Switching worktrees=all to the main-only default must tombstone the
// linked checkout's docs at attach: ready and usable throughout, no
// rebuild, and the fold persists the drop plus the new fingerprint.
func TestContentAttachReconcilesWorktreePolicyToMain(t *testing.T) {
	f := newInclusionWorktreeFixture(t)
	f.stampWorktreeBase(t, "all", "main", "linked")
	t.Setenv("SEEKFS_CONTENT_WORKTREES", "main")
	builds := stubContentBuildSchedule(t)

	f.s.attachContentForVolume(f.vol)
	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after worktree contraction = %q; want ready", got)
	}
	if !f.vol.content.usableForQuery() {
		t.Fatal("a worktree-reconciled base must stay usable")
	}
	if *builds != 0 {
		t.Fatalf("worktree contraction scheduled %d builds; want 0", *builds)
	}
	if hits, err := contentServiceSearch(t, f.vol, "content:needlemain", false); err != nil || len(hits) != 3 {
		t.Fatalf("main hits = %d; want 3: %v %v", len(hits), hits, err)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:needlelinked", false); len(hits) != 0 {
		t.Fatalf("linked checkout still findable under main-only: %v", hits)
	}
	f.s.runContentFold(f.vol)
	reloaded, err := contentLoadFile(f.gsx)
	releaseContentIndexOnCleanup(t, reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Docs) != 3 {
		t.Fatalf("fold kept %d docs; want the 3 main-checkout docs", len(reloaded.Docs))
	}
	for i := range reloaded.Docs {
		if reloaded.Docs[i].FRN == 20 {
			t.Fatal("fold kept the linked checkout's doc")
		}
	}
	resolved, ok := f.s.resolvedContentScope(f.vol)
	if !ok {
		t.Fatal("scope unresolvable after reconcile")
	}
	if reloaded.ScopeHash != resolved.fingerprint() {
		t.Fatal("fold did not persist the reconciled (main-only) fingerprint")
	}
}

// Switching the main-only default to worktrees=all must enqueue the linked
// checkout's files at attach; after the drain they are findable.
func TestContentAttachReconcilesWorktreePolicyToAll(t *testing.T) {
	f := newInclusionWorktreeFixture(t)
	f.stampWorktreeBase(t, "main", "main")
	t.Setenv("SEEKFS_CONTENT_WORKTREES", "all")
	builds := stubContentBuildSchedule(t)

	f.s.attachContentForVolume(f.vol)
	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after worktree expansion = %q; want ready", got)
	}
	if *builds != 0 {
		t.Fatalf("worktree expansion scheduled %d builds; want 0", *builds)
	}
	if n := f.vol.contentCoord.queued(); n != 1 {
		t.Fatalf("expansion queued %d FRNs; want 1 (linked.txt)", n)
	}
	resolve := func(frn uint64) (string, bool) {
		if frn == 20 {
			return f.linkedPath, true
		}
		return "", false
	}
	if n := f.vol.contentCoord.processQueue(resolve); n != 1 {
		t.Fatalf("expansion drain extracted %d docs; want 1", n)
	}
	if hits, err := contentServiceSearch(t, f.vol, "content:needlelinked", false); err != nil || len(hits) != 1 {
		t.Fatalf("linked hits = %d; want 1: %v %v", len(hits), hits, err)
	}
}

// A linked worktree that vanishes from disk and the index must reconcile
// like any other deletion: tombstoned at attach, dropped by the fold.
func TestContentAttachReconcilesVanishedWorktree(t *testing.T) {
	f := newInclusionWorktreeFixture(t)
	f.stampWorktreeBase(t, "all", "main", "linked")
	t.Setenv("SEEKFS_CONTENT_WORKTREES", "all")
	builds := stubContentBuildSchedule(t)

	if err := os.RemoveAll(filepath.Join(f.dir, "linked")); err != nil {
		t.Fatal(err)
	}
	for i := range f.vol.index.Records {
		if f.vol.index.Records[i].FRN == 4 || f.vol.index.Records[i].FRN == 20 {
			f.vol.index.Records[i].Deleted = true
		}
	}
	f.s.attachContentForVolume(f.vol)
	if got := f.vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after worktree vanish = %q; want ready", got)
	}
	if *builds != 0 {
		t.Fatalf("vanished worktree scheduled %d builds; want 0", *builds)
	}
	f.s.runContentFold(f.vol)
	reloaded, err := contentLoadFile(f.gsx)
	releaseContentIndexOnCleanup(t, reloaded)
	if err != nil {
		t.Fatal(err)
	}
	for i := range reloaded.Docs {
		if reloaded.Docs[i].FRN == 20 {
			t.Fatal("fold kept the vanished worktree's doc")
		}
	}
}

// Duplicates that diverge (one copy edited) re-resolve at fold: both paths
// stay listed with their own content, and the alias map empties.
func TestContentDedupeUpdatesWhenDuplicateDiverges(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	dir := t.TempDir()
	shared := "shared duplicatedup body\n"
	aPath := filepath.Join(dir, "a.txt")
	bPath := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(aPath, []byte(shared), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, []byte(shared), 0o644); err != nil {
		t.Fatal(err)
	}
	stat := func(path string) (int64, int64) {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.Size(), st.ModTime().UnixNano()
	}
	aSize, aMod := stat(aPath)
	bSize, bMod := stat(bPath)
	records := []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "a.txt", Size: aSize, ModUnix: aMod},
		{FRN: 20, ParentFRN: 20, Parent: -1, Name: "b.txt", Size: bSize, ModUnix: bMod},
	}
	const journal = uint64(7)
	const cp = int64(100)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	idx := &Index{Source: "usn", Volume: dir, Compact: true, JournalID: journal, Checkpoint: cp}
	idx.Records = append([]CompactRecord(nil), records...)
	contentIndexFRNs(idx)
	dbPath := filepath.Join(t.TempDir(), "seekfs_c.gsi")
	vol := newServiceVolumeIndex(dbPath, idx)
	releaseVolumeContentOnCleanup(t, vol)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	gsx := contentIndexPathForDB(dbPath)

	docs := []contentBuildDoc{
		{path: aPath, frn: 10, text: []byte(shared), class: contentClassText, version: 1, fileSize: aSize, fileModNano: aMod},
		{path: bPath, frn: 20, text: []byte(shared), class: contentClassText, version: 1, fileSize: bSize, fileModNano: bMod},
	}
	base, err := assembleContentIndex(docs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = journal
	base.CheckpointUSN = uint64(cp)
	if len(base.Aliases) != 1 {
		t.Fatalf("precondition aliases = %v; want the b.txt dedupe pair", base.Aliases)
	}
	// Explicit roots pin the temp volume (auto scope would select no
	// roots for a synthetic dir), so the fingerprint must cover them.
	t.Setenv("SEEKFS_CONTENT_SCOPE", "explicit")
	t.Setenv("SEEKFS_CONTENT_ROOTS", dir)
	resolved, ok := s.resolvedContentScope(vol)
	if !ok {
		t.Fatal("scope unresolvable for fixture volume")
	}
	base.ScopeHash = resolved.fingerprint()
	if err := contentSaveFile(gsx, base); err != nil {
		t.Fatal(err)
	}
	builds := stubContentBuildSchedule(t)
	s.attachContentForVolume(vol)
	if *builds != 0 {
		t.Fatalf("matching base scheduled %d builds; want 0", *builds)
	}
	if hits, err := contentServiceSearch(t, vol, "content:duplicatedup", false); err != nil || len(hits) != 2 {
		t.Fatalf("dup hits = %d; want 2 (both paths): %v %v", len(hits), hits, err)
	}

	// Diverge b.txt on disk and drain the change.
	if err := os.WriteFile(bPath, []byte("diverged needlediverged body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nbSize, nbMod := stat(bPath)
	for i := range vol.index.Records {
		if vol.index.Records[i].FRN == 20 {
			vol.index.Records[i].Size = nbSize
			vol.index.Records[i].ModUnix = nbMod
		}
	}
	vol.contentCoord.enqueueFRNs([]uint64{20})
	resolve := func(frn uint64) (string, bool) {
		if frn == 20 {
			return bPath, true
		}
		return "", false
	}
	if n := vol.contentCoord.processQueue(resolve); n != 1 {
		t.Fatalf("divergence drain extracted %d docs; want 1", n)
	}
	if hits, _ := contentServiceSearch(t, vol, "content:needlediverged", false); len(hits) != 1 {
		t.Fatalf("diverged hits = %v; want b.txt via the delta", hits)
	}
	s.runContentFold(vol)
	reloaded, err := contentLoadFile(gsx)
	releaseContentIndexOnCleanup(t, reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Aliases) != 0 {
		t.Fatalf("aliases after divergence = %v; want none", reloaded.Aliases)
	}
	if reloaded.Inclusion.DedupeFiles != 0 {
		t.Fatalf("dedupe files after divergence = %d; want 0", reloaded.Inclusion.DedupeFiles)
	}
	if len(reloaded.Docs) != 2 {
		t.Fatalf("docs after divergence = %d; want 2", len(reloaded.Docs))
	}
}

// Per-file caps follow worktree membership: a 2 MiB text file outside a
// repo truncates to the 1 MiB cap, the same file inside a repo indexes
// whole, and an explicit -max-text overrides both.
func TestContentCapsTruncatePerRepo(t *testing.T) {
	build := func(t *testing.T, withGit bool, opts contentBuildOptions) *contentIndex {
		t.Helper()
		dir := t.TempDir()
		if withGit {
			if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		body := strings.Repeat("needle line of source text\n", 80<<10)
		if len(body) < 2<<20 {
			t.Fatalf("fixture body = %d; want over 2 MiB", len(body))
		}
		if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		idx, err := buildContentIndexFromDir(context.Background(), dir, opts)
		if err != nil {
			t.Fatal(err)
		}
		return idx
	}
	outside := build(t, false, defaultContentBuildOptions())
	if len(outside.Docs) != 1 {
		t.Fatalf("outside docs = %d; want 1 (bounded prefix)", len(outside.Docs))
	}
	if got := int64(outside.Docs[0].TextLen); got != contentInclusionTextCapOutside {
		t.Fatalf("outside text = %d; want the 1 MiB cap", got)
	}
	if outside.Policy.Truncated != 1 || outside.Inclusion.Truncated != 1 {
		t.Fatalf("outside truncated = %d/%d; want 1/1", outside.Policy.Truncated, outside.Inclusion.Truncated)
	}
	inside := build(t, true, defaultContentBuildOptions())
	if len(inside.Docs) != 1 || int64(inside.Docs[0].TextLen) <= contentInclusionTextCapOutside {
		t.Fatalf("inside text = %d; want the whole ~2 MiB file", inside.Docs[0].TextLen)
	}
	if inside.Policy.Truncated != 0 {
		t.Fatalf("inside truncated = %d; want 0", inside.Policy.Truncated)
	}
	explicit := contentBuildOptions{MaxFiles: 2_000_000, MaxRaw: 4 << 20, MaxText: 3 << 20, Encoding: "auto"}
	overridden := build(t, false, explicit)
	if len(overridden.Docs) != 1 || int64(overridden.Docs[0].TextLen) <= contentInclusionTextCapOutside {
		t.Fatalf("explicit text = %d; want the whole file via override", overridden.Docs[0].TextLen)
	}
	if overridden.Policy.MaxText != 3<<20 {
		t.Fatalf("explicit policy max_text = %d; want 3 MiB", overridden.Policy.MaxText)
	}
}

// The attached base's CXIS inclusion stats surface in health (loaded).
func TestContentHealthSurfacesInclusion(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	idx := newContentIndex()
	idx.Inclusion = contentInclusionStats{
		Skipped:          map[string]int64{"lockfile": 3},
		SkippedBytes:     map[string]int64{"lockfile": 300},
		DedupeFiles:      5,
		DedupeBytes:      5000,
		WorktreesSkipped: 2,
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	st := newContentVolumeState("C:")
	st.setReady(idx, r, buildContentResolver(nil, nil, nil))
	h := st.healthSnapshot(0)
	if h.Inclusion.Skipped["lockfile"] != 3 || h.Inclusion.SkippedBytes["lockfile"] != 300 {
		t.Fatalf("health inclusion skips not surfaced: %+v", h.Inclusion)
	}
	if h.Inclusion.DedupeFiles != 5 || h.Inclusion.DedupeBytes != 5000 || h.Inclusion.WorktreesSkipped != 2 {
		t.Fatalf("health inclusion dedupe not surfaced: %+v", h.Inclusion)
	}
}
