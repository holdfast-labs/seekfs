package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func contentRTFExtract(t *testing.T, raw []byte) contentExtractResult {
	t.Helper()
	got, err := contentRTFExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	return got
}

// A small RTF yields its visible text with the control words stripped.
func TestContentRTFExtractsText(t *testing.T) {
	got := contentRTFExtract(t, []byte(`{\rtf1\ansi\deff0 Hello \b world\b0!}`))
	if got.Class != contentClassRTF {
		t.Fatalf("class = %d; want %d", got.Class, contentClassRTF)
	}
	text := string(got.Text)
	for _, want := range []string{"Hello", "world"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "deff") || strings.Contains(text, "\\b") {
		t.Fatalf("control words leaked into text: %q", text)
	}
}

// Destination groups (and \* ignorable destinations) are excluded; only body
// text is indexed.
func TestContentRTFSkipsDestinations(t *testing.T) {
	raw := []byte(`{\rtf1\ansi
{\fonttbl{\f0\fnil Arial;}}
{\colortbl;\red0\green0\blue0;}
{\info{\title infotitle}}
{\*\generator generatorneedle}
{\pict\pngblip 0123456789abcdef pictneedle}
bodyneedle visible
}`)
	text := string(contentRTFExtract(t, raw).Text)
	if !strings.Contains(text, "bodyneedle") || !strings.Contains(text, "visible") {
		t.Fatalf("body text lost: %q", text)
	}
	for _, banned := range []string{"Arial", "infotitle", "generatorneedle", "pictneedle", "red0", "0123456789abcdef"} {
		if strings.Contains(text, banned) {
			t.Fatalf("excluded destination content %q leaked: %q", banned, text)
		}
	}
}

// \uN unicode escapes and \ucN fallback skip-counts; \'hh decodes with the
// \ansicpgN codepage (default 1252).
func TestContentRTFUnicodeAndCodepage(t *testing.T) {
	// uc=1 (default): the fallback '?' after \u233 is dropped.
	text := string(contentRTFExtract(t, []byte(`{\rtf1\ansi\uc1\u233?real}`)).Text)
	if !strings.Contains(text, "éreal") {
		t.Fatalf("\\u with uc=1: got %q; want éreal", text)
	}
	// uc=0: no fallback is skipped, so '?' is real text.
	text = string(contentRTFExtract(t, []byte(`{\rtf1\ansi\uc0\u233?real}`)).Text)
	if !strings.Contains(text, "é?real") {
		t.Fatalf("\\u with uc=0: got %q; want é?real", text)
	}
	// \'hh in the declared codepage.
	text = string(contentRTFExtract(t, []byte(`{\rtf1\ansi\ansicpg1252 caf\'e9}`)).Text)
	if !strings.Contains(text, "café") {
		t.Fatalf("\\'e9 with ansicpg1252: got %q; want café", text)
	}
}

