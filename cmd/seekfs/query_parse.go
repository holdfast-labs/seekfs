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
	"sync"
	"sync/atomic"
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
	if queryHasFeatureToken(query) {
		for _, token := range contentTokenizeQuery(query) {
			for _, part := range featureSplitAlternatives(token) {
				if strings.HasPrefix(strings.TrimLeft(part, "!-"), "feature:") {
					continue
				}
				if strings.ContainsAny(part, `\/`) {
					return true
				}
			}
		}
		return false
	}
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
// in: roots scope like --under (which is also not counted): an `in:dir`
// with no other constraint is not searchable, mirroring `--under` alone.
func (pq parsedQuery) isEmpty() bool {
	return len(pq.Terms) == 0 && len(pq.Exts) == 0 && len(pq.Dirs) == 0 &&
		len(pq.Globs) == 0 && len(pq.PathGlobs) == 0 &&
		len(pq.ExactTerms) == 0 && len(pq.Prefixes) == 0 && len(pq.Suffixes) == 0 &&
		len(pq.Regexps) == 0 && pq.Type == "" &&
		len(pq.Parents) == 0 && pq.Under == "" && !pq.HasModAfter && len(pq.SizeFilters) == 0 &&
		len(pq.DateFilters) == 0 && len(pq.AttrFilters) == 0 && len(pq.Content) == 0 && len(pq.Features) == 0 &&
		len(pq.GitFilters) == 0 && len(pq.NotUnders) == 0 &&
		len(pq.OrGroups) == 0 && len(pq.NotGroups) == 0
}

