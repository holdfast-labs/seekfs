package main

import "testing"

func TestContentGitMarkerName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{".git", true},
		{".GIT", true},
		{".Git", true},
		{".gitignore", false},
		{".gitattributes", false},
		{"git", false},
		{"x.git", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := contentGitMarkerName(tc.name); got != tc.want {
			t.Errorf("contentGitMarkerName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestContentGitRepoRoot(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`C:\a\b\.git`, `c:\a\b`},
		{`C:\a\b\.git\`, `c:\a\b`},
		{`C:/a/b/.git`, `c:\a\b`},
		{".git", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := contentGitRepoRoot(tc.in); got != tc.want {
			t.Errorf("contentGitRepoRoot(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestContentGitMarkerDirVsFile(t *testing.T) {
	dir := contentNewGitMarker(`C:\a\b\.git`, true)
	if dir.Path != `c:\a\b\.git` || !dir.Dir || dir.Root != `c:\a\b` {
		t.Fatalf("dir marker = %+v", dir)
	}
	if got := contentGitExcludedPaths(dir); len(got) != 1 || got[0] != dir.Path {
		t.Errorf("dir excludes = %v, want [%s]", got, dir.Path)
	}

	file := contentNewGitMarker(`C:\a\b\.git`, false)
	if file.Path != `c:\a\b\.git` || file.Dir || file.Root != `c:\a\b` {
		t.Fatalf("file marker = %+v", file)
	}
	if got := contentGitExcludedPaths(file); len(got) != 1 || got[0] != file.Path {
		t.Errorf("file excludes = %v, want [%s]", got, file.Path)
	}

	if got := contentGitExcludedPaths(contentGitMarker{}); got != nil {
		t.Errorf("zero marker excludes = %v, want nil", got)
	}
}

func TestContentGitMarkerFieldsNormalized(t *testing.T) {
	m := contentNewGitMarker(`C:\A\B\.GIT\`, true)
	if m.Path != `c:\a\b\.git` {
		t.Errorf("Path = %q, want %q", m.Path, `c:\a\b\.git`)
	}
	if m.Root != `c:\a\b` {
		t.Errorf("Root = %q, want %q", m.Root, `c:\a\b`)
	}
}

func TestContentGitParseDirPointer(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"simple", "gitdir: C:/x/.git/worktrees/wt\r\n", "C:/x/.git/worktrees/wt", true},
		{"extra lines", "header line\n gitdir:  C:/x/.git/worktrees/wt  \nmore\n", "C:/x/.git/worktrees/wt", true},
		{"no pointer", "just a plain file\n", "", false},
		{"empty target", "gitdir:\n", "", false},
	}
	for _, tc := range cases {
		got, ok := contentParseGitDirPointer([]byte(tc.in))
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("%s: contentParseGitDirPointer(%q) = (%q,%v), want (%q,%v)",
				tc.name, tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestContentGitNestedMarkersDistinctRoots(t *testing.T) {
	outer := contentNewGitMarker(`C:\a\.git`, true)
	inner := contentNewGitMarker(`C:\a\sub\.git`, true)
	if outer.Root != `c:\a` {
		t.Errorf("outer root = %q, want %q", outer.Root, `c:\a`)
	}
	if inner.Root != `c:\a\sub` {
		t.Errorf("inner root = %q, want %q", inner.Root, `c:\a\sub`)
	}
	if outer.Root == inner.Root {
		t.Errorf("nested markers collapsed to same root %q", outer.Root)
	}
}
