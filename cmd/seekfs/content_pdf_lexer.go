package main

// PDF object syntax: a bounded lexer over one already-materialised byte slice.
// The whole file is capped at maxRaw by the caller (an over-cap PDF is skipped),
// so the buffer itself is the first bound. These limits stop a single hostile
// dictionary, array, string or nesting from turning that buffer into an
// unbounded structure; every one is a clamp, not an allocation driven by an
// attacker-controlled field.
//
// The same lexer serves the content-stream tokenizer (nextContent) and the
// ToUnicode CMap reader; refs (`N G R`) are only recognised by the object path.

import "strconv"

type contentPDFKind uint8

const (
	contentPDFNull contentPDFKind = iota
	contentPDFBool
	contentPDFInt
	contentPDFReal
	contentPDFName
	contentPDFString
	contentPDFArray
	contentPDFDict
	contentPDFKindRef
	contentPDFStream
)

const (
	contentPDFMaxDepth        = 48
	contentPDFMaxValues       = 1 << 17
	contentPDFMaxArrayLen     = 1 << 16
	contentPDFMaxDictLen      = 1 << 12
	contentPDFMaxStringLen    = 1 << 24
	contentPDFMaxNameLen      = 1 << 12
	contentPDFMaxResolveDepth = 12
)

type contentPDFRef struct {
	num int
	gen int
}

type contentPDFValue struct {
	kind contentPDFKind
	i    int64
	f    float64
	b    bool
	name string
	str  []byte
	arr  []contentPDFValue
	dict map[string]contentPDFValue
	ref  contentPDFRef
	// raw is the still-encoded payload of a stream value.
	raw []byte
}

func contentPDFNullValue() contentPDFValue { return contentPDFValue{kind: contentPDFNull} }

func contentPDFNameValue(n string) contentPDFValue {
	return contentPDFValue{kind: contentPDFName, name: n}
}

func contentPDFIntOf(v contentPDFValue) (int64, bool) {
	switch v.kind {
	case contentPDFInt:
		return v.i, true
	case contentPDFReal:
		return int64(v.f), true
	}
	return 0, false
}

func contentPDFNumOf(v contentPDFValue) float64 {
	switch v.kind {
	case contentPDFInt:
		return float64(v.i)
	case contentPDFReal:
		return v.f
	}
	return 0
}

func contentPDFNameOf(v contentPDFValue) (string, bool) {
	if v.kind == contentPDFName {
		return v.name, true
	}
	return "", false
}

// contentPDFDictInt reads an integer entry from a decoded dictionary, returning
// def when absent or not a number.
func contentPDFDictInt(d map[string]contentPDFValue, key string, def int64) int64 {
	if v, ok := d[key]; ok {
		if n, ok := contentPDFIntOf(v); ok {
			return n
		}
	}
	return def
}

func contentPDFIsWhite(b byte) bool {
	switch b {
	case 0x00, 0x09, 0x0a, 0x0c, 0x0d, 0x20:
		return true
	}
	return false
}

