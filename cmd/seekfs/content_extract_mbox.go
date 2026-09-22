package main

// The mbox extractor (WP11): a concatenation of email messages separated by
// "From " lines. It hand-rolls the From_ splitter (the mbox format has no
// stdlib parser) and reuses the EML extractor's per-message parsing, so a
// mailbox's subjects and bodies are indexed exactly as a single .eml would be.
//
// mboxrd quoting is undone on read: a body line matching ^>+From_ had a '>'
// prepended on write, so one leading '>' is stripped. A quoted line therefore
// neither looks like a separator nor leaves its escape in the index.
//
// Everything is bounded by the shared extraction policy: at most maxRaw source
// bytes are read, at most maxText bytes are emitted, the message count is
// capped, and ctx is checked between messages.

import (
	"bytes"
	"context"
	"io"
	"net/mail"
	"strings"
)

// contentMboxMaxMessages bounds how many messages one mailbox contributes. A
// pathological file of repeated "From " lines is otherwise millions of tiny
// messages; reaching the cap stops with Truncated rather than a silent drop.
const contentMboxMaxMessages = 1 << 16

type contentMboxExtractor struct{}

func (contentMboxExtractor) Name() string    { return "mbox" }
func (contentMboxExtractor) Version() uint16 { return 1 }
func (contentMboxExtractor) Class() uint16   { return contentClassMbox }

func (contentMboxExtractor) Extensions() []string { return []string{".mbox"} }

// Sniff declines: the extension covers the format, and claiming an
// extensionless blob would risk splitting arbitrary text on "From " lines.
func (contentMboxExtractor) Sniff([]byte) bool { return false }

func (contentMboxExtractor) Extract(ctx context.Context, r io.ReaderAt, size int64) (res contentExtractResult, err error) {
	// Defensive: a directly called extractor must not let malformed input crash.
	defer func() {
		if recover() != nil {
			res = contentExtractResult{Skipped: true, Reason: "malformed mailbox", Class: contentClassMbox}
			err = nil
		}
	}()

	s := contentExtractSettingsFromContext(ctx)
	raw, err := contentReadBounded(r, size, int(s.maxRaw))
	if err != nil {
		return contentExtractResult{}, err
	}
	truncated := size > s.maxRaw
	decoded := contentDecodeForIndex(raw, s.encoding, truncated)

	var out bytes.Buffer
	count := 0
	var walkErr error
	contentMboxSplit(decoded, func(msg string) bool {
		if err := ctx.Err(); err != nil {
			walkErr = err
			return false
		}
		if int64(out.Len()) >= s.maxText {
			truncated = true
			return false
		}
		if count >= contentMboxMaxMessages {
			truncated = true
			return false
		}
		count++

		parsed, perr := mail.ReadMessage(strings.NewReader(msg))
		if perr != nil {
			// A message whose headers cannot be parsed is skipped, not failed.
			return true
		}
		remaining := s.maxText - int64(out.Len())
		if remaining <= 0 {
			truncated = true
			return false
		}
		text, mtrunc, merr := contentEMLMessageText(ctx, parsed.Header, parsed.Body, remaining)
		if merr != nil {
			walkErr = merr
			return false
		}
		if mtrunc {
			truncated = true
		}
		if text != "" {
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			out.WriteString(text)
		}
		if mtrunc || int64(out.Len()) >= s.maxText {
			truncated = true
			return false
		}
		return true
	})
	if walkErr != nil {
		return contentExtractResult{}, walkErr
	}
	if out.Len() == 0 {
		return contentExtractResult{Class: contentClassMbox, Truncated: truncated}, nil
	}
	text := truncateUTF8(out.String(), int(s.maxText))
	res = contentExtractResult{Text: []byte(text), Class: contentClassMbox, Truncated: truncated}
	if truncated {
		res.Reason = "indexed bounded prefix up to policy cap"
	}
	return res, nil
}

// contentMboxSplit splits a mailbox into messages and calls yield with each
// message's bytes: the "From " separator line is dropped and mboxrd quoting is
// undone. yield returns false to stop early. Bytes before the first separator
// are treated as the first message, so a mailbox missing its leading From_ line
// still yields its content; empty regions are skipped.
func contentMboxSplit(raw string, yield func(msg string) bool) {
	start := 0
	for i := 0; i < len(raw); {
		nl := strings.IndexByte(raw[i:], '\n')
		var lineEnd int
		if nl < 0 {
			lineEnd = len(raw)
		} else {
			lineEnd = i + nl + 1
		}
		if contentMboxSeparator(raw[i:lineEnd]) {
			if i > start && !yield(contentMboxUnescape(raw[start:i])) {
				return
			}
			start = lineEnd
		}
		if nl < 0 {
			break
		}
		i = lineEnd
	}
	if start < len(raw) {
		yield(contentMboxUnescape(raw[start:]))
	}
}

// contentMboxSeparator reports whether a whole line opens a new message. The
// separator is exactly "From " at the start of a line; a trailing \r is
// irrelevant, and a quoted ">From " line is not a separator.
func contentMboxSeparator(line string) bool {
	return strings.HasPrefix(line, "From ")
}

// contentMboxUnescape undoes mboxrd quoting: every line matching ^>+From_ has
// one leading '>' removed, so ">From x" becomes "From x" and ">>From x" becomes
// ">From x". Messages with no such line are returned as-is (no copy).
func contentMboxUnescape(msg string) string {
	if !strings.Contains(msg, ">From ") {
		return msg
	}
	var b strings.Builder
	b.Grow(len(msg))
	for i := 0; i < len(msg); {
		nl := strings.IndexByte(msg[i:], '\n')
		var lineEnd int
		if nl < 0 {
			lineEnd = len(msg)
		} else {
			lineEnd = i + nl + 1
		}
		line := msg[i:lineEnd]
		if contentMboxQuotedFrom(line) {
			b.WriteString(line[1:])
		} else {
			b.WriteString(line)
		}
		i = lineEnd
	}
	return b.String()
}

// contentMboxQuotedFrom reports whether a line matches ^>+From_, the mboxrd
// escape shape.
func contentMboxQuotedFrom(line string) bool {
	j := 0
	for j < len(line) && line[j] == '>' {
		j++
	}
	return j > 0 && strings.HasPrefix(line[j:], "From ")
}

func init() { contentRegisterExtractor(contentMboxExtractor{}) }
