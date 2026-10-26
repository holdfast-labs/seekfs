package main

// Early publish: a content build answers a query from the extracted prefix
// (partial=true) before it finishes, then from the complete corpus.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentBuildPublishesPrefixEarly(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	restoreDocs, restoreBytes := contentPreviewMinDocs, contentPreviewMinBytes
	restoreHook, restoreDisabled := contentPreviewPublishedHook, contentPreviewPublishDisabled
	contentPreviewMinDocs, contentPreviewMinBytes = 1, 1<<40
	contentPreviewPublishDisabled = false
	t.Cleanup(func() {
		contentPreviewMinDocs, contentPreviewMinBytes = restoreDocs, restoreBytes
		contentPreviewPublishedHook, contentPreviewPublishDisabled = restoreHook, restoreDisabled
	})

	dir := t.TempDir()
	files := map[string]string{"a.txt": "alpha first file", "b.txt": "beta second file"}
	var recs []CompactRecord
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, CompactRecord{FRN: uint64(len(recs) + 1), Parent: -1, Name: name, Size: int64(len(body))})
	}
	const journal = uint64(0xEA)
	const cp = int64(50)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, _ := contentBuildTestVolume(t, dir, journal, uint64(cp), recs)

	previewQueries := 0
	contentPreviewPublishedHook = func(s *goSearchService, st *contentVolumeState, docs int) {
		previewQueries++
		h := st.healthSnapshot(0)
		if h.State != contentStateIndexing || !h.Partial {
			t.Fatalf("preview health = %+v; want indexing with partial=true", h)
		}
		if h.BuildDone != 1 || h.BuildTotal != 2 {
			t.Fatalf("preview progress = %d/%d; want 1/2", h.BuildDone, h.BuildTotal)
		}
		if docs != 1 {
			t.Fatalf("preview docs = %d; want 1", docs)
		}
		matches, err := contentServiceSearch(t, vol, "content:alpha", false)
		if err != nil {
			t.Fatalf("preview query: %v", err)
		}
		if len(matches) != 1 || !strings.HasSuffix(matches[0].Path, "a.txt") {
			t.Fatalf("preview query matches = %v; want a.txt", matches)
		}
		// The second-tier file is not indexed yet, so a query for it finds
		// nothing (and the volume is still visibly partial).
		if other, err := contentServiceSearch(t, vol, "content:beta", false); err != nil || len(other) != 0 {
			t.Fatalf("preview beta query = %v, %v; want no hits yet", other, err)
		}
	}

	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if previewQueries != 1 {
		t.Fatalf("preview published %d times; want 1", previewQueries)
	}
	h := vol.content.healthSnapshot(0)
	if h.State != contentStateReady || h.Partial || h.Docs != 2 {
		t.Fatalf("final health = %+v; want ready, complete, 2 docs", h)
	}
	for _, q := range []string{"content:alpha", "content:beta"} {
		matches, err := contentServiceSearch(t, vol, q, false)
		if err != nil || len(matches) != 1 {
			t.Fatalf("%s after build = %v, %v; want one hit", q, matches, err)
		}
	}
}
