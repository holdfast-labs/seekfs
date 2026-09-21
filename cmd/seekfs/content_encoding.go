package main

// Content text decoding, ported from tgrep's tgrep-core/src/encoding.rs.
//
// The content index and the content search must repair invalid bytes exactly
// the same way, or a pattern containing U+FFFD would select no candidate
// postings and the indexed search would miss a match the brute-force path
// reports. The fixup table maps an offset in the repaired text back to the
// source bytes, which is what lets search report real line/column positions on
// a file that is not valid UTF-8.

import (
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
)

// contentEncodingMode is how raw content bytes are turned into searchable text.
type contentEncodingMode struct {
	// auto sniffs a UTF-8/UTF-16 BOM, otherwise treats the bytes as UTF-8.
	auto bool
	// legacy makes auto's non-BOM fallback decode invalid UTF-8 as a legacy
	// single-byte encoding (Windows-1252, then Latin-1) rather than repairing
	// it to U+FFFD. The label parser's "auto" sets this; the zero mode keeps
	// the lossy-repair behavior the decoder primitives and their tests expect.
	legacy bool
	// none disables sniffing entirely: raw bytes are kept as-is (invalid bytes
	// are still repaired by the lossy pass, matching the index).
	none bool
	// explicit is a named WHATWG encoding; a BOM still wins over it.
	explicit encoding.Encoding
	// label is the user-facing name, for diagnostics.
	label string
}

// contentAutoEncoding is the default policy: BOM sniff, exact UTF-8, and a
// legacy single-byte fallback for non-UTF-8 bytes (PB6). This is what an absent
// -encoding override means for every extractor.
var contentAutoEncoding = contentEncodingMode{auto: true, legacy: true, label: "auto"}

const contentReplacementChar = '\uFFFD'

// contentReplacementLen is the UTF-8 byte length of U+FFFD.
var contentReplacementLen = utf8.RuneLen(contentReplacementChar)

// parseContentEncoding parses a -E/--encoding style label. Accepts "auto",
// "none", or any WHATWG label (utf-16le, sjis, latin1, ...).
func parseContentEncoding(label string) (contentEncodingMode, error) {
	label = strings.TrimSpace(label)
	switch strings.ToLower(label) {
	case "", "auto":
		return contentAutoEncoding, nil
	case "none":
		return contentEncodingMode{none: true, label: "none"}, nil
	}
	enc, err := htmlindex.Get(label)
	if err != nil {
		return contentEncodingMode{}, fmt.Errorf("unsupported encoding: %s", label)
	}
	return contentEncodingMode{explicit: enc, label: label}, nil
}

// sniffContentBOM returns the BOM-detected encoding and its byte length.
// Recognizes UTF-8, UTF-16LE and UTF-16BE, matching encoding_rs::for_bom.
func sniffContentBOM(b []byte) (enc string, n int) {
	switch {
	case len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF:
		return "utf-8", 3
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		return "utf-16le", 2
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		return "utf-16be", 2
	}
	return "", 0
}

// contentBorrowsWholeInput reports whether decoding would hand back the entire
// input untouched (no BOM trim, no transcode, no lossy repair). This is the
// precondition for searching bytes straight out of a memory map: invalid UTF-8
// is repaired, so those bytes are not what a search matches against.
func contentBorrowsWholeInput(b []byte, mode contentEncodingMode) bool {
	if _, n := sniffContentBOM(b); n > 0 {
		return false
	}
	if mode.explicit != nil && mode.explicit != encoding.Nop {
		return false
	}
	return utf8.Valid(b)
}

// contentDecodeWithFixups decodes bytes and records where lossy repair
// happened. Transcoded (UTF-16 or explicit-encoding) output is always valid
// UTF-8 and its offsets bear no relation to the source bytes, so no fixups are
// recorded for it.
func contentDecodeWithFixups(b []byte, mode contentEncodingMode) (string, contentLossyFixups) {
	if mode.none {
		return contentRepairUTF8(b, true)
	}
	if enc, n := sniffContentBOM(b); n > 0 {
		body := b[n:]
		if enc == "utf-8" {
			return contentRepairUTF8(body, true)
		}
		return contentTranscodeUTF16(enc, body), contentLossyFixups{}
	}
	if mode.explicit != nil && mode.explicit != encoding.Nop {
		return contentTranscode(mode.explicit, b), contentLossyFixups{}
	}
	if mode.legacy {
		return contentDecodeLegacy(b, false), contentLossyFixups{}
	}
	return contentRepairUTF8(b, true)
}

// contentDecode returns only the decoded text.
func contentDecode(b []byte, mode contentEncodingMode) string {
	text, _ := contentDecodeWithFixups(b, mode)
	return text
}

