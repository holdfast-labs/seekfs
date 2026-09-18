package main

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func contentUTF16LEWithBOM(s string) []byte {
	v := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		v = append(v, byte(u), byte(u>>8))
	}
	return v
}

func contentUTF16BEWithBOM(s string) []byte {
	v := []byte{0xFE, 0xFF}
	for _, u := range utf16.Encode([]rune(s)) {
		v = append(v, byte(u>>8), byte(u))
	}
	return v
}

func TestContentEncodingParsesSpecialLabels(t *testing.T) {
	for _, label := range []string{"auto", ""} {
		m, err := parseContentEncoding(label)
		if err != nil || !m.auto {
			t.Fatalf("parseContentEncoding(%q) = %+v, %v; want auto", label, m, err)
		}
	}
	m, err := parseContentEncoding("none")
	if err != nil || !m.none {
		t.Fatalf("parseContentEncoding(none) = %+v, %v; want none", m, err)
	}
}

func TestContentEncodingParsesWHATWGLabels(t *testing.T) {
	for _, label := range []string{"utf-16le", "sjis", "latin1"} {
		if _, err := parseContentEncoding(label); err != nil {
			t.Fatalf("parseContentEncoding(%q) failed: %v", label, err)
		}
	}
	if _, err := parseContentEncoding("definitely-not-an-encoding"); err == nil {
		t.Fatal("expected an error for an unknown label")
	}
}

func TestContentEncodingAutoDecodesUTF16BOM(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes []byte
	}{
		{"le", contentUTF16LEWithBOM("hello needle\n")},
		{"be", contentUTF16BEWithBOM("hello needle\n")},
	} {
		if got := contentDecode(tc.bytes, contentEncodingMode{auto: true}); got != "hello needle\n" {
			t.Fatalf("%s: got %q", tc.name, got)
		}
	}
}

func TestContentEncodingAutoStripsUTF8BOM(t *testing.T) {
	b := append([]byte{0xEF, 0xBB, 0xBF}, []byte("hello needle\n")...)
	if got := contentDecode(b, contentEncodingMode{auto: true}); got != "hello needle\n" {
		t.Fatalf("got %q", got)
	}
}

func TestContentEncodingNoneKeepsRawBytes(t *testing.T) {
	b := contentUTF16LEWithBOM("hi")
	got := contentDecode(b, contentEncodingMode{none: true})
	if got == "hi" {
		t.Fatalf("none mode must not decode UTF-16, got %q", got)
	}
}

func TestContentEncodingPlainUTF8Unchanged(t *testing.T) {
	for _, m := range []contentEncodingMode{{auto: true}, {none: true}} {
		if got := contentDecode([]byte("plain text\n"), m); got != "plain text\n" {
			t.Fatalf("mode %+v: got %q", m, got)
		}
	}
}

func TestContentEncodingLatin1HighBytes(t *testing.T) {
	m, err := parseContentEncoding("latin1")
	if err != nil {
		t.Fatal(err)
	}
	if got := contentDecode([]byte("caf\xe9"), m); got != "café" {
		t.Fatalf("got %q", got)
	}
}

func TestContentEncodingInvalidUTF8IsLossy(t *testing.T) {
	out := contentDecode([]byte("caf\xe9 x"), contentEncodingMode{auto: true})
	if !strings.Contains(out, "caf") || !strings.Contains(out, " x") {
		t.Fatalf("got %q", out)
	}
	if !strings.Contains(out, string(contentReplacementChar)) {
		t.Fatalf("expected a replacement char, got %q", out)
	}
}

func TestContentEncodingIndexAndSearchAgree(t *testing.T) {
	cases := [][]byte{
		[]byte("\xff"),
		[]byte("\xffleading\n"),
		[]byte("trailing\xff"),
		[]byte("two \xff\xfe apart \xff\n"),
		[]byte("adjacent \xff\xff\xff runs\n"),
		[]byte("caf\xe9 x"),
		[]byte("plain ascii\n"),
	}
	for _, raw := range cases {
		indexed := contentDecodeForIndex(raw, contentEncodingMode{auto: true})
		searched := contentDecode(raw, contentEncodingMode{auto: true})
		if indexed != searched {
			t.Fatalf("index and search disagree on %q: %q vs %q", raw, indexed, searched)
		}
	}
}

func TestContentLossyFixupsMapOffsets(t *testing.T) {
	// "caf\xe9 x" -> "caf\uFFFD x": the 0xe9 becomes a 3-byte replacement, so
	// decoded offset 6 (" ") maps back to source offset 4.
	raw := []byte("caf\xe9 x")
	text, fixups := contentDecodeWithFixups(raw, contentEncodingMode{auto: true})
	if text != "caf"+string(contentReplacementChar)+" x" {
		t.Fatalf("text = %q", text)
	}
	if fixups.isEmpty() {
		t.Fatal("expected fixups for invalid input")
	}
	for _, tc := range []struct{ decoded, want int }{
		{0, 0}, {3, 3}, {4, 3}, {5, 3}, // inside the replacement clamps to its start
		{6, 4}, {7, 5},
	} {
		if got := fixups.toSourceOffset(tc.decoded); got != tc.want {
			t.Errorf("toSourceOffset(%d) = %d; want %d", tc.decoded, got, tc.want)
		}
	}
}

func TestContentLossyFixupsNoRepairsIsIdentity(t *testing.T) {
	_, fixups := contentDecodeWithFixups([]byte("plain ascii\n"), contentEncodingMode{auto: true})
	if !fixups.isEmpty() {
		t.Fatal("valid input must produce no fixups")
	}
	if got := fixups.toSourceOffset(7); got != 7 {
		t.Fatalf("toSourceOffset(7) = %d; want 7", got)
	}
}

func TestContentEncodingMaximalSubsequence(t *testing.T) {
	// A truncated 3-byte sequence at EOF is one replacement, not two.
	truncated := append([]byte("x"), 0xE2, 0x82)
	text, _ := contentDecodeWithFixups(truncated, contentEncodingMode{auto: true})
	if n := strings.Count(text, string(contentReplacementChar)); n != 1 {
		t.Fatalf("truncated sequence produced %d replacements: %q", n, text)
	}
	// A valid 2-byte prefix followed by a bad continuation is one replacement
	// for the two prefix bytes, then the literal byte.
	bad := append([]byte("x"), 0xE2, 0x82, 'A')
	text, _ = contentDecodeWithFixups(bad, contentEncodingMode{auto: true})
	if n := strings.Count(text, string(contentReplacementChar)); n != 1 {
		t.Fatalf("bad continuation produced %d replacements: %q", n, text)
	}
	if !strings.HasSuffix(text, "A") {
		t.Fatalf("text should end with the literal A: %q", text)
	}
}

func TestContentBorrowsWholeInputRejectsInvalidUTF8(t *testing.T) {
	if contentBorrowsWholeInput([]byte("caf\xe9"), contentEncodingMode{auto: true}) {
		t.Fatal("invalid UTF-8 is repaired, so the bytes cannot be borrowed whole")
	}
}

func TestContentBorrowsWholeInput(t *testing.T) {
	if !contentBorrowsWholeInput([]byte("plain"), contentEncodingMode{auto: true}) {
		t.Fatal("auto + no BOM should borrow the whole input")
	}
	if contentBorrowsWholeInput(contentUTF16LEWithBOM("x"), contentEncodingMode{auto: true}) {
		t.Fatal("a BOM means the input is not handed back untouched")
	}
}
