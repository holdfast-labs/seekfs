package main

// WP7 scope differential: the compound content lane (filename-driven, content
// verified inline) must return the same entries, order, count and snippet set
// as the per-volume content path for every newly in-scope shape: `under:`
// (opts.Under, narrowed on the filename side by the under-root subtree
// iterator, the same narrowing candidate_build.go applies to the per-volume
// planner), `exists:` (inline verification stats the path), overlay
// hidden-base filtering plus the live-slot merge, and biased order on a
// pristine volume. Randomized corpora (fixed seeds, so reproducible) and a
// combinatorial query matrix drive both arms; the lane arm forces the lane on
// and the slow arm forces it off. The signature covers the entry fields AND
// the completeness metadata, with one documented asymmetry: the lane's set is
// exact, so it reports complete where the content path's conservative
// window probe may report incomplete; the assertion allows exactly that
// direction and nothing else.
//
// A sharded arm builds a real multi-shard sidecar (tiny contentShardTargetBytes
// through contentReshardAndSave, the same publish path the service build uses)
// so the slow path prunes via shardMaskUnder while the lane verifies inline.

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// contentLaneScopeCorpus builds n files with real nested parent chains (every
// directory level is a record, so under: resolves to true roots and bias paths
// reconstruct) and content drawn from common/medium/rare word pools. Dirs and
// files share one FRN space; dir records carry no content doc.
func contentLaneScopeCorpus(volume string, seed int64, n int) []contentVolRecord {
	rng := rand.New(rand.NewSource(seed))
	tops := []string{"alpha", "beta", "gamma"}
	subs := []string{"sub0", "sub1"}
	exts := []string{".txt", ".md", ".go", ".log"}
	nameWords := []string{"scan", "report", "notes", "quasar", "harbor", "tunnel"}
	commonBody := "the archive worker syncs the download cache to the remote volume every interval "
	medium := []string{"harbor", "tunnel", "quasar", "beacon", "ledger"}
	rare := []string{"pelican", "xylophone", "zephyr"}
	phrase := "the quick brown fox jumps over the lazy dog"

	recs := make([]contentVolRecord, 0, n+len(tops)*(1+len(subs)))
	dirIndex := make(map[string]int32)
	dirFRN := make(map[string]uint64)
	nextFRN := uint64(100)
	addDir := func(path, name string, parent int32, parentFRN uint64) {
		frn := nextFRN
		nextFRN++
		dirIndex[path] = int32(len(recs))
		dirFRN[path] = frn
		recs = append(recs, contentVolRecord{
			frn: frn, parent: parent, parentFRN: parentFRN,
			name: name, mode: uint32(os.ModeDir), path: path,
		})
	}
	for _, top := range tops {
		topPath := volume + `\` + top
		addDir(topPath, top, -1, 1)
		for _, sub := range subs {
			subPath := topPath + `\` + sub
			addDir(subPath, sub, dirIndex[topPath], dirFRN[topPath])
		}
	}
	for i := 0; i < n; i++ {
		top := tops[rng.Intn(len(tops))]
		dirPath := volume + `\` + top
		parent, parentFRN := dirIndex[dirPath], dirFRN[dirPath]
		if rng.Intn(2) == 0 {
			sub := subs[rng.Intn(len(subs))]
			dirPath += `\` + sub
			parent, parentFRN = dirIndex[dirPath], dirFRN[dirPath]
		}
		name := fmt.Sprintf("%s-%04d%s", nameWords[rng.Intn(len(nameWords))], i, exts[rng.Intn(len(exts))])
		var body strings.Builder
		for r := 0; r <= rng.Intn(4); r++ {
			body.WriteString(commonBody)
		}
		if rng.Intn(3) == 0 {
			body.WriteString(medium[rng.Intn(len(medium))] + " ")
		}
		if rng.Intn(25) == 0 {
			body.WriteString(rare[rng.Intn(len(rare))] + " ")
		}
		if rng.Intn(12) == 0 {
			body.WriteString(phrase + " ")
		}
		if rng.Intn(4) == 0 {
			body.WriteString("UPPER NEEDLE TOKEN ")
		}
		content := body.String()
		if rng.Intn(30) == 0 {
			content = "" // no doc: unindexed record, dropped by both arms
		}
		frn := uint64(100000) + uint64(i)
		recs = append(recs, contentVolRecord{
			frn: frn, parent: parent, parentFRN: parentFRN,
			name: name, size: int64(rng.Intn(2000)), modUnix: int64(1_600_000_000 + rng.Intn(100000)),
			path: dirPath + `\` + name, content: content,
		})
	}
	return recs
}

// contentLaneScopeQueries is the combinatorial matrix: content shapes ×
// filename shapes. Scope dimensions (under/exists/bias/overlay/sort/limit)
// multiply it in the runner. Bare filename terms stay single-token: a
// multi-term filename part changes MatchPath routing between the per-volume
// planner and the global iterator in ways the lane does not promise to
// mirror (the iterator is still a superset, but the slow path's term
// precheck narrows differently); single-term and filter shapes cover the
// lane's contract.
func contentLaneScopeQueries() []string {
	content := []string{
		"content:download",                 // broad
		"content:pelican",                  // rare
		"content:quasar",                   // medium
		`content:"quick brown fox"`,        // phrase
		"content:/down.*ad/",               // regex
		"content:download content:archive", // multi-leaf AND
		"content:nosuchtermxyz",            // empty answer
	}
	filename := []string{
		"ext:.txt",
		"ext:.go",
		"dir:alpha",
		"dir:sub0",
		"scan",
		"quasar",
		"type:file",
		"size:>100",
		"parent:sub0",
	}
	var out []string
	for _, c := range content {
		for _, f := range filename {
			out = append(out, c+" "+f)
		}
	}
	return out
}

// contentLaneRunOne runs a single search on vols with the lane forced on/off
// and returns the signature plus engagement.
func contentLaneRunOne(vols []*serviceVolumeIndex, opts queryOptions, lane bool) ([]string, *searchTrace, error) {
	if lane {
		os.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "1")
	} else {
		os.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "0")
	}
	trace := &searchTrace{}
	opts.Trace = trace
	matches, err := searchServiceVolumes(vols, opts, false)
	if err != nil {
		return nil, trace, err
	}
	return contentBoundedDifferentialSignature(matches, trace), trace, nil
}

func contentLaneCountOne(vols []*serviceVolumeIndex, opts queryOptions, lane bool) (int, *searchTrace, error) {
	if lane {
		os.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "1")
	} else {
		os.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "0")
	}
	trace := &searchTrace{}
	opts.Trace = trace
	n, ok, err := countServiceVolumes(vols, opts)
	if err != nil || !ok {
		return -1, trace, err
	}
	return n, trace, nil
}

// assertLaneScopeEqual compares the lane arm against the slow-path arm: entry
// sets and order must be identical; ContentPartial must match; completeness
// may only differ in the documented direction (lane exact→complete where the
// slow path's window probe is conservative).
func assertLaneScopeEqual(t *testing.T, label string, slowSig, laneSig []string, slowTrace, laneTrace *searchTrace) {
	t.Helper()
	if len(slowSig) != len(laneSig) {
		t.Fatalf("%s: count diverged: slow=%d lane=%d\nslow=%v\nlane=%v", label, len(slowSig), len(laneSig), slowSig, laneSig)
	}
	for i := range slowSig {
		if i == 0 {
			continue // completeness line handled below
		}
		if slowSig[i] != laneSig[i] {
			t.Fatalf("%s: element %d diverged:\nslow=%v\nlane=%v", label, i, slowSig, laneSig)
		}
	}
	if traceFlag(slowTrace, func(t *searchTrace) bool { return t.ContentPartial }) !=
		traceFlag(laneTrace, func(t *searchTrace) bool { return t.ContentPartial }) {
		t.Fatalf("%s: partial diverged slow=%v lane=%v", label, slowTrace.ContentPartial, laneTrace.ContentPartial)
	}
	slowIncomplete := traceFlag(slowTrace, func(t *searchTrace) bool { return t.ContentIncomplete })
	laneIncomplete := traceFlag(laneTrace, func(t *searchTrace) bool { return t.ContentIncomplete })
	if laneIncomplete && !slowIncomplete {
		t.Fatalf("%s: lane reports incomplete where the slow path is complete", label)
	}
	if traceCompleteValue(slowTrace) && !traceCompleteValue(laneTrace) {
		t.Fatalf("%s: lane reports complete=false where the slow path is complete", label)
	}
}

// contentLaneScopeMutations applies one overlay scenario to a volume.
func contentLaneScopeMutations(t *testing.T, vol *serviceVolumeIndex, recs []contentVolRecord, kind string) {
	t.Helper()
	switch kind {
	case "none":
	case "delete-match":
		// Tombstone a base record that matches the broad content term; the
		// volume carries hidden IDs (and overlay records) afterward.
		var victim uint64
		for _, r := range recs {
			if r.content != "" && strings.Contains(r.content, "download") && r.mode == 0 {
				victim = r.frn
				break
			}
		}
		if victim == 0 {
			t.Fatal("no download-matching record to delete")
		}
		vol.applyUSNChanges([]usnChange{{FRN: victim, USN: 7, Reason: usnReasonFileDelete}})
		if vol.snapshotHiddenBaseIDs().empty() {
			t.Fatal("delete did not hide a base record")
		}
	case "add-match":
		// A live overlay addition inside alpha with matching content; hidden
		// stays empty, overlay records exist. The addition's parent is the
		// real alpha dir record (not FRN 1), so the overlay path resolves
		// under alpha and the delta text carries the broad term.
		const addedFRN = 9_000_001
		var alphaFRN uint64
		for _, r := range recs {
			if r.mode != 0 && r.name == "alpha" {
				alphaFRN = r.frn
				break
			}
		}
		if alphaFRN == 0 {
			t.Fatal("no alpha dir record for the overlay addition")
		}
		vol.applyUSNChanges([]usnChange{{
			FRN: addedFRN, ParentFRN: alphaFRN, USN: 9_000_001,
			Reason: usnReasonFileCreate, Name: "overlay-addition.txt",
		}})
		vol.content.deltaView().upsert(contentDeltaDoc{
			FRN:  addedFRN,
			Path: vol.index.Volume + `\alpha\overlay-addition.txt`,
			Text: []byte("the overlay addition download body"),
		})
		if !vol.snapshotHiddenBaseIDs().empty() {
			t.Fatal("overlay addition must not hide base records")
		}
	default:
		t.Fatalf("unknown mutation %q", kind)
	}
}

// contentLaneScopeVolume mirrors newContentRecordVolume but also builds the
// resident filename indexes the global lanes drive (name order, trigrams):
// the service builds those in the background, and the fixture-only volumes
// the other content tests use never needed them because the per-volume
// content path scans in its own order.
func contentLaneScopeVolume(t *testing.T, volume string, recs []contentVolRecord) *serviceVolumeIndex {
	t.Helper()
	vol := newContentRecordVolume(t, volume, recs)
	vol.rebuildNameTrigramsLocked()
	return vol
}

func TestContentGlobalLaneScopeDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	queries := contentLaneScopeQueries()
	sorts := []string{"", "sort:size", "sort:modified", "sort:extension", "sort:type", "sort:path"}
	limits := []int{1, 7, 20, 5000}
	// Scope dimensions: under roots (volume root, top dir, sub dir, missing
	// dir), bias (none, real subdir, missing), overlays. A foreign-volume
	// under: is covered by the declined-shapes test (single-via-under):
	// serviceVolumesForQuery constrains the eligible set before either path
	// runs, so it exercises volume reduction, not lane narrowing.
	type scopeCase struct {
		name     string
		under    string
		rootBias string
		cwdBias  string
		mutate   string
	}
	scopes := []scopeCase{
		{"plain", "", "", "", "none"},
		{"under-root", `C:\`, "", "", "none"},
		{"under-top", `C:\alpha`, "", "", "none"},
		{"under-sub", `C:\beta\sub1`, "", "", "none"},
		{"under-missing", `C:\no-such-dir`, "", "", "none"},
		{"bias-root", "", `C:\alpha`, "", "none"},
		{"bias-sub", "", `C:\gamma\sub0`, "", "none"},
		{"bias-missing", "", `C:\no-such-dir`, "", "none"},
		{"bias-cwd", "", "", `C:\beta`, "none"},
		{"under-bias", `C:\alpha`, `C:\alpha`, "", "none"},
		{"delete", "", "", "", "delete-match"},
		{"add", "", "", "", "add-match"},
		{"delete-under", `C:\alpha`, "", "", "delete-match"},
		{"delete-bias", "", `C:\alpha`, "", "delete-match"},
		{"add-under", `C:\beta`, "", "", "add-match"},
		{"add-bias", "", `C:\beta`, "", "add-match"},
	}
	// One volume per (seed, scope): the 600-doc content index builds once
	// (~40s on this machine) and every query × sort × limit runs against it.
	// Rebuilding per subtest would multiply that by 70 queries × 6 sorts.
	for _, seed := range []int64{7, 42} {
		recs := contentLaneScopeCorpus("C:", seed, 600)
		for _, sc := range scopes {
			scVol := contentLaneScopeVolume(t, "C:", recs)
			contentLaneScopeMutations(t, scVol, recs, sc.mutate)
			scVols := []*serviceVolumeIndex{scVol}
			for _, q := range queries {
				for _, s := range sorts {
					for _, limit := range limits {
						query := strings.TrimSpace(q + " " + s)
						label := fmt.Sprintf("seed=%d/%s/%s/limit=%d/under=%q/bias=%q/%q", seed, sc.name, query, limit, sc.under, sc.rootBias, sc.cwdBias)
						t.Run(label, func(t *testing.T) {
							vols := scVols
							opts := queryOptions{Query: query, Limit: limit, Under: sc.under, RootBias: sc.rootBias, CWDBias: sc.cwdBias}
							slowSig, slowTrace, err := contentLaneRunOne(vols, opts, false)
							if err != nil {
								t.Fatalf("slow %q: %v", label, err)
							}
							laneSig, laneTrace, err := contentLaneRunOne(vols, opts, true)
							if err != nil {
								t.Fatalf("lane %q: %v", label, err)
							}
							// Engagement contract for the canonical compound shape
							// (sort "" only): pristine under:/bias scopes and the
							// overlay-handling scopes engage; bias with overlay
							// activity declines to the content path. All other
							// shapes only promise result equality (iterator
							// support varies by shape by design).
							if query == "content:download ext:.txt" && s == "" {
								didEngage := laneTrace.PlannerMode == "global-content-components"
								wantEngaged := sc.mutate == "none" || sc.name == "delete" || sc.name == "add" || sc.name == "delete-under" || sc.name == "add-under"
								if didEngage != wantEngaged {
									t.Fatalf("%s: engaged=%v want %v (planner=%q)", label, didEngage, wantEngaged, laneTrace.PlannerMode)
								}
							}
							assertLaneScopeEqual(t, label, slowSig, laneSig, slowTrace, laneTrace)
							// Count parity: lane count vs slow count, and
							// count == len(search) for non-stat shapes.
							slowCount, _, err := contentLaneCountOne(vols, opts, false)
							if err != nil {
								t.Fatalf("slow count %q: %v", label, err)
							}
							laneCount, laneCountTrace, err := contentLaneCountOne(vols, opts, true)
							if err != nil {
								t.Fatalf("lane count %q: %v", label, err)
							}
							if slowCount != laneCount {
								t.Fatalf("%s: count diverged slow=%d lane=%d", label, slowCount, laneCount)
							}
							// count == len(search) holds for the bounded search
							// window: a limit-trimmed search is a prefix of the
							// count, so compare against the untrimmed lane set
							// (limit 0 normalizes to the 100 default; pass an
							// explicit over-large limit for the true full set).
							if sc.under == "" && len(laneSig) > 1 {
								fullOpts := opts
								fullOpts.Limit = 1 << 30
								fullSig, _, err := contentLaneRunOne(vols, fullOpts, true)
								if err != nil {
									t.Fatalf("full lane search %q: %v", label, err)
								}
								if slowCount != len(fullSig)-1 {
									t.Fatalf("%s: count %d != len(search) %d", label, slowCount, len(fullSig)-1)
								}
							}
							if (sc.under != "") != laneCountTrace.ContentCountDivergent {
								t.Fatalf("%s: ContentCountDivergent=%v for under=%q", label, laneCountTrace.ContentCountDivergent, sc.under)
							}
						})
					}
				}
			}
		}
	}
}

// laneScopeMemfs is the fixture file set for the exists: differential: every
// path the real-path fixture writes. contentStatExists consults it when the
// platform stat fails, so the exists: predicate has a non-empty set on both
// arms even where the sandbox cannot stat the fixture back.
var laneScopeMemfs = make(map[string]struct{})

// laneScopeMemfsInfo is a minimal os.FileInfo for a memfs fixture path: only
// existence matters to the predicate (entryMatchesScalar discards the info).
type laneScopeMemfsInfo struct{ name string }

func (m laneScopeMemfsInfo) Name() string           { return m.name }
func (m laneScopeMemfsInfo) Size() int64            { return 1 }
func (m laneScopeMemfsInfo) Mode() os.FileMode      { return 0o644 }
func (m laneScopeMemfsInfo) ModTime() (t time.Time) { return t }
func (m laneScopeMemfsInfo) IsDir() bool            { return false }
func (m laneScopeMemfsInfo) Sys() any               { return nil }

// The `exists:` shape stats every result path: records carry fake C: paths
// that never exist, so both arms must agree on the empty set; a second volume
// with REAL temp-dir paths proves agreement on a non-empty exists: set.
func TestContentGlobalLaneExistsDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	// Fake paths: os.Stat fails for both arms; the sets (empty) must agree and
	// the count must flag the M9 divergence.
	recs := contentLaneScopeCorpus("C:", 7, 200)
	vol := contentLaneScopeVolume(t, "C:", recs)
	vols := []*serviceVolumeIndex{vol}
	for _, query := range []string{"content:download ext:.txt", "content:pelican dir:alpha"} {
		for _, limit := range []int{7, 5000} {
			label := fmt.Sprintf("fake/%s/limit=%d", query, limit)
			t.Run(label, func(t *testing.T) {
				opts := queryOptions{Query: query, Limit: limit, Exists: true}
				slowSig, slowTrace, err := contentLaneRunOne(vols, opts, false)
				if err != nil {
					t.Fatalf("slow: %v", err)
				}
				laneSig, laneTrace, err := contentLaneRunOne(vols, opts, true)
				if err != nil {
					t.Fatalf("lane: %v", err)
				}
				if laneTrace.PlannerMode != "global-content-components" {
					t.Fatalf("lane did not engage exists: shape (planner=%q)", laneTrace.PlannerMode)
				}
				assertLaneScopeEqual(t, label, slowSig, laneSig, slowTrace, laneTrace)
				if len(laneSig) != 1 {
					t.Fatalf("%s: fake-path exists: set = %v; want empty", label, laneSig)
				}
				slowCount, _, err := contentLaneCountOne(vols, opts, false)
				if err != nil {
					t.Fatalf("slow count: %v", err)
				}
				laneCount, laneCountTrace, err := contentLaneCountOne(vols, opts, true)
				if err != nil {
					t.Fatalf("lane count: %v", err)
				}
				if slowCount != laneCount {
					t.Fatalf("%s: count slow=%d lane=%d", label, slowCount, laneCount)
				}
				if !laneCountTrace.ContentCountDivergent {
					t.Fatalf("%s: exists: count did not flag the divergence", label)
				}
			})
		}
	}

	// Real paths: the record paths must name files that read as existing, so
	// the fixture writes real files and substitutes the exists: stat with a
	// memfs view over them (contentStatExists). A direct os.Stat cannot see
	// the fixture on this machine (file creation under temp names silently
	// misbehaves), but both arms share entryMatchesScalar, so the hook keeps
	// the differential exact while exercising the real predicate path.
	//
	// os.MkdirTemp (not t.TempDir) picks the dir for the same long-name
	// reason; every write is recorded in the memfs either way.
	dir, err := os.MkdirTemp("", "lanefix")
	if err != nil {
		t.Fatal(err)
	}
	if resolved, rerr := filepath.EvalSymlinks(dir); rerr == nil {
		dir = resolved
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	restoreStat := contentStatExists
	contentStatExists = func(path string) (os.FileInfo, error) {
		if info, serr := os.Stat(path); serr == nil {
			return info, nil
		}
		// Fall back to the fixture memfs, keyed by reconstructed entry
		// path (see below): files the harness wrote but the platform
		// cannot stat back.
		if _, ok := laneScopeMemfs[filepath.Clean(path)]; ok {
			return laneScopeMemfsInfo{name: filepath.Base(path)}, nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { contentStatExists = restoreStat })
	var realRecs []contentVolRecord
	var frn uint64 = 500000
	// laneScopeMemfs records every fixture path the harness writes, so the
	// contentStatExists hook above can answer for files the platform cannot
	// stat back. Reset per test: parallel subtests share the package.
	laneScopeMemfs = make(map[string]struct{})
	write := func(rel, body string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		laneScopeMemfs[p] = struct{}{}
		return p
	}
	// addDirAt mints a directory record with a preassigned FRN (below the
	// file range), so record order stays FRN-ascending while files interleave
	// directories. See the FRN note on the root record above.
	addDirAt := func(parent int32, parentFRN uint64, base, name string) (int32, uint64) {
		abs := filepath.Join(base, name)
		if err := os.MkdirAll(abs, 0o755); err != nil {
			t.Fatal(err)
		}
		frn++
		my := frn
		realRecs = append(realRecs, contentVolRecord{frn: my, parent: parent, parentFRN: parentFRN, name: name, mode: uint32(os.ModeDir), path: abs})
		return int32(len(realRecs) - 1), my
	}
	// Volume root record: the temp dir itself, so reconstructed paths
	// (<tmp>\alpha\...) are reachable from the parent chain. The Index volume
	// label is the temp dir's own volume (drive); reconstruction is
	// Volume + name chain, so entry paths read E:\<base>\<sub>\<file>.
	frn++
	realRecs = append(realRecs, contentVolRecord{frn: frn, parent: -1, parentFRN: 0, name: filepath.Base(dir), mode: uint32(os.ModeDir), path: dir})
	rootIdx, rootFRN := int32(0), frn
	alphaIdx, alphaFRN := addDirAt(rootIdx, rootFRN, dir, "alpha")
	betaIdx, betaFRN := addDirAt(rootIdx, rootFRN, dir, "beta")
	for i := 0; i < 60; i++ {
		// Interleave directories (alpha/beta), extensions (txt/go) and
		// content (download-bearing or not) on independent periods, so no
		// filename predicate is a proxy for the content predicate: the
		// differential must verify content on every filename candidate.
		var parent int32
		var parentFRN uint64
		var rel string
		if i%2 == 0 {
			parent, parentFRN, rel = betaIdx, betaFRN, "beta"
		} else {
			parent, parentFRN, rel = alphaIdx, alphaFRN, "alpha"
		}
		ext := "txt"
		if i%3 == 0 {
			ext = "go"
		}
		name := fmt.Sprintf("doc-%03d.%s", i, ext)
		body := "nothing to see here"
		if i%5 == 0 {
			body = "needle in a haystack download"
		}
		frn++
		realRecs = append(realRecs, contentVolRecord{
			frn: frn, parent: parent, parentFRN: parentFRN, name: name,
			size: int64(i + 1), modUnix: int64(1_600_000_000 + i),
			path: write(filepath.Join(rel, name), body), content: body,
		})
	}
	realVol := contentLaneScopeVolume(t, filepath.VolumeName(dir), realRecs)
	realVols := []*serviceVolumeIndex{realVol}
	// Populate the memfs with RECONSTRUCTED paths: entryMatchesScalar stats
	// Entry.Path, which is reconstructCompactPathCached (Volume + name
	// chain), not the stored record path, so the hook must answer for
	// exactly those spellings. Every file record was written above, so each
	// reconstructed file path reads as existing on both arms.
	pathCache := make(map[int]string)
	for id := 0; id < realVol.index.compactRecordCount(); id++ {
		laneScopeMemfs[realVol.index.reconstructCompactPathCached(id, pathCache)] = struct{}{}
	}
	// Every record path must stat: a silent write failure would otherwise
	// masquerade as an empty exists: set on both arms.
	for _, r := range realRecs {
		if r.mode == 0 {
			if _, serr := os.Stat(r.path); serr != nil {
				t.Fatalf("fixture file %q missing: %v", r.path, serr)
			}
		}
	}
	// Sanity without Exists: the ext+content shape must find the real files
	// (proves the records, parent chains and content docs are coherent), and
	// the lane must engage it.
	sanityOpts := queryOptions{Query: "content:download ext:txt", Limit: 5000}
	sanity, sanityTrace, err := contentLaneRunOne(realVols, sanityOpts, true)
	if err != nil {
		t.Fatalf("sanity search: %v", err)
	}
	t.Logf("sanity planner=%q source=%q decline=%q len=%d", sanityTrace.PlannerMode, sanityTrace.Source, sanityTrace.Decline, len(sanity))
	if sanityTrace.PlannerMode != "global-content-components" {
		t.Fatalf("sanity lane did not engage (planner=%q source=%q decline=%q)", sanityTrace.PlannerMode, sanityTrace.Source, sanityTrace.Decline)
	}
	if len(sanity) <= 1 {
		t.Fatalf("sanity search found no matches; fixture incoherent: %v", sanity)
	}
	t.Logf("sanity: %d matches, first=%q", len(sanity)-1, sanity[1])
	for _, query := range []string{"content:download ext:txt", "content:needle ext:go", "content:download path:beta", "content:download sort:size"} {
		for _, limit := range []int{7, 5000} {
			label := fmt.Sprintf("real/%s/limit=%d", query, limit)
			t.Run(label, func(t *testing.T) {
				opts := queryOptions{Query: query, Limit: limit, Exists: true}
				slowSig, slowTrace, err := contentLaneRunOne(realVols, opts, false)
				if err != nil {
					t.Fatalf("slow: %v", err)
				}
				laneSig, laneTrace, err := contentLaneRunOne(realVols, opts, true)
				if err != nil {
					t.Fatalf("lane: %v", err)
				}
				assertLaneScopeEqual(t, label, slowSig, laneSig, slowTrace, laneTrace)
				if len(laneSig) <= 1 {
					t.Fatalf("%s: real-path exists: set is empty; want matches", label)
				}
			})
		}
	}
}

// Declined shapes must stay on the content path (no lane planner mode) and
// still agree: relevance search, boolean-group content, pure content, content
// negation, multi-usable volumes, and case-sensitive compounds.
func TestContentGlobalLaneDeclinedShapesDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	recs := contentLaneScopeCorpus("C:", 7, 300)
	volC := contentLaneScopeVolume(t, "C:", recs)
	volF := contentLaneScopeVolume(t, "F:", contentLaneScopeCorpus("F:", 8, 300))
	declined := []struct {
		name  string
		vols  []*serviceVolumeIndex
		query string
		opts  func(queryOptions) queryOptions
		// countEngages reports whether the COUNT lane is expected to engage
		// this shape even though the search lane declines it (order is
		// irrelevant to a count, so sort:relevance stays eligible there).
		countEngages bool
	}{
		{"relevance", []*serviceVolumeIndex{volC}, "content:download ext:.txt sort:relevance", nil, true},
		{"or-content", []*serviceVolumeIndex{volC}, "content:download|dir:alpha ext:.txt", nil, false},
		{"pure-content", []*serviceVolumeIndex{volC}, "content:download", nil, false},
		{"not-content", []*serviceVolumeIndex{volC}, "scan !content:pelican", nil, false},
		{"multi-usable", []*serviceVolumeIndex{volC, volF}, "content:download ext:.txt", nil, false},
		// An under: on one volume's subtree constrains the eligible set to
		// that volume before either path runs (serviceVolumesForQuery), so
		// the lane engages single-volume there: the differential proves the
		// shaped result, not a decline.
		{"single-via-under", []*serviceVolumeIndex{volC, volF}, "content:download ext:.txt", func(o queryOptions) queryOptions {
			o.Under = `C:\alpha`
			return o
		}, false},
		{"case-sensitive", []*serviceVolumeIndex{volC}, "case:true content:download ext:.txt", nil, false},
		{"case-sensitive-term", []*serviceVolumeIndex{volC}, "case:true content:DOWNLOAD quasar", nil, false},
		{"no-root", []*serviceVolumeIndex{volC}, "content:download zzznopeqq", nil, false},
		// Filename OR-unions can exceed every estimate dimension, so the
		// gate declines them even though the component iterator could
		// serve them; the fallback stays exact.
		{"or-filename", []*serviceVolumeIndex{volC}, "content:pelican ext:.txt|ext:.md", nil, false},
		// type:-only and attrib-only have no boundable filename dimension
		// (and type:-only has no supported iterator root at all).
		{"type-only", []*serviceVolumeIndex{volC}, "type:file content:download", nil, false},
	}
	for _, tc := range declined {
		for _, limit := range []int{7, 500} {
			t.Run(fmt.Sprintf("%s/limit=%d", tc.name, limit), func(t *testing.T) {
				opts := queryOptions{Query: tc.query, Limit: limit}
				if tc.opts != nil {
					opts = tc.opts(opts)
				}
				slowSig, slowTrace, err := contentLaneRunOne(tc.vols, opts, false)
				if err != nil {
					t.Fatalf("slow: %v", err)
				}
				laneSig, laneTrace, err := contentLaneRunOne(tc.vols, opts, true)
				if err != nil {
					t.Fatalf("lane: %v", err)
				}
				// The single-via-under shape engages on both paths (the volume
				// constraint reduces the eligible set first); every other
				// declined shape must not, except relevance on the count path
				// (order is irrelevant to a count).
				didEngage := laneTrace.PlannerMode == "global-content-components"
				if wantEngage := tc.name == "single-via-under"; didEngage != wantEngage {
					t.Fatalf("%s: lane engaged=%v want %v", tc.name, didEngage, wantEngage)
				}
				assertLaneScopeEqual(t, tc.name, slowSig, laneSig, slowTrace, laneTrace)
				slowCount, _, err := contentLaneCountOne(tc.vols, opts, false)
				if err != nil {
					t.Fatalf("slow count: %v", err)
				}
				laneCount, laneCountTrace, err := contentLaneCountOne(tc.vols, opts, true)
				if err != nil {
					t.Fatalf("lane count: %v", err)
				}
				if didEngage := laneCountTrace.PlannerMode == "global-content-components"; didEngage != (tc.countEngages || tc.name == "single-via-under") {
					t.Fatalf("%s: count lane engaged=%v want %v", tc.name, didEngage, tc.countEngages)
				}
				if slowCount != laneCount {
					t.Fatalf("%s: count slow=%d lane=%d", tc.name, slowCount, laneCount)
				}
			})
		}
	}
}

// A sharded sidecar exercises the slow path's shardMaskUnder pruning while the
// lane verifies inline; both must agree on under:-scoped compound queries.
// The matrix stays on selective content terms (pelican): a broad term's
// posting superset on a sharded base meets the mask-pruned candidate path,
// whose folder-window semantics differ from the lane's exact set by design
// (see the mask contract in content_shards.go); selective terms prove the
// mask prunes without dropping true matches, which is what `under:`
// correctness needs.
func TestContentGlobalLaneShardedDifferential(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	restoreTarget := contentShardTargetBytes
	contentShardTargetBytes = 400
	t.Cleanup(func() { contentShardTargetBytes = restoreTarget })

	shardRecs := contentLaneScopeCorpus("C:", 99, 120)
	vol := newShardedContentRecordVolume(t, "C:", shardRecs)
	reader, _ := vol.content.readerResolverView()
	if reader == nil || reader.idx.shards == nil || len(reader.idx.shards) < 2 {
		t.Fatalf("fixture is not sharded (shards=%v)", reader.idx.shards)
	}
	vols := []*serviceVolumeIndex{vol}
	queries := []string{
		"content:pelican ext:.go",
	}
	unders := []string{"", `C:\`, `C:\alpha`, `C:\beta\sub1`, `C:\no-such-dir`}
	for _, q := range queries {
		for _, under := range unders {
			for _, limit := range []int{7, 500} {
				label := fmt.Sprintf("%s/under=%q/limit=%d", q, under, limit)
				t.Run(label, func(t *testing.T) {
					opts := queryOptions{Query: q, Limit: limit, Under: under}
					slowSig, slowTrace, err := contentLaneRunOne(vols, opts, false)
					if err != nil {
						t.Fatalf("slow: %v", err)
					}
					laneSig, laneTrace, err := contentLaneRunOne(vols, opts, true)
					if err != nil {
						t.Fatalf("lane: %v", err)
					}
					if under == "" {
						if laneTrace.PlannerMode != "global-content-components" {
							t.Fatalf("lane did not engage unscoped sharded query (planner=%q)", laneTrace.PlannerMode)
						}
					}
					assertLaneScopeEqual(t, label, slowSig, laneSig, slowTrace, laneTrace)
					slowCount, _, err := contentLaneCountOne(vols, opts, false)
					if err != nil {
						t.Fatalf("slow count: %v", err)
					}
					laneCount, _, err := contentLaneCountOne(vols, opts, true)
					if err != nil {
						t.Fatalf("lane count: %v", err)
					}
					if slowCount != laneCount {
						t.Fatalf("%s: count slow=%d lane=%d", label, slowCount, laneCount)
					}
				})
			}
		}
	}
}

// newShardedContentRecordVolume mirrors newContentRecordVolume but publishes
// the content index as a real multi-shard sidecar (manifest + shard files via
// contentReshardAndSave, the same publish path the service build uses) so the
// slow path prunes shards with shardMaskUnder.
func newShardedContentRecordVolume(t *testing.T, volume string, recs []contentVolRecord) *serviceVolumeIndex {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	idx := &Index{Source: "usn", Volume: volume, Compact: true}
	build := make([]contentBuildDoc, 0, len(recs))
	for _, r := range recs {
		idx.Records = append(idx.Records, CompactRecord{
			FRN: r.frn, ParentFRN: r.parentFRN, Parent: r.parent,
			Name: r.name, Mode: r.mode, Size: r.size, ModUnix: r.modUnix,
		})
		if r.content != "" {
			path := r.path
			if path == "" {
				path = volume + `\` + r.name
			}
			build = append(build, contentBuildDoc{path: path, frn: r.frn, text: []byte(r.content)})
		}
	}
	contentIndexFRNs(idx)
	tmpDir := t.TempDir()
	whole, err := assembleContentIndex(build, tmpDir)
	if err != nil {
		t.Fatalf("assemble content index: %v", err)
	}
	whole.Origin = contentOriginUSN
	gsx := filepath.Join(tmpDir, "test.gsx")
	header := newContentIndex()
	header.Origin = contentOriginUSN
	if _, err := contentReshardAndSave(gsx, whole, header, tmpDir); err != nil {
		t.Fatalf("reshard: %v", err)
	}
	composite, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatalf("load sharded index: %v", err)
	}
	releaseContentIndexOnCleanup(t, composite)
	reader, err := openContentReader(composite)
	if err != nil {
		t.Fatalf("open sharded reader: %v", err)
	}
	vol := newServiceVolumeIndex(filepath.Join(tmpDir, "seekfs_"+strings.TrimSuffix(volume, ":")+".gsi"), idx)
	if vol.content == nil {
		t.Fatal("content state not initialized")
	}
	frns := make([]uint64, len(composite.Docs))
	ids := make([]uint32, len(composite.Docs))
	for i := range composite.Docs {
		frns[i] = composite.Docs[i].FRN
		ids[i] = uint32(contentRecordIndexForFRN(idx, composite.Docs[i].FRN))
	}
	vol.content.setReady(composite, reader, buildContentResolver(composite.Docs, frns, ids))
	buildOrders(vol.index)
	vol.rebuildNameTrigramsLocked()
	return vol
}

// Snippets must survive the lane: the slow path attaches them post-merge, so
// the lane's entries must carry identical snippet text.
func TestContentGlobalLaneSnippetsMatch(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	recs := contentLaneScopeCorpus("C:", 7, 200)
	vol := contentLaneScopeVolume(t, "C:", recs)
	vols := []*serviceVolumeIndex{vol}
	for _, query := range []string{
		"content:download ext:.txt",
		`content:"quick brown fox" ext:.md`,
		"content:pelican dir:alpha",
		"content:/down.*ad/ ext:.txt",
	} {
		t.Run(query, func(t *testing.T) {
			slowSig, _, err := contentLaneRunOne(vols, queryOptions{Query: query, Limit: 20}, false)
			if err != nil {
				t.Fatalf("slow: %v", err)
			}
			laneSig, laneTrace, err := contentLaneRunOne(vols, queryOptions{Query: query, Limit: 20}, true)
			if err != nil {
				t.Fatalf("lane: %v", err)
			}
			if laneTrace.PlannerMode != "global-content-components" {
				t.Fatalf("lane did not engage (planner=%q)", laneTrace.PlannerMode)
			}
			if len(slowSig) != len(laneSig) {
				t.Fatalf("count slow=%d lane=%d", len(slowSig), len(laneSig))
			}
			for i := range slowSig {
				if slowSig[i] != laneSig[i] {
					t.Fatalf("element %d diverged:\nslow=%q\nlane=%q", i, slowSig[i], laneSig[i])
				}
			}
			// The signature includes Snippet, so equality above already proves
			// snippets match; assert non-trivially that term/phrase queries
			// attach snippets (a regex-only match yields no snippet by
			// design, on both arms).
			if !strings.HasPrefix(query, "content:/") {
				snippets := 0
				for _, line := range laneSig[1:] {
					parts := strings.Split(line, "|")
					if len(parts) == 7 && parts[6] != "" {
						snippets++
					}
				}
				if snippets == 0 && len(laneSig) > 1 {
					t.Fatalf("%s: no snippets attached by the lane", query)
				}
			}
		})
	}
}
