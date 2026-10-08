package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func implicitPathSeparatorToken(raw string) bool {
	if raw == "" || strings.HasPrefix(raw, "!") || strings.HasPrefix(raw, "-") ||
		strings.Contains(raw, "|") || !strings.ContainsAny(raw, `\/`) {
		return false
	}
	colon := strings.IndexByte(raw, ':')
	return colon < 0 || (colon == 1 && ((raw[0] >= 'a' && raw[0] <= 'z') || (raw[0] >= 'A' && raw[0] <= 'Z')))
}

func promotePathExtensionTerms(pq *parsedQuery) {
	if pq == nil || (!pq.MatchPath && len(pq.Dirs) == 0 && pq.Under == "") {
		return
	}
	terms := pq.Terms[:0]
	for _, term := range pq.Terms {
		if ext, ok := dottedExtensionTerm(term); ok {
			pq.Exts = append(pq.Exts, ext)
			continue
		}
		terms = append(terms, term)
	}
	pq.Terms = terms
	promotePathBareExtensionTerms(pq)
}

func promotePathBareExtensionTerms(pq *parsedQuery) {
	if pq == nil || (!pq.MatchPath && len(pq.Dirs) == 0 && pq.Under == "") {
		return
	}
	hasPathAnchor := pq.Under != "" || len(pq.Dirs) > 0
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) || commonPathBareExtensionTerm(term) || strings.ContainsAny(term, `\/*?[]:.`) {
			continue
		}
		if len(term) >= 3 {
			hasPathAnchor = true
			break
		}
	}
	if !hasPathAnchor {
		return
	}
	terms := pq.Terms[:0]
	for _, term := range pq.Terms {
		if commonPathBareExtensionTerm(term) {
			pq.Exts = append(pq.Exts, term)
			continue
		}
		terms = append(terms, term)
	}
	pq.Terms = terms
}

func commonPathBareExtensionTerm(term string) bool {
	switch strings.ToLower(term) {
	case "md", "nrrd", "raw", "pdf", "json", "go", "py", "txt", "csv", "tsv",
		"doc", "docx", "xls", "xlsx", "ppt", "pptx", "png", "jpg", "jpeg",
		"zip", "whl", "toml", "yaml", "yml", "xml", "html", "css", "js", "ts":
		return true
	default:
		return false
	}
}

func queryLooksPathScoped(query string) bool {
	for _, field := range strings.Fields(query) {
		if strings.ContainsAny(field, `\/`) {
			return true
		}
	}
	return false
}

func queryLooksLoosePathScoped(query string) bool {
	fields := strings.Fields(query)
	for _, field := range fields {
		if strings.HasPrefix(field, "!") || strings.HasPrefix(field, "-") {
			continue
		}
		raw := strings.TrimLeft(field, "!-")
		if raw == "" {
			continue
		}
		key, _, hasPrefix := strings.Cut(raw, ":")
		if hasPrefix {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "path", "fullpath", "full-path", "full_path", "fullpathname", "full-path-name", "location":
				return true
			case "regex", "regexp", "re":
				// A regex is matched against the full path, so it is inherently
				// path-scoped.  Treat a bare regex: query as path-scoped so the
				// regex-literal planner can serve it instead of declining to an
				// exhaustive scan.
				return true
			}
			continue
		}
		// An explicit path-like token (with a path separator) is the only
		// remaining auto path trigger.  Plain multi-term queries stay name
		// matching so they can use the fast filename posting instead of forcing
		// an exhaustive path scan.
		if strings.ContainsAny(raw, `\/`) {
			return true
		}
	}
	return false
}