func contentPDFIsDelim(b byte) bool {
	switch b {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func contentPDFIsRegular(b byte) bool { return !contentPDFIsWhite(b) && !contentPDFIsDelim(b) }

func contentPDFIsLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func contentPDFIsDigit(b byte) bool { return b >= '0' && b <= '9' }

func contentPDFHexVal(b byte) (byte, bool) {
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

type contentPDFLexer struct {
	buf    []byte
	pos    int
	budget int
}

func newContentPDFLexer(buf []byte) *contentPDFLexer {
	return &contentPDFLexer{buf: buf, budget: contentPDFMaxValues}
}

func (l *contentPDFLexer) skipSpace() {
	for l.pos < len(l.buf) {
		b := l.buf[l.pos]
		if contentPDFIsWhite(b) {
			l.pos++
			continue
		}
		if b == '%' {
			for l.pos < len(l.buf) && l.buf[l.pos] != '\n' && l.buf[l.pos] != '\r' {
				l.pos++
			}
			continue
		}
		return
	}
}

// readWord reads a run of regular characters (a token without delimiters).
func (l *contentPDFLexer) readWord() string {
	start := l.pos
	for l.pos < len(l.buf) && contentPDFIsRegular(l.buf[l.pos]) {
		l.pos++
		if l.pos-start > contentPDFMaxNameLen {
			break
		}
	}
	return string(l.buf[start:l.pos])
}

func (l *contentPDFLexer) peekWord() string {
	save := l.pos
	w := l.readWord()
	l.pos = save
	return w
}

// readName reads a name after the leading '/', decoding #xx escapes.
func (l *contentPDFLexer) readName() (string, bool) {
	if l.pos >= len(l.buf) || l.buf[l.pos] != '/' {
		return "", false
	}
	l.pos++
	out := make([]byte, 0, 16)
	for l.pos < len(l.buf) {
		b := l.buf[l.pos]
		if !contentPDFIsRegular(b) {
			break
		}
		if b == '#' && l.pos+2 < len(l.buf) {
			hi, ok1 := contentPDFHexVal(l.buf[l.pos+1])
			lo, ok2 := contentPDFHexVal(l.buf[l.pos+2])
			if ok1 && ok2 {
				out = append(out, hi<<4|lo)
				l.pos += 3
				if len(out) > contentPDFMaxNameLen {
					break
				}
				continue
			}
		}
		out = append(out, b)
		l.pos++
		if len(out) > contentPDFMaxNameLen {
			break
		}
	}
	return string(out), true
}

func (l *contentPDFLexer) readLiteralString() ([]byte, bool) {
	if l.pos >= len(l.buf) || l.buf[l.pos] != '(' {
		return nil, false
	}
	l.pos++
	out := make([]byte, 0, 32)
	depth := 1
	for l.pos < len(l.buf) {
		c := l.buf[l.pos]
		switch c {
		case '\\':
			l.pos++
			if l.pos >= len(l.buf) {
				return out, true
			}
			switch e := l.buf[l.pos]; e {
			case 'n':
				out = append(out, '\n')
				l.pos++
			case 'r':
				out = append(out, '\r')
				l.pos++
			case 't':
				out = append(out, '\t')
				l.pos++
			case 'b':
				out = append(out, '\b')
				l.pos++
			case 'f':
				out = append(out, '\f')
				l.pos++
			case '(', ')', '\\':
				out = append(out, e)
				l.pos++
			case '\r':
				l.pos++
				if l.pos < len(l.buf) && l.buf[l.pos] == '\n' {
					l.pos++
				}
			case '\n':
				l.pos++
			default:
				if e >= '0' && e <= '7' {
					v, n := 0, 0
					for n < 3 && l.pos < len(l.buf) && l.buf[l.pos] >= '0' && l.buf[l.pos] <= '7' {
						v = v*8 + int(l.buf[l.pos]-'0')
						l.pos++
						n++
					}
					out = append(out, byte(v))
					continue
				}
				out = append(out, e)
				l.pos++
			}
		case '(':
			depth++
			out = append(out, c)
			l.pos++
		case ')':
			depth--
			l.pos++
			if depth == 0 {
				return out, true
			}
			out = append(out, c)
		default:
			out = append(out, c)
			l.pos++
			if len(out) > contentPDFMaxStringLen {
				return out, true
			}
		}
	}
	return out, false
}

func (l *contentPDFLexer) readHexString() ([]byte, bool) {
	if l.pos >= len(l.buf) || l.buf[l.pos] != '<' {
		return nil, false
	}
	l.pos++
	out := make([]byte, 0, 32)
	var hi byte
	haveHi := false
	for l.pos < len(l.buf) {
		c := l.buf[l.pos]
		if c == '>' {
			l.pos++
			if haveHi {
				out = append(out, hi<<4)
			}
			return out, true
		}
		if v, ok := contentPDFHexVal(c); ok {
			if haveHi {
				out = append(out, hi<<4|v)
				haveHi = false
			} else {
				hi = v
				haveHi = true
			}
		}
		l.pos++
		if len(out) > contentPDFMaxStringLen {
			break
		}
	}
	return out, false
}

func (l *contentPDFLexer) parseArray(depth int) (contentPDFValue, bool) {
	l.pos++
	arr := make([]contentPDFValue, 0, 8)
	for {
		l.skipSpace()
		if l.pos >= len(l.buf) {
			return contentPDFNullValue(), false
		}
		if l.buf[l.pos] == ']' {
			l.pos++
			return contentPDFValue{kind: contentPDFArray, arr: arr}, true
		}
		v, ok := contentPDFParseValue(l, depth+1)
		if !ok {
			return contentPDFNullValue(), false
		}
		if len(arr) < contentPDFMaxArrayLen {
			arr = append(arr, v)
		}
	}
}

func (l *contentPDFLexer) parseDict(depth int) (contentPDFValue, bool) {
	l.pos += 2
	d := make(map[string]contentPDFValue, 8)
	for {
		l.skipSpace()
		if l.pos >= len(l.buf) {
			return contentPDFNullValue(), false
		}
		if l.buf[l.pos] == '>' && l.pos+1 < len(l.buf) && l.buf[l.pos+1] == '>' {
			l.pos += 2
			return contentPDFValue{kind: contentPDFDict, dict: d}, true
		}
		if l.buf[l.pos] != '/' {
			return contentPDFNullValue(), false
		}
		key, ok := l.readName()
		if !ok {
			return contentPDFNullValue(), false
		}
		v, ok := contentPDFParseValue(l, depth+1)
		if !ok {
			return contentPDFNullValue(), false
		}
		if len(d) < contentPDFMaxDictLen {
			d[key] = v
		}
	}
}

// contentPDFParseValue parses one object value. ok is false on malformed input
// or when the token is a keyword / terminator the caller must handle; explicit
// `null` returns ok=true.
func contentPDFParseValue(l *contentPDFLexer, depth int) (contentPDFValue, bool) {
	if depth > contentPDFMaxDepth {
		return contentPDFNullValue(), false
	}
	l.budget--
	if l.budget < 0 {
		return contentPDFNullValue(), false
	}
	l.skipSpace()
	if l.pos >= len(l.buf) {
		return contentPDFNullValue(), false
	}
	c := l.buf[l.pos]
	switch {
	case c == '/':
		name, ok := l.readName()
		if !ok {
			return contentPDFNullValue(), false
		}
		return contentPDFNameValue(name), true
	case c == '(':
		s, ok := l.readLiteralString()
		if !ok {
			return contentPDFNullValue(), false
		}
		return contentPDFValue{kind: contentPDFString, str: s}, true
	case c == '<':
		if l.pos+1 < len(l.buf) && l.buf[l.pos+1] == '<' {
			return l.parseDict(depth)
		}
		s, ok := l.readHexString()
		if !ok {
			return contentPDFNullValue(), false
		}
		return contentPDFValue{kind: contentPDFString, str: s}, true
	case c == '[':
		return l.parseArray(depth)
	case c == ']' || c == '>' || c == '}':
		return contentPDFNullValue(), false
	case c == '-' || c == '+' || c == '.' || contentPDFIsDigit(c):
		return l.parseNumberOrRef()
	case contentPDFIsRegular(c):
		switch l.readWord() {
		case "true":
			return contentPDFValue{kind: contentPDFBool, b: true}, true
		case "false":
			return contentPDFValue{kind: contentPDFBool}, true
		case "null":
			return contentPDFNullValue(), true
		}
		return contentPDFNullValue(), false
	default:
		l.pos++
		return contentPDFNullValue(), false
	}
}

func (l *contentPDFLexer) parseNumberOrRef() (contentPDFValue, bool) {
	start := l.pos
	tok := l.readWord()
	if tok == "" {
		l.pos = start + 1
		return contentPDFNullValue(), false
	}
	if n, err := strconv.ParseInt(tok, 10, 64); err == nil {
		if n >= 0 {
			save := l.pos
			l.skipSpace()
			if l.pos < len(l.buf) && contentPDFIsDigit(l.buf[l.pos]) {
				save2 := l.pos
				tok2 := l.readWord()
				if g, err2 := strconv.Atoi(tok2); err2 == nil && g >= 0 {
					l.skipSpace()
					if l.pos < len(l.buf) && l.buf[l.pos] == 'R' &&
						(l.pos+1 >= len(l.buf) || !contentPDFIsRegular(l.buf[l.pos+1])) {
						l.pos++
						return contentPDFValue{kind: contentPDFKindRef, ref: contentPDFRef{num: int(n), gen: g}}, true
					}
				}
				l.pos = save2
			}
			l.pos = save
		}
		return contentPDFValue{kind: contentPDFInt, i: n}, true
	}
	if f, err := strconv.ParseFloat(tok, 64); err == nil {
		return contentPDFValue{kind: contentPDFReal, f: f}, true
	}
	l.pos = start + 1
	return contentPDFNullValue(), false
}

// nextContent returns the next content-stream token. An operator is returned in
// op with isOp=true; an operand value has isOp=false. ok is false at EOF.
func (l *contentPDFLexer) nextContent() (op string, val contentPDFValue, isOp bool, ok bool) {
	for {
		l.skipSpace()
		if l.pos >= len(l.buf) {
			return "", contentPDFNullValue(), false, false
		}
		c := l.buf[l.pos]
		switch {
		case c == '\'' || c == '"':
			l.pos++
			return string(rune(c)), contentPDFNullValue(), true, true
		case contentPDFIsLetter(c):
			return l.readWord(), contentPDFNullValue(), true, true
		case c == '/' || c == '(' || c == '[' || c == '<' ||
			c == '+' || c == '-' || c == '.' || contentPDFIsDigit(c):
			start := l.pos
			v, parsed := contentPDFParseValue(l, 0)
			if !parsed {
				if l.pos <= start {
					l.pos = start + 1
				}
				continue
			}
			return "", v, false, true
		default:
			l.pos++
		}
	}
}
