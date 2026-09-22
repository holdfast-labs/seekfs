package main

// The email extractor (WP11): .eml, Apple .emlx, and MHTML .mht. Email is the
// one common container that hides its searchable text behind transfer encodings
// (quoted-printable/base64) and a MIME part tree, so indexing the raw file would
// index encoded bytes rather than the message. This extractor parses the header
// block, walks the part tree, decodes each text part, and skips attachments
// entirely (body-only for v1).
//
// Everything is bounded by the shared extraction policy: at most maxRaw source
// bytes are read, at most maxText bytes are emitted, multipart recursion is
// depth-capped, and ctx is checked between parts.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"

	"golang.org/x/text/encoding/htmlindex"
)

// contentEMLMaxDepth bounds multipart recursion. A hostile message can nest
// multiparts or point a part's boundary back at an ancestor; the depth counter
// stops both without an unbounded chain of open parts.
const contentEMLMaxDepth = 8

// contentEMLHeaders are the message headers made searchable. Body-only
// extraction would otherwise lose the sender/subject/date a user searches by.
var contentEMLHeaders = []string{"Subject", "From", "To", "Cc", "Date"}

type contentEMLExtractor struct{}

func (contentEMLExtractor) Name() string    { return "email" }
func (contentEMLExtractor) Version() uint16 { return 1 }
func (contentEMLExtractor) Class() uint16   { return contentClassEmail }

func (contentEMLExtractor) Extensions() []string {
	return []string{".eml", ".emlx", ".mht"}
}

// Sniff declines. The three extensions cover the format, and claiming an
// extensionless raw MIME blob would risk indexing its encoded body.
func (contentEMLExtractor) Sniff([]byte) bool { return false }

func (contentEMLExtractor) Extract(ctx context.Context, r io.ReaderAt, size int64) (res contentExtractResult, err error) {
	// Defensive: the coordinator recovers from a panic too, but a directly
	// called extractor must not let a malformed message crash the process.
	defer func() {
		if recover() != nil {
			res = contentExtractResult{Skipped: true, Reason: "malformed message", Class: contentClassEmail}
			err = nil
		}
	}()

	s := contentExtractSettingsFromContext(ctx)
	raw, err := contentReadBounded(r, size, int(s.maxRaw))
	if err != nil {
		return contentExtractResult{}, err
	}
	truncated := size > s.maxRaw
	// Apple .emlx wraps an EML in a byte-count line + trailing plist. Strip that
	// framing on the raw bytes (the count is a byte length) before decoding.
	if body, ok := contentEMLEMLXBody(raw); ok {
		raw = body
	}
	decoded := contentDecodeForIndex(raw, s.encoding, truncated)

	// mail.ReadMessage parses the header block and hands back the body, which
	// may itself be a multipart tree. A message whose headers cannot be parsed
	// is skipped, not failed.
	msg, merr := mail.ReadMessage(strings.NewReader(decoded))
	if merr != nil {
		return contentExtractResult{Skipped: true, Reason: "malformed message", Class: contentClassEmail}, nil
	}

	text, wtrunc, werr := contentEMLMessageText(ctx, msg.Header, msg.Body, s.maxText)
	if werr != nil {
		return contentExtractResult{}, werr
	}
	truncated = truncated || wtrunc
	if text == "" {
		return contentExtractResult{Class: contentClassEmail, Truncated: truncated}, nil
	}
	text = truncateUTF8(text, int(s.maxText))
	res = contentExtractResult{Text: []byte(text), Class: contentClassEmail, Truncated: truncated}
	if truncated {
		res.Reason = "indexed bounded prefix up to policy cap"
	}
	return res, nil
}

// contentEMLMessageText extracts one parsed message's searchable text: the
// selected headers plus the decoded text/plain and text/html parts, skipping
// attachments. It is the per-message core shared by the .eml/.emlx/.mht and
// .mbox extractors. truncated reports that the walk stopped at maxText.
func contentEMLMessageText(ctx context.Context, header mail.Header, body io.Reader, maxText int64) (text string, truncated bool, err error) {
	w := &contentEMLWalker{ctx: ctx, maxText: maxText}
	w.appendHeaders(header)
	if err := w.processPart(textproto.MIMEHeader(header), body, 0); err != nil {
		return "", w.truncated, err
	}
	return w.out.String(), w.truncated, nil
}

// contentEMLEMLXBody detects Apple's .emlx framing: a first line holding the
// decimal byte length of the message, then the message, then an XML plist. It
// returns just the message bytes. A plain EML's first line is a header, which
// always contains a colon, so the all-digits test cannot misfire.
func contentEMLEMLXBody(raw []byte) ([]byte, bool) {
	nl := bytes.IndexByte(raw, '\n')
	if nl <= 0 {
		return raw, false
	}
	line := bytes.TrimRight(raw[:nl], "\r")
	if len(line) == 0 {
		return raw, false
	}
	for _, c := range line {
		if c < '0' || c > '9' {
			return raw, false
		}
	}
	n, err := strconv.Atoi(string(line))
	if err != nil || n < 0 {
		return raw, false
	}
	body := raw[nl+1:]
	if n < len(body) {
		body = body[:n]
	}
	return body, true
}

