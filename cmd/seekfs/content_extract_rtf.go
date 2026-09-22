package main

// The RTF extractor: a hand-rolled, bounded scanner over the control-word
// syntax. It needs no dependency and, like the PDF spike, deliberately covers
// the common "born-in-Word, simple text" case rather than the full spec.
//
// The scanner is flat, not recursive: groups are a depth counter and a skipped
// region is tracked by the depth at which it started, so a deeply nested or
// unterminated document cannot exhaust the stack. It reads at most maxRaw,
// emits at most maxText, checks ctx periodically, and drops NULs.
//
// Destination groups that carry no visible text (\fonttbl, \colortbl, \pict,
// \info, \field, ... and any \* ignorable destination) are skipped whole.
// Control words are otherwise ignored, which is the spec's rule for unknown
// words; only the text-bearing ones are translated.

import (
	"bytes"
	"context"
	"io"
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
)

const (
	// contentRTFMaxGroupDepth bounds brace nesting so a pathological document
	// (millions of unmatched "{") stops early instead of scanning to the cap.
	contentRTFMaxGroupDepth = 4096
	// contentRTFMaxUC bounds \ucN's fallback skip count. Spec values are 0..2;
	// a larger one would swallow real text, so it is clamped.
	contentRTFMaxUC     = 32
	contentRTFDefaultCP = 1252
)

type contentRTFExtractor struct{}

func (contentRTFExtractor) Name() string    { return "rtf" }
func (contentRTFExtractor) Version() uint16 { return 1 }
func (contentRTFExtractor) Class() uint16   { return contentClassRTF }

func (contentRTFExtractor) Extensions() []string { return []string{".rtf"} }

// Sniff claims an extensionless file opening with the RTF header.
func (contentRTFExtractor) Sniff(head []byte) bool {
	h := bytes.TrimLeft(head, " \t\r\n\f")
	h = bytes.TrimPrefix(h, []byte{0xEF, 0xBB, 0xBF})
	return bytes.HasPrefix(h, []byte("{\\rtf"))
}

