package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestContentPathExtension(t *testing.T) {
	cases := map[string]string{
		`C:\a\b\Readme.MD`:     ".md",
		`C:\a\b\main.go`:       ".go",
		`C:\a\b\noext`:         "",
		`C:\a\b\.gitignore`:    "",
		`C:\a\b\archive.tar.gz`: ".gz",
		`/unix/path/Doc.DOCX`:  ".docx",
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

func TestContentTextExtractorSkipsOversize(t *testing.T) {
	got, err := contentTextExtractor{}.Extract(context.Background(), bytes.NewReader(nil), int64(contentExtractMaxRawBytes)+1)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Skipped {
		t.Fatal("an oversize raw file must be skipped")
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
