# Optional seekfs features

Filename/path search remains the core service. Features are selected at service
startup or through the plugin CLI: content is built in; other compatible services run as managed companion
executables. Feature failures appear separately in `loaded --json` and do not
make filename search unavailable.

## Configuration

Use the CLI for setup and live changes:

```powershell
seekfs plugin list
seekfs plugin add content
seekfs plugin doctor content --wait
seekfs plugin config content --root F:\work --git=false --budget-bytes 1073741824
seekfs plugin disable content
seekfs plugin enable content --wait
seekfs plugin remove content
```

`add` saves configuration and starts activation/indexing in the running service
in the background. `doctor --wait` shows progress and waits for ready (default
timeout 10 minutes). A timeout does not cancel indexing. `--json` emits structured
status; `plugin path` prints the registry location. Disabled/removed content can
be re-added without restarting seekfs, reusing a valid persisted sidecar.

Plugins live in **`plugin.toml` beside the selected `seekfs.toml`**, independently
of the core config. By default the CLI discovers the running service's pinned
plugin file, so working-directory changes do not edit a different registry.
`--config`, `--plugin-config` and `--pipe` select an explicit setup/instance.
Listing/doctor are local read operations; live activation uses the privileged
service's existing elevated-mutation permission. The service reads its own
registry, not a client-supplied executable configuration.

Register another compatible service:

```powershell
seekfs plugin add tags --command C:\Tools\seekfs-tags.exe --arg=--scope --arg notes
seekfs plugin doctor tags --wait
```

`content` is the bundled catalog entry. Other names require a supplied compatible
executable; no tagger is bundled. `plugin list` includes catalog availability,
installation/enabled state, runtime readiness and diagnostics.

The resulting `plugin.toml` looks like:

```toml
[plugins.content]
installed = true
enabled = true
scope = 'auto'

# Example provider name/path: a compatible tags executable is not bundled.
[plugins.tags]
installed = true
enabled = true
command = 'C:\Tools\seekfs-tags.exe'
args = ['--scope', 'notes']
timeout_seconds = 5
```

- Features default to disabled. Disabled companions do not need an executable.
- `command` is an absolute executable path; arguments are passed directly, with
  no shell expansion. Names use lowercase letters, digits, `_` and `-`, starting
  with a letter (maximum 64 characters). `content` is reserved.
- `timeout_seconds` bounds each IPC request, defaults to 5, and accepts 1–60.
- Feature argument arrays support quoted strings, commas inside strings, and
  single-quoted literal Windows paths. Unknown feature keys are rejected.
- Content's existing `[content]` and per-volume settings in `seekfs.toml` still
  apply. Global plugin scope/roots/excludes/extensions/git/budget can be edited by
  `plugin config`; per-volume/environment scope overrides retain their precedence.
- An explicit content entry in `plugin.toml` controls live activation, including
  removal, over legacy `[features.content]` and environment opt-in. Legacy setups
  remain supported when no plugin entry exists.
- CLI changes apply live; after manual edits use `seekfs plugin reload`.
  Disable/remove cancel workers and release mappings/locks safely while retaining
  indexed data. Core filename search keeps running. A removal tombstone prevents
  legacy config/environment from silently re-enabling the plugin.
- If the service is offline, settings are saved for its next startup. Doctor
  reports that it needs to be started. Older service binaries need an upgrade/
  restart before they can accept live plugin commands.

Standalone startup with an explicit configuration:

```powershell
seekfs service -config C:\ProgramData\seekfs\seekfs.toml
```

Inspect the resident instance:

```powershell
seekfs loaded --json
```

The `features` array reports enabled/state/error and optional progress. `plugin
list --json` and `plugin doctor --json` report installation and readiness. Content
also retains its detailed per-volume `content` health.

## Queries

Built-in content keeps its existing syntax and optimized planner:

```powershell
seekfs search 'ext:go content:"connection refused"'
```

Queryable companions use `feature:<name>:<term>`:

