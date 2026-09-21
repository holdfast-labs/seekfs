package main

import (
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/text/encoding/charmap"
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
		indexed := contentDecodeForIndex(raw, contentEncodingMode{auto: true}, false)
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

// PB6: auto's default policy keeps valid UTF-8 exact, and decodes non-UTF-8
// bytes as Windows-1252 (accented terms survive) instead of repairing to U+FFFD.
func TestContentEncodingAutoKeepsValidUTF8(t *testing.T) {
	s := "café naïve — plain"
	if got := contentDecode([]byte(s), contentAutoEncoding); got != s {
		t.Fatalf("valid UTF-8 changed: %q -> %q", s, got)
	}
}

func TestContentEncodingAutoFallsBackToCP1252(t *testing.T) {
	// 0xE9 -> é, 0x92 -> right single quote in Windows-1252.
	got := contentDecode([]byte{'c', 'a', 'f', 0xe9, ' ', 0x92}, contentAutoEncoding)
	if !strings.Contains(got, "café") || !strings.ContainsRune(got, '\u2019') {
		t.Fatalf("bytes not decoded as CP1252: %q", got)
	}
	if strings.ContainsRune(got, contentReplacementChar) {
		t.Fatalf("auto must not repair a CP1252 byte to U+FFFD: %q", got)
	}
}

// Windows-1252 leaves five byte values undefined; the fallback re-decodes the
// buffer as Latin-1 so no source byte is dropped. Go's charmap maps those five
// to U+FFFD, which is what triggers the Latin-1 fallback; assert both halves so
// a charmap change cannot silently make this pass through the CP1252 path.
func TestContentEncodingLegacyUndefinedByteFallsBackToLatin1(t *testing.T) {
	cp1252, err := charmap.Windows1252.NewDecoder().Bytes([]byte{0x81})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.ContainsRune(string(cp1252), contentReplacementChar) {
		t.Fatalf("precondition: CP1252 0x81 mapped to %q; want U+FFFD to trigger fallback", cp1252)
	}
	got := contentDecode([]byte{'a', 0x81, 'b'}, contentAutoEncoding)
	if strings.ContainsRune(got, contentReplacementChar) {
		t.Fatalf("undefined CP1252 byte left a replacement: %q", got)
	}
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") || !strings.ContainsRune(got, '\u0081') {
		t.Fatalf("Latin-1 fallback lost bytes: %q", got)
	}
}

// PF-4 regression: a mostly-UTF-8 file with one stray invalid byte must keep its
// valid UTF-8 spans byte-exact (so "café" stays findable) while still recovering
// the stray span as legacy text.
func TestContentEncodingLegacyHybridKeepsValidUTF8(t *testing.T) {
	raw := []byte("caf\xc3\xa9 \x92") // valid "café", then a stray CP1252 0x92
	got := contentDecode(raw, contentAutoEncoding)
	if !strings.Contains(got, "café") {
		t.Fatalf("valid UTF-8 span was not preserved: %q", got)
	}
	if strings.Contains(got, "cafÃ©") {
		t.Fatalf("valid UTF-8 was mojibaked: %q", got)
	}
	if !strings.ContainsRune(got, '\u2019') {
		t.Fatalf("stray legacy byte not recovered as CP1252: %q", got)
	}
	if strings.ContainsRune(got, contentReplacementChar) {
		t.Fatalf("hybrid decode should not repair the stray byte: %q", got)
	}
}

// PF-4 regression: an over-cap UTF-8 prefix cut mid-rune (the real truncation
// flag set by the extractor) must decode as UTF-8 with the incomplete tail
// repaired, not as CP1252, so accented terms stay findable.
func TestContentEncodingLegacyTruncatedTailDecodesAsUTF8(t *testing.T) {
	raw := []byte("caf\xc3\xa9 \xc3") // "café " then the lead byte of a cut é
	got := contentDecodeForIndex(raw, contentAutoEncoding, true)
	if !strings.HasPrefix(got, "café ") {
		t.Fatalf("valid UTF-8 prefix not preserved: %q", got)
	}
	if strings.Contains(got, "cafÃ©") {
		t.Fatalf("truncated UTF-8 was mojibaked as legacy: %q", got)
	}
	if !strings.HasSuffix(got, string(contentReplacementChar)) {
		t.Fatalf("incomplete tail was not repaired: %q", got)
	}
}

