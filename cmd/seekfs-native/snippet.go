package main

import (
	"sort"
	"strings"
)

// Snippet highlight segmentation for content results. Ports
// contentHighlightTerms and highlightSegments from
// cmd/seekfs/ui_frontend/query.js so both UIs emphasize the same terms.

// contentHighlightTerms returns the literal content: terms of query, for
// highlighting in snippets. Regex terms (content:/.../) are skipped.
func contentHighlightTerms(query string) []string {
	var terms []string
	for _, token := range queryTokens(query) {
		stripped := strings.TrimLeft(token, "!-")
		if len(stripped) < 9 {
			continue
		}
		if !strings.EqualFold(stripped[:8], "content:") {
			continue
		}
		value := stripped[8:]
		if strings.HasPrefix(value, "/") {
			continue
		}
		value = strings.Trim(value, `"`)
		if value != "" {
			terms = append(terms, value)
		}
	}
	return terms
}

// Segment is one run of a snippet: either plain text or an emphasized match.
type Segment struct {
	Text  string
	Match bool
}

// highlightSegments splits text into segments around case-insensitive
// occurrences of any term, longest term first.
func highlightSegments(text string, terms []string) []Segment {
	wanted := make([]string, 0, len(terms))
	seen := map[string]struct{}{}
	for _, t := range terms {
		if t == "" {
			continue
		}
		lower := strings.ToLower(t)
		if _, dup := seen[lower]; dup {
			continue
		}
		seen[lower] = struct{}{}
		wanted = append(wanted, lower)
	}
	sort.Slice(wanted, func(i, j int) bool { return len(wanted[i]) > len(wanted[j]) })
	if text == "" || len(wanted) == 0 {
		if text == "" {
			return nil
		}
		return []Segment{{Text: text}}
	}
	lower := strings.ToLower(text)
	var out []Segment
	pos, plain := 0, 0
	for pos < len(text) {
		var hit string
		for _, t := range wanted {
			if strings.HasPrefix(lower[pos:], t) {
				hit = t
				break
			}
		}
		if hit != "" {
			if pos > plain {
				out = append(out, Segment{Text: text[plain:pos]})
			}
			out = append(out, Segment{Text: text[pos : pos+len(hit)], Match: true})
			pos += len(hit)
			plain = pos
		} else {
			pos++
		}
	}
	if plain < len(text) {
		out = append(out, Segment{Text: text[plain:]})
	}
	return out
}

// matchSegmentBounds returns [start,end) byte ranges of the matched runs,
// for owner-draw highlighting in the results list. It uses the same
// longest-first rule as highlightSegments.
func matchSegmentBounds(text string, terms []string) [][2]int {
	var bounds [][2]int
	wanted := dedupLower(terms)
	if text == "" || len(wanted) == 0 {
		return nil
	}
	lower := strings.ToLower(text)
	pos := 0
	for pos < len(text) {
		var hitLen int
		for _, t := range wanted {
			if strings.HasPrefix(lower[pos:], t) {
				hitLen = len(t)
				break
			}
		}
		if hitLen > 0 {
			bounds = append(bounds, [2]int{pos, pos + hitLen})
			pos += hitLen
		} else {
			pos++
		}
	}
	return bounds
}

func dedupLower(terms []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, t := range terms {
		if t == "" {
			continue
		}
		lower := strings.ToLower(t)
		if _, dup := seen[lower]; dup {
			continue
		}
		seen[lower] = struct{}{}
		out = append(out, lower)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}
