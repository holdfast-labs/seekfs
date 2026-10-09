package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "index database path")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *db == defaultDB() && len(cfg.DBs) > 0 {
		*db = cfg.DBs[0]
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	idx, err := loadIndex(*db)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeJSON(os.Stdout, indexInfoToJSON(idx, *db))
	}
	fmt.Printf("version: %d\n", idx.Version)
	fmt.Printf("source: %s\n", idx.Source)
	fmt.Printf("built_at: %s\n", idx.BuiltAt.Format(time.RFC3339Nano))
	fmt.Printf("entries: %d\n", idx.entryCount())
	fmt.Printf("roots: %s\n", strings.Join(idx.Roots, "; "))
	if idx.Volume != "" {
		fmt.Printf("volume: %s\n", idx.Volume)
		fmt.Printf("journal_id: %d\n", idx.JournalID)
		fmt.Printf("checkpoint_usn: %d\n", idx.Checkpoint)
	}
	if idx.ContentHash != "" {
		fmt.Printf("content_hash: %s\n", idx.ContentHash)
	}
	if layout := estimateIndexLayout(idx, *db); layout != nil {
		fmt.Printf("file_bytes: %d\n", layout.FileBytes)
		fmt.Printf("record_bytes: %d\n", layout.RecordBytes)
		fmt.Printf("name_blob_bytes: %d\n", layout.NameBlobBytes)
		fmt.Printf("name_table_bytes: %d\n", layout.NameTableBytes)
		fmt.Printf("bytes_per_record: %.2f\n", layout.BytesPerRecord)
	}
	return nil
}

