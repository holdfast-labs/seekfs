package main

// Service-level content tests for walk-indexed volumes (no USN journal):
// build, attach, query, edit/create/delete/rename + reconcile/watch, fold,
// restart, and the sharded variant. Walk record FRNs are positional
// (FRN = i+1 in walk order), so every fixture here goes through the real
// walkRoot + save/load round trip: synthetic CompactRecords with hand-picked
// FRNs would hide a misjoin.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// newContentWalkTestVolume builds a walk filename index over dir (real files
// on disk, production walkRoot path), round-trips it through the .gsi, and
// returns the service volume. The content DB lives outside dir so the walk
// never indexes seekfs's own artifacts.
func newContentWalkTestVolume(t *testing.T, dir string) (*serviceVolumeIndex, string) {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	t.Setenv("SEEKFS_CONTENT_SCOPE", "explicit")
	t.Setenv("SEEKFS_CONTENT_ROOTS", dir)
	idx := &Index{Version: indexVersion, Roots: []string{dir}, BuiltAt: time.Now(), Source: "walk"}
	if err := walkRoot(dir, idx); err != nil {
		t.Fatal(err)
	}
	buildOrders(idx)
	dbPath := filepath.Join(t.TempDir(), "walk.gsi")
	if err := saveIndex(dbPath, idx); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadIndexForService(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	vol := newServiceVolumeIndex(dbPath, loaded)
	if vol.content == nil || vol.contentCoord == nil {
		t.Fatal("content state not initialized")
	}
	releaseVolumeContentOnCleanup(t, vol)
	return vol, dbPath
}

// contentWalkDrainTick runs one walk drain pass: reconcile the filename index
// against the content base (forced, metadata-only), extract the queue
// through the key->path resolver, and record unindexable keys, exactly as
// the drain loop does.
func contentWalkDrainTick(t *testing.T, s *goSearchService, vol *serviceVolumeIndex) {
	t.Helper()
	s.contentReconcileWalkVolume(vol, true)
	vol.contentCoord.declined = func(key uint64, size, modNano int64) {
		vol.content.walkDeclinedNote(key, contentWalkPrint{size: size, mod: modNano})
	}
	defer func() { vol.contentCoord.declined = nil }()
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	cache := make(map[int]string, 64)
	vol.contentCoord.processQueue(func(key uint64) (string, bool) {
		return s.contentResolveWalkPath(vol, key, cache)
	})
}

// contentWalkSearch is contentServiceSearch that fails the test on error.
func contentWalkSearch(t *testing.T, vol *serviceVolumeIndex, query string) []Entry {
	t.Helper()
	hits, err := contentServiceSearch(t, vol, query, false)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	return hits
}

func contentWalkNames(hits []Entry) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Name)
	}
	sort.Strings(out)
	return out
}

// The stable key is a pure function of the cleaned absolute path (so a
// rescan that renumbers every record still joins), and case-sensitive (so
// two files differing only by case on a case-sensitive filesystem never
// share a key).
func TestContentWalkKeyStable(t *testing.T) {
	a := contentWalkKey(filepath.Join(`E:\bench`, "root", "a.txt"))
	b := contentWalkKey(`E:\bench\root\.\a.txt`)
	if a == 0 || a != b {
		t.Fatalf("walk key not stable across Clean variants: %d vs %d", a, b)
	}
	upper := contentWalkKey(filepath.Join(`E:\bench`, "root", "A.TXT"))
	if upper == a {
		t.Fatal("walk key folds case: distinct files would share a key")
	}
	if got := contentWalkKey(""); got != 0 {
		t.Fatalf("walk key of empty path = %d; want 0", got)
	}
}

