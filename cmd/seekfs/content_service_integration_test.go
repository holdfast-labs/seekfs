package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// contentIndexFRNs sets the persisted FRN column the resolver joins against.
func contentIndexFRNs(idx *Index) {
	for i := range idx.Records {
		idx.Derived.FRNs = append(idx.Derived.FRNs, idx.Records[i].FRN)
		idx.Derived.FRNRecordIDs = append(idx.Derived.FRNRecordIDs, uint32(i))
	}
}

func contentRecordIndexForFRN(idx *Index, frn uint64) int {
	for i := range idx.Records {
		if idx.Records[i].FRN == frn {
			return i
		}
	}
	return -1
}

func TestContentNormalizationHashIsConsistent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("Alpha Needle Beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Docs) != 1 {
		t.Fatalf("docs = %d; want 1", len(idx.Docs))
	}
	base := idx.Docs[0]
	if base.ContentHash == ([contentHashLen]byte{}) {
		t.Fatal("base ContentHash must be populated")
	}
	// The delta extractor must compute the same hash for unchanged content, so a
	// touch-only write is skipped.
	if _, changed, err := contentExtractDeltaDoc(base.FRN, path, base.ContentHash, true); err != nil {
		t.Fatal(err)
	} else if changed {
		t.Fatal("unchanged content reported as changed: normalization/hash mismatch")
	}
}

func TestContentPathUnderBoundary(t *testing.T) {
	root := filepath.Join("C:", "docs")
	if !contentPathUnder(filepath.Join(root, "sub", "a.txt"), filepath.Join(root, "sub")) {
		t.Fatal("a file directly under the scope must match")
	}
	if contentPathUnder(filepath.Join(root, "submarine", "b.txt"), filepath.Join(root, "sub")) {
		t.Fatal("`sub` must not match `submarine`")
	}
	if contentPathUnder(filepath.Join(root, "other", "c.txt"), filepath.Join(root, "sub")) {
		t.Fatal("an unrelated path must not match")
	}
}

func TestContentLookupFRNColumn(t *testing.T) {
	frns := []uint64{10, 20, 30}
	if i, ok := contentLookupFRNColumn(frns, 20); !ok || i != 1 {
		t.Fatalf("lookup 20 = %d, %v", i, ok)
	}
	if _, ok := contentLookupFRNColumn(frns, 25); ok {
		t.Fatal("lookup of a missing FRN must fail")
	}
}

func TestContentAttachRequiresFRNKeyedIndex(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 5, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})
	dir := t.TempDir()
	idx := commonSearchFixture()
	contentIndexFRNs(idx)
	idx.JournalID = 5
	idx.Checkpoint = 50
	vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)
	if vol.content == nil {
		t.Fatal("content state should exist when the flag is on")
	}
	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)

	gsx := contentIndexPathForDB(vol.dbPath)

	// A path-keyed (walk) sidecar must be refused, not attached as ready.
	walk := newContentIndex()
	walk.Origin = contentOriginWalk
	walk.Docs = []contentDoc{{DocID: 0, FRN: contentPathKey("notes.txt")}}
	if err := contentSaveFile(gsx, walk); err != nil {
		t.Fatal(err)
	}
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)
	if got := vol.content.stateOf(); got != contentStateUnavailable {
		t.Fatalf("walk-keyed sidecar attached as %q; want unavailable", got)
	}

	// A USN-keyed sidecar with a watermark attaches and its resolver maps docs
	// to records.
	usn := newContentIndex()
	usn.Origin = contentOriginUSN
	usn.JournalID = 5
	usn.CheckpointUSN = 50
	usn.Docs = []contentDoc{{DocID: 0, FRN: 5, ContentType: contentClassText, ExtractorVersion: 1}, {DocID: 1, FRN: 6, ContentType: contentClassText, ExtractorVersion: 1}}
	stampContentTestScope("C:", usn)
	if err := contentSaveFile(gsx, usn); err != nil {
		t.Fatal(err)
	}
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("FRN-keyed sidecar state = %q; want ready", got)
	}
	wantID := contentRecordIndexForFRN(idx, 5)
	if id, ok := vol.content.resolver.recordID(0); !ok || int(id) != wantID {
		t.Fatalf("resolver doc0 = %d, %v; want %d", id, ok, wantID)
	}
}

func TestContentResolverRebindsAfterBaseSwap(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 5, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seekfs_c.gsi")
	idx := commonSearchFixture()
	contentIndexFRNs(idx)
	idx.JournalID = 5
	idx.Checkpoint = 50
	vol := newServiceVolumeIndex(dbPath, idx)
	s := &goSearchService{stop: make(chan struct{})}
	defer close(s.stop)

	usn := newContentIndex()
	usn.Origin = contentOriginUSN
	usn.JournalID = 5
	usn.CheckpointUSN = 50
	usn.Docs = []contentDoc{{DocID: 0, FRN: 5, ContentType: contentClassText, ExtractorVersion: 1}}
	stampContentTestScope("C:", usn)
	if err := contentSaveFile(contentIndexPathForDB(dbPath), usn); err != nil {
		t.Fatal(err)
	}
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)
	if vol.content.stateOf() != contentStateReady {
		t.Fatalf("state = %q; want ready", vol.content.stateOf())
	}

	// A base swap that renumbers the record table: FRN 5 now points at record 0
	// of a two-record index. The resolver must follow without re-extracting.
	swapped := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 5}
	swapped.Records = []CompactRecord{
		{FRN: 5, ParentFRN: 5, Parent: -1, Name: "swapped", Mode: 0, Size: 10},
		{FRN: 77, ParentFRN: 5, Parent: 0, Name: "other", Mode: 0, Size: 10},
	}
	swapped.Derived.FRNs = []uint64{5, 77}
	swapped.Derived.FRNRecordIDs = []uint32{0, 1}
	replaceServiceVolumeContents(vol, newServiceVolumeIndex(dbPath, swapped))

	if id, ok := vol.content.resolver.recordID(0); !ok || id != 0 {
		t.Fatalf("post-swap resolver doc0 = %d, %v; want 0", id, ok)
	}
}

