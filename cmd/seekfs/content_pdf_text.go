package main

// Content-stream text extraction: a bounded tokenizer that runs the page's
// content, tracks text positioning, and maps the bytes of Tj/TJ/'/" operands to
// Unicode through the active font. Word separation is heuristic (a positioning
// move or a strong TJ adjustment emits a space or newline) because glyph widths
// are not read.
//
// Font decoding: /ToUnicode CMaps (bfchar/bfrange) win when present; simple
// fonts otherwise fall back to WinAnsi/MacRoman/Standard encoding tables plus
// /Differences glyph names. Type0/CID fonts without a /ToUnicode map are dropped
// (documented non-goal), so no CJK CID text is fabricated.

import (
	"strings"
	"unicode/utf16"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
)

const (
	contentPDFWordGap       = 120.0
	contentPDFMaxCMapOps    = 1 << 16
	contentPDFMaxCMapEntry  = 1 << 16
	contentPDFMaxContentOps = 1 << 22
	contentPDFMaxFormDepth  = 8
	contentPDFMaxXObjects   = 256
)

type contentPDFFont struct {
	simple [256]rune
	cid    bool
	toUni  map[uint32]rune
}

// contentPDFTextWriter accumulates decoded text under maxText. Separators are
// deferred until the next non-empty write so a paragraph break at the cap does
// not trail whitespace.
type contentPDFTextWriter struct {
	out       []byte
	maxText   int
	have      bool
	sep       byte
	truncated bool
}

func newContentPDFTextWriter(maxText int64) *contentPDFTextWriter {
	if maxText < 0 {
		maxText = 0
	}
	if maxText > contentExtractHardMaxTextBytes {
		maxText = contentExtractHardMaxTextBytes
	}
	return &contentPDFTextWriter{maxText: int(maxText)}
}

func (w *contentPDFTextWriter) done() bool { return w.truncated }

func (w *contentPDFTextWriter) space() {
	if w.have && w.sep == 0 {
		w.sep = ' '
	}
}

func (w *contentPDFTextWriter) newline() {
	if w.have {
		w.sep = '\n'
	}
}

func (w *contentPDFTextWriter) writeString(s string) {
	if s == "" || w.truncated {
		return
	}
	if w.have && w.sep != 0 {
		b := []byte{w.sep}
		w.sep = 0
		w.appendBytes(b)
	}
	w.have = true
	w.appendBytes([]byte(s))
}

func (w *contentPDFTextWriter) appendBytes(b []byte) {
	if w.truncated || len(b) == 0 {
		return
	}
	remain := w.maxText - len(w.out)
	if remain <= 0 {
		w.truncated = true
		return
	}
	if len(b) > remain {
		b = b[:remain]
		w.truncated = true
	}
	w.out = append(w.out, b...)
	if len(w.out) >= w.maxText {
		w.truncated = true
	}
}

// extractPageText decodes every content stream of a page in order and appends
// its text.
func (d *contentPDFDoc) extractPageText(pg contentPDFPage, w *contentPDFTextWriter) error {
	fonts := d.pageFonts(pg.resources)
	xobjs := d.pageXObjects(pg.resources)
	for _, cv := range pg.contents {
		if w.done() {
			return nil
		}
		if err := d.ctx.Err(); err != nil {
			return err
		}
		s := d.resolve(cv)
		if s.kind != contentPDFStream {
			continue
		}
		data, _, err := contentPDFDecodeStream(d.ctx, d, s.dict, s.raw, d.maxRaw)
		if err != nil || len(data) == 0 {
			continue
		}
		d.contentStreamText(data, fonts, xobjs, w, 0)
	}
	return nil
}

// pageXObjects resolves a resources dict's /XObject map to its stream values by
// name. Form XObjects draw text through `Do`; image XObjects are ignored (no OCR).
func (d *contentPDFDoc) pageXObjects(resources contentPDFValue) map[string]contentPDFValue {
	out := map[string]contentPDFValue{}
	resources = d.resolve(resources)
	if resources.kind != contentPDFDict {
		return out
	}
	xv := d.resolve(resources.dict["XObject"])
	if xv.kind != contentPDFDict {
		return out
	}
	for name, ref := range xv.dict {
		if len(out) >= contentPDFMaxXObjects {
			break
		}
		if ov := d.resolve(ref); ov.kind == contentPDFStream {
			out[name] = ov
		}
	}
	return out
}

