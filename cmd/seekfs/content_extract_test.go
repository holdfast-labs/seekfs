package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentPathExtension(t *testing.T) {
	cases := map[string]string{
		`C:\a\b\Readme.MD`:      ".md",
		`C:\a\b\main.go`:        ".go",
		`C:\a\b\noext`:          "",
		`C:\a\b\.gitignore`:     "",
		`C:\a\b\archive.tar.gz`: ".gz",
		`/unix/path/Doc.DOCX`:   ".docx",
	}
	for in, want := range cases {
		if got := contentPathExtension(in); got != want {
			t.Errorf("contentPathExtension(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestContentLooksBinary(t *testing.T) {
	if !contentLooksBinary([]byte("abc\x00def")) {
		t.Fatal("NUL byte must be detected as binary")
	}
	if contentLooksBinary([]byte("plain text without NUL")) {
		t.Fatal("plain text must not be binary")
	}
}

func TestContentExtractorForPathFallsBackToText(t *testing.T) {
	e := contentExtractorForPath(`C:\x\notes.txt`, []byte("hello"))
	if e == nil || e.Name() != "text" {
		t.Fatalf("got %v; want text extractor", e)
	}
	// A binary file with no claiming extension must not be claimed.
	if e := contentExtractorForPath(`C:\x\blob.bin`, []byte("a\x00b")); e != nil {
		t.Fatalf("binary blob claimed by %v", e)
	}
}

func TestContentExtractorSelectionPrefersFormatSniffers(t *testing.T) {
	// An extensionless zip must be claimed by the OOXML extractor, not the text
	// fallback (whose Sniff accepts any non-binary head).
	if e := contentExtractorForPath(`C:\x\weirdname`, []byte("PK\x03\x04rest")); e == nil || e.Name() != "ooxml" {
		t.Fatalf("extensionless zip claimed by %v; want ooxml", e)
	}
	if e := contentExtractorForPath(`C:\x\weirdname`, []byte("%PDF-1.7 rest")); e == nil || e.Name() != "pdf" {
		t.Fatalf("extensionless pdf claimed by %v; want pdf", e)
	}
	if e := contentExtractorForPath(`C:\x\weirdname`, []byte("just some text")); e == nil || e.Name() != "text" {
		t.Fatalf("plain text claimed by %v; want text", e)
	}
}

func TestContentTextExtractorDecodes(t *testing.T) {
	raw := []byte("alpha needle beta\n")
	got, err := contentTextExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Text) != "alpha needle beta\n" || got.Class != contentClassText {
		t.Fatalf("got %+v", got)
	}
}

func TestContentTextExtractorSkipsBinary(t *testing.T) {
	raw := []byte("abc\x00def")
	got, err := contentTextExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Skipped || got.Reason != "binary" {
		t.Fatalf("got %+v", got)
	}
}

func TestContentTextExtractorDecodesUTF16(t *testing.T) {
	raw := contentUTF16LEWithBOM("needle\n")
	got, err := contentTextExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Text) != "needle\n" {
		t.Fatalf("got %q", got.Text)
	}
}

// P6-5: a BOM-less UTF-16 file is decoded by the text extractor instead of
// being skipped as binary.
func TestContentTextExtractorDecodesBOMlessUTF16(t *testing.T) {
	raw := contentUTF16WithoutBOM("needle needle\n", "utf-16le")
	got, err := contentTextExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped || string(got.Text) != "needle needle\n" {
		t.Fatalf("got %+v; want decoded text", got)
	}
}

// PB7: an over-cap text file is indexed as a bounded prefix (never a silent
// drop), marked Truncated, and the policy cap is respected so memory stays
// bounded by maxRaw/maxText.
func TestContentTextExtractorIndexesBoundedPrefix(t *testing.T) {
	raw := []byte("needle " + strings.Repeat("x", 200))
	ctx := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: 8, maxText: 4})
	got, err := contentTextExtractor{}.Extract(ctx, bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped {
		t.Fatalf("an over-cap text file must be prefix-indexed, not skipped: %+v", got)
	}
	if !got.Truncated || got.Reason == "" {
		t.Fatalf("prefix-indexed doc must be marked Truncated with a reason: %+v", got)
	}
	if len(got.Text) > 4 {
		t.Fatalf("text cap not respected: %d bytes", len(got.Text))
	}
}

