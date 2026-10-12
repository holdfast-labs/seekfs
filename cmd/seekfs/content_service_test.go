package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContentChangeExcluded(t *testing.T) {
	for _, n := range []string{"v.gsx", "v.gsx.123456.tmp", "v.gsx.lock", "v.gsi", "v.gsi.wal", "v.gsi.999.tmp", ".seekfs-content.gsx"} {
		if !contentChangeExcluded(n) {
			t.Errorf("%q should be excluded from content change intake", n)
		}
	}
	for _, n := range []string{"main.go", "notes.txt", "report.pdf", "index.html"} {
		if contentChangeExcluded(n) {
			t.Errorf("%q should NOT be excluded", n)
		}
	}
}

func TestContentEligibleForExtraction(t *testing.T) {
	if contentEligibleForExtraction(0, contentAttrDirectory) {
		t.Fatal("a directory must not be eligible")
	}
	if contentEligibleForExtraction(uint32(1)<<31, 0) {
		t.Fatal("a directory mode must not be eligible (os.ModeDir bit 31)")
	}
	if !contentEligibleForExtraction(0, 0x20 /* archive */) {
		t.Fatal("an ordinary file must be eligible")
	}
	for _, attr := range []uint32{contentAttrOffline, contentAttrRecallOnOpen, contentAttrRecallOnDataAccess, contentAttrReparsePoint} {
		if contentEligibleForExtraction(0, attr) {
			t.Fatalf("attribute %#x must make a file ineligible (cloud/reparse)", attr)
		}
	}
}

func contentTestState() *contentVolumeState {
	idx := newContentIndex()
	idx.Docs = []contentDoc{
		{DocID: 0, FRN: 100},
		{DocID: 1, FRN: 200},
	}
	r, _ := openContentReader(idx)
	s := newContentVolumeState("C:")
	s.setReady(idx, r, buildContentResolver(idx.Docs, []uint64{100, 200}, []uint32{0, 1}))
	return s
}

// PB7: the attached base's extraction policy and skip/truncation counts are
// surfaced in health, so a size cap is visible rather than a silent drop.
func TestContentHealthSurfacesBuildPolicy(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	idx := newContentIndex()
	idx.Policy = contentBuildPolicy{MaxRaw: 8, MaxText: 4, Skipped: 2, Truncated: 1, ScopeDropped: 1}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	st := newContentVolumeState("C:")
	st.setReady(idx, r, buildContentResolver(nil, nil, nil))
	h := st.healthSnapshot(0)
	if h.MaxRaw != 8 || h.MaxText != 4 || h.Skipped != 2 || h.Truncated != 1 {
		t.Fatalf("health policy not surfaced: %+v", h)
	}
	if !h.Incomplete || h.State != contentStateDegraded {
		t.Fatalf("budget-limited sidecar not marked incomplete: %+v", h)
	}
}

func TestContentCoordinatorDirtyPromote(t *testing.T) {
	s := contentTestState()
	c := newContentCoordinator(s)

	// Without a drain attached, the stream is ignored (no undrained map).
	c.observeChanges([]usnChange{{FRN: 111, Reason: usnReasonDataOverwrite, Attr: 0}})
	if c.dirtyCount() != 0 {
		t.Fatal("observeChanges must be a no-op until enableDrain")
	}
	c.enableDrain()

	// A data overwrite for a normal file marks it dirty.
	c.observeChanges([]usnChange{{FRN: 111, Reason: usnReasonDataOverwrite, Attr: 0}})
	if c.dirtyCount() != 1 || c.queued() != 0 {
		t.Fatalf("dirty=%d queued=%d; want 1/0", c.dirtyCount(), c.queued())
	}

	// A close promotes it to the queue.
	c.closeFRN(111)
	if c.dirtyCount() != 0 || c.queued() != 1 {
		t.Fatalf("dirty=%d queued=%d; want 0/1", c.dirtyCount(), c.queued())
	}

	// A close for a never-written FRN is a no-op (read-only open).
	c.closeFRN(222)
	if c.queued() != 1 {
		t.Fatalf("read-only close queued work: queued=%d", c.queued())
	}

	q := c.takeQueued()
	if len(q) != 1 || q[0] != 111 {
		t.Fatalf("takeQueued = %v; want [111]", q)
	}
	if c.queued() != 0 {
		t.Fatal("takeQueued must drain the queue")
	}
}

func TestContentCoordinatorSkipsIneligibleAndRename(t *testing.T) {
	s := contentTestState()
	c := newContentCoordinator(s)
	c.enableDrain()

	c.observeChanges([]usnChange{
		{FRN: 1, Reason: usnReasonFileCreate, Attr: contentAttrDirectory},
		{FRN: 2, Reason: usnReasonDataOverwrite, Attr: contentAttrOffline},
		{FRN: 3, Reason: usnReasonDataOverwrite, Attr: contentAttrRecallOnDataAccess},
		{FRN: 5, Reason: usnReasonClose, Attr: 0},
	})
	if c.dirtyCount() != 0 {
		t.Fatalf("dirty=%d; want 0 (dirs, cloud, bare close must not mark)", c.dirtyCount())
	}

	// A rename-new for an FRN that already has a content doc is an intra-volume
	// rename: the bytes are unchanged, so it must not be re-extracted.
	c.observeChanges([]usnChange{{FRN: 100, Reason: usnReasonRenameNew, Attr: 0}})
	if c.dirtyCount() != 0 {
		t.Fatalf("dirty=%d; an intra-volume rename with an existing doc must not mark", c.dirtyCount())
	}

	// A rename-new for an FRN with no content doc is a file moved into the
	// volume while stopped; it must be extracted.
	c.observeChanges([]usnChange{{FRN: 999, Reason: usnReasonRenameNew, Attr: 0}})
	if c.dirtyCount() != 1 {
		t.Fatalf("dirty=%d; a moved-in file (no doc) must mark", c.dirtyCount())
	}
	c.promoteAll()
	if q := c.takeQueued(); len(q) != 1 || q[0] != 999 {
		t.Fatalf("queued = %v; want [999]", q)
	}
}

