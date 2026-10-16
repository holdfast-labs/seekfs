package main

// ripgrep-style line output for content queries (`seekfs search --lines`).
// It runs client-side after the result set is known: each hit file is re-read
// from disk and its lines are tested with the same matcher the query verified
// with (case policy incl. smart case, phrases, regex with per-line ^/$), so the
// service protocol is unchanged.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"unicode/utf8"
)

// contentLineMaxBytes caps one printed line; longer lines (minified files)
// are cut with an ellipsis so a single hit cannot flood the terminal.
const contentLineMaxBytes = 500

// contentLineOutput carries the --lines options from the CLI to the printers.
type contentLineOutput struct {
	maxPerFile int
	before     int
	after      int
}

// contentOutputLine is one printed line: a match, or context around one.
type contentOutputLine struct {
	Line  int    `json:"line"`
	Text  string `json:"text"`
	Match bool   `json:"match"`
}

// contentMatchingLines returns the matching lines of raw (lines matching any
// of the positive leaves, at most maxPerFile) plus before/after context lines,
// in file order. Phrases match within one line; they never span a newline.
func contentMatchingLines(raw []byte, m *contentLeafMatcher, positive []contentLeaf, maxPerFile, before, after int) []contentOutputLine {
	if m == nil || len(positive) == 0 {
		return nil
	}
	if maxPerFile <= 0 {
		maxPerFile = 10
	}
	before, after = max(before, 0), max(after, 0)
	lines := bytes.Split(raw, []byte{'\n'})
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	var hits []int
	for i := range lines {
		lines[i] = bytes.TrimSuffix(lines[i], []byte{'\r'})
		if len(hits) < maxPerFile && contentLineMatches(m, lines[i], positive) {
			hits = append(hits, i)
		}
	}
	var out []contentOutputLine
	next := 0 // first line index not yet emitted
	for h, hit := range hits {
		lo := max(hit-before, next)
		hi := min(hit+after, len(lines)-1)
		if h+1 < len(hits) {
			// Context after this hit stops before the next hit, which
			// emits its own lines.
			hi = min(hi, hits[h+1]-1)
		}
		for i := lo; i <= hi; i++ {
			out = append(out, contentOutputLine{Line: i + 1, Text: contentLineText(lines[i]), Match: i == hit})
		}
		next = hi + 1
	}
	return out
}

func contentLineMatches(m *contentLeafMatcher, line []byte, positive []contentLeaf) bool {
	var folded []byte
	if !m.caseSensitive {
		folded = contentFoldAccent(line)
	}
	for _, leaf := range positive {
		if leaf.Sym {
			continue
		}
		if leaf.Fuzzy {
			word := string(contentFoldAccent([]byte(leaf.Text)))
			tau := contentFuzzyGrepDistance(leaf)
			for term := range contentTermsOf(line) {
				if damerauLevenshteinBounded([]rune(string(contentFoldAccent([]byte(term)))), []rune(word), tau) <= tau {
					return true
				}
			}
			continue
		}
		if m.match(line, folded, leaf) {
			return true
		}
	}
	return false
}

func contentLineText(b []byte) string {
	if len(b) <= contentLineMaxBytes {
		return string(b)
	}
	cut := contentLineMaxBytes
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return string(b[:cut]) + "…"
}

// contentFileLines reads path and returns its output lines. ok is false when
// the file cannot be read or is not UTF-8 text (binary, or an extracted format
// such as PDF/DOCX); callers then fall back to the result's snippet.
func contentFileLines(path string, m *contentLeafMatcher, positive []contentLeaf, o contentLineOutput) ([]contentOutputLine, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, contentExtractMaxTextBytes))
	if err != nil || bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		return nil, false
	}
	return contentMatchingLines(raw, m, positive, o.maxPerFile, o.before, o.after), true
}

// contentLinePrinter prints ripgrep-style lines for content hits:
// `path:LINE:text` for matches, `path-LINE-text` for context, and `--`
// between non-adjacent groups when context is requested.
type contentLinePrinter struct {
	m        *contentLeafMatcher
	positive []contentLeaf
	o        contentLineOutput
	printed  bool // a group was printed (for the `--` separator)
}

// cliContentLinePrinter returns the --lines printer for opts, or nil when
// --lines was not requested or the query has no positive content leaf.
func cliContentLinePrinter(opts queryOptions) *contentLinePrinter {
	if opts.lineOutput == nil {
		return nil
	}
	if !contentSearchEnabled() {
		// The service answered a content query, so its content leaves are
		// valid; this short-lived CLI process may simply not have content
		// selected locally. Parse them anyway to match lines client-side.
		selected := true
		servicePluginContentSelection.Store(&selected)
	}
	return newContentLinePrinter(opts, *opts.lineOutput)
}

// newContentLinePrinter returns nil when the query has no positive content
// leaf (filename-only queries print paths as before).
func newContentLinePrinter(opts queryOptions, o contentLineOutput) *contentLinePrinter {
	pq, err := parseQuery(opts)
	if err != nil {
		return nil
	}
	positive := contentPositiveLeaves(pq)
	if len(positive) == 0 {
		return nil
	}
	return &contentLinePrinter{m: newContentLeafMatcher(pq), positive: positive, o: o}
}

func (p *contentLinePrinter) lines(path string) ([]contentOutputLine, bool) {
	return contentFileLines(path, p.m, p.positive, p.o)
}

func (p *contentLinePrinter) print(w io.Writer, path, snippet string) {
	lines, ok := p.lines(path)
	if !ok || len(lines) == 0 {
		// Not line-addressable (binary/extracted, unreadable, or the match
		// spans lines): keep the hit visible with its snippet.
		p.separate(w)
		if snippet != "" {
			fmt.Fprintf(w, "%s:0:%s\n", path, snippet)
		} else {
			fmt.Fprintln(w, path)
		}
		return
	}
	for i, l := range lines {
		if i == 0 || l.Line != lines[i-1].Line+1 {
			p.separate(w)
		}
		sep := '-'
		if l.Match {
			sep = ':'
		}
		fmt.Fprintf(w, "%s%c%d%c%s\n", path, sep, l.Line, sep, l.Text)
	}
}

func (p *contentLinePrinter) separate(w io.Writer) {
	if p.printed && (p.o.before > 0 || p.o.after > 0) {
		fmt.Fprintln(w, "--")
	}
	p.printed = true
}

// annotateJSON attaches the --lines output to JSON results (additive
// `lines` field; results that are not line-addressable keep only snippet).
func (p *contentLinePrinter) annotateJSON(results []jsonResult) {
	for i := range results {
		if lines, ok := p.lines(results[i].Path); ok {
			results[i].Lines = lines
		}
	}
}
