package main

// The Outlook .msg extractor (WP11). A .msg is an OLE2/CFB compound file whose
// searchable text lives in named MAPI property streams
// (`__substg1.0_<propid><type>`). This extractor reads only the handful of
// properties that carry user-visible text, in a fixed preference order, and
// never touches attachments.
//
// Bounds: the compound file is read through an io.SectionReader capped at the
// extraction policy's maxRaw (an over-cap file is skipped), every stream read is
// clamped to the text cap, LZFu output is capped at maxText, ctx is checked
// between properties, and the assembled text is truncated at maxText with
// Truncated set.

import (
	"bytes"
	"context"
	"io"
	"strings"
)

// MAPI property ids and types this extractor reads. `propid<<16 | type` is the
// key into the compound file's property index.
const (
	contentMSGPRSubject       = 0x0037
	contentMSGPRSenderName    = 0x0C1A
	contentMSGPRSenderEmail   = 0x0C1F
	contentMSGPRDisplayTo     = 0x0E04
	contentMSGPRDisplayCc     = 0x0E03
	contentMSGPRBody          = 0x1000
	contentMSGPRHTML          = 0x1013
	contentMSGPRRTFCompressed = 0x1009

	contentMSGTypeString  = 0x001F // UTF-16LE
	contentMSGTypeString8 = 0x001E // 8-bit ANSI
	contentMSGTypeBinary  = 0x0102
)

// contentMSGHeaderLimit bounds a header property (subject/from/to/cc) read. These
// fields are short; the cap keeps a crafted multi-megabyte "subject" from being
// materialised just to be discarded.
const contentMSGHeaderLimit = 1 << 20

type contentMSGLExtractor struct{}

func (contentMSGLExtractor) Name() string    { return "msg" }
func (contentMSGLExtractor) Version() uint16 { return 1 }
func (contentMSGLExtractor) Class() uint16   { return contentClassMSG }

func (contentMSGLExtractor) Extensions() []string { return []string{".msg"} }

// Sniff declines. A bare CFB signature cannot be told apart from a legacy
// .doc/.xls/.ppt (not supported here), so claiming an extensionless compound
// file would route a legacy document into an extractor that finds no MAPI
// properties. The .msg extension is the claim.
func (contentMSGLExtractor) Sniff([]byte) bool { return false }

func (contentMSGLExtractor) Extract(ctx context.Context, r io.ReaderAt, size int64) (res contentExtractResult, err error) {
	// Defensive: the coordinator recovers from a panic too, but a directly
	// called extractor must not let a malformed compound file crash the process.
	defer func() {
		if recover() != nil {
			res = contentExtractResult{Skipped: true, Reason: "malformed message", Class: contentClassMSG}
			err = nil
		}
	}()

	s := contentExtractSettingsFromContext(ctx)
	// A CFB's directory can sit anywhere and its chains cannot be prefixed, so
	// an over-cap compound file is skipped with a visible reason rather than
	// parsed from a bounded prefix.
	if size > s.maxRaw {
		return contentExtractResult{Skipped: true, Reason: "raw size over cap", Class: contentClassMSG}, nil
	}
	if size <= 0 {
		return contentExtractResult{Skipped: true, Reason: "empty file", Class: contentClassMSG}, nil
	}
	cfb, cerr := newContentCFBReader(ctx, io.NewSectionReader(r, 0, s.maxRaw), size)
	if cerr != nil {
		return contentExtractResult{Skipped: true, Reason: "malformed compound file", Class: contentClassMSG}, nil
	}

	w := &contentEMLWalker{ctx: ctx, maxText: s.maxText}
	truncated := false

	// Headers first: sender/subject/recipients are what a user searches by.
	w.appendText(contentMSGPropertyString(cfb, contentMSGPRSubject, s))
	w.appendText(contentMSGPropertyString(cfb, contentMSGPRSenderName, s))
	w.appendText(contentMSGPropertyString(cfb, contentMSGPRSenderEmail, s))
	w.appendText(contentMSGPropertyString(cfb, contentMSGPRDisplayTo, s))
	w.appendText(contentMSGPropertyString(cfb, contentMSGPRDisplayCc, s))

	if err := ctx.Err(); err != nil {
		return contentExtractResult{}, err
	}

	// Body preference: PR_BODY (plain), then PR_HTML, then PR_RTF_COMPRESSED.
	body, bodyTruncated, err := contentMSGBody(ctx, cfb, s)
	if err != nil {
		return contentExtractResult{}, err
	}
	truncated = truncated || bodyTruncated
	w.appendText(body)
	truncated = truncated || w.truncated

	if w.out.Len() == 0 {
		return contentExtractResult{Class: contentClassMSG, Truncated: truncated}, nil
	}
	text := truncateUTF8(w.out.String(), int(s.maxText))
	res = contentExtractResult{Text: []byte(text), Class: contentClassMSG, Truncated: truncated}
	if truncated {
		res.Reason = "indexed bounded prefix up to policy cap"
	}
	return res, nil
}

