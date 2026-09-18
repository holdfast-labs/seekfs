package main

// The PDF extractor: a Phase-0 spike. It is deliberately not a full PDF parser.
// It scans for `stream`/`endstream` markers, inflates `/FlateDecode` streams,
// ignores object streams, and pulls the operands of the text-showing operators
// (Tj, TJ, ', ") out of content streams. That covers the "born-digital, simple
// encoding" case that most searchable PDFs in a code repo fall into. Scanned
// image PDFs, encrypted files, CID/Type0 fonts, and object-stream-only writers
// degrade to Skipped. Correctness on simple files matters more than coverage.

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"io"
	"strconv"
)

type contentPDFExtractor struct{}

func (contentPDFExtractor) Name() string    { return "pdf" }
func (contentPDFExtractor) Version() uint16 { return 1 }

func (contentPDFExtractor) Extensions() []string { return []string{".pdf"} }

func (contentPDFExtractor) Sniff(head []byte) bool { return bytes.HasPrefix(head, []byte("%PDF-")) }

func (contentPDFExtractor) Extract(ctx context.Context, r io.ReaderAt, size int64) (contentExtractResult, error) {
	if size > contentExtractMaxRawBytes {
		return contentExtractResult{Skipped: true, Reason: "raw size over cap", Class: contentClassPDF}, nil
	}
	raw, err := contentReadBounded(r, size, contentExtractMaxRawBytes)
	if err != nil {
		return contentExtractResult{}, err
	}
	if !bytes.HasPrefix(raw, []byte("%PDF-")) {
		return contentExtractResult{Skipped: true, Reason: "not a pdf", Class: contentClassPDF}, nil
	}

	var text []byte
	for i := 0; i < len(raw); {
		if err := ctx.Err(); err != nil {
			return contentExtractResult{}, err
		}
		idx := bytes.Index(raw[i:], []byte("stream"))
		if idx < 0 {
			break
		}
		pos := i + idx
		after := pos + len("stream")
		// Reject the "stream" inside "endstream" and any keyword that is not
		// followed by an EOL as the spec requires.
		if (pos > 0 && contentPDFLetter(raw[pos-1])) ||
			after >= len(raw) || (raw[after] != '\r' && raw[after] != '\n') {
			i = after
			continue
		}
		start := after
		if raw[start] == '\r' {
			start++
		}
		if start < len(raw) && raw[start] == '\n' {
			start++
		}

		var dict []byte
		if ds := bytes.LastIndex(raw[:pos], []byte("<<")); ds >= 0 {
			dict = raw[ds:pos]
		}
		// Object streams hold other objects and their own text is not content.
		if bytes.Contains(dict, []byte("/ObjStm")) {
			i = start
			continue
		}

		end := -1
		if n, ok := contentPDFDictLength(dict); ok && n >= 0 && start+n <= len(raw) {
			end = start + n
		} else if k := bytes.Index(raw[start:], []byte("endstream")); k >= 0 {
			end = start + k
		}
		if end <= start {
			break
		}

		data := raw[start:end]
		if bytes.Contains(dict, []byte("/FlateDecode")) {
			data = contentPDFInflate(data)
		}
		if len(data) > 0 {
			text = contentPDFExtractStreamText(data, text)
			// Cap inside the loop: many inflated streams must not accumulate
			// past the text budget before the single end-of-function truncate.
			if len(text) >= contentExtractMaxTextBytes {
				text = []byte(truncateUTF8(string(text), contentExtractMaxTextBytes))
				break
			}
		}
		i = end
	}

	if len(text) == 0 {
		return contentExtractResult{Skipped: true, Reason: "no extractable text", Class: contentClassPDF}, nil
	}
	if len(text) > contentExtractMaxTextBytes {
		text = []byte(truncateUTF8(string(text), contentExtractMaxTextBytes))
	}
	return contentExtractResult{Text: text, Class: contentClassPDF}, nil
}

// contentPDFInflate best-effort decompresses a stream. zlib framing is tried
// first (the spec's /FlateDecode), then raw deflate for writers that omit the
// header. A partial decode still yields whatever text came out before the
// error, which is more useful than nothing for a search index.
func contentPDFInflate(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		b := contentPDFReadLimit(zr)
		zr.Close()
		if len(b) > 0 {
			return b
		}
	}
	fr := flate.NewReader(bytes.NewReader(data))
	b := contentPDFReadLimit(fr)
	fr.Close()
	if len(b) > 0 {
		return b
	}
	return nil
}

// contentPDFReadLimit caps decompression output so a zip bomb cannot exhaust
// memory; the raw cap is a cheap bound for a spike.
func contentPDFReadLimit(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, contentExtractMaxRawBytes))
	return b
}

