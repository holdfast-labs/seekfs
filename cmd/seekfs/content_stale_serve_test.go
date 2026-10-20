package main

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// staleBaseFixture creates a real one-file volume, builds and attaches its
// content base, then rewrites the file on disk to newText. The rebuild that
// follows keeps the old base attached, so a query can be answered from it
// while disk verification sees the new bytes.
func staleBaseFixture(t *testing.T, oldText, newText string) (*serviceVolumeIndex, *goSearchService, string) {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte(oldText), 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(0xD1)
	const checkpoint = int64(120)
	stubContentCatchUpJournal(t, usnJournalDataV0{
		UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: checkpoint,
	})
	vol, _ := contentBuildTestVolume(t, dir, journal, uint64(checkpoint), []CompactRecord{
		{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: int64(len(oldText))},
	})
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("precondition: content state = %q; want ready", got)
	}
	if err := os.WriteFile(note, []byte(newText), 0o644); err != nil {
		t.Fatal(err)
	}
	return vol, s, note
}

// A rebuild keeps the previous valid base answering queries. The answer is
// flagged stale/incomplete (and complete=false); when the rebuild publishes,
// the new index answers and the flags clear.
func TestContentRebuildServesStaleBase(t *testing.T) {
	vol, s, note := staleBaseFixture(t, "old needle alpha", "new needle omega")

	var duringHits []Entry
	var duringTrace *searchTrace
	var duringHealth contentHealth
	contentBuildIndexingHook = func(st *contentVolumeState) {
		duringHealth = st.healthSnapshot(0)
		var err error
		duringHits, duringTrace, err = contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:needle", 100)
		if err != nil {
			t.Fatalf("search during rebuild: %v", err)
		}
	}

	vol.content.markCatchUpIncomplete()
	s.scheduleContentRebuild(vol)

	if duringHealth.State != contentStateIndexing || !duringHealth.Stale || !duringHealth.Incomplete {
		t.Fatalf("health during rebuild = %+v; want indexing with stale+incomplete", duringHealth)
	}
	if len(duringHits) != 1 || duringHits[0].Path != note {
		t.Fatalf("hits during rebuild = %v; want %s from the previous base", namesOf(duringHits), note)
	}
	if duringTrace == nil || !duringTrace.ContentStale {
		t.Fatalf("trace during rebuild = %+v; want ContentStale", duringTrace)
	}
	if duringTrace.Complete == nil || *duringTrace.Complete {
		t.Fatalf("trace Complete during rebuild = %v; want false", duringTrace.Complete)
	}

	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state after publish = %q; want ready", got)
	}
	if h := vol.content.healthSnapshot(0); h.Stale || h.Incomplete {
		t.Fatalf("health after publish = %+v; want stale and incomplete cleared", h)
	}
	oldHits, oldTrace, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:alpha", 100)
	if err != nil {
		t.Fatalf("search old text after publish: %v", err)
	}
	if len(oldHits) != 0 {
		t.Fatalf("old text still matched after publish: %v", namesOf(oldHits))
	}
	if oldTrace.ContentStale {
		t.Fatal("trace still reports stale after publish")
	}
	newHits, newTrace, err := contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:omega", 100)
	if err != nil {
		t.Fatalf("search new text after publish: %v", err)
	}
	if len(newHits) != 1 || newHits[0].Path != note {
		t.Fatalf("new text hits = %v; want %s", namesOf(newHits), note)
	}
	if newTrace.ContentStale {
		t.Fatal("new index answer is flagged stale")
	}
}

// Disk verification still applies to the old base: when the file no longer
// contains the term, the stale answer must not return it.
func TestContentStaleBaseDiskVerification(t *testing.T) {
	vol, s, _ := staleBaseFixture(t, "old needle alpha", "haystack only")

	var duringHits []Entry
	contentBuildIndexingHook = func(*contentVolumeState) {
		var err error
		duringHits, _, err = contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:needle", 100)
		if err != nil {
			t.Fatalf("search during rebuild: %v", err)
		}
	}

	vol.content.markCatchUpIncomplete()
	s.scheduleContentRebuild(vol)

	if len(duringHits) != 0 {
		t.Fatalf("stale base returned %v for text that no longer exists on disk", namesOf(duringHits))
	}
}

