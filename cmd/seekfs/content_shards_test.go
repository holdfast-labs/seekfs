package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestContentPlanShardsSplitsLargeFoldersAndPacksSmallOnes(t *testing.T) {
	paths := []string{
		`C:\r\big\a.go`, `C:\r\big\b.go`, `C:\r\big\sub\c.go`,
		`C:\r\small1\d.go`, `C:\r\small2\e.go`, `C:\r\top.go`,
	}
	sizes := []int64{40, 40, 20, 10, 10, 5}
	plan := contentPlanShards(paths, sizes, 50)
	infos := make([]contentShardInfo, len(plan))
	for i, p := range plan {
		infos[i].Prefixes = p
	}
	a := newContentShardAssignerFromInfo(infos)
	if !contains(plan[a.root], "") {
		t.Fatalf("no shard owns the root prefix: %v", plan)
	}
	used := make([]int64, len(plan))
	for i, p := range paths {
		used[a.file(p)] += sizes[i]
	}
	for i, u := range used {
		if u > 50 && len(plan[i]) > 1 {
			t.Errorf("shard %d (%v) packs %d bytes over the 50-byte target", i, plan[i], u)
		}
	}
	// big's own files exceed the target, so they cannot share a shard with
	// big\sub; small folders pack together.
	if a.file(`C:\r\big\a.go`) == a.file(`C:\r\big\sub\c.go`) {
		t.Errorf("an oversized folder was not split: %v", plan)
	}
	if a.file(`C:\r\small1\d.go`) != a.file(`C:\r\small2\e.go`) {
		t.Errorf("small sibling folders were not packed together: %v", plan)
	}
	// A folder created after the build belongs to its nearest planned owner.
	if a.file(`C:\r\big\new\f.go`) != a.file(`C:\r\big\a.go`) {
		t.Errorf("new folder under big not owned by big's shard")
	}
	if a.file(`D:\elsewhere\g.go`) != a.root {
		t.Errorf("an unplanned path must fall back to the root shard")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestContentShardTableRoundTrip(t *testing.T) {
	in := []contentShardInfo{{File: "a.gsx", Docs: 3, Prefixes: []string{"", `c:\x`}}, {File: "b.gsx", Docs: 0}}
	out, err := decodeContentShards(encodeContentShards(in))
	if err != nil || len(out) != 2 || out[0].File != "a.gsx" || out[0].Docs != 3 || len(out[0].Prefixes) != 2 || out[0].Prefixes[1] != `c:\x` || out[1].File != "b.gsx" {
		t.Fatalf("round trip = %+v, %v", out, err)
	}
	if _, err := decodeContentShards(encodeContentShards([]contentShardInfo{{File: `..\evil.gsx`}})); err == nil {
		t.Fatal("a shard name with a path component must be refused")
	}
}

// shardedFixture is a service-built content volume split into one shard per
// folder (a, b, c).
type shardedFixture struct {
	s     *goSearchService
	vol   *serviceVolumeIndex
	gsx   string
	dir   string
	paths map[uint64]string
}

func newShardedFixture(t *testing.T) *shardedFixture {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentBuildSync(t)
	restoreTarget := contentShardTargetBytes
	contentShardTargetBytes = 60
	t.Cleanup(func() { contentShardTargetBytes = restoreTarget })

	dir := t.TempDir()
	const journal = uint64(0xABC)
	const cp = int64(120)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 1_000_000})
	d := uint32(os.ModeDir)
	records := []CompactRecord{
		{FRN: 2, ParentFRN: 1, Parent: -1, Mode: d, Name: "a"},
		{FRN: 3, ParentFRN: 1, Parent: -1, Mode: d, Name: "b"},
		{FRN: 4, ParentFRN: 1, Parent: -1, Mode: d, Name: "c"},
	}
	files := []struct {
		frn    uint64
		parent int32
		folder string
		name   string
		text   string
	}{
		{11, 0, "a", "a1.txt", "shared alphaone padding padding"},
		{12, 0, "a", "a2.txt", "shared alphatwo padding padding"},
		{21, 1, "b", "b1.txt", "shared betaone padding padding"},
		{22, 1, "b", "b2.txt", "shared betatwo padding padding"},
		{31, 2, "c", "c1.txt", "shared gammaone padding padding"},
	}
	paths := map[uint64]string{}
	for _, f := range files {
		p := filepath.Join(dir, f.folder, f.name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.text), 0o644); err != nil {
			t.Fatal(err)
		}
		paths[f.frn] = p
		records = append(records, CompactRecord{FRN: f.frn, ParentFRN: records[f.parent].FRN, Parent: f.parent, Name: f.name, Size: int64(len(f.text))})
	}
	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(cp), records)
	s := contentTestService(t)
	s.ensureContentBuild(vol)
	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("sharded build state = %q; want ready (%+v)", got, vol.content.healthSnapshot(0))
	}
	return &shardedFixture{s: s, vol: vol, gsx: gsx, dir: dir, paths: paths}
}

func (f *shardedFixture) shardFiles(t *testing.T) []string {
	t.Helper()
	reader := f.vol.content.readerView()
	if reader == nil || reader.idx.shards == nil {
		t.Fatal("attached content base is not sharded")
	}
	var out []string
	for _, sh := range reader.idx.shards {
		out = append(out, sh.info.File)
	}
	return out
}

func (f *shardedFixture) churn(t *testing.T, frn uint64, usn int64, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	f.vol.contentCoord.observeChanges([]usnChange{{FRN: frn, USN: usn, Reason: usnReasonDataOverwrite | usnReasonClose}})
	f.vol.contentCoord.processQueue(func(frn uint64) (string, bool) {
		p, ok := f.paths[frn]
		return p, ok
	})
}