// contentDecodeForIndex repairs bytes the same way a search will, so the index
// holds exactly the bytes a later search matches against. truncated is the
// extractor's real signal that b was cut at the raw-size cap; only then may an
// incomplete UTF-8 tail be treated as a truncation artifact rather than as
// genuinely invalid legacy bytes.
func contentDecodeForIndex(b []byte, mode contentEncodingMode, truncated bool) string {
	if mode.none {
		return contentRepairString(b, false)
	}
	if enc, n := sniffContentBOM(b); n > 0 {
		body := b[n:]
		if enc == "utf-8" {
			return contentRepairString(body, false)
		}
		return contentTranscodeUTF16(enc, body)
	}
	if mode.explicit != nil && mode.explicit != encoding.Nop {
		return contentTranscode(mode.explicit, b)
	}
	if mode.legacy {
		return contentDecodeLegacy(b, truncated)
	}
	return contentRepairString(b, false)
}

func contentTranscodeUTF16(enc string, body []byte) string {
	if len(body)%2 != 0 {
		body = body[:len(body)-1]
	}
	units := make([]uint16, len(body)/2)
	for i := range units {
		if enc == "utf-16le" {
			units[i] = uint16(body[2*i]) | uint16(body[2*i+1])<<8
		} else {
			units[i] = uint16(body[2*i])<<8 | uint16(body[2*i+1])
		}
	}
	return string(utf16.Decode(units))
}

func contentTranscode(enc encoding.Encoding, b []byte) string {
	dec := enc.NewDecoder()
	out, err := dec.Bytes(b)
	if err != nil {
		// Decoders substitute U+FFFD rather than erroring in normal use; if one
		// does fail, repair the raw bytes so the search still runs.
		return contentRepairString(b, false)
	}
	return string(out)
}

// contentDecodeLegacy decodes the default auto policy's non-UTF-8 bytes as a
// legacy single-byte encoding, without giving up the UTF-8 around them.
//
// truncated is the caller's real signal that b was cut at the raw-size cap (the
// extractor's size > maxRaw), never inferred from the byte shape. Only when it
// is set -- and b is otherwise a UTF-8 prefix whose one invalid rune is an
// incomplete EOF tail -- is the whole buffer decoded as UTF-8 with that tail
// repaired, because the apparent invalidity is an artifact of the cut. Every
// other invalid buffer is decoded hybridly: every valid UTF-8 span is kept
// byte-exact and only the invalid spans are legacy-decoded. That keeps a genuine
// Latin-1/CP1252 file ending in "caf\xe9" searchable as "café" (no cut, so no
// repair) and a mostly-UTF-8 file with one stray byte searchable (its "café"
// stays "café") while recovering the stray span, instead of re-decoding the
// entire buffer as CP1252 and mojibaking it.
//
// Each invalid span decodes as Windows-1252 (the WHATWG/browser fallback),
// except a span whose CP1252 decode yields U+FFFD -- one of CP1252's five
// undefined bytes, 0x81/0x8D/0x8F/0x90/0x9D, which Go's charmap maps to
// U+FFFD. Such a span is decoded as Latin-1 (ISO-8859-1) instead, which maps
// every byte, so no source byte is silently dropped.
func contentDecodeLegacy(b []byte, truncated bool) string {
	if utf8.Valid(b) {
		return string(b)
	}
	if truncated && contentIsUTF8TruncatedAtEOF(b) {
		return contentRepairString(b, false)
	}
	var sb strings.Builder
	sb.Grow(len(b))
	rest := b
	for len(rest) > 0 {
		r, size := utf8.DecodeRune(rest)
		if r == utf8.RuneError && size == 1 {
			n := contentInvalidSeqLen(rest)
			if n < 1 {
				n = 1
			}
			if n > len(rest) {
				n = len(rest)
			}
			sb.WriteString(contentDecodeLegacySpan(rest[:n]))
			rest = rest[n:]
			continue
		}
		sb.Write(rest[:size])
		rest = rest[size:]
	}
	return sb.String()
}

// contentDecodeLegacySpan decodes one maximal invalid UTF-8 span with the
// legacy fallback: Windows-1252, or Latin-1 when CP1252 leaves U+FFFD.
func contentDecodeLegacySpan(span []byte) string {
	if s := contentTranscode(charmap.Windows1252, span); !strings.ContainsRune(s, contentReplacementChar) {
		return s
	}
	return contentTranscode(charmap.ISO8859_1, span)
}

// contentIsUTF8TruncatedAtEOF reports whether the only rune-level problem in b
// is a single multibyte sequence cut short at EOF. It is consulted only when the
// caller's real truncation flag is already set; the flag alone is not enough,
// because an over-cap buffer may also carry an earlier genuinely invalid byte,
// and then the hybrid path must recover that byte instead of repairing it away.
func contentIsUTF8TruncatedAtEOF(b []byte) bool {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			return contentTruncatedTailAtEOF(b[i:])
		}
		i += size
	}
	return false
}