func dottedExtensionTerm(term string) (string, bool) {
	// Bare dotted tokens are only an extension shorthand for common short
	// extensions. Longer dotted strings, such as ".opencode", are ordinary
	// substrings unless the user explicitly writes ext:opencode.
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

// isEmpty reports whether a parsed query carries no searchable constraints.
func (pq parsedQuery) isEmpty() bool {
	return len(pq.Terms) == 0 && len(pq.Exts) == 0 && len(pq.Dirs) == 0 &&
		len(pq.Globs) == 0 && len(pq.Regexps) == 0 && pq.Type == "" &&
		len(pq.Parents) == 0 && pq.Under == "" && !pq.HasModAfter && len(pq.SizeFilters) == 0 &&
		len(pq.DateFilters) == 0 && len(pq.AttrFilters) == 0 && len(pq.OrGroups) == 0 && len(pq.NotGroups) == 0
}

// applyQueryToken parses a single whitespace-delimited token and folds it into
// pq. It handles OR groups (a|b), negation (!term or -term), structured filters
// (ext:, dir:, glob:, regex:, type:, case:, size:, dm:), and plain terms.
// Unknown "name:" style prefixes are rejected so the tool never silently
// degrades an unsupported filter into a literal substring match.
func applyQueryToken(pq *parsedQuery, raw string) error {
	if raw == "" {
		return nil
	}

	// Bare path-separator tokens ("/", "\", "\\") are noise from shell
	// typing: a query like "AGENTS.md / pyproject.toml / ext:py" uses
	// slashes as visual separators. They carry no search intent and, kept
	// as terms, would gate the fast lanes (a "/" term cannot drive gram
	// candidates) and force the slow bounded scan. Drop them.
	if isBarePathSeparatorToken(raw) {
		return nil
	}

	// Negation: !term or -term excludes records matching the inner token.
	if (strings.HasPrefix(raw, "!") || strings.HasPrefix(raw, "-")) && len(raw) > 1 {
		inner := raw[1:]
		sub := parsedQuery{MatchPath: pq.MatchPath, CaseSensitive: pq.CaseSensitive}
		if err := applyQueryToken(&sub, inner); err != nil {
			return err
		}
		if !sub.isEmpty() {
			pq.NotGroups = append(pq.NotGroups, sub)
		}
		return nil
	}

	// OR group: a|b|c. Each alternative is parsed as its own subquery; a record
	// matches the group if it matches any alternative. We only treat '|' as an
	// operator when it joins token-like alternatives (not inside a regex, which
	// uses the regex: prefix and is handled before this point).
	if strings.Contains(raw, "|") && !strings.HasPrefix(raw, "regex:") {
		parts := strings.Split(raw, "|")
		group := make([]parsedQuery, 0, len(parts))
		for _, part := range parts {
			if part == "" {
				continue
			}
			sub := parsedQuery{MatchPath: pq.MatchPath, CaseSensitive: pq.CaseSensitive}
			if err := applyQueryToken(&sub, part); err != nil {
				return err
			}
			if !sub.isEmpty() {
				if sub.MatchPath {
					pq.MatchPath = true
				}
				group = append(group, sub)
			}
		}
		if len(group) == 1 {
			// A degenerate "a|" collapses to a plain token.
			mergeSubquery(pq, group[0])
		} else if len(group) > 1 {
			pq.OrGroups = append(pq.OrGroups, group)
		}
		return nil
	}

	switch {
	case strings.HasPrefix(raw, "ext:"):
		ext := strings.TrimPrefix(raw, "ext:")
		ext = strings.TrimPrefix(ext, ".")
		if ext != "" {
			pq.Exts = append(pq.Exts, normalizeCase(ext, pq.CaseSensitive))
		}
	case strings.HasPrefix(raw, "dir:"):
		dir := strings.TrimPrefix(raw, "dir:")
		if dir != "" {
			pq.Dirs = append(pq.Dirs, normalizeCase(dir, pq.CaseSensitive))
		}
	case strings.HasPrefix(raw, "path:"):
		term := strings.TrimPrefix(raw, "path:")
		pq.MatchPath = true
		if term != "" {
			if isDriveRelativePathTerm(term) {
				// The documented drive-scoped form is `path:C: term`.
				// A fused `path:C:.nrrd` is a drive-relative literal, not
				// an extension filter; indexed seekfs paths are absolute and
				// cannot contain this colon form.  Mark it impossible so the
				// strict parser semantics do not trigger a full-volume scan.
				pq.Impossible = true
			}
			pq.Terms = append(pq.Terms, queryPlainTerms(term, pq.CaseSensitive, true)...)
		}
	case strings.HasPrefix(raw, "parent:"):
		parent := strings.TrimPrefix(raw, "parent:")
		if parent != "" {
			if strings.ContainsAny(parent, `\/:*?[]`) {
				return fmt.Errorf("invalid parent filter %q; parent: matches one directory name, not a path or glob", raw)
			}
			pq.Parents = append(pq.Parents, normalizeCase(parent, pq.CaseSensitive))
		}
	case strings.HasPrefix(raw, "glob:"):
		glob := strings.TrimPrefix(raw, "glob:")
		if glob != "" {
			pq.Globs = append(pq.Globs, normalizeCase(glob, pq.CaseSensitive))
		}
	case strings.HasPrefix(raw, "regex:"):
		pat := strings.TrimPrefix(raw, "regex:")
		if pat == "" {
			return nil
		}
		pq.RegexTerms = appendRegexLiteralTerms(pq.RegexTerms, pat, pq.CaseSensitive)
		if !pq.CaseSensitive {
			pat = "(?i)" + pat
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return fmt.Errorf("invalid regex %q: %w", pat, err)
		}
		pq.Regexps = append(pq.Regexps, re)
	case strings.HasPrefix(raw, "size:"):
		sf, err := parseSizeFilter(strings.TrimPrefix(raw, "size:"))
		if err != nil {
			return err
		}
		pq.SizeFilters = append(pq.SizeFilters, sf)
	case strings.HasPrefix(raw, "dm:"):
		df, err := parseDateFilter(strings.TrimPrefix(raw, "dm:"))
		if err != nil {
			return err
		}
		pq.DateFilters = append(pq.DateFilters, df)
	case strings.HasPrefix(raw, "attrib:"):
		mask, err := parseAttribFilter(strings.TrimPrefix(raw, "attrib:"))
		if err != nil {
			return err
		}
		pq.AttrFilters = append(pq.AttrFilters, mask)
	case strings.HasPrefix(raw, "sort:"):
		sortColumn := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "sort:")))
		switch sortColumn {
		case "size", "modified", "extension", "type", "path":
			pq.SortColumn = sortColumn
		default:
			return fmt.Errorf("unsupported sort %q; supported: sort:size, sort:modified, sort:extension, sort:type, sort:path", raw)
		}
	case raw == "case:" || raw == "case:true":
		pq.CaseSensitive = true
	case raw == "case:false":
		pq.CaseSensitive = false
	case raw == "type:file" || raw == "type:dir":
		pq.Type = strings.TrimPrefix(raw, "type:")
	case isUnknownFilterToken(raw):
		return fmt.Errorf("unsupported filter %q; supported: path: parent: ext: dir: glob: regex: type: case: size: dm: attrib: sort:size sort:modified sort:extension sort:type sort:path (and !term, a|b)", raw)
	case looksLikeImplicitFilenameGlob(raw):
		pq.Globs = append(pq.Globs, normalizeCase(raw, pq.CaseSensitive))
	default:
		fuzzyMarked := false
		trimmed := raw
		if strings.HasSuffix(trimmed, "~") && len(trimmed) > 1 {
			trimmed = strings.TrimSuffix(trimmed, "~")
			fuzzyMarked = trimmed != ""
		}
		terms := queryPlainTerms(trimmed, pq.CaseSensitive, pq.MatchPath)
		if fuzzyMarked && len(terms) == 1 {
			pq.Fuzzy = true
		}
		pq.Terms = append(pq.Terms, terms...)
	}
	return nil
}

