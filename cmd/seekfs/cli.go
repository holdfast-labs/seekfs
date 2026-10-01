package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if wantsJSON(os.Args[1:]) {
			_ = json.NewEncoder(os.Stderr).Encode(jsonError{OK: false, Error: err.Error()})
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func wantsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || arg == "-json" {
			return true
		}
	}
	return false
}

func run(args []string) error {
	if len(args) == 0 {
		return cmdDefault(args)
	}
	switch args[0] {
	case "index":
		return cmdIndex(args[1:])
	case "index-usn":
		return cmdIndexUSN(args[1:])
	case "index-volumes":
		return cmdIndexVolumes(args[1:])
	case "upgrade-index":
		return cmdUpgradeIndex(args[1:])
	case "compact-index":
		return cmdCompactIndex(args[1:])
	case "augment-pngc":
		return cmdAugmentPNGC(args[1:])
	case "direct", "direct-v9":
		return cmdDirect(args[1:])
	case "service":
		return cmdService(args[1:])
	case "install":
		return cmdInstallService(args[1:])
	case "setup-service":
		return cmdSetupService(args[1:])
	case "launch":
		return cmdLaunch(args[1:])
	case "start":
		return cmdControlService(args[1:], "start")
	case "stop":
		return cmdControlService(args[1:], "stop")
	case "restart":
		return cmdControlService(args[1:], "restart")
	case "status":
		return cmdDoctor(args[1:])
	case "config":
		return cmdConfig(args[1:])
	case "plugin", "plugins":
		return cmdPlugin(args[1:])
	case "defaults":
		return cmdDefaults(args[1:])
	case "uninstall":
		return cmdUninstallService(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "service-index-usn":
		return cmdServiceIndexUSN(args[1:])
	case "loaded", "service-info":
		return cmdServiceInfo(args[1:])
	case "bench":
		return cmdBenchAgent(args[1:])
	case "ui":
		return cmdUI(args[1:])
	case "info":
		return cmdInfo(args[1:])
	case "search":
		return cmdSearch(args[1:], false)
	case "count":
		return cmdSearch(args[1:], true)
	case "content-index":
		return cmdContentIndex(args[1:])
	case "content":
		return cmdContent(args[1:])
	case "watch":
		return runWatch(args[1:])
	case "version":
		fmt.Printf("seekfs %s commit=%s date=%s\n", version, commit, date)
		return nil
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return nil
	case "agent":
		printAgentHelp()
		return nil
	case "syntax":
		printSearchHelp()
		return nil
	default:
		if looksLikeSearchWithoutSubcommand(args) {
			return cmdSearch(normalizeSearchArgs(args), false)
		}
		return usage()
	}
}

func looksLikeSearchWithoutSubcommand(args []string) bool {
	for _, arg := range args {
		if arg == "--under" || strings.HasPrefix(arg, "--under=") ||
			arg == "-db" || arg == "--db" || strings.HasPrefix(arg, "-db=") || strings.HasPrefix(arg, "--db=") ||
			arg == "-path" || arg == "--path" || arg == "--json" || arg == "-json" ||
			arg == "-n" || arg == "--n" || strings.HasPrefix(arg, "-n=") || strings.HasPrefix(arg, "--n=") ||
			arg == "-service" || arg == "--service" || arg == "-local" || arg == "--local" ||
			arg == "--exists" || arg == "-exists" || arg == "--cwd-bias" || arg == "-cwd-bias" ||
			arg == "-root-bias" || arg == "--root-bias" || strings.HasPrefix(arg, "-root-bias=") || strings.HasPrefix(arg, "--root-bias=") ||
			arg == "-recent" || arg == "--recent" || strings.HasPrefix(arg, "-recent=") || strings.HasPrefix(arg, "--recent=") ||
			arg == "-modified-after" || arg == "--modified-after" || strings.HasPrefix(arg, "-modified-after=") || strings.HasPrefix(arg, "--modified-after=") ||
			arg == "-case" || arg == "--case" {
			return true
		}
	}
	if len(args) > 1 && !strings.HasPrefix(args[0], "-") {
		return true
	}
	if len(args) == 1 && looksLikeImplicitFilenameGlob(args[0]) {
		return true
	}
	return false
}

func normalizeSearchArgs(args []string) []string {
	valueFlags := map[string]bool{
		"--under": true, "-db": true, "--db": true, "-n": true, "--n": true,
		"-config": true, "--config": true, "-pipe": true, "--pipe": true,
		"-root-bias": true, "--root-bias": true, "-recent": true, "--recent": true,
		"-modified-after": true, "--modified-after": true,
		"-interval": true, "--interval": true,
		"-exec": true, "--exec": true, "-exec-on": true, "--exec-on": true,
	}
	boolFlags := map[string]bool{
		"-path": true, "--path": true, "--json": true, "-json": true,
		"-service": true, "--service": true, "-local": true, "--local": true,
		"--exists": true, "-exists": true, "--cwd-bias": true, "-cwd-bias": true,
		"-case": true, "--case": true, "-exec-shell": true, "--exec-shell": true,
		"-fuzzy": true, "--fuzzy": true,
	}
	flags := make([]string, 0, len(args))
	query := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if valueFlags[arg] && i+1 < len(args) {
			flags = append(flags, arg, args[i+1])
			i++
			continue
		}
		if boolFlags[arg] || strings.Contains(arg, "=") && strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			continue
		}
		query = append(query, arg)
	}
	return append(flags, query...)
}

