package main

// Extended glob matching (name-search lessons, fsearch parity).
//
// Beyond the historical `glob:*.py` filename match this file adds:
//   - brace sets: `*.{c,h}`, `src/**/*.{c,h}` expand to alternatives;
//     a name/path matches when ANY alternative matches;
//   - `**` path globs: `**` crosses directory separators, `*`/`?`/`[]`
//     stay within one path segment;
//   - path globs: a pattern containing `/` (or `\`, normalized) or `**`
//     matches against the full path instead of just the file name.
//
// Planner soundness: brace alternatives are OR, so only literals COMMON to
// every expansion may feed posting lanes (globCommonLiterals); the
// per-alternative ext optimization (simpleGlobExts) declines brace
// patterns instead of reading them as literal extensions.

import (
	"path"
	"path/filepath"
	"strings"
)

// isPathGlobPattern reports whether pattern must match against the full
// path: it names a directory (separator) or uses recursive `**`.
func isPathGlobPattern(pattern string) bool {
	if strings.Contains(pattern, "**") {
		return true
	}
	return strings.ContainsAny(pattern, `/\`)
}

// expandBraceAlternatives expands `{a,b}` sets into one pattern per
// alternative. Nested braces expand recursively; an unbalanced brace is
// literal (returned as the single pattern).
func expandBraceAlternatives(pattern string) []string {
	start := strings.IndexByte(pattern, '{')
	if start < 0 {
		return []string{pattern}
	}
	depth := 0
	end := -1
	for i := start; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
				break
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return []string{pattern}
	}
	// Split the body on top-level commas.
	var alts []string
	depth = 0
	cur := start + 1
	for i := start + 1; i < end; i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				alts = append(alts, pattern[cur:i])
				cur = i + 1
			}
		}
	}
	alts = append(alts, pattern[cur:end])
	if len(alts) < 2 {
		// No top-level comma: not a set (`{GUID}`); the braces are
		// literal, like bash.
		return []string{pattern}
	}
	prefix, suffix := pattern[:start], pattern[end+1:]
	var out []string
	for _, alt := range alts {
		for _, expanded := range expandBraceAlternatives(prefix + alt + suffix) {
			out = append(out, expanded)
		}
	}
	return out
}

// matchNameGlob reports whether a file NAME matches a filename glob.
// Patterns are already case-normalized by the parser when the query is not
// case-sensitive. Brace sets match when any alternative matches.
func matchNameGlob(pattern, name string) bool {
	if strings.Contains(pattern, "{") {
		for _, alt := range expandBraceAlternatives(pattern) {
			if ok, err := filepath.Match(alt, name); err == nil && ok {
				return true
			}
		}
		return false
	}
	ok, err := filepath.Match(pattern, name)
	return err == nil && ok
}

// normalizeGlobPath maps a filesystem path to `/`-separated form for glob
// matching, folding case unless the query is case-sensitive.
func normalizeGlobPath(pathIn string, caseSensitive bool) string {
	s := strings.ReplaceAll(pathIn, `\`, "/")
	if !caseSensitive {
		s = strings.ToLower(s)
	}
	return s
}

// matchPathGlob reports whether a full PATH matches a path glob pattern.
// The pattern is already case-normalized by the parser when the query is
// not case-sensitive. `**` as a whole segment matches zero or more path
// segments; within a segment `*`, `?` and `[...]` follow path.Match.
//
// Relative patterns (no leading `/` or drive) match at any depth: `src/...`
// behaves as `**/src/...`, so a path glob finds its subtree anywhere in the
// index. Absolute patterns anchor at the root.
func matchPathGlob(pattern, pathIn string, caseSensitive bool) bool {
	normPath := normalizeGlobPath(pathIn, caseSensitive)
	normPat := strings.ReplaceAll(pattern, `\`, "/")
	try := func(pat string) bool {
		if strings.Contains(pat, "{") {
			for _, alt := range expandBraceAlternatives(pat) {
				if matchDoublestar(alt, normPath) {
					return true
				}
			}
			return false
		}
		return matchDoublestar(pat, normPath)
	}
	if try(normPat) {
		return true
	}
	if isAbsoluteGlobPattern(normPat) {
		return false
	}
	return try("**/" + normPat)
}

// isAbsoluteGlobPattern reports whether a `/`-separated glob pattern anchors
// at the filesystem root (leading `/` or a drive prefix like `e:`).
func isAbsoluteGlobPattern(normPat string) bool {
	if strings.HasPrefix(normPat, "/") {
		return true
	}
	if len(normPat) >= 2 && normPat[1] == ':' &&
		((normPat[0] >= 'a' && normPat[0] <= 'z') || (normPat[0] >= 'A' && normPat[0] <= 'Z')) {
		return true
	}
	return false
}

// matchDoublestar matches an already-normalized pattern against an
// already-normalized `/`-separated path.
func matchDoublestar(pattern, s string) bool {
	patSegs := strings.Split(pattern, "/")
	pathSegs := strings.Split(s, "/")
	return matchDoublestarSegs(patSegs, pathSegs)
}

func matchDoublestarSegs(pat, s []string) bool {
	if len(pat) == 0 {
		return len(s) == 0
	}
	if pat[0] == "**" {
		// `**` swallows zero or more segments. Collapse runs first so
		// `**/**` does not recurse exponentially.
		for len(pat) > 1 && pat[1] == "**" {
			pat = pat[1:]
		}
		for i := 0; i <= len(s); i++ {
			if matchDoublestarSegs(pat[1:], s[i:]) {
				return true
			}
		}
		return false
	}
	if len(s) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], s[0])
	if err != nil || !ok {
		return false
	}
	return matchDoublestarSegs(pat[1:], s[1:])
}

// regexDisjunctMasks converts provably-required regex literal disjuncts
// into letter masks, one combined mask per disjunct. A match must contain
// every run of some disjunct, so a candidate path whose mask lacks every
// disjunct mask cannot match. Returns nil when nothing is provable (also
// caps at 16 disjuncts so hostile alternations cannot bloat the per-query
// parse or the per-candidate check).
func regexDisjunctMasks(pattern string) []uint64 {
	alts := regexRequiredLiteralAlternatives(pattern)
	if len(alts) == 0 || len(alts) > 16 {
		return nil
	}
	out := make([]uint64, 0, len(alts))
	for _, runs := range alts {
		var m uint64
		empty := true
		for _, r := range runs {
			if r == "" {
				continue
			}
			empty = false
			m |= memoMaskOf(r)
		}
		if empty {
			return nil
		}
		out = append(out, m)
	}
	return out
}

// braceGlobExts returns the extension list when pattern is a brace set over
// plain `*.ext` alternatives (`*.{c,h}` → [c h]), so the query rides the
// ext postings as ext:c|h instead of scanning. Non-brace patterns and
// non-ext alternatives decline.
func braceGlobExts(pattern string) ([]string, bool) {
	if !strings.Contains(pattern, "{") {
		return nil, false
	}
	var exts []string
	for _, alt := range expandBraceAlternatives(pattern) {
		if !strings.HasPrefix(alt, "*.") || strings.Count(alt, "*") != 1 {
			return nil, false
		}
		rest := strings.TrimPrefix(alt, "*.")
		if rest == "" || strings.ContainsAny(rest, "\\/*?[]:{}!") {
			return nil, false
		}
		exts = append(exts, rest)
	}
	if len(exts) < 1 {
		return nil, false
	}
	return exts, true
}

// bracePathGlobExts returns the extension list when every brace expansion
// of a path pattern ends in a plain `*.ext` final segment
// (`src/**/*.{c,h}` → [c h]), so the query can AND an ext OR for the
// posting lanes. Non-brace patterns and non-ext finals decline.
func bracePathGlobExts(pattern string) ([]string, bool) {
	if !strings.Contains(pattern, "{") {
		return nil, false
	}
	var exts []string
	for _, alt := range expandBraceAlternatives(pattern) {
		seg := alt
		if i := strings.LastIndexAny(seg, `/\`); i >= 0 {
			seg = seg[i+1:]
		}
		if !strings.HasPrefix(seg, "*.") || strings.Count(seg, "*") != 1 {
			return nil, false
		}
		rest := strings.TrimPrefix(seg, "*.")
		if rest == "" || strings.ContainsAny(rest, "\\/*?[]:{}!") {
			return nil, false
		}
		exts = append(exts, rest)
	}
	if len(exts) < 1 {
		return nil, false
	}
	return exts, true
}

// globAltsFor returns the precomputed expansions for parallel arrays,
// falling back to live expansion for hand-built queries.
func globAltsFor(alts [][]string, idx int, pattern string) []string {
	if idx < len(alts) && len(alts[idx]) > 0 {
		return alts[idx]
	}
	return expandBraceAlternatives(pattern)
}

// matchNameAlts reports whether a file NAME matches any precomputed
// alternative. Patterns are already case-normalized by the parser when the
// query is not case-sensitive.
func matchNameAlts(alts []string, name string) bool {
	for _, alt := range alts {
		if ok, err := filepath.Match(alt, name); err == nil && ok {
			return true
		}
	}
	return false
}

// matchPathAlts reports whether a full PATH matches any precomputed
// alternative (relative patterns match at any depth, as in matchPathGlob).
func matchPathAlts(alts []string, pathIn string, caseSensitive bool) bool {
	normPath := normalizeGlobPath(pathIn, caseSensitive)
	for _, alt := range alts {
		normAlt := strings.ReplaceAll(alt, `\`, "/")
		if matchDoublestar(normAlt, normPath) {
			return true
		}
		if !isAbsoluteGlobPattern(normAlt) && matchDoublestar("**/"+normAlt, normPath) {
			return true
		}
	}
	return false
}

// globCommonLiterals returns literal runs (len >= 3, same floor as
// globCommonLiterals returns literal runs (len >= 3, same floor as
// splitGlobLiteralTerms) present in EVERY brace expansion of pattern, so
// they are safe required posting terms for the pattern as a whole.
func globCommonLiterals(pattern string, caseSensitive bool) []string {
	expansions := expandBraceAlternatives(pattern)
	if len(expansions) == 1 {
		return splitGlobLiteralTerms(pattern, caseSensitive)
	}
	common := make(map[string]int)
	var order []string
	for i, alt := range expansions {
		seen := make(map[string]struct{})
		for _, lit := range splitGlobLiteralTerms(alt, caseSensitive) {
			if _, ok := seen[lit]; ok {
				continue
			}
			seen[lit] = struct{}{}
			common[lit]++
			if i == 0 {
				order = append(order, lit)
			}
		}
	}
	out := make([]string, 0, len(order))
	for _, lit := range order {
		if common[lit] == len(expansions) {
			out = append(out, lit)
		}
	}
	return out
}
