package main

// CLI for usage signals (gap G7): `seekfs open` records an access event,
// `seekfs recent` lists recently opened files. Both prefer the resident
// service (which owns the shared store) and fall back to the local store
// file when the pipe is unreachable, so `open` works offline.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func cmdOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	query := fs.String("query", "", "query that led to this file (feeds the query-history combo boost)")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	if fs.NArg() == 0 {
		return errors.New("open requires a path")
	}
	path := normalizeSignalPath(fs.Arg(0))
	if resp, err := callService(*pipeName, serviceRequest{Command: "open", Path: path, OpenQuery: *query}); err == nil {
		if !resp.OK {
			return errors.New(resp.Message)
		}
		if *jsonOut {
			return writeJSON(os.Stdout, map[string]any{"ok": true, "path": path, "source": "service"})
		}
		fmt.Printf("recorded open for %s\n", path)
		return nil
	}
	// The pipe is unreachable: record locally so the signal is not lost.
	// The service reads the same file (same SEEKFS_DIR) on its next load,
	// and a standalone service on this machine shares it outright.
	sharedSignalStore().recordOpen(path, *query)
	if *jsonOut {
		return writeJSON(os.Stdout, map[string]any{"ok": true, "path": path, "source": "local"})
	}
	fmt.Printf("recorded open for %s (service unreachable; stored locally)\n", path)
	return nil
}

func cmdRecent(args []string) error {
	fs := flag.NewFlagSet("recent", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	limit := fs.Int("n", 20, "maximum files to list")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	if resp, err := callService(*pipeName, serviceRequest{Command: "recent", Limit: *limit}); err == nil {
		if !resp.OK {
			return errors.New(resp.Message)
		}
		return writeRecentOutput(resp.Recent, *jsonOut)
	}
	// The pipe is unreachable: read the local store file.
	recent := sharedSignalStore().recent(*limit)
	rows := make([]recentFileJSON, 0, len(recent))
	for _, r := range recent {
		row := recentFileJSON{
			Path:     r.Path,
			Name:     filepath.Base(r.Path),
			Opens:    r.Opens,
			Score:    r.Score,
			LastOpen: time.Unix(0, r.LastOpenUnix).Format(time.RFC3339Nano),
		}
		if fi, err := os.Stat(r.Path); err == nil {
			row.Modified = fi.ModTime().Format(time.RFC3339Nano)
		}
		rows = append(rows, row)
	}
	return writeRecentOutput(rows, *jsonOut)
}

func writeRecentOutput(rows []recentFileJSON, jsonOut bool) error {
	if jsonOut {
		return writeJSON(os.Stdout, map[string]any{"ok": true, "count": len(rows), "recent": rows})
	}
	for _, r := range rows {
		fmt.Println(r.Path)
	}
	return nil
}
