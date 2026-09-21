package main

// The plain-text/code extractor: the v1 workhorse. It decodes with the same
// auto/BOM rules the content index uses, so the bytes it returns are exactly
// the bytes a later search matches against.

import (
	"context"
	"io"
)

type contentTextExtractor struct{}

func (contentTextExtractor) Name() string    { return "text" }
func (contentTextExtractor) Version() uint16 { return 1 }

// Extensions is empty: the text extractor is the fallback for any file whose
// extension is not claimed by a richer extractor and which is not binary.
func (contentTextExtractor) Extensions() []string { return nil }

// Sniff accepts any file whose head is not binary, or that carries a BOM (a
// UTF-16 file's bytes contain NULs but it is text). The coordinator also applies
// an extension allowlist before calling, so this is the last-resort claimer.
func (contentTextExtractor) Sniff(head []byte) bool {
	return contentHasBOM(head) || !contentLooksBinary(head)
}

func (contentTextExtractor) Extract(ctx context.Context, r io.ReaderAt, size int64) (contentExtractResult, error) {
	s := contentExtractSettingsFromContext(ctx)
	// Index a bounded prefix of an over-cap text file instead of skipping it:
	// truncation is marked and counted, so the cap is visible, not a silent
	// drop. Memory stays bounded by maxRaw. Container formats (zip/PDF) cannot
	// be prefixed and skip instead.
	raw, err := contentReadBounded(r, size, int(s.maxRaw))
	if err != nil {
		return contentExtractResult{}, err
	}
	truncated := size > s.maxRaw
	// A BOM-marked UTF-16 file is text even though its raw bytes contain NULs;
	// decode first, and only a BOM-less NUL makes a file binary.
	if !contentHasBOM(raw) && contentLooksBinary(raw) {
		return contentExtractResult{Skipped: true, Reason: "binary", Class: contentClassText}, nil
	}
	text := contentDecodeForIndex(raw, s.encoding, truncated)
	if !contentHasBOM(raw) && contentLooksBinary([]byte(text)) {
		return contentExtractResult{Skipped: true, Reason: "binary", Class: contentClassText}, nil
	}
	if int64(len(text)) > s.maxText {
		text = truncateUTF8(text, int(s.maxText))
		truncated = true
	}
	res := contentExtractResult{Text: []byte(text), Class: contentClassText, Truncated: truncated}
	if truncated {
		res.Reason = "indexed bounded prefix up to policy cap"
	}
	return res, nil
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