func (d *contentPDFDoc) contentStreamText(data []byte, fonts map[string]*contentPDFFont, xobjs map[string]contentPDFValue, w *contentPDFTextWriter, depth int) {
	l := &contentPDFLexer{buf: data, budget: contentPDFMaxValues}
	var stack []contentPDFValue
	var font *contentPDFFont
	var x, y, lastX, lastY float64
	havePos := false
	ops := 0
	for {
		if w.done() {
			return
		}
		ops++
		if ops > contentPDFMaxContentOps {
			return
		}
		op, val, isOp, ok := l.nextContent()
		if !ok {
			return
		}
		if !isOp {
			if len(stack) < 64 {
				stack = append(stack, val)
			} else {
				copy(stack, stack[1:])
				stack[len(stack)-1] = val
			}
			continue
		}
		switch op {
		case "Tf":
			if len(stack) >= 2 {
				if n, ok := contentPDFNameOf(stack[len(stack)-2]); ok {
					font = fonts[n]
				}
			}
		case "BT":
			x, y, havePos = 0, 0, false
		case "Td", "TD":
			if len(stack) >= 2 {
				tx, ty := contentPDFNumOf(stack[len(stack)-2]), contentPDFNumOf(stack[len(stack)-1])
				x += tx
				y += ty
				contentPDFMove(w, havePos, lastX, lastY, x, y)
				lastX, lastY, havePos = x, y, true
			}
		case "Tm":
			if len(stack) >= 6 {
				x, y = contentPDFNumOf(stack[len(stack)-2]), contentPDFNumOf(stack[len(stack)-1])
				contentPDFMove(w, havePos, lastX, lastY, x, y)
				lastX, lastY, havePos = x, y, true
			}
		case "T*":
			w.newline()
		case "Tj":
			if len(stack) >= 1 {
				d.contentPDFShow(stack[len(stack)-1], font, w)
			}
		case "'":
			w.newline()
			if len(stack) >= 1 {
				d.contentPDFShow(stack[len(stack)-1], font, w)
			}
		case `"`:
			w.newline()
			if len(stack) >= 1 {
				d.contentPDFShow(stack[len(stack)-1], font, w)
			}
		case "TJ":
			if len(stack) >= 1 {
				d.contentPDFShowArray(stack[len(stack)-1], font, w)
			}
		case "Do":
			// A Form XObject draws text through its own resources; recurse,
			// bounded by depth and the writer's text cap. Image XObjects are
			// skipped (no OCR).
			if depth < contentPDFMaxFormDepth && len(stack) >= 1 {
				if n, ok := contentPDFNameOf(stack[len(stack)-1]); ok {
					if ov, ok := xobjs[n]; ok {
						if frame, _, derr := contentPDFDecodeStream(d.ctx, d, ov.dict, ov.raw, d.maxRaw); derr == nil && len(frame) > 0 {
							subFonts := d.pageFonts(ov.dict["Resources"])
							subXobjs := d.pageXObjects(ov.dict["Resources"])
							d.contentStreamText(frame, subFonts, subXobjs, w, depth+1)
						}
					}
				}
			}
		}
		stack = stack[:0]
	}
}

func contentPDFMove(w *contentPDFTextWriter, havePos bool, lastX, lastY, x, y float64) {
	if !havePos {
		return
	}
	if y != lastY {
		w.newline()
	} else if x != lastX {
		w.space()
	}
}

func (d *contentPDFDoc) contentPDFShow(v contentPDFValue, font *contentPDFFont, w *contentPDFTextWriter) {
	if v.kind != contentPDFString {
		return
	}
	w.writeString(contentPDFDecodeString(font, v.str))
}

func (d *contentPDFDoc) contentPDFShowArray(v contentPDFValue, font *contentPDFFont, w *contentPDFTextWriter) {
	if v.kind != contentPDFArray {
		return
	}
	for _, e := range v.arr {
		switch e.kind {
		case contentPDFString:
			w.writeString(contentPDFDecodeString(font, e.str))
		case contentPDFInt, contentPDFReal:
			if contentPDFNumOf(e) <= -contentPDFWordGap {
				w.space()
			}
		}
	}
}

