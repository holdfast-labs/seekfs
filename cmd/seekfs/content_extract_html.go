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
	"mime"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding"
)

// contentHTMLMaxToken caps one token's buffered bytes. Without it a single
// unterminated <script>, comment, or giant text run would buffer the whole
// document; the tokenizer then reports ErrBufferExceeded and extraction stops
// with what was already emitted. Well under maxRaw (default 32 MiB).
const contentHTMLMaxToken = 1 << 20

// contentHTMLCharsetPeek bounds the head inspected for a <meta> charset. The
// WHATWG prescan reads at most the first 1024 bytes; no more is decoded.
const contentHTMLCharsetPeek = 1024

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

// contentHTMLDecodeMode resolves the encoding for an HTML document. The shared
// policy has precedence: an explicit -encoding override, a BOM, or the shared
// BOM-less UTF-16 sniff all win. Only when none of those applies does a
// <meta charset> / <meta http-equiv> declaration take effect, applied through
// the decoder's explicit path so the stored text stays index/search-consistent.
// Nothing declared (or an unusable label) leaves the shared auto decoder's
// repair/CP1252 behavior untouched.
func contentHTMLDecodeMode(raw []byte, mode contentEncodingMode) contentEncodingMode {
	if mode.none || mode.explicit != nil || contentHasBOM(raw) {
		return mode
	}
	if mode.auto && contentSniffUTF16BOMless(raw) != "" {
		return mode
	}
	enc, ok := contentHTMLMetaCharsetEncoding(raw)
	if !ok {
		return mode
	}
	return contentEncodingMode{explicit: enc, label: "html-meta"}
}

// contentHTMLMetaCharsetEncoding returns the encoding declared by a <meta> in
// the bounded document head, if the declaration is present and resolves to a
// usable non-UTF-8 WHATWG label. It mirrors the WHATWG prescan that
// charset.DetermineEncoding runs, but only to tell a real declaration apart
// from DetermineEncoding's no-declaration windows-1252 fallback, which must not
// override the shared auto decoder's hybrid UTF-8/legacy recovery.
func contentHTMLMetaCharsetEncoding(head []byte) (encoding.Encoding, bool) {
	label, ok := contentHTMLMetaCharsetLabel(head)
	if !ok {
		return nil, false
	}
	enc, name := charset.Lookup(label)
	// utf-8 needs no transcode (and would bypass the shared repair); utf-16
	// labels are a contradiction in an ASCII-parsed meta and are left to the
	// shared BOM/BOM-less handling.
	if enc == nil || name == "" || name == "utf-8" || strings.HasPrefix(name, "utf-16") {
		return nil, false
	}
	return enc, true
}

// contentHTMLMetaCharsetLabel scans the first contentHTMLCharsetPeek bytes for a
// <meta> declaring a charset, in either the HTML5 (<meta charset=...>) or the
// legacy (<meta http-equiv=Content-Type content="...; charset=...">) form.
func contentHTMLMetaCharsetLabel(head []byte) (string, bool) {
	if len(head) > contentHTMLCharsetPeek {
		head = head[:contentHTMLCharsetPeek]
	}
	z := html.NewTokenizer(bytes.NewReader(head))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return "", false
		case html.StartTagToken, html.SelfClosingTagToken:
			tag, hasAttr := z.TagName()
			if !bytes.EqualFold(tag, []byte("meta")) {
				continue
			}
			var cs, content string
			hasPragma := false
			for hasAttr {
				var key, val []byte
				key, val, hasAttr = z.TagAttr()
				switch {
				case bytes.EqualFold(key, []byte("charset")):
					if cs == "" {
						cs = string(val)
					}
				case bytes.EqualFold(key, []byte("http-equiv")):
					if strings.EqualFold(string(val), "content-type") {
						hasPragma = true
					}
				case bytes.EqualFold(key, []byte("content")):
					if content == "" {
						content = string(val)
					}
				}
			}
			if cs != "" {
				return strings.TrimSpace(cs), true
			}
			if hasPragma && content != "" {
				if _, params, err := mime.ParseMediaType(content); err == nil {
					if label := strings.TrimSpace(params["charset"]); label != "" {
						return label, true
					}
				}
			}
		}
	}
}

func (contentHTMLExtractor) Extract(ctx context.Context, r io.ReaderAt, size int64) (contentExtractResult, error) {
	s := contentExtractSettingsFromContext(ctx)
	raw, err := contentReadBounded(r, size, int(s.maxRaw))
	if err != nil {
		return contentExtractResult{}, err
	}
	truncated := size > s.maxRaw
	// Decode first: entities then see UTF-8, and a <meta charset>-less legacy
	// document is repaired by the shared decoder rather than double-decoded. A
	// declared <meta> charset is honored when the shared policy has no stronger
	// signal (BOM / explicit override / BOM-less UTF-16).
	decoded := contentDecodeForIndex(raw, contentHTMLDecodeMode(raw, s.encoding), truncated)
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
