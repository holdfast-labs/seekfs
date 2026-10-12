package main

import (
	"regexp"
	"strings"
)

// Query normalization and the Contents-toggle rewrite, ported from
// cmd/seekfs/ui.go (normalizeUIQueryForService and friends) and
// cmd/seekfs/ui_frontend/query.js (toContentQuery, normalizeLiveQuery).
// Keeping these identical to the Wails UI means both UIs send the service
// the same query string for the same user input.

// normalizeUIQueryForService rewrites Everything-style aliases to seekfs
// syntax and reports whether the query must match against the full path.
func normalizeUIQueryForService(query string, matchPath bool) (string, bool) {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return query, matchPath
	}
	if !matchPath && shouldUIUseLoosePathMatching(fields) {
		matchPath = true
	}
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		converted, nextMatchPath := normalizeEverythingTokenForService(field, matchPath)
		matchPath = nextMatchPath
		out = append(out, converted...)
	}
	return strings.Join(out, " "), matchPath
}

func shouldUIUseLoosePathMatching(fields []string) bool {
	for _, field := range fields {
		if strings.HasPrefix(field, "!") || strings.HasPrefix(field, "-") {
			continue
		}
		raw := strings.TrimLeft(field, "!-")
		if raw == "" {
			continue
		}
		if isVolumeQueryTerm(raw) {
			continue
		}
		if strings.ContainsAny(raw, `\/`) {
			return true
		}
		key, _, hasPrefix := strings.Cut(raw, ":")
		if hasPrefix {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "path", "fullpath", "full-path", "full_path", "fullpathname", "full-path-name", "location":
				return true
			}
			continue
		}
	}
	return false
}

// incompleteUIQuery reports whether the user is mid-token (a trailing "."
// or a bare "ext:" filter), in which case live search must not query the
// service yet.
func incompleteUIQuery(query string) bool {
	fields := strings.Fields(strings.TrimSpace(query))
	if len(fields) == 0 {
		return false
	}
	last := fields[len(fields)-1]
	return last == "." || strings.HasSuffix(last, ":")
}

// hasIncompleteTrailingToken is the query.js twin of incompleteUIQuery.
func hasIncompleteTrailingToken(query string) bool {
	return incompleteUIQuery(query)
}

var singleCharBlocklist = regexp.MustCompile(`^[^\w*?]$`)

// normalizeLiveQuery blanks partial input so live search never queries the
// service for an unfinished token. Ports normalizeLiveQuery from query.js.
func normalizeLiveQuery(query string) string {
	if query == "" {
		return ""
	}
	if singleCharBlocklist.MatchString(query) {
		return ""
	}
	if hasIncompleteTrailingToken(query) {
		return ""
	}
	return query
}

func normalizeEverythingTokenForService(field string, matchPath bool) ([]string, bool) {
	neg := ""
	raw := field
	if len(raw) > 1 && (raw[0] == '!' || raw[0] == '-') {
		neg = raw[:1]
		raw = raw[1:]
	}
	key, value, ok := strings.Cut(raw, ":")
	if !ok {
		return []string{field}, matchPath
	}
	key = strings.ToLower(strings.TrimSpace(key))
	value = strings.TrimSpace(value)
	if value == "" {
		switch key {
		case "file", "files":
			return []string{neg + "type:file"}, matchPath
		case "folder", "folders", "directory", "directories", "dir":
			return []string{neg + "type:dir"}, matchPath
		}
		return []string{field}, matchPath
	}

	switch key {
	case "path", "fullpath", "full-path", "full_path", "fullpathname", "full-path-name":
		if _, ok := dottedExtensionTerm(strings.ToLower(value)); ok {
			return []string{neg + "path:" + value}, true
		}
		return []string{neg + value}, true
	case "pathpart", "path-part", "path_part", "location":
		return []string{neg + "dir:" + value}, matchPath
	case "ext", "extension":
		return []string{neg + "ext:" + strings.TrimPrefix(value, ".")}, matchPath
	case "file", "files":
		return append([]string{neg + "type:file"}, normalizePrefixedEverythingValue(neg, value)...), matchPath
	case "folder", "folders", "directory", "directories":
		return append([]string{neg + "type:dir"}, normalizePrefixedEverythingValue(neg, value)...), matchPath
	case "name", "basename", "filename", "file-name":
		return []string{neg + value}, matchPath
	case "sz":
		return []string{neg + "size:" + value}, matchPath
	case "date", "date-modified", "datemodified", "modified":
		return []string{neg + "dm:" + value}, matchPath
	case "case-sensitive":
		return []string{"case:" + value}, matchPath
	default:
		return []string{field}, matchPath
	}
}