// contentTruncatedTailAtEOF reports whether b is a valid UTF-8 lead byte with
// only continuation bytes after it, too few to complete the sequence (so it
// was cut at EOF rather than being genuinely malformed).
func contentTruncatedTailAtEOF(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	var need int
	switch lead := b[0]; {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 2
	case lead >= 0xE0 && lead <= 0xEF:
		need = 3
	case lead >= 0xF0 && lead <= 0xF4:
		need = 4
	default:
		return false
	}
	if len(b) >= need {
		return false
	}
	for i := 1; i < len(b); i++ {
		if b[i]&0xC0 != 0x80 {
			return false
		}
	}
	return true
}

// contentLossyFixups maps an offset in the repaired text back to the source.
// shifts holds "(offset of the replacement in the decoded text, total bytes
// gained through it)", ascending by offset.
type contentLossyFixups struct {
	shifts []contentShift
}

type contentShift struct {
	at     int
	gained int
}

func (f contentLossyFixups) isEmpty() bool { return len(f.shifts) == 0 }

// exact reports whether decoded lies on a replacement boundary or outside every
// replacement, so toSourceOffset is a faithful inverse there. An offset inside
// a replacement can only be clamped, so callers that need real source bytes
// must fall back for that region.
func (f contentLossyFixups) exact(decoded int) bool {
	for _, s := range f.shifts {
		if decoded > s.at && decoded < s.at+contentReplacementLen {
			return false
		}
	}
	return true
}

// toSourceOffset maps an offset in the decoded text to the source bytes. An
// offset may land inside a replacement rather than on its boundary; such an
// offset is clamped to the replacement's source position.
func (f contentLossyFixups) toSourceOffset(decoded int) int {
	if len(f.shifts) == 0 {
		return decoded
	}
	// partition point: first index with at >= decoded
	i := 0
	for i < len(f.shifts) && f.shifts[i].at < decoded {
		i++
	}
	if i == 0 {
		return decoded
	}
	at := f.shifts[i-1].at
	gained := f.shifts[i-1].gained
	gainedBefore := 0
	if i >= 2 {
		gainedBefore = f.shifts[i-2].gained
	}
	if decoded < at+contentReplacementLen {
		return at - gainedBefore
	}
	return decoded - gained
}

// contentRepairUTF8 repairs invalid UTF-8, recording fixups when record is true.
func contentRepairUTF8(b []byte, record bool) (string, contentLossyFixups) {
	if utf8.Valid(b) {
		return string(b), contentLossyFixups{}
	}
	var sb strings.Builder
	sb.Grow(len(b))
	var shifts []contentShift
	gained := 0
	rest := b
	for len(rest) > 0 {
		r, size := utf8.DecodeRune(rest)
		if r == utf8.RuneError && size == 1 {
			// One replacement per maximal invalid subsequence, matching Rust's
			// from_utf8_lossy rather than one per invalid byte.
			replaced := contentInvalidSeqLen(rest)
			if replaced < 1 {
				replaced = 1
			}
			if replaced > len(rest) {
				replaced = len(rest)
			}
			gain := contentReplacementLen - replaced
			if gain > 0 {
				gained += gain
			}
			if record {
				shifts = append(shifts, contentShift{at: sb.Len(), gained: gained})
			}
			sb.WriteRune(contentReplacementChar)
			rest = rest[replaced:]
			continue
		}
		sb.Write(rest[:size])
		rest = rest[size:]
	}
	return sb.String(), contentLossyFixups{shifts: shifts}
}

// contentInvalidSeqLen returns how many bytes to consume for an invalid UTF-8
// sequence starting at b[0], matching Rust's Utf8Error::error_len: the maximal
// valid prefix of the attempted sequence, or the whole remaining input when it
// is truncated at EOF.
func contentInvalidSeqLen(b []byte) int {
	lead := b[0]
	var need int
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 2
	case lead >= 0xE0 && lead <= 0xEF:
		need = 3
	case lead >= 0xF0 && lead <= 0xF4:
		need = 4
	default:
		return 1
	}
	for i := 1; i < need; i++ {
		if i >= len(b) {
			return len(b) // truncated: consume the rest
		}
		if b[i]&0xC0 != 0x80 {
			return i // valid prefix length before the bad continuation
		}
	}
	// All continuation bytes were present but the scalar is still invalid
	// (overlong, surrogate, or above U+10FFFF): the minimal subsequence.
	return 1
}

// contentRepairString is the indexing variant (no fixup allocation).
func contentRepairString(b []byte, record bool) string {
	s, _ := contentRepairUTF8(b, record)
	return s
}