// Build + attach + query through the service-owned path: a fresh walk volume
// with the flag on must become content-ready without a manual CLI step, and
// the docs must be keyed on the stable path hash with the v6 fingerprint.
func TestContentWalkBuildAttachQuery(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("alpha needleone here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "beta.txt"), []byte("nothing relevant"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol, _ := newContentWalkTestVolume(t, root)
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("walk build state = %q; want ready (%+v)", got, vol.content.healthSnapshot(0))
	}
	vol.content.mu.RLock()
	origin := vol.content.idx.Origin
	docs := append([]contentDoc(nil), vol.content.idx.Docs...)
	vol.content.mu.RUnlock()
	if origin != contentOriginWalk {
		t.Fatalf("attached base origin = %d; want walk (%d)", origin, contentOriginWalk)
	}
	if len(docs) != 2 {
		t.Fatalf("attached docs = %d; want 2", len(docs))
	}
	for _, d := range docs {
		if d.FileSize <= 0 || d.FileModNano <= 0 {
			t.Fatalf("doc FRN %d missing v6 fingerprint (size=%d mod=%d)", d.FRN, d.FileSize, d.FileModNano)
		}
	}
	// Every doc key must be the stable key of a live record path: a join on
	// positional FRNs would fail this after any rescan.
	s.indexMu.RLock()
	keys, _, ok := s.contentWalkKeys(vol)
	s.indexMu.RUnlock()
	if !ok {
		t.Fatal("no walk join column")
	}
	for _, d := range docs {
		if i, found := contentLookupFRNColumn(keys, d.FRN); !found || i >= len(keys) {
			t.Fatalf("doc key %d joins no live record", d.FRN)
		}
	}
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:needleone")); len(got) != 1 || got[0] != "alpha.txt" {
		t.Fatalf("content:needleone = %v; want [alpha.txt]", got)
	}
	if got := contentWalkSearch(t, vol, "content:zzznope"); len(got) != 0 {
		t.Fatalf("content:zzznope = %v; want empty", contentWalkNames(got))
	}
}

// A compound filename+content query on a walk volume must agree between the
// compound lane (on by default) and the per-volume path: both verify through
// the same walk-keyed content resolution.
func TestContentWalkCompoundLaneParity(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("laneparity needle here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.md"), []byte("laneparity other place"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "c.txt"), []byte("unrelated words"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol, _ := newContentWalkTestVolume(t, root)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition state = %q; want ready", got)
	}
	t.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "1")
	lane := contentWalkNames(contentWalkSearch(t, vol, "ext:txt content:laneparity"))
	t.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "0")
	slow := contentWalkNames(contentWalkSearch(t, vol, "ext:txt content:laneparity"))
	if len(lane) != 1 || lane[0] != "a.txt" {
		t.Fatalf("lane route = %v; want [a.txt]", lane)
	}
	if len(slow) != 1 || slow[0] != "a.txt" {
		t.Fatalf("per-volume route = %v; want [a.txt]", slow)
	}
}

// A USN-keyed sidecar next to a walk .gsi must be refused, mirroring the
// existing USN-refuses-walk guard: the key spaces never join.
func TestContentWalkRejectsUSNSidecar(t *testing.T) {
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol, dbPath := newContentWalkTestVolume(t, root)
	usn := newContentIndex()
	usn.Origin = contentOriginUSN
	usn.JournalID = 7
	usn.CheckpointUSN = 90
	stampContentTestScope(vol.volume, usn)
	if err := contentSaveFile(contentIndexPathForDB(dbPath), usn); err != nil {
		t.Fatal(err)
	}
	s := contentTestService(t)
	s.attachContentForVolume(vol)
	if got := vol.content.stateOf(); got == contentStateReady {
		t.Fatalf("USN-keyed sidecar attached to a walk volume as ready")
	}
	if vol.content.usableForQuery() {
		t.Fatal("foreign-origin sidecar must not be usable")
	}
}