// contentMSGPropertyString reads a string property (UTF-16LE preferred, then
// ANSI), decoding it through the shared decoder. A missing property is "".
func contentMSGPropertyString(cfb *contentCFBReader, id uint32, s contentExtractSettings) string {
	limit := s.maxText
	if limit > contentMSGHeaderLimit {
		limit = contentMSGHeaderLimit
	}
	if data, _, ok := contentMSGProperty(cfb, id, contentMSGTypeString, limit); ok {
		return strings.TrimRight(contentTranscodeUTF16("utf-16le", data), "\x00")
	}
	if data, _, ok := contentMSGProperty(cfb, id, contentMSGTypeString8, limit); ok {
		return contentDecodeForIndex(data, s.encoding, false)
	}
	return ""
}

// contentMSGProperty reads one property stream at most limit bytes. ok reports
// that the stream exists; complete reports the whole declared stream was read (a
// stream over limit yields a bounded prefix with complete=false).
func contentMSGProperty(cfb *contentCFBReader, id, typ uint32, limit int64) (data []byte, complete, ok bool) {
	idx, ok := cfb.propertyStream(id, typ)
	if !ok {
		return nil, false, false
	}
	data, complete = cfb.readStreamBounded(idx, limit)
	return data, complete, true
}

// contentMSGBody returns the searchable body text and whether it was cut at the
// text cap. PR_BODY wins; PR_HTML is stripped via the shared HTML walk; a
// compressed RTF body is LZFu-inflated and run through the RTF extractor's walk.
func contentMSGBody(ctx context.Context, cfb *contentCFBReader, s contentExtractSettings) (string, bool, error) {
	// PR_BODY: UTF-16LE, else ANSI.
	if data, complete, ok := contentMSGProperty(cfb, contentMSGPRBody, contentMSGTypeString, s.maxText); ok {
		if text := strings.TrimRight(contentTranscodeUTF16("utf-16le", data), "\x00"); text != "" {
			return text, !complete, nil
		}
	}
	if data, complete, ok := contentMSGProperty(cfb, contentMSGPRBody, contentMSGTypeString8, s.maxText); ok {
		if text := contentDecodeForIndex(data, s.encoding, false); text != "" {
			return text, !complete, nil
		}
	}

	// PR_HTML: markup stored as binary, or as string8 by some writers. Strip it
	// with the shared HTML walk, selecting the decoder through the same
	// <meta charset>-aware mode the HTML extractor uses, so a declared charset
	// is honored here too rather than mojibaking through the auto decoder.
	for _, typ := range [...]uint32{contentMSGTypeBinary, contentMSGTypeString8} {
		data, complete, ok := contentMSGProperty(cfb, contentMSGPRHTML, typ, s.maxText)
		if !ok || len(data) == 0 {
			continue
		}
		decoded := contentDecodeForIndex(data, contentHTMLDecodeMode(data, s.encoding), false)
		text, htmlTruncated, err := contentHTMLText(ctx, decoded, s.maxText)
		if err != nil {
			return "", false, err
		}
		if text != "" {
			return text, !complete || htmlTruncated, nil
		}
	}

	// PR_RTF_COMPRESSED: LZFu-inflate, then reuse the RTF extractor's walk.
	if data, complete, ok := contentMSGProperty(cfb, contentMSGPRRTFCompressed, contentMSGTypeBinary, s.maxText); ok && len(data) > 0 {
		raw, lzTruncated, err := contentLZFuDecompress(ctx, data, s.maxText)
		if err == nil && len(raw) > 0 {
			rtfRes, rerr := contentRTFExtractor{}.Extract(ctx, bytes.NewReader(raw), int64(len(raw)))
			if rerr == nil {
				return string(rtfRes.Text), !complete || lzTruncated || rtfRes.Truncated, nil
			}
		}
	}
	return "", false, nil
}

func init() { contentRegisterExtractor(contentMSGLExtractor{}) }