func extractUnderPathArg(args []string) ([]string, string) {
	for i, arg := range args {
		under := ""
		if isDriveToken(arg) {
			under = strings.ToUpper(arg[:1]) + `:\`
		} else if filepath.IsAbs(arg) {
			under = filepath.Clean(arg)
		}
		if under == "" {
			continue
		}
		rest := append([]string{}, args[:i]...)
		rest = append(rest, args[i+1:]...)
		return rest, under
	}
	return args, ""
}

func isDriveToken(arg string) bool {
	return len(arg) == 2 && arg[1] == ':' && ((arg[0] >= 'A' && arg[0] <= 'Z') || (arg[0] >= 'a' && arg[0] <= 'z'))
}

func (idx *Index) entryCount() int {
	if idx.Compact {
		if idx.MMapRecords != nil {
			return idx.MMapRecords.Len()
		}
		if idx.PackedRecords != nil {
			return idx.PackedRecords.Len()
		}
		return len(idx.Records)
	}
	return len(idx.Entries)
}

func cmdBenchAgent(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	var dbs stringList
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	fs.Var(&dbs, "db", "index database path; repeatable")
	useService := fs.Bool("service", false, "query the installed seekfs service")
	countOnly := fs.Bool("count", false, "benchmark count-only service queries")
	useResident := fs.Bool("resident", false, "query resident service-volume indexes in-process")
	residentWait := fs.Duration("resident-wait", 2*time.Minute, "max time to wait for resident background indexes")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	iterations := fs.Int("iterations", 100, "number of benchmark iterations")
	warmup := fs.Int("warmup", 0, "untimed warmup iterations")
	limit := fs.Int("n", 20, "maximum results per query")
	queryFile := fs.String("query-file", "", "file containing benchmark queries, one per line")
	matchPath := fs.Bool("path", false, "match full path and file name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	queries := fs.Args()
	if *queryFile != "" {
		fileQueries, err := readBenchQueries(*queryFile)
		if err != nil {
			return err
		}
		queries = append(queries, fileQueries...)
	}
	if len(queries) == 0 {
		queries = []string{"ext:go", "glob:*.md", "type:dir docs", "README", "main"}
	}
	if *iterations <= 0 {
		return errors.New("iterations must be positive")
	}
	if *warmup < 0 {
		return errors.New("warmup must be non-negative")
	}
	if *countOnly && !*useService {
		return errors.New("count benchmarking requires -service")
	}
	var indexes []*Index
	var residentVolumes []*serviceVolumeIndex
	if !*useService {
		if len(dbs) == 0 {
			dbs = append(dbs, defaultDB())
		}
		if *useResident {
			indexes, residentVolumes, _, err = loadConfiguredVolumes(dbs)
			if err != nil {
				return err
			}
			_ = indexes
			svc := &goSearchService{}
			svc.startBackgroundNameOrderBuilds(residentVolumes)
			svc.startBackgroundNameTrigramBuilds(residentVolumes)
			svc.startBackgroundNameIdentityBuilds(residentVolumes)
			waitResidentBackgroundIndexes(residentVolumes, *residentWait)
		} else {
			indexes, err = loadIndexes(dbs)
			if err != nil {
				return err
			}
		}
	}
	timings := make([]float64, 0, *iterations)
	backendTimings := make([]float64, 0, *iterations)
	queryTimings := make(map[string][]float64, len(queries))
	queryBackendTimings := make(map[string][]float64, len(queries))
	sourceCounts := make(map[string]int)
	querySourceCounts := make(map[string]map[string]int, len(queries))
	declineCounts := make(map[string]int)
	queryDeclineCounts := make(map[string]map[string]int, len(queries))
	candidateCounts := make([]float64, 0, *iterations)
	queryCandidateCounts := make(map[string][]float64, len(queries))
	queryFailures := make(map[string]int, len(queries))
	queryResultHashes := make(map[string]string, len(queries))
	queryResultCounts := make(map[string]int, len(queries))
	queryResultConsistent := make(map[string]bool, len(queries))
	queryDiagnostics := make(map[string]benchDiagnostics, len(queries))
	queryDiagnosticsConsistent := make(map[string]bool, len(queries))
	queryResponseSeen := make(map[string]bool, len(queries))
	failures := 0
	for i := 0; i < *warmup; i++ {
		query := queries[i%len(queries)]
		opts := queryOptions{Query: query, MatchPath: *matchPath, Limit: *limit}
		if err := runBenchQuery(*useService, *useResident, *countOnly, *pipeName, indexes, residentVolumes, opts, nil); err != nil {
			return fmt.Errorf("warmup query %q failed: %w", query, err)
		}
	}
	for i := 0; i < *iterations; i++ {
		query := queries[i%len(queries)]
		opts := queryOptions{Query: query, MatchPath: *matchPath, Limit: *limit}
		start := time.Now()
		if *useService {
			var resp serviceResponse
			resp, err = benchServiceRequest(*pipeName, opts, *countOnly)
			if err == nil {
				resultHash := benchResultHash(resp, *countOnly)
				diagnostics := benchDiagnosticsFromResponse(resp)
				if !queryResponseSeen[query] {
					queryResultHashes[query] = resultHash
					queryResultCounts[query] = resp.Count
					queryResultConsistent[query] = true
					queryDiagnostics[query] = diagnostics
					queryDiagnosticsConsistent[query] = true
					queryResponseSeen[query] = true
				} else {
					if queryResultHashes[query] != resultHash || queryResultCounts[query] != resp.Count {
						queryResultConsistent[query] = false
					}
					if !sameBenchDiagnostics(queryDiagnostics[query], diagnostics) {
						queryDiagnosticsConsistent[query] = false
					}
				}
				backendTimings = append(backendTimings, resp.SearchMS)
				queryBackendTimings[query] = append(queryBackendTimings[query], resp.SearchMS)
				recordBenchSource(sourceCounts, querySourceCounts, query, resp.Source)
				recordBenchOptional(declineCounts, queryDeclineCounts, query, resp.Decline)
				candidateCounts = append(candidateCounts, float64(resp.Candidates))
				queryCandidateCounts[query] = append(queryCandidateCounts[query], float64(resp.Candidates))
			}
		} else if *useResident {
			trace := &searchTrace{}
			opts.Trace = trace
			_, err = searchServiceVolumes(residentVolumes, opts, false)
			if err == nil {
				recordBenchSource(sourceCounts, querySourceCounts, query, trace.Source)
				recordBenchOptional(declineCounts, queryDeclineCounts, query, trace.Decline)
				candidateCounts = append(candidateCounts, float64(trace.Candidates))
				queryCandidateCounts[query] = append(queryCandidateCounts[query], float64(trace.Candidates))
			}
		} else {
			_, err = searchAll(indexes, opts, false)
		}
		elapsed := float64(time.Since(start).Nanoseconds()) / 1_000_000
		timings = append(timings, elapsed)
		queryTimings[query] = append(queryTimings[query], elapsed)
		if err != nil {
			failures++
			queryFailures[query]++
		}
	}
	perQuery := make([]benchQuerySummary, 0, len(queries))
	seenQueries := make(map[string]struct{}, len(queries))
	for _, query := range queries {
		if _, seen := seenQueries[query]; seen {
			continue
		}
		seenQueries[query] = struct{}{}
		item := benchQuerySummary{
			Query:                 query,
			Iterations:            len(queryTimings[query]),
			Failures:              queryFailures[query],
			Stats:                 latencyStats(queryTimings[query]),
			Backend:               latencyStats(queryBackendTimings[query]),
			Sources:               querySourceCounts[query],
			Declines:              queryDeclineCounts[query],
			Candidates:            latencyStats(queryCandidateCounts[query]),
			ResultHash:            queryResultHashes[query],
			ResultCount:           queryResultCounts[query],
			ResultConsistent:      queryResponseSeen[query] && queryResultConsistent[query],
			DiagnosticsConsistent: queryResponseSeen[query] && queryDiagnosticsConsistent[query],
		}
		if queryResponseSeen[query] {
			diagnostics := queryDiagnostics[query]
			item.Diagnostics = &diagnostics
		}
		perQuery = append(perQuery, item)
		if len(queryBackendTimings[query]) == 0 {
			perQuery[len(perQuery)-1].Backend = nil
		}
		if len(querySourceCounts[query]) == 0 {
			perQuery[len(perQuery)-1].Sources = nil
		}
		if len(queryDeclineCounts[query]) == 0 {
			perQuery[len(perQuery)-1].Declines = nil
		}
		if len(queryCandidateCounts[query]) == 0 {
			perQuery[len(perQuery)-1].Candidates = nil
		}
	}
	summary := benchSummary{
		OK:         failures == 0,
		Mode:       benchModeName(*useService, *useResident, *countOnly),
		Iterations: *iterations,
		Failures:   failures,
		Queries:    len(queries),
		Stats:      latencyStats(timings),
		Backend:    latencyStats(backendTimings),
		Sources:    sourceCounts,
		Declines:   declineCounts,
		Candidates: latencyStats(candidateCounts),
		PerQuery:   perQuery,
	}
	if len(backendTimings) == 0 {
		summary.Backend = nil
	}
	if len(sourceCounts) == 0 {
		summary.Sources = nil
	}
	if len(declineCounts) == 0 {
		summary.Declines = nil
	}
	if len(candidateCounts) == 0 {
		summary.Candidates = nil
	}
	if *jsonOut {
		return writeJSON(os.Stdout, summary)
	}
	fmt.Printf("mode: %s\niterations: %d\nqueries: %d\nfailures: %d\n", summary.Mode, summary.Iterations, summary.Queries, summary.Failures)
	for _, key := range []string{"min", "median", "p90", "p95", "p99", "max"} {
		fmt.Printf("%s_ms: %.3f\n", key, summary.Stats[key])
	}
	for _, item := range summary.PerQuery {
		if item.Backend != nil {
			fmt.Printf("query=%q iterations=%d failures=%d median_ms=%.3f backend_median_ms=%.3f p90_ms=%.3f p99_ms=%.3f max_ms=%.3f candidates_median=%.0f sources=%v\n", item.Query, item.Iterations, item.Failures, item.Stats["median"], item.Backend["median"], item.Stats["p90"], item.Stats["p99"], item.Stats["max"], item.Candidates["median"], item.Sources)
		} else {
			fmt.Printf("query=%q iterations=%d failures=%d median_ms=%.3f p90_ms=%.3f p99_ms=%.3f max_ms=%.3f candidates_median=%.0f sources=%v\n", item.Query, item.Iterations, item.Failures, item.Stats["median"], item.Stats["p90"], item.Stats["p99"], item.Stats["max"], item.Candidates["median"], item.Sources)
		}
	}
	return nil
}