// A count during a stale-serving rebuild is incomplete and never exact.
func TestContentStaleCountIncomplete(t *testing.T) {
	vol, s, _ := staleBaseFixture(t, "old needle alpha", "new needle omega")

	var duringCount int
	var duringErr error
	var duringTrace *searchTrace
	contentBuildIndexingHook = func(*contentVolumeState) {
		duringTrace = &searchTrace{}
		duringCount, _, duringErr = countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
			Query: "content:needle",
			Trace: duringTrace,
		})
	}

	vol.content.markCatchUpIncomplete()
	s.scheduleContentRebuild(vol)

	if duringTrace == nil || !duringTrace.ContentIncomplete || !duringTrace.ContentStale {
		t.Fatalf("count trace during rebuild = %+v; want incomplete and stale", duringTrace)
	}
	if duringTrace.Complete == nil || *duringTrace.Complete {
		t.Fatalf("count Complete during rebuild = %v; want false", duringTrace.Complete)
	}
	if duringErr == nil && duringCount != 1 {
		t.Fatalf("count during rebuild = %d, err=nil; want a refusal or the verified count", duringCount)
	}
	if duringErr != nil && !errors.Is(duringErr, errContentIncomplete) {
		t.Fatalf("count during rebuild err = %v; want errContentIncomplete", duringErr)
	}

	afterTrace := &searchTrace{}
	count, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "content:needle",
		Trace: afterTrace,
	})
	if err != nil || !ok {
		t.Fatalf("count after publish = %d, ok=%v, err=%v", count, ok, err)
	}
	if count != 1 || afterTrace.ContentStale || afterTrace.ContentIncomplete {
		t.Fatalf("count after publish = %d trace=%+v; want exact, not stale", count, afterTrace)
	}
	if afterTrace.Complete == nil || !*afterTrace.Complete {
		t.Fatalf("count after publish Complete = %v; want true", afterTrace.Complete)
	}
}

// An integrity-failed or version-mismatched base is never served: during the
// rebuild the existing unavailable error is returned.
func TestContentStaleIntegrityOrVersionRefuses(t *testing.T) {
	t.Run("integrity", func(t *testing.T) {
		vol, _, _ := staleBaseFixture(t, "old needle alpha", "new needle omega")
		reader, _ := vol.content.readerResolverView()
		if reader == nil || reader.idx == nil {
			t.Fatal("precondition: no attached base")
		}
		if !vol.content.markIntegrityFailed(reader.idx, "section CRC mismatch") {
			t.Fatal("markIntegrityFailed did not drop the attached base")
		}
		var duringErr error
		contentBuildIndexingHook = func(*contentVolumeState) {
			_, _, duringErr = contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:needle", 100)
		}
		s := contentTestService(t)
		s.scheduleContentRebuild(vol)
		if duringErr == nil || !strings.Contains(duringErr.Error(), "content indexing unavailable") {
			t.Fatalf("search during integrity rebuild err = %v; want the unavailable error", duringErr)
		}
	})

	t.Run("version", func(t *testing.T) {
		t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
		stubContentCatchUpSync(t)
		stubContentBuildSync(t)

		dir := t.TempDir()
		note := filepath.Join(dir, "note.txt")
		if err := os.WriteFile(note, []byte("needle"), 0o644); err != nil {
			t.Fatal(err)
		}
		const journal = uint64(0xD2)
		const checkpoint = int64(50)
		stubContentCatchUpJournal(t, usnJournalDataV0{
			UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: checkpoint,
		})
		vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(checkpoint), []CompactRecord{
			{FRN: 10, ParentFRN: 10, Parent: -1, Name: "note.txt", Size: 6},
		})
		bad := contentIndexEncode(newContentIndex())
		binary.LittleEndian.PutUint32(bad[8:], 1)
		if err := os.WriteFile(gsx, bad, 0o644); err != nil {
			t.Fatal(err)
		}

		var duringErr error
		contentBuildIndexingHook = func(*contentVolumeState) {
			_, _, duringErr = contentServiceSearchLimit(t, []*serviceVolumeIndex{vol}, "content:needle", 100)
		}
		s := contentTestService(t)
		s.ensureContentBuild(vol)
		if duringErr == nil || !strings.Contains(duringErr.Error(), "content indexing unavailable") {
			t.Fatalf("search during version rebuild err = %v; want the unavailable error", duringErr)
		}
	})
}
