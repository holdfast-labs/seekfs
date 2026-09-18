package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"strings"
	"testing"
)

// contentPDFTestDoc assembles a minimal single-stream PDF around content.
// compress selects /FlateDecode and the /Length is computed from the bytes
// actually written, so the parser's direct-length path is exercised.
func contentPDFTestDoc(content []byte, compress bool) []byte {
	body := content
	filter := ""
	if compress {
		var buf bytes.Buffer
		zw := zlib.NewWriter(&buf)
		zw.Write(content)
		zw.Close()
		body = buf.Bytes()
		filter = " /Filter /FlateDecode"
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	b.WriteString("1 0 obj\n")
	fmt.Fprintf(&b, "<< /Length %d%s >>\n", len(body), filter)
	b.WriteString("stream\n")
	b.Write(body)
	b.WriteString("\nendstream\nendobj\n%%EOF\n")
	return b.Bytes()
}

func contentPDFTestExtract(t *testing.T, doc []byte) contentExtractResult {
	t.Helper()
	got, err := contentPDFExtractor{}.Extract(context.Background(), bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	return got
}

func TestContentPDFExtractsPlainText(t *testing.T) {
	doc := contentPDFTestDoc([]byte("BT /F1 12 Tf (alpha needle beta) Tj ET"), false)
	got := contentPDFTestExtract(t, doc)
	if !strings.Contains(string(got.Text), "alpha needle beta") {
		t.Fatalf("text %q missing needle; result %+v", got.Text, got)
	}
	if got.Class != contentClassPDF || got.Skipped {
		t.Fatalf("got %+v", got)
	}
}

func TestContentPDFExtractsFlateText(t *testing.T) {
	doc := contentPDFTestDoc([]byte("BT /F1 12 Tf (alpha needle beta) Tj ET"), true)
	got := contentPDFTestExtract(t, doc)
	if !strings.Contains(string(got.Text), "alpha needle beta") {
		t.Fatalf("text %q missing needle; result %+v", got.Text, got)
	}
	if got.Class != contentClassPDF || got.Skipped {
		t.Fatalf("got %+v", got)
	}
}

func TestContentPDFExtractsTJArray(t *testing.T) {
	doc := contentPDFTestDoc([]byte("BT /F1 12 Tf [(hel) -50 (lo)] TJ ET"), false)
	got := contentPDFTestExtract(t, doc)
	text := string(got.Text)
	if !strings.Contains(text, "hel") || !strings.Contains(text, "lo") {
		t.Fatalf("text %q missing TJ operands; result %+v", text, got)
	}
}

func TestContentPDFSkipsNonPDF(t *testing.T) {
	raw := []byte("this is definitely not a pdf file")
	got := contentPDFTestExtract(t, raw)
	if !got.Skipped || got.Reason != "not a pdf" {
		t.Fatalf("got %+v", got)
	}
}

func TestContentPDFMalformedDoesNotPanic(t *testing.T) {
	cases := [][]byte{
		[]byte("%PDF-1.4"),
		[]byte("%PDF-"),
		[]byte("%PDF-1.4\n1 0 obj\n<< /Length 100 >>\nstream\nshort"),
		[]byte("%PDF-1.4\nstream\n(BT (unclosed) Tj"),
		[]byte("%PDF-1.4\n<< /Filter /FlateDecode >>\nstream\n\x00\x01garbage\nendstream"),
		append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte{0x00, 0x28, 0x5c, 0x29, 0xff}, 64)...),
	}
	for i, raw := range cases {
		got, err := contentPDFExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatalf("case %d: unexpected error %v", i, err)
		}
		if !got.Skipped && len(got.Text) != 0 {
			t.Fatalf("case %d: expected skipped or empty, got %+v", i, got)
		}
	}
}
