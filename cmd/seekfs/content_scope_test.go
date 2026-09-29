package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
	if want := int64(float64(contentGSXMaxBytes) * contentScopeBudgetFraction); s.BudgetBytes != want {
		t.Fatalf("default budget = %d; want %d", s.BudgetBytes, want)
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
	if s.BudgetBytes != defaultContentScope().BudgetBytes {
		t.Fatalf("invalid budget = %d; want default", s.BudgetBytes)
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
		{`C:\proj\main.exe`, false, ""},
		{`C:\other\readme.md`, false, ""},
		{`C:\Windows\a.txt`, false, ""},
	}
	for _, c := range cases {
		unit, kind, ok := r.allows(c.path)
		if ok != c.ok || (ok && kind != c.kind) {
			t.Errorf("allows(%q) = (%q,%q,%v); want ok=%v kind=%q", c.path, unit, kind, ok, c.ok, c.kind)
		}
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
	est := scanContentScope(idx, defaultContentScope(), "C:", 0)
	if len(est.Repos) != 1 || est.Repos[0] != contentNormPath(`C:\proj`) {
		t.Fatalf("repos = %v; want [c:\\proj]", est.Repos)
	}
	if est.Files != 2 || est.Bytes != 140 {
		t.Fatalf("estimate files=%d bytes=%d; want 2/140 (only proj/*.go)", est.Files, est.Bytes)
	}
	if len(est.ByUnit) != 1 || est.ByUnit[0].Kind != "repo" || est.ByUnit[0].Files != 2 {
		t.Fatalf("by-unit = %+v", est.ByUnit)
	}
	if est.Excluded["exclude"] == 0 || est.Excluded["ext"] != 0 {
		t.Fatalf("excluded = %+v; want exclude>0, ext=0", est.Excluded)
	}
	if est.ProjectedGSXBytes <= est.Bytes {
		t.Fatalf("projected %d should exceed bytes %d (overhead)", est.ProjectedGSXBytes, est.Bytes)
	}

	// MaxFiles truncation is surfaced: two includable files, cap 1.
	if capped := scanContentScope(idx, defaultContentScope(), "C:", 1); !capped.MaxFilesHit || capped.Files != 1 {
		t.Fatalf("maxFiles=1 => files=%d hit=%v; want 1/true", capped.Files, capped.MaxFilesHit)
	}
	budgeted := defaultContentScope()
	budgeted.BudgetBytes = 110
	if capped := scanContentScope(idx, budgeted, "C:", 0); !capped.BudgetHit || capped.Files != 1 || capped.Bytes != 100 {
		t.Fatalf("budget=110 => files=%d bytes=%d hit=%v; want 1/100/true", capped.Files, capped.Bytes, capped.BudgetHit)
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
	est := scanContentScope(vol.index, scope, dir, 1)
	if !est.MaxFilesHit || est.Files != 1 || len(est.ByUnit) != 1 || est.ByUnit[0].Kind != "repo" {
		t.Fatalf("limited estimate = %+v; want one repository file", est)
	}
}
