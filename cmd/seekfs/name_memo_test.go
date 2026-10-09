package main

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memoTestVolumes builds two packed volumes (the service load shape after
// the pack-on-load change) with name identities ensured, ready for memo
// lane tests.
func memoTestVolumes(t *testing.T) []*serviceVolumeIndex {
	t.Helper()
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	a := dottedPathBenchmarkIndex(2000)
	a.packCompactRecords(true)
	b := dottedPathBenchmarkIndex(500)
	b.Volume = "E:"
	b.packCompactRecords(true)
	va := newServiceVolumeIndex("memo-a.gsi", a)
	vb := newServiceVolumeIndex("memo-b.gsi", b)
	for _, vol := range []*serviceVolumeIndex{va, vb} {
		if ident := vol.index.ensureNameIdentity(); ident == nil {
			t.Fatal("name identity declined packed fixture")
		}
	}
	return []*serviceVolumeIndex{va, vb}
}

func memoTraceHasLane(t *testing.T, trace *searchTrace, want string) {
	t.Helper()
	for _, term := range trace.Terms {
		if term.Kind == "memo" && term.Source == want {
			return
		}
	}
	t.Fatalf("trace has no memo %s marker: %+v", want, trace.Terms)
}

func TestNameMaskSoundness(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5eed))
	alpha := []byte("abcdefghijklmnopqrstuvwxyz0123456789._- ")
	for i := 0; i < 20000; i++ {
		nl := 1 + rng.Intn(24)
		nb := make([]byte, nl)
		for j := range nb {
			nb[j] = alpha[rng.Intn(len(alpha))]
		}
		tl := 1 + rng.Intn(8)
		tb := make([]byte, tl)
		for j := range tb {
			tb[j] = alpha[rng.Intn(len(alpha))]
		}
		name, term := string(nb), string(tb)
		subset := memoMaskOf(term)&^memoMaskOf(name) == 0
		contains := strings.Contains(name, term)
		if contains && !subset {
			t.Fatalf("mask false negative: term %q in name %q", term, name)
		}
	}
}

func TestNameMaskCaseFolded(t *testing.T) {
	// Masks fold ASCII identically whether computed over raw or folded
	// input, so parse-folded terms and stored-folded names agree.
	for _, s := range []string{"Main.GO", "README.md", "TrainingData_01.nrrd", "a-B_c d"} {
		if memoMaskOf(s) != memoMaskOf(strings.ToLower(s)) {
			t.Fatalf("mask differs after fold for %q", s)
		}
	}
}

func TestPackedNameIdentityBuild(t *testing.T) {
	idx := dottedPathBenchmarkIndex(2000)
	idx.packCompactRecords(true)
	ident := idx.ensureNameIdentity()
	if ident == nil {
		t.Fatal("identity declined packed index")
	}
	if ident.token {
		t.Fatal("packed identity misdetected as token")
	}
	if ident.records != idx.compactRecordCount() {
		t.Fatalf("records = %d, want %d", ident.records, idx.compactRecordCount())
	}
	// Every record resolves to a name whose exemplar carries the same
	// folded string.
	seen := make(map[uint32]string, ident.count)
	for i := 0; i < ident.records; i++ {
		nid := ident.recName[i]
		if nid == memoUnknownName || int(nid) >= ident.count {
			t.Fatalf("record %d has bad name id %d", i, nid)
		}
		s := idx.compactLowerNameAt(i)
		if prev, ok := seen[nid]; ok {
			if prev != s {
				t.Fatalf("name id %d maps to %q and %q", nid, prev, s)
			}
			continue
		}
		seen[nid] = s
		if got := ident.identName(idx, nid); got != s {
			t.Fatalf("exemplar string = %q, want %q", got, s)
		}
		if memoMaskOf(s) != ident.masks[nid] {
			t.Fatalf("mask mismatch for %q", s)
		}
	}
	if len(ident.roots) == 0 {
		t.Fatal("no roots collected")
	}
}