// contentPDFDecodeString maps a shown byte string through the active font. A
// missing font falls back to Latin-1, which keeps simple ASCII text searchable.
func contentPDFDecodeString(f *contentPDFFont, b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if f == nil {
		var sb strings.Builder
		sb.Grow(len(b))
		for _, c := range b {
			sb.WriteRune(rune(c))
		}
		return sb.String()
	}
	var sb strings.Builder
	sb.Grow(len(b))
	if f.cid {
		for i := 0; i+1 < len(b); i += 2 {
			code := uint32(b[i])<<8 | uint32(b[i+1])
			if r, ok := f.toUni[code]; ok && r != 0 {
				contentPDFAppendRune(&sb, r)
			}
		}
		return sb.String()
	}
	for _, c := range b {
		if r, ok := f.toUni[uint32(c)]; ok {
			if r != 0 {
				contentPDFAppendRune(&sb, r)
			}
			continue
		}
		if r := f.simple[c]; r != 0 {
			contentPDFAppendRune(&sb, r)
		}
	}
	return sb.String()
}

// contentPDFAppendRune expands a Unicode ligature presentation form to its
// ASCII letters, so a ToUnicode/Differences that maps a ligature to U+FB00–
// FB06 (very common) keeps the word searchable as plain text. (A producer that
// maps the ligature to a single base letter, e.g. `fl` → `f`, is lossy in the
// PDF itself and cannot be recovered here.)
func contentPDFAppendRune(sb *strings.Builder, r rune) {
	switch r {
	case 0xFB00:
		sb.WriteString("ff")
	case 0xFB01:
		sb.WriteString("fi")
	case 0xFB02:
		sb.WriteString("fl")
	case 0xFB03:
		sb.WriteString("ffi")
	case 0xFB04:
		sb.WriteString("ffl")
	case 0xFB05, 0xFB06:
		sb.WriteString("st")
	default:
		sb.WriteRune(r)
	}
}

// ----- fonts -----

func (d *contentPDFDoc) pageFonts(resources contentPDFValue) map[string]*contentPDFFont {
	out := map[string]*contentPDFFont{}
	resources = d.resolve(resources)
	if resources.kind != contentPDFDict {
		return out
	}
	fv := d.resolve(resources.dict["Font"])
	if fv.kind != contentPDFDict {
		return out
	}
	for name, ref := range fv.dict {
		if len(out) >= contentPDFMaxFonts {
			break
		}
		if err := d.ctx.Err(); err != nil {
			break
		}
		if f := d.buildFont(ref); f != nil {
			out[name] = f
		}
	}
	return out
}

func (d *contentPDFDoc) buildFont(v contentPDFValue) *contentPDFFont {
	v = d.resolve(v)
	if v.kind != contentPDFDict {
		return nil
	}
	f := &contentPDFFont{}
	sub, _ := contentPDFNameOf(d.resolve(v.dict["Subtype"]))
	f.cid = sub == "Type0"
	if tv, ok := v.dict["ToUnicode"]; ok {
		tv = d.resolve(tv)
		if tv.kind == contentPDFStream {
			if data, _, err := contentPDFDecodeStream(d.ctx, d, tv.dict, tv.raw, d.maxRaw); err == nil {
				f.toUni = contentPDFParseCMap(data)
			}
		}
	}
	if f.cid {
		// A Type0/CID font with no /ToUnicode map is dropped: the codes are CIDs,
		// not Latin-1, so emitting them as text would only add noise.
		return f
	}
	f.simple = contentPDFSimpleEncodingTable(d, v.dict)
	return f
}

// contentPDFSimpleEncodingTable resolves a simple font's base encoding and
// applies /Encoding/Differences glyph names on top.
func contentPDFSimpleEncodingTable(d *contentPDFDoc, font map[string]contentPDFValue) [256]rune {
	encName := ""
	var encDict map[string]contentPDFValue
	if ev, ok := font["Encoding"]; ok {
		ev = d.resolve(ev)
		if n, ok := contentPDFNameOf(ev); ok {
			encName = n
		} else if ev.kind == contentPDFDict {
			encDict = ev.dict
			if n, ok := contentPDFNameOf(d.resolve(ev.dict["BaseEncoding"])); ok {
				encName = n
			}
		}
	}
	table := contentPDFBaseEncoding(encName)
	if encDict != nil {
		dv := d.resolve(encDict["Differences"])
		if dv.kind == contentPDFArray {
			code := 0
			for _, e := range dv.arr {
				e = d.resolve(e)
				if n, ok := contentPDFIntOf(e); ok {
					code = int(n)
					continue
				}
				if n, ok := contentPDFNameOf(e); ok {
					if r, ok := contentPDFGlyphRune(n); ok && code >= 0 && code < 256 {
						table[code] = r
					}
					code++
				}
				if code > 256 {
					break
				}
			}
		}
	}
	return table
}

