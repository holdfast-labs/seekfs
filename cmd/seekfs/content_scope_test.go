package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errTestReserve = errors.New("test reserve stub")

// stampContentTestScope makes hand-built USN fixtures represent a sidecar
// produced under the current default service scope.
func stampContentTestScope(volume string, idx *contentIndex) {
	idx.ScopeHash = contentScopeForVolume(appConfig{}, volume).resolve(volume, nil).fingerprint()
}

func TestContentScopeSectionParsing(t *testing.T) {
	cases := map[string]string{
		"content":       "content",
		`content."C:"`:  "content:C:",
		"content.C:":    "content:C:",
		"content.F:":    "content:F:",
		"unrelated":     "ignore",
		"content_extra": "ignore",
	}
	for in, want := range cases {
		if got := parseContentConfigSection(in); got != want {
			t.Errorf("parseContentConfigSection(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestContentScopeForVolumeDefaults(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SCOPE", "")
	t.Setenv("SEEKFS_CONTENT_ROOTS", "")
	t.Setenv("SEEKFS_CONTENT_EXCLUDE", "")
	s := contentScopeForVolume(appConfig{}, "C:")
	if s.Mode != contentScopeAuto || !s.IncludeGit || !s.SystemExcludes {
		t.Fatalf("defaults = %+v; want auto/git/system", s)
	}
	// Without config there is no byte budget: the default indexes
	// everything selected and stops only at the volume's free-space
	// reserve, so it stays out of the fingerprint.
	if s.BudgetBytes != 0 {
		t.Fatalf("default budget = %d; want 0 (unlimited, reserve-governed)", s.BudgetBytes)
	}
	if s.BudgetExplicit {
		t.Fatal("default budget must not be explicit")
	}
}

func TestContentScopeForVolumeConfigAndEnv(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SCOPE", "")
	t.Setenv("SEEKFS_CONTENT_ROOTS", "")
	t.Setenv("SEEKFS_CONTENT_EXCLUDE", "")
	no := false
	cfg := appConfig{
		Content: contentScopeConfig{
			Mode:  "explicit",
			Roots: []string{`C:\Users\me\src`},
			Git:   &no,
		},
		ContentByVolume: map[string]contentScopeConfig{
			"F:": {Roots: []string{`F:\proj`}},
		},
	}
	c := contentScopeForVolume(cfg, "C:")
	if c.Mode != contentScopeExplicit || c.IncludeGit {
		t.Fatalf("global override = %+v; want explicit/no-git", c)
	}
	if len(c.Roots) != 1 || c.Roots[0] != contentNormPath(`C:\Users\me\src`) {
		t.Fatalf("global roots = %v", c.Roots)
	}
	f := contentScopeForVolume(cfg, "F:")
	if len(f.Roots) != 1 || f.Roots[0] != contentNormPath(`F:\proj`) {
		t.Fatalf("F: override roots = %v; want the per-volume root", f.Roots)
	}

	t.Setenv("SEEKFS_CONTENT_ROOTS", `C:\env`)
	e := contentScopeForVolume(appConfig{}, "C:")
	if len(e.Roots) != 1 || e.Roots[0] != contentNormPath(`C:\env`) {
		t.Fatalf("env roots = %v", e.Roots)
	}
	t.Setenv("SEEKFS_CONTENT_SCOPE", "off")
	if got := contentScopeForVolume(appConfig{}, "C:"); !got.disabled() {
		t.Fatalf("env mode off not applied: %+v", got)
	}
}

func TestContentScopeInvalidFallsBack(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SCOPE", "")
	t.Setenv("SEEKFS_CONTENT_ROOTS", "")
	t.Setenv("SEEKFS_CONTENT_EXCLUDE", "")
	cfg := appConfig{Content: contentScopeConfig{Mode: "bogus", BudgetBytes: -5}}
	s := contentScopeForVolume(cfg, "C:")
	if s.Mode != contentScopeAuto {
		t.Fatalf("invalid mode = %q; want auto", s.Mode)
	}
	if s.BudgetBytes != 0 || s.BudgetExplicit {
		t.Fatalf("invalid budget = %+v; want the unlimited default", s)
	}
}

func TestLoadConfigContentSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seekfs.toml")
	body := "# comment\n" +
		"default_limit = 50\n" +
		"[content]\n" +
		"mode = \"explicit\"\n" +
		"roots = [\"C:\\\\src\", \"C:\\\\work\"]\n" +
		"git = false\n" +
		"[content.\"f:\"]\n" +
		"roots = [\"F:\\\\proj\"]\n" +
		"[other]\n" +
		"mode = \"ignored\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultLimit != 50 {
		t.Fatalf("default_limit = %d; want 50", cfg.DefaultLimit)
	}
	if cfg.Content.Mode != "explicit" || len(cfg.Content.Roots) != 2 || cfg.Content.Git == nil || *cfg.Content.Git {
		t.Fatalf("global content = %+v", cfg.Content)
	}
	if ov, ok := cfg.ContentByVolume["F:"]; !ok || len(ov.Roots) != 1 {
		t.Fatalf("per-volume content = %+v", cfg.ContentByVolume)
	}
	// The ignored section must not leak into top-level parsing.
	if cfg.ContentByVolume["other"].Mode == "ignored" {
		t.Fatal("unknown section leaked into content config")
	}
}

