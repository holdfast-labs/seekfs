package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// postingFoldFile is one file in the posting-fold fixture.
type postingFoldFile struct {
	frn    uint64
	folder string
	name   string
	text   string
}

type postingFoldFixture struct {
	s       *goSearchService
	vol     *serviceVolumeIndex
	gsx     string
	dir     string
	paths   map[uint64]string
	journal uint64
	cp      uint64
}

func postingFoldText(folder string, n int) string {
	return fmt.Sprintf("shared token%d unique%d func File%d() {}\n", n, n, n)
}

func newPostingFoldFixture(t *testing.T, files []postingFoldFile) *postingFoldFixture {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	restoreTarget := contentShardTargetBytes
	contentShardTargetBytes = 60
	t.Cleanup(func() { contentShardTargetBytes = restoreTarget })

	const journal = uint64(0xABC)
	const cp = uint64(120)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 1_000_000})

	dir := t.TempDir()
	d := uint32(os.ModeDir)
	folders := []string{"a", "b", "c"}
	folderFRN := map[string]uint64{"a": 2, "b": 3, "c": 4}
	folderIndex := map[string]int32{"a": 0, "b": 1, "c": 2}
	records := make([]CompactRecord, 0, len(folders)+len(files))
	for _, f := range folders {
		records = append(records, CompactRecord{FRN: folderFRN[f], ParentFRN: 1, Parent: -1, Mode: d, Name: f})
	}
	paths := make(map[uint64]string, len(files))
	for _, f := range files {
		p := filepath.Join(dir, f.folder, f.name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.text), 0o644); err != nil {
			t.Fatal(err)
		}
		paths[f.frn] = p
		records = append(records, CompactRecord{
			FRN: f.frn, ParentFRN: folderFRN[f.folder], Parent: folderIndex[f.folder],
			Name: f.name, Size: int64(len(f.text)),
		})
	}
	vol, gsx := contentBuildTestVolume(t, dir, journal, cp, records)
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("posting-fold build state = %q; want ready (%+v)", got, vol.content.healthSnapshot(0))
	}
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	return &postingFoldFixture{s: s, vol: vol, gsx: gsx, dir: dir, paths: paths, journal: journal, cp: cp}
}

func (f *postingFoldFixture) refreshRecords() {
	f.vol.index.Derived.FRNs = nil
	f.vol.index.Derived.FRNRecordIDs = nil
	contentIndexFRNs(f.vol.index)
}

func (f *postingFoldFixture) recordIndex(frn uint64) int {
	for i := range f.vol.index.Records {
		if f.vol.index.Records[i].FRN == frn {
			return i
		}
	}
	return -1
}

func (f *postingFoldFixture) folderRecordIndex(folder string) int {
	for i := range f.vol.index.Records {
		if f.vol.index.Records[i].Mode&uint32(os.ModeDir) != 0 && f.vol.index.Records[i].Name == folder {
			return i
		}
	}
	return -1
}

