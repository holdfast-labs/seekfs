package main

// The HTML extractor: the first rich format, and the one that stops the service
// from indexing markup (tags, entities, script/style) as raw text.
//
// It walks golang.org/x/net/html's *tokenizer*, not its DOM parser. The DOM is
// O(document) memory and html.Parse recurses per nesting level, so a deeply
// nested document can exhaust the stack; the tokenizer is O(current token) and
// bounded by SetMaxBuf. Entity decoding comes free with the tokenizer's Text()
// on TextToken. The extension match wins over the raw-text fallback, so
// `.html/.htm/.xhtml` are no longer indexed as markup.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// contentHTMLMaxToken caps one token's buffered bytes. Without it a single
// unterminated <script>, comment, or giant text run would buffer the whole
// document; the tokenizer then reports ErrBufferExceeded and extraction stops
// with what was already emitted. Well under maxRaw (default 32 MiB).
const contentHTMLMaxToken = 1 << 20

type contentHTMLExtractor struct{}

func (contentHTMLExtractor) Name() string    { return "html" }
func (contentHTMLExtractor) Version() uint16 { return 1 }
func (contentHTMLExtractor) Class() uint16   { return contentClassHTML }

func (contentHTMLExtractor) Extensions() []string {
	return []string{".html", ".htm", ".xhtml"}
}

// Sniff claims an extensionless file that opens with an HTML doctype or <html>
// tag. XML/SVG (which open with <?xml) are deliberately left to raw text.
func (contentHTMLExtractor) Sniff(head []byte) bool {
	h := bytes.TrimLeft(head, " \t\r\n\f")
	h = bytes.TrimPrefix(h, []byte{0xEF, 0xBB, 0xBF})
	if len(h) == 0 {
		return false
	}
	h = bytes.ToLower(h)
	return bytes.HasPrefix(h, []byte("<!doctype html")) || bytes.HasPrefix(h, []byte("<html"))
}

// contentHTMLRawTextTags are elements whose content is not readable page text
// and is excluded. The tokenizer does not distinguish raw-text content, so the
// walk tracks the open element itself.
var contentHTMLRawTextTags = map[string]bool{
	"script": true, "style": true, "textarea": true,
}

// contentHTMLBlockTags separate words at element boundaries, so text either
// side of a block element does not glue into one token.
var contentHTMLBlockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"br": true, "caption": true, "dd": true, "div": true, "dl": true,
	"dt": true, "fieldset": true, "figcaption": true, "figure": true,
	"footer": true, "form": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "header": true, "hr": true,
	"li": true, "main": true, "nav": true, "ol": true, "p": true,
	"pre": true, "section": true, "table": true, "tbody": true,
	"td": true, "tfoot": true, "th": true, "thead": true, "tr": true,
	"ul": true,
}

func (contentHTMLExtractor) Extract(ctx context.Context, r io.ReaderAt, size int64) (contentExtractResult, error) {
	s := contentExtractSettingsFromContext(ctx)
	raw, err := contentReadBounded(r, size, int(s.maxRaw))
	if err != nil {
		return contentExtractResult{}, err
	}
	truncated := size > s.maxRaw
	// Decode first: entities then see UTF-8, and a <meta charset>-less legacy
	// document is repaired by the shared decoder rather than double-decoded.
	decoded := contentDecodeForIndex(raw, s.encoding, truncated)
	z := html.NewTokenizer(strings.NewReader(decoded))
	z.SetMaxBuf(contentHTMLMaxToken)

	var out bytes.Buffer
	inRaw := ""
walk:
	for {
		if err := ctx.Err(); err != nil {
			return contentExtractResult{}, err
		}
		switch z.Next() {
		case html.ErrorToken:
			// io.EOF is the normal end; ErrBufferExceeded (a pathological
			// token) or any other read error means a bounded prefix.
			if !errors.Is(z.Err(), io.EOF) {
				truncated = true
			}
			break walk
		case html.TextToken:
			// Skip raw-text content: the tokenizer hands it back as text.
			if inRaw == "" {
				contentHTMLAppendText(&out, z.Text())
			}
		case html.StartTagToken:
			tag := contentHTMLTagName(z)
			if contentHTMLRawTextTags[tag] {
				inRaw = tag
			}
			if contentHTMLBlockTags[tag] {
				contentHTMLSeparate(&out)
			}
		case html.EndTagToken:
			tag := contentHTMLTagName(z)
			if tag == inRaw {
				inRaw = ""
			}
			if contentHTMLBlockTags[tag] {
				contentHTMLSeparate(&out)
			}
		case html.SelfClosingTagToken:
			if contentHTMLBlockTags[contentHTMLTagName(z)] {
				contentHTMLSeparate(&out)
			}
		}
		if int64(out.Len()) >= s.maxText {
			truncated = true
			break walk
		}
	}

	if out.Len() == 0 {
		return contentExtractResult{Class: contentClassHTML, Truncated: truncated}, nil
	}
	text := truncateUTF8(out.String(), int(s.maxText))
	res := contentExtractResult{Text: []byte(text), Class: contentClassHTML, Truncated: truncated}
	if truncated {
		res.Reason = "indexed bounded prefix up to policy cap"
	}
	return res, nil
}

// contentHTMLTagName returns the lowercased name of the current tag token.
func contentHTMLTagName(z *html.Tokenizer) string {
	name, _ := z.TagName()
	return string(name)
}

// contentHTMLAppendText copies token text out of the tokenizer's reusable buffer
// and drops NUL bytes, which regular text tokens are not guaranteed to repair.
func contentHTMLAppendText(out *bytes.Buffer, text []byte) {
	for _, b := range text {
		if b != 0 {
			out.WriteByte(b)
		}
	}
}

// contentHTMLSeparate inserts a single space unless the output already ends in
// whitespace (or is empty), so block boundaries do not collapse to nothing or
// pile up spaces.
func contentHTMLSeparate(out *bytes.Buffer) {
	if out.Len() == 0 {
		return
	}
	switch out.Bytes()[out.Len()-1] {
	case ' ', '\t', '\r', '\n':
		return
	}
	out.WriteByte(' ')
}

func init() { contentRegisterExtractor(contentHTMLExtractor{}) }