// contentPDFExtractStreamText walks one content stream and appends the text of
// string operands to dst. It handles (...), [...] TJ, and the ' and " line
// operators. Strings are emitted only when the operator that follows is one of
// Tj/TJ/'/", so literal data elsewhere in the stream is ignored.
func contentPDFExtractStreamText(data, dst []byte) []byte {
	for i := 0; i < len(data); {
		switch data[i] {
		case '(':
			s, next, ok := contentPDFReadLiteral(data, i)
			if !ok {
				i = next
				continue
			}
			j := contentPDFSkipWS(data, next)
			if op, _ := contentPDFReadOp(data, j); op == "Tj" || op == "'" || op == `"` {
				dst = contentPDFAppendText(dst, s)
			}
			i = next
		case '[':
			strs, next := contentPDFReadArrayStrings(data, i)
			j := contentPDFSkipWS(data, next)
			if op, _ := contentPDFReadOp(data, j); op == "TJ" {
				for _, s := range strs {
					dst = contentPDFAppendText(dst, s)
				}
			}
			i = next
		default:
			i++
		}
	}
	return dst
}

// contentPDFReadLiteral reads a PDF string literal beginning at data[start]=='('
// with balanced parentheses and backslash escapes. It returns the decoded bytes
// and the index just past the closing paren; ok is false when unterminated.
func contentPDFReadLiteral(data []byte, start int) (string, int, bool) {
	var b bytes.Buffer
	depth := 1
	i := start + 1
	for i < len(data) {
		c := data[i]
		switch c {
		case '\\':
			i++
			if i >= len(data) {
				return b.String(), i, true
			}
			switch e := data[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '(', ')', '\\':
				b.WriteByte(e)
			case '\r':
				if i+1 < len(data) && data[i+1] == '\n' {
					i++
				}
			case '\n':
				// Line continuation: emit nothing.
			default:
				if e >= '0' && e <= '7' {
					v, n := 0, 0
					for n < 3 && i < len(data) && data[i] >= '0' && data[i] <= '7' {
						v = v*8 + int(data[i]-'0')
						i++
						n++
					}
					b.WriteByte(byte(v))
					continue
				}
				b.WriteByte(e)
			}
			i++
		case '(':
			depth++
			b.WriteByte(c)
			i++
		case ')':
			depth--
			if depth == 0 {
				return b.String(), i + 1, true
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), i, false
}

// contentPDFReadArrayStrings collects the string literals of an array that
// starts at data[start]=='['. Numbers and other tokens are skipped.
func contentPDFReadArrayStrings(data []byte, start int) ([]string, int) {
	var strs []string
	i := start + 1
	for i < len(data) {
		switch data[i] {
		case ']':
			return strs, i + 1
		case '(':
			s, next, ok := contentPDFReadLiteral(data, i)
			if !ok {
				return strs, next
			}
			strs = append(strs, s)
			i = next
		default:
			i++
		}
	}
	return strs, i
}

// contentPDFSkipWS advances past whitespace and NUL delimiters.
func contentPDFSkipWS(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\r', '\n', '\f', 0:
			i++
		default:
			return i
		}
	}
	return i
}

// contentPDFReadOp reads the operator token at data[i]: a run of letters, or a
// single ' or " operator.
func contentPDFReadOp(data []byte, i int) (string, int) {
	if i >= len(data) {
		return "", i
	}
	if data[i] == '\'' || data[i] == '"' {
		return string(data[i]), i + 1
	}
	start := i
	for i < len(data) && contentPDFLetter(data[i]) {
		i++
	}
	return string(data[start:i]), i
}

// contentPDFAppendText maps a decoded PDF string to UTF-8 and appends it with a
// separating space. Bytes below 0x80 are ASCII; higher bytes are treated as
// Latin-1, a best-effort stand-in for the standard 14 fonts' simple encodings.
func contentPDFAppendText(dst []byte, s string) []byte {
	if len(s) == 0 {
		return dst
	}
	if len(dst) > 0 {
		dst = append(dst, ' ')
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x80 {
			dst = append(dst, c)
			continue
		}
		dst = append(dst, 0xC0|(c>>6), 0x80|(c&0x3F))
	}
	return dst
}

// contentPDFDictLength parses a direct "/Length N" in a stream dictionary.
// Indirect lengths ("/Length 7 0 R") are rejected so the caller falls back to
// searching for endstream.
func contentPDFDictLength(dict []byte) (int, bool) {
	idx := bytes.Index(dict, []byte("/Length"))
	if idx < 0 {
		return 0, false
	}
	i := contentPDFSkipWS(dict, idx+len("/Length"))
	start := i
	for i < len(dict) && dict[i] >= '0' && dict[i] <= '9' {
		i++
	}
	if i == start {
		return 0, false
	}
	n, err := strconv.Atoi(string(dict[start:i]))
	if err != nil {
		return 0, false
	}
	if j := contentPDFSkipWS(dict, i); j < len(dict) && dict[j] >= '0' && dict[j] <= '9' {
		return 0, false
	}
	return n, true
}

func contentPDFLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func init() { contentRegisterExtractor(contentPDFExtractor{}) }
