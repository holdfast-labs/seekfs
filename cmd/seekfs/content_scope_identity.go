package main

import (
	"crypto/sha256"
	"encoding/json"
	"sort"
)

// fingerprint identifies the effective scope and budget selection policy used
// to build a sidecar. Old sidecars have a zero hash and must be rebuilt.
func (r contentScopeResolved) fingerprint() [32]byte {
	exts := make([]string, 0, len(r.ExtSet))
	for ext := range r.ExtSet {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	canonical := struct {
		SelectionVersion int
		Mode             contentScopeMode
		Roots            []string
		Repos            []string
		ExplicitRoots    []string
		Excludes         []string
		Exts             []string
		BudgetBytes      int64
		SystemExcludes   bool
	}{1, r.Mode, sortedContentScopePaths(r.Roots), sortedContentScopePaths(r.Repos),
		sortedContentScopePaths(r.ExplicitRoots), sortedContentScopePaths(r.Excludes),
		exts, r.BudgetBytes, r.SystemExcludes}
	b, _ := json.Marshal(canonical) // fixed fields of supported JSON scalar types
	return sha256.Sum256(b)
}

func sortedContentScopePaths(paths []string) []string {
	out := append([]string(nil), paths...)
	sort.Strings(out)
	return out
}