func (f *shardedFixture) count(t *testing.T, term string) int {
	t.Helper()
	hits, err := contentServiceSearch(t, f.vol, "content:"+term, false)
	if err != nil {
		t.Fatalf("search %s: %v", term, err)
	}
	return len(hits)
}

func TestContentShardedBuildServesAllShards(t *testing.T) {
	f := newShardedFixture(t)
	files := f.shardFiles(t)
	if len(files) < 3 {
		t.Fatalf("shards = %v; want one per folder", files)
	}
	if n := f.count(t, "shared"); n != 5 {
		t.Fatalf("content:shared = %d hits; want 5 across shards", n)
	}
	if n := f.count(t, "betatwo"); n != 1 {
		t.Fatalf("content:betatwo = %d hits; want 1", n)
	}
	if health := f.vol.content.healthSnapshot(0); health.Docs != 5 {
		t.Fatalf("health docs = %d; want 5", health.Docs)
	}

	// A restart attaches the manifest and its shards.
	idx2 := &Index{Source: "usn", Volume: f.vol.index.Volume, Compact: true, JournalID: f.vol.index.JournalID, Checkpoint: f.vol.index.Checkpoint}
	idx2.Records = append([]CompactRecord(nil), f.vol.index.Records...)
	contentIndexFRNs(idx2)
	vol2 := newServiceVolumeIndex(f.vol.dbPath, idx2)
	releaseVolumeContentOnCleanup(t, vol2)
	f.s.attachContentForVolume(vol2)
	if got := vol2.content.stateOf(); got != contentStateReady {
		t.Fatalf("restart attach state = %q; want ready", got)
	}
	if hits, _ := contentServiceSearch(t, vol2, "content:gammaone", false); len(hits) != 1 {
		t.Fatalf("content:gammaone after restart = %v", hits)
	}
}

func TestContentShardMaskUnderPrunesShards(t *testing.T) {
	f := newShardedFixture(t)
	reader := f.vol.content.readerView()
	mask := reader.shardMaskUnder(filepath.Join(f.dir, "b"))
	n := 0
	for _, m := range mask {
		if m {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("under b selects %d shards (%v); want 1", n, mask)
	}
	var got []string
	for _, id := range collectDocIDStream(reader.candidateStreamIn("shared", mask), 0) {
		got = append(got, filepath.Base(reader.docPath(id)))
	}
	sort.Strings(got)
	if !sameStringSet(got, []string{"b1.txt", "b2.txt"}) {
		t.Fatalf("under-b candidates = %v; want b1.txt, b2.txt", got)
	}
	if reader.shardMaskUnder("") != nil {
		t.Fatal("no scope must not mask shards")
	}
}

func TestContentShardedFoldRewritesOnlyTouchedShard(t *testing.T) {
	f := newShardedFixture(t)
	before := f.shardFiles(t)
	f.churn(t, 21, 200, f.paths[21], "shared betaedited padding padding")
	f.s.runContentFold(f.vol)
	after := f.shardFiles(t)
	changed := 0
	for i := range before {
		if before[i] != after[i] {
			changed++
		}
	}
	if len(after) != len(before) || changed != 1 {
		t.Fatalf("fold changed %d shard files (before %v, after %v); want exactly 1", changed, before, after)
	}
	if got := f.vol.content.deltaView().liveCount(); got != 0 {
		t.Fatalf("delta after fold = %d; want 0", got)
	}
	if f.count(t, "betaedited") != 1 || f.count(t, "betaone") != 0 || f.count(t, "shared") != 5 {
		t.Fatal("folded shard does not serve the edit, or kept the old text, or lost docs")
	}
	// Superseded shard files are swept once nothing maps them.
	entries, _ := os.ReadDir(contentShardDir(f.gsx))
	if len(entries) != len(after) {
		t.Fatalf("shard dir holds %d files; want %d (old shards swept)", len(entries), len(after))
	}
}

func TestContentShardedFoldMovesRenamedDocBetweenShards(t *testing.T) {
	f := newShardedFixture(t)
	before := f.shardFiles(t)
	moved := filepath.Join(f.dir, "c", "a1-moved.txt")
	if err := os.Rename(f.paths[11], moved); err != nil {
		t.Fatal(err)
	}
	f.paths[11] = moved
	f.churn(t, 11, 200, moved, "shared alphamoved padding padding")
	f.s.runContentFold(f.vol)
	after := f.shardFiles(t)
	changed := 0
	for i := range before {
		if before[i] != after[i] {
			changed++
		}
	}
	if changed != 2 {
		t.Fatalf("rename fold changed %d shards; want 2 (old and new folder)", changed)
	}
	if f.count(t, "alphamoved") != 1 || f.count(t, "alphaone") != 0 || f.count(t, "shared") != 5 {
		t.Fatal("renamed doc duplicated, lost, or kept its old text")
	}
	reader, resolver := f.vol.content.readerResolverView()
	gi, ok := resolver.docForFRN(11)
	if !ok || reader.docPath(uint32(gi)) != moved {
		t.Fatalf("FRN 11 resolves to %q; want %q", reader.docPath(uint32(gi)), moved)
	}
	mask := reader.shardMaskUnder(filepath.Join(f.dir, "c"))
	found := false
	for _, id := range collectDocIDStream(reader.candidateStreamIn("alphamoved", mask), 0) {
		found = found || reader.docPath(id) == moved
	}
	if !found {
		t.Fatal("moved doc is not in the shard owning its new folder")
	}
}