func contentPDFBaseEncoding(name string) [256]rune {
	var t [256]rune
	for i := 0; i < 256; i++ {
		t[i] = rune(i)
	}
	switch name {
	case "WinAnsiEncoding":
		return contentPDFCharmapTable(charmap.Windows1252, t)
	case "MacRomanEncoding":
		return contentPDFCharmapTable(charmap.Macintosh, t)
	}
	return t
}

func contentPDFCharmapTable(enc encoding.Encoding, fallback [256]rune) [256]rune {
	t := fallback
	for i := 0; i < 256; i++ {
		rs := []rune(contentTranscode(enc, []byte{byte(i)}))
		if len(rs) == 1 && rs[0] != contentReplacementChar {
			t[i] = rs[0]
		}
	}
	return t
}

// ----- ToUnicode CMaps -----

func contentPDFParseCMap(data []byte) map[uint32]rune {
	out := make(map[uint32]rune)
	l := &contentPDFLexer{buf: data, budget: contentPDFMaxValues}
	var operands []contentPDFValue
	for {
		op, val, isOp, ok := l.nextContent()
		if !ok {
			break
		}
		if !isOp {
			if len(operands) < contentPDFMaxCMapOps {
				operands = append(operands, val)
			}
			continue
		}
		switch op {
		case "beginbfchar":
			operands = operands[:0]
		case "endbfchar":
			contentPDFCMapBFChar(out, operands)
			operands = operands[:0]
		case "beginbfrange":
			operands = operands[:0]
		case "endbfrange":
			contentPDFCMapBFRange(out, operands)
			operands = operands[:0]
		case "endcmap":
			return out
		default:
			operands = operands[:0]
		}
	}
	return out
}

func contentPDFCMapBFChar(out map[uint32]rune, ops []contentPDFValue) {
	for i := 0; i+1 < len(ops); i += 2 {
		if len(out) >= contentPDFMaxCMapEntry {
			return
		}
		src, ok := contentPDFHexCode(ops[i])
		if !ok {
			continue
		}
		if rs, ok := contentPDFUTF16Runes(ops[i+1]); ok && len(rs) > 0 {
			out[src] = rs[0]
		}
	}
}

func contentPDFCMapBFRange(out map[uint32]rune, ops []contentPDFValue) {
	for i := 0; i+2 < len(ops); i += 3 {
		lo, ok1 := contentPDFHexCode(ops[i])
		hi, ok2 := contentPDFHexCode(ops[i+1])
		if !ok1 || !ok2 || hi < lo {
			continue
		}
		dst := ops[i+2]
		if dst.kind == contentPDFArray {
			for k, e := range dst.arr {
				if len(out) >= contentPDFMaxCMapEntry {
					return
				}
				code := lo + uint32(k)
				if code > hi {
					break
				}
				if rs, ok := contentPDFUTF16Runes(e); ok && len(rs) > 0 {
					out[code] = rs[0]
				}
			}
			continue
		}
		rs, ok := contentPDFUTF16Runes(dst)
		if !ok || len(rs) == 0 {
			continue
		}
		base := rs[0]
		for code := lo; code <= hi; code++ {
			if len(out) >= contentPDFMaxCMapEntry {
				return
			}
			out[code] = base + rune(code-lo)
			if code == ^uint32(0) {
				break
			}
		}
	}
}

func contentPDFHexCode(v contentPDFValue) (uint32, bool) {
	if v.kind != contentPDFString || len(v.str) == 0 || len(v.str) > 4 {
		return 0, false
	}
	var c uint32
	for _, b := range v.str {
		c = c<<8 | uint32(b)
	}
	return c, true
}

func contentPDFUTF16Runes(v contentPDFValue) ([]rune, bool) {
	if v.kind != contentPDFString {
		return nil, false
	}
	b := v.str
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return utf16.Decode(units), true
}

// contentPDFGlyphRune maps an Adobe glyph name from /Differences to a rune. The
// set covers the accented Latin letters and punctuation that show up in simple
// fonts; an unknown name leaves the base-encoding entry untouched.
func contentPDFGlyphRune(name string) (rune, bool) {
	r, ok := contentPDFGlyphNames[name]
	return r, ok
}

