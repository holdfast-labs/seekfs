package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newWalkMetaTestVolume builds a real walk filename index over root (temp
// files on disk, production walkRoot path) and returns it on a bare
// service, mirroring what the walk watcher serves.
func newWalkMetaTestVolume(t *testing.T, root string) (*serviceVolumeIndex, *goSearchService) {
	t.Helper()
	idx := &Index{Version: indexVersion, Roots: []string{root}, BuiltAt: time.Now(), Source: "walk"}
	if err := walkRoot(root, idx); err != nil {
		t.Fatal(err)
	}
	buildOrders(idx)
	dbPath := filepath.Join(t.TempDir(), "walkmeta.gsi")
	if err := saveIndex(dbPath, idx); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadIndexForService(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	vol := newServiceVolumeIndex(dbPath, loaded)
	s := &goSearchService{stop: make(chan struct{})}
	t.Cleanup(func() {
		if !s.serviceStopping() {
			close(s.stop)
		}
	})
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	return vol, s
}

func searchWalkMetaCount(t *testing.T, vol *serviceVolumeIndex, query string) int {
	t.Helper()
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: query, Limit: 20}, false)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	return len(got)
}

// TestWalkMetaRefreshUpdatesSizeAndMod proves the metadata-only watcher
// path refreshes per-record Size/ModUnix: a write-only edit (same name)
// must become visible to size: filters and sort:modified after the
// "watch-meta" rebuild. Under the old skip-the-rebuild behavior both
// stayed stale forever.
func TestWalkMetaRefreshUpdatesSizeAndMod(t *testing.T) {
	root := t.TempDir()
	growPath := filepath.Join(root, "grow-zz.txt")
	otherPath := filepath.Join(root, "other-zz.txt")
	if err := os.WriteFile(growPath, []byte("tiny"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherPath, []byte("tiny"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol, s := newWalkMetaTestVolume(t, root)

	if n := searchWalkMetaCount(t, vol, "zz size:>1kb"); n != 0 {
		t.Fatalf("precondition size count = %d; want 0", n)
	}

	// Write-only edit: same name, ~3 KB of new content.
	grown := make([]byte, 0, 3000)
	for len(grown) < 3000 {
		grown = append(grown, "metagrowth-0123456789abcdef"...)
	}
	if err := os.WriteFile(growPath, grown[:3000], 0o644); err != nil {
		t.Fatal(err)
	}
	// The exact call the watcher makes for a metadata-only batch.
	if err := s.rebuildWalkVolumeInPlace(vol, "watch-meta"); err != nil {
		t.Fatal(err)
	}
	if n := searchWalkMetaCount(t, vol, "zz size:>1kb"); n != 1 {
		t.Fatalf("post-edit size count = %d; want 1 (grow-zz.txt)", n)
	}

	// sort:modified must follow live mtimes across rebuilds. Pin explicit
	// mtimes an hour apart so filesystem granularity cannot flake it, and
	// assert the winner flips (direction-agnostic).
	now := time.Now()
	if err := os.Chtimes(growPath, now, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(otherPath, now, now); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuildWalkVolumeInPlace(vol, "watch-meta"); err != nil {
		t.Fatal(err)
	}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "zz sort:modified", Limit: 2}, false)
	if err != nil {
		t.Fatalf("sort:modified search: %v", err)
	}
	if len(got) != 2 || got[0].Name != "other-zz.txt" {
		t.Fatalf("sort:modified first = %v; want other-zz.txt newest", pathsOf(got))
	}
	if err := os.Chtimes(growPath, now, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuildWalkVolumeInPlace(vol, "watch-meta"); err != nil {
		t.Fatal(err)
	}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "zz sort:modified", Limit: 2}, false)
	if err != nil {
		t.Fatalf("sort:modified search: %v", err)
	}
	if len(got) != 2 || got[0].Name != "grow-zz.txt" {
		t.Fatalf("sort:modified first after touch = %v; want grow-zz.txt newest", pathsOf(got))
	}
}

// TestWalkMetaRefreshRearmsContent proves the content walk reconcile
// notices a write-only edit once the watcher rebuilds: the edited file's
// (size, mtime) fingerprint changes, the key re-queues, and the new text
// becomes findable. Without the meta rebuild the fingerprint never
// changes and re-extraction never triggers.
func TestWalkMetaRefreshRearmsContent(t *testing.T) {
	stubContentBuildSync(t)
	stubContentCatchUpSync(t)
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "live-meta.txt")
	if err := os.WriteFile(path, []byte("before metabefore"), 0o644); err != nil {
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
	contentWalkDrainTick(t, s, vol)
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:metabefore")); len(got) != 1 {
		t.Fatalf("precondition search = %v; want [live-meta.txt]", got)
	}
	if n := vol.contentCoord.queued(); n != 0 {
		t.Fatalf("precondition queued = %d; want steady state 0", n)
	}

	// Write-only edit: same name, new text, new size.
	if err := os.WriteFile(path, []byte("after metaafter plus much more text to change the size"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The exact call the watcher makes for a metadata-only batch; it
	// reconciles content internally when the volume is attached.
	if err := s.rebuildWalkVolumeInPlace(vol, "watch-meta"); err != nil {
		t.Fatal(err)
	}
	if n := vol.contentCoord.queued(); n != 1 {
		t.Fatalf("reconcile queued = %d after meta rebuild; want 1 (edited key re-armed)", n)
	}
	contentWalkDrainTick(t, s, vol)
	if got := contentWalkNames(contentWalkSearch(t, vol, "content:metaafter")); len(got) != 1 || got[0] != "live-meta.txt" {
		t.Fatalf("edited content = %v; want [live-meta.txt]", got)
	}
	if got := contentWalkSearch(t, vol, "content:metabefore"); len(got) != 0 {
		t.Fatalf("stale content still findable: %v", contentWalkNames(got))
	}
}
