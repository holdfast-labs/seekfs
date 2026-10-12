package main

// Tests for the `.gsx` mmap load path: zero-copy sections, release on replace,
// the heap fallback, and query/replace concurrency. Test helpers that register
// a mapping release before a temp-dir cleanup live here too.

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func releaseContentIndexOnCleanup(t *testing.T, idx *contentIndex) {
	t.Helper()
	if idx == nil {
		return
	}
	t.Cleanup(idx.Release)
}

// releaseVolumeContentOnCleanup unmaps the base a service attached to vol. A
// test's service cleanup only closes s.stop, which does not release mappings.
func releaseVolumeContentOnCleanup(t *testing.T, vols ...*serviceVolumeIndex) {
	t.Helper()
	t.Cleanup(func() {
		for _, vol := range vols {
			if vol == nil || vol.content == nil {
				continue
			}
			vol.content.mu.Lock()
			idx := vol.content.idx
			vol.content.idx = nil
			vol.content.mu.Unlock()
			if idx != nil {
				idx.Release()
			}
		}
	})
}

// TestContentMappedLoadMatchesHeapRead proves the mapped path is behaviorally
// identical to the old read-into-heap path (and that it really maps).
func TestContentMappedLoadMatchesHeapRead(t *testing.T) {
	idx := buildTestContentIndex(t, map[string]string{
		"a.txt": "alpha needle beta",
		"b.txt": "nothing here",
		"c.md":  "Needle in markdown",
		"d.bin": "binary\x00needle",
	})
	path := filepath.Join(t.TempDir(), "content.gsx")
	if err := contentSaveFile(path, idx); err != nil {
		t.Fatal(err)
	}

	mapped, err := contentLoadFileMapped(path)
	if err != nil {
		t.Fatalf("mapped load: %v", err)
	}
	if mapped.release == nil {
		t.Fatal("mapped load did not map the sidecar")
	}
	releaseContentIndexOnCleanup(t, mapped)

	heap, err := contentLoadFileHeap(path)
	if err != nil {
		t.Fatalf("heap load: %v", err)
	}
	if heap.release != nil {
		t.Fatal("heap load must not own a mapping")
	}

	mr, err := openContentReader(mapped)
	if err != nil {
		t.Fatal(err)
	}
	hr, err := openContentReader(heap)
	if err != nil {
		t.Fatal(err)
	}
	if mr.contentDocCount() != hr.contentDocCount() {
		t.Fatalf("doc count mapped=%d heap=%d", mr.contentDocCount(), hr.contentDocCount())
	}
	for _, term := range []string{"needle", "NEEDLE", "alpha", "markdown", "zzznope", "needle beta"} {
		got := mr.search(term, 0)
		want := hr.search(term, 0)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("search %q mapped=%v heap=%v", term, got, want)
		}
	}
}

// TestContentMappedLoadFallsBackWhenMapFails proves a failed mapping never
// fails a load that could have succeeded: it reads into the heap instead.
func TestContentMappedLoadFallsBackWhenMapFails(t *testing.T) {
	idx := buildTestContentIndex(t, map[string]string{"a.txt": "alpha needle beta"})
	path := filepath.Join(t.TempDir(), "content.gsx")
	if err := contentSaveFile(path, idx); err != nil {
		t.Fatal(err)
	}
	restore := contentMapFileFn
	contentMapFileFn = func(string) ([]byte, func(), error) {
		return nil, nil, errors.New("forced map failure")
	}
	t.Cleanup(func() { contentMapFileFn = restore })

	loaded, err := contentLoadFileMapped(path)
	if err != nil {
		t.Fatalf("fallback load: %v", err)
	}
	if loaded.release != nil {
		t.Fatal("fallback load must not own a mapping")
	}
	r, err := openContentReader(loaded)
	if err != nil {
		t.Fatal(err)
	}
	got := contentPathsOf(r.search("needle", 0))
	if len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("fallback search = %v; want [a.txt]", got)
	}
}

