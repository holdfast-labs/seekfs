package main

// Content scope: which files an automatic content build considers. A scope
// turns "whole volume, every sniffed-as-text file" into a bounded,
// inspectable corpus: explicit roots, detected git worktrees, the built-in
// system/generated excludes, known-binary extension skips, and a byte budget
// (ceiling 32 GiB; corpora past 64 MiB shard, so totals past the 4 GiB
// single-file `.gsx` cap stay per-file safe).
//
// Scope affects *which* docs are indexed, never the `.gsx` layout, so it needs
// no format bump. It is resolved per volume from the global config plus a
// per-volume override plus environment; see contentScopeForVolume.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type contentScopeMode string

const (
	// contentScopeAuto detects roots (git worktrees, known folders) and applies
	// the built-in excludes and the budget.
	contentScopeAuto contentScopeMode = "auto"
	// contentScopeOff disables content entirely for the volume (no build).
	contentScopeOff contentScopeMode = "off"
	// contentScopeExplicit indexes only the configured Roots (plus detected git
	// worktrees when IncludeGit).
	contentScopeExplicit contentScopeMode = "explicit"
)

const (
	// contentScopeBudgetFraction bounds the projected `.gsx` at a fraction of
	// the hard cap, leaving headroom for index overhead and delta growth. It
	// applies only when no volume anchor is available at all.
	contentScopeBudgetFraction = 0.5
	// contentScopeBudgetCeilBytes caps an explicit budget_bytes well above
	// the 4 GiB single-file sidecar cap: any corpus past the 64 MiB shard
	// target builds as folder shards, each individually capped at
	// contentGSXMaxBytes by contentSaveFile, so a large total budget stays
	// memory-bounded and per-file safe.
	contentScopeBudgetCeilBytes = 32 << 30
)

// contentScopeConfig is the raw, per-key config form (pointer bools are
// tri-state so an unset key does not override a lower layer).
type contentScopeConfig struct {
	Mode           string
	Roots          []string
	Exclude        []string
	Exts           []string
	Git            *bool
	SystemExcludes *bool
	BudgetBytes    int64
}

// contentScope is the resolved scope for one volume.
type contentScope struct {
	Mode           contentScopeMode
	Roots          []string            // normalized, absolute-ish path prefixes
	Exclude        []string            // normalized path prefixes / globs
	Exts           map[string]struct{} // nil = the supported-type registry
	IncludeGit     bool
	SystemExcludes bool
	BudgetBytes    int64
	// BudgetExplicit is true when the budget came from config or environment
	// (vs the free-space-derived default). Only an explicit budget joins the
	// scope fingerprint.
	BudgetExplicit bool
}

func defaultContentScope() contentScope {
	// BudgetBytes 0 with BudgetExplicit false means unlimited: index
	// everything selected, stopping only when the projected index would
	// breach the volume's free-space reserve (see contentScopeAllowance).
	// The memory side stays bounded because sharding caps each shard at
	// contentShardTargetBytes of extracted text.
	return contentScope{
		Mode:           contentScopeAuto,
		IncludeGit:     true,
		SystemExcludes: true,
	}
}

// contentNormPath normalizes a path for scope comparison: cleaned, and
// case-folded (Windows matching is case-insensitive). Separators are left to
// filepath so both `\` and `/` forms compare after Clean.
func contentNormPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return strings.ToLower(filepath.Clean(p))
}

