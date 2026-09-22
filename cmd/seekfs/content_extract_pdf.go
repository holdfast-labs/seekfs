package main

// The PDF extractor (WP11). Text-layer only: a bounded, panic-free parser over
// the PDF object graph that decodes the page content streams and returns their
// text. No OCR, and no decryption — a /Encrypt trailer is detected and skipped.
//
// The parser is hand-rolled because the available libraries are unsuitable
// (ledongthuc/pdf panics on malformed input, pdfcpu does not extract text,
// unipdf is commercial). It is deliberately bounded at every level:
//
//   - input: the whole file must fit maxRaw, else the document is skipped;
//   - decode: every filter output (Flate/LZW/ASCIIHex/ASCII85/RunLength,
//     predictors included) is hard-capped, so a decompression bomb stops at the
//     cap instead of materialising gigabytes;
//   - objects: bounded value count, array/dict sizes, nesting depth, object
//     count and /ObjStm size, with every count read from the file clamped;
//   - output: text is truncated at maxText with Truncated set.
//
// A malformed PDF is a Skipped, never a crash, a hang or a huge allocation. The
// coordinator's recover/deadline is a backstop, not the correctness mechanism.

import (
	"bytes"
	"context"
	"io"
)

type contentPDFExtractor struct{}

func (contentPDFExtractor) Name() string    { return "pdf" }
func (contentPDFExtractor) Version() uint16 { return 2 }
func (contentPDFExtractor) Class() uint16   { return contentClassPDF }

func (contentPDFExtractor) Extensions() []string { return []string{".pdf"} }

func (contentPDFExtractor) Sniff(head []byte) bool { return bytes.HasPrefix(head, []byte("%PDF-")) }

func (contentPDFExtractor) Extract(ctx context.Context, r io.ReaderAt, size int64) (res contentExtractResult, err error) {
	// Defensive: the coordinator recovers too, but a directly called extractor
	// must not let a malformed PDF crash the process.
	defer func() {
		if recover() != nil {
			res = contentExtractResult{Skipped: true, Reason: "malformed pdf", Class: contentClassPDF}
			err = nil
		}
	}()

	s := contentExtractSettingsFromContext(ctx)
	// A PDF's xref and text objects can live anywhere, so a bounded prefix is
	// not a usable document: an over-cap PDF is skipped with a visible reason.
	if size > s.maxRaw {
		return contentExtractResult{Skipped: true, Reason: "raw size over cap", Class: contentClassPDF}, nil
	}
	raw, err := contentReadBounded(r, size, int(s.maxRaw))
	if err != nil {
		return contentExtractResult{}, err
	}
	if !bytes.HasPrefix(raw, []byte("%PDF-")) {
		return contentExtractResult{Skipped: true, Reason: "not a pdf", Class: contentClassPDF}, nil
	}

	doc, ok := contentPDFLoadDoc(ctx, raw, s.maxRaw, s.maxText)
	if !ok {
		return contentExtractResult{Skipped: true, Reason: "malformed pdf", Class: contentClassPDF}, nil
	}
	if doc.encrypted {
		return contentExtractResult{Skipped: true, Reason: "encrypted pdf not decrypted", Class: contentClassPDF}, nil
	}

	tw := newContentPDFTextWriter(s.maxText)
	for _, pg := range doc.collectPages() {
		if err := ctx.Err(); err != nil {
			return contentExtractResult{}, err
		}
		if err := doc.extractPageText(pg, tw); err != nil {
			return contentExtractResult{}, err
		}
		if tw.done() {
			break
		}
	}

	if len(tw.out) == 0 {
		return contentExtractResult{Skipped: true, Reason: "no extractable text", Class: contentClassPDF}, nil
	}
	text := truncateUTF8(string(tw.out), int(s.maxText))
	res = contentExtractResult{Text: []byte(text), Class: contentClassPDF, Truncated: tw.truncated}
	if tw.truncated {
		res.Reason = "indexed bounded prefix up to policy cap"
	}
	return res, nil
}

func init() { contentRegisterExtractor(contentPDFExtractor{}) }