func TestContentCoordinatorQuietWindowAndInvalidate(t *testing.T) {
	s := contentTestState()
	c := newContentCoordinator(s)
	c.enableDrain()
	c.observeChanges([]usnChange{{FRN: 7, Reason: usnReasonDataExtend, Attr: 0}})
	c.promoteAll()
	if c.queued() != 1 || c.dirtyCount() != 0 {
		t.Fatalf("promoteAll left dirty=%d queued=%d", c.dirtyCount(), c.queued())
	}
	c.invalidate("journal reset")
	if c.state.stateOf() != contentStateStale {
		t.Fatalf("state = %q; want stale", c.state.stateOf())
	}
	if c.queued() != 0 || c.dirtyCount() != 0 {
		t.Fatalf("invalidate left dirty=%d queued=%d", c.dirtyCount(), c.queued())
	}
	if c.state.deltaView().len() != 0 {
		t.Fatal("invalidate must clear the delta")
	}
}

func TestContentCoordinatorDeleteRemoves(t *testing.T) {
	s := contentTestState()
	c := newContentCoordinator(s)
	c.enableDrain()
	s.delta.upsert(contentDeltaDoc{FRN: 42, Path: "x", Text: []byte("hi")})
	c.observeChanges([]usnChange{{FRN: 42, Reason: usnReasonFileDelete, Attr: 0}})
	if len(s.deltaView().live()) != 0 {
		t.Fatal("a delete must remove the delta doc")
	}
}

func TestContentDeltaUpsertDelete(t *testing.T) {
	d := newContentDelta()
	d.upsert(contentDeltaDoc{FRN: 1, Path: "a", Text: []byte("one")})
	d.upsert(contentDeltaDoc{FRN: 1, Path: "a", Text: []byte("two")})
	if d.len() != 1 {
		t.Fatalf("delta len = %d; want 1 after upsert", d.len())
	}
	if string(d.live()[0].Text) != "two" {
		t.Fatalf("delta text = %q; want two", d.live()[0].Text)
	}
	d.delete(1)
	if len(d.live()) != 0 {
		t.Fatal("deleted doc must not be live")
	}
}

func TestContentCoordinatorProcessQueue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("alpha needle beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := contentTestState()
	c := newContentCoordinator(s)
	c.enableDrain()
	c.observeChanges([]usnChange{{FRN: 9, Reason: usnReasonFileCreate | usnReasonClose, Attr: 0}})
	if c.queued() != 1 {
		t.Fatalf("queued=%d; want 1", c.queued())
	}
	resolve := func(frn uint64) (string, bool) {
		if frn == 9 {
			return path, true
		}
		return "", false
	}
	if n := c.processQueue(resolve); n != 1 {
		t.Fatalf("processQueue changed=%d; want 1", n)
	}
	live := s.deltaView().live()
	if len(live) != 1 || live[0].FRN != 9 {
		t.Fatalf("delta = %+v", live)
	}

	// A write that does not change content must not re-index.
	c.observeChanges([]usnChange{{FRN: 9, Reason: usnReasonDataOverwrite, Attr: 0}})
	c.closeFRN(9)
	if n := c.processQueue(resolve); n != 0 {
		t.Fatalf("unchanged file re-indexed: changed=%d", n)
	}

	// An FRN that no longer resolves is removed from the delta.
	c.observeChanges([]usnChange{{FRN: 9, Reason: usnReasonDataOverwrite, Attr: 0}})
	c.closeFRN(9)
	if n := c.processQueue(func(uint64) (string, bool) { return "", false }); n != 0 {
		t.Fatalf("unresolvable FRN changed=%d; want 0", n)
	}
	if len(s.deltaView().live()) != 0 {
		t.Fatal("unresolvable FRN must delete its delta doc")
	}
}

func TestContentVolumeStateRebuildResolverAfterBaseSwap(t *testing.T) {
	s := contentTestState()
	// Initial base: FRN 100 -> record 0, 200 -> record 1.
	s.rebuildResolver([]uint64{100, 200}, []uint32{0, 1})
	if id, ok := s.resolver.recordID(0); !ok || id != 0 {
		t.Fatalf("initial resolver doc 0 -> %d, %v", id, ok)
	}

	// A base swap renumbers records: FRN 100 -> 5, 200 dropped.
	s.rebuildResolver([]uint64{100}, []uint32{5})
	if id, ok := s.resolver.recordID(0); !ok || id != 5 {
		t.Fatalf("post-swap resolver doc 0 -> %d, %v; want 5", id, ok)
	}
	if _, ok := s.resolver.recordID(1); ok {
		t.Fatal("a dropped FRN must resolve to no record after the swap")
	}
}
