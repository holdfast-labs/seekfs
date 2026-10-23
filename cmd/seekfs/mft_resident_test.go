//go:build windows

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestParseMFTResidentRecord(t *testing.T) {
	tests := []struct {
		name   string
		opts   synthOpts
		frn    uint64
		reason string
		want   string
	}{
		{"resident", synthOpts{inUse: true, name: "a.txt", seq: 7, content: []byte("needle")}, 42, "", "needle"},
		{"nonresident", synthOpts{inUse: true, name: "a.txt", nonResident: true, size: 4096}, 42, contentResidentReasonNonResident, ""},
		{"compressed", synthOpts{inUse: true, name: "a.txt", content: []byte("x"), attrs: ntfsAttrCompressed}, 42, contentResidentReasonCompressed, ""},
		{"encrypted", synthOpts{inUse: true, name: "a.txt", content: []byte("x"), attrs: ntfsAttrEncrypted}, 42, contentResidentReasonEncrypted, ""},
		{"sparse", synthOpts{inUse: true, name: "a.txt", content: []byte("x"), attrs: ntfsAttrSparse}, 42, contentResidentReasonSparse, ""},
		{"reparse", synthOpts{inUse: true, name: "a.txt", content: []byte("x"), attrs: ntfsAttrReparsePoint}, 42, contentResidentReasonReparse, ""},
		{"attribute list", synthOpts{inUse: true, name: "a.txt", content: []byte("x"), attrList: true}, 42, contentResidentReasonAttributeList, ""},
		{"extension record", synthOpts{inUse: true, name: "a.txt", content: []byte("x"), baseRef: (uint64(1) << 48) | 43}, 42, contentResidentReasonExtension, ""},
		{"not in use", synthOpts{name: "a.txt", content: []byte("x")}, 42, contentResidentReasonNotInUse, ""},
		{"directory", synthOpts{inUse: true, isDir: true, name: "d"}, 42, contentResidentReasonDirectory, ""},
	}
	for _, tt := range tests {
		rec := synthMFTRecord(tt.opts)
		got, reason, ok := parseMFTResidentRecord(rec, tt.frn, 512)
		if tt.reason != "" {
			if ok || reason != tt.reason {
				t.Errorf("%s: ok=%v reason=%q; want fallback %q", tt.name, ok, reason, tt.reason)
			}
			continue
		}
		if !ok || reason != "" {
			t.Errorf("%s: ok=%v reason=%q; want resident data", tt.name, ok, reason)
			continue
		}
		if string(got.data) != tt.want {
			t.Errorf("%s: data=%q; want %q", tt.name, got.data, tt.want)
		}
		if got.seq != tt.opts.seq {
			t.Errorf("%s: seq=%d; want %d", tt.name, got.seq, tt.opts.seq)
		}
	}
}

func residentFSCTLOutput(rec []byte, ref uint64) ([]byte, uint32) {
	out := make([]byte, 12+len(rec)+8)
	binary.LittleEndian.PutUint64(out[0:8], ref)
	binary.LittleEndian.PutUint32(out[8:12], uint32(len(rec)))
	copy(out[12:], rec)
	return out, uint32(12 + len(rec))
}

func TestParseNTFSFileRecordOutputValidation(t *testing.T) {
	const frn = 42
	const seq = 7
	rec := synthMFTRecord(synthOpts{inUse: true, name: "a.txt", seq: seq, content: []byte("needle")})
	out, ret := residentFSCTLOutput(rec, (uint64(seq)<<48)|frn)
	if data, reason, ok := parseNTFSFileRecordOutput(out, ret, frn, seq, 6, 512); !ok || string(data) != "needle" {
		t.Fatalf("valid output = %q reason=%q ok=%v", data, reason, ok)
	}
	if _, reason, ok := parseNTFSFileRecordOutput(out, ret, frn, seq+1, 6, 512); ok || reason != contentResidentReasonSequence {
		t.Fatalf("sequence mismatch = reason=%q ok=%v", reason, ok)
	}
	if _, reason, ok := parseNTFSFileRecordOutput(out, ret, frn+1, seq, 6, 512); ok || reason != contentResidentReasonSequence {
		t.Fatalf("record mismatch = reason=%q ok=%v", reason, ok)
	}
	if _, reason, ok := parseNTFSFileRecordOutput(out, ret, frn, seq, 5, 512); ok || reason != contentResidentReasonSize {
		t.Fatalf("size mismatch = reason=%q ok=%v", reason, ok)
	}
	if _, reason, ok := parseNTFSFileRecordOutput(out, 8, frn, seq, 6, 512); ok || reason != contentResidentReasonParse {
		t.Fatalf("short ret = reason=%q ok=%v", reason, ok)
	}
}

// fakeResidentSource serves synthetic resident bytes and records attempts.
type fakeResidentSource struct {
	stats    contentResidentStats
	texts    map[uint64][]byte
	reasons  map[uint64]string
	attempts atomic.Int64
	closed   atomic.Bool
}

