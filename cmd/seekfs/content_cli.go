package main

// CLI surface for the offline content index:
//
//	seekfs content-index -root <dir> [-out file.gsx]
//	seekfs content -db file.gsx [-n 100] [--json] <term>

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func cmdContentIndex(args []string) error {
	fs := flag.NewFlagSet("content-index", flag.ContinueOnError)
	root := fs.String("root", "", "directory to index (walk/path-keyed .gsx)")
	db := fs.String("db", "", "record index (.gsi) to build a USN/FRN-keyed .gsx for")
	out := fs.String("out", "", "output .gsx path")
	under := fs.String("under", "", "only index files under this path")
	exts := fs.String("ext", "", "comma-separated extension allowlist, e.g. .go,.md,.txt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := defaultContentBuildOptions()
	opts.Under = *under
	if strings.TrimSpace(*exts) != "" {
		opts.Exts = make(map[string]struct{})
		for _, e := range strings.Split(*exts, ",") {
			e = strings.ToLower(strings.TrimSpace(e))
			if e == "" {
				continue
			}
			if !strings.HasPrefix(e, ".") {
				e = "." + e
			}
			opts.Exts[e] = struct{}{}
		}
	}
	if !contentSearchEnabled() {
		return contentUnavailableError()
	}
	if (*db == "") == (*root == "") {
		// Neither set defaults to a walk build of the current directory.
		if *db == "" {
			*root = "."
		} else {
			return fmt.Errorf("content-index: specify exactly one of -db or -root")
		}
	}

	var (
		idx     *contentIndex
		outPath = *out
		err     error
	)
	if *db != "" {
		rec, lerr := loadIndex(*db)
		if lerr != nil {
			return fmt.Errorf("content-index: load %s: %w", *db, lerr)
		}
		idx, err = buildContentIndexForIndex(context.Background(), rec, opts)
		if outPath == "" {
			outPath = contentIndexPathForDB(*db)
		}
	} else {
		idx, err = buildContentIndexFromDir(context.Background(), *root, opts)
		if outPath == "" {
			outPath = filepath.Join(*root, ".seekfs-content.gsx")
		}
	}
	if err != nil {
		return err
	}
	if outPath == "" {
		return fmt.Errorf("content-index: no output path")
	}
	if err := contentSaveFile(outPath, idx); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "indexed %d documents into %s\n", len(idx.Docs), outPath)
	return nil
}

// contentCLIResult is the additive JSON shape for the offline `content` command:
// path plus the matched-text snippet when one is available.
type contentCLIResult struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet,omitempty"`
}

func cmdContent(args []string) error {
	fs := flag.NewFlagSet("content", flag.ContinueOnError)
	db := fs.String("db", "", "content index (.gsx) to query")
	n := fs.Int("n", 100, "maximum results")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !contentSearchEnabled() {
		return contentUnavailableError()
	}
	term := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(term) == "" {
		return fmt.Errorf("content: missing search term")
	}
	if *db == "" {
		return fmt.Errorf("content: -db <file.gsx> is required")
	}
	idx, err := contentLoadFile(*db)
	if err != nil {
		return err
	}
	r, err := openContentReader(idx)
	if err != nil {
		return err
	}
	hits := r.search(term, *n)

	if *asJSON {
		results := make([]contentCLIResult, 0, len(hits))
		for _, h := range hits {
			results = append(results, contentCLIResult{Path: h.Path, Snippet: h.Snippet})
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(struct {
			OK      bool               `json:"ok"`
			Count   int                `json:"count"`
			Results []contentCLIResult `json:"results"`
		}{OK: true, Count: len(results), Results: results})
	}
	for _, h := range hits {
		if h.Snippet != "" {
			fmt.Fprintf(os.Stdout, "%s\t%s\n", h.Path, h.Snippet)
			continue
		}
		fmt.Fprintln(os.Stdout, h.Path)
	}
	return nil
}