func isDriveRelativePathTerm(term string) bool {
	if len(term) < 3 || term[1] != ':' {
		return false
	}
	c := term[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	return term[2] != '\\' && term[2] != '/'
}

// mergeSubquery folds the constraints of src into dst. Used when an OR group
// collapses to a single alternative.
func mergeSubquery(dst *parsedQuery, src parsedQuery) {
	dst.Terms = append(dst.Terms, src.Terms...)
	dst.Exts = append(dst.Exts, src.Exts...)
	dst.Dirs = append(dst.Dirs, src.Dirs...)
	dst.Globs = append(dst.Globs, src.Globs...)
	dst.Regexps = append(dst.Regexps, src.Regexps...)
	dst.RegexTerms = append(dst.RegexTerms, src.RegexTerms...)
	dst.Parents = append(dst.Parents, src.Parents...)
	dst.SizeFilters = append(dst.SizeFilters, src.SizeFilters...)
	dst.DateFilters = append(dst.DateFilters, src.DateFilters...)
	dst.AttrFilters = append(dst.AttrFilters, src.AttrFilters...)
	if src.Type != "" {
		dst.Type = src.Type
	}
	if src.MatchPath {
		dst.MatchPath = true
	}
}

// isUnknownFilterToken reports whether raw looks like an unsupported "name:"
// filter prefix (a short alphabetic prefix followed by ':') rather than a plain
// term that merely contains a colon (e.g. a Windows drive path "c:\foo").
func isUnknownFilterToken(raw string) bool {
	idx := strings.IndexByte(raw, ':')
	if idx <= 0 {
		return false
	}
	prefix := raw[:idx]
	// Windows drive letters ("c", "d") are single-char; treat single-char
	// prefixes as path-like, not filters.
	if len(prefix) < 2 {
		return false
	}
	// A filter prefix starts with a letter and is otherwise alphanumeric
	// (e.g. "size2", "attrib"). Anything else (digits-first, punctuation) is a
	// plain term that merely contains a colon.
	for i, r := range prefix {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		if i == 0 && !isLetter {
			return false
		}
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}

func queryPlainTerms(raw string, caseSensitive, matchPath bool) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !matchPath || !strings.ContainsAny(raw, `\/`) {
		return []string{normalizeCase(raw, caseSensitive)}
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == '\\' || r == '/' })
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "." {
			continue
		}
		out = append(out, normalizeCase(part, caseSensitive))
	}
	if len(out) == 0 {
		return []string{normalizeCase(raw, caseSensitive)}
	}
	return out
}

