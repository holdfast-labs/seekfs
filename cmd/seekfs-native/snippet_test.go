package main

import (
	"reflect"
	"testing"
)

func TestContentHighlightTerms(t *testing.T) {
	got := contentHighlightTerms("content:needle content:/re.*/ ext:go")
	if !reflect.DeepEqual(got, []string{"needle"}) {
		t.Fatalf("contentHighlightTerms = %q, want [needle]", got)
	}
	got = contentHighlightTerms(`content:"two words" !content:skip`)
	if !reflect.DeepEqual(got, []string{"two words", "skip"}) {
		t.Fatalf("contentHighlightTerms = %q, want phrase + negated", got)
	}
	if got := contentHighlightTerms("needle haystack"); len(got) != 0 {
		t.Fatalf("plain terms must yield no highlight terms, got %q", got)
	}
}

func TestHighlightSegments(t *testing.T) {
	segs := highlightSegments("func needle() NEEDLE", []string{"needle"})
	var texts []string
	matches := 0
	for _, s := range segs {
		texts = append(texts, s.Text)
		if s.Match {
			matches++
		}
	}
	if matches != 2 {
		t.Fatalf("matches = %d, want 2 (case-insensitive); segs=%+v", matches, segs)
	}
	joined := ""
	for _, s := range segs {
		joined += s.Text
	}
	if joined != "func needle() NEEDLE" {
		t.Fatalf("segments do not reassemble: %q", joined)
	}
}

func TestHighlightSegmentsLongestFirst(t *testing.T) {
	segs := highlightSegments("haystack needle", []string{"needle", "needle hay", "haystack"})
	matchCount := 0
	for _, s := range segs {
		if s.Match {
			matchCount++
			if s.Text != "haystack" && s.Text != "needle" {
				t.Fatalf("unexpected match %q", s.Text)
			}
		}
	}
	// "haystack" matches whole, then "needle"; "needle hay" must not win.
	if matchCount != 2 {
		t.Fatalf("matches = %d, want 2; segs=%+v", matchCount, segs)
	}
}

func TestHighlightSegmentsEmpty(t *testing.T) {
	if segs := highlightSegments("", []string{"x"}); len(segs) != 0 {
		t.Fatalf("empty text must yield no segments, got %+v", segs)
	}
	if segs := highlightSegments("abc", nil); len(segs) != 1 || segs[0].Match {
		t.Fatalf("no terms must yield one plain segment, got %+v", segs)
	}
}

func TestMatchSegmentBounds(t *testing.T) {
	bounds := matchSegmentBounds("xx NEEDLE yy needle", []string{"needle"})
	if !reflect.DeepEqual(bounds, [][2]int{{3, 9}, {13, 19}}) {
		t.Fatalf("bounds = %v, want [[3 9] [13 19]]", bounds)
	}
	if bounds := matchSegmentBounds("nothing here", []string{"needle"}); len(bounds) != 0 {
		t.Fatalf("no match must yield no bounds, got %v", bounds)
	}
}