// lateVerified reports whether the query carries a predicate decidable only
// at the materialized-entry gate: Exists stats the filesystem, and git:
// resolves worktree status from the entry path. Rank-truncating candidate
// lanes must decline when true (they verify at the record level and would
// otherwise underfill the page); the full verify loop then decides exactly.
// Near is ranking-only and never sets this.
func (pq parsedQuery) lateVerified() bool {
	return pq.Exists || len(pq.GitFilters) > 0
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
		// A negated in: root cannot fold into Under (no single root
		// expresses "not under"); it becomes a per-record exclusion.
		if len(sub.InUnders) > 0 {
			pq.NotUnders = append(pq.NotUnders, sub.InUnders...)
			sub.InUnders = nil
		}
		if !sub.isEmpty() {
			pq.NotGroups = append(pq.NotGroups, sub)
		} else if len(sub.NotUnders) > 0 {
			// A pure `!in:dir` still constrains the query; keep the
			// exclusion even though the subquery has no other terms.
			pq.NotGroups = append(pq.NotGroups, sub)
		}
		return nil
	}

	// OR group: a|b|c. Each alternative is parsed as its own subquery; a record
	// matches the group if it matches any alternative. We only treat '|' as an
	// operator when it joins token-like alternatives (not inside a regex, which
	// uses the regex: prefix and is handled before this point). A
	// double-quoted phrase keeps its `|` literally.
	if _, isRegexSpan := contentTokenIsRegexSpan(raw); strings.Contains(raw, "|") && len(featureSplitAlternatives(raw)) > 1 && !strings.HasPrefix(raw, "regex:") && !isRegexSpan && !isQuotedQueryToken(raw) {
		parts := featureSplitAlternatives(raw)
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
	case strings.HasPrefix(raw, "feature:"):
		leaf, err := parseFeatureLeaf(strings.TrimPrefix(raw, "feature:"))
		if err != nil {
			return err
		}
		pq.Features = append(pq.Features, leaf)
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
	case strings.HasPrefix(raw, "in:"):
		// `in:<dir>` aliases `--under <dir>`: scope results to the
		// subtree. Multiple roots (and `--under`) conjoin at the end of
		// parseQuery; disjoint roots make the query impossible.
		root := strings.TrimSpace(strings.TrimPrefix(raw, "in:"))
		if root != "" {
			pq.InUnders = append(pq.InUnders, normalizeFilterPath(root))
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
		if glob == "" {
			break
		}
		norm := normalizeCase(glob, pq.CaseSensitive)
		if exts, ok := braceGlobExts(norm); ok && !isPathGlobPattern(glob) {
			// `glob:*.{c,h}` is exactly ext:c OR ext:h: build the OR
			// group manually so each alternative carries its ext:
			// re-parsing `ext:c|h` would split into ext:c OR bare
			// term h (prefixes do not distribute across `|`).
			appendExtOrGroup(pq, exts)
			return nil
		}
		if isPathGlobPattern(glob) {
			// A pattern naming directories (or `**`) matches the
			// full path; its brace-common literals join Dirs so
			// path posting lanes engage instead of full scans.
			pq.PathGlobs = append(pq.PathGlobs, norm)
			pq.PathGlobAlts = append(pq.PathGlobAlts, expandBraceAlternatives(norm))
			for _, lit := range globCommonLiterals(norm, pq.CaseSensitive) {
				pq.Dirs = append(pq.Dirs, lit)
			}
			// A brace set over final `*.ext` segments additionally
			// constrains the extension (required: any path match ends
			// in one of them), so AND an ext OR for the posting lanes.
			if exts, ok := bracePathGlobExts(norm); ok {
				appendExtOrGroup(pq, exts)
			}
		} else {
			pq.Globs = append(pq.Globs, norm)
			pq.GlobAlts = append(pq.GlobAlts, expandBraceAlternatives(norm))
			if strings.Contains(glob, "{") {
				for _, lit := range globCommonLiterals(norm, pq.CaseSensitive) {
					pq.Terms = append(pq.Terms, lit)
				}
			}
		}
	case strings.HasPrefix(raw, "git:"):
		spec := strings.TrimPrefix(raw, "git:")
		filters, err := parseGitFilters(spec)
		if err != nil {
			return err
		}
		pq.GitFilters = append(pq.GitFilters, filters...)
	case strings.HasPrefix(raw, "near:"):
		anchor := strings.TrimSpace(strings.TrimPrefix(raw, "near:"))
		if anchor != "" {
			// A near: anchor uses the same normalization as --under and the
			// bias roots, so query and flag forms compare identically.
			pq.Near = normalizeFilterPath(anchor)
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
		// Per-regex letter-mask prefilter (name-search lessons, fsearch
		// bitmap): a match must contain every literal run of some
		// provably-required disjunct, so a candidate whose path mask
		// lacks every disjunct mask cannot match and skips MatchString.
		// regexRequiredLiteralAlternatives is sound (widening-only) and
		// memoMaskOf is case-blind, so this never false-negatives under
		// (?i) or case-sensitive patterns alike. Nil when unprovable.
		pq.RegexMasks = append(pq.RegexMasks, regexDisjunctMasks(pat))
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
		case "relevance":
			// Content relevance is implemented only by the content pipeline.
			// A filename-only query keeps the pre-content contract and rejects
			// it as an unsupported sort, so the flag-off behavior is unchanged.
			if !queryHasContentToken(pq.Raw) {
				return fmt.Errorf("unsupported sort %q; supported: sort:size, sort:modified, sort:extension, sort:type, sort:path", raw)
			}
			pq.SortColumn = sortColumn
		default:
			return fmt.Errorf("unsupported sort %q; supported: sort:size, sort:modified, sort:extension, sort:type, sort:path", raw)
		}
	case raw == "case:" || raw == "case:true":
		pq.CaseSensitive = true
		pq.CaseExplicit = true
	case raw == "case:false":
		pq.CaseSensitive = false
		pq.CaseExplicit = true
	case raw == "type:file" || raw == "type:dir":
		pq.Type = strings.TrimPrefix(raw, "type:")
	case strings.HasPrefix(raw, "content:"):
		leaf, err := parseContentLeaf(strings.TrimPrefix(raw, "content:"))
		if err != nil {
			return err
		}
		pq.Content = append(pq.Content, leaf)
		if !contentSearchEnabled() {
			return contentUnavailableError()
		}
		return nil
	case strings.HasPrefix(raw, "sym:"):
		name := strings.TrimPrefix(raw, "sym:")
		if !singleContentWord(name) {
			return fmt.Errorf("sym: accepts a single identifier only")
		}
		pq.Content = append(pq.Content, contentLeaf{Kind: contentLeafTerm, Text: name, WholeWord: true, Sym: true})
		if !contentSearchEnabled() {
			return contentUnavailableError()
		}
		return nil
	case strings.HasPrefix(raw, "content~"):
		colon := strings.IndexByte(raw, ':')
		if colon < 0 || !strings.HasPrefix(raw, "content~") {
			return fmt.Errorf("invalid fuzzy content query %q; use content~:word or content~2:word", raw)
		}
		distance := -1
		distText := strings.TrimPrefix(raw[:colon], "content~")
		if distText != "" {
			n, err := strconv.Atoi(distText)
			if err != nil || n < 0 || n > 2 {
				return fmt.Errorf("invalid fuzzy content distance %q; expected 0, 1, or 2", distText)
			}
			distance = n
		}
		leaf, err := parseContentLeaf(raw[colon+1:])
		if err != nil {
			return err
		}
		if leaf.Kind != contentLeafTerm || !singleContentWord(leaf.Text) {
			return fmt.Errorf("content~ accepts a single word (letters, digits, and underscore) only")
		}
		leaf.Fuzzy = true
		leaf.FuzzyDistance = distance
		pq.Content = append(pq.Content, leaf)
		if !contentSearchEnabled() {
			return contentUnavailableError()
		}
		return nil
	case isUnknownFilterToken(raw):
		return fmt.Errorf("unsupported filter %q; supported: path: parent: ext: dir: glob: regex: type: case: size: dm: attrib: git: near: in: content: (needs SEEKFS_CONTENT_SEARCH=1) sort:size sort:modified sort:extension sort:type sort:path (and !term, a|b)", raw)
	case looksLikeImplicitFilenameGlob(raw):
		norm := normalizeCase(raw, pq.CaseSensitive)
		if exts, ok := braceGlobExts(norm); ok {
			appendExtOrGroup(pq, exts)
			return nil
		}
		pq.Globs = append(pq.Globs, norm)
		pq.GlobAlts = append(pq.GlobAlts, expandBraceAlternatives(norm))
		if strings.Contains(raw, "{") {
			for _, lit := range globCommonLiterals(norm, pq.CaseSensitive) {
				pq.Terms = append(pq.Terms, lit)
			}
		}
	default:
		fuzzyMarked := false
		trimmed := raw
		if strings.HasSuffix(trimmed, "~") && len(trimmed) > 1 {
			trimmed = strings.TrimSuffix(trimmed, "~")
			fuzzyMarked = trimmed != ""
		}
		// A bare glob path (`src/**/*.{c,h}`, `**/OWNERS`) routes to the
		// path-glob lane instead of being split into path terms.
		if isBareGlobPattern(trimmed) {
			return addBareGlobPattern(pq, trimmed)
		}
		t := trimmed
		quoted := false
		if isQuotedQueryToken(t) {
			quoted = true
			t = t[1 : len(t)-1]
		}
		// Name operators (fsearch syntax): `'exact` (whole name equals),
		// `^prefix` (name starts with), `suffix$` (name ends with). They
		// always constrain the file NAME, in filename and -path modes
		// alike; the stripped text also joins Terms so posting lanes
		// engage, and verification enforces the anchor.
		exact, prefix, suffix := false, false, false
		if len(t) > 1 && t[0] == '\'' {
			exact = true
			t = t[1:]
		}
		if !exact && len(t) > 1 && t[0] == '^' {
			prefix = true
			t = t[1:]
		}
		if !exact && len(t) > 1 && t[len(t)-1] == '$' {
			suffix = true
			t = t[:len(t)-1]
		}
		if (exact || prefix || suffix) && strings.ContainsAny(t, `/\`) {
			// Anchors name the file, not the path: a remainder with
			// separators degrades to plain terms.
			t = trimmed
			quoted = false
			if isQuotedQueryToken(t) {
				quoted = true
				t = t[1 : len(t)-1]
			}
			exact, prefix, suffix = false, false, false
		}
		terms := queryPlainTerms(t, pq.CaseSensitive, pq.MatchPath)
		if fuzzyMarked && len(terms) == 1 {
			pq.Fuzzy = true
		}
		pq.Terms = append(pq.Terms, terms...)
		if quoted {
			if pq.QuotedTerms == nil {
				pq.QuotedTerms = make(map[string]struct{})
			}
			for _, term := range terms {
				pq.QuotedTerms[term] = struct{}{}
			}
		}
		if (exact || prefix || suffix) && len(terms) == 1 {
			switch {
			case exact:
				pq.ExactTerms = append(pq.ExactTerms, terms[0])
			case prefix:
				pq.Prefixes = append(pq.Prefixes, terms[0])
			case suffix:
				pq.Suffixes = append(pq.Suffixes, terms[0])
			}
			if prefix && suffix {
				// `^foo$` anchors both ends; record the suffix too.
				pq.Suffixes = append(pq.Suffixes, terms[0])
			}
		}
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
	dst.PathGlobs = append(dst.PathGlobs, src.PathGlobs...)
	// GlobAlts/PathGlobAlts parallel Globs/PathGlobs by index: pad both
	// sides so the verifier pairs each pattern with its own expansions.
	for len(dst.GlobAlts) < len(dst.Globs) {
		dst.GlobAlts = append(dst.GlobAlts, nil)
	}
	for len(src.GlobAlts) < len(src.Globs) {
		src.GlobAlts = append(src.GlobAlts, nil)
	}
	dst.GlobAlts = append(dst.GlobAlts, src.GlobAlts...)
	for len(dst.PathGlobAlts) < len(dst.PathGlobs) {
		dst.PathGlobAlts = append(dst.PathGlobAlts, nil)
	}
	for len(src.PathGlobAlts) < len(src.PathGlobs) {
		src.PathGlobAlts = append(src.PathGlobAlts, nil)
	}
	dst.PathGlobAlts = append(dst.PathGlobAlts, src.PathGlobAlts...)
	// RegexMasks parallels Regexps by index: pad both sides with nils so
	// the entry gate pairs each mask with its own regex.
	for len(dst.RegexMasks) < len(dst.Regexps) {
		dst.RegexMasks = append(dst.RegexMasks, nil)
	}
	for len(src.RegexMasks) < len(src.Regexps) {
		src.RegexMasks = append(src.RegexMasks, nil)
	}
	dst.RegexMasks = append(dst.RegexMasks, src.RegexMasks...)
	dst.ExactTerms = append(dst.ExactTerms, src.ExactTerms...)
	dst.Prefixes = append(dst.Prefixes, src.Prefixes...)
	dst.Suffixes = append(dst.Suffixes, src.Suffixes...)
	dst.InUnders = append(dst.InUnders, src.InUnders...)
	dst.NotUnders = append(dst.NotUnders, src.NotUnders...)
	if len(src.QuotedTerms) > 0 {
		if dst.QuotedTerms == nil {
			dst.QuotedTerms = make(map[string]struct{}, len(src.QuotedTerms))
		}
		for k := range src.QuotedTerms {
			dst.QuotedTerms[k] = struct{}{}
		}
	}
	dst.Regexps = append(dst.Regexps, src.Regexps...)
	dst.RegexTerms = append(dst.RegexTerms, src.RegexTerms...)
	dst.Parents = append(dst.Parents, src.Parents...)
	dst.SizeFilters = append(dst.SizeFilters, src.SizeFilters...)
	dst.DateFilters = append(dst.DateFilters, src.DateFilters...)
	dst.AttrFilters = append(dst.AttrFilters, src.AttrFilters...)
	dst.GitFilters = append(dst.GitFilters, src.GitFilters...)
	dst.Content = append(dst.Content, src.Content...)
	dst.Features = append(dst.Features, src.Features...)
	if src.Near != "" && dst.Near == "" {
		dst.Near = src.Near
	}
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

// isQuotedQueryToken reports whether raw is a double-quoted phrase token:
// it starts with `"` and ends with `"` (at least the open/close pair).
// Negation prefixes are NOT stripped here; callers handle `!"..."` by
// keeping the whole token intact through the quote-aware splitter, so the
// negation branch sees the quotes.
func isQuotedQueryToken(raw string) bool {
	return len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"'
}

// splitNameQueryTokens splits a name query on whitespace like strings.Fields,
// but keeps double-quoted spans together (the quotes stay on the token so
// applyQueryToken can mark the phrase quoted). Without any `"` the result
// is exactly strings.Fields, so unquoted queries are byte-identical.
func splitNameQueryTokens(query string) []string {
	if !strings.Contains(query, `"`) {
		return strings.Fields(query)
	}
	var out []string
	var b strings.Builder
	inQuote := false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range query {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case unicodeIsSpace(r) && !inQuote:
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

func unicodeIsSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

// isBareGlobPattern reports whether a default-branch token is a glob path
// rather than a plain term: brace sets with a real alternation always are
// (`*.{c,h}`, `{a,b}*.cc`), and wildcard tokens (`*?[]`, illegal in Windows
// file names) with a separator or `**` are (`src/**/*.cc`). A lone brace
// pair (`{GUID}`) or unbalanced brace stays a literal substring term.
func isBareGlobPattern(raw string) bool {
	if hasBraceAlternation(raw) {
		return true
	}
	if !strings.ContainsAny(raw, "*?[") {
		return false
	}
	return strings.ContainsAny(raw, `/\`) || strings.Contains(raw, "**")
}

// hasBraceAlternation reports whether s holds a {...} group with a
// top-level comma (a real set, not literal braces).
func hasBraceAlternation(s string) bool {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return false
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return false
			}
		case ',':
			if depth == 1 {
				return true
			}
		}
	}
	return false
}

// appendExtOrGroup ANDs an ext OR group (one alternative per extension)
// into the query. Used for brace globs over `*.ext` sets, which are exactly
// an extension disjunction. Built manually (not by re-parsing `ext:a|b`)
// because `|` prefixes do not distribute: `ext:c|h` parses as ext:c OR
// bare term h.
func appendExtOrGroup(pq *parsedQuery, exts []string) {
	group := make([]parsedQuery, 0, len(exts))
	for _, e := range exts {
		group = append(group, parsedQuery{MatchPath: pq.MatchPath, CaseSensitive: pq.CaseSensitive, Exts: []string{e}})
	}
	pq.OrGroups = append(pq.OrGroups, group)
}

// addBareGlobPattern folds a bare glob token into the query: path patterns
// join PathGlobs (with their brace-common literals as dir: filters so path
// posting lanes engage), plain filename patterns join Globs. All-`*.ext`
// brace sets ride the ext postings as ext:a|b instead.
func addBareGlobPattern(pq *parsedQuery, raw string) error {
	norm := normalizeCase(raw, pq.CaseSensitive)
	if isPathGlobPattern(raw) {
		pq.PathGlobs = append(pq.PathGlobs, norm)
		pq.PathGlobAlts = append(pq.PathGlobAlts, expandBraceAlternatives(norm))
		for _, lit := range globCommonLiterals(norm, pq.CaseSensitive) {
			pq.Dirs = append(pq.Dirs, lit)
		}
		if exts, ok := bracePathGlobExts(norm); ok {
			appendExtOrGroup(pq, exts)
		}
		return nil
	}
	if exts, ok := braceGlobExts(norm); ok {
		appendExtOrGroup(pq, exts)
		return nil
	}
	pq.Globs = append(pq.Globs, norm)
	pq.GlobAlts = append(pq.GlobAlts, expandBraceAlternatives(norm))
	for _, lit := range globCommonLiterals(norm, pq.CaseSensitive) {
		pq.Terms = append(pq.Terms, lit)
	}
	return nil
}

// mergeInUnders folds `in:` roots and the `--under` flag into one effective
// Under: every root must contain the result, so nested roots collapse to
// the deepest and disjoint roots make the query impossible (no scan).
func mergeInUnders(pq *parsedQuery) {
	if len(pq.InUnders) == 0 {
		return
	}
	roots := append([]string(nil), pq.InUnders...)
	if pq.Under != "" {
		roots = append(roots, pq.Under)
	}
	deepest := roots[0]
	for _, r := range roots[1:] {
		switch {
		case pathUnder(deepest, r):
			// deepest already under r.
		case pathUnder(r, deepest):
			deepest = r
		default:
			pq.Impossible = true
			return
		}
	}
	pq.Under = deepest
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
		// Brace sets are OR: only literals common to every expansion
		// may feed posting lanes (globCommonLiterals intersects them).
		for _, term := range globCommonLiterals(glob, caseSensitive) {
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
		// Brace sets choose between extensions (OR); no single ext is
		// required, so they contribute nothing here.
		if strings.Contains(glob, "{") {
			continue
		}
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

// entryMatchesScalar evaluates a query's name/path/scalar constraints only. It
// is the shared body of entryMatches and entryMatchesWithContent; OR/NOT groups
// recurse in the callers so content-aware verification can apply its own
// per-alternative (joint) semantics.
func entryMatchesScalar(entry Entry, pq parsedQuery, matchPath bool) bool {
	for _, leaf := range pq.Features {
		if _, ok := leaf.matches[entry.FRN]; !ok {
			return false
		}
	}
	// Index-reconstructed paths are already clean; cleanPathIfNeeded skips
	// the Clean allocation for them while preserving exact semantics for
	// anything else (overlay/filesystem entries).
	path := cleanPathIfNeeded(entry.Path)
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
	for _, root := range pq.NotUnders {
		if pathUnder(path, root) {
			return false
		}
	}
	if pq.Exists {
		if _, err := contentStatExists(path); err != nil {
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
	if !containsAllFolded(haystack, nil, pq.Terms, pq) {
		return false
	}
	foldedName := ""
	if !matchNameAnchors(cmpName, &foldedName, pq) {
		return false
	}
	for _, ext := range pq.Exts {
		actual := strings.TrimPrefix(filepath.Ext(name), ".")
		if normalizeCase(actual, pq.CaseSensitive) != ext {
			return false
		}
	}
	foldedPath := ""
	for _, dir := range pq.Dirs {
		if !matchFoldedSubstring(cmpPath, &foldedPath, dir, pq.CaseSensitive, false) {
			return false
		}
	}
	for gi, glob := range pq.Globs {
		if matchNameAlts(globAltsFor(pq.GlobAlts, gi, glob), cmpName) {
			continue
		}
		// Accent-folded retry for non-ASCII patterns or names.
		if pq.CaseSensitive || (!nameNeedsFold(glob) && !nameNeedsFold(cmpName)) {
			return false
		}
		fp, fn := glob, cmpName
		if nameNeedsFold(glob) {
			fp = foldAccentLower(glob)
		}
		if nameNeedsFold(cmpName) {
			if foldedName == "" {
				foldedName = foldAccentLower(cmpName)
			}
			fn = foldedName
		}
		if !matchNameGlob(fp, fn) {
			return false
		}
	}
	for gi, glob := range pq.PathGlobs {
		if matchPathAlts(globAltsFor(pq.PathGlobAlts, gi, glob), path, pq.CaseSensitive) {
			continue
		}
		if pq.CaseSensitive || (!nameNeedsFold(glob) && !nameNeedsFold(path)) {
			return false
		}
		fp, fn := glob, path
		if nameNeedsFold(glob) {
			fp = foldAccentLower(glob)
		}
		if nameNeedsFold(path) {
			fn = foldAccentLower(path)
		}
		if !matchPathGlob(fp, fn, pq.CaseSensitive) {
			return false
		}
	}
	for ri, re := range pq.Regexps {
		// Letter-bitmap prefilter: a match must contain every literal run
		// of some provably-required disjunct. memoMaskOf is case-blind, so
		// the subset test is necessary under (?i) and case-sensitive
		// patterns alike; it only skips the MatchString below.
		if ri < len(pq.RegexMasks) && len(pq.RegexMasks[ri]) > 0 {
			pm := memoMaskOf(path)
			possible := false
			for _, dm := range pq.RegexMasks[ri] {
				if dm&^pm == 0 {
					possible = true
					break
				}
			}
			if !possible {
				return false
			}
		}
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
	if !pq.gitFiltersMatch(entry) {
		return false
	}
	return attrFiltersMatch(entry.Mode, pq.AttrFilters)
}

func entryMatches(entry Entry, pq parsedQuery, matchPath bool) bool {
	if !entryMatchesScalar(entry, pq, matchPath) {
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
		if queryHasAnyContentLeaf(neg) {
			// Content exclusion is applied by entryMatchesWithContent, which
			// knows the per-document results. entryMatches only sees name/scalar
			// data and must not drop every entry here.
			continue
		}
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

// filterPathNormCache memoizes normalizeFilterPath for absolute paths.
// Under/bias roots are re-normalized per candidate inside pathUnder; the
// normalization of an absolute path is cwd-independent, so caching it turns
// two Abs+Clean calls per candidate into a map lookup. Relative paths still
// go through normalizeFilterPath every time (their Abs depends on cwd).
//
// The resident service sees a new root per typed `under:` prefix, so the
// cache is bounded: past filterPathNormCacheMax entries it starts over.
var filterPathNormCache sync.Map // map[string]string

var filterPathNormCacheLen atomic.Int64

const filterPathNormCacheMax = 1024

func normalizeFilterPathCached(path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return normalizeFilterPath(path)
	}
	if v, ok := filterPathNormCache.Load(path); ok {
		return v.(string)
	}
	norm := normalizeFilterPath(path)
	if filterPathNormCacheLen.Add(1) > filterPathNormCacheMax {
		filterPathNormCache.Clear()
		filterPathNormCacheLen.Store(1)
	}
	filterPathNormCache.Store(path, norm)
	return norm
}

// pathLooksClean reports whether filepath.Clean(path) is the identity, without
// allocating. Anything doubtful returns false and the caller runs Clean.
// Windows Clean only reacts to forward slashes, duplicate or trailing
// separators, and exact "." / ".." elements, so the absence of all of those
// (plus the verbatim-prefix guard) proves Clean is a no-op.
func pathLooksClean(path string) bool {
	if path == "" {
		// Clean("") is ".", not the identity.
		return false
	}
	n := len(path)
	// A bare drive-relative volume (`C:`) cleans to `C:.`, not the identity.
	if n == 2 && path[1] == ':' {
		return false
	}
	// Verbatim/extended prefixes go through Clean's special cases; let Clean
	// handle them rather than reproduce them here.
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return false
	}
	unc := n >= 2 && path[0] == '\\' && path[1] == '\\'
	for i := 0; i < n; i++ {
		c := path[i]
		if c == '/' {
			return false
		}
		if c == '\\' {
			// Duplicate separator (except the UNC lead) or a trailing
			// separator past a bare root: Clean would collapse/strip it.
			if i+1 < n && path[i+1] == '\\' {
				if !(i == 0 && unc) {
					return false
				}
			} else if i+1 == n {
				// Trailing separator: Clean keeps it only on a bare
				// root (drive `E:\` or UNC `\\host\share\`).
				if !isBareRootPath(path) {
					return false
				}
			}
		} else if c == '.' {
			// An exact "." or ".." element: Clean would resolve it.
			prev := i == 0 || path[i-1] == '\\' || path[i-1] == '/'
			if prev {
				next := i + 1
				if next >= n || path[next] == '\\' || path[next] == '/' {
					return false
				}
				if path[next] == '.' {
					after := next + 1
					if after >= n || path[after] == '\\' || path[after] == '/' {
						return false
					}
				}
			}
		}
	}
	return true
}

// isBareRootPath reports whether path is a bare filesystem root whose
// trailing separator survives Clean: a drive root like `E:\` or a UNC root
// like `\\host\share\`.
func isBareRootPath(path string) bool {
	if len(path) == 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
		return true
	}
	if len(path) >= 2 && path[0] == '\\' && path[1] == '\\' {
		rest := path[2:]
		// UNC root: exactly server + share and nothing deeper.
		parts := strings.Split(strings.TrimRight(rest, `\/`), `\`)
		if len(parts) == 2 {
			return true
		}
		// Tolerate forward slashes inside the UNC body the same way.
		parts = strings.Split(strings.TrimRight(rest, `\/`), `/`)
		if len(parts) == 2 {
			return true
		}
	}
	return false
}

// cleanPathIfNeeded is filepath.Clean that returns the input unchanged (no
// allocation) when Clean would be the identity. Index-reconstructed paths are
// built from components and are almost always already clean.
func cleanPathIfNeeded(path string) string {
	if pathLooksClean(path) {
		return path
	}
	return filepath.Clean(path)
}

func pathUnder(path, root string) bool {
	// Fast path for the hot case (both absolute, e.g. index entries tested
	// against a normalized under/bias root): an exact separator-bounded
	// prefix of two clean absolute paths is exactly what Rel would report
	// as contained, without Abs/Clean/Rel per candidate. Anything else
	// (case-only differences, relative paths) falls through to the exact
	// slow path so semantics never change.
	if filepath.IsAbs(path) && filepath.IsAbs(root) {
		// For absolute inputs normalizeFilterPath is Abs+Clean == Clean,
		// so these are exactly the normalized forms the slow path would
		// compare; the Rel check below then decides with identical
		// semantics.
		normRoot := normalizeFilterPathCached(root)
		clean := cleanPathIfNeeded(path)
		if strings.EqualFold(clean, normRoot) {
			return true
		}
		if len(clean) > len(normRoot) && strings.HasPrefix(clean, normRoot) {
			if normRoot[len(normRoot)-1] == '\\' || normRoot[len(normRoot)-1] == '/' {
				// A separator-terminated root (drive/UNC root) contains
				// every longer path with that prefix.
				return true
			}
			if rest := clean[len(normRoot):]; rest[0] == '\\' || rest[0] == '/' {
				return true
			}
		}
		return pathUnderCleaned(clean, normRoot)
	}
	return pathUnderSlow(path, root)
}

// pathUnderCleaned is the Rel containment check on already-normalized
// absolute paths.
func pathUnderCleaned(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != "" && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func pathUnderSlow(path, root string) bool {
	path = normalizeFilterPath(path)
	root = normalizeFilterPathCached(root)
	if strings.EqualFold(path, root) {
		return true
	}
	return pathUnderCleaned(path, root)
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

// queryNowFunc is the clock parseDateFilter reads for relative specs
// (today/yesterday/thisweek/durations). A var so tests can pin "now"
// deterministically instead of flaking across midnight.
var queryNowFunc = time.Now

// parseDateFilter parses an Everything-style dm: constraint. Supported specs:
// "today", "yesterday", "thisweek", "lastweek", a relative duration like "24h"
// or "7d", or an absolute date (YYYY-MM-DD) / RFC3339 timestamp meaning
// "modified on or after".
func parseDateFilter(spec string) (dateFilter, error) {
	spec = strings.ToLower(strings.TrimSpace(spec))
	if spec == "" {
		return dateFilter{}, errors.New("empty dm: filter")
	}
	now := queryNowFunc()
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

// compactPathContainsAllFolded is compactPathContainsAll under the query
// folding rule, in a single parent-chain walk per term: unfolded bytes
// first (byte-identical to compactPathContainsAll on ASCII input), then the
// accent-folded comparison for fold-eligible terms. Quoted and
// case-sensitive terms stay byte-exact. Pure-ASCII queries over pure-ASCII
// chains pay one extra branch per name.
func (idx *Index) compactPathContainsAllFolded(i int, terms []string, pq parsedQuery) bool {
	for _, term := range terms {
		if !idx.compactPathContainsTermFolded(i, term, pq.CaseSensitive || isQuotedTerm(pq, term) || foldOffKillSwitch) {
			return false
		}
	}
	return true
}

func (idx *Index) compactPathContainsTermFolded(i int, term string, exact bool) bool {
	foldedTerm := term
	termNeedsFold := false
	if !exact && nameNeedsFold(term) {
		foldedTerm = foldAccentLower(term)
		termNeedsFold = true
	}
	if idx.Volume != "" {
		if containsFoldASCII(idx.Volume, term) {
			return true
		}
		if !exact && nameNeedsFold(idx.Volume) && strings.Contains(foldAccentLower(idx.Volume), foldedTerm) {
			return true
		}
	}
	cur := i
	for depth := 0; depth < 1024; depth++ {
		if cur < 0 || cur >= idx.compactRecordCount() {
			return false
		}
		parent, name := idx.compactParentNameAt(cur)
		if containsFoldASCII(name, term) {
			return true
		}
		if !exact && (termNeedsFold || nameNeedsFold(name)) {
			foldedName := name
			if nameNeedsFold(name) {
				foldedName = foldAccentLower(name)
			}
			if strings.Contains(foldedName, foldedTerm) {
				return true
			}
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
	unresolved := false
	cur := i
	for depth := 0; depth < 1024; depth++ {
		if path, ok := cache[cur]; ok {
			if path == "" {
				cache[i] = ""
				return ""
			}
			for p := len(parts) - 1; p >= 0; p-- {
				path += `\` + parts[p]
			}
			cache[i] = path
			return path
		}
		if cur < 0 || cur >= idx.compactRecordCount() {
			unresolved = true
			break
		}
		if _, ok := seen[cur]; ok {
			// A self-parented root record (or a cycle) ends the walk at the
			// root, as before: it is not an orphan.
			break
		}
		seen[cur] = struct{}{}
		parent, name := idx.compactParentNameAt(cur)
		if name != "." {
			parts = append(parts, name)
		}
		if parent < 0 {
			// A record whose nonzero parent FRN is absent is an orphan (the
			// parent was dropped by an earlier fold). Do not report it as a
			// child of the volume root; that fabricated path made orphaned
			// worktree children appear as E:\.git.
			rec := idx.compactRecord(cur)
			if len(idx.Roots) > 0 && rec.ParentFRN != 0 && rec.ParentFRN != rec.FRN {
				unresolved = true
			}
			break
		}
		if int(parent) >= idx.compactRecordCount() {
			unresolved = true
			break
		}
		cur = int(parent)
	}
	if unresolved {
		cache[i] = ""
		return ""
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
		} else if rootName != "" && p == len(parts)-1 && parts[p] == rootName {
			// The chain top names the walk root itself: it is already the
			// path prefix, so re-appending it would double it. This holds
			// for the root record too (single-element chain): its path is
			// the root, and every cache entry below it stays consistent
			// no matter which ancestor a later lookup hits first.
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
	unresolved := false
	for depth := 0; depth < 1024; depth++ {
		if cur < 0 || cur >= len(paths) {
			unresolved = true
			break
		}
		if computed[cur] {
			path := paths[cur]
			if path == "" {
				unresolved = true
				break
			}
			for p := len(parts) - 1; p >= 0; p-- {
				path += `\` + strings.ToLower(parts[p])
			}
			paths[i] = path
			computed[i] = true
			return parts
		}
		if seen[cur] == gen {
			// Self-parented root or cycle: ends at the root, not an orphan.
			break
		}
		seen[cur] = gen
		rec := idx.compactRecord(cur)
		if rec.Name != "." {
			parts = append(parts, rec.Name)
		}
		if rec.Parent < 0 {
			if len(idx.Roots) > 0 && rec.ParentFRN != 0 && rec.ParentFRN != rec.FRN {
				unresolved = true
			}
			break
		}
		if int(rec.Parent) >= len(paths) {
			unresolved = true
			break
		}
		cur = int(rec.Parent)
	}
	if unresolved {
		paths[i] = ""
		computed[i] = true
		return parts
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

// isQuotedTerm reports whether a normalized term came from a double-quoted
// phrase: it matches literally, with no accent folding (exact-match
// semantics, like `case:`).
func isQuotedTerm(pq parsedQuery, term string) bool {
	if len(pq.QuotedTerms) == 0 {
		return false
	}
	_, ok := pq.QuotedTerms[term]
	return ok
}

// matchFoldedSubstring reports whether term occurs in haystack. Unless exact
// (case-sensitive or quoted), both sides fold accents: an ASCII needle also
// matches across removed marks (`resume` in `résumé`), and a non-ASCII
// needle matches folded ASCII text. folded caches foldAccentLower(haystack)
// across terms; pass a fresh variable per haystack.
//
// Cost discipline (hot path): the SIMD substring check runs first, so hits
// cost exactly one Contains; only misses pay the ASCII flag scan, and only
// non-ASCII haystacks pay for NFD. A fused single-pass scan measured 2-3x
// slower than memmem here and was rejected.
func matchFoldedSubstring(haystack string, folded *string, term string, caseSensitive, quoted bool) bool {
	if caseSensitive || quoted || foldOffKillSwitch {
		return strings.Contains(haystack, term)
	}
	if nameNeedsFold(term) {
		foldedTerm := foldAccentLower(term)
		if strings.Contains(haystack, foldedTerm) {
			return true
		}
		if !nameNeedsFold(haystack) {
			return false
		}
		if *folded == "" {
			*folded = foldAccentLower(haystack)
		}
		return strings.Contains(*folded, foldedTerm)
	}
	if strings.Contains(haystack, term) {
		return true
	}
	if !nameNeedsFold(haystack) {
		return false
	}
	if *folded == "" {
		*folded = foldAccentLower(haystack)
	}
	return strings.Contains(*folded, term)
}

// containsAllFolded is containsAll under the query folding rule.
func containsAllFolded(haystack string, folded *string, terms []string, pq parsedQuery) bool {
	var local string
	if folded == nil {
		folded = &local
	}
	for _, term := range terms {
		if !matchFoldedSubstring(haystack, folded, term, pq.CaseSensitive, isQuotedTerm(pq, term)) {
			return false
		}
	}
	return true
}

// matchNameAnchors enforces `'exact`/`^prefix`/`suffix$` against the file
// NAME (basename) in filename and -path modes alike, under the query
// folding rule (exact bytes for case-sensitive/quoted).
func matchNameAnchors(cmpName string, folded *string, pq parsedQuery) bool {
	if len(pq.ExactTerms)+len(pq.Prefixes)+len(pq.Suffixes) == 0 {
		return true
	}
	var local string
	if folded == nil {
		folded = &local
	}
	for _, term := range pq.ExactTerms {
		if !matchAnchored(cmpName, folded, term, pq, 0) {
			return false
		}
	}
	for _, term := range pq.Prefixes {
		if !matchAnchored(cmpName, folded, term, pq, 1) {
			return false
		}
	}
	for _, term := range pq.Suffixes {
		if !matchAnchored(cmpName, folded, term, pq, 2) {
			return false
		}
	}
	return true
}

// matchAnchored compares one anchor term: kind 0 equality, 1 prefix,
// 2 suffix.
func matchAnchored(cmpName string, folded *string, term string, pq parsedQuery, kind int) bool {
	cmp := func(hay, needle string) bool {
		switch kind {
		case 0:
			return hay == needle
		case 1:
			return strings.HasPrefix(hay, needle)
		default:
			return strings.HasSuffix(hay, needle)
		}
	}
	if pq.CaseSensitive || isQuotedTerm(pq, term) || foldOffKillSwitch {
		return cmp(cmpName, term)
	}
	foldedTerm := term
	if nameNeedsFold(term) {
		foldedTerm = foldAccentLower(term)
	}
	if cmp(cmpName, foldedTerm) {
		return true
	}
	if !nameNeedsFold(cmpName) {
		return false
	}
	if *folded == "" {
		*folded = foldAccentLower(cmpName)
	}
	return cmp(*folded, foldedTerm)
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