// isBarePathSeparatorToken reports whether raw consists only of path
// separators (or a drive-less trailing separator), e.g. "/", "\", "\\".
// Such tokens are visual noise from shell typing, not search intent.
func isBarePathSeparatorToken(raw string) bool {
	if raw == "" {
		return false
	}
	for _, r := range raw {
		if r != '\\' && r != '/' {
			return false
		}
	}
	return true
}

func looksLikeImplicitFilenameGlob(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, `\/`) {
		return false
	}
	return strings.ContainsAny(raw, "*?[")
}

func globLiteralTerms(globs []string, caseSensitive bool) []string {
	seen := make(map[string]struct{}, len(globs))
	out := make([]string, 0, len(globs))
	for _, glob := range globs {
		for _, term := range splitGlobLiteralTerms(glob, caseSensitive) {
			if _, ok := seen[term]; ok {
				continue
			}
			seen[term] = struct{}{}
			out = append(out, term)
		}
	}
	return out
}

func complexGlobExts(globs []string) []string {
	seen := make(map[string]struct{}, len(globs))
	out := make([]string, 0, len(globs))
	for _, glob := range globs {
		ext := strings.TrimPrefix(filepath.Ext(glob), ".")
		if ext == "" || strings.ContainsAny(ext, `\/*?[]:`) {
			continue
		}
		ext = strings.ToLower(ext)
		if _, ok := seen[ext]; ok {
			continue
		}
		seen[ext] = struct{}{}
		out = append(out, ext)
	}
	return out
}

