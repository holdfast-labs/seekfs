# seekfs v1.12.0

This release turns content search on by default, adds the native desktop app
with a Settings window, and improves name-search speed and accuracy.

## Content search

Content search finds text inside files, not only names and paths. The index
holds trigram postings and a per-file filter. Each match and each snippet is
verified against the current file on disk. A file edited since the last index
sync is read directly; terms the edit adds become searchable after the index
catches up. (`docs/CONTENT_SEARCH.md`)

- `content:` matches a case-insensitive literal substring. `content:/re/` is a
  Go RE2 regex with no lookaround. An uppercase literal makes content matching
  case-sensitive unless `case:` is explicit.
- `content~:word` and `content~N:word` match an indexed word within an edit
  distance. A `content:` query with zero results retries with per-word
  dictionary correction. Exact results are never altered, and regex, NOT and
  `case:true` leaves are never corrected. (`docs/CONTENT_SEARCH.md`)
- `sym:Name` finds definition lines by source language. `sort:relevance` adds a
  definition bonus for the defining file. (`docs/CONTENT_SEARCH.md`)
- `--lines` prints ripgrep-style `path:LINE:text` output. `-C`, `-A`, `-B` and
  `--max-per-file` control context and the per-file limit.
  (`docs/CONTENT_SEARCH.md`)
- `--budget 150ms` bounds disk verification. `--cursor` and `next_cursor` page
  through results. Each page reruns the query up to its offset, so cost grows
  with page depth. (`docs/CONTENT_SEARCH.md`)

### Scope

In `auto` mode, each volume includes detected Git worktrees first, then
configured roots, then known folders (Documents, Desktop and Downloads), then
the volume root as the final lowest-priority include. Text files outside every
repo and known folder are therefore in scope. Excludes, Git ignore rules, and
the built-in dependency, cache, build-output and binary skips still apply. The
budget fills repos and configured roots before known folders, and known folders
before drive-root files. `explicit` mode uses the configured roots only, and
`off` disables content for the volume. (`docs/CONTENT_SEARCH.md`)

### Disk and budget

There is no default `budget_bytes`. The build stops when its projected peak
disk use would push free space on the sidecar's volume below a reserve. The
reserve is 5% of the volume capacity, clamped to 10 GiB, so it is 10 GiB on any
volume of known size. The projection includes the compressed text spool and the
index, and it tightens when text does not compress well. An explicit
`budget_bytes` caps the projected final index size. Health reports
`scope_dropped` and the enforced limit. (`docs/CONTENT_SEARCH.md`)

### Turn it off

Set `SEEKFS_CONTENT_SEARCH=0`, set `[features.content] enabled = false` in
`seekfs.toml`, or use the native app's Settings window (Content search). An
explicit entry in `plugin.toml` also controls the feature.
(`docs/CONFIG.md`, `docs/FEATURES.md`, `docs/SETTINGS_API.md`)

### First build and health

The first build reads every in-scope file and extracts its text. Antivirus
scanning can make this build slow: cold opens measured about 7-13 ms per file
on the blink/renderer/core fixture, against 0.03 ms warm.
(`docs/WORK_QUEUE.md`)

`seekfs loaded --json` reports feature state and per-volume content health,
progress and inclusion counts. Extraction errors are grouped by reason.
(`docs/FEATURES.md`, `docs/CONTENT_SEARCH.md`)

## Desktop app

`seekfs-native.exe` is a native, GPU-rendered window built with Gio. It has a
Finder/Explorer-style layout. The sidebar holds favorites, volumes and saved
searches. The window has Details, Icons and grouped views, a preview pane, the
Explorer context menu, drag-out, rename, and Recycle Bin delete. It follows the
system theme and accent color, and its Contents toggle searches inside files.
(`README.md`)

The Settings window has three sections: **Drives**, **Content search** and
**Search**. Drives lists local fixed NTFS volumes plus indexed volumes that are
not fixed NTFS, and can add or remove an index. Content search controls
enablement, mode, roots, excludes, extensions, Git rules, system excludes and
the byte budget. Changes write the same `seekfs.toml` and `plugin.toml` files as
the CLI, and they apply live. (`docs/SETTINGS_API.md`)

The Settings window is being finished on a parallel branch. This release
includes the service side: `settings-get` and `settings-set` over the service
pipe, with an elevated caller for changes and an asynchronous volume build.
`seekfs-ui.exe` remains available. (`docs/SETTINGS_API.md`)

## Name search

- **Ranking signals.** Usage (opens, frecency and same-query picks), Git dirt
  and recent commits, and `near:` proximity reorder results within a rank band.
  They do not change the result set or the counts. On the chromium walk index
  (555,849 records, 100 labeled targets), seeded usage raised plain-name top-1
  from 39.0% to 66.0%. Partial-name top-1 rose from 2.0% to 13.0%. Never-opened
  control targets did not move. (`docs/BENCHMARKS.md`) `seekfs open <path>
  [--query q]` records an open, and `seekfs recent` lists recent opens. The
  store is local. (`docs/REFERENCE_TOOLS.md`)
- **Typo tolerance.** One edit is accepted, or two for words of 8 or more
  letters, with Damerau transpositions. On the same index, one-typo top-1 is
  99.5% (N=200, seed 42) and two-typo top-1 is 93.0% (N=100, seed 7).
  Auto-fuzzy correction runs only when exact matching returns zero hits.
  (`docs/BENCHMARKS.md`, `docs/WORK_QUEUE.md`)