func (contentRTFExtractor) Extract(ctx context.Context, r io.ReaderAt, size int64) (contentExtractResult, error) {
	s := contentExtractSettingsFromContext(ctx)
	raw, err := contentReadBounded(r, size, int(s.maxRaw))
	if err != nil {
		return contentExtractResult{}, err
	}
	truncated := size > s.maxRaw
	// Decode with the shared policy first: RTF syntax is ASCII, so the control
	// words are untouched, and any stray 8-bit bytes are repaired the same way
	// a later search would repair them.
	doc := []byte(contentDecodeForIndex(raw, s.encoding, truncated))

	var out bytes.Buffer
	cp := contentRTFDefaultCP
	uc := 1
	fallback := 0
	depth := 0
	skipping := false
	skipDepth := 0

	for i := 0; i < len(doc); {
		if i&0x3FFF == 0 {
			if err := ctx.Err(); err != nil {
				return contentExtractResult{}, err
			}
		}
		if int64(out.Len()) >= s.maxText {
			truncated = true
			break
		}
		switch b := doc[i]; {
		case b == '{':
			depth++
			if depth > contentRTFMaxGroupDepth {
				truncated = true
				i = len(doc)
				continue
			}
			i++
		case b == '}':
			if skipping && depth <= skipDepth {
				skipping = false
				// A \uN inside the skipped region armed no fallback (see below),
				// but one armed just before the region must not leak past it.
				fallback = 0
			}
			if depth > 0 {
				depth--
			}
			i++
		case b == '\\':
			if i+1 >= len(doc) {
				i++
				continue
			}
			if !contentRTFLetter(doc[i+1]) {
				// \* marks an ignorable destination, which is a whole group.
				if doc[i+1] == '*' {
					if !skipping && depth > 0 {
						skipping = true
						skipDepth = depth
					}
					i += 2
					continue
				}
				i = contentRTFReadSymbol(doc, i, cp, &out, skipping, &fallback)
				continue
			}
			word, param, has, next := contentRTFReadWord(doc, i)
			i = next
			switch word {
			case "ansicpg":
				if has {
					cp = int(param)
				}
			case "uc":
				if has {
					uc = contentRTFClampUC(param)
				}
			case "u":
				if has {
					r := rune(param)
					if param < 0 {
						r = rune(param + 0x10000)
					}
					if r > 0 && !skipping {
						out.WriteRune(r)
						// Arm the fallback skip-count only for emitted text; the
						// skipped region writes nothing, so it consumes nothing.
						fallback = uc
					}
				}
			case "par", "line", "sect", "page":
				contentRTFWriteControl(&out, "\n", skipping)
			case "tab":
				contentRTFWriteControl(&out, "\t", skipping)
			case "emdash":
				contentRTFWriteControl(&out, "\u2014", skipping)
			case "endash":
				contentRTFWriteControl(&out, "\u2013", skipping)
			case "bullet":
				contentRTFWriteControl(&out, "\u2022", skipping)
			case "lquote":
				contentRTFWriteControl(&out, "\u2018", skipping)
			case "rquote":
				contentRTFWriteControl(&out, "\u2019", skipping)
			case "ldblquote":
				contentRTFWriteControl(&out, "\u201C", skipping)
			case "rdblquote":
				contentRTFWriteControl(&out, "\u201D", skipping)
			case "bin":
				// Binary payload: skip it so its braces/backslashes are not
				// parsed as markup.
				if has && param > 0 {
					n := int(param)
					if n > len(doc)-i {
						n = len(doc) - i
					}
					i += n
				}
			default:
				if !skipping && depth > 0 && contentRTFSkipWord(word) {
					skipping = true
					skipDepth = depth
				}
			}
		case b == 0 || b == '\r' || b == '\n':
			// NULs dropped; CR/LF in the RTF source are markup, not text.
			i++
		default:
			contentRTFWriteLiteral(&out, b, skipping, &fallback)
			i++
		}
	}

	if out.Len() == 0 {
		return contentExtractResult{Class: contentClassRTF, Truncated: truncated}, nil
	}
	// The byte-level scanner can split a multibyte rune (a control symbol consumes
	// only the lead byte, then the continuation bytes are emitted as literals), so
	// repair the buffer to valid UTF-8 before returning it.
	text := truncateUTF8(contentRepairString(out.Bytes(), false), int(s.maxText))
	res := contentExtractResult{Text: []byte(text), Class: contentClassRTF, Truncated: truncated}
	if truncated {
		res.Reason = "indexed bounded prefix up to policy cap"
	}
	return res, nil
}

// contentRTFReadWord reads a control word starting at s[i]=='\\': the letter
// run, an optional signed numeric parameter, and the one-space delimiter.
func contentRTFReadWord(s []byte, i int) (word string, param int64, hasParam bool, next int) {
	j := i + 1
	start := j
	for j < len(s) && contentRTFLetter(s[j]) {
		j++
	}
	word = string(s[start:j])
	if j < len(s) && (s[j] == '-' || (s[j] >= '0' && s[j] <= '9')) {
		neg := s[j] == '-'
		if neg {
			j++
		}
		var v int64
		nd := 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			if v < 1<<40 {
				v = v*10 + int64(s[j]-'0')
			}
			j++
			nd++
		}
		if nd > 0 {
			hasParam = true
			if neg {
				v = -v
			}
			param = v
		}
	}
	if j < len(s) && s[j] == ' ' {
		j++
	}
	return word, param, hasParam, j
}

