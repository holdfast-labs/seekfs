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
	if size > contentExtractMaxRawBytes {
		return contentExtractResult{Skipped: true, Reason: "raw size over cap", Class: contentClassText}, nil
	}
	raw, err := contentReadBounded(r, size, contentExtractMaxRawBytes)
	if err != nil {
		return contentExtractResult{}, err
	}
	// A BOM-marked UTF-16 file is text even though its raw bytes contain NULs;
	// decode first, and only a BOM-less NUL makes a file binary.
	if !contentHasBOM(raw) && contentLooksBinary(raw) {
		return contentExtractResult{Skipped: true, Reason: "binary", Class: contentClassText}, nil
	}
	text := contentDecodeForIndex(raw, contentEncodingMode{auto: true})
	if !contentHasBOM(raw) && contentLooksBinary([]byte(text)) {
		return contentExtractResult{Skipped: true, Reason: "binary", Class: contentClassText}, nil
	}
	if len(text) > contentExtractMaxTextBytes {
		text = truncateUTF8(text, contentExtractMaxTextBytes)
	}
	return contentExtractResult{Text: []byte(text), Class: contentClassText}, nil
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
