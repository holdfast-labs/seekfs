package main

// WP11 email extractor tests: header/body searchability, MIME transfer
// encodings, MHTML and .emlx framing, attachment skipping, and the shared
// bounds (maxText, multipart depth, malformed input).

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func contentEMLExtract(t *testing.T, raw []byte) contentExtractResult {
	t.Helper()
	got, err := contentEMLExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	return got
}

// A simple message's Subject and text/plain body are searchable through a built
// index, and the doc is stamped class=Email / version=1.
func TestContentEMLSimpleHeaderAndBody(t *testing.T) {
	raw := []byte("From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: Hello subjectneedle\r\n" +
		"Date: Mon, 1 Jan 2024 00:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"bodyneedle here.\r\n")

	got := contentEMLExtract(t, raw)
	if got.Class != contentClassEmail || got.Skipped {
		t.Fatalf("result = %+v; want class Email, not skipped", got)
	}
	for _, want := range []string{"subjectneedle", "bodyneedle"} {
		if !strings.Contains(string(got.Text), want) {
			t.Fatalf("text %q missing %q", got.Text, want)
		}
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mail.eml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Docs) != 1 {
		t.Fatalf("built %d docs; want 1", len(idx.Docs))
	}
	d := idx.Docs[0]
	if d.ContentType != contentClassEmail || d.ExtractorVersion != (contentEMLExtractor{}).Version() {
		t.Fatalf("doc identity = (%d,%d); want Email v1", d.ContentType, d.ExtractorVersion)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"subjectneedle", "bodyneedle"} {
		if hits := r.search(term, 0); len(hits) != 1 || hits[0].Path != "mail.eml" {
			t.Fatalf("term %q not findable: %v", term, contentPathsOf(hits))
		}
	}
}

// multipart/alternative: text from both the plain and HTML parts is extracted,
// and HTML tags are not indexed.
func TestContentEMLMultipartAlternative(t *testing.T) {
	raw := []byte("From: a@example.com\r\n" +
		"Subject: alt\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"BOUND\"\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"plainneedle here\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<html><body><p>htmlneedle</p><b>tagged</b></body></html>\r\n" +
		"--BOUND--\r\n")

	text := string(contentEMLExtract(t, raw).Text)
	for _, want := range []string{"plainneedle", "htmlneedle", "tagged"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q missing %q", text, want)
		}
	}
	for _, banned := range []string{"<html>", "<p>", "<b>"} {
		if strings.Contains(text, banned) {
			t.Fatalf("extracted markup %q: %q", banned, text)
		}
	}
}

// Quoted-printable and encoded-word headers decode; a UTF-8 body survives.
func TestContentEMLQuotedPrintableAndEncodedWord(t *testing.T) {
	raw := []byte("From: =?utf-8?Q?Jos=C3=A9?= <jose@example.com>\r\n" +
		"Subject: =?utf-8?Q?subjectneedle?=\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"qpneedle caf=C3=A9=20here\r\n")

	text := string(contentEMLExtract(t, raw).Text)
	for _, want := range []string{"subjectneedle", "qpneedle", "café", "José"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "=?utf-8?") || strings.Contains(text, "=C3=A9") {
		t.Fatalf("encoding not decoded: %q", text)
	}
}

// A base64 body decodes.
func TestContentEMLBase64Body(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("base64needle here"))
	raw := []byte("Subject: b64\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" + enc + "\r\n")
	text := string(contentEMLExtract(t, raw).Text)
	if !strings.Contains(text, "base64needle") {
		t.Fatalf("base64 body not decoded: %q", text)
	}
}

// An attachment part is skipped: none of its bytes are indexed.
func TestContentEMLAttachmentSkipped(t *testing.T) {
	att := base64.StdEncoding.EncodeToString([]byte("attachneedle attachment payload"))
	raw := []byte("Subject: att\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"bodyneedle\r\n" +
		"--B\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" + att + "\r\n" +
		"--B--\r\n")

	text := string(contentEMLExtract(t, raw).Text)
	if !strings.Contains(text, "bodyneedle") {
		t.Fatalf("text part lost: %q", text)
	}
	if strings.Contains(text, "attachneedle") {
		t.Fatalf("attachment bytes indexed: %q", text)
	}
}

// A body-only-attachments message yields no text at all, and is not an error.
func TestContentEMLAttachmentsOnlyNoText(t *testing.T) {
	att := base64.StdEncoding.EncodeToString([]byte("attachneedle payload"))
	raw := []byte("MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" + att + "\r\n" +
		"--B--\r\n")
	got := contentEMLExtract(t, raw)
	if got.Skipped {
		t.Fatalf("body-only attachments must not be Skipped: %+v", got)
	}
	if len(got.Text) != 0 {
		t.Fatalf("body-only attachments yielded text: %q", got.Text)
	}
}