func TestContentScopeResolvedAllows(t *testing.T) {
	s := defaultContentScope()
	r := s.resolve("C:", []string{contentNormPath(`C:\proj`)})
	cases := []struct {
		path string
		ok   bool
		kind string
	}{
		{`C:\proj\src\main.go`, true, "repo"},
		{`C:\proj\main.go`, true, "repo"},
		{`C:\proj\node_modules\x.js`, false, ""},
		{`C:\proj\build\out.txt`, false, ""},
		{`C:\proj\.git\config`, false, ""},
		{`C:\proj\main.exe`, false, ""}, // known-binary: rejected without opening
		{`C:\proj\photo.png`, false, ""},
		{`C:\proj\Makefile`, true, "repo"}, // extensionless: admitted, sniffed at build
		{`C:\proj\notes.xyz`, true, "repo"},
		{`C:\proj\OWNERS`, true, "repo"},
		{`C:\other\readme.md`, false, ""},
		{`C:\Windows\a.txt`, false, ""},
	}
	for _, c := range cases {
		unit, kind, ok := r.allows(c.path)
		if ok != c.ok || (ok && kind != c.kind) {
			t.Errorf("allows(%q) = (%q,%q,%v); want ok=%v kind=%q", c.path, unit, kind, ok, c.ok, c.kind)
		}
	}
	if !r.SniffText {
		t.Error("default scope must select by sniffing, not by allowlist")
	}

	// Explicit mode: only configured roots (plus repos when IncludeGit).
	exp := defaultContentScope()
	exp.Mode = contentScopeExplicit
	exp.Roots = normalizeContentPaths([]string{`C:\src`})
	re := exp.resolve("C:", []string{contentNormPath(`C:\proj`)})
	if _, _, ok := re.allows(`C:\src\a.go`); !ok {
		t.Error("explicit root file not allowed")
	}
	if _, _, ok := re.allows(`C:\other\a.go`); ok {
		t.Error("explicit mode allowed a file outside its roots")
	}
	exp.Roots = nil
	exp.IncludeGit = false
	if _, _, ok := exp.resolve("C:", []string{contentNormPath(`C:\proj`)}).allows(`C:\proj\a.go`); ok {
		t.Error("git=false allowed an unconfigured repo")
	}
	exp.Roots = normalizeContentPaths([]string{`C:\src`})
	exp.Mode = contentScopeOff
	if _, _, ok := exp.resolve("C:", nil).allows(`C:\src\a.go`); ok {
		t.Error("off mode allowed a file")
	}
	exp.Mode = contentScopeExplicit
	exp.SystemExcludes = false
	if _, _, ok := exp.resolve("C:", nil).allows(`C:\src\build\a.go`); !ok {
		t.Error("system_excludes=false rejected a generated directory")
	}

	// An explicit extension allowlist stays a verbatim gate: unknown
	// extensions are rejected without sniffing, and sniff mode is off.
	exp.Exts = contentExtSet([]string{".go"})
	xl := exp.resolve("C:", nil)
	if xl.SniffText {
		t.Error("explicit exts must disable sniff selection")
	}
	if _, _, ok := xl.allows(`C:\src\a.go`); !ok {
		t.Error("explicit allowlist rejected a listed extension")
	}
	if _, _, ok := xl.allows(`C:\src\Makefile`); ok {
		t.Error("explicit allowlist admitted an unlisted extensionless file")
	}
	if _, _, ok := xl.allows(`C:\src\n.png`); ok {
		t.Error("explicit allowlist admitted an unlisted extension")
	}
}

