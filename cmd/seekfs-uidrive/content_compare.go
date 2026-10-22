//go:build windows

package main

import (
	"path/filepath"
	"sort"
	"strings"
)

type contentComparison struct {
	Expected []string `json:"expected"`
	Actual   []string `json:"actual"`
	Missing  []string `json:"missing,omitempty"`
	Extra    []string `json:"extra,omitempty"`
	Pass     bool     `json:"pass"`
}

func normalizeResultPath(path string) string {
	if path == "" {
		return ""
	}
	return strings.ToLower(filepath.ToSlash(filepath.Clean(path)))
}

func expectedResultPaths(root string, rel []string) []string {
	out := make([]string, 0, len(rel))
	for _, r := range rel {
		out = append(out, filepath.Join(root, filepath.FromSlash(r)))
	}
	sort.Strings(out)
	return out
}

func compareResultPaths(expected, actual []string, root string) contentComparison {
	cmp := contentComparison{Pass: true}
	exp := map[string]string{}
	for _, p := range expected {
		n := normalizeResultPath(p)
		exp[n] = p
	}
	act := map[string]string{}
	for _, p := range actual {
		n := normalizeResultPath(p)
		act[n] = p
	}
	for n, p := range exp {
		cmp.Expected = append(cmp.Expected, p)
		if _, ok := act[n]; !ok {
			cmp.Missing = append(cmp.Missing, p)
		}
	}
	for n, p := range act {
		cmp.Actual = append(cmp.Actual, p)
		if _, ok := exp[n]; !ok {
			cmp.Extra = append(cmp.Extra, p)
		}
	}
	sort.Strings(cmp.Expected)
	sort.Strings(cmp.Actual)
	sort.Strings(cmp.Missing)
	sort.Strings(cmp.Extra)
	cmp.Pass = len(cmp.Missing) == 0 && len(cmp.Extra) == 0
	_ = root
	return cmp
}

func snippetContains(snippet, needle string, caseSensitive bool) bool {
	if needle == "" {
		return true
	}
	if caseSensitive {
		return strings.Contains(snippet, needle)
	}
	return strings.Contains(strings.ToLower(snippet), strings.ToLower(needle))
}
