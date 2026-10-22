//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fixture.go: deterministic fixture tree and the fixture JSON the service
// test mode reads. File names carry the scenario query tokens.

type fixturePlan struct {
	Root            string
	Files           int
	Dirs            int
	PerEntryDelayMS int
}

type uiDriveFixture struct {
	Volumes []uiDriveFixtureVolume `json:"volumes"`
}

type uiDriveFixtureVolume struct {
	Volume          string `json:"volume"`
	Label           string `json:"label,omitempty"`
	FS              string `json:"fs,omitempty"`
	TotalBytes      uint64 `json:"total_bytes,omitempty"`
	FreeBytes       uint64 `json:"free_bytes,omitempty"`
	Root            string `json:"root"`
	PerEntryDelayMS int    `json:"per_entry_delay_ms,omitempty"`
}

// generateFixtureTree writes files small files in nested directories. It
// returns the plan used by the fixture JSON.
func generateFixtureTree(root string, files int) (fixturePlan, error) {
	plan := fixturePlan{Root: root, PerEntryDelayMS: 1}
	if files < 0 {
		files = 0
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return plan, err
	}
	dirs := files / 200
	if dirs < 1 {
		dirs = 1
	}
	if dirs > 100 {
		dirs = 100
	}
	for d := 0; d < dirs; d++ {
		dir := filepath.Join(root, fmt.Sprintf("group-%03d", d), fmt.Sprintf("nested-%02d", d%7))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return plan, err
		}
	}
	perDir := files
	if dirs > 0 {
		perDir = (files + dirs - 1) / dirs
	}
	written := 0
	for d := 0; d < dirs && written < files; d++ {
		dir := filepath.Join(root, fmt.Sprintf("group-%03d", d), fmt.Sprintf("nested-%02d", d%7))
		for i := 0; i < perDir && written < files; i++ {
			global := d*perDir + i
			var name string
			switch global % 3 {
			case 0:
				name = fmt.Sprintf("needle_%05d.txt", global)
			case 1:
				name = fmt.Sprintf("alpha_report_%05d.log", global)
			default:
				name = fmt.Sprintf("doc_%05d.md", global)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture\n"), 0o600); err != nil {
				return plan, err
			}
			plan.Files++
			written++
		}
	}
	plan.Dirs = dirs*2 + 1
	return plan, nil
}

// ---- content corpus ----

type contentExpectation struct {
	ID         string   `json:"id"`
	Query      string   `json:"query"`
	Expected   []string `json:"expected"`
	Snippet    string   `json:"snippet,omitempty"`
	SmartCase  bool     `json:"smart_case,omitempty"`
	Regex      bool     `json:"regex,omitempty"`
	Fuzzy      bool     `json:"fuzzy,omitempty"`
	SnippetAlt string   `json:"snippet_alt,omitempty"`
}

