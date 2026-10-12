# seekfs

[![Windows CI](https://github.com/abhim-dv/seekfs/actions/workflows/windows.yml/badge.svg)](https://github.com/abhim-dv/seekfs/actions/workflows/windows.yml)

`seekfs` is a Windows-first CLI and service for fast local file-name and
full-path search. It builds compact local indexes and can keep them resident in
a Windows service so command-line searches avoid loading large databases on
every invocation.

`seekfs` is independent software. It is not affiliated with, endorsed by, or
sponsored by voidtools or Everything.

> **Getting the most out of seekfs:** use `seekfs search`/`seekfs count` against
> the resident service, put flags before the query (`--under <folder>` to scope,
> `-path` for path syntax), and write quoted multi-term or filter-style queries
> like `ext:go dir:cmd main` — then drop to `rg` only for file-content or line
> matching, which seekfs intentionally does not index.

## Who It Is For

`seekfs` is for local developer and agent workflows that need fast,
machine-readable file discovery from a CLI. It is currently focused on Windows
and NTFS/USN indexing.

## Quickstart

Build:

```powershell
go build -o seekfs.exe ./cmd/seekfs
```

Create a small fallback index:

```powershell
.\seekfs.exe index -root $env:USERPROFILE\Documents -db .\docs.gsi
.\seekfs.exe search -db .\docs.gsi -n 20 report
.\seekfs.exe search -db .\docs.gsi -path -n 20 "src main"
.\seekfs.exe count  -db .\docs.gsi report
```

## Service Setup

Use the service for full-volume NTFS/USN indexing and resident search.

```powershell
.\seekfs.exe index-volumes -volume C: -volume F: -launch
```

Preview defaults before indexing:

```powershell
.\seekfs.exe defaults --json
.\seekfs.exe index-volumes --dry-run --json
```

Reinstall the service with the indexes it should keep resident:

```powershell
.\seekfs.exe launch -db F:\seekfs_c.gsi -db F:\seekfs_f.gsi
```

Query through the service:

```powershell
.\seekfs.exe config set output_format json
.\seekfs.exe config set default_limit 20
.\seekfs.exe search "gh.exe"
.\seekfs.exe --under F:\git\seekfs "main.go"
.\seekfs.exe search --under F:\git\seekfs "main.go"
.\seekfs.exe search -path "ext:go dir:cmd main"
.\seekfs.exe count  -path "ext:go dir:cmd main"
```

When no `-db` is supplied, `search` and `count` use the resident service by
default. Use `-local` to skip the service and read a local DB file instead.

## Native Desktop UI (`seekfs-native`)

`seekfs-native.exe` is a GPU-rendered desktop search window (Gio) that talks
to the resident service over its named pipe — same JSON protocol as the CLI.
Build and run it against a standalone service (no admin needed):

```powershell
# Serve the prebuilt Chromium filename index on a dev pipe:
E:\bench\seekfs-content.exe service -pipe \\.\pipe\seekfs-native-dev -sddl "D:P(A;;GA;;;OW)(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;AU)" -db E:\bench\chromium.gsi

# In this repo (all go commands via the shared build lock):
bash /e/bench/gorun.sh build -ldflags=-H=windowsgui -o E:/bench/seekfs-native.exe ./cmd/seekfs-native
E:\bench\seekfs-native.exe -pipe \\.\pipe\seekfs-native-dev
```

Flags: `-pipe` (service pipe, default `\\.\pipe\seekfs-service`), `-n`
(result limit). Environment: `SEEKFS_NATIVE_QUERY` runs one search at
startup (deterministic screenshots), `SEEKFS_NATIVE_DPISCALE=1.5` scales all
layout for scaling tests, `SEEKFS_NATIVE_DEBUGLOG=<file>` logs search
delivery and frames, `SEEKFS_NATIVE_SETTINGS` overrides the settings path
(`%APPDATA%\seekfs\native.json` by default).

Features: instant debounced search, Contents toggle (`content:` rewrite with
snippet matches), Details/Icons/grouped views, sortable/resizable columns,
sidebar (Favorites, indexed volumes, saved searches), preview pane (text
with match highlight, images, metadata), Explorer shell menu, drag-out,
Recycle Bin delete, in-place rename, back/forward history, system
light/dark theme with accent color, per-monitor DPI.

Content search needs an NTFS change-journal content index; without one the
UI reports "content unavailable" instead of empty results.

## Examples

Search file names:

```powershell
.\seekfs.exe search main
.\seekfs.exe search "gh.exe"
```

Search full paths:

```powershell
.\seekfs.exe search -path "src cmd"
```

Agent-friendly filters:

```powershell
.\seekfs.exe search -path "ext:go dir:cmd main"
.\seekfs.exe search -path --under F:\git\seekfs "type:file glob:*.md"
.\seekfs.exe search -path --exists --recent 24h "ext:go"
.\seekfs.exe search "ext:png|jpg"
.\seekfs.exe search "report !draft"
.\seekfs.exe search "size:>100mb"
.\seekfs.exe count  "ext:log dm:today"
.\seekfs.exe bench -service --json -iterations 100
.\seekfs.exe bench -service -count --json -iterations 100
```

Performance note for agents: prefer filename-only search when looking for a
known file or executable name. Use `-path` only when the query needs directory
terms, `dir:`, `--under`, regex over full paths, or path context. Broad
full-path searches can be much slower on very large indexes.

Agent usage note: `seekfs` searches indexed file names and paths, not file
contents or symbols. Use `rg` for text-content search, definitions, import
references, and line matches. For repo-local file discovery, use `--under` to
avoid unrelated machine-wide results. If `seekfs` is not on PATH in a fresh
agent shell, call the binary directly, for example `F:\git\seekfs\seekfs.exe`.
For agent convenience, omitting the `search` subcommand also works for search
flags, for example `seekfs --under F:\git\seekfs "main.go"`.
Bare wildcard filename queries such as `seekfs "*_test.go"` are also treated
as filename globs without requiring an explicit `glob:` prefix.

Inspect an index:

```powershell
.\seekfs.exe info -db F:\seekfs_c.gsi
```

## Current Benchmark Snapshot

On the development machine, packed v9 C: + F: indexes measured:

```text
C: + F: entries: about 23.7M
steady service working set: about 3.1 GB after loading
expanded benchmark-cache working set: about 3.3 GB
```

Representative warm service CLI timings:

- `-path "src main.go"`: about 50-100 ms.
- `-path "src"`: about 500 ms on the broad scan path.
- `count ext:md`: about 90-100 ms.
- Public benchmark query suites ran with zero failures before release.

## Limitations

- Windows and NTFS are the primary target.
- Result ranking is simple and not Everything-compatible.
- Directory sizes are the recursive size of their contents, persisted in the v9
  index and updated live: when the journal reports a file create/modify/delete,
  the file's metadata is re-read from disk and the change propagates to its
  ancestor folders (the same way Everything maintains folder sizes). `sort:size`
  and `size:` filters order and match directories by that recursive total. A
  directory created since the last index build reports 0 until it is folded
  into the base.
- File sizes and modification times are re-read from disk when the journal
  reports a change, so a modified or renamed file no longer reports 0 between
  persists.
- `size:`, `dm:`, and `attrib:` require an index built with file metadata. NTFS
  service indexes capture this from the MFT; older indexes without this metadata
  return a clear error for these filters.
- Index files contain local path names and should be treated as sensitive local
  metadata.

## Documentation

- [Service setup](docs/SERVICE.md)
- [Search syntax](search-syntax.md)
- [Configuration](docs/CONFIG.md)
- [Service pipe protocol](docs/OPEN_PROTOCOL.md)
- [Benchmarks](docs/BENCHMARKS.md)
- [Security notes](SECURITY.md)

## Config Shortcuts

```powershell
.\seekfs.exe config path
.\seekfs.exe config set output_format json
.\seekfs.exe config set dbs = '["F:\\seekfs_c.gsi", "F:\\seekfs_f.gsi"]'
.\seekfs.exe defaults --json
```

## Development

```powershell
go test ./...
go vet ./...
go build -o seekfs.exe ./cmd/seekfs
powershell -ExecutionPolicy Bypass -File .\scripts\test_seekfs_cli.ps1
```

## Release Artifacts

The initial release artifact is an unsigned zip:

```text
seekfs-windows-amd64.zip
```

It contains `seekfs.exe`, README, license, notice, and docs. Windows may warn
about unsigned executables until code signing is added.
