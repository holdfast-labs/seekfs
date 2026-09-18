package main

// The parsed representation of `content:` constraints and the P0 gate stub.
//
// Content search is off by default and the content index does not exist yet, so
// every `content:` query is rejected with a clear error rather than being
// planned as a name query. That keeps the "off means normal seekfs" contract
// and guarantees no silently wrong results before the P1/P2 wiring lands.

import (
	"fmt"
	"os"
	"strings"
)

// contentLeafKind discriminates the three content constraint forms.
type contentLeafKind uint8

const (
	contentLeafTerm contentLeafKind = iota
	contentLeafPhrase
	contentLeafRegex
)

// contentLeaf is one `content:` constraint. LeafID is assigned deterministically
// from the canonical DFS order of content leaves so repeated parseQuery calls
// agree; per-leaf verification keys on it.
type contentLeaf struct {
	Kind   contentLeafKind
	Text   string
	LeafID int
}

// contentSearchEnabled reports whether content search is switched on. Default
// off: content is opt-in, and until the index and planner land the flag only
// selects between "disabled" and "not yet built".
func contentSearchEnabled() bool {
	return os.Getenv("SEEKFS_CONTENT_SEARCH") == "1"
}

// contentUnavailableError is returned for any `content:` query. It is explicit:
// an absent or still-building content index must never look like "no matches".
func contentUnavailableError() error {
	if !contentSearchEnabled() {
		return fmt.Errorf("content search is disabled; set SEEKFS_CONTENT_SEARCH=1 to opt in")
	}
	return fmt.Errorf("content indexing unavailable: the content index is not built or still building")
}

// parseContentLeaf parses the value after `content:`.
func parseContentLeaf(raw string) (contentLeaf, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return contentLeaf{}, fmt.Errorf("empty content: term")
	}
	// content:/regex/ (the tokenizer keeps the span whole).
	if strings.HasPrefix(raw, "/") && strings.HasSuffix(raw, "/") && len(raw) >= 2 {
		pat := raw[1 : len(raw)-1]
		if pat == "" {
			return contentLeaf{}, fmt.Errorf("empty content regex")
		}
		return contentLeaf{Kind: contentLeafRegex, Text: pat}, nil
	}
	// A quoted phrase.
	if len(raw) >= 2 && (raw[0] == '"' || raw[0] == '\'') && raw[len(raw)-1] == raw[0] {
		return contentLeaf{Kind: contentLeafPhrase, Text: contentUnquoteToken(raw)}, nil
	}
	return contentLeaf{Kind: contentLeafTerm, Text: raw}, nil
}

// queryHasPositiveContentLeaf reports whether the query tree contains a content
// leaf in a positive position: top-level or inside any OR alternative. Content
// under NOT is exclusion and does not make the query content-driven.
func queryHasPositiveContentLeaf(pq parsedQuery) bool {
	if len(pq.Content) > 0 {
		return true
	}
	for _, group := range pq.OrGroups {
		for i := range group {
			if queryHasPositiveContentLeaf(group[i]) {
				return true
			}
		}
	}
	return false
}

// contentAssignLeafIDs assigns LeafIDs deterministically: positive Content
// first, then OR alternatives in order, then NOT groups, recursing in field
// order. A pure function of tree position, so every re-parse yields the same
// IDs.
func contentAssignLeafIDs(pq *parsedQuery) {
	next := 0
	contentAssignLeafIDsInto(pq, &next)
}

func contentAssignLeafIDsInto(pq *parsedQuery, next *int) {
	for i := range pq.Content {
		pq.Content[i].LeafID = *next
		*next++
	}
	for g := range pq.OrGroups {
		for a := range pq.OrGroups[g] {
			contentAssignLeafIDsInto(&pq.OrGroups[g][a], next)
		}
	}
	for n := range pq.NotGroups {
		contentAssignLeafIDsInto(&pq.NotGroups[n], next)
	}
}
