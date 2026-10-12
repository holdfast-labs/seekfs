package main

// Parsed content constraints and optional-feature selection. Disabled content
// is rejected explicitly rather than being interpreted as a filename term.

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// contentSearchEnabled honors the environment override, then the service's
// configured selection (or a cached default config for offline commands).
func contentSearchEnabled() bool {
	if selected := servicePluginContentSelection.Load(); selected != nil {
		return *selected
	}
	if value, ok := os.LookupEnv("SEEKFS_CONTENT_SEARCH"); ok {
		return value == "1"
	}
	if cfg := serviceContentSelection.Load(); cfg != nil {
		return *cfg
	}
	defaultContentSelection.Do(func() {
		cfg, err := loadConfig("")
		if err == nil {
			defaultContentSelected = featureContentEnabled(cfg)
		}
	})
	return defaultContentSelected
}

var serviceContentSelection atomic.Pointer[bool]
var servicePluginContentSelection atomic.Pointer[bool]
var defaultContentSelection sync.Once
var defaultContentSelected bool

// queryHasContentToken reports whether the raw query contains a token that
// STARTS with content:/!content:/-content:. A substring match would switch the
// tokenizer for a path like C:/x/content:y; only a token boundary counts.
func queryHasContentToken(query string) bool {
	for _, raw := range strings.Fields(query) {
		token := strings.TrimLeft(raw, "!-")
		if strings.HasPrefix(token, "content:") {
			return true
		}
	}
	return false
}

// contentUnavailableError is returned for any `content:` query. It is explicit:
// an absent or still-building content index must never look like "no matches".
func contentUnavailableError() error {
	if !contentSearchEnabled() {
		return fmt.Errorf("content search is disabled; run seekfs plugin add content to enable it")
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

// queryHasAnyContentLeaf reports whether the query tree contains a content leaf
// anywhere: top-level, inside an OR alternative, or under NOT. Candidate
// generation only needs positive leaves; evaluation and the unavailable check
// need any leaf, because `!content:x` is exclusion that must still be applied.
func queryHasAnyContentLeaf(pq parsedQuery) bool {
	if len(pq.Content) > 0 {
		return true
	}
	for _, group := range pq.OrGroups {
		for i := range group {
			if queryHasAnyContentLeaf(group[i]) {
				return true
			}
		}
	}
	for i := range pq.NotGroups {
		if queryHasAnyContentLeaf(pq.NotGroups[i]) {
			return true
		}
	}
	return false
}

// contentAllLeaves returns every content leaf in the tree in the canonical DFS
// order contentAssignLeafIDs used, so LeafID is a stable key. Callers use it to
// evaluate a document against every leaf once.
func contentAllLeaves(pq parsedQuery) []contentLeaf {
	var out []contentLeaf
	contentCollectLeaves(pq, &out)
	return out
}

func contentCollectLeaves(pq parsedQuery, out *[]contentLeaf) {
	*out = append(*out, pq.Content...)
	for g := range pq.OrGroups {
		for a := range pq.OrGroups[g] {
			contentCollectLeaves(pq.OrGroups[g][a], out)
		}
	}
	for n := range pq.NotGroups {
		contentCollectLeaves(pq.NotGroups[n], out)
	}
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
