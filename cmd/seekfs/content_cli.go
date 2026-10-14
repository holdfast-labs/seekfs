package main

// CLI surface for the offline content index:
//
//	seekfs content-index -root <dir> [-out file.gsx]
//	seekfs content -db file.gsx [-n 100] [--json] <term>

import (
	"context"
	"encoding/json"
	"errors"
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
	scoped := fs.Bool("scoped", false, "with -db, derive the auto content scope from the index (git worktrees + configured/known roots)")
	estimate := fs.Bool("estimate", false, "with -db, print the auto content scope estimate (files/bytes/repos) and exit")
	sniffSample := fs.Int("sniff-sample", 0, "with -estimate, head-sniff up to N text-candidate files to project the binary share")
	listPaths := fs.Bool("paths", false, "with -estimate, print every selected path (one per line) instead of the summary")
	jsonOut := fs.Bool("json", false, "with -estimate, emit JSON")
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	encoding := fs.String("encoding", "auto", "text encoding override: auto, none, or a WHATWG label (latin1, windows-1252, utf-16le, sjis, ...)")
	maxRaw := fs.Int64("max-raw", 0, "max raw source bytes per file (0 = default 32 MiB, clamped to the hard ceiling)")
	maxText := fs.Int64("max-text", 0, "max extracted text bytes per file (0 = default 16 MiB, clamped to the hard ceiling)")
	tier2Budget := fs.Int64("tier2-budget", 0, "with -root, partition the corpus at this many extracted-text bytes: the head builds tier 1, the tail is covered by the tier-2 filter sidecar (<out>.filter)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := defaultContentBuildOptions()
	opts.Under = *under
	opts.MaxRaw = *maxRaw
	opts.MaxText = *maxText
	opts.Tier2Budget = *tier2Budget
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
	if *db != "" && !*all && !*scoped && !*estimate && strings.TrimSpace(*under) == "" && len(opts.Exts) == 0 {
		return fmt.Errorf("content-index: -db without a scope would index the whole volume; pass -ext <list>, -under <path>, -all, or --scoped")
	}

	// --estimate is read-only: it must run before the volume lock so it can
	// estimate against a volume a live service owns.
	if *estimate {
		if *db == "" {
			return errors.New("content-index: --estimate requires -db")
		}
		rec, lerr := loadIndex(*db)
		if lerr != nil {
			return fmt.Errorf("content-index: load %s: %w", *db, lerr)
		}
		cfg, err := loadConfig(*configPath)
		if err != nil {
			return fmt.Errorf("content-index: config: %w", err)
		}
		scope := contentScopeForVolume(cfg, rec.Volume)
		est := scanContentScope(rec, scope, rec.Volume, opts.MaxFiles, *sniffSample, *listPaths)
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(est)
		}
		if *listPaths {
			for _, p := range est.Paths {
				fmt.Fprintln(os.Stdout, p)
			}
			return nil
		}
		fmt.Fprintf(os.Stdout, "content scope estimate for %s (mode=%s):\n", rec.Volume, scope.Mode)
		budget := fmt.Sprintf("%d", est.BudgetBytes)
		if !scope.BudgetExplicit {
			budget = "unlimited (reserve-governed)"
		}
		fmt.Fprintf(os.Stdout, "  files=%d bytes=%d projected_gsx=%d max_files_hit=%v budget_hit=%v budget=%s eligible_files=%d eligible_bytes=%d\n", est.Files, est.Bytes, est.ProjectedGSXBytes, est.MaxFilesHit, est.BudgetHit, budget, est.EligibleFiles, est.EligibleBytes)
		fmt.Fprintf(os.Stdout, "  text_candidates=%d binary_candidates=%d", est.TextCandidates, est.BinaryCandidates)
		for reason, n := range est.Excluded {
			fmt.Fprintf(os.Stdout, " excluded_%s=%d", reason, n)
		}
		fmt.Fprintln(os.Stdout)
		if *sniffSample > 0 {
			fmt.Fprintf(os.Stdout, "  sniff_sample: opened=%d binary=%d projected_text_files=%d\n", est.SampledFiles, est.SampledBinary, est.ProjectedTextFiles)
		}
		for _, r := range est.Repos {
			fmt.Fprintf(os.Stdout, "  repo: %s\n", r)
		}
		for _, u := range est.ByUnit {
			fmt.Fprintf(os.Stdout, "  %s [%s]: files=%d bytes=%d\n", u.Root, u.Kind, u.Files, u.Bytes)
		}
		return nil
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
		flt *contentFilter
		err error
	)
	if *db != "" {
		rec, lerr := loadIndex(*db)
		if lerr != nil {
			return fmt.Errorf("content-index: load %s: %w", *db, lerr)
		}
		cfg, cfgErr := loadConfig(*configPath)
		if cfgErr != nil {
			return fmt.Errorf("content-index: config: %w", cfgErr)
		}
		if *scoped {
			scope := contentScopeForVolume(cfg, rec.Volume)
			if scope.disabled() {
				return errors.New("content-index: content scope is off for this volume")
			}
			resolved, _ := scope.resolveForIndex(rec.Volume, rec)
			opts.Scope = &resolved
		}
		idx, err = buildContentIndexForIndex(context.Background(), rec, opts)
	} else {
		idx, flt, err = buildContentIndexFromDirFiltered(context.Background(), *root, opts)
	}
	if err != nil {
		return err
	}
	if opts.Scope != nil {
		idx.ScopeHash = opts.Scope.fingerprint()
	}
	if flt != nil {
		flt.ScopeHash = idx.ScopeHash
		flt.MaxRaw, flt.MaxText = idx.Policy.MaxRaw, idx.Policy.MaxText
	}
	if err := contentSaveFile(outPath, idx); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "indexed %d documents into %s\n", len(idx.Docs), outPath)
	if flt != nil {
		if err := contentSaveFilter(contentFilterPathForGSX(outPath), flt); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "filter: %d files into %s (%.1f KB)\n", len(flt.Records), contentFilterPathForGSX(outPath), float64(flt.EncodedSize)/1024)
	}
	if p := idx.Policy; p != (contentBuildPolicy{}) {
		limit := fmt.Sprintf("budget=%d", p.BudgetBytes)
		if p.ReserveBytes > 0 {
			limit = fmt.Sprintf("reserve=%d", p.ReserveBytes)
		} else if p.BudgetBytes == 0 {
			limit = "budget=unlimited"
		}
		fmt.Fprintf(os.Stdout, "policy: max_raw=%d max_text=%d skipped=%d truncated=%d scope_dropped=%d scope_dropped_bytes=%d %s\n", p.MaxRaw, p.MaxText, p.Skipped, p.Truncated, p.ScopeDropped, p.ScopeDroppedBytes, limit)
	}
	if idx.Scanned > 0 || idx.Declined > 0 {
		// Reconcile: indexed == eligible - declined - skipped (truncated files
		// still contribute a bounded prefix, so they are not subtracted).
		fmt.Fprintf(os.Stdout, "scan: eligible=%d declined=%d\n", idx.Scanned, idx.Declined)
	}
	return nil
}

