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
	"golang.org/x/text/encoding/htmlindex"
)

// contentEncodingMode is how raw content bytes are turned into searchable text.
type contentEncodingMode struct {
	// auto sniffs a UTF-8/UTF-16 BOM, otherwise treats the bytes as UTF-8.
	auto bool
	// none disables sniffing entirely: raw bytes are kept as-is (invalid bytes
	// are still repaired by the lossy pass, matching the index).
	none bool
	// explicit is a named WHATWG encoding; a BOM still wins over it.
	explicit encoding.Encoding
	// label is the user-facing name, for diagnostics.
	label string
}

const contentReplacementChar = '\uFFFD'

// contentReplacementLen is the UTF-8 byte length of U+FFFD.
var contentReplacementLen = utf8.RuneLen(contentReplacementChar)

// parseContentEncoding parses a -E/--encoding style label. Accepts "auto",
// "none", or any WHATWG label (utf-16le, sjis, latin1, ...).
func parseContentEncoding(label string) (contentEncodingMode, error) {
	label = strings.TrimSpace(label)
	switch strings.ToLower(label) {
	case "", "auto":
		return contentEncodingMode{auto: true, label: "auto"}, nil
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
	return contentRepairUTF8(b, true)
}

// contentDecode returns only the decoded text.
func contentDecode(b []byte, mode contentEncodingMode) string {
	text, _ := contentDecodeWithFixups(b, mode)
	return text
}

// contentDecodeForIndex repairs bytes the same way a search will, so the index
// holds exactly the bytes a later search matches against.
func contentDecodeForIndex(b []byte, mode contentEncodingMode) string {
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