// TestContentMappedReplaceReleasesOldMapping proves the service attach path
// releases the replaced base's mapping exactly once, and the new base is
// queryable. A counter hook observes the unmap.
func TestContentMappedReplaceReleasesOldMapping(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 5, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})

	dir := t.TempDir()
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 5, Checkpoint: 50}
	idx.Records = []CompactRecord{{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10}}
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)
	// No coordinator: this test isolates the base swap, so no drain/catch-up
	// goroutine runs concurrently with the release.
	vol.contentCoord = nil
	releaseVolumeContentOnCleanup(t, vol)
	gsx := contentIndexPathForDB(vol.dbPath)
	base := newContentIndex()
	base.Origin = contentOriginUSN
	base.JournalID = 5
	base.CheckpointUSN = 50
	base.Docs = []contentDoc{{DocID: 0, FRN: 10, ContentType: contentClassText, ExtractorVersion: 1}}
	stampContentTestScope("C:", base)
	if err := contentSaveFile(gsx, base); err != nil {
		t.Fatal(err)
	}

	var unmaps int32
	setContentMappingReleaseHook(func() { atomic.AddInt32(&unmaps, 1) })
	t.Cleanup(func() { setContentMappingReleaseHook(nil) })

	s := contentTestService(t)
	s.attachContentForVolume(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after attach = %q; want ready", got)
	}
	if n := atomic.LoadInt32(&unmaps); n != 0 {
		t.Fatalf("base mapping released before it was replaced: %d", n)
	}

	// Drop the attached base (journal-reset style) and re-attach so the new
	// load replaces the old index under indexMu; the old mapping must unmap.
	vol.content.markStale("test replace")
	s.attachContentForVolume(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after re-attach = %q; want ready", got)
	}
	if n := atomic.LoadInt32(&unmaps); n != 1 {
		t.Fatalf("replaced mapping unmaps = %d; want exactly 1", n)
	}
	if reader, _ := vol.content.readerResolverView(); reader == nil {
		t.Fatal("no reader after replace")
	}
}

// TestContentMappedQueryConcurrentWithReplace exercises the service lock
// discipline without -race: a reader holds indexMu.RLock for the whole query
// while a replacer holds indexMu.Lock to swap and unmap. A correct discipline
// means no use-after-unmap; a wrong one would fault.
func TestContentMappedQueryConcurrentWithReplace(t *testing.T) {
	idx := buildTestContentIndex(t, map[string]string{
		"a.txt": "alpha needle beta",
		"c.md":  "Needle in markdown",
	})
	path := filepath.Join(t.TempDir(), "content.gsx")
	if err := contentSaveFile(path, idx); err != nil {
		t.Fatal(err)
	}

	st := newContentVolumeState("C:")
	first, err := contentLoadFileMapped(path)
	if err != nil {
		t.Fatal(err)
	}
	firstReader, err := openContentReader(first)
	if err != nil {
		t.Fatal(err)
	}
	st.setReady(first, firstReader, buildContentResolver(first.Docs, nil, nil))

	var s goSearchService
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.indexMu.RLock()
			reader, _ := st.readerResolverView()
			if reader != nil {
				_ = reader.search("needle", 3)
			}
			s.indexMu.RUnlock()
		}
	}()

	for i := 0; i < 200; i++ {
		next, err := contentLoadFileMapped(path)
		if err != nil {
			t.Fatalf("map %d: %v", i, err)
		}
		reader, err := openContentReader(next)
		if err != nil {
			t.Fatalf("reader %d: %v", i, err)
		}
		s.indexMu.Lock()
		old := st.setReady(next, reader, buildContentResolver(next.Docs, nil, nil))
		if old != nil {
			old.Release()
		}
		s.indexMu.Unlock()
	}

	close(stop)
	wg.Wait()

	st.mu.Lock()
	final := st.idx
	st.idx = nil
	st.mu.Unlock()
	if final != nil {
		final.Release()
	}
}