// normalizeContentPaths cleans each entry and drops empties.
func normalizeContentPaths(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if n := contentNormPath(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// contentExtSet normalizes an extension allowlist: lowercase, leading dot.
func contentExtSet(in []string) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out[e] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseContentScopeMode(s string) (contentScopeMode, bool) {
	switch contentScopeMode(strings.ToLower(strings.TrimSpace(s))) {
	case contentScopeAuto:
		return contentScopeAuto, true
	case contentScopeOff:
		return contentScopeOff, true
	case contentScopeExplicit:
		return contentScopeExplicit, true
	}
	return "", false
}

// contentScopeForVolume resolves the effective scope: defaults, then the global
// [content] config, then any [content."<volume>"] override, then environment.
// Without config or environment budget_bytes the budget is unlimited and
// reserve-governed (see contentScopeAllowance); an explicit value wins and is
// clamped to the ceiling.
func contentScopeForVolume(cfg appConfig, volume string) contentScope {
	s := defaultContentScope()
	budgetSet := false
	apply := func(sc contentScopeConfig) {
		if m, ok := parseContentScopeMode(sc.Mode); ok {
			s.Mode = m
		}
		if sc.Roots != nil {
			s.Roots = normalizeContentPaths(sc.Roots)
		}
		if len(sc.Exclude) > 0 {
			s.Exclude = append(s.Exclude, normalizeContentPaths(sc.Exclude)...)
		}
		if sc.Exts != nil {
			s.Exts = contentExtSet(sc.Exts)
		}
		if sc.Git != nil {
			s.IncludeGit = *sc.Git
		}
		if sc.SystemExcludes != nil {
			s.SystemExcludes = *sc.SystemExcludes
		}
		if sc.BudgetBytes > 0 {
			s.BudgetBytes = sc.BudgetBytes
			budgetSet = true
		}
	}
	apply(cfg.Content)
	if d, ok := cfg.Plugins["content"]; ok && d.Installed {
		apply(d.Content)
	}
	if ov, ok := cfg.ContentByVolume[strings.ToUpper(volume)]; ok {
		apply(ov)
	}
	if applyContentScopeEnv(&s) {
		budgetSet = true
	}
	// BudgetExplicit records operator provenance for the scope fingerprint:
	// only an explicit budget joins the sidecar identity. The default
	// (BudgetBytes 0) is reserve-governed against live disk figures, and
	// fingerprinting it would rebuild on every drift.
	s.BudgetExplicit = budgetSet
	if s.Mode == "" {
		s.Mode = contentScopeAuto
	}
	if !budgetSet {
		// No explicit budget: unlimited, reserve-governed. Sharding bounds
		// memory per shard, so the budget is disk space, enforced live
		// against the reserve (see contentScopeAllowance), not a byte cap.
		s.BudgetBytes = 0
	} else if s.BudgetBytes <= 0 || s.BudgetBytes > contentScopeBudgetCeilBytes {
		s.BudgetBytes = contentScopeBudgetCeilBytes
	}
	return s
}

// contentScopeBudgetAnchor returns a filesystem path on the index volume for
// the disk-derived budget, or "" when the volume names no anchor. A bare
// volume ("E:") anchors at its root; an absolute path anchors at itself.
func contentScopeBudgetAnchor(volume string) string {
	volume = strings.TrimSpace(volume)
	if volume == "" {
		return ""
	}
	if len(volume) >= 2 && volume[1] == ':' {
		return volume[:2] + string(filepath.Separator)
	}
	if filepath.IsAbs(volume) {
		return volume
	}
	return ""
}

// applyContentScopeEnv overlays global environment overrides. It is read at
// resolve time (like contentServiceEncodingLabel) so the CLI and service agree
// without shared mutable state; per-volume overrides stay config-only. It
// reports whether the environment set an explicit budget.
func applyContentScopeEnv(s *contentScope) bool {
	if m, ok := parseContentScopeMode(os.Getenv("SEEKFS_CONTENT_SCOPE")); ok {
		s.Mode = m
	}
	if v := strings.TrimSpace(os.Getenv("SEEKFS_CONTENT_ROOTS")); v != "" {
		s.Roots = normalizeContentPaths(filepath.SplitList(v))
	}
	if v := strings.TrimSpace(os.Getenv("SEEKFS_CONTENT_EXCLUDE")); v != "" {
		s.Exclude = append(s.Exclude, normalizeContentPaths(filepath.SplitList(v))...)
	}
	if v := strings.TrimSpace(os.Getenv("SEEKFS_CONTENT_BUDGET_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			s.BudgetBytes = n
			return true
		}
	}
	return false
}

// contentScopeDisabled reports whether content should be skipped entirely.
func (s contentScope) disabled() bool { return s.Mode == contentScopeOff }

// parseContentConfigSection maps a TOML section name to a scope target:
// "content" for [content], "content:<volume>" for [content."C:"], or "ignore"
// for any other section (so its keys are skipped, not misread as top-level).
func parseContentConfigSection(inner string) string {
	inner = strings.TrimSpace(inner)
	if inner == "content" {
		return "content"
	}
	rest, ok := strings.CutPrefix(inner, "content.")
	if !ok {
		return "ignore"
	}
	rest = strings.TrimSpace(strings.Trim(strings.TrimSpace(rest), `"`))
	if rest == "" {
		return "content"
	}
	return "content:" + rest
}

// applyContentScopeConfig routes one key=value under a content section into the
// global Content config or the per-volume override.
func applyContentScopeConfig(cfg *appConfig, section, key, value string) {
	if vol, ok := strings.CutPrefix(section, "content:"); ok {
		vol = strings.ToUpper(vol)
		if cfg.ContentByVolume == nil {
			cfg.ContentByVolume = make(map[string]contentScopeConfig)
		}
		sc := cfg.ContentByVolume[vol]
		applyContentScopeKey(&sc, key, value)
		cfg.ContentByVolume[vol] = sc
		return
	}
	applyContentScopeKey(&cfg.Content, key, value)
}

func applyContentScopeKey(sc *contentScopeConfig, key, value string) {
	switch key {
	case "mode":
		sc.Mode = parseTOMLString(value)
	case "roots", "root", "dirs":
		sc.Roots = append(sc.Roots, parseTOMLStringArray(value)...)
	case "exclude", "excludes":
		sc.Exclude = append(sc.Exclude, parseTOMLStringArray(value)...)
	case "exts":
		sc.Exts = append(sc.Exts, parseTOMLStringArray(value)...)
	case "git":
		if b, ok := parseTOMLBool(value); ok {
			sc.Git = &b
		}
	case "system_excludes":
		if b, ok := parseTOMLBool(value); ok {
			sc.SystemExcludes = &b
		}
	case "budget_bytes":
		var n int64
		if _, err := fmt.Sscanf(value, "%d", &n); err == nil {
			sc.BudgetBytes = n
		}
	}
}
