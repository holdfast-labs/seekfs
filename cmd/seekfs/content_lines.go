package main

import (
	"bytes"
)

type contentOutputLine struct {
	Line          int      `json:"line"`
	Text          string   `json:"text"`
	ContextBefore []string `json:"context_before"`
	ContextAfter  []string `json:"context_after"`
	Context       bool     `json:"-"`
}

// contentMatchingLines returns ripgrep-style matching lines and merged context
// groups. Phrases are matched within a single line; they do not span newlines.
func contentMatchingLines(raw []byte, matcher *contentLeafMatcher, maxPerFile, before, after int) []contentOutputLine {
	if maxPerFile <= 0 {
		maxPerFile = 10
	}
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	rawLines := bytes.Split(raw, []byte{'\n'})
	if len(rawLines) > 0 && len(rawLines[len(rawLines)-1]) == 0 {
		rawLines = rawLines[:len(rawLines)-1]
	}
	lines := make([]string, len(rawLines))
	matched := make([]bool, len(rawLines))
	for i, b := range rawLines {
		b = bytes.TrimSuffix(b, []byte{'\r'})
		lines[i] = string(b)
		for _, leaf := range matcher.leaves {
			if matcher.match(b, nil, leaf) {
				matched[i] = true
				break
			}
		}
	}
	var hits []int
	for i, yes := range matched {
		if yes {
			hits = append(hits, i)
			if len(hits) == maxPerFile {
				break
			}
		}
	}
	selected := make([]bool, len(lines))
	context := make([]bool, len(lines))
	for _, hit := range hits {
		lo, hi := hit-before, hit+after
		if lo < 0 {
			lo = 0
		}
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		for i := lo; i <= hi; i++ {
			selected[i] = true
			if !matched[i] {
				context[i] = true
			}
		}
	}
	out := make([]contentOutputLine, 0)
	for i, ok := range selected {
		if ok {
			out = append(out, contentOutputLine{Line: i + 1, Text: lines[i], Context: context[i]})
		}
	}
	// Populate exact context arrays for additive JSON output.
	for i := range out {
		if !out[i].Context {
			continue
		}
		for j := i - 1; j >= 0 && out[j].Line+1 == out[j+1].Line; j-- {
			if !out[j].Context {
				break
			}
			out[i].ContextBefore = append([]string{out[j].Text}, out[i].ContextBefore...)
		}
		for j := i + 1; j < len(out) && out[j].Line == out[j-1].Line+1; j++ {
			if !out[j].Context {
				break
			}
			out[i].ContextAfter = append(out[i].ContextAfter, out[j].Text)
		}
	}
	return out
}
