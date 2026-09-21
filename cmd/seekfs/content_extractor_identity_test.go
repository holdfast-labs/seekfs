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
// version.
var contentTestBumpVersion uint16 = 1

type contentBumpExtractor struct{}

func (contentBumpExtractor) Name() string         { return "test-bump" }
func (contentBumpExtractor) Version() uint16      { return contentTestBumpVersion }
func (contentBumpExtractor) Class() uint16        { return contentClassHTML }
func (contentBumpExtractor) Extensions() []string { return []string{".bumptest"} }
func (contentBumpExtractor) Sniff([]byte) bool    { return false }

func (contentBumpExtractor) Extract(_ context.Context, _ io.ReaderAt, _ int64) (contentExtractResult, error) {
	return contentExtractResult{Text: []byte("bump needle"), Class: contentClassHTML}, nil
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
		{path: "a.bumptest", frn: 1, text: []byte("bump needle"), class: contentClassHTML, version: 1},
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

// attach must refuse a base whose extractor identity no longer matches, and
// attach one that does.
func TestContentAttachRejectsStaleExtractorIdentity(t *testing.T) {
	attachWith := func(t *testing.T, mutate func(*contentIndex)) *contentVolumeState {
		t.Helper()
		t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
		stubContentCatchUpSync(t)
		stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: 7, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 60})
		dir := t.TempDir()
		vol, gsx := contentBuildTestVolume(t, dir, 7, 50, []CompactRecord{
			{FRN: 10, ParentFRN: 1, Parent: -1, Name: "a.txt", Size: 10},
		})
		base, err := assembleContentIndex([]contentBuildDoc{
			{path: `C:\a.txt`, frn: 10, text: []byte("needle"), class: contentClassText, version: 1},
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
		if err := contentSaveFile(gsx, base); err != nil {
			t.Fatal(err)
		}
		s := contentTestService(t)
		s.attachContentForVolume(vol)
		return vol.content
	}

	if st := attachWith(t, nil); st.stateOf() != contentStateReady {
		t.Fatalf("current identity state = %q; want ready", st.stateOf())
	}
	// A bumped stored version simulates a `Version()` bump after the base was
	// built: it must go stale and refuse queries rather than serve old text.
	st := attachWith(t, func(idx *contentIndex) { idx.Docs[0].ExtractorVersion = 999 })
	if st.stateOf() != contentStateStale {
		t.Fatalf("bumped-version state = %q; want stale", st.stateOf())
	}
	if st.usableForQuery() {
		t.Fatal("a version-mismatched base must not answer content queries")
	}
	// A zero class is the pre-P1 `.gsx` shape and must also rebuild.
	st = attachWith(t, func(idx *contentIndex) { idx.Docs[0].ContentType = 0 })
	if st.stateOf() != contentStateStale {
		t.Fatalf("zero-class state = %q; want stale", st.stateOf())
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
