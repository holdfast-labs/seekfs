package main

// P1-P3 prerequisites for the file-format work: per-doc extractor identity
// (ContentType/ExtractorVersion), the invalidation rule that rebuilds on a
// mismatch, and the offline builder's panic/timeout isolation.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// contentBumpExtractor is a test extractor whose Version() is mutable, so a test
// can simulate an extractor version bump against an index built at the old
// version. It claims contentClassLegacy, a class no production extractor uses, so
// it cannot collide with a real extractor's version in the registry.
var contentTestBumpVersion uint16 = 1

type contentBumpExtractor struct{}

func (contentBumpExtractor) Name() string         { return "test-bump" }
func (contentBumpExtractor) Version() uint16      { return contentTestBumpVersion }
func (contentBumpExtractor) Class() uint16        { return contentClassLegacy }
func (contentBumpExtractor) Extensions() []string { return []string{".bumptest"} }
func (contentBumpExtractor) Sniff([]byte) bool    { return false }

func (contentBumpExtractor) Extract(_ context.Context, _ io.ReaderAt, _ int64) (contentExtractResult, error) {
	return contentExtractResult{Text: []byte("bump needle"), Class: contentClassLegacy}, nil
}

type contentPanicExtractor struct{}

func (contentPanicExtractor) Name() string         { return "test-panic" }
func (contentPanicExtractor) Version() uint16      { return 1 }
func (contentPanicExtractor) Class() uint16        { return contentClassText }
func (contentPanicExtractor) Extensions() []string { return []string{".panic"} }
func (contentPanicExtractor) Sniff([]byte) bool    { return false }

func (contentPanicExtractor) Extract(context.Context, io.ReaderAt, int64) (contentExtractResult, error) {
	panic("content extractor test panic")
}

// contentSpinExtractor ignores its context and blocks forever, so only a
// boundary-enforced deadline can make the build return.
type contentSpinExtractor struct{}

func (contentSpinExtractor) Name() string         { return "test-spin" }
func (contentSpinExtractor) Version() uint16      { return 1 }
func (contentSpinExtractor) Class() uint16        { return contentClassText }
func (contentSpinExtractor) Extensions() []string { return []string{".spin"} }
func (contentSpinExtractor) Sniff([]byte) bool    { return false }

func (contentSpinExtractor) Extract(context.Context, io.ReaderAt, int64) (contentExtractResult, error) {
	select {}
}

// A build stamps each doc with the producing extractor's class and version, and
// the fields round-trip through the `.gsx` doc table.
func TestContentBuildStampsExtractorIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("needle text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "doc.pdf"), contentPDFTestDoc([]byte("BT /F1 12 Tf (needle pdf) Tj ET"), false), 0o644); err != nil {
		t.Fatal(err)
	}
	docx := contentOOXMLTestZip(t, map[string]string{
		"word/document.xml": `<w:document xmlns:w="http://x"><w:body><w:p><w:r><w:t>needle docx</w:t></w:r></w:p></w:body></w:document>`,
	})
	if err := os.WriteFile(filepath.Join(dir, "doc.docx"), docx, 0o644); err != nil {
		t.Fatal(err)
	}

	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint16]uint16{
		contentClassText:  contentTextExtractor{}.Version(),
		contentClassPDF:   contentPDFExtractor{}.Version(),
		contentClassOOXML: contentOOXMLExtractor{}.Version(),
	}
	if len(idx.Docs) != len(want) {
		t.Fatalf("built %d docs; want %d", len(idx.Docs), len(want))
	}
	for i := range idx.Docs {
		d := idx.Docs[i]
		v, ok := want[d.ContentType]
		if !ok {
			t.Fatalf("doc %d has unclaimed class %d", i, d.ContentType)
		}
		if d.ExtractorVersion != v {
			t.Fatalf("doc %d class %d version = %d; want %d", i, d.ContentType, d.ExtractorVersion, v)
		}
	}

	got, err := contentIndexDecode(contentIndexEncode(idx))
	if err != nil {
		t.Fatal(err)
	}
	for i := range idx.Docs {
		if got.Docs[i].ContentType != idx.Docs[i].ContentType || got.Docs[i].ExtractorVersion != idx.Docs[i].ExtractorVersion {
			t.Fatalf("doc %d identity did not round-trip: got %+v want %+v", i, got.Docs[i], idx.Docs[i])
		}
	}
}