```powershell
seekfs search 'feature:tags:blue ext:go'
seekfs search 'feature:tags:"two words" !ext:tmp sort:modified' -n 20
seekfs count 'feature:tags:blue|ext:md'
```

The companion interprets the term and honors the supplied case policy. Seekfs
combines predicate results with its filename/scalar filters and AND/OR/NOT tree,
resolves current paths, orders the result page and applies the limit. Quoted
terms preserve spaces and literal `|`; content regexes can also participate in
the same Boolean expression. An opaque feature term does not turn filename
matching into full-path matching merely because it contains a slash.

Generic feature queries are local service queries. Offline index search and
remote feature predicates are refused. Relevance sorting, fuzzy rewriting and
watch predicates are not supported for generic companions. Mixed content/feature
queries apply content predicates but do not add content snippets. Background-only
companions can omit query capability and still receive lifecycle/metadata updates.

### Initial query ceilings

These ceilings fail explicitly, including for counts; a partial count is never
reported as exact:

- At most 100,000 candidate IDs retained across distinct predicates/volumes.
- At most 100,000 verified matches across volumes.
- A top-level feature conjunction uses its smallest FRN set as a candidate
  superset. Queries driven only by OR/NOT use a bounded fallback scan (at most
  100,000 base/overlay slots per volume).
- Only the requested result page is retained, using a top-N heap. Its row/string
  storage has a conservative 16 MiB budget; lower `-n` if that budget is reached.
- Filename overlay copies are limited to 100,000 slots; a larger overlay asks the
  caller to wait for filename compaction rather than allocate an unbounded copy.

These are the generic adapter's limits. Content retains its existing independent
planner/budgets. Another feature can gain a native fast lane when a real workload
needs it.

## Companion protocol v1

The public Go message definitions and bounded frame helpers are in
[`feature/protocol.go`](../feature/protocol.go). The wire format is ordinary JSON,
so companions can be implemented in other languages.

### Transport and ownership

- Seekfs launches the configured executable. Stdin carries requests; stdout
  carries responses. Each message is one JSON line, strictly below 1 MiB.
  Flush every response; write diagnostics to stderr.
- Requests are serialized per companion. Every response echoes `version: 1` and
  the request's `id`, and includes `ok`. An operation error uses `ok: false` and
  `error`; this does not restart an otherwise healthy process.
- Companions must wait for `hello` before starting work. They run under the seekfs
  service account and receive local indexed-file metadata.
- Data belongs under `<seekfs_dir>\features\<name>`. The host supplies the absolute
  directory and holds an ownership lock across companion restarts. Configured
  seekfs data directories are excluded from metadata snapshots/change delivery.
- A Windows Job terminates the process tree when the host exits, including a
  host crash. Protocol faults, broken pipes and request timeouts restart the
  companion with backoff, capped at one attempt per 30 seconds after repeated
  failures. Ordinary application/query errors do not restart it.
- `deadline_unix` is a Unix-nanosecond request deadline; honor it for expensive
  operations. On query cancellation the host briefly drains a quick reply to
  keep a healthy warmed process; a stalled request is terminated. A reconnect
  always starts with fresh snapshots.

### Handshake

Host:

```json
{"version":1,"id":1,"op":"hello","feature":"tags","data_dir":"C:\\ProgramData\\seekfs\\features\\tags","deadline_unix":1790000000000000000}
```

Companion:

```json
{"version":1,"id":1,"ok":true,"complete":true,"capabilities":["query"]}
```

`query` is the initial optional capability. Empty capabilities designate a
background-only feature. Snapshot/change/health/removal operations are required.

### File inventory and changes

Each volume context contains:

| Field | Meaning |
| --- | --- |
| `index_id` | Stable hash of the normalized database path; use this to key active inventory tables. Multiple databases may describe the same drive. |
| `id` | Human-readable volume name, e.g. `C:`. |
| `journal_id` | NTFS journal generation. |
| `generation` | Host invalidation epoch; changes on index replacement/directory path invalidation. Not a durable physical file identity. |
| `checkpoint` | Filename-index USN checkpoint represented by the inventory. |
| `cursor` | Host change-feed position represented by this update. |