// A walk sidecar whose docs join no live record must stay stale, never ready
// with an empty result set: this is the misjoin guard (results resolving to
// the wrong record would be worse than an error).
func TestContentWalkZeroJoinStaysStale(t *testing.T) {
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol, dbPath := newContentWalkTestVolume(t, root)
	base, err := assembleContentIndex([]contentBuildDoc{
		{path: `E:\nowhere\ghost.txt`, frn: contentWalkKey(`E:\nowhere\ghost.txt`), text: []byte("ghost needle"), class: contentClassText, version: contentTextExtractor{}.Version()},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginWalk
	base.CheckpointUSN = uint64(time.Now().UnixNano())
	stampContentTestScope(vol.volume, base)
	if err := contentSaveFile(contentIndexPathForDB(dbPath), base); err != nil {
		t.Fatal(err)
	}
	s := contentTestService(t)
	s.attachContentForVolume(vol)
	if got := vol.content.stateOf(); got != contentStateStale {
		t.Fatalf("zero-join walk base state = %q; want stale", got)
	}
	if vol.content.usableForQuery() {
		t.Fatal("zero-join walk base must not be usable")
	}
}

// A reconcile with no changes must be a no-op: nothing enqueued, no
// tombstones. In particular, out-of-scope files (never indexed) must not
// plant phantom tombstones that every fold reprocesses, and empty files
// (unindexable) must not re-enqueue forever.
func TestContentWalkReconcileSteadyState(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("steadymarker text here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0, 1, 2, 3, 4}, 0o644); err != nil {
		t.Fatal(err)
	}
	vol, _ := newContentWalkTestVolume(t, root)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition state = %q; want ready", got)
	}
	s.contentReconcileWalkVolume(vol, true)
	if n := vol.contentCoord.queued(); n != 0 {
		t.Fatalf("steady-state reconcile queued %d keys; want 0", n)
	}
	if n := vol.content.deltaView().len(); n != 0 {
		t.Fatalf("steady-state reconcile left %d delta entries; want 0 (no phantoms)", n)
	}
	// And again: idempotent.
	s.contentReconcileWalkVolume(vol, true)
	if n := vol.contentCoord.queued(); n != 0 {
		t.Fatalf("second steady-state reconcile queued %d keys; want 0", n)
	}
	if n := vol.content.deltaView().len(); n != 0 {
		t.Fatalf("second steady-state reconcile left %d delta entries; want 0", n)
	}
}

// A nonzero-size file with no indexable text (binary-sniffed) must converge
// too: the first reconcile enqueues it, extraction declines, and later
// reconciles remember it instead of retrying forever. A file that gains
// text afterwards is still picked up via its new fingerprint.
func TestContentWalkReconcileUnindexableConverges(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(root, "data.txt")
	blob := make([]byte, 0, 300)
	for i := 0; i < 300; i++ {
		blob = append(blob, byte(i%251), 0)
	}
	if err := os.WriteFile(binPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	vol, _ := newContentWalkTestVolume(t, root)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition state = %q; want ready", got)
	}
	// First pass: enqueued, extraction declines, remembered.
	contentWalkDrainTick(t, s, vol)
	if n := vol.content.deltaView().len(); n != 0 {
		t.Fatalf("declined file left %d delta entries; want 0", n)
	}
	// Second pass: remembered, not re-enqueued.
	s.contentReconcileWalkVolume(vol, true)
	if n := vol.contentCoord.queued(); n != 0 {
		t.Fatalf("remembered unindexable file re-queued (%d); want 0", n)
	}
	// The file gains real text (and size): the new fingerprint re-arms it.
	if err := os.WriteFile(binPath, []byte("nowindexable marker with plenty of text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuildWalkVolumeInPlace(vol, "test"); err != nil {
		t.Fatal(err)
	}
	contentWalkDrainTick(t, s, vol)
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:nowindexable")); len(got) != 1 || got[0] != "data.txt" {
		t.Fatalf("re-armed content = %v; want [data.txt]", got)
	}
}

// Deleting a file tombstones it; restoring the identical content before the
// fold must rescind the tombstone, or the fold would drop live content.
func TestContentWalkRestoreBeforeFoldKeepsDoc(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "fragile.txt")
	text := []byte("fragilemarker durable text content here")
	if err := os.WriteFile(path, text, 0o644); err != nil {
		t.Fatal(err)
	}
	vol, _ := newContentWalkTestVolume(t, root)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition state = %q; want ready", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuildWalkVolumeInPlace(vol, "test"); err != nil {
		t.Fatal(err)
	}
	contentWalkDrainTick(t, s, vol)
	if got := contentWalkSearch(t, vol, "content:fragilemarker"); len(got) != 0 {
		t.Fatalf("deleted content still findable: %v", contentWalkNames(got))
	}

	if err := os.WriteFile(path, text, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuildWalkVolumeInPlace(vol, "test"); err != nil {
		t.Fatal(err)
	}
	contentWalkDrainTick(t, s, vol)
	s.runContentFold(vol)
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:fragilemarker")); len(got) != 1 || got[0] != "fragile.txt" {
		t.Fatalf("restored content after fold = %v; want [fragile.txt]", got)
	}
}