// contentIndexExtractorMismatch flags a zero class, an unregistered class, or a
// version that no longer matches the registry.
func TestContentIndexExtractorMismatch(t *testing.T) {
	textVer := contentTextExtractor{}.Version()
	idx := func(d contentDoc) *contentIndex {
		i := newContentIndex()
		i.Docs = []contentDoc{d}
		return i
	}
	if reason, stale := contentIndexExtractorMismatch(idx(contentDoc{ContentType: contentClassText, ExtractorVersion: textVer})); stale {
		t.Fatalf("current class/version reported stale: %s", reason)
	}
	if _, stale := contentIndexExtractorMismatch(idx(contentDoc{ContentType: 0, ExtractorVersion: textVer})); !stale {
		t.Fatal("a zero content type (old .gsx) must be stale")
	}
	if _, stale := contentIndexExtractorMismatch(idx(contentDoc{ContentType: contentClassText, ExtractorVersion: textVer + 1})); !stale {
		t.Fatal("a bumped extractor version must be stale")
	}
	if _, stale := contentIndexExtractorMismatch(idx(contentDoc{ContentType: 9999, ExtractorVersion: 1})); !stale {
		t.Fatal("an unregistered class must be stale")
	}
	if _, stale := contentIndexExtractorMismatch(nil); !stale {
		t.Fatal("a nil index must be stale")
	}
}

// Every registered extractor must declare a distinct class: invalidation
// resolves a class to the first matching extractor, so two extractors sharing a
// class would make a Version() bump on one re-extract the other's docs with the
// other extractor's unchanged version, and the mismatch would never clear.
func TestContentExtractorClassesAreDistinct(t *testing.T) {
	seen := make(map[uint16]string, len(contentExtractors))
	for _, e := range contentExtractors {
		if prev, dup := seen[e.Class()]; dup {
			t.Fatalf("class %d declared by both %q and %q", e.Class(), prev, e.Name())
		}
		seen[e.Class()] = e.Name()
		if v, ok := contentExtractorVersionForClass(e.Class()); !ok || v != e.Version() {
			t.Fatalf("class %d resolves to (%d,%v); want %q's version %d", e.Class(), v, ok, e.Name(), e.Version())
		}
	}
}