func usage() error {
	printUsage(os.Stderr)
	return errors.New("unknown or missing command")
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  seekfs index -root <path> [-root <path>...] [-db seekfs.db]
  seekfs index-usn -volume C: [-db seekfs.db]
  seekfs index-volumes [-volume C:] [-volume F:] [-index-dir path] [-launch]
  seekfs upgrade-index -db seekfs.gsi
  seekfs compact-index -db seekfs.gsi
  seekfs augment-pngc -db source.gsi -out target.gsi [-max-output-growth bytes] [-max-heap bytes] [-min-free-disk bytes]
  seekfs direct -out target.gsi (-root path | -records N) [-spool-dir path] [-run-records N] [-run-bytes bytes] [--json]
  seekfs launch [-db index.gsi...] [--json]
  seekfs install [-pipe \\.\pipe\seekfs-service] [-sddl <sddl>] [-db index.gsi...]
  seekfs setup-service [-pipe \\.\pipe\seekfs-service] [-sddl <sddl>] [-db index.gsi...] [-no-start]
  seekfs start|stop|restart
  seekfs uninstall
  seekfs doctor [--json]
  seekfs status [--json]
  seekfs loaded [--json]
  seekfs defaults [--json]
  seekfs config path|show|get|set
  seekfs plugin list|add|remove|enable|disable|config|doctor|reload|path
  seekfs service-index-usn -volume C: -db seekfs.gsi [-pipe \\.\pipe\seekfs-service]
  seekfs bench [-db index.gsi...] [-service] [-count] [--json] [-iterations 100]
  seekfs ui [-pipe \\.\pipe\seekfs-service] [-n 200]
  seekfs info [-db seekfs.gsi] [--json]
  seekfs syntax
  seekfs agent
  seekfs search [-db seekfs.db...] [--json] [-n 100] [-path] <query>
  seekfs count [-db seekfs.db...] [--json] [-path] <query>
  seekfs content-index (-root <dir> | -db <gsi>) [-out content.gsx] [-under <path>] [-ext .go,.md]
  seekfs content -db content.gsx [-n 100] [--json] <term>
  seekfs watch "<query>" [-interval 2s] [-n 10000] [-under PATH] [-exec CMD] [-pipe \\.\pipe\seekfs-service]
  seekfs version

Agent use:
  seekfs is for indexed file-name and path discovery, preferably through the
  resident service. It is not a content/symbol search tool; use rg for text
  matches.

  If you are an agent (LLM, script, or automation), run "seekfs agent" first:
  it explains how to query fast, what works well, and what to avoid so you
  do not fall into a slow full-volume scan.

  Quick start for agents:
    seekfs agent                -> agent-focused guidance (read this first)
    seekfs search --under <repo> "query"   -> repo-scoped discovery
    seekfs count --under <repo> "query"    -> count matching entries
    seekfs loaded --json        -> what volumes are indexed & fresh

Agent starting points:
  seekfs agent
  F:\git\seekfs\seekfs.exe agent
  seekfs config set output_format json
  seekfs config set default_limit 20
  seekfs search "gh.exe"
  seekfs --under F:\git\seekfs "main.go"
  seekfs search --under F:\git\seekfs "main.go"
  seekfs search -path "ext:go dir:cmd main"
  seekfs count -path "type:file ext:go"`)
}

func printSearchHelp() {
	fmt.Print(`seekfs search syntax

Supported today:
  plain text        Case-insensitive substring match against file name.
  multiple terms    Whitespace-separated terms are ANDed.
  -path             Match terms against the full path instead of just the name.
  -n <num>          Limit returned rows; default is agent-safe 100.
  --json            Emit machine-readable JSON.
  --under <path>    Only return results under a workspace/project.
  --exists          Verify result paths still exist on disk.
  --cwd-bias        Rank paths under the current directory first.
  --root-bias path  Rank paths under a specific root first.
  --recent 24h      Only return entries modified within a duration.
  --modified-after  Only return entries modified after RFC3339 or YYYY-MM-DD.
  --case            Case-sensitive matching.
  count             Print the number of matches instead of result paths.

Query filters:
  ext:go            Match exact extension without leading dot.
  dir:src           Match a directory/path segment substring.
  parent:src        Match entries whose immediate parent directory is src.
  glob:*.py         Match file name glob.
  regex:<pattern>   Match normalized full path regex.
  size:>100mb       Match file sizes with >, >=, <, <=, = and k/m/g/t units.
  dm:today          Match modification date macros, durations, or YYYY-MM-DD.
  attrib:H          Match file attributes; flags R,H,S,D,A are supported.
  sort:size         Rank results by file size ascending.
  sort:modified     Rank results by modification time, newest first.
  sort:extension    Rank results by extension, then name.
  sort:type         Rank directories before files, then name.
  sort:path         Rank results by full path.
  case:             Enable case-sensitive matching from the query.
  type:file         Only files.
  type:dir          Only directories.
  content:foo       Case-insensitive literal substring in file content; not a
                    whole-word match (foo matches foobar). Needs the
                    content plugin (seekfs plugin add content); content:/re/ is a regex
                    match for word boundaries or alternation.
  a|b               OR alternatives within a term, such as ext:png|jpg.
  !term, -term      Exclude a term or filter.

Examples:
  seekfs search "gh.exe"
  seekfs --under F:\git\seekfs "main.go"
  seekfs search -path "ext:go dir:cmd main"
  seekfs search -path --under F:\git\seekfs "type:file glob:*.md"
  seekfs search -path --exists --recent 24h "ext:go"
  seekfs search "ext:png|jpg"
  seekfs search "report !draft"
  seekfs search "size:>100mb"
  seekfs count "ext:log dm:today"
  seekfs count -path "type:dir docs"

Not implemented yet:
  quoted phrase parsing beyond the shell's normal argument grouping
  ranking compatible with Everything

Performance notes:
  Prefer filename-only search when you know the file name or executable name.
  Example: use "gh.exe" without -path to find the GitHub CLI binary.
  Use -path only when the query includes directory/path terms, dir:, --under,
  regex over full paths, or when path context is required. Full-path broad
  searches can be much slower on very large indexes.
`)
}

func printAgentHelp() {
	fmt.Print(`seekfs agent help

Purpose:
  Agent-first indexed file search for local filesystems. Prefer service mode
  for low latency; it avoids loading large indexes on each CLI invocation.

Recommended commands:
  seekfs loaded --json
  seekfs config set output_format json
  seekfs config set default_limit 20
  seekfs search "gh.exe"
  seekfs --under F:\git\seekfs "main.go"
  seekfs search --under F:\git\seekfs "main.go"
  seekfs search -path "ext:go dir:cmd main"
  seekfs count -path "type:file ext:go"
  seekfs launch -db F:\seekfs_c.gsi -db F:\seekfs_f.gsi
  seekfs config set output_format json
  seekfs bench -service --json -iterations 100
  seekfs bench -service -count --json -iterations 100

JSON result shape:
  {
    "ok": true,
    "query": "ext:go main",
    "count": 1,
    "limit": 20,
    "results": [{
      "path": "F:\\repo\\cmd\\seekfs\\main.go",
      "name": "main.go",
      "volume": "F:",
      "is_dir": false,
      "size": 123,
      "modified": "2026-05-22T12:00:00Z",
      "index_source": "walk"
    }]
  }

Useful search controls:
  --json              Required for robust automation.
  -service            Query the installed resident service; default when no -db is passed.
  -local              Do not auto-query the resident service.
  -path               Match full paths, not just names. Use only when needed.
  -n 20               Keep result sets bounded.
  --under <path>      Constrain search to a workspace.
  --exists            Filter stale index entries.
  --cwd-bias          Prefer current repo paths.
  --root-bias <path>  Prefer a specific repo/root.

Query filters:
  ext:go, parent:src, dir:src, glob:*.py, regex:<pattern>, case:, type:file, type:dir

Agent usage rules:
  seekfs searches indexed file names and paths, not file contents or symbols.
  Use rg for text-content search, definitions, import references, and line matches.
  For repo-local discovery, pass --under <repo> instead of relying on global ranking:
    seekfs search --under F:\git\seekfs "main.go"
    seekfs search -path --under F:\git\seekfs "ext:go dir:cmd main"
  Do not use -path with only a directory name when you want to list a tree.
  Add a file term/filter too, or use --under with a filename query.
  If seekfs is not on PATH, try the installed/repo binary directly:
    F:\git\seekfs\seekfs.exe search "gh.exe"

How to query well (learned from real agent usage):
  Prefer filename terms over path terms. Name-only search is the fastest,
  most reliable lane. Add -path only when the directory structure matters.
  Scope with --under whenever you know the workspace:
    seekfs search --under F:\git\seekfs "ext:go main"
  Use count first to probe scale before pulling results:
    seekfs count --under F:\git\seekfs "ext:go"
  Combine terms to narrow: the engine intersects filename terms, so
  "main.go router" finds files whose names contain both. The intersection
  shrinks the candidate pool and keeps the search fast.
  Use ext: to bound by type instead of adding a glob:
    seekfs search --under F:\git\seekfs "main ext:go"     (fast)
    seekfs search --under F:\git\seekfs "glob:*.go main"  (usually fine)
  When you need a file you know the approximate name of, a plain substring
  term beats a glob: "report" finds report.pdf, reports/, reporting.md.

Pitfalls that force the slow full-volume scan:
  - Leading wildcards with a rare literal: glob:*acme* has no indexed
    gram for the middle run, so it scans the whole index. Use a plain term
    ("acme") instead, or scope with --under.
  - Character-class globs (glob:*[ab]* or glob:*a?) cannot be reduced to a
    literal; they always scan. Prefer a plain term or regex.
  - Regex searches always scan the full path space. Prefer plain terms +
    ext:/dir: filters, and add --under.
  - Extremely common single terms with no other constraint (e.g. "log" alone
    across the whole disk) are broad; add a second term, an ext:, or --under.
  - Remember: seekfs is not a content search. Do not query for words inside
    files; that is rg's job.

Performance guidance:
  Start with filename-only search for exact names and executables:
    seekfs search "gh.exe"
  Add -path only for path-aware queries:
    seekfs search -path "ext:go dir:cmd main"

Fast-path tips (avoid the slow full-volume scan):
  - Bare "/" separators are ignored, but do not add them as query noise:
      good:  seekfs search "AGENTS.md pyproject.toml ext:py"
      slow:  seekfs search "AGENTS.md / pyproject.toml / ext:py"
  - For repo-local discovery always scope with --under; broad un-scoped
    multi-term queries can fall to a slow bounded scan:
      seekfs search --under F:\git\seekfs "ext:go dir:cmd main"
  - A filename glob is a substring match and is fast when its literal run
    is indexed:
      good:  seekfs search "glob:*report*"      (fast)
             seekfs search "glob:*notes.txt*"   (fast)
      slow:  seekfs search "glob:*acme*"        (rare word, grams not in
             the index -> falls back to a full scan)
    Prefer a plain filename term over a leading-* glob when you only need
    the middle of a name. If a rare-word glob is slow, run it once with a
    bounded result set (-n) or scope it with --under, or fall back to rg.
  - A query that yields zero results is often the fastest signal: the fast
    lane can prove "no match" in ms, while a slow path means the query
    could not be answered from the index (rare/odd grams, character-class
    globs, regex).

Working with results:
  Use --json for machine-readable output. The default human format is fine
  for eyeballing. Set default_limit so a broad query does not flood:
    seekfs config set default_limit 20
  If a file is not found, check the index is current first:
    seekfs loaded --json   -> volumes, states, checkpoint freshness
    seekfs doctor          -> per-volume health and replay state
  A missing file that rg finds is usually one of: it is newer than the last
  index build, it lives in an unindexed location (e.g. the seekfs dir itself
  is never indexed), or the name never matched. Check loaded/doctor before
  assuming a bug.

Config:
  seekfs reads seekfs.toml from the current directory, the user config dir, or
  the seekfs dir (SEEKFS_DIR env, else %ProgramData%\seekfs). The seekfs dir is
  where the config and volume indexes live; it is never indexed itself.
  Supported keys: dbs, db, db_paths, db_path, volumes, volume, service_pipe,
  default_limit, output_format, seekfs_dir.

Errors:
  With --json, errors are written to stderr as:
  {"ok":false,"error":"message"}
  and the process exits nonzero.
`)
}

func cmdIndex(args []string) error {
	var roots stringList
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "index database path")
	fs.Var(&roots, "root", "root to index; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(roots) == 0 {
		return errors.New("index requires at least one -root")
	}

	start := time.Now()
	idx := &Index{Version: indexVersion, Roots: roots, BuiltAt: time.Now(), Source: "walk"}
	for _, root := range roots {
		if err := walkRoot(root, idx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", root, err)
		}
	}
	buildOrders(idx)
	ensureCompactIndexForService(idx)
	if err := saveIndex(*db, idx); err != nil {
		return err
	}
	fmt.Printf("indexed %d entries in %s\n", len(idx.Entries), time.Since(start).Round(time.Millisecond))
	return nil
}