// Edit a file on disk, run the watcher-equivalent rebuild, and drain: the
// new text becomes findable and the old text stops matching, within the
// reconcile + drain path (no USN stream involved).
func TestContentWalkReconcileEdit(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "live.txt")
	if err := os.WriteFile(path, []byte("before editmarker"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol, _ := newContentWalkTestVolume(t, root)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	s.indexes = []*Index{vol.index}
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition state = %q; want ready", got)
	}
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:editmarker")); len(got) != 1 {
		t.Fatalf("precondition search = %v; want [live.txt]", got)
	}

	if err := os.WriteFile(path, []byte("after editmarker plus much more text to change the size"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuildWalkVolumeInPlace(vol, "test"); err != nil {
		t.Fatal(err)
	}
	contentWalkDrainTick(t, s, vol)
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:plus")); len(got) != 1 || got[0] != "live.txt" {
		t.Fatalf("edited content = %v; want [live.txt]", got)
	}
	if got := contentWalkSearch(t, vol, "content:before"); len(got) != 0 {
		t.Fatalf("stale content still findable: %v", contentWalkNames(got))
	}
}

// Create, delete, and rename files on disk, then rebuild + drain: creates
// become findable, deletes stop matching, and a rename moves the match to
// the new name without leaving the old one behind.
func TestContentWalkReconcileCreateDeleteRename(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keepmarker stays"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gone.txt"), []byte("gonemarker leaves"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("movemarker travels"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol, _ := newContentWalkTestVolume(t, root)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition state = %q; want ready", got)
	}

	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("newmarker arrives with extra padding"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "old.txt"), filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuildWalkVolumeInPlace(vol, "test"); err != nil {
		t.Fatal(err)
	}
	contentWalkDrainTick(t, s, vol)

	if got := contentWalkNames(contentWalkSearch(t, vol, "content:newmarker")); len(got) != 1 || got[0] != "new.txt" {
		t.Fatalf("created content = %v; want [new.txt]", got)
	}
	if got := contentWalkSearch(t, vol, "content:gonemarker"); len(got) != 0 {
		t.Fatalf("deleted content still findable: %v", contentWalkNames(got))
	}
	got := contentWalkNames(contentWalkSearch(t, vol, "content:movemarker"))
	if len(got) != 1 || got[0] != "renamed.txt" {
		t.Fatalf("renamed content = %v; want [renamed.txt]", got)
	}
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:keepmarker")); len(got) != 1 || got[0] != "keep.txt" {
		t.Fatalf("untouched content = %v; want [keep.txt]", got)
	}
}

// A walk fold persists the delta (changed docs, tombstones) into a new
// walk-origin base with the watermark advanced, and the delta drains.
func TestContentWalkFold(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("foldmarker old text here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("foldmarker steady text"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol, dbPath := newContentWalkTestVolume(t, root)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition state = %q; want ready", got)
	}
	gsx := contentIndexPathForDB(dbPath)
	before, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	beforeCP := before.CheckpointUSN
	before.Release()

	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("foldmarker new longer text after edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuildWalkVolumeInPlace(vol, "test"); err != nil {
		t.Fatal(err)
	}
	contentWalkDrainTick(t, s, vol)
	if n := vol.content.deltaView().len(); n == 0 {
		t.Fatal("reconcile+drain produced no delta entries to fold")
	}
	s.runContentFold(vol)

	after, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Release()
	if after.Origin != contentOriginWalk {
		t.Fatalf("folded base origin = %d; want walk", after.Origin)
	}
	if after.JournalID != 0 {
		t.Fatalf("folded walk base journal = %d; want 0", after.JournalID)
	}
	if after.CheckpointUSN < beforeCP {
		t.Fatalf("folded walk watermark %d went backwards from %d", after.CheckpointUSN, beforeCP)
	}
	if n := vol.content.deltaView().len(); n != 0 {
		t.Fatalf("delta after fold = %d entries; want drained", n)
	}
	if got := vol.content.stateOf(); got != contentStateReady && got != contentStateDegraded {
		t.Fatalf("state after fold = %q; want ready/degraded", got)
	}
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:longer")); len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("folded new content = %v; want [a.txt]", got)
	}
	if got := contentWalkSearch(t, vol, "content:steady"); len(got) != 0 {
		t.Fatalf("folded deleted content still findable: %v", contentWalkNames(got))
	}
	// The v6 fingerprint survives the fold so the next reconcile diffs
	// against extraction-time values, not zeroes.
	for _, d := range after.Docs {
		if d.FileSize <= 0 || d.FileModNano <= 0 {
			t.Fatalf("folded doc FRN %d lost its fingerprint", d.FRN)
		}
	}
}