func TestContentScopeFingerprintAndPriority(t *testing.T) {
	s := defaultContentScope()
	s.Roots = normalizeContentPaths([]string{`C:\work`, `C:\notes`})
	a := s.resolve("C:", []string{contentNormPath(`C:\repo`)})
	s.Roots[0], s.Roots[1] = s.Roots[1], s.Roots[0]
	b := s.resolve("C:", []string{contentNormPath(`C:\repo`)})
	if a.fingerprint() != b.fingerprint() {
		t.Fatal("root order changed scope identity")
	}
	b.Excludes = append(b.Excludes, contentNormPath(`C:\repo\private`))
	if a.fingerprint() == b.fingerprint() {
		t.Fatal("narrowed scope kept old identity")
	}
	if got := a.priority(`C:\repo\a.txt`); got != 0 {
		t.Fatalf("repo priority = %d", got)
	}
	if got := a.priority(`C:\work\a.txt`); got != 1 {
		t.Fatalf("explicit root priority = %d", got)
	}
	if got := a.priority(`C:\else\a.txt`); got != 3 {
		t.Fatalf("unmatched priority = %d", got)
	}
}

func TestContentScopeEstimateAndDetect(t *testing.T) {
	d := uint32(os.ModeDir)
	recs := []CompactRecord{
		{FRN: 1, Parent: -1, Mode: d, Name: "."},
		{FRN: 2, Parent: 0, ParentFRN: 1, Mode: d, Name: "proj"},
		{FRN: 3, Parent: 1, ParentFRN: 2, Mode: d, Name: ".git"},
		{FRN: 4, Parent: 1, ParentFRN: 2, Name: "main.go", Size: 100},
		{FRN: 5, Parent: 1, ParentFRN: 2, Mode: d, Name: "node_modules"},
		{FRN: 6, Parent: 4, ParentFRN: 5, Name: "index.js", Size: 50},
		{FRN: 7, Parent: 1, ParentFRN: 2, Mode: d, Name: "build"},
		{FRN: 8, Parent: 6, ParentFRN: 7, Name: "out.txt", Size: 20},
		{FRN: 9, Parent: 0, ParentFRN: 1, Name: "readme.md", Size: 10},
		{FRN: 10, Parent: 1, ParentFRN: 2, Name: "util.go", Size: 40},
	}
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Roots: []string{`C:\`}, Records: recs}
	est := scanContentScope(idx, defaultContentScope(), "C:", 0, 0, false)
	if len(est.Repos) != 1 || est.Repos[0] != contentNormPath(`C:\proj`) {
		t.Fatalf("repos = %v; want [c:\\proj]", est.Repos)
	}
	// Inside a worktree git ignore rules replace the name-based excludes;
	// C:\proj has no ignore files, so node_modules and build are indexed.
	if est.Files != 4 || est.Bytes != 210 {
		t.Fatalf("estimate files=%d bytes=%d; want 4/210 (everything under proj)", est.Files, est.Bytes)
	}
	if len(est.ByUnit) != 1 || est.ByUnit[0].Kind != "repo" || est.ByUnit[0].Files != 4 {
		t.Fatalf("by-unit = %+v", est.ByUnit)
	}
	if est.Excluded["exclude"] == 0 || est.Excluded["ext"] != 0 {
		t.Fatalf("excluded = %+v; want exclude>0, ext=0", est.Excluded)
	}
	// The projection scales extracted bytes by the compact-encoding
	// multiplier (< 1), so it sits below the raw byte count.
	if want := int64(float64(est.Bytes) * contentScopeEncodedMultiplier); est.ProjectedGSXBytes != want {
		t.Fatalf("projected %d; want %d (bytes x multiplier)", est.ProjectedGSXBytes, want)
	}

	// MaxFiles truncation is surfaced: two includable files, cap 1.
	if capped := scanContentScope(idx, defaultContentScope(), "C:", 1, 0, false); !capped.MaxFilesHit || capped.Files != 1 {
		t.Fatalf("maxFiles=1 => files=%d hit=%v; want 1/true", capped.Files, capped.MaxFilesHit)
	}
	budgeted := defaultContentScope()
	budgeted.BudgetBytes = 110
	budgeted.BudgetExplicit = true
	if capped := scanContentScope(idx, budgeted, "C:", 0, 0, false); !capped.BudgetHit || capped.Files != 1 || capped.Bytes != 20 {
		t.Fatalf("budget=110 => files=%d bytes=%d hit=%v; want 1/20/true (build/out.txt sorts first)", capped.Files, capped.Bytes, capped.BudgetHit)
	} else if capped.BudgetBytes != 110 || capped.EligibleFiles != 4 || capped.EligibleBytes != 210 {
		t.Fatalf("budget=110 => budget=%d eligible=%d/%d; want 110, 4 files, 210 bytes", capped.BudgetBytes, capped.EligibleFiles, capped.EligibleBytes)
	} else if capped.TextCandidates != 0 || capped.BinaryCandidates != 0 {
		t.Fatalf("budget=110 => text/binary candidates = %d/%d; want 0/0 (all extensions known)", capped.TextCandidates, capped.BinaryCandidates)
	}
}

func TestContentBinarySkipListDisjointFromExtractors(t *testing.T) {
	claimed := make(map[string]string)
	for _, e := range contentExtractors {
		if e.Name() == "text" {
			continue
		}
		for _, ext := range e.Extensions() {
			claimed[ext] = e.Name()
		}
	}
	for _, want := range contentDefaultTextExtensions {
		claimed[want] = "text-allowlist"
	}
	for ext := range contentBinaryExtensions {
		if ext == "" || ext[0] != '.' || ext != strings.ToLower(ext) {
			t.Fatalf("skip list entry %q must be lowercase with a leading dot", ext)
		}
		if by, ok := claimed[ext]; ok {
			t.Fatalf("skip list entry %q is claimed by %s; the build would never open it", ext, by)
		}
	}
	// Spot checks: obvious binaries skipped, text-ish formats sniffed.
	for _, ext := range []string{".png", ".zip", ".mp4", ".exe", ".ttf", ".doc", ".gz"} {
		if !contentBinaryExtension(ext) {
			t.Errorf("extension %q must be in the binary skip list", ext)
		}
	}
	for _, ext := range []string{"", ".go", ".md", ".pdf", ".docx", ".sha1", ".gni", ".dat", ".svg", ".eps"} {
		if contentBinaryExtension(ext) {
			t.Errorf("extension %q must not be skipped; the sniff decides", ext)
		}
	}
}

func TestContentScopeEstimateSniffBuckets(t *testing.T) {
	d := uint32(os.ModeDir)
	recs := []CompactRecord{
		{FRN: 1, Parent: -1, Mode: d, Name: "."},
		{FRN: 2, Parent: 0, ParentFRN: 1, Mode: d, Name: "proj"},
		{FRN: 3, Parent: 1, ParentFRN: 2, Mode: d, Name: ".git"},
		{FRN: 4, Parent: 1, ParentFRN: 2, Name: "main.go", Size: 100},
		{FRN: 5, Parent: 1, ParentFRN: 2, Name: "OWNERS", Size: 30},
		{FRN: 6, Parent: 1, ParentFRN: 2, Name: "notes.xyz", Size: 40},
		{FRN: 7, Parent: 1, ParentFRN: 2, Name: "logo.png", Size: 500},
		{FRN: 8, Parent: 1, ParentFRN: 2, Name: "data.bin", Size: 600},
	}
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Roots: []string{`C:\`}, Records: recs}
	est := scanContentScope(idx, defaultContentScope(), "C:", 0, 0, false)
	if est.Files != 3 || est.EligibleFiles != 3 {
		t.Fatalf("estimate files=%d eligible=%d; want 3 (main.go, OWNERS, notes.xyz)", est.Files, est.EligibleFiles)
	}
	if est.TextCandidates != 2 {
		t.Fatalf("text candidates = %d; want 2 (OWNERS, notes.xyz)", est.TextCandidates)
	}
	if est.BinaryCandidates != 2 || est.Excluded["binary"] != 2 {
		t.Fatalf("binary = %d/%d; want 2 (logo.png, data.bin)", est.BinaryCandidates, est.Excluded["binary"])
	}
	if est.Excluded["ext"] != 0 {
		t.Fatalf("excluded = %+v; want no ext bucket in sniff mode", est.Excluded)
	}
	// An explicit allowlist restores the ext bucket and disables candidates.
	exp := defaultContentScope()
	exp.Mode = contentScopeExplicit
	exp.Exts = contentExtSet([]string{".go"})
	xest := scanContentScope(idx, exp, "C:", 0, 0, false)
	if xest.Files != 1 || xest.Excluded["ext"] != 4 || xest.TextCandidates != 0 {
		t.Fatalf("explicit estimate = %+v; want 1 file, 4 ext-excluded, 0 candidates", xest)
	}
}

func TestContentEstimateSniffSampleProjectsBinary(t *testing.T) {
	dir := t.TempDir()
	// Deterministic record order: 10 binary candidates, then 20 text ones, so
	// a stride sample hits a fixed binary count. Names carry no extension, so
	// every file is a sniff text candidate.
	type candidateFile struct {
		rel, body string
	}
	var ordered []candidateFile
	for i := 0; i < 10; i++ {
		rel := "b/candidate-" + string(rune('a'+i))
		ordered = append(ordered, candidateFile{rel: rel, body: "binary\x00body"})
	}
	for i := 0; i < 20; i++ {
		rel := "t/candidate-" + string(rune('a'+i%10)) + string(rune('0'+i/10))
		ordered = append(ordered, candidateFile{rel: rel, body: "plain text body"})
	}
	tree := make(map[string]string, len(ordered))
	for _, c := range ordered {
		tree[c.rel] = c.body
	}
	writeContentTree(t, dir, tree)
	d := uint32(os.ModeDir)
	recs := []CompactRecord{{FRN: 1, Parent: -1, Mode: d, Name: "."}}
	var frn uint64 = 2
	addDir := func(name string, parent int32, parentFRN uint64) int {
		recs = append(recs, CompactRecord{FRN: frn, Parent: parent, ParentFRN: parentFRN, Mode: d, Name: name})
		frn++
		return len(recs) - 1
	}
	bDir := addDir("b", 0, 1)
	tDir := addDir("t", 0, 1)
	for _, c := range ordered {
		parts := strings.Split(c.rel, "/")
		dirID, dirFRN := bDir, uint64(2)
		if parts[0] == "t" {
			dirID, dirFRN = tDir, 3
		}
		recs = append(recs, CompactRecord{FRN: frn, Parent: int32(dirID), ParentFRN: dirFRN, Name: parts[1], Size: int64(len(c.body))})
		frn++
	}
	idx := &Index{Source: "usn", Volume: dir, Compact: true, Roots: []string{dir}, Records: recs}
	scope := contentScope{Mode: contentScopeExplicit, Roots: normalizeContentPaths([]string{dir}), BudgetBytes: 1 << 30}
	est := scanContentScope(idx, scope, dir, 0, 0, false)
	if est.Files != 30 || est.TextCandidates != 30 {
		t.Fatalf("unsampled estimate files=%d candidates=%d; want 30/30", est.Files, est.TextCandidates)
	}
	if est.SampledFiles != 0 || est.ProjectedTextFiles != 0 {
		t.Fatalf("unsampled estimate sampled=%d projected=%d; want 0/0", est.SampledFiles, est.ProjectedTextFiles)
	}
	sampled := scanContentScope(idx, scope, dir, 0, 10, false)
	if sampled.SampledFiles != 10 {
		t.Fatalf("sampled files = %d; want 10", sampled.SampledFiles)
	}
	// 10 of 30 candidates are binary: any stride-10 sample must find some,
	// and the projection must land below the file count.
	if sampled.SampledBinary == 0 {
		t.Fatal("sample found no binary files among 10 binary of 30")
	}
	if sampled.ProjectedTextFiles <= 0 || sampled.ProjectedTextFiles >= sampled.Files {
		t.Fatalf("projected text files = %d; want between 1 and %d", sampled.ProjectedTextFiles, sampled.Files-1)
	}
}

func TestContentScopeReserve(t *testing.T) {
	oldFree, oldTotal, oldReserve := contentFreeBytesFn, contentTotalBytesFn, contentDiskReserveBytes
	defer func() {
		contentFreeBytesFn, contentTotalBytesFn, contentDiskReserveBytes = oldFree, oldTotal, oldReserve
	}()
	vol := `E:\`
	// A 300 GiB volume: reserve is 5% (15 GiB), inside [10, 20] GiB.
	contentTotalBytesFn = func(_ string) (uint64, error) { return 300 << 30, nil }
	contentFreeBytesFn = func(_ string) (uint64, error) { return 100 << 30, nil }
	if got := contentScopeReserveFor(vol); got != 15<<30 {
		t.Fatalf("reserve(300GiB) = %d; want 15 GiB", got)
	}
	// A 100 GiB volume: 5% (5 GiB) is raised to the 10 GiB floor.
	contentTotalBytesFn = func(_ string) (uint64, error) { return 100 << 30, nil }
	if got := contentScopeReserveFor(vol); got != contentScopeReserveMinBytes {
		t.Fatalf("reserve(100GiB) = %d; want the 10 GiB floor", got)
	}
	// A 2 TiB volume: 5% (~102 GiB) is capped at 20 GiB.
	contentTotalBytesFn = func(_ string) (uint64, error) { return 2 << 40, nil }
	if got := contentScopeReserveFor(vol); got != contentScopeReserveMaxBytes {
		t.Fatalf("reserve(2TiB) = %d; want the 20 GiB cap", got)
	}
	// Unknown capacity falls back to the fixed reserve (0 in the test
	// suite, which disables the reserve there).
	contentTotalBytesFn = func(_ string) (uint64, error) { return 0, errTestReserve }
	contentDiskReserveBytes = 7
	if got := contentScopeReserveFor(vol); got != 7 {
		t.Fatalf("reserve(unknown) = %d; want the fixed fallback", got)
	}
	contentDiskReserveBytes = 0
	// Allowance: (free - reserve) / multiplier, on a 2 TiB volume (20 GiB
	// reserve) with 100 GiB free.
	contentTotalBytesFn = func(_ string) (uint64, error) { return 2 << 40, nil }
	contentFreeBytesFn = func(_ string) (uint64, error) { return 100 << 30, nil }
	if got, want := contentScopeAllowance(vol), int64(float64(80<<30)/contentScopeEncodedMultiplier); got != want {
		t.Fatalf("allowance = %d; want %d", got, want)
	}
	// At or below the reserve nothing more is selected: less free space
	// never means more indexing.
	contentFreeBytesFn = func(_ string) (uint64, error) { return 1 << 30, nil }
	if got := contentScopeAllowance(vol); got != 0 {
		t.Fatalf("allowance below reserve = %d; want 0", got)
	}
	if contentScopeFitsReserve(vol, 1) {
		t.Fatal("below the reserve nothing fits")
	}
	// The fits check agrees with the allowance: 100 GiB free, 20 GiB
	// reserve: 120 GiB extracted projects to 60 GiB, leaving 40 GiB (fits);
	// 200 GiB projects to 100 GiB, leaving nothing (breaches).
	contentFreeBytesFn = func(_ string) (uint64, error) { return 100 << 30, nil }
	if !contentScopeFitsReserve(vol, 120<<30) {
		t.Fatal("120 GiB extracted must fit: projected 60 GiB leaves 40 GiB of a 20 GiB reserve")
	}
	if contentScopeFitsReserve(vol, 200<<30) {
		t.Fatal("200 GiB extracted must not fit: projected 100 GiB leaves nothing")
	}
	// Anchors: bare volumes anchor at the root, absolute paths at themselves,
	// anything else anchors nowhere.
	if a := contentScopeBudgetAnchor("E:"); a == "" {
		t.Fatal("bare volume must anchor")
	}
	if a := contentScopeBudgetAnchor(""); a != "" {
		t.Fatalf("empty volume anchored at %q", a)
	}
	if a := contentScopeBudgetAnchor("relative"); a != "" {
		t.Fatalf("relative volume anchored at %q", a)
	}
}

func TestContentScopeEstimateReserveTruncates(t *testing.T) {
	oldFree, oldTotal := contentFreeBytesFn, contentTotalBytesFn
	defer func() { contentFreeBytesFn, contentTotalBytesFn = oldFree, oldTotal }()
	// 20 GiB reserve (5% of 500 GiB, capped), 9 bytes of headroom: allowance
	// is 18 extracted bytes.
	contentTotalBytesFn = func(_ string) (uint64, error) { return 500 << 30, nil }
	contentFreeBytesFn = func(_ string) (uint64, error) { return (20 << 30) + 9, nil }
	d := uint32(os.ModeDir)
	recs := []CompactRecord{
		{FRN: 1, Parent: -1, Mode: d, Name: "."},
		{FRN: 2, Parent: 0, ParentFRN: 1, Mode: d, Name: "proj"},
		{FRN: 3, Parent: 1, ParentFRN: 2, Mode: d, Name: ".git"},
		{FRN: 4, Parent: 1, ParentFRN: 2, Name: "main.go", Size: 100},
		{FRN: 5, Parent: 1, ParentFRN: 2, Name: "OWNERS", Size: 30},
		{FRN: 6, Parent: 1, ParentFRN: 2, Name: "notes.xyz", Size: 40},
	}
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Roots: []string{`C:\`}, Records: recs}
	est := scanContentScope(idx, defaultContentScope(), "C:", 0, 0, false)
	if !est.BudgetHit || est.Files != 0 {
		t.Fatalf("reserve estimate files=%d hit=%v; want 0/true (100-byte first file over the 18-byte allowance)", est.Files, est.BudgetHit)
	}
	if est.EligibleFiles != 3 || est.EligibleBytes != 170 {
		t.Fatalf("eligible=%d/%d; want 3/170 (exact totals despite truncation)", est.EligibleFiles, est.EligibleBytes)
	}
	if est.BudgetBytes != 0 {
		t.Fatalf("estimate budget = %d; want 0 (reserve-governed, not a byte cap)", est.BudgetBytes)
	}
}

func TestContentScopeFingerprintBudgetProvenance(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SCOPE", "")
	t.Setenv("SEEKFS_CONTENT_ROOTS", "")
	t.Setenv("SEEKFS_CONTENT_EXCLUDE", "")
	t.Setenv("SEEKFS_CONTENT_BUDGET_BYTES", "")
	base := defaultContentScope()
	a := base.resolve("C:", nil)
	// The derived default is stable and budgets nothing into the identity.
	if a.fingerprint() != base.resolve("C:", nil).fingerprint() {
		t.Fatal("derived-budget fingerprint must be stable")
	}
	// The same numeric budget, made explicit, joins the identity...
	// (an explicit budget is always positive; 0 means unlimited).
	exp := base
	exp.BudgetBytes = 12345678
	exp.BudgetExplicit = true
	if exp.resolve("C:", nil).fingerprint() == a.fingerprint() {
		t.Fatal("an explicit budget must join the scope fingerprint")
	}
	// ...and distinct explicit budgets differ.
	exp2 := exp
	exp2.BudgetBytes++
	if exp.resolve("C:", nil).fingerprint() == exp2.resolve("C:", nil).fingerprint() {
		t.Fatal("distinct explicit budgets must fingerprint differently")
	}
	// contentScopeForVolume marks config and env budgets explicit.
	cfg := appConfig{Content: contentScopeConfig{BudgetBytes: 1234}}
	if s := contentScopeForVolume(cfg, "C:"); !s.BudgetExplicit || s.BudgetBytes != 1234 {
		t.Fatalf("config budget = %+v; want explicit 1234", s)
	}
	// An explicit budget past the old single-file cap is honored (sharding
	// keeps every shard file under it); past the ceiling it clamps to the
	// ceiling instead of silently dropping to a derived default.
	big := appConfig{Content: contentScopeConfig{BudgetBytes: 8 << 30}}
	if s := contentScopeForVolume(big, "C:"); !s.BudgetExplicit || s.BudgetBytes != 8<<30 {
		t.Fatalf("8 GiB budget = %+v; want explicit 8 GiB", s)
	}
	huge := appConfig{Content: contentScopeConfig{BudgetBytes: 64 << 30}}
	if s := contentScopeForVolume(huge, "C:"); !s.BudgetExplicit || s.BudgetBytes != contentScopeBudgetCeilBytes {
		t.Fatalf("64 GiB budget = %+v; want explicit clamped to the %d ceiling", s, contentScopeBudgetCeilBytes)
	}
	t.Setenv("SEEKFS_CONTENT_BUDGET_BYTES", "5678")
	if s := contentScopeForVolume(appConfig{}, "C:"); !s.BudgetExplicit || s.BudgetBytes != 5678 {
		t.Fatalf("env budget = %+v; want explicit 5678", s)
	}
}

func TestContentScopeEstimatePrefersRepository(t *testing.T) {
	dir := t.TempDir()
	vol, _ := contentBuildTestVolume(t, dir, 3, 10, []CompactRecord{
		{FRN: 1, Parent: -1, Name: "a.txt", Size: 10},
		{FRN: 2, Parent: -1, Name: "repo", Mode: uint32(os.ModeDir)},
		{FRN: 3, Parent: 1, ParentFRN: 2, Name: ".git", Mode: uint32(os.ModeDir)},
		{FRN: 4, Parent: 1, ParentFRN: 2, Name: "z.txt", Size: 10},
	})
	scope := contentScope{Mode: contentScopeExplicit, Roots: normalizeContentPaths([]string{dir}), IncludeGit: true, SystemExcludes: true, BudgetBytes: 100}
	est := scanContentScope(vol.index, scope, dir, 1, 0, false)
	if !est.MaxFilesHit || est.Files != 1 || len(est.ByUnit) != 1 || est.ByUnit[0].Kind != "repo" {
		t.Fatalf("limited estimate = %+v; want one repository file", est)
	}
}
