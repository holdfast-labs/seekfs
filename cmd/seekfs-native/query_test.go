package main

import (
	"reflect"
	"testing"
)

func TestNormalizeUIQueryPathFilter(t *testing.T) {
	query, matchPath := normalizeUIQueryForService("path:Downloads ext:png", false)
	if !matchPath {
		t.Fatal("path: did not enable path matching")
	}
	if query != "Downloads ext:png" {
		t.Fatalf("query = %q, want %q", query, "Downloads ext:png")
	}
}

func TestNormalizeUIQueryEverythingAliases(t *testing.T) {
	cases := []struct {
		in        string
		wantQuery string
		wantPath  bool
	}{
		{"extension:.go", "ext:go", false},
		{"folder:", "type:dir", false},
		{"file:", "type:file", false},
		{"folder:regex:^src$", "type:dir regex:^src$", false},
		{"sz:>10mb", "size:>10mb", false},
		{"date-modified:today", "dm:today", false},
		{"location:Downloads", "dir:Downloads", true},
		{"name:main.go", "main.go", false},
	}
	for _, tc := range cases {
		gotQuery, gotPath := normalizeUIQueryForService(tc.in, false)
		if gotQuery != tc.wantQuery || gotPath != tc.wantPath {
			t.Fatalf("normalizeUIQueryForService(%q) = (%q, %v), want (%q, %v)", tc.in, gotQuery, gotPath, tc.wantQuery, tc.wantPath)
		}
	}
}

func TestNormalizeUIQueryKeepsDottedSubstringTerms(t *testing.T) {
	cases := []struct {
		in        string
		wantQuery string
		wantPath  bool
	}{
		{".opencode", ".opencode", false},
		{"path:C: .opencode", "C: .opencode", true},
		{"path:Downloads .nrrd", "Downloads .nrrd", true},
	}
	for _, tc := range cases {
		gotQuery, gotPath := normalizeUIQueryForService(tc.in, false)
		if gotQuery != tc.wantQuery || gotPath != tc.wantPath {
			t.Fatalf("normalizeUIQueryForService(%q) = (%q, %v), want (%q, %v)", tc.in, gotQuery, gotPath, tc.wantQuery, tc.wantPath)
		}
	}
}

func TestIncompleteUIQuery(t *testing.T) {
	for _, q := range []string{"path:Downloads .", "ext:", "path:Downloads ext:"} {
		if !incompleteUIQuery(q) {
			t.Fatalf("incompleteUIQuery(%q) = false, want true", q)
		}
	}
	for _, q := range []string{"path:Downloads .nrrd", "path:.nrrd", "Downloads ext:nrrd"} {
		if incompleteUIQuery(q) {
			t.Fatalf("incompleteUIQuery(%q) = true, want false", q)
		}
	}
}

func TestNormalizeLiveQuery(t *testing.T) {
	if got := normalizeLiveQuery("ext:"); got != "" {
		t.Fatalf("normalizeLiveQuery(ext:) = %q, want blank", got)
	}
	if got := normalizeLiveQuery("."); got != "" {
		t.Fatalf("normalizeLiveQuery(.) = %q, want blank", got)
	}
	if got := normalizeLiveQuery("main.go"); got != "main.go" {
		t.Fatalf("normalizeLiveQuery(main.go) = %q, want passthrough", got)
	}
}

func TestToContentQuery(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"needle haystack ext:go", "content:needle content:haystack ext:go"},
		{"needle", "content:needle"},
		{`content:"two words" ext:go`, `content:"two words" ext:go`},
		{"!skip needle", "!skip content:needle"},
		{"a OR b", "content:a OR content:b"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := toContentQuery(tc.in); got != tc.want {
			t.Fatalf("toContentQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildServiceQueryContentsMode(t *testing.T) {
	q, _ := buildServiceQuery("needle ext:go", true, false)
	if q != "content:needle ext:go" {
		t.Fatalf("buildServiceQuery contents = %q, want content:needle ext:go", q)
	}
	q, _ = buildServiceQuery("needle", false, false)
	if q != "needle" {
		t.Fatalf("buildServiceQuery plain = %q, want needle", q)
	}
}

func TestQueryLooksLoosePathScoped(t *testing.T) {
	if !queryLooksLoosePathScoped("path:foo bar") {
		t.Fatal("path: query must be path-scoped")
	}
	if !queryLooksLoosePathScoped("regex:foo.*") {
		t.Fatal("regex query must be path-scoped")
	}
	if !queryLooksLoosePathScoped(`foo\bar`) {
		t.Fatal("path-like token must be path-scoped")
	}
	if queryLooksLoosePathScoped("needle haystack") {
		t.Fatal("plain multi-term query must stay name matching")
	}
}

func TestQueryTokensQuotedPhrase(t *testing.T) {
	got := queryTokens(`content:"two words" ext:go`)
	want := []string{`content:"two words"`, "ext:go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("queryTokens = %q, want %q", got, want)
	}
}

func TestContentQueryHasLiteral(t *testing.T) {
	if !contentQueryHasLiteral("content:needle ext:go") {
		t.Fatal("literal content term not detected")
	}
	if contentQueryHasLiteral("content:/re.*/") {
		t.Fatal("regex content term must not count as literal")
	}
	if contentQueryHasLiteral("needle") {
		t.Fatal("plain term must not count as content literal")
	}
}