// A registered extractor's Version() bump must make a previously-built index
// stale, and an unchanged version must not.
func TestContentExtractorVersionBumpInvalidates(t *testing.T) {
	restore := contentExtractors
	contentExtractors = append(append([]contentExtractor(nil), contentExtractors...), contentBumpExtractor{})
	t.Cleanup(func() { contentExtractors = restore })
	restoreVer := contentTestBumpVersion
	t.Cleanup(func() { contentTestBumpVersion = restoreVer })
	contentTestBumpVersion = 1

	build, err := assembleContentIndex([]contentBuildDoc{
		{path: "a.bumptest", frn: 1, text: []byte("bump needle"), class: contentClassLegacy, version: 1},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, stale := contentIndexExtractorMismatch(build); stale {
		t.Fatal("an index built at the current version must not be stale")
	}

	contentTestBumpVersion = 2
	reason, stale := contentIndexExtractorMismatch(build)
	if !stale {
		t.Fatal("a Version() bump must invalidate the index")
	}
	t.Logf("bump reason: %s", reason)
}

// attach must refresh a registered-class version mismatch in place (usable,
// degraded refresh signal) and fall back to a rebuild for an unregistered class
// or an old class==0 doc that can no longer be re-extracted.
func TestContentAttachExtractorIdentityRefresh(t *testing.T) {
	attachWith := func(t *testing.T, name string, mutate func(*contentIndex)) *serviceVolumeIndex {
		t.Helper()
		t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
		stubContentCatchUpSync(t)
		stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})
		dir := t.TempDir()
		vol, gsx := contentBuildTestVolume(t, dir, 7, 50, []CompactRecord{
			{FRN: 10, ParentFRN: 1, Parent: -1, Name: name, Size: 10},
		})
		base, err := assembleContentIndex([]contentBuildDoc{
			{path: `C:\` + name, frn: 10, text: []byte("needle"), class: contentClassText, version: 1},
		}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		base.Origin = contentOriginUSN
		base.JournalID = 7
		base.CheckpointUSN = 50
		if mutate != nil {
			mutate(base)
		}
		stampContentTestScope(vol.volume, base)
		if err := contentSaveFile(gsx, base); err != nil {
			t.Fatal(err)
		}
		s := contentTestService(t)
		s.attachContentForVolume(vol)
		releaseVolumeContentOnCleanup(t, vol)
		return vol
	}

	// A current identity attaches ready with no refresh.
	vol := attachWith(t, "a.txt", nil)
	if st := vol.content.stateOf(); st != contentStateReady || vol.content.healthSnapshot(0).ExtractorRefresh {
		t.Fatalf("current identity state = %q refresh=%v; want ready/none", st, vol.content.healthSnapshot(0).ExtractorRefresh)
	}

	// A bumped stored version simulates a `Version()` bump after the base was
	// built: the doc is re-extracted in place. The volume stays ready and usable
	// (the old text is a valid, older extraction) but surfaced degraded until the
	// refresh folds.
	vol = attachWith(t, "a.txt", func(idx *contentIndex) { idx.Docs[0].ExtractorVersion = 999 })
	if st := vol.content.stateOf(); st != contentStateReady {
		t.Fatalf("bumped-version state = %q; want ready (refresh in place)", st)
	}
	if !vol.content.usableForQuery() {
		t.Fatal("a version-mismatched base must stay usable during a refresh")
	}
	h := vol.content.healthSnapshot(0)
	if !h.ExtractorRefresh || h.StaleExtractorDocs != 1 || h.State != contentStateDegraded {
		t.Fatalf("bumped-version health = %+v; want degraded refresh of 1 doc", h)
	}
	if got := vol.contentCoord.queued(); got != 1 {
		t.Fatalf("bumped-version queued = %d; want the stale FRN enqueued", got)
	}

	// A zero class is the pre-P1 `.gsx` shape; a `.txt` extension is still
	// extractable, so it refreshes in place too.
	vol = attachWith(t, "a.txt", func(idx *contentIndex) { idx.Docs[0].ContentType = 0 })
	if st := vol.content.stateOf(); st != contentStateReady {
		t.Fatalf("zero-class state = %q; want ready (refresh in place)", st)
	}
	if h := vol.content.healthSnapshot(0); !h.ExtractorRefresh || h.StaleExtractorDocs != 1 {
		t.Fatalf("zero-class health = %+v; want degraded refresh of 1 doc", h)
	}

	// A zero class whose extension nothing extracts cannot be refreshed: rebuild.
	vol = attachWith(t, "a.zzzunknown", func(idx *contentIndex) { idx.Docs[0].ContentType = 0 })
	if st := vol.content.stateOf(); st != contentStateStale {
		t.Fatalf("unmapped zero-class state = %q; want stale (rebuild)", st)
	}

	// An unregistered class cannot be refreshed: rebuild.
	vol = attachWith(t, "a.txt", func(idx *contentIndex) { idx.Docs[0].ContentType = 9999 })
	if st := vol.content.stateOf(); st != contentStateStale {
		t.Fatalf("unregistered-class state = %q; want stale (rebuild)", st)
	}
	if vol.content.usableForQuery() {
		t.Fatal("an unregistered-class base must not answer content queries")
	}
}

// A registered extractor's Version() bump re-extracts only that class's docs in
// place: the other class's docs are untouched, the volume stays usable (serving
// the older text) with a degraded refresh signal, and a fold publishes the
// refreshed base and clears the signal.
func TestContentExtractorRefreshReextractsOnlyStaleClass(t *testing.T) {
	restore := contentExtractors
	contentExtractors = append(append([]contentExtractor(nil), contentExtractors...), contentBumpExtractor{})
	t.Cleanup(func() { contentExtractors = restore })
	restoreVer := contentTestBumpVersion
	t.Cleanup(func() { contentTestBumpVersion = restoreVer })
	contentTestBumpVersion = 2 // registry bumped after the base was built at 1
	textVer := contentTextExtractor{}.Version()

	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 1_000_000})

	dir := t.TempDir()
	htmlPath := filepath.Join(dir, "a.bumptest")
	txtPath := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(htmlPath, []byte("whatever"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txtPath, []byte("keep this text"), 0o644); err != nil {
		t.Fatal(err)
	}

	const journal = uint64(7)
	const baseCP = int64(100)
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, JournalID: journal, Checkpoint: baseCP}
	idx.Records = []CompactRecord{
		{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.bumptest", Size: 8},
		{FRN: 20, ParentFRN: 1, Parent: -1, Name: "b.txt", Size: 14},
	}
	contentIndexFRNs(idx)
	dbPath := filepath.Join(dir, "seekfs_c.gsi")
	base, err := assembleContentIndex([]contentBuildDoc{
		{path: `C:\a.bumptest`, frn: 10, text: []byte("old html text"), class: contentClassLegacy, version: 1},
		{path: `C:\b.txt`, frn: 20, text: []byte("keep this text"), class: contentClassText, version: textVer},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = journal
	base.CheckpointUSN = uint64(baseCP)
	stampContentTestScope(idx.Volume, base)
	if err := contentSaveFile(contentIndexPathForDB(dbPath), base); err != nil {
		t.Fatal(err)
	}

	vol := newServiceVolumeIndex(dbPath, idx)
	s := &goSearchService{stop: make(chan struct{})}
	t.Cleanup(func() { close(s.stop) })
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)

	// The stale class is refreshed in place: ready and usable, degraded signal.
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state = %q; want ready", got)
	}
	if !vol.content.usableForQuery() {
		t.Fatal("volume must stay usable during a refresh")
	}
	if h := vol.content.healthSnapshot(0); !h.ExtractorRefresh || h.StaleExtractorDocs != 1 || h.State != contentStateDegraded {
		t.Fatalf("refresh health = %+v; want degraded refresh of 1 doc", h)
	}
	// The old text is still served during the refresh (not silently dropped).
	if hits, _ := contentServiceSearch(t, vol, "content:old", false); len(hits) != 1 {
		t.Fatalf("old text not served during refresh: %v", hits)
	}

	resolve := func(frn uint64) (string, bool) {
		switch frn {
		case 10:
			return htmlPath, true
		case 20:
			return txtPath, true
		}
		return "", false
	}
	if n := vol.contentCoord.processQueue(resolve); n != 1 {
		t.Fatalf("refresh re-extracted %d docs; want only the stale class (1)", n)
	}
	// The degraded signal survives the extraction until the fold publishes.
	if h := vol.content.healthSnapshot(0); !h.ExtractorRefresh {
		t.Fatalf("refresh signal cleared before the fold: %+v", h)
	}

	// The refresh forces a fold even though the delta is far below its trigger.
	s.maybeFoldContentDelta(vol)

	reloaded, err := contentLoadFile(contentIndexPathForDB(dbPath))
	releaseContentIndexOnCleanup(t, reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Docs) != 2 {
		t.Fatalf("folded base docs = %d; want 2", len(reloaded.Docs))
	}
	for _, d := range reloaded.Docs {
		switch d.FRN {
		case 10:
			if d.ContentType != contentClassLegacy || d.ExtractorVersion != 2 {
				t.Fatalf("refreshed doc identity = (%d,%d); want legacy/2", d.ContentType, d.ExtractorVersion)
			}
		case 20:
			if d.ContentType != contentClassText || d.ExtractorVersion != textVer {
				t.Fatalf("untouched doc identity = (%d,%d); want text/%d", d.ContentType, d.ExtractorVersion, textVer)
			}
		default:
			t.Fatalf("unexpected doc FRN %d", d.FRN)
		}
	}
	if hits, _ := contentServiceSearch(t, vol, "content:bump", false); len(hits) != 1 {
		t.Fatalf("refreshed text not searchable after fold: %v", hits)
	}
	if h := vol.content.healthSnapshot(0); h.ExtractorRefresh || h.StaleExtractorDocs != 0 || h.State != contentStateReady {
		t.Fatalf("post-fold health = %+v; want ready with no refresh", h)
	}
}

// A stale set larger than the refresh cap falls back to the full rebuild rather
// than enqueuing an unbounded amount of re-extraction work.
func TestContentExtractorRefreshOverThresholdRebuilds(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})

	restoreMax := contentRefreshMaxDocs
	contentRefreshMaxDocs = 2
	t.Cleanup(func() { contentRefreshMaxDocs = restoreMax })

	dir := t.TempDir()
	records := []CompactRecord{
		{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10},
		{FRN: 20, ParentFRN: 1, Parent: -1, Name: "b.txt", Size: 10},
		{FRN: 30, ParentFRN: 1, Parent: -1, Name: "c.txt", Size: 10},
	}
	vol, gsx := contentBuildTestVolume(t, dir, 7, 50, records)
	build := make([]contentBuildDoc, 0, len(records))
	for _, rec := range records {
		build = append(build, contentBuildDoc{path: `C:\` + rec.Name, frn: rec.FRN, text: []byte("needle"), class: contentClassText, version: 999})
	}
	base, err := assembleContentIndex(build, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = 7
	base.CheckpointUSN = 50
	stampContentTestScope(vol.volume, base)
	if err := contentSaveFile(gsx, base); err != nil {
		t.Fatal(err)
	}

	s := contentTestService(t)
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)
	if got := vol.content.stateOf(); got != contentStateStale {
		t.Fatalf("state = %q; want stale (over-cap refresh falls back to rebuild)", got)
	}
	if vol.content.usableForQuery() {
		t.Fatal("an over-cap stale base must not answer content queries")
	}
}

// A stale doc whose file was deleted is tombstoned by the drain and dropped by
// the fold, never served at its old-version text.
func TestContentExtractorRefreshDeletedDocTombstoned(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 1_000_000})

	dir := t.TempDir()
	vol, gsx := contentBuildTestVolume(t, dir, 7, 100, []CompactRecord{
		{FRN: 10, ParentFRN: 1, Parent: -1, Name: "gone.txt", Size: 10},
	})
	base, err := assembleContentIndex([]contentBuildDoc{
		{path: `C:\gone.txt`, frn: 10, text: []byte("vanished needle"), class: contentClassText, version: 999},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = 7
	base.CheckpointUSN = 100
	stampContentTestScope(vol.volume, base)
	if err := contentSaveFile(gsx, base); err != nil {
		t.Fatal(err)
	}

	s := contentTestService(t)
	s.attachContentForVolume(vol)
	releaseVolumeContentOnCleanup(t, vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("state = %q; want ready refresh", got)
	}
	if hits, _ := contentServiceSearch(t, vol, "content:vanished", false); len(hits) != 1 {
		t.Fatalf("stale text not served before the refresh: %v", hits)
	}

	// The FRN no longer resolves: the drain tombstones it.
	if n := vol.contentCoord.processQueue(func(uint64) (string, bool) { return "", false }); n != 0 {
		t.Fatalf("deleted FRN extracted %d docs; want 0", n)
	}
	s.maybeFoldContentDelta(vol)

	reloaded, err := contentLoadFile(gsx)
	releaseContentIndexOnCleanup(t, reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Docs) != 0 {
		t.Fatalf("folded base docs = %d; want the deleted doc dropped", len(reloaded.Docs))
	}
	if hits, _ := contentServiceSearch(t, vol, "content:vanished", false); len(hits) != 0 {
		t.Fatalf("deleted doc still served after fold: %v", hits)
	}
	if h := vol.content.healthSnapshot(0); h.ExtractorRefresh || h.State != contentStateReady {
		t.Fatalf("post-fold health = %+v; want ready with no refresh", h)
	}
}

// The offline builder must survive a panicking extractor (skip, no crash) and a
// context-ignoring hanging extractor (deadline, no wedge).
func TestContentOfflineBuildPanicAndTimeoutIsolated(t *testing.T) {
	restoreTimeout := contentExtractDocTimeout
	contentExtractDocTimeout = 50 * time.Millisecond
	t.Cleanup(func() { contentExtractDocTimeout = restoreTimeout })
	restoreExtractors := contentExtractors
	contentExtractors = append(append([]contentExtractor(nil), contentExtractors...), contentPanicExtractor{}, contentSpinExtractor{})
	t.Cleanup(func() { contentExtractors = restoreExtractors })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.panic"), []byte("boom"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.spin"), []byte("hang"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}

	type result struct {
		idx *contentIndex
		err error
	}
	done := make(chan result, 1)
	go func() {
		idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
		done <- result{idx, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if len(r.idx.Docs) != 1 {
			t.Fatalf("built %d docs; want only c.txt", len(r.idx.Docs))
		}
		d := r.idx.Docs[0]
		wantVer := contentTextExtractor{}.Version()
		if d.ContentType != contentClassText || d.ExtractorVersion != wantVer {
			t.Fatalf("surviving doc identity = (%d,%d); want text/%d", d.ContentType, d.ExtractorVersion, wantVer)
		}
		if r.idx.Policy.Skipped < 2 {
			t.Fatalf("skipped = %d; want the panic and the hang both skipped", r.idx.Policy.Skipped)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("offline build wedged on a panicking or hanging extractor")
	}
}