// PF-4 regression: without the real truncation signal, a genuine legacy buffer
// ending in a byte that merely looks like a cut UTF-8 sequence must take the
// hybrid path and stay decodable. 0xE9 alone (no trailing newline) and the
// 2-byte tail 0xE9 0x92 both have the shape the old byte heuristic mistook for
// a cut rune, so each is repaired to U+FFFD and café becomes unsearchable.
func TestContentEncodingLegacyLeadByteTailIsNotTruncation(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("caf\xe9"),     // 0xE9 would be a cut 3-byte lead
		[]byte("caf\xe9\x92"), // a 2-byte tail that also looks like a cut sequence
	} {
		got := contentDecodeForIndex(raw, contentAutoEncoding, false)
		if !strings.Contains(got, "café") {
			t.Fatalf("genuine legacy tail not decoded as CP1252: %q -> %q", raw, got)
		}
		if strings.ContainsRune(got, contentReplacementChar) {
			t.Fatalf("genuine legacy tail repaired to U+FFFD: %q -> %q", raw, got)
		}
	}
}

// The real truncation signal gates the repair: an over-cap buffer whose first
// invalid byte is an earlier stray (not the EOF tail) takes the hybrid path, so
// the stray is recovered as legacy and the UTF-8 prefix is untouched.
func TestContentEncodingTruncatedWithEarlierStrayTakesHybrid(t *testing.T) {
	raw := []byte("caf\xc3\xa9 \x92\xc3") // valid "café", stray 0x92, cut é lead
	got := contentDecodeForIndex(raw, contentAutoEncoding, true)
	if !strings.HasPrefix(got, "café ") {
		t.Fatalf("valid UTF-8 prefix not preserved: %q", got)
	}
	if !strings.ContainsRune(got, '\u2019') {
		t.Fatalf("earlier stray not recovered as CP1252: %q", got)
	}
}

// A genuinely legacy buffer (never valid UTF-8) still decodes and stays
// findable; the hybrid path must not break the pure CP1252 case.
func TestContentEncodingLegacyCP1252StillDecodes(t *testing.T) {
	got := contentDecode([]byte("caf\xe9 na\xefve"), contentAutoEncoding)
	if !strings.Contains(got, "café") || !strings.Contains(got, "naïve") {
		t.Fatalf("CP1252 bytes not decoded: %q", got)
	}
	if strings.ContainsRune(got, contentReplacementChar) {
		t.Fatalf("CP1252 decode left a replacement: %q", got)
	}
}

// The old single normalization is split (PF-6a): contentRepairText reuses the
// extractor's auto (legacy-aware) mode and preserves case, and contentFoldText
// is the lowercase view the case-insensitive index and verify use. For
// already-decoded text the repair is the identity, so the split is exactly the
// old normalize plus the raw bytes it dropped. This pins index and delta to the
// same pair.
func TestContentRepairAndFoldTextSplit(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("Caf\xe9 Needle"),
		[]byte("Valid Caf\xc3\xa9\n"),
		[]byte{0x81, 'X', 0x92},
	} {
		extracted := contentDecodeForIndex(raw, contentAutoEncoding, false)
		if got := contentRepairText([]byte(extracted)); string(got) != string(extracted) {
			t.Fatalf("repair(decode(%q)) = %q; want identity %q", raw, got, extracted)
		}
		want := []byte(strings.ToLower(extracted))
		if got := contentFoldText([]byte(extracted)); string(got) != string(want) {
			t.Fatalf("fold(decode(%q)) = %q; want %q", raw, got, want)
		}
	}
}

func TestContentEncodingLegacyIndexAndSearchAgree(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("caf\xe9 needle"),
		[]byte{0x81, 0x92, 'x'},
		[]byte("plain ascii\n"),
		[]byte("valid café utf8\n"),
	} {
		if a, b := contentDecodeForIndex(raw, contentAutoEncoding, false), contentDecode(raw, contentAutoEncoding); a != b {
			t.Fatalf("index/search disagree on %q: %q vs %q", raw, a, b)
		}
	}
}

// The explicit -encoding override beats the auto fallback (a BOM would still
// win, per the existing BOM tests). WHATWG maps "latin1" to windows-1252, so a
// genuinely distinct label is used here.
func TestContentEncodingExplicitOverrideWins(t *testing.T) {
	raw := []byte{0xa1} // CP1252 '¡' (U+00A1) vs ISO-8859-2 'Ą' (U+0104)
	iso2, err := parseContentEncoding("iso-8859-2")
	if err != nil {
		t.Fatal(err)
	}
	auto := contentDecode(raw, contentAutoEncoding)
	explicit := contentDecode(raw, iso2)
	if auto == explicit {
		t.Fatalf("explicit iso-8859-2 must differ from auto/cp1252: %q", auto)
	}
	if explicit != "Ą" {
		t.Fatalf("explicit iso-8859-2 decoded 0xa1 to %q; want Ą", explicit)
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