Preserve 64-bit integer values when decoding JSON. A record carries the indexed
`frn`, current path/name, file size, modification time in Unix nanoseconds and
mode. FRNs are seekfs's normalized NTFS record numbers, not full sequence-bearing
physical file IDs. `mode` combines Windows attribute bits with the high
`os.ModeDir` bit. Directory size is zero, not an aggregate subtree estimate.
Deletes carry `frn` and `deleted: true`.

The host sends an authoritative, transactionally staged inventory:

1. `snapshot_begin` with the target volume context. Discard any abandoned staging
   transaction and start a fresh inventory for this `index_id`.
2. Zero or more `records` requests. Pages contain at most 256 records and are
   split further when long paths would exceed the frame limit.
3. `snapshot_commit` with the same target context. Publish the staged inventory
   atomically and acknowledge it. Reuse validated extraction caches if desired;
   membership/path metadata comes from the new inventory.

For an incremental update:

1. `changes_begin` includes the target `volume` and the last committed `previous`
   context. Validate the previous context before starting a staging transaction.
2. `records` carries current upserts and tombstones; there may be no records when
   only the checkpoint advanced.
3. `changes_commit` atomically publishes the update and target context.

Retain the committed inventory when staging fails; the next begin abandons the
unfinished transaction. Acknowledgments should be prompt: queue extraction in
the companion's own bounded workers and report incompleteness until it finishes.

The host retains a 4,096-change replay window. Gaps, directory moves/deletes,
base replacement and companion reconnects force an authoritative snapshot.
Snapshots can finish while ordinary replay continues; later changes are applied
from their captured cursor. Retired contexts receive `volume_remove`; forget
their active inventory and cancel their work. Cache-retention policy remains the
companion's responsibility.

Only ready compact NTFS indexes with a valid journal checkpoint are supported in
v1. The host briefly locks the filename view to copy bounded metadata pages and
never performs IPC while holding the filename-index/replay locks. Records and
returned result strings do not retain aliases into an unmapped index.

### Health and queries

`health` includes a volume context. Reply with `complete: false` while extraction
or maintenance makes that inventory incomplete; `message` may explain why.
Optional `progress` is `{ "current": 120, "total": 500, "unit": "files" }`.
Counters must be nonnegative; `total: 0` means unknown. The host polls health even
when file metadata is unchanged.

`query` includes a volume context, opaque `query` text, `case_sensitive`, a `limit`
and a deadline. Return `frns` for that inventory and `complete: true` only when
the set is exhaustive for the term. The list must fit both the requested limit
and the frame cap. Return `complete: false` for a capped or unfinished answer.
Seekfs refuses such searches/counts, and refuses answers whose filename context
changed before verification. It evaluates the joint Boolean predicate itself;
companions should not attempt filename filtering, global sorting or pagination.

## Implementation phases and checks

| Phase | Delivery |
| --- | --- |
| 1 | Trace startup/replay/replacement/query boundaries; record the phased plan. |
| 2 | Validate feature configuration; route content through shared lifecycle and applied-change hooks. |
| 3 | Managed companion processes, versioned IPC, snapshots/replay recovery, health/progress and query capability. |
| 4 | Joint query semantics, bounded top-N/candidate retention, real subprocess recovery/isolation checks, and this usage guide. |

The CLI setup phase adds the separate atomic/locked `plugin.toml`, live reload
queue, per-plugin cancellation and readiness diagnostics. Tests in
`cmd/seekfs/plugin_test.go` exercise CLI add → ready → query → remove → re-add,
queued-build cancellation, live companion replacement and mutation/path guards.

`cmd/seekfs/feature_companion_test.go` contains a working metadata-name companion
used as a real subprocess. Checks cover configuration opt-in, Boolean/case/filter/
sort/count semantics, multiple indexes/volumes, replay gaps, directory renames,
process restarts, malformed/oversized replies, application errors, cancellation,
long-path frames and termination after an abrupt host exit.

```powershell
go test ./...
go vet ./...
```