// A base swap within the same journal generation keeps content ready and only
// rebinds the resolver; a swap whose journal id changed (a real reset) drops the
// stale content index instead of serving it.
func TestContentInvalidatedOnJournalReset(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seekfs_c.gsi")

	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 111}
	idx.Records = append(idx.Records, CompactRecord{FRN: 5, ParentFRN: 5, Parent: -1, Name: "a", Size: 10})
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(dbPath, idx)
	if vol.content == nil || vol.contentCoord == nil {
		t.Fatal("content state not initialized")
	}
	vol.content.setReady(newContentIndex(), nil, nil)
	vol.content.delta.upsert(contentDeltaDoc{FRN: 5, Path: `C:\a`, Text: []byte("old")})

	// Same journal: rebind only, content stays ready.
	same := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 111}
	same.Records = append(same.Records, CompactRecord{FRN: 5, ParentFRN: 5, Parent: -1, Name: "a", Size: 10})
	same.Derived.FRNs = []uint64{5}
	same.Derived.FRNRecordIDs = []uint32{0}
	replaceServiceVolumeContents(vol, newServiceVolumeIndex(dbPath, same))
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("same-journal swap state = %q; want ready", got)
	}

	// Different journal id: a real reset invalidates the content index.
	reset := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: 222}
	reset.Records = append(reset.Records, CompactRecord{FRN: 5, ParentFRN: 5, Parent: -1, Name: "a", Size: 10})
	reset.Derived.FRNs = []uint64{5}
	reset.Derived.FRNRecordIDs = []uint32{0}
	replaceServiceVolumeContents(vol, newServiceVolumeIndex(dbPath, reset))
	if got := vol.content.stateOf(); got != contentStateStale {
		t.Fatalf("journal-reset swap state = %q; want stale (invalidated)", got)
	}
	if vol.content.usableForQuery() {
		t.Fatal("invalidated content index must not be usable")
	}
	if vol.content.deltaView().len() != 0 {
		t.Fatal("journal reset must clear the pending delta")
	}
}

// TestContentFreshnessUSNWriteCloseLatency measures the write-close ->
// content-findable latency of the USN-driven drain/persist path so the
// p95 <= 5s target is observable. It asserts correctness (the changed content
// becomes findable after the drain) and reports timing; it deliberately does
// not assert a wall-clock bound, which would flake in CI.
func TestContentFreshnessUSNWriteCloseLatency(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "live.txt")
	idx := &Index{Source: "usn", Volume: "C:", Compact: true}
	idx.Records = append(idx.Records, CompactRecord{FRN: 9, ParentFRN: 9, Parent: -1, Name: "live.txt", Size: 10})
	contentIndexFRNs(idx)
	vol := newServiceVolumeIndex(filepath.Join(dir, "seekfs_c.gsi"), idx)
	if vol.content == nil || vol.contentCoord == nil {
		t.Fatal("content state not initialized")
	}
	cidx := newContentIndex()
	cidx.Origin = contentOriginUSN
	reader, err := openContentReader(cidx)
	if err != nil {
		t.Fatal(err)
	}
	vol.content.setReady(cidx, reader, buildContentResolver(cidx.Docs, nil, nil))
	vol.contentCoord.enableDrain()

	resolve := func(frn uint64) (string, bool) {
		if frn == 9 {
			return path, true
		}
		return "", false
	}

	const rounds = 20
	latencies := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		needle := fmt.Sprintf("freshmarker%02d", i)
		if err := os.WriteFile(path, []byte(needle+" body"), 0o644); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		vol.contentCoord.observeChanges([]usnChange{{FRN: 9, Reason: usnReasonDataOverwrite | usnReasonClose, Attr: 0}})
		if n := vol.contentCoord.processQueue(resolve); n != 1 {
			t.Fatalf("round %d: processQueue changed=%d; want 1", i, n)
		}
		matches, err := contentServiceSearch(t, vol, "content:"+needle, false)
		if err != nil {
			t.Fatalf("round %d: search: %v", i, err)
		}
		latency := time.Since(start)
		if len(matches) != 1 {
			t.Fatalf("round %d: changed content not findable after drain (%d matches)", i, len(matches))
		}
		latencies = append(latencies, latency)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[(len(latencies)*95)/100-1]
	t.Logf("USN write-close -> content-findable over %d rounds: p95=%s max=%s", rounds, p95, latencies[len(latencies)-1])
}
