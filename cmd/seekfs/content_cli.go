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
	all := fs.Bool("all", false, "with -db, explicitly opt in to a whole-volume build (every extractable file)")
	encoding := fs.String("encoding", "auto", "text encoding override: auto, none, or a WHATWG label (latin1, windows-1252, utf-16le, sjis, ...)")
	maxRaw := fs.Int64("max-raw", 0, "max raw source bytes per file (0 = default 32 MiB, clamped to the hard ceiling)")
	maxText := fs.Int64("max-text", 0, "max extracted text bytes per file (0 = default 16 MiB, clamped to the hard ceiling)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := defaultContentBuildOptions()
	opts.Under = *under
	opts.MaxRaw = *maxRaw
	opts.MaxText = *maxText
	if *encoding != "" {
		if _, err := parseContentEncoding(*encoding); err != nil {
			return fmt.Errorf("content-index: %w", err)
		}
		opts.Encoding = *encoding
	}
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
	// M5: a -db build with no scope indexes the whole volume, which is a
	// resource/semantics footgun. Require an explicit scope or opt-in.
	if *db != "" && !*all && strings.TrimSpace(*under) == "" && len(opts.Exts) == 0 {
		return fmt.Errorf("content-index: -db without -ext/-under would index the whole volume; pass -ext <list>, -under <path>, or -all to opt in")
	}

	outPath := *out
	if outPath == "" {
		if *db != "" {
			outPath = contentIndexPathForDB(*db)
		} else {
			outPath = filepath.Join(*root, ".seekfs-content.gsx")
		}
	}
	// M10: building the service-owned sidecar must not race a live service's
	// persist/swap. Hold the same advisory lock the service holds; fail fast
	// with a clear message when a service (or another build) owns the volume.
	if *db != "" && samePath(outPath, contentIndexPathForDB(*db)) {
		lk, lerr := acquireContentVolumeLock(outPath)
		if lerr != nil {
			return fmt.Errorf("content-index: %w: %s; stop the running service or index through it", lerr, outPath)
		}
		defer lk.release()
	}

	var (
		idx *contentIndex
		err error
	)
	if *db != "" {
		rec, lerr := loadIndex(*db)
		if lerr != nil {
			return fmt.Errorf("content-index: load %s: %w", *db, lerr)
		}
		idx, err = buildContentIndexForIndex(context.Background(), rec, opts)
	} else {
		idx, err = buildContentIndexFromDir(context.Background(), *root, opts)
	}
	if err != nil {
		return err
	}
	if err := contentSaveFile(outPath, idx); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "indexed %d documents into %s\n", len(idx.Docs), outPath)
	if p := idx.Policy; p != (contentBuildPolicy{}) {
		fmt.Fprintf(os.Stdout, "policy: max_raw=%d max_text=%d skipped=%d truncated=%d\n", p.MaxRaw, p.MaxText, p.Skipped, p.Truncated)
	}
	return nil
}

// contentCLIResponse is the additive JSON shape for the offline `content`
// command: `results` stays an array of path strings (the original shape) and
// `snippets` is a parallel array of matched-text windows, aligned to `results`.
// Consumers that only know the old shape are unaffected.
type contentCLIResponse struct {
	OK       bool     `json:"ok"`
	Count    int      `json:"count"`
	Results  []string `json:"results"`
	Snippets []string `json:"snippets"`
}

func cmdContent(args []string) error {
	fs := flag.NewFlagSet("content", flag.ContinueOnError)
	db := fs.String("db", "", "content index (.gsx) to query")
	n := fs.Int("n", 100, "maximum results")
	asJSON := fs.Bool("json", false, "emit JSON")
	withSnippet := fs.Bool("snippet", false, "append the matched-text snippet to each line")
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
	// A one-shot CLI query owns the mapping only for its own duration; release
	// it on return. The hits are copied strings, so nothing outlives it.
	defer idx.Release()
	r, err := openContentReader(idx)
	if err != nil {
		return err
	}
	hits := r.search(term, *n)

	if *asJSON {
		resp := contentCLIResponse{
			OK:       true,
			Count:    len(hits),
			Results:  make([]string, 0, len(hits)),
			Snippets: make([]string, 0, len(hits)),
		}
		for _, h := range hits {
			resp.Results = append(resp.Results, h.Path)
			resp.Snippets = append(resp.Snippets, h.Snippet)
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(resp)
	}
	for _, h := range hits {
		if *withSnippet && h.Snippet != "" {
			fmt.Fprintf(os.Stdout, "%s\t%s\n", h.Path, h.Snippet)
			continue
		}
		fmt.Fprintln(os.Stdout, h.Path)
	}
	return nil
}