// contentRTFReadSymbol handles the control symbol at s[i]=='\\' whose following
// byte is not a letter, returning the index just past it.
func contentRTFReadSymbol(s []byte, i int, cp int, out *bytes.Buffer, skipping bool, fallback *int) int {
	c := s[i+1]
	switch c {
	case '\'':
		if i+3 >= len(s) {
			return len(s)
		}
		hi, ok1 := contentRTFHex(s[i+2])
		lo, ok2 := contentRTFHex(s[i+3])
		if ok1 && ok2 {
			contentRTFWriteString(out, string(contentRTFDecodeByte(cp, hi<<4|lo)), skipping, fallback)
		}
		return i + 4
	case '~':
		contentRTFWriteControl(out, "\u00A0", skipping)
		return i + 2
	case '_':
		contentRTFWriteControl(out, "\u2011", skipping)
		return i + 2
	case '-':
		// Optional hyphen: dropped so words around a line break stay whole.
		return i + 2
	case '{', '}', '\\':
		contentRTFWriteLiteral(out, c, skipping, fallback)
		return i + 2
	case '\r', '\n':
		contentRTFWriteControl(out, "\n", skipping)
		return i + 2
	default:
		// \* is handled by the caller; every other symbol is undefined and
		// ignored.
		return i + 2
	}
}

// contentRTFWriteLiteral writes one source character, consuming a pending \uN
// fallback count. A NUL is always dropped.
func contentRTFWriteLiteral(out *bytes.Buffer, b byte, skipping bool, fallback *int) {
	if skipping || b == 0 {
		return
	}
	if *fallback > 0 {
		*fallback--
		return
	}
	out.WriteByte(b)
}

// contentRTFWriteString writes a decoded \'hh character (one fallback character
// however many UTF-8 bytes it is).
func contentRTFWriteString(out *bytes.Buffer, s string, skipping bool, fallback *int) {
	if skipping || s == "" {
		return
	}
	if *fallback > 0 {
		*fallback--
		return
	}
	out.WriteString(s)
}

// contentRTFWriteControl writes a control-word expansion. Control words are not
// fallback characters, so a pending \ucN count is not consumed.
func contentRTFWriteControl(out *bytes.Buffer, s string, skipping bool) {
	if skipping || s == "" {
		return
	}
	out.WriteString(s)
}

// contentRTFSkipWord reports whether a control word opens a destination whose
// body is not visible text.
func contentRTFSkipWord(word string) bool {
	switch word {
	case "fonttbl", "colortbl", "stylesheet", "info",
		"pict", "object", "field",
		"header", "headerl", "headerr", "headerf",
		"footer", "footerl", "footerr", "footerf",
		"footnote", "annotation",
		"themedata", "colorschememapping", "datastore", "latentstyles",
		"listtable", "listoverridetable", "rsidtbl", "generator", "filetbl":
		return true
	}
	return false
}

// contentRTFClampUC bounds a \ucN count.
func contentRTFClampUC(param int64) int {
	if param < 0 {
		return 0
	}
	if param > contentRTFMaxUC {
		return contentRTFMaxUC
	}
	return int(param)
}

// contentRTFDecodeByte decodes one \'hh byte in codepage cp. CP1252's five
// undefined bytes fall back to Latin-1 so no source byte is dropped, mirroring
// contentDecodeLegacySpan.
func contentRTFDecodeByte(cp int, b byte) []byte {
	if b < 0x80 {
		return []byte{b}
	}
	if enc := contentRTFCodepage(cp); enc != nil {
		if s := contentTranscode(enc, []byte{b}); !strings.ContainsRune(s, contentReplacementChar) {
			return []byte(s)
		}
	}
	return []byte(contentDecodeLegacy([]byte{b}, false))
}

// contentRTFCodepage maps the common single-byte ANSI code pages. Anything else
// returns nil and falls back to the shared Windows-1252/Latin-1 path.
func contentRTFCodepage(cp int) encoding.Encoding {
	switch cp {
	case 874:
		return charmap.Windows874
	case 1250:
		return charmap.Windows1250
	case 1251:
		return charmap.Windows1251
	case 1252:
		return charmap.Windows1252
	case 1253:
		return charmap.Windows1253
	case 1254:
		return charmap.Windows1254
	case 1255:
		return charmap.Windows1255
	case 1256:
		return charmap.Windows1256
	case 1257:
		return charmap.Windows1257
	case 1258:
		return charmap.Windows1258
	case 28591, 20127:
		return charmap.ISO8859_1
	}
	return nil
}

func contentRTFLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func contentRTFHex(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}

func init() { contentRegisterExtractor(contentRTFExtractor{}) }
