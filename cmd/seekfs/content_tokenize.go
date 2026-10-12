package main

// Query tokenization for content search. The existing parser splits on
// strings.Fields, which cannot represent a quoted phrase or a regex literal
// containing '|' or spaces. This tokenizer keeps quoted spans and
// `<prefix>:/.../` regex spans as single tokens so applyQueryToken can parse
// them without the '|' OR-split tearing them apart.
//
// v1 only enables single-token content terms; phrases and regex parse correctly
// here and are switched on in P3.

import "strings"

func contentIsSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// contentTokenizeQuery splits q on unquoted whitespace. A double- or
// single-quoted span is kept whole (quotes included) so the parser can tell a
// phrase from a bare word. A `:/.../` span is kept whole as well, so a regex
// like content:/error|warn/ survives OR splitting.
func contentTokenizeQuery(q string) []string {
	var out []string
	i, n := 0, len(q)
	for i < n {
		for i < n && contentIsSpace(q[i]) {
			i++
		}
		if i >= n {
			break
		}
		start := i
		for i < n && !contentIsSpace(q[i]) {
			switch q[i] {
			case '"', '\'':
				i = contentSkipQuoted(q, i)
			case ':':
				if i+1 < n && q[i+1] == '/' {
					if end, ok := contentSkipRegexSpan(q, i+2); ok {
						i = end
						// Continue scanning flags/OR alternatives normally so a
						// following quoted phrase keeps its embedded whitespace.
						continue
					}
				}
				i++
			default:
				i++
			}
		}
		out = append(out, q[start:i])
	}
	return out
}

// contentSkipQuoted returns the index just past the closing quote, or len(q)
// when the quote is unterminated. A backslash escapes the next byte.
func contentSkipQuoted(q string, i int) int {
	quote := q[i]
	i++
	for i < len(q) {
		if q[i] == '\\' && i+1 < len(q) {
			i += 2
			continue
		}
		if q[i] == quote {
			return i + 1
		}
		i++
	}
	return len(q)
}

// contentSkipRegexSpan returns the index just past the closing '/', and whether
// a closing slash was found.
func contentSkipRegexSpan(q string, i int) (int, bool) {
	for i < len(q) {
		if q[i] == '\\' && i+1 < len(q) {
			i += 2
			continue
		}
		if q[i] == '/' {
			return i + 1, true
		}
		i++
	}
	return i, false
}

// contentUnquoteToken strips one layer of surrounding matching quotes and
// resolves backslash escapes for the quote character. A token without
// surrounding quotes is returned unchanged.
func contentUnquoteToken(tok string) string {
	if len(tok) < 2 {
		return tok
	}
	quote := tok[0]
	if (quote != '"' && quote != '\'') || tok[len(tok)-1] != quote {
		return tok
	}
	inner := tok[1 : len(tok)-1]
	if !strings.ContainsRune(inner, '\\') {
		return inner
	}
	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			i++
		}
		b.WriteByte(inner[i])
	}
	return b.String()
}

// contentTokenIsRegexSpan reports whether tok is `<something>:/.../` and returns
// the pattern between the slashes.
func contentTokenIsRegexSpan(tok string) (string, bool) {
	p := strings.Index(tok, ":/")
	if p < 0 {
		return "", false
	}
	// A regex inside an OR alternative is not a regex spanning the whole token.
	if len(featureSplitAlternatives(tok)) > 1 {
		return "", false
	}
	rest := tok[p+2:]
	end := strings.LastIndexByte(rest, '/')
	if end <= 0 {
		return "", false
	}
	return rest[:end], true
}
