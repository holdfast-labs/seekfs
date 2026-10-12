package main

// WP11 mbox extractor tests: the From_ splitter, mboxrd unescaping, attachment
// skipping, the shared bounds (maxText, malformed input), and the service
// allowlist.

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func contentMboxExtract(t *testing.T, raw []byte) contentExtractResult {
	t.Helper()
	got, err := contentMboxExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	return got
}

// Two messages in one mailbox: each message's subject and body are searchable
// through a built index, and the doc is stamped class=Mbox / version=1.
func TestContentMboxTwoMessagesSearchable(t *testing.T) {
	raw := []byte("From alice@example.com Mon Jan  1 00:00:00 2024\n" +
		"From: Alice <alice@example.com>\n" +
		"Subject: onesubjneedle\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"\n" +
		"onebodyneedle here\n" +
		"From bob@example.com Mon Jan  1 00:00:00 2024\n" +
		"From: Bob <bob@example.com>\n" +
		"Subject: twosubjneedle\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"\n" +
		"twobodyneedle here\n")

	got := contentMboxExtract(t, raw)
	if got.Class != contentClassMbox || got.Skipped {
		t.Fatalf("result = %+v; want class Mbox, not skipped", got)
	}
	for _, want := range []string{"onesubjneedle", "onebodyneedle", "twosubjneedle", "twobodyneedle"} {
		if !strings.Contains(string(got.Text), want) {
			t.Fatalf("text %q missing %q", got.Text, want)
		}
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mail.mbox"), raw, 0o644); err != nil {
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
	if d.ContentType != contentClassMbox || d.ExtractorVersion != (contentMboxExtractor{}).Version() {
		t.Fatalf("doc identity = (%d,%d); want Mbox v1", d.ContentType, d.ExtractorVersion)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"onesubjneedle", "onebodyneedle", "twosubjneedle", "twobodyneedle"} {
		if hits := r.search(term, 0); len(hits) != 1 || hits[0].Path != "mail.mbox" {
			t.Fatalf("term %q not findable: %v", term, contentPathsOf(hits))
		}
	}
}

// mboxrd quoting: ">From " unescapes to "From " and, being quoted, is not a
// message separator.
func TestContentMboxMboxrdUnescape(t *testing.T) {
	raw := []byte("From alice@example.com Mon Jan  1 00:00:00 2024\n" +
		"Subject: quoted\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"\n" +
		"bodyneedle\n" +
		">From quotedneedle\n" +
		">>From deepneedle\n" +
		"From bob@example.com Mon Jan  1 00:00:00 2024\n" +
		"Subject: second\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"\n" +
		"secondbodyneedle\n")

	text := string(contentMboxExtract(t, raw).Text)
	for _, want := range []string{"From quotedneedle", ">From deepneedle", "secondbodyneedle"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q missing %q", text, want)
		}
	}
	if strings.Contains(text, ">From quotedneedle") {
		t.Fatalf("mboxrd escape left in text: %q", text)
	}
}

// The splitter drops the From_ line and undoes mboxrd quoting; a message with
// no leading From_ line still yields its content.
func TestContentMboxSplit(t *testing.T) {
	raw := "From a@b Mon Jan  1 00:00:00 2024\n" +
		"Subject: one\n\nbodyone\n" +
		">From escaped\n" +
		"From c@d Mon Jan  1 00:00:00 2024\n" +
		"Subject: two\n\nbodytwo\n"
	var msgs []string
	contentMboxSplit(raw, func(msg string) bool {
		msgs = append(msgs, msg)
		return true
	})
	if len(msgs) != 2 {
		t.Fatalf("split into %d messages; want 2: %q", len(msgs), msgs)
	}
	if !strings.HasPrefix(msgs[0], "Subject: one") || !strings.Contains(msgs[0], "From escaped") {
		t.Fatalf("message 1 = %q", msgs[0])
	}
	if !strings.HasPrefix(msgs[1], "Subject: two") {
		t.Fatalf("message 2 = %q", msgs[1])
	}

	// A mailbox missing its leading From_ line still yields the content.
	msgs = nil
	contentMboxSplit("Subject: lone\n\nbody\n", func(msg string) bool {
		msgs = append(msgs, msg)
		return true
	})
	if len(msgs) != 1 || !strings.Contains(msgs[0], "body") {
		t.Fatalf("headerless-leading split = %q", msgs)
	}
}

// A message with an attachment: the text part is indexed, the attachment is not.
func TestContentMboxAttachmentSkipped(t *testing.T) {
	att := base64.StdEncoding.EncodeToString([]byte("attachneedle attachment payload"))
	raw := []byte("From a@b Mon Jan  1 00:00:00 2024\n" +
		"Subject: att\n" +
		"MIME-Version: 1.0\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\n" +
		"\n" +
		"--B\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"\n" +
		"bodyneedle\n" +
		"--B\n" +
		"Content-Type: application/octet-stream\n" +
		"Content-Transfer-Encoding: base64\n" +
		"\n" + att + "\n" +
		"--B--\n")

	text := string(contentMboxExtract(t, raw).Text)
	if !strings.Contains(text, "bodyneedle") {
		t.Fatalf("text part lost: %q", text)
	}
	if strings.Contains(text, "attachneedle") {
		t.Fatalf("attachment bytes indexed: %q", text)
	}
}

// Output is capped at maxText with a truncation reason.
func TestContentMboxTruncatesAtMaxText(t *testing.T) {
	raw := []byte("From a@b Mon Jan  1 00:00:00 2024\n" +
		"Subject: trunc\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"\n" +
		"alpha needle beta gamma delta\n")
	ctx := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: 1 << 20, maxText: 8})
	got, err := contentMboxExtractor{}.Extract(ctx, bytes.NewReader(raw), int64(len(raw)))
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

// Malformed or empty input yields no text and no error, never a panic.
func TestContentMboxMalformedDoesNotPanic(t *testing.T) {
	for _, raw := range [][]byte{
		{},
		[]byte("this is not a mailbox at all\x00\x01\x02"),
		[]byte("From \nFrom \n"),
		[]byte("From a@b Mon Jan  1 00:00:00 2024\nno colon header line\n"),
	} {
		got, err := contentMboxExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatalf("malformed input %q returned error: %v", raw, err)
		}
		if got.Class != contentClassMbox {
			t.Fatalf("malformed input %q class = %d; want Mbox", raw, got.Class)
		}
		if len(got.Text) != 0 {
			t.Fatalf("malformed input %q yielded text: %q", raw, got.Text)
		}
	}
}

func TestContentServiceAllowlistIncludesMbox(t *testing.T) {
	if _, ok := contentServiceExtensions()[".mbox"]; !ok {
		t.Fatal(".mbox missing from the service content allowlist")
	}
	if e := contentExtractorForPath("mail.mbox", []byte("From a@b\n\nbody")); e == nil || e.Name() != "mbox" {
		t.Fatalf(".mbox claimed by %v; want mbox", e)
	}
}