func (f *postingFoldFixture) churn(t *testing.T, frn uint64, usn int64, text string) {
	t.Helper()
	if err := os.WriteFile(f.paths[frn], []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if i := f.recordIndex(frn); i >= 0 {
		f.vol.index.Records[i].Size = int64(len(text))
	}
	f.vol.contentCoord.observeChanges([]usnChange{{FRN: frn, USN: usn, Reason: usnReasonDataOverwrite | usnReasonClose}})
	f.vol.contentCoord.processQueue(func(frn uint64) (string, bool) {
		p, ok := f.paths[frn]
		return p, ok
	})
}

func (f *postingFoldFixture) add(t *testing.T, frn uint64, folder, name, text string, usn int64) {
	t.Helper()
	p := filepath.Join(f.dir, folder, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	f.paths[frn] = p
	fi := f.folderRecordIndex(folder)
	if fi < 0 {
		t.Fatalf("folder %q has no record", folder)
	}
	f.vol.index.Records = append(f.vol.index.Records, CompactRecord{
		FRN: frn, ParentFRN: f.vol.index.Records[fi].FRN, Parent: int32(fi),
		Name: name, Size: int64(len(text)),
	})
	// The record index is FRN-sorted; keep it so the content FRN column
	// binary search resolves the new record. Folders have lower FRNs and stay
	// first, so their Parent indexes remain valid.
	sort.SliceStable(f.vol.index.Records, func(i, j int) bool {
		return f.vol.index.Records[i].FRN < f.vol.index.Records[j].FRN
	})
	f.refreshRecords()
	f.vol.contentCoord.observeChanges([]usnChange{{FRN: frn, USN: usn, Reason: usnReasonFileCreate | usnReasonClose}})
	f.vol.contentCoord.processQueue(func(frn uint64) (string, bool) {
		p, ok := f.paths[frn]
		return p, ok
	})
}

func (f *postingFoldFixture) remove(t *testing.T, frn uint64, usn int64) {
	t.Helper()
	p := f.paths[frn]
	delete(f.paths, frn)
	kept := f.vol.index.Records[:0]
	for _, r := range f.vol.index.Records {
		if r.FRN != frn {
			kept = append(kept, r)
		}
	}
	f.vol.index.Records = kept
	f.refreshRecords()
	_ = os.Remove(p)
	f.vol.contentCoord.observeChanges([]usnChange{{FRN: frn, USN: usn, Reason: usnReasonFileDelete}})
}

func (f *postingFoldFixture) rename(t *testing.T, frn uint64, folder, name, text string, usn int64) {
	t.Helper()
	np := filepath.Join(f.dir, folder, name)
	if err := os.MkdirAll(filepath.Dir(np), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.paths[frn], np); err != nil {
		t.Fatal(err)
	}
	f.paths[frn] = np
	if i := f.recordIndex(frn); i >= 0 {
		fi := f.folderRecordIndex(folder)
		if fi < 0 {
			t.Fatalf("folder %q has no record", folder)
		}
		f.vol.index.Records[i].Name = name
		f.vol.index.Records[i].Parent = int32(fi)
		f.vol.index.Records[i].ParentFRN = f.vol.index.Records[fi].FRN
		f.vol.index.Records[i].Size = int64(len(text))
	}
	f.refreshRecords()
	f.churn(t, frn, usn, text)
}

func (f *postingFoldFixture) rebuiltVolume(t *testing.T) (*serviceVolumeIndex, *goSearchService) {
	t.Helper()
	vol, _ := contentBuildTestVolume(t, f.dir, f.journal, f.cp, append([]CompactRecord(nil), f.vol.index.Records...))
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("rebuilt volume state = %q; want ready (%+v)", got, vol.content.healthSnapshot(0))
	}
	s.volumes = []*serviceVolumeIndex{vol}
	s.indexes = []*Index{vol.index}
	return vol, s
}

func (f *postingFoldFixture) shard(t *testing.T, file string) *contentShard {
	t.Helper()
	reader := f.vol.content.readerView()
	if reader == nil || reader.idx.shards == nil {
		t.Fatalf("volume is not sharded")
	}
	for _, sh := range reader.idx.shards {
		if sh.info.File == file {
			return sh
		}
	}
	return nil
}

func readSectionFile(t *testing.T, path string) []byte {
	t.Helper()
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodePostingSectionForTest(t *testing.T, sec []byte) map[string][]uint32 {
	t.Helper()
	if len(sec) == 0 {
		return nil
	}
	idx, err := decodeContentPostingIndex(sec)
	if err != nil {
		t.Fatalf("decode postings: %v", err)
	}
	out := make(map[string][]uint32)
	for i := 0; i < idx.entryCount(); i++ {
		e := idx.entryAt(i)
		key := string(e.key[:])
		next, found := idx.postings(key)
		if !found {
			continue
		}
		for {
			id, _, ok := next()
			if !ok {
				break
			}
			out[key] = append(out[key], id)
		}
	}
	return out
}

func decodePrunedForTest(t *testing.T, sec []byte) map[uint32]struct{} {
	t.Helper()
	if len(sec) == 0 {
		return nil
	}
	set, err := decodeContentPrunedGrams(sec)
	if err != nil {
		t.Fatalf("decode pruned grams: %v", err)
	}
	return set
}

func assertShardMatchesRebuild(t *testing.T, f *postingFoldFixture, sh *contentShard) {
	t.Helper()
	if sh == nil || sh.reader.filter == nil {
		t.Fatalf("shard has no filter section")
	}
	settings := contentServiceExtractSettings()
	ctx := contentWithExtractSettings(context.Background(), settings)
	basePaths := make([]string, len(sh.idx.Docs))
	for i := range sh.idx.Docs {
		basePaths[i] = sh.reader.docPath(uint32(i))
	}
	src := newContentFoldDiskSource(f.vol, sh.idx.Docs, basePaths, nil, settings, ctx, nil)
	defer src.stop()
	fid := contentFilterID{
		MaxRaw:       sh.reader.filter.MaxRaw,
		MaxText:      sh.reader.filter.MaxText,
		CapsExplicit: sh.reader.filter.CapsExplicit,
		Root:         sh.reader.filter.Root,
		Encoding:     sh.reader.filter.Encoding,
	}
	built, err := assembleContentIndexStreamed(src.next, t.TempDir(), fid)
	if err != nil {
		t.Fatalf("rebuild shard %s: %v", sh.info.File, err)
	}
	if got, want := contentDocTableBytes(built.docs), sh.idx.Sections[contentSectionDocTable]; !bytes.Equal(got, want) {
		t.Fatalf("shard %s CXDT differs from rebuild", sh.info.File)
	}
	if got, want := encodeContentPaths(built.paths), sh.idx.Sections[contentSectionPaths]; !bytes.Equal(got, want) {
		t.Fatalf("shard %s CXPT differs from rebuild", sh.info.File)
	}
	if got, want := readSectionFile(t, built.filterPath), sh.idx.Sections[contentSectionFilter]; !bytes.Equal(got, want) {
		t.Fatalf("shard %s CXFL differs from rebuild", sh.info.File)
	}
	if got, want := encodeContentAliases(built.aliasPairs), sh.idx.Sections[contentSectionAlias]; !bytes.Equal(got, want) {
		t.Fatalf("shard %s CXAL differs from rebuild", sh.info.File)
	}
	gotGrams := decodePostingSectionForTest(t, sh.idx.Sections[contentSectionGrams])
	wantGrams := decodePostingSectionForTest(t, readSectionFile(t, built.gramPath))
	gotPruned := decodePrunedForTest(t, sh.idx.Sections[contentSectionPruned])
	wantPruned := decodePrunedForTest(t, readSectionFile(t, built.prunePath))
	for key, wantIDs := range wantGrams {
		g := contentGramU32Of(key)
		if _, ok := gotPruned[g]; ok {
			continue
		}
		if _, ok := wantPruned[g]; ok {
			continue
		}
		gotIDs, ok := gotGrams[key]
		if !ok || !samePostingIDs(gotIDs, wantIDs) {
			t.Fatalf("shard %s CXGR key %q = %v; rebuild %v", sh.info.File, key, gotIDs, wantIDs)
		}
	}
	for key := range gotGrams {
		g := contentGramU32Of(key)
		if _, ok := gotPruned[g]; ok {
			continue
		}
		if _, ok := wantPruned[g]; ok {
			continue
		}
		if _, ok := wantGrams[key]; !ok {
			t.Fatalf("shard %s CXGR has key %q missing from rebuild", sh.info.File, key)
		}
	}
}

func samePostingIDs(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameNames(t *testing.T, got []Entry, want []Entry) {
	t.Helper()
	a, b := namesOf(got), namesOf(want)
	if len(a) != len(b) {
		t.Fatalf("names = %v; want %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("names = %v; want %v", a, b)
		}
	}
}

func TestContentPostingFoldEquivalenceWithRebuild(t *testing.T) {
	var files []postingFoldFile
	for i := 0; i < 8; i++ {
		text := postingFoldText("a", i)
		switch i {
		case 1:
			text += "phrase alpha one\n"
		case 2:
			text += "resume r\u00e9sum\u00e9\n"
		}
		files = append(files, postingFoldFile{frn: uint64(10 + i), folder: "a", name: fmt.Sprintf("a%d.go", i), text: text})
		files = append(files, postingFoldFile{frn: uint64(20 + i), folder: "b", name: fmt.Sprintf("b%d.go", i), text: postingFoldText("b", i)})
		files = append(files, postingFoldFile{frn: uint64(30 + i), folder: "c", name: fmt.Sprintf("c%d.go", i), text: postingFoldText("c", i)})
	}
	f := newPostingFoldFixture(t, files)
	before := make(map[string]string)
	for _, sh := range f.vol.content.readerView().idx.shards {
		before[sh.info.File] = sh.info.File
	}

	var choices []string
	contentPostingFoldChoiceHook = func(shard string, posting bool) {
		if posting {
			choices = append(choices, shard)
		}
	}
	t.Cleanup(func() { contentPostingFoldChoiceHook = nil })

	f.churn(t, 20, 200, postingFoldText("b", 20)+"edited20\n")
	f.rename(t, 11, "c", "a1-renamed.go", postingFoldText("renamed", 11)+"moved11\n", 201)
	f.remove(t, 21, 202)
	f.churn(t, 30, 203, postingFoldText("c", 30)+"edited30\n")
	f.add(t, 38, "c", "new38.go", postingFoldText("new", 38)+"newdoc\n", 204)

	f.s.runContentFold(f.vol)
	if len(choices) < 2 {
		t.Fatalf("posting fold ran on %d shards; want the 3 dirty shards", len(choices))
	}
	if got := f.vol.content.deltaView().liveCount(); got != 0 {
		t.Fatalf("delta after fold = %d; want 0", got)
	}

	reader := f.vol.content.readerView()
	if reader == nil || reader.idx.shards == nil {
		t.Fatal("fold did not publish a sharded base")
	}
	for _, sh := range reader.idx.shards {
		assertShardMatchesRebuild(t, f, sh)
	}

	// The same final corpus built from disk must answer every query identically.
	rebuilt, rebuiltSvc := f.rebuiltVolume(t)
	queries := []string{
		"content:shared",
		"content:unique3",
		"content:\"alpha one\"",
		"content:resume",
		"content:/unique[0-9]+/",
		"content:NEWDOC",
		"sym:File3",
		"content:edited20",
		"content:moved11",
	}
	for _, q := range queries {
		got, err := contentServiceSearch(t, f.vol, q, false)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		want, err := contentServiceSearch(t, rebuilt, q, false)
		if err != nil {
			t.Fatalf("%s (rebuild): %v", q, err)
		}
		sameNames(t, got, want)
		gotCount := contentServiceCount(t, []*serviceVolumeIndex{f.vol}, q)
		if len(got) != gotCount {
			t.Fatalf("%s search hits = %d; count = %d", q, len(got), gotCount)
		}
		if wantCount := contentServiceCount(t, []*serviceVolumeIndex{rebuilt}, q); gotCount != wantCount {
			t.Fatalf("%s count = %d; rebuild %d", q, gotCount, wantCount)
		}
	}
	under := filepath.Join(f.dir, "b")
	got, err := searchServiceVolumes([]*serviceVolumeIndex{f.vol}, queryOptions{Query: "content:shared", Under: under, Limit: 100}, false)
	if err != nil {
		t.Fatal(err)
	}
	want, err := searchServiceVolumes([]*serviceVolumeIndex{rebuilt}, queryOptions{Query: "content:shared", Under: under, Limit: 100}, false)
	if err != nil {
		t.Fatal(err)
	}
	sameNames(t, got, want)

	// Cursors paginate the same result sequence on both copies.
	gotPages, _ := contentCursorPages(t, f.s, "content:shared", 3)
	wantPages, _ := contentCursorPages(t, rebuiltSvc, "content:shared", 3)
	if !sameStringSet(gotPages, wantPages) {
		t.Fatalf("cursor pages = %v; rebuild %v", gotPages, wantPages)
	}
}

func TestContentPostingFoldRemapAndDedupe(t *testing.T) {
	t.Run("delete-first-middle-last", func(t *testing.T) {
		var files []postingFoldFile
		for i := 0; i < 9; i++ {
			files = append(files, postingFoldFile{frn: uint64(10 + i), folder: "a", name: fmt.Sprintf("f%d.go", i), text: postingFoldText("a", i)})
		}
		f := newPostingFoldFixture(t, files)
		f.remove(t, 10, 200)
		f.s.runContentFold(f.vol)
		f.remove(t, 14, 201)
		f.s.runContentFold(f.vol)
		f.remove(t, 18, 202)
		f.s.runContentFold(f.vol)
		reader := f.vol.content.readerView()
		for _, sh := range reader.idx.shards {
			assertShardMatchesRebuild(t, f, sh)
		}
		if got := f.count(t, "content:shared"); got != 6 {
			t.Fatalf("content:shared = %d; want 6", got)
		}
		if got := f.count(t, "content:unique0"); got != 0 {
			t.Fatalf("deleted first doc still matches: %d", got)
		}
		if got := f.count(t, "content:unique8"); got != 0 {
			t.Fatalf("deleted last doc still matches: %d", got)
		}
	})

	t.Run("delete-primary", func(t *testing.T) {
		files := []postingFoldFile{
			{frn: 10, folder: "a", name: "p.go", text: "duplicate needle shared\n" + postingFoldText("a", 10)},
			{frn: 11, folder: "a", name: "dup.go", text: "duplicate needle shared\n" + postingFoldText("a", 10)},
			{frn: 12, folder: "a", name: "x.go", text: postingFoldText("a", 12)},
			{frn: 13, folder: "a", name: "y.go", text: postingFoldText("a", 13)},
			{frn: 14, folder: "a", name: "z.go", text: postingFoldText("a", 14)},
			{frn: 15, folder: "a", name: "w.go", text: postingFoldText("a", 15)},
		}
		f := newPostingFoldFixture(t, files)
		f.remove(t, 10, 200)
		f.s.runContentFold(f.vol)
		reader := f.vol.content.readerView()
		for _, sh := range reader.idx.shards {
			assertShardMatchesRebuild(t, f, sh)
		}
		if got := f.count(t, "content:duplicate"); got != 1 {
			t.Fatalf("duplicate hits after primary delete = %d; want 1 (the promoted alias)", got)
		}
		if got := f.count(t, "content:unique10"); got != 1 {
			t.Fatalf("promoted alias content = %d; want 1", got)
		}
	})

	t.Run("change-primary", func(t *testing.T) {
		files := []postingFoldFile{
			{frn: 10, folder: "a", name: "p.go", text: "duplicate needle shared\n" + postingFoldText("a", 10)},
			{frn: 11, folder: "a", name: "dup.go", text: "duplicate needle shared\n" + postingFoldText("a", 10)},
			{frn: 12, folder: "a", name: "x.go", text: postingFoldText("a", 12)},
			{frn: 13, folder: "a", name: "y.go", text: postingFoldText("a", 13)},
			{frn: 14, folder: "a", name: "z.go", text: postingFoldText("a", 14)},
			{frn: 15, folder: "a", name: "w.go", text: postingFoldText("a", 15)},
		}
		f := newPostingFoldFixture(t, files)
		f.churn(t, 10, 200, "changed primary only\n")
		f.s.runContentFold(f.vol)
		reader := f.vol.content.readerView()
		for _, sh := range reader.idx.shards {
			assertShardMatchesRebuild(t, f, sh)
		}
		if got := f.count(t, "content:duplicate"); got != 1 {
			t.Fatalf("duplicate hits after primary change = %d; want 1 (the alias promoted)", got)
		}
		if got := f.count(t, "content:changed"); got != 1 {
			t.Fatalf("changed primary content = %d; want 1", got)
		}
	})

	t.Run("lower-docid-duplicate", func(t *testing.T) {
		files := []postingFoldFile{
			{frn: 10, folder: "a", name: "p.go", text: "duplicate needle shared\n"},
			{frn: 11, folder: "a", name: "dup.go", text: "duplicate needle shared\n"},
			{frn: 12, folder: "a", name: "x.go", text: postingFoldText("a", 12)},
			{frn: 13, folder: "a", name: "y.go", text: postingFoldText("a", 13)},
			{frn: 14, folder: "a", name: "z.go", text: postingFoldText("a", 14)},
			{frn: 15, folder: "a", name: "w.go", text: postingFoldText("a", 15)},
		}
		f := newPostingFoldFixture(t, files)
		f.add(t, 9, "a", "lower.go", "duplicate needle shared\n", 200)
		f.s.runContentFold(f.vol)
		reader := f.vol.content.readerView()
		for _, sh := range reader.idx.shards {
			assertShardMatchesRebuild(t, f, sh)
		}
		if got := f.count(t, "content:duplicate"); got != 3 {
			t.Fatalf("duplicate hits after lower-DocID add = %d; want 3", got)
		}
	})
}

func (f *postingFoldFixture) count(t *testing.T, query string) int {
	t.Helper()
	hits, err := contentServiceSearch(t, f.vol, query, false)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return len(hits)
}

func TestContentPostingFoldPruningNoFalseNegatives(t *testing.T) {
	old := contentGramPruneDF
	contentGramPruneDF = 1
	t.Cleanup(func() { contentGramPruneDF = old })

	var files []postingFoldFile
	for i := 0; i < 6; i++ {
		files = append(files, postingFoldFile{frn: uint64(10 + i), folder: "a", name: fmt.Sprintf("p%d.go", i), text: "shared common " + postingFoldText("a", i)})
	}
	f := newPostingFoldFixture(t, files)
	f.churn(t, 10, 200, "shared common edited padding\n")
	f.s.runContentFold(f.vol)
	if got := f.count(t, "content:shared"); got != 6 {
		t.Fatalf("content:shared = %d; want 6 with pruned grams", got)
	}
	if got := f.count(t, "content:common"); got != 6 {
		t.Fatalf("content:common = %d; want 6 with pruned grams", got)
	}
	if got := f.count(t, "content:edited"); got != 1 {
		t.Fatalf("content:edited = %d; want 1", got)
	}
}

func TestContentPostingFoldOneChangedDocReadsOneFile(t *testing.T) {
	var files []postingFoldFile
	for i := 0; i < 6; i++ {
		files = append(files, postingFoldFile{frn: uint64(10 + i), folder: "a", name: fmt.Sprintf("r%d.go", i), text: postingFoldText("a", i)})
	}
	f := newPostingFoldFixture(t, files)
	prev := contentPostingFoldExtract
	reads := 0
	contentPostingFoldExtract = func(ctx context.Context, item contentBuildItem) (contentBuildDoc, contentExtractResult, bool) {
		reads++
		return prev(ctx, item)
	}
	t.Cleanup(func() { contentPostingFoldExtract = prev })
	var posted bool
	contentPostingFoldChoiceHook = func(_ string, p bool) { posted = posted || p }
	t.Cleanup(func() { contentPostingFoldChoiceHook = nil })

	f.churn(t, 12, 200, postingFoldText("a", 12)+"onechanged\n")
	f.s.runContentFold(f.vol)
	if !posted {
		t.Fatal("fold did not use the posting path")
	}
	if reads != 1 {
		t.Fatalf("posting fold read %d files; want exactly 1", reads)
	}
	if got := f.count(t, "content:onechanged"); got != 1 {
		t.Fatalf("changed doc = %d; want 1", got)
	}
}

func TestContentPostingFoldUnshardedOneChangedDocReadsOneFile(t *testing.T) {
	files := []contentFixtureFile{
		{10, "a0.txt", postingFoldText("a", 0)},
		{11, "a1.txt", postingFoldText("a", 1)},
		{12, "a2.txt", postingFoldText("a", 2)},
		{13, "a3.txt", postingFoldText("a", 3)},
		{14, "a4.txt", postingFoldText("a", 4)},
		{15, "a5.txt", postingFoldText("a", 5)},
	}
	f := newContentFoldFixture(t, files)
	if f.vol.content.readerView().idx.shards != nil {
		t.Fatal("precondition: fixture base should be a single file")
	}
	prev := contentPostingFoldExtract
	reads := 0
	contentPostingFoldExtract = func(ctx context.Context, item contentBuildItem) (contentBuildDoc, contentExtractResult, bool) {
		reads++
		return prev(ctx, item)
	}
	t.Cleanup(func() { contentPostingFoldExtract = prev })
	var choices []bool
	contentPostingFoldChoiceHook = func(_ string, p bool) { choices = append(choices, p) }
	t.Cleanup(func() { contentPostingFoldChoiceHook = nil })

	f.churn(t, 12, 200, postingFoldText("a", 12)+"unsharded\n")
	f.s.runContentFold(f.vol)
	if len(choices) != 1 || !choices[0] {
		t.Fatalf("fold choices = %v; want one posting fold", choices)
	}
	if reads != 1 {
		t.Fatalf("posting fold read %d files; want exactly 1", reads)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:unsharded", false); len(hits) != 1 {
		t.Fatalf("changed doc = %v; want 1", hits)
	}
	if hits, _ := contentServiceSearch(t, f.vol, "content:unique2", false); len(hits) != 0 {
		t.Fatalf("old text still matches: %v", hits)
	}
}

func TestContentPostingFoldFallbackRestoresTerms(t *testing.T) {
	t.Run("changed-share", func(t *testing.T) {
		oldPct := contentPostingFoldMaxChangedPercent
		contentPostingFoldMaxChangedPercent = 1
		t.Cleanup(func() { contentPostingFoldMaxChangedPercent = oldPct })
		var files []postingFoldFile
		for i := 0; i < 20; i++ {
			files = append(files, postingFoldFile{frn: uint64(10 + i), folder: "a", name: fmt.Sprintf("c%d.go", i), text: postingFoldText("a", i)})
		}
		f := newPostingFoldFixture(t, files)
		var choices []bool
		contentPostingFoldChoiceHook = func(_ string, p bool) { choices = append(choices, p) }
		t.Cleanup(func() { contentPostingFoldChoiceHook = nil })
		f.churn(t, 10, 200, postingFoldText("a", 10)+"fallbackterm\n")
		f.s.runContentFold(f.vol)
		if len(choices) != 1 || choices[0] {
			t.Fatalf("fold choice = %v; want one full re-extract", choices)
		}
		assertShardTermsMatchRebuild(t, f)
	})

	t.Run("fold-count", func(t *testing.T) {
		oldMax := contentPostingFoldMaxFolds
		oldPct := contentPostingFoldMaxChangedPercent
		contentPostingFoldMaxFolds = 1
		contentPostingFoldMaxChangedPercent = 100
		t.Cleanup(func() {
			contentPostingFoldMaxFolds = oldMax
			contentPostingFoldMaxChangedPercent = oldPct
		})
		var files []postingFoldFile
		for i := 0; i < 20; i++ {
			files = append(files, postingFoldFile{frn: uint64(10 + i), folder: "a", name: fmt.Sprintf("d%d.go", i), text: postingFoldText("a", i)})
		}
		f := newPostingFoldFixture(t, files)
		var choices []bool
		contentPostingFoldChoiceHook = func(_ string, p bool) { choices = append(choices, p) }
		t.Cleanup(func() { contentPostingFoldChoiceHook = nil })
		f.churn(t, 10, 200, postingFoldText("a", 10)+"foldone\n")
		f.s.runContentFold(f.vol)
		f.churn(t, 11, 201, postingFoldText("a", 11)+"foldtwo\n")
		f.s.runContentFold(f.vol)
		f.churn(t, 12, 202, postingFoldText("a", 12)+"foldthree\n")
		f.s.runContentFold(f.vol)
		if len(choices) != 3 || !choices[0] || !choices[1] || choices[2] {
			t.Fatalf("fold choices = %v; want posting, posting, full", choices)
		}
		assertShardTermsMatchRebuild(t, f)
	})
}

func assertShardTermsMatchRebuild(t *testing.T, f *postingFoldFixture) {
	t.Helper()
	reader := f.vol.content.readerView()
	if reader == nil || reader.idx.shards == nil {
		t.Fatal("volume is not sharded")
	}
	for _, sh := range reader.idx.shards {
		settings := contentServiceExtractSettings()
		ctx := contentWithExtractSettings(context.Background(), settings)
		basePaths := make([]string, len(sh.idx.Docs))
		for i := range sh.idx.Docs {
			basePaths[i] = sh.reader.docPath(uint32(i))
		}
		src := newContentFoldDiskSource(f.vol, sh.idx.Docs, basePaths, nil, settings, ctx, nil)
		built, err := assembleContentIndexStreamed(src.next, t.TempDir(), contentFilterID{
			MaxRaw: sh.reader.filter.MaxRaw, MaxText: sh.reader.filter.MaxText,
			CapsExplicit: sh.reader.filter.CapsExplicit, Root: sh.reader.filter.Root,
			Encoding: sh.reader.filter.Encoding,
		})
		src.stop()
		if err != nil {
			t.Fatal(err)
		}
		gotSec := sh.idx.Sections[contentSectionTerms]
		wantSec := readSectionFile(t, built.termPath)
		if !bytes.Equal(gotSec, wantSec) {
			got, want := decodeTermsForTest(t, gotSec), decodeTermsForTest(t, wantSec)
			t.Fatalf("shard %s CXTR differs from rebuild: got %d terms, want %d", sh.info.File, len(got), len(want))
		}
	}
}

func decodeTermsForTest(t *testing.T, sec []byte) map[string]int {
	t.Helper()
	if len(sec) == 0 {
		return nil
	}
	d, err := decodeContentTermDict(sec)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]int, d.keyCount())
	for i := 0; i < d.keyCount(); i++ {
		k, ok := d.keyAt(i)
		if !ok {
			t.Fatal("term key decode failed")
		}
		out[k] = d.dfAt(i)
	}
	return out
}

func TestContentPostingFoldFailureKeepsOldBaseAndChecksums(t *testing.T) {
	var files []postingFoldFile
	for i := 0; i < 6; i++ {
		files = append(files, postingFoldFile{frn: uint64(10 + i), folder: "a", name: fmt.Sprintf("k%d.go", i), text: postingFoldText("a", i)})
	}
	f := newPostingFoldFixture(t, files)
	before := f.shardFiles(t)

	oldSave := contentFoldSaveManifest
	contentFoldSaveManifest = func(string, *contentIndex, []contentShardInfo) error {
		return errors.New("injected manifest failure")
	}
	t.Cleanup(func() { contentFoldSaveManifest = oldSave })
	f.churn(t, 10, 200, postingFoldText("a", 10)+"lostedit\n")
	f.s.runContentFold(f.vol)
	if got := f.shardFiles(t); !sameStringSet(got, before) {
		t.Fatalf("shard files after failed manifest = %v; want %v", got, before)
	}

	// A restart sees the old base, not the uncommitted shard.
	idx2 := &Index{Source: "usn", Volume: f.vol.index.Volume, Compact: true, JournalID: f.vol.index.JournalID, Checkpoint: f.vol.index.Checkpoint}
	idx2.Records = append([]CompactRecord(nil), f.vol.index.Records...)
	contentIndexFRNs(idx2)
	vol2 := newServiceVolumeIndex(f.vol.dbPath, idx2)
	releaseVolumeContentOnCleanup(t, vol2)
	s2 := contentTestService(t)
	s2.attachContentForVolume(vol2)
	if got := vol2.content.stateOf(); got != contentStateReady {
		t.Fatalf("restart state = %q; want ready", got)
	}
	contentFoldSaveManifest = oldSave
	if hits, _ := contentServiceSearch(t, vol2, "content:unique1", false); len(hits) != 1 {
		t.Fatalf("old base not served after failed fold: %v", hits)
	}
	if hits, _ := contentServiceSearch(t, vol2, "content:lostedit", false); len(hits) != 0 {
		t.Fatalf("uncommitted edit reached the base: %v", hits)
	}

	// Checksums: a flipped byte in any rebuilt section must fail load/verify.
	f.s.runContentFold(f.vol)
	reader := f.vol.content.readerView()
	for _, sh := range reader.idx.shards {
		path := filepath.Join(contentShardDir(f.gsx), sh.info.File)
		assertShardChecksums(t, path)
	}
}

func (f *postingFoldFixture) shardFiles(t *testing.T) []string {
	t.Helper()
	reader := f.vol.content.readerView()
	if reader == nil || reader.idx.shards == nil {
		return nil
	}
	out := make([]string, 0, len(reader.idx.shards))
	for _, sh := range reader.idx.shards {
		out = append(out, sh.info.File)
	}
	sort.Strings(out)
	return out
}

func assertShardChecksums(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < contentHeaderSize() {
		t.Fatal("shard too short")
	}
	tableOffset := binary.LittleEndian.Uint64(data[32:])
	count := binary.LittleEndian.Uint32(data[40:])
	for i := uint32(0); i < count; i++ {
		base := int(tableOffset) + 4 + int(i)*contentSectionEntrySize
		tag := binary.LittleEndian.Uint32(data[base:])
		off := binary.LittleEndian.Uint64(data[base+4:])
		length := binary.LittleEndian.Uint64(data[base+12:])
		if length == 0 {
			continue
		}
		mut := append([]byte(nil), data...)
		mut[int(off)+int(length)/2] ^= 0xFF
		tmp := filepath.Join(t.TempDir(), "bad.gsx")
		if err := os.WriteFile(tmp, mut, 0o600); err != nil {
			t.Fatal(err)
		}
		idx, err := contentLoadFile(tmp)
		if err == nil {
			err = verifyContentSectionCRCs(idx)
			idx.Release()
		}
		var crcErr *contentSectionCRCError
		if !errors.As(err, &crcErr) {
			t.Fatalf("section %08x flipped: err = %v; want contentSectionCRCError", tag, err)
		}
	}
}