- **Query language.** Name anchors `'exact`, `^prefix` and `suffix$`; `**` and
  brace globs such as `glob:src/**/*.{c,h}`; `in:<dir>` as an alias of
  `--under`; and accent folding, so `resume` finds `résumé`. Globs and anchors
  use postings and do not scan the whole volume.
  (`docs/BENCHMARKS.md`, `docs/REFERENCE_TOOLS.md`)
- **Freshness and footprint.** On the 1.46M-name fixture, create, rename and
  delete appear in 83-350 ms, 82 ms and 59 ms. A write-only edit updates
  `size:` and `dm:` in about 600 ms. After idle release the working set falls
  to about 72 MB. (`docs/BENCHMARKS.md`)

## Service and reliability

- **Settings API.** `settings-get` returns the volumes, their index state and
  the content options. `settings-set` adds or removes a volume and changes the
  content options. Both use the service pipe and the same config files as the
  CLI. Only an elevated caller may change settings, and a volume add returns
  before the background build finishes. (`docs/SETTINGS_API.md`)
- **Client timeout.** Content requests carry the client's deadline. The service
  uses a 2 s deadline inside the client wait, and a capped result is a partial
  page marked incomplete. The client cancels only on a pipe disconnect.
  (`docs/WORK_QUEUE.md`)
- **Pipe stop race.** A service stop that raced a client connect could hand a
  handler a closed pipe handle whose number was reused. That is fixed.
  (`docs/WORK_QUEUE.md`)
- **Extraction errors.** `loaded --json` now groups extraction errors by reason.
  Vanished files are not errors. Paths past `MAX_PATH` open with the
  extended-length prefix. Test output stays out of the service log.
  (`docs/WORK_QUEUE.md`)
- **FRN identity.** Records, WAL frames and content docs carry the NTFS
  sequence. The name index is v10, WAL is v2 (v1 still replays), and content is
  v13. A known sequence that differs from an event means another occupant.
  Delete, rename-old, data and info events are ignored, and create or
  rename-new replaces the old occupant and its subtree. Sequence 0 means
  unknown and keeps the old behavior. (`docs/WORK_QUEUE.md`)
- **Posting-level fold.** A fold re-extracts only changed or added docs. It
  carries the unchanged doc table, paths, filter rows, aliases and gram
  postings. A shard falls back to a full re-extract past 25% changed docs or 32
  posting folds, which restores exact term document frequency and pruning.
  (`docs/WORK_QUEUE.md`)
- **Save peak.** A shard save now removes each section file as it copies the
  bytes to the output. The measured peak fell from 1.07x to 0.60x extracted
  bytes on the synthetic save fixture, and the output bytes did not change.
  (`docs/CONTENT_SEARCH.md`, `docs/WORK_QUEUE.md`)
- **Cursor and overlays.** The v2 cursor resumes from the last emitted record
  ID and carries name, sidecar and delta identity. It pages through a live name
  overlay and content delta. Incomplete overlay-merged pages keep rank order
  and symbol promotion as a rank prefix. When the identity changes, the cursor
  restarts with `cursor_restarted`. (`docs/WORK_QUEUE.md`)
- **Explicit verify budget.** A short capped page now refills from the verify
  frontier when hidden overlay rows cap the window pass. This applies to
  single-volume queries. (`docs/WORK_QUEUE.md`)

## Compatibility and upgrade

- Existing v1.11.0 name indexes load. A v9 index loads without the sequence
  section, and each record's sequence reads as 0 (unknown). New indexes write
  v10. Content sidecars are v13; older sidecars fail the version gate and
  rebuild on first run. (`docs/WORK_QUEUE.md`, `docs/CONTENT_SEARCH.md`)
- Content search is on by default. The service builds the content index in the
  background on first start, and rebuilds it after a format change.
  (`docs/CONTENT_SEARCH.md`)
- New config keys are the `[content]` keys `mode`, `roots`, `exclude`, `exts`,
  `git`, `system_excludes` and `budget_bytes`; the per-volume
  `[content."X:"]` overrides; and the `plugin.toml` entries.
  (`docs/CONFIG.md`, `docs/FEATURES.md`)
- New CLI commands are `seekfs settings get [--json]` and `seekfs settings set
  --json <file>`, plus `seekfs plugin list|add|remove|enable|disable|config|
  doctor|reload|path`. Content search adds `--lines`, `--budget`, `--cursor`
  and `--content-verify-budget`.
  (`docs/SETTINGS_API.md`, `docs/FEATURES.md`, `docs/CONTENT_SEARCH.md`)
- Replace the executables and restart the service. The Windows binaries are
  unsigned. The release includes the CLI, the desktop apps, documentation, and
  a SHA-256 checksum for the zip archive.

## Known limits

- The Windows binaries are unsigned. Windows shows the usual warnings.
  (`docs/RELEASE.md`)
- Content matches are verified by reading files on disk. A very broad regex
  page can be marked incomplete when the default disk read budget runs out.
  Raise `--content-verify-budget` to allow more reads.
  (`docs/CONTENT_SEARCH.md`)
- The explicit-budget refill for overlay-capped pages applies to one volume.
  Multi-volume refill is not implemented yet. (`docs/WORK_QUEUE.md`)
- Fuzzy-suggestion ordering is approximate between full folds. A posting fold
  restores exact term document frequency and pruning, and a full re-extract
  occurs past 25% changed docs or 32 posting folds. (`docs/WORK_QUEUE.md`)
