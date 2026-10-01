# Configuration

`seekfs` can read a small `seekfs.toml` file from the current directory or from:

```text
%AppData%\seekfs\seekfs.toml
```

You can also pass an explicit path:

```powershell
.\seekfs.exe search -config .\seekfs.toml -service "main"
```

Supported keys:

```toml
dbs = ["F:\\seekfs_c.gsi", "F:\\seekfs_f.gsi"]
volumes = ["C:", "F:"]
service_pipe = "\\\\.\\pipe\\seekfs-service"
default_limit = 100
output_format = "json"
```

With `output_format = "json"` and `default_limit` set, agent calls can stay
short:

```powershell
.\seekfs.exe search "gh.exe"
.\seekfs.exe search -path "ext:go dir:cmd main"
```

When no `-db` is supplied, `search` and `count` use the resident service by
default. Pass `-local` to skip the service and load the configured/default DB
from disk.

Single-value aliases are also accepted:

```toml
db = "F:\\seekfs_c.gsi"
volume = "C:"
db_path = "F:\\seekfs_c.gsi"
db_paths = ["F:\\seekfs_c.gsi", "F:\\seekfs_f.gsi"]
```

This parser intentionally supports only the simple string, integer, and string
array forms used by `seekfs`.

## Optional features

Use the plugin CLI to select and configure optional features:

```powershell
seekfs plugin list
seekfs plugin add content
seekfs plugin doctor content --wait
seekfs plugin config content --root F:\work --git=false
```

Plugin settings are written to `plugin.toml` beside the service's `seekfs.toml`;
the plugin CLI leaves core config intact and applies changes live. Legacy
`[features.<name>]` and `[content]` settings remain supported. Other compatible
executables can be registered with `plugin add NAME --command <absolute-path>`.
See [FEATURES.md](FEATURES.md) for configuration,
query syntax, limits and the companion protocol. `seekfs loaded --json` reports
feature health independently of filename-search health.

## Editing Config

Use `seekfs config` so agents and users do not need to locate the file manually:

```powershell
.\seekfs.exe config path
.\seekfs.exe config show
.\seekfs.exe config set output_format json
.\seekfs.exe config set default_limit 20
.\seekfs.exe config set dbs = '["F:\\seekfs_c.gsi", "F:\\seekfs_f.gsi"]'
.\seekfs.exe config get dbs
```

## Build tuning (name trigrams)

Index builds are bounded and parallel by default. These environment variables
exist for low-memory or unusual environments; the defaults are fine otherwise.

| Variable | Meaning | Default |
| --- | --- | --- |
| `SEEKFS_NAME_GRAM_EXTERNAL` | `0` forces the in-memory name-gram builder; any other value forces the external, bounded-memory builder | external for real volumes, in-memory for small indexes |
| `SEEKFS_NAME_GRAM_EXTERNAL_MIN_RECORDS` | Record-count floor below which the in-memory builder is used | `250000` |
| `SEEKFS_NAME_GRAM_SPOOL_DIR` | Directory for external-builder spill files | `%ProgramData%\seekfs\gram-spool` (the seekfs dir, which is never indexed) |
| `SEEKFS_GRAM_SPILL_BYTES` | Per-worker spill buffer budget, in bytes | `67108864` (64 MiB) |
| `SEEKFS_GRAM_SPILL_WORKERS` | Parallel spill workers | `GOMAXPROCS`, capped at 8 |
| `SEEKFS_GRAM_MERGE_WORKERS` | Parallel merge workers | `GOMAXPROCS`, capped at 8 |
| `SEEKFS_GRAM_MERGE_FANIN` | Maximum runs merged per pass (minimum 2) | `32` |

## Runtime tuning (service)

These knobs tune the resident service and query engine. Leave them unset unless
you have a specific memory or latency problem; the defaults are tuned for a
normal machine. Booleans accept `1`/`true`/`yes`/`on` to enable and
`0`/`false`/`no`/`off` to disable, except the persist diagnostics below, which
only accept the literal `1`.

### Memory

| Variable | Meaning | Default |
| --- | --- | --- |
| `SEEKFS_MEMORY_MODE` | `lowmem` (aliases `mmap`, `low-memory`) keeps indexes memory-mapped and drops the heaviest resident view; empty keeps everything resident | empty |
| `SEEKFS_GO_MEM_LIMIT_MB` | Go runtime soft memory limit, in MiB | runtime default |
| `SEEKFS_IDLE_RELEASE_SECONDS` | Idle time before the service runs a GC and returns unused memory to the OS | `900` |
| `SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING` | Low-memory cap on stored trigram posting length | `250000` |

### Service startup

| Variable | Meaning | Default |
| --- | --- | --- |
| `SEEKFS_STARTUP_WORKERS` | Parallel per-volume startup loaders, capped at the configured DB count | `2` (`1` for a single DB) |
| `SEEKFS_DISABLE_BACKGROUND_PERSIST` | `1` disables the debounced background persist loop (debug/tests) | off |

### Query and postings

| Variable | Meaning | Default |
| --- | --- | --- |
| `SEEKFS_POSTING_CACHE_MB` | Mapped posting-block LRU cache budget, in MiB; `<= 0` disables it | `128` |
| `SEEKFS_QUERY_POSTING_PREFETCH_BYTES` | Per-query posting prefetch budget, in bytes | `33554432` (32 MiB) |

### Search accelerators

These gate the resident secondary indexes. They are on by default; set `0` to
force the slower fallback paths. Under `SEEKFS_MEMORY_MODE=lowmem` the defaults
for `SEEKFS_SUBTREE_INTERVALS` and `SEEKFS_NAME_ORDER` flip off, while
`SEEKFS_PATH_GRAMS` and `SEEKFS_NAME_TRIGRAMS` stay on.

| Variable | Meaning | Default |
| --- | --- | --- |
| `SEEKFS_SUBTREE_INTERVALS` | Subtree interval metadata for `dir:` / `--under` | on |
| `SEEKFS_PATH_GRAMS` | Path component postings | on |
| `SEEKFS_NAME_ORDER` | Resident name-order rank | on |
| `SEEKFS_NAME_TRIGRAMS` | Name trigram index | on |
| `SEEKFS_NAME_TRIGRAM_MAX_RECORDS` | Record cap above which name trigrams are not built at startup; ignored in low-memory mode or when `SEEKFS_NAME_TRIGRAMS` is set explicitly | `20000000` |
| `SEEKFS_SELF_NAME_GRAMS` | Selective/complete name gram sections for filename queries | on |
| `SEEKFS_GLOBAL_PLANNER` | Force the global multi-volume planner | off (the `ui` launcher sets `1`) |

### Persistence diagnostics

| Variable | Meaning | Default |
| --- | --- | --- |
| `SEEKFS_PERSIST_TRACE` | `1` logs per-stage heap stats during index persist (previous spelling `SEEKFS_V9_PERSIST_TRACE` also accepted) | off |
| `SEEKFS_PERSIST_PROFILE_DIR` | Directory for per-stage heap `pprof` dumps (previous spelling `SEEKFS_V9_PERSIST_PROFILE_DIR` also accepted) | unset |

### Config discovery

| Variable | Meaning | Default |
| --- | --- | --- |
| `SEEKFS_DB` | Default index path when no `-db`/`dbs` is configured | user cache dir `\seekfs\index.gsi` |
| `SEEKFS_DIR` | Overrides the seekfs data directory | `%ProgramData%\seekfs` |
