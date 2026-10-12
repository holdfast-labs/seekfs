package main

// WP11 validation: one fuzz target per parser. Each asserts the extractor
// contract on arbitrary bytes — no panic (the safe path recovers and reports
// contentExtractPanicError), bounded output (never over the policy's maxRaw /
// maxText), valid UTF-8 text, and a non-empty Reason whenever the result is a
// skip. The per-document deadline bounds runtime; the extractors' own caps bound
// memory, so even an input that ignores ctx cannot stall the fuzz worker past
// the deadline.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
	"unicode/utf8"
)

// contentFuzzExtractor is the shared driver: seeds f, then fuzzes e through the
// production safe path with small caps so iterations stay fast.
func contentFuzzExtractor(f *testing.F, e contentExtractor, seeds ...[]byte) {
	for _, s := range seeds {
		f.Add(s)
	}
	restore := contentExtractDocTimeout
	contentExtractDocTimeout = 10 * time.Second
	f.Cleanup(func() { contentExtractDocTimeout = restore })

	const cap = 4 << 20
	f.Fuzz(func(t *testing.T, raw []byte) {
		ctx := contentWithExtractSettings(context.Background(), contentExtractSettings{
			maxRaw:  cap,
			maxText: cap,
		})
		res, err := contentExtractSafely(ctx, e, bytes.NewReader(raw), int64(len(raw)))
		if errors.Is(err, contentExtractPanicError) {
			t.Fatalf("%s panicked on %d bytes", e.Name(), len(raw))
		}
		// A deadline can surface either as the wrapper's skip or, if the
		// extractor's goroutine returns first, as its own context error with
		// Skipped unset; neither is a contract violation.
		if err != nil && !res.Skipped && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("%s returned err %v without Skipped", e.Name(), err)
		}
		if int64(len(res.Text)) > cap {
			t.Fatalf("%s text %d over cap %d", e.Name(), len(res.Text), cap)
		}
		if len(res.Text) > 0 && !utf8.Valid(res.Text) {
			t.Fatalf("%s returned invalid UTF-8", e.Name())
		}
		if res.Skipped && res.Reason == "" {
			t.Fatalf("%s skipped without a reason", e.Name())
		}
		// On a timeout the wrapper's skip carries no class; the extractor's own
		// results always stamp their declared class.
		if err == nil && res.Class != e.Class() {
			t.Fatalf("%s class %d != declared %d", e.Name(), res.Class, e.Class())
		}
	})
}

func FuzzContentHTMLExtractor(f *testing.F) {
	contentFuzzExtractor(f, contentHTMLExtractor{},
		[]byte(`<!DOCTYPE html><html><head><title>T</title></head><body>hello needle</body></html>`),
		[]byte(`<script>var x=1</script><p>a&nbsp;b</p>`),
		[]byte(``),
	)
}

func FuzzContentEMLExtractor(f *testing.F) {
	contentFuzzExtractor(f, contentEMLExtractor{},
		[]byte("From: a@b\r\nSubject: needle\r\n\r\nbody text\r\n"),
		[]byte("Content-Type: text/plain\r\n\r\nhello"),
		[]byte(``),
	)
}

func FuzzContentMboxExtractor(f *testing.F) {
	contentFuzzExtractor(f, contentMboxExtractor{},
		[]byte("From a@b\nSubject: needle\n\nbody\n\nFrom c@d\nSubject: two\n\nsecond\n"),
		[]byte("From x\n\nFrom_ quoting >From here\n"),
		[]byte(``),
	)
}

func FuzzContentRTFExtractor(f *testing.F) {
	contentFuzzExtractor(f, contentRTFExtractor{},
		[]byte(`{\rtf1\ansi Hello \u233? world\par}`),
		[]byte(`{\rtf1{\fonttbl{\f0 Arial;}}{\*\generator x;}body \'e9\par}`),
		[]byte(``),
	)
}

func FuzzContentMSGLExtractor(f *testing.F) {
	contentFuzzExtractor(f, contentMSGLExtractor{},
		contentMSGSimpleFixture(),
		[]byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1},
		[]byte(``),
	)
}

func FuzzContentPDFExtractor(f *testing.F) {
	contentFuzzExtractor(f, contentPDFExtractor{},
		contentPDFTestDoc([]byte("BT /F1 12 Tf (needle) Tj ET"), true),
		contentPDFTestDoc([]byte("BT /F1 12 Tf (plain) Tj ET"), false),
		[]byte("%PDF-1.7\n"),
		[]byte(``),
	)
}