func TestContentRTFGroupPropertiesAndFields(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{\rtf1\uc1 {\uc0\u233?}\u233?tail}`, "é?étail"},
		{`{\rtf1\ansicpg1252 {\ansicpg1251\'e9}\'e9}`, "йé"},
		{`{\rtf1{\fonttbl\uc0\ansicpg1251 ignored}\u233?\'e9}`, "éé"},
		{`{\rtf1 before {\field{\*\fldinst HYPERLINK "hidden"}{\fldrslt visible}} after}`, "before visible after"},
		{`{\rtf1{\field{\fldinst hidden}{\fldrslt visible}}}`, "visible"},
		{`{\rtf1{\u233}tail}`, "étail"},
		{`{\rtf1\u233{tail}}`, "étail"},
	} {
		if got := string(contentRTFExtract(t, []byte(tc.raw)).Text); got != tc.want {
			t.Errorf("Extract(%q) = %q; want %q", tc.raw, got, tc.want)
		}
	}
}

// A \uN inside a skipped destination must not arm a fallback count: the
// skipped region writes nothing, so the next characters of real text are not
// consumed as phantom fallback (regression: body text came out as "eal").
// A non-BMP \u escape arrives as a UTF-16 surrogate pair; the two halves must
// reassemble to the astral rune, not two U+FFFD (Word emits emoji this way).
func TestContentRTFUnicodeSurrogatePair(t *testing.T) {
	text := string(contentRTFExtract(t, []byte(`{\rtf1\ansi\uc1\u-10179?\u-8704? EMOJIEND}`)).Text)
	if !strings.Contains(text, "\U0001F600") {
		t.Fatalf("surrogate pair not reassembled: %q", text)
	}
	if !strings.Contains(text, "EMOJIEND") {
		t.Fatalf("text after surrogate pair lost: %q", text)
	}
}

func TestContentRTFUnicodeInSkippedDestination(t *testing.T) {
	for _, raw := range []string{
		`{\rtf1\ansi{\info \u233}realneedle}`,
		`{\rtf1\ansi{\fonttbl\u233}realneedle}`,
		`{\rtf1\ansi{\field\u233}realneedle}`,
	} {
		text := string(contentRTFExtract(t, []byte(raw)).Text)
		if !strings.Contains(text, "realneedle") {
			t.Fatalf("%s: got %q; want realneedle (fallback leaked out of skipped group)", raw, text)
		}
	}
}

// \par/\line produce newlines, \tab a tab, and escaped braces/backslash are
// emitted literally.
func TestContentRTFControlWordsAndEscapes(t *testing.T) {
	raw := []byte(`{\rtf1 a\{b\}c\\d\par e\line f\tab g}`)
	text := string(contentRTFExtract(t, raw).Text)
	if !strings.Contains(text, "a{b}c\\d") {
		t.Fatalf("escaped braces/backslash wrong: %q", text)
	}
	if !strings.Contains(text, "d\ne\nf\tg") {
		t.Fatalf("par/line/tab wrong: %q", text)
	}
}

// Malformed and unterminated RTF must not panic or hang.
func TestContentRTFMalformedDoesNotPanic(t *testing.T) {
	got := contentRTFExtract(t, []byte(`{\rtf1\ansi hello`))
	if !strings.Contains(string(got.Text), "hello") {
		t.Fatalf("unterminated RTF lost text: %q", got.Text)
	}
	got = contentRTFExtract(t, []byte(`{\rtf1 `+strings.Repeat("{", contentRTFMaxGroupDepth*4)+`text}`))
	if !got.Truncated {
		t.Fatalf("deep nesting must be bounded and marked Truncated: %+v", got)
	}
}

// Output is cut at maxText with Truncated set.
func TestContentRTFTruncatesAtMaxText(t *testing.T) {
	raw := []byte(`{\rtf1 alpha needle beta gamma delta}`)
	ctx := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: 1 << 20, maxText: 8})
	got, err := contentRTFExtractor{}.Extract(ctx, bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || got.Reason == "" {
		t.Fatalf("over-text-cap doc must be Truncated with a reason: %+v", got)
	}
	if len(got.Text) > 8 {
		t.Fatalf("text cap not respected: %d bytes", len(got.Text))
	}
}

// .rtf is in the service allowlist and is claimed by the RTF extractor.
func TestContentServiceAllowlistIncludesRTF(t *testing.T) {
	if _, ok := contentServiceExtensions()[".rtf"]; !ok {
		t.Fatal(".rtf missing from the service content allowlist")
	}
	if e := contentExtractorForPath("doc.rtf", []byte(`{\rtf1 hi}`)); e == nil || e.Name() != "rtf" {
		t.Fatalf(".rtf claimed by %v; want rtf", e)
	}
}

// A .rtf file is extracted (class RTF) and its terms are findable through a
// built content index.
func TestContentBuildIndexesRTF(t *testing.T) {
	dir := t.TempDir()
	raw := `{\rtf1\ansi\deff0{\fonttbl{\f0\fnil Arial;}}` +
		`{\*\generator generatorneedle} titleneedle \b bodyneedle\b0!}`
	if err := os.WriteFile(filepath.Join(dir, "note.rtf"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Docs) != 1 {
		t.Fatalf("built %d docs; want 1", len(idx.Docs))
	}
	if idx.Docs[0].ContentType != contentClassRTF || idx.Docs[0].ExtractorVersion != (contentRTFExtractor{}).Version() {
		t.Fatalf("doc identity = (%d,%d); want RTF v1", idx.Docs[0].ContentType, idx.Docs[0].ExtractorVersion)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"titleneedle", "bodyneedle"} {
		if hits := r.search(term, 0); len(hits) != 1 || hits[0].Path != "note.rtf" {
			t.Fatalf("term %q not findable: %v", term, contentPathsOf(hits))
		}
	}
	if hits := r.search("Arial", 0); len(hits) != 0 {
		t.Fatalf("fonttbl content indexed: %v", contentPathsOf(hits))
	}
}
