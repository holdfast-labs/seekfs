package main

import (
	"crypto/sha256"
	"encoding/json"
	"sort"
)

// contentScopeSelectionVersion changes whenever the rules that pick files
// for a given scope change, so sidecars selected under old rules rebuild.
// Version 2: git ignore rules replace name-based excludes inside worktrees.
// Version 3: only an explicit budget joins the identity; the free-space
// derived default moves with disk usage and must not invalidate sidecars.
// Version 4: the global excludes file joins the ignore digest.
// Version 5: sniff-based default selection (SniffText) joins the identity: an
// explicit allowlist identical to the registry still selects differently from
// sniffing, so the mode bit must invalidate.
// Version 6: the inclusion policy (§6z) joins the identity: the linked-
// worktree filter (Repos now holds only indexed checkouts plus the mode),
// the content-skip list, and the lockfile/large-JSON/minified/build-output/
// cache skips. A worktree appearing or vanishing, or a skip-list edit,
// reconciles the selection difference instead of rebuilding (see §6p),
// except past the rebuild fraction.
// Version 7: whole-volume auto root and the outside-repo dependency/binary
// skips join the selection rules.
const contentScopeSelectionVersion = 7

// fingerprint identifies the effective scope and budget selection policy used
// to build a sidecar. Old sidecars have a zero hash and must be rebuilt. The
// budget joins only when explicitly configured: the derived default tracks
// free space, and fingerprinting it would rebuild on every drift.
func (r contentScopeResolved) fingerprint() [32]byte {
	exts := make([]string, 0, len(r.ExtSet))
	for ext := range r.ExtSet {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	var budget int64
	if r.BudgetExplicit {
		budget = r.BudgetBytes
	}
	canonical := struct {
		SelectionVersion int
		Mode             contentScopeMode
		Roots            []string
		Repos            []string
		ExplicitRoots    []string
		Excludes         []string
		Exts             []string
		SniffText        bool
		BudgetBytes      int64
		SystemExcludes   bool
		IgnoreDigest     [32]byte
		WorktreesAll     bool
		Skip             []string
	}{contentScopeSelectionVersion, r.Mode, sortedContentScopePaths(r.Roots), sortedContentScopePaths(r.Repos),
		sortedContentScopePaths(r.ExplicitRoots), sortedContentScopePaths(r.Excludes),
		exts, r.SniffText, budget, r.SystemExcludes, r.IgnoreDigest, r.WorktreesAll,
		sortedContentScopePaths(r.Skip)}
	b, _ := json.Marshal(canonical) // fixed fields of supported JSON scalar types
	return sha256.Sum256(b)
}

func sortedContentScopePaths(paths []string) []string {
	out := append([]string(nil), paths...)
	sort.Strings(out)
	return out
}