// contentEMLWordDecoder decodes RFC 2047 encoded-words in headers, using the
// WHATWG label table for non-UTF-8 charsets. An unknown label passes the bytes
// through so one odd header cannot blank the whole extraction.
func contentEMLWordDecoder() mime.WordDecoder {
	return mime.WordDecoder{CharsetReader: func(label string, input io.Reader) (io.Reader, error) {
		enc, err := htmlindex.Get(label)
		if err != nil || enc == nil {
			return input, nil
		}
		return enc.NewDecoder().Reader(input), nil
	}}
}

// contentEMLMediaType resolves a part's media type and parameters, defaulting a
// missing or unparseable Content-Type to text/plain.
func contentEMLMediaType(ctype string) (string, map[string]string) {
	if strings.TrimSpace(ctype) == "" {
		return "text/plain", nil
	}
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		return "text/plain", nil
	}
	return strings.ToLower(mt), params
}

// contentEMLDecodeCharset maps a part's declared body charset to UTF-8. A
// missing/UTF-8/ASCII label needs no work; other labels go through the shared
// decoder, so a legacy part is repaired the same way a file is.
func contentEMLDecodeCharset(b []byte, label string) string {
	label = strings.TrimSpace(label)
	switch strings.ToLower(label) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return string(b)
	}
	mode, err := parseContentEncoding(label)
	if err != nil {
		return contentRepairString(b, false)
	}
	return contentDecodeForIndex(b, mode, false)
}

type contentEMLWalker struct {
	ctx       context.Context
	out       bytes.Buffer
	maxText   int64
	truncated bool
	done      bool
}

// appendHeaders folds the selected headers into the text with their RFC 2047
// encoded-words decoded.
func (w *contentEMLWalker) appendHeaders(h mail.Header) {
	dec := contentEMLWordDecoder()
	for _, name := range contentEMLHeaders {
		for _, v := range h[name] {
			s, err := dec.DecodeHeader(v)
			if err != nil {
				s = v
			}
			w.appendText(s)
		}
	}
}

// appendText adds a decoded chunk, separating it from the previous one, and
// stops the walk once the text cap is reached.
func (w *contentEMLWalker) appendText(s string) {
	if s == "" || w.done {
		return
	}
	if w.out.Len() > 0 {
		switch w.out.Bytes()[w.out.Len()-1] {
		case '\n', ' ', '\t':
		default:
			w.out.WriteByte('\n')
		}
	}
	w.out.WriteString(s)
	if int64(w.out.Len()) >= w.maxText {
		w.truncated = true
		w.done = true
	}
}

// readDecoded applies a part's Content-Transfer-Encoding and returns at most
// maxText bytes. A malformed base64/quoted-printable body yields whatever
// decoded before the error rather than failing the message.
func (w *contentEMLWalker) readDecoded(body io.Reader, cte string) []byte {
	var r io.Reader = body
	switch cte {
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	}
	limit := w.maxText
	if limit < 1 {
		limit = 1
	}
	b, _ := io.ReadAll(io.LimitReader(r, limit))
	return b
}

// processPart decodes one MIME entity given its already-parsed header and body
// reader. depth counts multipart nesting; reaching the bound stops the walk with
// Truncated rather than recursing further.
func (w *contentEMLWalker) processPart(header textproto.MIMEHeader, body io.Reader, depth int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.done {
		return nil
	}
	mediaType, params := contentEMLMediaType(header.Get("Content-Type"))
	cte := strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding")))

	if strings.HasPrefix(mediaType, "multipart/") {
		if depth >= contentEMLMaxDepth {
			w.truncated = true
			return nil
		}
		boundary := params["boundary"]
		if boundary == "" {
			// multipart.NewReader panics on an empty boundary; a message that
			// declares none has no parts to walk.
			return nil
		}
		mr := multipart.NewReader(body, boundary)
		for {
			if err := w.ctx.Err(); err != nil {
				return err
			}
			if w.done {
				return nil
			}
			part, perr := mr.NextRawPart()
			if perr != nil {
				if !errors.Is(perr, io.EOF) {
					w.truncated = true
				}
				return nil
			}
			if err := w.processPart(part.Header, part, depth+1); err != nil {
				return err
			}
		}
	}

	switch {
	case mediaType == "text/html":
		data := contentEMLDecodeCharset(w.readDecoded(body, cte), params["charset"])
		text, htmlTruncated, err := contentHTMLText(w.ctx, data, w.maxText)
		if err != nil {
			return err
		}
		if htmlTruncated {
			w.truncated = true
		}
		w.appendText(text)
	case strings.HasPrefix(mediaType, "text/"):
		data := contentEMLDecodeCharset(w.readDecoded(body, cte), params["charset"])
		w.appendText(string(data))
	default:
		// Attachment or other non-text entity: skipped. Its body is never
		// buffered; the multipart reader discards the remainder on the next
		// NextRawPart call.
	}
	return nil
}

func init() { contentRegisterExtractor(contentEMLExtractor{}) }