func TestMemoLaneSearchParity(t *testing.T) {
	volumes := memoTestVolumes(t)
	queries := []queryOptions{
		{Query: "trainingdata", MatchPath: true, Limit: 20},
		{Query: "nrrd-cache", MatchPath: true, Limit: 20},
		{Query: "sample-volume.nrrd", MatchPath: true, Limit: 20},
		{Query: "Dataset", MatchPath: true, Limit: 100},
		{Query: "zzzz-no-hit-memo", MatchPath: true, Limit: 20},
		{Query: "trainingdata Dataset", MatchPath: true, Limit: 20},
		{Query: "scan", MatchPath: true, Limit: 20},
		{Query: "main", Limit: 20},
		{Query: "ext:nrrd", Limit: 20},
		{Query: "type:file ext:json", MatchPath: true, Limit: 20},
		{Query: "trainingdata !backup", MatchPath: true, Limit: 20},
		{Query: "nrrd ext:json", MatchPath: true, Limit: 20},
		{Query: "dir:workspace nrrd", MatchPath: true, Limit: 20},
	}
	for _, opts := range queries {
		t.Run(strings.ReplaceAll(opts.Query, " ", "_"), func(t *testing.T) {
			want, err := r5ExhaustivePlannerOracle(volumes, opts, false)
			if err != nil {
				t.Fatal(err)
			}
			trace := &searchTrace{}
			tracedOpts := opts
			tracedOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, tracedOpts, false)
			if err != nil {
				t.Fatalf("search: %v trace=%+v", err, *trace)
			}
			if !sameOrderedStrings(pathsOf(got), pathsOf(want)) {
				t.Fatalf("search paths=%v want=%v trace=%+v", pathsOf(got), pathsOf(want), *trace)
			}
			wantCount, err := r5ExhaustivePlannerOracle(volumes, opts, true)
			if err != nil {
				t.Fatal(err)
			}
			n, _, err := countServiceVolumes(volumes, opts)
			if err != nil {
				t.Fatal(err)
			}
			if n != len(wantCount) {
				t.Fatalf("count=%d want=%d trace=%+v", n, len(wantCount), *trace)
			}
		})
	}
}

func TestMemoLaneEngagesBoundedFallback(t *testing.T) {
	volumes := memoTestVolumes(t)
	// Single-term path queries decline the components lane (needs 2+
	// terms) and land in the bounded fallback, where the memo scan runs.
	for _, q := range []string{"trainingdata", "nrrd-cache", "zzzz-no-hit-memo"} {
		trace := &searchTrace{}
		got, err := searchServiceVolumes(volumes, queryOptions{Query: q, MatchPath: true, Limit: 20, Trace: trace}, false)
		if err != nil {
			t.Fatal(err)
		}
		memoTraceHasLane(t, trace, "memo-scan")
		want, err := r5ExhaustivePlannerOracle(volumes, queryOptions{Query: q, MatchPath: true, Limit: 20}, false)
		if err != nil {
			t.Fatal(err)
		}
		if !sameOrderedStrings(pathsOf(got), pathsOf(want)) {
			t.Fatalf("q=%q paths=%v want=%v", q, pathsOf(got), pathsOf(want))
		}
	}
}