func normalizePrefixedEverythingValue(neg, value string) []string {
	if key, rest, ok := strings.Cut(value, ":"); ok {
		switch strings.ToLower(key) {
		case "regex", "glob", "ext", "extension", "size", "sz", "dm", "date", "date-modified", "datemodified", "modified", "dir", "pathpart", "path-part", "location":
			converted, _ := normalizeEverythingTokenForService(key+":"+rest, false)
			for i := range converted {
				if neg != "" && !strings.HasPrefix(converted[i], neg) {
					converted[i] = neg + converted[i]
				}
			}
			return converted
		}
	}
	return []string{neg + value}
}

func isVolumeQueryTerm(term string) bool {
	if len(term) != 2 || term[1] != ':' {
		return false
	}
	c := term[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// dottedExtensionTerm reports whether term is a bare dotted extension
// shorthand (".go", ".txt"); longer dotted strings like ".opencode" are
// ordinary substrings.
func dottedExtensionTerm(term string) (string, bool) {
	if len(term) < 2 || len(term) > 6 || term[0] != '.' || strings.ContainsAny(term, `\/*?[]:`) {
		return "", false
	}
	ext := term[1:]
	for _, r := range ext {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			continue
		}
		return "", false
	}
	return ext, true
}

// queryTokens splits query on whitespace, except that a double-quoted phrase
// (alone or after a prefix, as in content:"two words") stays one token.
// Ports queryTokens from query.js.
func queryTokens(query string) []string {
	re := regexp.MustCompile(`(?:[^\s"]+|"[^"]*"?)+`)
	return re.FindAllString(query, -1)
}

// isStructuralToken reports whether the contents toggle must leave a token
// alone: a filter (ext:, path:, content:...), an operator, or a negation.
// Ports isStructuralToken from query.js.
func isStructuralToken(token string) bool {
	if ok, _ := regexp.MatchString(`^[a-z][\w-]*:`, token); ok {
		return true
	}
	if token == "|" {
		return true
	}
	switch strings.ToLower(token) {
	case "or", "and", "not":
		return true
	}
	return strings.HasPrefix(token, "-") || strings.HasPrefix(token, "!")
}

// toContentQuery rewrites the plain words of query as content: terms, so
// with the contents toggle on "needle haystack ext:go" searches inside
// files for both words while the filter still applies to names.
// Ports toContentQuery from query.js.
func toContentQuery(query string) string {
	tokens := queryTokens(query)
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if isStructuralToken(token) {
			out = append(out, token)
		} else {
			out = append(out, "content:"+token)
		}
	}
	return strings.Join(out, " ")
}

// buildServiceQuery applies the Contents toggle (if on) and then normalizes
// for the service. Returns the service query and match_path flag.
func buildServiceQuery(raw string, contentsMode bool, matchPath bool) (string, bool) {
	if contentsMode {
		raw = toContentQuery(raw)
	}
	return normalizeUIQueryForService(raw, matchPath)
}

// contentQueryHasLiteral reports whether query carries at least one literal
// (non-regex) content: term, i.e. snippet highlighting has something to find.
func contentQueryHasLiteral(query string) bool {
	return len(contentHighlightTerms(query)) > 0
}