var contentPDFGlyphNames = map[string]rune{
	"space": ' ', "exclam": '!', "quotedbl": '"', "numbersign": '#', "dollar": '$',
	"percent": '%', "ampersand": '&', "quotesingle": '\'', "quoteright": '\u2019',
	"quoteleft": '\u2018', "quotedblleft": '\u201C', "quotedblright": '\u201D',
	"parenleft": '(', "parenright": ')', "asterisk": '*', "plus": '+', "comma": ',',
	"hyphen": '-', "period": '.', "slash": '/', "zero": '0', "one": '1', "two": '2',
	"three": '3', "four": '4', "five": '5', "six": '6', "seven": '7', "eight": '8',
	"nine": '9', "colon": ':', "semicolon": ';', "less": '<', "equal": '=',
	"greater": '>', "question": '?', "at": '@', "bracketleft": '[', "backslash": '\\',
	"bracketright": ']', "asciicircum": '^', "underscore": '_', "grave": '`',
	"braceleft": '{', "bar": '|', "braceright": '}', "asciitilde": '~',
	"exclamdown": '\u00A1', "cent": '\u00A2', "sterling": '\u00A3', "currency": '\u00A4',
	"yen": '\u00A5', "brokenbar": '\u00A6', "section": '\u00A7', "dieresis": '\u00A8',
	"copyright": '\u00A9', "ordfeminine": '\u00AA', "guillemotleft": '\u00AB',
	"logicalnot": '\u00AC', "registered": '\u00AE', "macron": '\u00AF',
	"degree": '\u00B0', "plusminus": '\u00B1', "twosuperior": '\u00B2',
	"threesuperior": '\u00B3', "acute": '\u00B4', "mu": '\u00B5', "paragraph": '\u00B6',
	"periodcentered": '\u00B7', "cedilla": '\u00B8', "onesuperior": '\u00B9',
	"ordmasculine": '\u00BA', "guillemotright": '\u00BB', "onequarter": '\u00BC',
	"onehalf": '\u00BD', "threequarters": '\u00BE', "questiondown": '\u00BF',
	"Agrave": '\u00C0', "Aacute": '\u00C1', "Acircumflex": '\u00C2', "Atilde": '\u00C3',
	"Adieresis": '\u00C4', "Aring": '\u00C5', "AE": '\u00C6', "Ccedilla": '\u00C7',
	"Egrave": '\u00C8', "Eacute": '\u00C9', "Ecircumflex": '\u00CA', "Edieresis": '\u00CB',
	"Igrave": '\u00CC', "Iacute": '\u00CD', "Icircumflex": '\u00CE', "Idieresis": '\u00CF',
	"Eth": '\u00D0', "Ntilde": '\u00D1', "Ograve": '\u00D2', "Oacute": '\u00D3',
	"Ocircumflex": '\u00D4', "Otilde": '\u00D5', "Odieresis": '\u00D6', "multiply": '\u00D7',
	"Oslash": '\u00D8', "Ugrave": '\u00D9', "Uacute": '\u00DA', "Ucircumflex": '\u00DB',
	"Udieresis": '\u00DC', "Yacute": '\u00DD', "Thorn": '\u00DE', "germandbls": '\u00DF',
	"agrave": '\u00E0', "aacute": '\u00E1', "acircumflex": '\u00E2', "atilde": '\u00E3',
	"adieresis": '\u00E4', "aring": '\u00E5', "ae": '\u00E6', "ccedilla": '\u00E7',
	"egrave": '\u00E8', "eacute": '\u00E9', "ecircumflex": '\u00EA', "edieresis": '\u00EB',
	"igrave": '\u00EC', "iacute": '\u00ED', "icircumflex": '\u00EE', "idieresis": '\u00EF',
	"eth": '\u00F0', "ntilde": '\u00F1', "ograve": '\u00F2', "oacute": '\u00F3',
	"ocircumflex": '\u00F4', "otilde": '\u00F5', "odieresis": '\u00F6', "divide": '\u00F7',
	"oslash": '\u00F8', "ugrave": '\u00F9', "uacute": '\u00FA', "ucircumflex": '\u00FB',
	"udieresis": '\u00FC', "yacute": '\u00FD', "thorn": '\u00FE', "ydieresis": '\u00FF',
	"Euro": '\u20AC', "endash": '\u2013', "emdash": '\u2014', "bullet": '\u2022',
	"ellipsis": '\u2026', "trademark": '\u2122', "dagger": '\u2020', "daggerdbl": '\u2021',
	"perthousand": '\u2030', "guilsinglleft": '\u2039', "guilsinglright": '\u203A',
	"fi": '\uFB01', "fl": '\uFB02', "minus": '\u2212', "fraction": '\u2044',
	"ff": '\uFB00', "ffi": '\uFB03', "ffl": '\uFB04',
}