// A container format cannot be prefix-indexed; over-cap it must Skip with a
// visible reason (counted in policy), never silently vanish.
func TestContentContainerExtractorSkipsOversizeWithReason(t *testing.T) {
	ctx := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: 4})
	got, err := contentOOXMLExtractor{}.Extract(ctx, bytes.NewReader(nil), 100)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Skipped || got.Reason != "raw size over cap" {
		t.Fatalf("got %+v; want Skipped/raw size over cap", got)
	}
	pdf, err := contentPDFExtractor{}.Extract(ctx, bytes.NewReader(nil), 100)
	if err != nil {
		t.Fatal(err)
	}
	if !pdf.Skipped || pdf.Reason != "raw size over cap" {
		t.Fatalf("got %+v; want Skipped/raw size over cap", pdf)
	}
}

// PB6 explicit override: a build with -encoding iso-8859-2 decodes 0xA1 as Ą,
// which the default CP1252 policy would have decoded as ¡.
func TestContentBuildEncodingOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte{'x', ' ', 0xa1}, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := defaultContentBuildOptions()
	opts.Encoding = "iso-8859-2"
	idx, err := buildContentIndexFromDir(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("Ą", 0); len(hits) != 1 {
		t.Fatalf("explicit encoding term not findable: %v", contentPathsOf(hits))
	}
}

// PB6 end-to-end offline: a Windows-1252/Latin-1 file with an accented term is
// decoded and findable, and the built index records the policy.
func TestContentBuildDecodesLegacyEncoding(t *testing.T) {
	dir := t.TempDir()
	// 0xE9 is "é" in Windows-1252/Latin-1 and invalid UTF-8.
	if err := os.WriteFile(filepath.Join(dir, "latin.txt"), []byte("caf\xe9 needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	hits := r.search("café", 0)
	if len(hits) != 1 || hits[0].Path != "latin.txt" {
		t.Fatalf("accented term not findable: %v", contentPathsOf(hits))
	}
	if idx.Policy.MaxRaw != contentExtractMaxRawBytes || idx.Policy.MaxText != contentExtractMaxTextBytes {
		t.Fatalf("default policy not recorded: %+v", idx.Policy)
	}
}

// PF-4 regression end-to-end: a genuine Latin-1 file that ends in a lead-like
// byte with no trailing newline is not a truncated UTF-8 prefix, so the
// extractor must not repair it to U+FFFD; café stays findable.
func TestContentBuildLegacyLeadByteAtEOFIsSearchable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "latin.txt"), []byte("caf\xe9"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("café", 0); len(hits) != 1 || hits[0].Path != "latin.txt" {
		t.Fatalf("legacy lead-like tail lost: %v", contentPathsOf(hits))
	}
}

// PF-4 regression end-to-end: a UTF-8 file over the raw cap is cut mid-rune;
// the bounded prefix must decode as UTF-8 (tail repaired), not as CP1252, so an
// accented term before the cut stays findable.
func TestContentBuildTruncatedUTF8PrefixStaysUTF8(t *testing.T) {
	dir := t.TempDir()
	full := []byte("café needle " + strings.Repeat("é", 64) + "é")
	if err := os.WriteFile(filepath.Join(dir, "utf8.txt"), full, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := defaultContentBuildOptions()
	// One byte short: the prefix ends on the lead byte of the final "é".
	opts.MaxRaw = int64(len(full) - 1)
	opts.MaxText = 1 << 20
	idx, err := buildContentIndexFromDir(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("café", 0); len(hits) != 1 || hits[0].Path != "utf8.txt" {
		t.Fatalf("accented term lost on truncated UTF-8: %v", contentPathsOf(hits))
	}
}

func TestTruncateUTF8DoesNotSplitRunes(t *testing.T) {
	s := strings.Repeat("a", 5) + "é" + "b" // é is 2 bytes at index 5..6
	got := truncateUTF8(s, 6)
	if strings.ContainsRune(got, 'é') {
		t.Fatalf("truncateUTF8 split a rune: %q", got)
	}
	if got != strings.Repeat("a", 5) {
		t.Fatalf("got %q", got)
	}
}
