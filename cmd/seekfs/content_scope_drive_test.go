package main

// Tests for auto-scope whole-volume coverage (branch content/auto-drive-root):
// the volume root joins as the lowest-priority include, so text files outside
// repos and known folders are in scope (parity with the reference indexer),
// while dependency/cache trees stay out and the budget still fills repos,
// configured roots and known folders first.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentScopeDriveAutoAdmitsOutsideRepos(t *testing.T) {
	// Pin the home directory on C: so the known-folder roots are deterministic.
	t.Setenv("USERPROFILE", `C:\Users\drive-tester`)

	scope := contentScope{
		Mode:           contentScopeAuto,
		Roots:          normalizeContentPaths([]string{`C:\explicit`}),
		IncludeGit:     true,
		SystemExcludes: true,
	}
	r := scope.resolve("C:", []string{contentNormPath(`C:\repo`)})

	// A non-repo text file outside known folders is now in scope, admitted by
	// the drive root with no skip reason.
	unit, kind, reason, ok := r.allowsReason(`C:\else\readme.md`, -1)
	if !ok || reason != "" || kind != "root" || unit != contentNormPath(`C:\`) {
		t.Fatalf("allowsReason(C:\\else\\readme.md) = (%q,%q,%q,%v); want the c:\\ drive root, admitted", unit, kind, reason, ok)
	}

	// Priority order: repo < explicit root < known folder < drive root, with
	// an unmatched path (another volume) ranking below every include.
	cases := []struct {
		path string
		want int
	}{
		{`C:\repo\a.txt`, 0},
		{`C:\explicit\a.txt`, 1},
		{filepath.Join(`C:\Users\drive-tester`, "Documents", "a.txt"), 2},
		{`C:\else\a.txt`, 3},
		{`D:\else\a.txt`, 4},
	}
	for _, c := range cases {
		if got := r.priority(c.path); got != c.want {
			t.Errorf("priority(%q) = %d; want %d", c.path, got, c.want)
		}
	}
}

func TestContentScopeDriveExplicitStaysNarrow(t *testing.T) {
	scope := contentScope{
		Mode:           contentScopeExplicit,
		Roots:          normalizeContentPaths([]string{`C:\explicit`}),
		SystemExcludes: true,
	}
	r := scope.resolve("C:", nil)
	if r.DriveRoot != "" {
		t.Fatalf("explicit mode added a drive root %q; want none", r.DriveRoot)
	}
	_, _, reason, ok := r.allowsReason(`C:\else\readme.md`, -1)
	if ok || reason != "root" {
		t.Fatalf("explicit allowsReason(C:\\else\\readme.md) = (%q,%v); want reason root, rejected", reason, ok)
	}
}

func TestContentScopeDriveSkipsDependenciesOutsideRepos(t *testing.T) {
	dependency := []string{
		`C:\CodexVenvs\env\Lib\site-packages\pkg\mod.py`,
		`C:\app\node_modules\pkg\index.js`,
		`C:\app\__pycache__\mod.py`,
		`C:\app\.venv\Lib\mod.py`,
		`C:\app\venv\Scripts\run.py`,
		`C:\app\.tox\mod.py`,
		`C:\app\.mypy_cache\mod.py`,
	}
	for _, p := range dependency {
		norm := contentNormPath(p)
		base := strings.ToLower(filepath.Base(p))
		reason, skip := contentInclusionSkip(norm, base, contentPathExtension(norm), 100, false, true, nil)
		if !skip || reason != contentSkipDependency {
			t.Errorf("outside-repo skip(%q) = (%q,%v); want dependency", p, reason, skip)
		}
	}
	// Inside a repo gitignore keeps deciding: with no ignore files an
	// un-ignored (or tracked) node_modules file stays admitted, unchanged.
	scope := contentScope{Mode: contentScopeAuto, IncludeGit: true, SystemExcludes: true}
	r := scope.resolve("C:", []string{contentNormPath(`C:\repo`)})
	r.attachGitIgnore(nil)
	_, kind, reason, ok := r.allowsReason(`C:\repo\node_modules\index.js`, -1)
	if !ok || reason != "" || kind != "repo" {
		t.Fatalf("in-repo node_modules = (%q,%q,%v); want kind repo, admitted unchanged", kind, reason, ok)
	}
}

func TestContentScopeDriveBudgetDropsDriveRootFilesLast(t *testing.T) {
	d := uint32(os.ModeDir)
	recs := []CompactRecord{
		{FRN: 1, Parent: -1, Mode: d, Name: "."},
		{FRN: 2, Parent: 0, ParentFRN: 1, Mode: d, Name: "repo"},
		{FRN: 3, Parent: 1, ParentFRN: 2, Mode: d, Name: ".git"},
		{FRN: 4, Parent: 1, ParentFRN: 2, Name: "r1.txt", Size: 100},
		{FRN: 5, Parent: 1, ParentFRN: 2, Name: "r2.txt", Size: 100},
		{FRN: 6, Parent: 0, ParentFRN: 1, Mode: d, Name: "loose"},
		{FRN: 7, Parent: 5, ParentFRN: 6, Name: "l1.txt", Size: 100},
		{FRN: 8, Parent: 5, ParentFRN: 6, Name: "l2.txt", Size: 100},
	}
	idx := &Index{Source: "usn", Volume: "C:", Compact: true, Roots: []string{`C:\`}, Records: recs}
	scope := defaultContentScope()
	scope.BudgetBytes = 200
	scope.BudgetExplicit = true
	est := scanContentScope(idx, scope, "C:", 0, 0, true)
	if est.Files != 2 || !est.BudgetHit {
		t.Fatalf("budget estimate files=%d hit=%v; want 2 repo files then a budget hit", est.Files, est.BudgetHit)
	}
	for _, p := range est.Paths {
		if !contentPathUnder(p, contentNormPath(`C:\repo`)) {
			t.Fatalf("budget kept %q; want only repo files (drive-root files dropped)", p)
		}
	}
}
