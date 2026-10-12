package main

import (
	"strings"
	"testing"
)

func TestContentTokenizePlainFields(t *testing.T) {
	got := contentTokenizeQuery("alpha beta   gamma")
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v; want %v", got, want)
		}
	}
}

func TestContentTokenizeQuotedPhrase(t *testing.T) {
	got := contentTokenizeQuery(`content:"to be or not to be"`)
	if len(got) != 1 {
		t.Fatalf("quoted phrase split into %v", got)
	}
	if inner := contentUnquoteToken(strings.TrimPrefix(got[0], "content:")); inner != "to be or not to be" {
		t.Fatalf("unquote = %q", inner)
	}
}

func TestContentTokenizeQuotedGlob(t *testing.T) {
	got := contentTokenizeQuery(`glob:"*.md"`)
	if len(got) != 1 || got[0] != `glob:"*.md"` {
		t.Fatalf("got %v", got)
	}
}

func TestContentTokenizeRegexSpanKeepsPipe(t *testing.T) {
	got := contentTokenizeQuery(`content:/error|warn/ file.txt`)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	pat, ok := contentTokenIsRegexSpan(got[0])
	if !ok || pat != "error|warn" {
		t.Fatalf("pattern = %q, %v", pat, ok)
	}
}

func TestContentTokenizeRegexSpanKeepsSpaces(t *testing.T) {
	got := contentTokenizeQuery(`content:/a b/ extra`)
	if len(got) != 2 || got[0] != "content:/a b/" || got[1] != "extra" {
		t.Fatalf("got %v", got)
	}
}

func TestContentTokenizeEscapedQuote(t *testing.T) {
	got := contentTokenizeQuery(`content:"say \"hi\""`)
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if inner := contentUnquoteToken(strings.TrimPrefix(got[0], "content:")); inner != `say "hi"` {
		t.Fatalf("unquote = %q", inner)
	}
}

func TestContentTokenizeUnterminatedQuoteIsOneToken(t *testing.T) {
	got := contentTokenizeQuery(`content:"unterminated`)
	if len(got) != 1 || got[0] != `content:"unterminated` {
		t.Fatalf("got %v", got)
	}
}

func TestContentTokenizeDoesNotSplitPipes(t *testing.T) {
	got := contentTokenizeQuery("content:a|content:b")
	if len(got) != 1 || got[0] != "content:a|content:b" {
		t.Fatalf("got %v; OR splitting must happen in the parser, not the tokenizer", got)
	}
}

func TestBuildContentResolverJoinsByFRN(t *testing.T) {
	docs := []contentDoc{
		{FRN: 10}, {FRN: 20}, {FRN: 30},
	}
	baseFRNs := []uint64{5, 20, 30, 40}
	baseIDs := []uint32{0, 1, 2, 3}
	r := buildContentResolver(docs, baseFRNs, baseIDs)

	if id, ok := r.recordID(0); ok {
		t.Fatalf("FRN 10 has no base record, got id %d", id)
	}
	if id, ok := r.recordID(1); !ok || id != 1 {
		t.Fatalf("FRN 20 -> id %d, %v; want 1, true", id, ok)
	}
	if id, ok := r.recordID(2); !ok || id != 2 {
		t.Fatalf("FRN 30 -> id %d, %v; want 2, true", id, ok)
	}
	if got := r.liveDocCount(); got != 2 {
		t.Fatalf("liveDocCount = %d; want 2", got)
	}
}

func TestContentResolverDocForFRN(t *testing.T) {
	docs := []contentDoc{{FRN: 10}, {FRN: 20}, {FRN: 30}}
	r := buildContentResolver(docs, []uint64{10, 30}, []uint32{7, 9})
	if i, ok := r.docForFRN(30); !ok || i != 2 {
		t.Fatalf("docForFRN(30) = %d, %v", i, ok)
	}
	if _, ok := r.docForFRN(25); ok {
		t.Fatal("docForFRN found a missing FRN")
	}
}

func TestContentResolverNilSafe(t *testing.T) {
	var r *contentResolver
	if _, ok := r.recordID(0); ok {
		t.Fatal("nil resolver must not report a record")
	}
	if r.liveDocCount() != 0 {
		t.Fatal("nil resolver must have zero live docs")
	}
}