// contentCLIResponse is the additive JSON shape for the offline `content`
// command: `results` stays an array of path strings (the original shape) and
// `snippets` is a parallel array of matched-text windows, aligned to `results`.
// Consumers that only know the old shape are unaffected. When a zero-result
// query was retried with typo-tolerant corrections, FuzzyCorrection names the
// corrected words ("Showing results for receive").
type contentCLIResponse struct {
	OK              bool     `json:"ok"`
	Count           int      `json:"count"`
	Results         []string `json:"results"`
	Snippets        []string `json:"snippets"`
	FuzzyCorrection string   `json:"fuzzy_correction,omitempty"`
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
	// Small sections were verified at load; check the large ones before
	// serving results, so a flipped byte fails the query instead of answering
	// from corrupt bytes.
	if err := verifyContentSectionCRCs(idx); err != nil {
		return fmt.Errorf("content: %s failed integrity check: %w", *db, err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		return err
	}
	// Tier-2 filter (§6y): the sibling sidecar covers files the text budget
	// dropped. Missing or unusable means tier 1 alone, never an error.
	var flt *contentFilter
	if f, ferr := contentLoadFilter(contentFilterPathForGSX(*db)); ferr == nil {
		flt = f
		defer f.Release()
	}
	settings := contentDefaultExtractSettings()
	hits, complete := r.searchFilter(term, *n, flt, settings)
	if !complete {
		// The filter scan stopped at the verification cap with records
		// unvisited: the answer is a visible subset, never silent.
		fmt.Fprintf(os.Stderr, "content: incomplete results (filter verification cap reached; narrow the term or raise -n)\n")
	}
	fuzzy := ""
	if len(hits) == 0 {
		if corrected, show, ok := contentFuzzyCorrectOfflineText(r, term); ok {
			if retry := r.search(corrected, *n); len(retry) > 0 {
				hits, fuzzy = retry, show
			}
		}
	}

	if *asJSON {
		resp := contentCLIResponse{
			OK:              true,
			Count:           len(hits),
			Results:         make([]string, 0, len(hits)),
			Snippets:        make([]string, 0, len(hits)),
			FuzzyCorrection: fuzzy,
		}
		for _, h := range hits {
			resp.Results = append(resp.Results, h.Path)
			resp.Snippets = append(resp.Snippets, h.Snippet)
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(resp)
	}
	if fuzzy != "" {
		fmt.Fprintf(os.Stdout, "(showing results for %q)\n", fuzzy)
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