func TestMemoCountEngages(t *testing.T) {
	volumes := memoTestVolumes(t)
	trace := &searchTrace{}
	n, _, err := countServiceVolumes(volumes, queryOptions{Query: "trainingdata", MatchPath: true, Trace: trace})
	if err != nil {
		t.Fatal(err)
	}
	// The global fallback labels the source; memo engagement is proven
	// by the additive memo-count marker (one per volume counted).
	memoTraceHasLane(t, trace, "memo-count")
	want, err := r5ExhaustivePlannerOracle(volumes, queryOptions{Query: "trainingdata", MatchPath: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(want) {
		t.Fatalf("count=%d want=%d", n, len(want))
	}
}

func TestNameIdentityResetOnMutation(t *testing.T) {
	idx := dottedPathBenchmarkIndex(100)
	idx.packCompactRecords(true)
	if idx.ensureNameIdentity() == nil {
		t.Fatal("identity declined")
	}
	idx.setCompactRecord(3, idx.compactRecord(3))
	if idx.nameIdent.Load() != nil {
		t.Fatal("setCompactRecord did not reset the identity")
	}
	if idx.ensureNameIdentity() == nil {
		t.Fatal("identity declined after reset")
	}
	idx.appendCompactRecord(CompactRecord{FRN: 9999, ParentFRN: 1, Parent: 0, Name: "appended.txt"})
	if idx.nameIdent.Load() != nil {
		t.Fatal("appendCompactRecord did not reset the identity")
	}
}

func TestMemoPrefixAndFilenameParity(t *testing.T) {
	for _, root := range []string{"", `C:\archive-root\workspace`} {
		idx := dottedPathBenchmarkIndex(100)
		idx.Volume = `\\archive-server\share`
		if root != "" {
			idx.Roots = []string{root}
		}
		idx.packCompactRecords(true)
		vol := newServiceVolumeIndex("prefix.gsi", idx)
		for _, matchPath := range []bool{false, true} {
			for _, query := range []string{"archive", "server", "workspace !server", "workspace !archive", "workspace !path:control", "workspace .", "workspace ext:json"} {
				pq := mustParsePrefilterQuery(t, queryOptions{Query: query, MatchPath: matchPath})
				got, ok := vol.memoCount(pq, hiddenBaseIDs{})
				if !ok {
					continue // Explicit path negations in filename mode must decline.
				}
				want := 0
				for id := 0; id < idx.compactRecordCount(); id++ {
					entry := compactEntryFromRecord(idx, id, idx.compactRecord(id), make(map[int]string), true)
					if entryMatches(entry, pq, pq.MatchPath) {
						want++
					}
				}
				if got != want {
					t.Fatalf("root=%q path=%v query=%q count=%d want=%d", root, matchPath, query, got, want)
				}
			}
		}
	}
}

func TestMemoDirectoryAggregateSize(t *testing.T) {
	idx := dottedPathBenchmarkIndex(100)
	idx.packCompactRecords(true)
	idx.Derived.SubtreeBytes = make([]uint64, idx.compactRecordCount())
	idx.Derived.SubtreeBytes[1] = 5000 // workspace, raw record size is 1024
	deltas := map[int]int64{1: 2000}
	idx.dirSizeDelta.Store(&deltas)
	vol := newServiceVolumeIndex("aggregate.gsi", idx)
	pq := mustParsePrefilterQuery(t, queryOptions{Query: "workspace type:dir size:>6000", MatchPath: true})
	n, ok := vol.memoCount(pq, hiddenBaseIDs{})
	if !ok || n != 1 {
		t.Fatalf("aggregate memo count=%d ok=%v want 1", n, ok)
	}
}

func TestMemoBuildCancellationDoesNotPublish(t *testing.T) {
	volumes := memoTestVolumes(t)
	vol := volumes[0]
	pq := mustParsePrefilterQuery(t, queryOptions{Query: "workspace", MatchPath: true})
	pq.Cancel = func() bool { return true }
	if memo, _ := vol.memoFor(pq); memo != nil {
		t.Fatal("canceled name scoring published a memo")
	}
	if vol.index.nameMemo.memo != nil {
		t.Fatal("canceled memo remained cached")
	}
}

func TestMemoLaneCycleSafe(t *testing.T) {
	idx := &Index{Source: "usn", Volume: "C:", Compact: true}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) {
		idx.Records = append(idx.Records, CompactRecord{FRN: frn, ParentFRN: parentFRN, Parent: parent, Name: name})
	}
	add(1, 1, -1, ".", uint32(os.ModeDir))
	add(2, 1, 0, "workspace", uint32(os.ModeDir))
	add(3, 2, 1, "needle.txt", 0)
	add(4, 2, 1, "loop-a", uint32(os.ModeDir))
	add(5, 4, 3, "loop-b", uint32(os.ModeDir))
	// Close a parent cycle: needle <-> loop-b. Records unreachable from
	// any root take the legacy verify path inside the memo scan, so this
	// asserts the memo scan directly against a brute-force legacy verify
	// over every record. (Service-level planned lanes have a separate
	// pre-existing gap on cyclic graphs.)
	idx.Records[2].Parent = 4
	idx.Records[4].Parent = 2
	buildOrders(idx)
	idx.packCompactRecords(true)
	vol := newServiceVolumeIndex("memo-cycle.gsi", idx)
	if vol.index.ensureNameIdentity() == nil {
		t.Fatal("identity declined")
	}
	volumes := []*serviceVolumeIndex{vol}
	_ = volumes
	for _, q := range []string{"needle", "loop", "workspace needle", "zzz-no-hit-cycle"} {
		pq, err := parseQuery(queryOptions{Query: q, MatchPath: true, Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		pq.Limit = 20
		// Brute force: legacy verify of every record.
		want := map[int]bool{}
		cache := make(map[int]string)
		for id := 0; id < idx.compactRecordCount(); id++ {
			if _, ok := compactCandidateEntryIfMatch(idx, pq, id, cache, true, false); ok {
				want[id] = true
			}
		}
		got, ok := vol.memoScanAll(pq, hiddenBaseIDs{}, nil)
		if !ok {
			t.Fatalf("q=%q memo scan declined", q)
		}
		if len(got) != len(want) {
			t.Fatalf("q=%q memo ids=%v want %d records", q, got, len(want))
		}
		for _, id := range got {
			if !want[id] {
				t.Fatalf("q=%q memo returned non-match %d", q, id)
			}
		}
		n, ok := vol.memoCount(pq, hiddenBaseIDs{})
		if !ok || n != len(want) {
			t.Fatalf("q=%q memo count=%d,%v want %d", q, n, ok, len(want))
		}
	}
}

func TestMemoLaneWithTombstones(t *testing.T) {
	volumes := memoTestVolumes(t)
	opts := queryOptions{Query: "sample-volume.nrrd", MatchPath: true, Limit: 20}
	before, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("no baseline results")
	}
	// Tombstone the FRN 7 base record (sample-volume.nrrd on volume A);
	// the memo scan must skip it via the hidden set while matching
	// everything else exactly.
	volumes[0].applyUSNChanges([]usnChange{{FRN: 7, USN: 10, Reason: usnReasonFileDelete}})
	got, err := searchServiceVolumes(volumes, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(before))
	dropped := false
	for _, p := range pathsOf(before) {
		if !dropped && strings.HasPrefix(p, `C:\`) && strings.Contains(p, "sample-volume.nrrd") {
			dropped = true
			continue
		}
		want = append(want, p)
	}
	if !dropped {
		t.Fatalf("baseline has no C: sample-volume.nrrd to tombstone: %v", pathsOf(before))
	}
	if !sameOrderedStrings(pathsOf(got), want) {
		t.Fatalf("paths=%v want=%v", pathsOf(got), want)
	}
}

// memoMmapFixture builds a small v9 mmap-backed volume (the production
// shape) with a two-level tree, for covering the token-identity path:
// token masks, tokenAt/parentAt/modeAt readers, and memo parity.
func memoMmapFixture(t *testing.T) *serviceVolumeIndex {
	t.Helper()
	records := []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}}
	add := func(frn, parentFRN uint64, parent int32, name string, mode uint32) {
		records = append(records, CompactRecord{FRN: frn, ParentFRN: parentFRN, Parent: parent, Name: name, Mode: mode, Size: 1024})
	}
	add(101, 100, 0, "workspace", uint32(os.ModeDir))
	add(102, 101, 1, "Dataset", uint32(os.ModeDir))
	add(103, 102, 2, "sample-volume.nrrd", 0)
	add(104, 102, 2, "sample-labels.raw", 0)
	add(105, 101, 1, "control", uint32(os.ModeDir))
	add(106, 105, 5, "control-volume.nrrd", 0)
	add(107, 100, 0, "notes.txt", 0)
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`F:\`},
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: records,
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "memo-mmap.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save memo mmap fixture: %v", err)
	}
	loaded, err := loadIndexMMap(db)
	if err != nil {
		t.Fatalf("load memo mmap fixture: %v", err)
	}
	t.Cleanup(func() {
		if loaded.MMapRecords != nil {
			_ = loaded.MMapRecords.file.close()
		}
	})
	vol := newServiceVolumeIndex(db, loaded)
	if vol.index.MMapRecords == nil {
		t.Fatal("fixture is not mmap-backed")
	}
	if ident := vol.index.ensureNameIdentity(); ident == nil || !ident.token {
		t.Fatalf("token identity declined (ident=%+v)", ident)
	}
	return vol
}

func TestMemoLaneMmapParity(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	vol := memoMmapFixture(t)
	volumes := []*serviceVolumeIndex{vol}
	// Literal expectations: the mmap-loaded fixture has no in-RAM name
	// order, so the exhaustive oracle scans ID order while service lanes
	// sort; assert the sorted service result directly. Counts still check
	// against the oracle (order-free).
	ds := `F:\workspace\Dataset`
	sv := `F:\workspace\Dataset\sample-volume.nrrd`
	sl := `F:\workspace\Dataset\sample-labels.raw`
	cv := `F:\workspace\control\control-volume.nrrd`
	nt := `F:\notes.txt`
	type mmapCase struct {
		opts queryOptions
		want []string
	}
	queries := []mmapCase{
		{queryOptions{Query: "Dataset", MatchPath: true, Limit: 20}, []string{ds, sl, sv}},
		{queryOptions{Query: "nrrd", MatchPath: true, Limit: 20}, []string{cv, sv}},
		{queryOptions{Query: "sample-volume.nrrd", MatchPath: true, Limit: 20}, []string{sv}},
		{queryOptions{Query: "workspace control", MatchPath: true, Limit: 20}, []string{`F:\workspace\control`, cv}},
		{queryOptions{Query: "notes", Limit: 20}, []string{nt}},
		{queryOptions{Query: "zzz-no-hit-mmap", MatchPath: true, Limit: 20}, nil},
		{queryOptions{Query: "type:file ext:nrrd", MatchPath: true, Limit: 20}, []string{cv, sv}},
		{queryOptions{Query: "Dataset !control", MatchPath: true, Limit: 20}, []string{ds, sl, sv}},
	}
	for _, tc := range queries {
		t.Run(strings.ReplaceAll(tc.opts.Query, " ", "_"), func(t *testing.T) {
			trace := &searchTrace{}
			tracedOpts := tc.opts
			tracedOpts.Trace = trace
			got, err := searchServiceVolumes(volumes, tracedOpts, false)
			if err != nil {
				t.Fatalf("search: %v trace=%+v", err, *trace)
			}
			if !sameOrderedStrings(pathsOf(got), tc.want) {
				t.Fatalf("search paths=%v want=%v trace=%+v", pathsOf(got), tc.want, *trace)
			}
			wantCount, err := r5ExhaustivePlannerOracle(volumes, tc.opts, true)
			if err != nil {
				t.Fatal(err)
			}
			n, _, err := countServiceVolumes(volumes, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if n != len(wantCount) {
				t.Fatalf("count=%d want=%d", n, len(wantCount))
			}
			if n != len(tc.want) {
				t.Fatalf("count=%d want=%d (search agreement)", n, len(tc.want))
			}
		})
	}
}

func TestMemoLaneMmapEngages(t *testing.T) {
	t.Setenv("SEEKFS_GLOBAL_PLANNER", "1")
	vol := memoMmapFixture(t)
	// Unit-level engagement on the token identity: the memo scan and
	// count must agree exactly with a brute-force legacy verify, and the
	// scan must record its lane marker. (Which service lane serves a
	// query first depends on posting selectivity, so service-level
	// engagement is asserted on the packed fixtures instead.)
	pq, err := parseQuery(queryOptions{Query: "Dataset", MatchPath: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	pq.Limit = 20
	pq.Trace = &searchTrace{}
	want := map[int]bool{}
	cache := make(map[int]string)
	for id := 0; id < vol.index.compactRecordCount(); id++ {
		if _, ok := compactCandidateEntryIfMatch(vol.index, pq, id, cache, true, false); ok {
			want[id] = true
		}
	}
	if len(want) == 0 {
		t.Fatal("brute force found nothing")
	}
	got, ok := vol.memoScanAll(pq, hiddenBaseIDs{}, nil)
	if !ok {
		t.Fatal("memo scan declined on token identity")
	}
	if len(got) != len(want) {
		t.Fatalf("memo ids=%v want %d records", got, len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("memo returned non-match %d", id)
		}
	}
	memoTraceHasLane(t, pq.Trace, "memo-scan")
	n, ok := vol.memoCount(pq, hiddenBaseIDs{})
	if !ok || n != len(want) {
		t.Fatalf("memo count=%d,%v want %d", n, ok, len(want))
	}
}
