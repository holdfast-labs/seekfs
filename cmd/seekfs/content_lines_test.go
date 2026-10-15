package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func contentLinesFor(t *testing.T, query, text string, maxPerFile, before, after int) []contentOutputLine {
	t.Helper()
	selected := true
	servicePluginContentSelection.Store(&selected)
	t.Cleanup(func() { servicePluginContentSelection.Store(nil) })
	pq, err := parseQuery(queryOptions{Query: query, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	return contentMatchingLines([]byte(text), newContentLeafMatcher(pq), contentPositiveLeaves(pq), maxPerFile, before, after)
}

func lineNumbers(lines []contentOutputLine) (nums []int, matches []bool) {
	for _, l := range lines {
		nums = append(nums, l.Line)
		matches = append(matches, l.Match)
	}
	return nums, matches
}

func TestContentLinesMatchesContextAndLimit(t *testing.T) {
	text := "foo\nnear\nfar\nmid\nFOO two\nx\ny\nfoo three\n"
	// Case-insensitive, no context: every matching line, in order.
	got := contentLinesFor(t, "content:foo", text, 10, 0, 0)
	if nums, _ := lineNumbers(got); len(nums) != 3 || nums[0] != 1 || nums[1] != 5 || nums[2] != 8 {
		t.Fatalf("lines = %+v", got)
	}
	// Context 1: groups 1-2 and 4-6 and 7-8 merge where adjacent; matches flagged.
	got = contentLinesFor(t, "content:foo", text, 10, 1, 1)
	nums, matches := lineNumbers(got)
	want := []int{1, 2, 4, 5, 6, 7, 8}
	if len(nums) != len(want) {
		t.Fatalf("context lines = %+v; want %v", got, want)
	}
	for i := range want {
		if nums[i] != want[i] || matches[i] != (want[i] == 1 || want[i] == 5 || want[i] == 8) {
			t.Fatalf("context lines = %+v; want %v", got, want)
		}
	}
	// max-per-file stops matching; the last match keeps its after-context.
	got = contentLinesFor(t, "content:foo", text, 2, 0, 1)
	if nums, _ := lineNumbers(got); len(nums) != 4 || nums[3] != 6 {
		t.Fatalf("limited lines = %+v", got)
	}
}

func TestContentLinesCaseRegexPhraseCRLF(t *testing.T) {
	text := "Mutex lock\r\nmutex unlock\r\nfoo\r\nfoobar\r\nquick brown\r\nfox\r\n"
	// Smart case: uppercase literal matches only the exact case.
	if got := contentLinesFor(t, "content:Mutex", text, 10, 0, 0); len(got) != 1 || got[0].Line != 1 || got[0].Text != "Mutex lock" {
		t.Fatalf("smart case = %+v", got)
	}
	// Regex anchors apply per line; CR is stripped.
	if got := contentLinesFor(t, `content:/^foo$/`, text, 10, 0, 0); len(got) != 1 || got[0].Line != 3 {
		t.Fatalf("anchored regex = %+v", got)
	}
	// A phrase matches within one line only.
	if got := contentLinesFor(t, `content:"brown fox"`, text, 10, 0, 0); len(got) != 0 {
		t.Fatalf("cross-line phrase = %+v; want none", got)
	}
	// Negated leaves never print; positive OR-any per line.
	if got := contentLinesFor(t, "content:unlock !content:zzz", text, 10, 0, 0); len(got) != 1 || got[0].Line != 2 {
		t.Fatalf("negation = %+v", got)
	}
}

func TestContentLinesPrinterFormat(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(text, []byte("one\nfoo\ntwo\nthree\nfour\nfoo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "b.bin")
	if err := os.WriteFile(bin, []byte("foo\x00bar"), 0o644); err != nil {
		t.Fatal(err)
	}
	selected := true
	servicePluginContentSelection.Store(&selected)
	t.Cleanup(func() { servicePluginContentSelection.Store(nil) })
	p := newContentLinePrinter(queryOptions{Query: "content:foo"}, contentLineOutput{maxPerFile: 10, before: 1, after: 0})
	if p == nil {
		t.Fatal("no printer for a content query")
	}
	var buf bytes.Buffer
	p.print(&buf, text, "")
	p.print(&buf, bin, "foo bar")
	want := text + "-1-one\n" + text + ":2:foo\n--\n" + text + "-5-four\n" + text + ":6:foo\n--\n" + bin + ":0:foo bar\n"
	if buf.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", buf.String(), want)
	}
	if newContentLinePrinter(queryOptions{Query: "readme"}, contentLineOutput{}) != nil {
		t.Fatal("filename-only query must keep plain path output")
	}
}
