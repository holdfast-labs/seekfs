package main

import (
	"strings"
	"testing"
)

func TestContentQueryGateDisabled(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "")
	var pq parsedQuery
	err := applyQueryToken(&pq, "content:needle")
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("content: with the flag off returned %v; want a disabled error", err)
	}
}

func TestContentQueryGateEnabledParses(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	var pq parsedQuery
	if err := applyQueryToken(&pq, "content:needle"); err != nil {
		t.Fatalf("content: with the flag on returned %v; want nil", err)
	}
	if len(pq.Content) != 1 || pq.Content[0].Kind != contentLeafTerm || pq.Content[0].Text != "needle" {
		t.Fatalf("parsed content leaf = %+v", pq.Content)
	}
	// The same token with the flag off must still be refused at parse time.
	t.Setenv("SEEKFS_CONTENT_SEARCH", "")
	var off parsedQuery
	if err := applyQueryToken(&off, "content:needle"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("content: with the flag off returned %v; want a disabled error", err)
	}
}

func TestParseContentLeafForms(t *testing.T) {
	cases := []struct {
		in   string
		kind contentLeafKind
		text string
	}{
		{"needle", contentLeafTerm, "needle"},
		{`"to be or not"`, contentLeafPhrase, "to be or not"},
		{`/err.r|warn/`, contentLeafRegex, "err.r|warn"},
	}
	for _, tc := range cases {
		leaf, err := parseContentLeaf(tc.in)
		if err != nil {
			t.Fatalf("parseContentLeaf(%q): %v", tc.in, err)
		}
		if leaf.Kind != tc.kind || leaf.Text != tc.text {
			t.Fatalf("parseContentLeaf(%q) = %+v; want kind %d text %q", tc.in, leaf, tc.kind, tc.text)
		}
	}
	if _, err := parseContentLeaf(""); err == nil {
		t.Fatal("empty content: term must be rejected")
	}
}

func TestQueryHasPositiveContentLeaf(t *testing.T) {
	top := parsedQuery{Content: []contentLeaf{{Text: "a"}}}
	if !queryHasPositiveContentLeaf(top) {
		t.Fatal("top-level content must count as positive")
	}
	or := parsedQuery{OrGroups: [][]parsedQuery{{{Terms: []string{"x"}}, {Content: []contentLeaf{{Text: "a"}}}}}}
	if !queryHasPositiveContentLeaf(or) {
		t.Fatal("content in an OR alternative must count as positive")
	}
	not := parsedQuery{NotGroups: []parsedQuery{{Content: []contentLeaf{{Text: "a"}}}}}
	if queryHasPositiveContentLeaf(not) {
		t.Fatal("content under NOT is exclusion, not positive")
	}
}

func TestContentAssignLeafIDsDeterministic(t *testing.T) {
	build := func() *parsedQuery {
		return &parsedQuery{
			Content: []contentLeaf{{Text: "top"}},
			OrGroups: [][]parsedQuery{{
				{Content: []contentLeaf{{Text: "or1"}}, OrGroups: [][]parsedQuery{{{Content: []contentLeaf{{Text: "nested"}}}}}},
				{Content: []contentLeaf{{Text: "or2"}}},
			}},
			NotGroups: []parsedQuery{{Content: []contentLeaf{{Text: "not"}}}},
		}
	}
	a, b := build(), build()
	contentAssignLeafIDs(a)
	contentAssignLeafIDs(b)

	ids := func(pq *parsedQuery) []int {
		var out []int
		out = append(out, pq.Content[0].LeafID)
		out = append(out, pq.OrGroups[0][0].Content[0].LeafID)
		out = append(out, pq.OrGroups[0][0].OrGroups[0][0].Content[0].LeafID)
		out = append(out, pq.OrGroups[0][1].Content[0].LeafID)
		out = append(out, pq.NotGroups[0].Content[0].LeafID)
		return out
	}
	got := ids(a)
	want := []int{0, 1, 2, 3, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LeafIDs = %v; want %v (canonical DFS order)", got, want)
		}
	}
	if idsB := ids(b); !equalInts(got, idsB) {
		t.Fatalf("LeafIDs are not deterministic across parses: %v vs %v", got, idsB)
	}
}

func equalInts(a, b []int) bool {
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