// Restart: a persisted walk .gsi + .gsx re-attaches ready with old content
// findable; a post-restart watcher rebuild picks up the disk change.
func TestContentWalkRestart(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("restartmarker original text"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol, dbPath := newContentWalkTestVolume(t, root)
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition state = %q; want ready", got)
	}

	// A new service over the same persisted files is a restart.
	loaded, err := loadIndexForService(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	vol2 := newServiceVolumeIndex(dbPath, loaded)
	releaseVolumeContentOnCleanup(t, vol2)
	s2 := contentTestService(t)
	s2.volumes = []*serviceVolumeIndex{vol2}
	s2.indexes = []*Index{vol2.index}
	s2.ensureContentBuild(vol2)
	if got := vol2.content.stateOf(); got != contentStateReady {
		t.Fatalf("restart attach state = %q; want ready", got)
	}
	if got := contentWalkNames(contentWalkSearch(t, vol2, "content:original")); len(got) != 1 || got[0] != "note.txt" {
		t.Fatalf("restart content = %v; want [note.txt]", got)
	}

	if err := os.WriteFile(path, []byte("restartmarker revised text with more words"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s2.rebuildWalkVolumeInPlace(vol2, "test"); err != nil {
		t.Fatal(err)
	}
	contentWalkDrainTick(t, s2, vol2)
	if got := contentWalkNames(contentWalkSearch(t, vol2, "content:revised")); len(got) != 1 || got[0] != "note.txt" {
		t.Fatalf("post-restart edited content = %v; want [note.txt]", got)
	}
	if got := contentWalkSearch(t, vol2, "content:original"); len(got) != 0 {
		t.Fatalf("stale post-restart content still findable: %v", contentWalkNames(got))
	}
}

// Sharded walk variant: a corpus over the shard target builds a manifest,
// serves every shard, and folds dirty shards only.
func TestContentWalkSharded(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	restoreTarget := contentShardTargetBytes
	contentShardTargetBytes = 60
	t.Cleanup(func() { contentShardTargetBytes = restoreTarget })

	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	for _, folder := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(root, folder), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	texts := map[string]string{
		"a": "shardmarker alpha padding padding padding",
		"b": "shardmarker beta padding padding padding",
	}
	for folder, text := range texts {
		if err := os.WriteFile(filepath.Join(root, folder, folder+".txt"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	vol, _ := newContentWalkTestVolume(t, root)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("sharded walk build state = %q; want ready", got)
	}
	reader := vol.content.readerView()
	if reader == nil || reader.idx.shards == nil {
		t.Fatal("walk base over the shard target is not sharded")
	}
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:shardmarker")); len(got) != 2 {
		t.Fatalf("cross-shard content = %v; want both files", got)
	}

	if err := os.WriteFile(filepath.Join(root, "a", "a.txt"), []byte("shardmarker alpha revised with change"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuildWalkVolumeInPlace(vol, "test"); err != nil {
		t.Fatal(err)
	}
	contentWalkDrainTick(t, s, vol)
	s.runContentFold(vol)
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:revised")); len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("folded sharded content = %v; want [a.txt]", got)
	}
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:beta")); len(got) != 1 || got[0] != "b.txt" {
		t.Fatalf("untouched shard content = %v; want [b.txt]", got)
	}
	reader = vol.content.readerView()
	if reader == nil || reader.idx.Origin != contentOriginWalk {
		t.Fatal("folded sharded base lost its walk origin")
	}
}

// Freshness probe for the watch path (edit -> rebuild -> reconcile ->
// drain -> findable), mirroring the USN write-close latency test: it asserts
// correctness and reports per-round latency without a wall-clock bound.
func TestContentWalkFreshnessEditToFindable(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "live.txt")
	if err := os.WriteFile(path, []byte("seed text"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol, _ := newContentWalkTestVolume(t, root)
	s := contentTestService(t)
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition state = %q; want ready", got)
	}

	const rounds = 5
	latencies := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		needle := strings.Repeat("x", i) + "freshwalkmarker"
		if err := os.WriteFile(path, []byte(needle+" body text for size change"), 0o644); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := s.rebuildWalkVolumeInPlace(vol, "test"); err != nil {
			t.Fatal(err)
		}
		contentWalkDrainTick(t, s, vol)
		matches := contentWalkSearch(t, vol, "content:"+needle)
		latency := time.Since(start)
		if len(matches) != 1 {
			t.Fatalf("round %d: edited content not findable after rebuild+drain (%d matches)", i, len(matches))
		}
		latencies = append(latencies, latency)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("walk edit -> content-findable over %d rounds: p50=%s max=%s", rounds, latencies[len(latencies)/2], latencies[len(latencies)-1])
}
