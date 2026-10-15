package main

import "testing"

func TestContentLinesMultipleMatchesContextAndLimit(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	pq, err := parseQuery(queryOptions{Query: `content:foo`, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	m := newContentLeafMatcher(pq)
	got := contentMatchingLines([]byte("foo\nnear\nfoo\nfar\nfoo\n"), m, 2, 1, 1)
	if len(got) != 4 || got[0].Line != 1 || got[1].Line != 2 || got[2].Line != 3 || got[3].Line != 4 {
		t.Fatalf("lines = %#v", got)
	}
	if !got[1].Context || got[1].Text != "near" {
		t.Fatalf("context = %#v", got[1])
	}
}

func TestContentLinesRegexAnchorsAndCRLF(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	pq, err := parseQuery(queryOptions{Query: `content:/^foo$/`, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	got := contentMatchingLines([]byte("foo\r\nfoobar\r\nfoo\r\n"), newContentLeafMatcher(pq), 10, 0, 0)
	if len(got) != 2 || got[1].Line != 3 {
		t.Fatalf("lines = %#v", got)
	}
}