// Apple .emlx: the leading byte-count line is stripped and the trailing plist is
// not indexed.
func TestContentEMLEMLXFraming(t *testing.T) {
	eml := "Subject: emlxneedle\r\nContent-Type: text/plain\r\n\r\nemlxbodyneedle\r\n"
	raw := []byte(strconv.Itoa(len(eml)) + "\n" + eml +
		"<?xml version=\"1.0\"?><plist><dict><key>X</key><string>plistneedle</string></dict></plist>")
	text := string(contentEMLExtract(t, raw).Text)
	for _, want := range []string{"emlxneedle", "emlxbodyneedle"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "plistneedle") {
		t.Fatalf("trailing plist indexed: %q", text)
	}
}

// MHTML (.mht): the multipart/related text/html part is extracted, resources
// are skipped.
func TestContentEMLMHT(t *testing.T) {
	img := base64.StdEncoding.EncodeToString([]byte("imageneeedle resource bytes"))
	raw := []byte("From: <Saved by Blink>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/related; boundary=\"MHTMLBOUND\"\r\n" +
		"\r\n" +
		"--MHTMLBOUND\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"<html><body><p>mhtmlneedle</p></body></html>\r\n" +
		"--MHTMLBOUND\r\n" +
		"Content-Type: image/png\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" + img + "\r\n" +
		"--MHTMLBOUND--\r\n")

	text := string(contentEMLExtract(t, raw).Text)
	if !strings.Contains(text, "mhtmlneedle") {
		t.Fatalf("mhtml html part not extracted: %q", text)
	}
	if strings.Contains(text, "imageneeedle") || strings.Contains(text, "<html>") {
		t.Fatalf("mhtml resource or markup indexed: %q", text)
	}
}

// Malformed MIME must not panic: header-less junk and an empty multipart
// boundary both degrade to a skip or empty text.
func TestContentEMLMalformedDoesNotPanic(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("this is not a mime message at all\x00\x01\x02"),
		[]byte("From: a@b\r\nContent-Type: multipart/mixed; boundary=\"\"\r\n\r\nbody"),
		[]byte("Content-Type: multipart/mixed\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nstuck"),
	} {
		got, err := contentEMLExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatalf("malformed input returned error: %v", err)
		}
		if got.Class != contentClassEmail {
			t.Fatalf("malformed input class = %d; want Email", got.Class)
		}
	}
}

// contentEMLDeepMultipart builds a message nested depth levels deep, with a
// text/plain leaf at the bottom.
func contentEMLDeepMultipart(depth int) []byte {
	var b strings.Builder
	b.WriteString("MIME-Version: 1.0\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=\"B0\"\n\n")
	for i := 0; i < depth; i++ {
		b.WriteString("--B" + strconv.Itoa(i) + "\n")
		if i == depth-1 {
			b.WriteString("Content-Type: text/plain\n\n")
			b.WriteString("deepneedle\n")
			break
		}
		b.WriteString("Content-Type: multipart/mixed; boundary=\"B" + strconv.Itoa(i+1) + "\"\n\n")
	}
	for i := depth - 1; i >= 0; i-- {
		b.WriteString("--B" + strconv.Itoa(i) + "--\n")
	}
	return []byte(b.String())
}

// A deeply nested multipart is depth-bounded: no panic, no unbounded recursion,
// and the walk stops with a truncation signal.
func TestContentEMLDeepNestingBounded(t *testing.T) {
	raw := contentEMLDeepMultipart(contentEMLMaxDepth + 12)
	got := contentEMLExtract(t, raw)
	if !got.Truncated {
		t.Fatalf("depth bound not signalled: %+v", got)
	}
	if len(got.Text) != 0 {
		t.Fatalf("text below the depth bound was indexed: %q", got.Text)
	}
}

// Output is capped at maxText with a truncation reason.
func TestContentEMLTruncatesAtMaxText(t *testing.T) {
	raw := []byte("Subject: trunc\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nalpha needle beta gamma delta\r\n")
	ctx := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: 1 << 20, maxText: 8})
	got, err := contentEMLExtractor{}.Extract(ctx, bytes.NewReader(raw), int64(len(raw)))
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

func TestContentServiceAllowlistIncludesEML(t *testing.T) {
	for _, ext := range []string{".eml", ".emlx", ".mht"} {
		if _, ok := contentServiceExtensions()[ext]; !ok {
			t.Fatalf("%s missing from the service content allowlist", ext)
		}
	}
	if e := contentExtractorForPath("mail.eml", []byte("Subject: hi\r\n\r\nbody")); e == nil || e.Name() != "email" {
		t.Fatalf(".eml claimed by %v; want email", e)
	}
}