func splitGlobLiteralTerms(glob string, caseSensitive bool) []string {
	var b strings.Builder
	out := make([]string, 0, 2)
	flush := func() {
		if b.Len() >= 3 {
			out = append(out, normalizeCase(b.String(), caseSensitive))
		}
		b.Reset()
	}
	for _, r := range glob {
		switch r {
		case '*', '?', '[', ']', '\\', '/', ':':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

func appendRegexLiteralTerms(out []string, pattern string, caseSensitive bool) []string {
	var b strings.Builder
	flush := func() {
		// Two-character runs are kept too: dropping them can hide an
		// alternation such as `.*\.(md|txt)$`, whose only >=3 run ("txt")
		// would then look like a required literal even though "md" matches
		// the regex without it.  Keeping short runs makes the planner treat
		// the query as ambiguous and decline to the exhaustive scan.
		if b.Len() >= 2 {
			out = append(out, normalizeCase(b.String(), caseSensitive))
		}
		b.Reset()
	}
	escaped := false
	for _, r := range pattern {
		if escaped {
			if isRegexLiteralRune(r) {
				b.WriteRune(r)
			} else {
				flush()
			}
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if isRegexLiteralRune(r) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func isRegexLiteralRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
}

func entryMatches(entry Entry, pq parsedQuery, matchPath bool) bool {
	path := filepath.Clean(entry.Path)
	name := entry.Name
	if name == "" {
		name = filepath.Base(path)
	}
	// Reuse the prebuilt lowercase strings when they describe exactly the
	// strings under test. compactEntryFromRecord builds LowerPath as
	// ToLower(Path) and LowerName as ToLower(Name) (compactLowerNameAt is
	// exactly strings.ToLower, including edge cases like U+0130), so when
	// Clean is a no-op the recompute below would only reproduce them while
	// allocating a fresh copy per candidate. ToLower and Clean commute
	// here: Clean only reacts to separators and dot elements, which
	// lowercasing neither creates nor removes, so Clean(Lower(Path)) would
	// equal Lower(Clean(Path)) whenever they differ at all.
	var cmpPath, cmpName string
	if pq.CaseSensitive {
		cmpPath, cmpName = path, name
	} else if entry.LowerPath != "" && path == entry.Path && entry.LowerName != "" && name == entry.Name {
		cmpPath, cmpName = entry.LowerPath, entry.LowerName
	} else {
		cmpPath, cmpName = strings.ToLower(path), strings.ToLower(name)
	}
	haystack := cmpName
	if matchPath {
		haystack = cmpPath
	}
	if pq.Under != "" && !pathUnder(path, pq.Under) {
		return false
	}
	if pq.Exists {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	if pq.HasModAfter {
		if entry.ModUnix == 0 || !time.Unix(0, entry.ModUnix).After(pq.ModifiedAfter) {
			return false
		}
	}
	if pq.Type == "file" && entry.Mode&uint32(os.ModeDir) != 0 {
		return false
	}
	if pq.Type == "dir" && entry.Mode&uint32(os.ModeDir) == 0 {
		return false
	}
	if len(pq.Parents) > 0 {
		parentName := normalizeCase(filepath.Base(filepath.Dir(path)), pq.CaseSensitive)
		for _, parent := range pq.Parents {
			if parentName != parent {
				return false
			}
		}
	}
	if !containsAll(haystack, pq.Terms) {
		return false
	}
	for _, ext := range pq.Exts {
		actual := strings.TrimPrefix(filepath.Ext(name), ".")
		if normalizeCase(actual, pq.CaseSensitive) != ext {
			return false
		}
	}
	for _, dir := range pq.Dirs {
		if !strings.Contains(cmpPath, dir) {
			return false
		}
	}
	for _, glob := range pq.Globs {
		ok, err := filepath.Match(glob, cmpName)
		if err != nil || !ok {
			return false
		}
	}
	for _, re := range pq.Regexps {
		if !re.MatchString(path) {
			return false
		}
	}
	for _, sf := range pq.SizeFilters {
		if !sf.matches(entry.Size) {
			return false
		}
	}
	for _, df := range pq.DateFilters {
		if !df.matches(entry.ModUnix) {
			return false
		}
	}
	if !attrFiltersMatch(entry.Mode, pq.AttrFilters) {
		return false
	}
	for _, group := range pq.OrGroups {
		matched := false
		for _, alt := range group {
			if entryMatches(entry, alt, matchPath || alt.MatchPath) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, neg := range pq.NotGroups {
		if entryMatches(entry, neg, matchPath || neg.MatchPath) {
			return false
		}
	}
	return true
}

func normalizedLimit(limit int, countOnly bool) int {
	if limit <= 0 && !countOnly {
		return 100
	}
	if limit <= 0 && countOnly {
		return 0
	}
	return limit
}

func normalizeCase(s string, caseSensitive bool) string {
	if caseSensitive {
		return s
	}
	return strings.ToLower(s)
}

func normalizeFilterPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func pathUnder(path, root string) bool {
	path = normalizeFilterPath(path)
	root = normalizeFilterPath(root)
	if strings.EqualFold(path, root) {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != "" && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func parseTimeValue(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", value); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q; use RFC3339 or YYYY-MM-DD", value)
}

// parseSizeFilter parses an Everything-style size constraint such as ">100mb",
// ">=1gb", "<4k", or "1024". A bare number is treated as an exact match.
func parseSizeFilter(spec string) (sizeFilter, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return sizeFilter{}, errors.New("empty size: filter")
	}
	op := "="
	switch {
	case strings.HasPrefix(spec, ">="):
		op, spec = ">=", spec[2:]
	case strings.HasPrefix(spec, "<="):
		op, spec = "<=", spec[2:]
	case strings.HasPrefix(spec, ">"):
		op, spec = ">", spec[1:]
	case strings.HasPrefix(spec, "<"):
		op, spec = "<", spec[1:]
	case strings.HasPrefix(spec, "="):
		op, spec = "=", spec[1:]
	}
	bytes, err := parseByteSize(strings.TrimSpace(spec))
	if err != nil {
		return sizeFilter{}, fmt.Errorf("invalid size: filter %q: %w", spec, err)
	}
	return sizeFilter{op: op, bytes: bytes}, nil
}

// parseByteSize parses a number with an optional unit suffix (b, kb, mb, gb,
// tb; the trailing 'b' is optional, e.g. "100mb" or "100m"). Units are 1024-based.
func parseByteSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "tb"):
		mult, s = 1<<40, s[:len(s)-2]
	case strings.HasSuffix(s, "gb"):
		mult, s = 1<<30, s[:len(s)-2]
	case strings.HasSuffix(s, "mb"):
		mult, s = 1<<20, s[:len(s)-2]
	case strings.HasSuffix(s, "kb"):
		mult, s = 1<<10, s[:len(s)-2]
	case strings.HasSuffix(s, "t"):
		mult, s = 1<<40, s[:len(s)-1]
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "b"):
		mult, s = 1, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("not a valid byte count")
	}
	return n * mult, nil
}

func parseAttribFilter(spec string) (uint32, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, errors.New("empty attrib: filter")
	}
	var mask uint32
	for _, r := range spec {
		switch r {
		case 'r', 'R':
			mask |= fileAttributeReadonly
		case 'h', 'H':
			mask |= fileAttributeHidden
		case 's', 'S':
			mask |= fileAttributeSystem
		case 'd', 'D':
			mask |= fileAttributeDir
		case 'a', 'A':
			mask |= fileAttributeArchive
		default:
			return 0, fmt.Errorf("invalid attrib: flag %q; supported flags are R,H,S,D,A", r)
		}
	}
	return mask, nil
}

// parseDateFilter parses an Everything-style dm: constraint. Supported specs:
// "today", "yesterday", "thisweek", "lastweek", a relative duration like "24h"
// or "7d", or an absolute date (YYYY-MM-DD) / RFC3339 timestamp meaning
// "modified on or after".
func parseDateFilter(spec string) (dateFilter, error) {
	spec = strings.ToLower(strings.TrimSpace(spec))
	if spec == "" {
		return dateFilter{}, errors.New("empty dm: filter")
	}
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch spec {
	case "today":
		return dateFilter{after: startOfDay, before: startOfDay.AddDate(0, 0, 1)}, nil
	case "yesterday":
		return dateFilter{after: startOfDay.AddDate(0, 0, -1), before: startOfDay}, nil
	case "thisweek":
		weekday := int(startOfDay.Weekday())
		weekStart := startOfDay.AddDate(0, 0, -weekday)
		return dateFilter{after: weekStart, before: weekStart.AddDate(0, 0, 7)}, nil
	case "lastweek":
		weekday := int(startOfDay.Weekday())
		weekStart := startOfDay.AddDate(0, 0, -weekday-7)
		return dateFilter{after: weekStart, before: weekStart.AddDate(0, 0, 7)}, nil
	}
	// Relative duration: support a "d" (days) suffix in addition to Go durations.
	if strings.HasSuffix(spec, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(spec, "d")); err == nil {
			return dateFilter{after: now.AddDate(0, 0, -days)}, nil
		}
	}
	if d, err := time.ParseDuration(spec); err == nil {
		return dateFilter{after: now.Add(-d)}, nil
	}
	if t, err := parseTimeValue(spec); err == nil {
		return dateFilter{after: t}, nil
	}
	return dateFilter{}, fmt.Errorf("invalid dm: filter %q; use today|yesterday|thisweek|lastweek, a duration (24h, 7d), or a date", spec)
}

// matchesSize reports whether size satisfies the filter.
func (sf sizeFilter) matches(size int64) bool {
	switch sf.op {
	case ">":
		return size > sf.bytes
	case ">=":
		return size >= sf.bytes
	case "<":
		return size < sf.bytes
	case "<=":
		return size <= sf.bytes
	default:
		return size == sf.bytes
	}
}

// matches reports whether the modification time (unix nanoseconds) satisfies the
// filter. A zero before bound means "no upper bound".
func (df dateFilter) matches(modUnixNanos int64) bool {
	if modUnixNanos == 0 {
		return false
	}
	t := time.Unix(0, modUnixNanos)
	if !df.after.IsZero() && t.Before(df.after) {
		return false
	}
	if !df.before.IsZero() && !t.Before(df.before) {
		return false
	}
	return true
}

func attrFiltersMatch(mode uint32, filters []uint32) bool {
	for _, mask := range filters {
		if mode&mask != mask {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func biasOrderEntries(idx *Index, order []int, root string) []int {
	if root == "" || len(order) == 0 {
		return order
	}
	out := append([]int(nil), order...)
	sort.SliceStable(out, func(i, j int) bool {
		a := pathUnder(idx.Entries[out[i]].Path, root)
		b := pathUnder(idx.Entries[out[j]].Path, root)
		return a && !b
	})
	return out
}

func (idx *Index) biasOrderCompact(order []int, root string) []int {
	if root == "" || len(order) == 0 {
		return order
	}
	cache := make(map[int]string)
	out := append([]int(nil), order...)
	sort.SliceStable(out, func(i, j int) bool {
		a := pathUnder(idx.reconstructCompactPathCached(out[i], cache), root)
		b := pathUnder(idx.reconstructCompactPathCached(out[j], cache), root)
		return a && !b
	})
	return out
}

func (idx *Index) compactPathContainsTerm(i int, term string) bool {
	if idx.Volume != "" && containsFoldASCII(idx.Volume, term) {
		return true
	}
	// Walk the parent chain without a per-call cycle-detection map: this is on
	// the broad-scan hot path (called per term per record across tens of millions
	// of records), and the depth cap already bounds a malformed chain. A cycle
	// simply exhausts the cap and returns false, the same result the map gave.
	cur := i
	for depth := 0; depth < 1024; depth++ {
		if cur < 0 || cur >= idx.compactRecordCount() {
			return false
		}
		parent, name := idx.compactParentNameAt(cur)
		if containsFoldASCII(name, term) {
			return true
		}
		if parent < 0 || int(parent) == cur {
			return false
		}
		cur = int(parent)
	}
	return false
}

func (idx *Index) reconstructCompactPath(i int) string {
	return idx.reconstructCompactPathCached(i, make(map[int]string))
}

func (idx *Index) reconstructCompactPathCached(i int, cache map[int]string) string {
	if path, ok := cache[i]; ok {
		return path
	}
	if i < 0 || i >= idx.compactRecordCount() {
		return ""
	}
	parts := make([]string, 0, 16)
	seen := make(map[int]struct{}, 16)
	cur := i
	for depth := 0; depth < 1024; depth++ {
		if path, ok := cache[cur]; ok {
			for p := len(parts) - 1; p >= 0; p-- {
				path += `\` + parts[p]
			}
			cache[i] = path
			return path
		}
		if cur < 0 || cur >= idx.compactRecordCount() {
			break
		}
		if _, ok := seen[cur]; ok {
			break
		}
		seen[cur] = struct{}{}
		parent, name := idx.compactParentNameAt(cur)
		if name != "." {
			parts = append(parts, name)
		}
		if parent < 0 {
			break
		}
		cur = int(parent)
	}
	root := idx.Volume
	rootName := ""
	if len(idx.Roots) > 0 {
		root = strings.TrimRight(idx.Roots[0], `\`)
		rootName = filepath.Base(root)
	}
	path := root
	for p := len(parts) - 1; p >= 0; p-- {
		if path == "" {
			path = parts[p]
		} else if rootName != "" && p == len(parts)-1 && parts[p] == rootName && len(parts) > 1 {
			continue
		} else {
			path += `\` + parts[p]
		}
	}
	cache[i] = path
	return path
}

// reconstructLowerPathInto is the map-free path builder used by the path rank.
// paths is a memo indexed by record id that doubles as the rank's sort-key
// array; computed marks which ids already have a path.  seen is generation-
// stamped so one array detects cycles across calls without clearing.  parts is a
// reusable scratch buffer holding RAW component names: the root-name skip test
// must compare raw strings exactly as reconstructCompactPathCached does, while
// each component is lowercased only when joined so the returned path matches
// strings.ToLower(reconstructCompactPathCached(...)).
func (idx *Index) reconstructLowerPathInto(i int, paths []string, computed []bool, seen []int32, gen int32, parts []string) []string {
	if i < 0 || i >= len(paths) || computed[i] {
		return parts
	}
	cur := i
	for depth := 0; depth < 1024; depth++ {
		if cur < 0 || cur >= len(paths) {
			break
		}
		if computed[cur] {
			path := paths[cur]
			for p := len(parts) - 1; p >= 0; p-- {
				path += `\` + strings.ToLower(parts[p])
			}
			paths[i] = path
			computed[i] = true
			return parts
		}
		if seen[cur] == gen {
			break
		}
		seen[cur] = gen
		rec := idx.compactRecord(cur)
		if rec.Name != "." {
			parts = append(parts, rec.Name)
		}
		if rec.Parent < 0 {
			break
		}
		cur = int(rec.Parent)
	}
	root := idx.Volume
	rootName := ""
	if len(idx.Roots) > 0 {
		root = strings.TrimRight(idx.Roots[0], `\`)
		rootName = filepath.Base(root)
	}
	path := strings.ToLower(root)
	for p := len(parts) - 1; p >= 0; p-- {
		part := strings.ToLower(parts[p])
		if path == "" {
			path = part
		} else if rootName != "" && p == len(parts)-1 && parts[p] == rootName && len(parts) > 1 {
			continue
		} else {
			path += `\` + part
		}
	}
	paths[i] = path
	computed[i] = true
	return parts
}

func containsAll(s string, terms []string) bool {
	for _, term := range terms {
		if !strings.Contains(s, term) {
			return false
		}
	}
	return true
}

func containsFoldASCII(s, term string) bool {
	if term == "" {
		return true
	}
	if len(term) > len(s) {
		return false
	}
	first := foldASCII(term[0])
	last := len(s) - len(term)
	for i := 0; i <= last; i++ {
		if foldASCII(s[i]) != first {
			continue
		}
		matched := true
		for j := 1; j < len(term); j++ {
			if foldASCII(s[i+j]) != foldASCII(term[j]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func sameUint32Slice(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type diskHeader struct {
	Magic       [8]byte
	Version     uint32
	EntryCount  uint64
	RootCount   uint64
	BuiltUnix   int64
	JournalID   uint64
	Checkpoint  int64
	Compact     uint32
	NameBlobLen uint64
	TokenCount  uint64
}

func readIndex(r io.Reader) (*Index, error) {
	return readIndexWithReaderAt(r, nil, 0)
}