type contentFreshness struct {
	Path     string `json:"path"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Query    string `json:"query_before"`
	QueryNew string `json:"query_after"`
}

type contentExpectedSet struct {
	Root      string               `json:"root"`
	Queries   []contentExpectation `json:"queries"`
	Negative  []contentExpectation `json:"negative"`
	Freshness contentFreshness     `json:"freshness"`
	Files     int                  `json:"content_files"`
}

func writeContentFile(root, rel, body string) error {
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o600)
}

// generateContentFixture adds a small, known-answer content corpus under the
// existing fixture tree. It returns the expected UI answers; the harness writes
// this to JSON next to the fixture.
func generateContentFixture(root string) (contentExpectedSet, error) {
	set := contentExpectedSet{Root: root}
	content := "content"
	files := map[string]string{
		content + "/unique-alpha.txt": "only this file says zzuniqueneedlealpha once.\n",
		content + "/phrase-a.txt":     "cobalt lantern phrase appears in file a.\n",
		content + "/phrase-b.txt":     "the cobalt lantern phrase also appears in file b.\n",
		content + "/phrase-c.txt":     "a third cobalt lantern phrase lives here.\n",
		content + "/case-lower.txt":   "smartcaseword in lowercase.\n",
		content + "/case-upper.txt":   "SMARTCASEWORD in uppercase.\n",
		content + "/case-mixed.txt":   "SmartCaseWord in mixed case.\n",
		content + "/regex-a.txt":      "identifier REGEX-A-123 belongs here.\n",
		content + "/regex-b.txt":      "identifier REGEX-B-456 belongs here.\n",
		content + "/typo-target.txt":  "refactoring is the intended word.\n",
		content + "/symdef.go":        "package fixture\n\n// SymNeedleTarget is a known definition.\nfunc SymNeedleTarget() {}\n",
		content + "/symuse.txt":       "we call SymNeedleTarget from this comment only.\n",
		content + "/freshness.txt":    "freshtokenold is the current token.\n",
		"repo/.gitignore":             "ignored-*.txt\n",
		"repo/keep.txt":               "repostoreneedle is kept inside the nested repository.\n",
		"repo/ignored-secret.txt":     "ignoredneedle must stay out of results.\n",
		"repo/.git/HEAD":              "ref: refs/heads/main\n",
		"repo/.git/config":            "[core]\n\trepositoryformatversion = 0\n",
		"node_modules/pkg/dep.js":     "// excludedneedle in a dependency tree\n",
	}
	for rel, body := range files {
		if err := writeContentFile(root, rel, body); err != nil {
			return set, err
		}
		set.Files++
	}
	// Above the 1 MiB text cap outside a git repo: the needle sits past the cap.
	large := strings.Repeat("filler text for the oversized content fixture. ", 40000) + "largefileunique"
	if err := writeContentFile(root, content+"/large.txt", large); err != nil {
		return set, err
	}
	set.Files++
	// Binary sniff: NUL in the head window means the needle must not match.
	binary := append([]byte{0x00, 0x01, 0x02, 0x03}, []byte("binaryneedle inside binary bytes\n")...)
	if err := os.WriteFile(filepath.Join(root, content, "binary.bin"), binary, 0o600); err != nil {
		return set, err
	}
	set.Files++
	// A few thousand small text files so the content build has realistic work.
	for i := 0; i < 3000; i++ {
		rel := fmt.Sprintf("%s/bulk/bulk-%04d.txt", content, i)
		body := fmt.Sprintf("bulk filler record %04d with ordinary words only.\n", i)
		if err := writeContentFile(root, rel, body); err != nil {
			return set, err
		}
		set.Files++
	}
	set.Queries = []contentExpectation{
		{ID: "unique", Query: "content:zzuniqueneedlealpha", Expected: []string{content + "/unique-alpha.txt"}, Snippet: "zzuniqueneedlealpha"},
		{ID: "phrase", Query: `content:"cobalt lantern phrase"`, Expected: []string{
			content + "/phrase-a.txt", content + "/phrase-b.txt", content + "/phrase-c.txt",
		}, Snippet: "cobalt lantern phrase"},
		{ID: "smartcase-lower", Query: "content:smartcaseword", Expected: []string{
			content + "/case-lower.txt", content + "/case-upper.txt", content + "/case-mixed.txt",
		}, Snippet: "smartcaseword", SmartCase: true},
		{ID: "smartcase-upper", Query: "content:SMARTCASEWORD", Expected: []string{
			content + "/case-upper.txt",
		}, Snippet: "SMARTCASEWORD"},
		{ID: "regex", Query: `content:/REGEX-[AB]-[0-9]{3}/`, Expected: []string{
			content + "/regex-a.txt", content + "/regex-b.txt",
		}, Regex: true},
		{ID: "fuzzy", Query: "content~:refactoringg", Expected: []string{
			content + "/typo-target.txt",
		}, SnippetAlt: "refactoring", Fuzzy: true},
		{ID: "sym", Query: "sym:SymNeedleTarget", Expected: []string{
			content + "/symdef.go",
		}, SnippetAlt: "SymNeedleTarget"},
		{ID: "nested-repo", Query: "content:repostoreneedle", Expected: []string{
			"repo/keep.txt",
		}, Snippet: "repostoreneedle"},
	}
	set.Negative = []contentExpectation{
		{ID: "binary", Query: "content:binaryneedle"},
		{ID: "excluded-node-modules", Query: "content:excludedneedle"},
		{ID: "ignored-gitignore", Query: "content:ignoredneedle"},
		{ID: "oversized", Query: "content:largefileunique"},
	}
	set.Freshness = contentFreshness{
		Path: content + "/freshness.txt", Before: "freshtokenold", After: "freshtokennew",
		Query: "content:freshtokenold", QueryNew: "content:freshtokennew",
	}
	return set, nil
}