func (f *fakeResidentSource) ResidentText(frn uint64, _ uint16, _ int64) ([]byte, string, bool) {
	f.attempts.Add(1)
	if text, ok := f.texts[frn]; ok {
		f.stats.hit()
		return append([]byte(nil), text...), "", true
	}
	reason := f.reasons[frn]
	if reason == "" {
		reason = contentResidentReasonNonResident
	}
	f.stats.fallback(reason)
	return nil, reason, false
}

func (f *fakeResidentSource) Counters() contentResidentCounters { return f.stats.snapshot() }
func (f *fakeResidentSource) Close()                            { f.closed.Store(true) }

func useFakeResidentSource(t *testing.T, src *fakeResidentSource) {
	t.Helper()
	previous := contentOpenResidentSource
	contentOpenResidentSource = func(string) (contentResidentSource, error) { return src, nil }
	t.Cleanup(func() { contentOpenResidentSource = previous })
}

func countExtractOpens(t *testing.T) *atomic.Int32 {
	t.Helper()
	restore := contentOpenForExtract
	opens := &atomic.Int32{}
	contentOpenForExtract = func(path string) (*os.File, error) {
		opens.Add(1)
		return restore(path)
	}
	t.Cleanup(func() { contentOpenForExtract = restore })
	return opens
}

func TestContentBuildUsesResidentBytesWithoutOpening(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	t.Setenv("SEEKFS_CONTENT_MFT_RESIDENT", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	body := []byte("alpha resident")
	if err := os.WriteFile(filepath.Join(dir, "tiny.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(0xE1)
	const cp = int64(60)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, _ := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 10, Parent: -1, Name: "tiny.txt", Size: int64(len(body))},
	})
	src := &fakeResidentSource{texts: map[uint64][]byte{10: body}}
	useFakeResidentSource(t, src)
	opens := countExtractOpens(t)

	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("build state=%q; want ready", got)
	}
	if opens.Load() != 0 {
		t.Fatalf("opener called %d times; resident file must not open", opens.Load())
	}
	if !src.closed.Load() {
		t.Fatal("resident source was not closed at build end")
	}
	h := vol.content.healthSnapshot(0)
	if h.ResidentHits != 1 || h.ResidentFallbacks != 0 {
		t.Fatalf("resident health = hits %d fallbacks %d; want 1/0", h.ResidentHits, h.ResidentFallbacks)
	}
	matches, err := contentServiceSearch(t, vol, "content:alpha", false)
	if err != nil || len(matches) != 1 {
		t.Fatalf("resident query = %v, %v; want one hit", matches, err)
	}
}

func TestContentBuildResidentFallbackOpens(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	t.Setenv("SEEKFS_CONTENT_MFT_RESIDENT", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	body := []byte("beta opened")
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(0xE2)
	const cp = int64(61)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, _ := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 20, Parent: -1, Name: "small.txt", Size: int64(len(body))},
	})
	src := &fakeResidentSource{reasons: map[uint64]string{20: contentResidentReasonNonResident}}
	useFakeResidentSource(t, src)
	opens := countExtractOpens(t)

	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if opens.Load() != 1 {
		t.Fatalf("opener called %d times; fallback must open once", opens.Load())
	}
	h := vol.content.healthSnapshot(0)
	if h.ResidentHits != 0 || h.ResidentFallbacks != 1 || h.ResidentFallbackReasons[contentResidentReasonNonResident] != 1 {
		t.Fatalf("resident health = %+v; want one nonresident fallback", h)
	}
	matches, err := contentServiceSearch(t, vol, "content:beta", false)
	if err != nil || len(matches) != 1 {
		t.Fatalf("fallback query = %v, %v; want one hit", matches, err)
	}
}

func TestContentBuildOpensWhenResidentDisabled(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	t.Setenv("SEEKFS_CONTENT_MFT_RESIDENT", "0")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)

	dir := t.TempDir()
	body := []byte("gamma opened")
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	const journal = uint64(0xE3)
	const cp = int64(62)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: cp})
	vol, _ := contentBuildTestVolume(t, dir, journal, uint64(cp), []CompactRecord{
		{FRN: 30, Parent: -1, Name: "small.txt", Size: int64(len(body))},
	})
	src := &fakeResidentSource{texts: map[uint64][]byte{30: body}}
	useFakeResidentSource(t, src)
	opens := countExtractOpens(t)

	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if opens.Load() != 1 {
		t.Fatalf("opener called %d times; disabled resident path must open once", opens.Load())
	}
	if src.attempts.Load() != 0 {
		t.Fatalf("resident source attempted %d reads while disabled", src.attempts.Load())
	}
	if h := vol.content.healthSnapshot(0); h.ResidentHits != 0 || h.ResidentFallbacks != 0 {
		t.Fatalf("disabled resident health = %+v; want zero", h)
	}
}
