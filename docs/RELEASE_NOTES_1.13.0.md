# seekfs v1.13.0

This release makes the desktop app show indexing progress and keep status
fresh. It cuts search and resize latency, reduces name-index build memory, and
adds content-index speed control, priority ordering and early results.

## Desktop app

- **Search latency.** The app sends the first search after a short burst tick,
  sends only the latest query while a request is in flight, and drops stale
  responses. On the 20,000-file fixture, action to rows fell from about 175 ms
  to 16.0 ms p50. The three harness queries took 12.9, 19.6 and 16.0 ms.
- **Resize latency.** The presenter keeps one bucketed headless window and
  reuses one BGRA buffer. A resize no longer recreates the GPU context or swaps
  pixels twice per frame. Frame p50 is 9.9 ms and p95 is 13.2 ms, against
  252-271 ms before. `SetWindowPos` p50 is 12.9 ms, against 240-277 ms. One
  frame remains near 267-305 ms when the window crosses the bucket boundary
  that creates the larger GPU context; the other frames stay near 10-13 ms.
- **Indexing progress.** Settings, the sidebar, the status bar and the results
  area show the build percent and file counts. The results area says
  `Indexing X: 42%` while the first drive builds, instead of offering setup.
- **Live settings.** While Settings is open or a drive indexes, the app polls
  `settings-get` every 500 ms. A refresh keeps the user's staged, unapplied
  edits and updates the server state, progress and entries.
- **Status refresh.** A background refresher polls `info` every 1 s while the
  service is unreachable or unhealthy, every 2 s while a drive or content index
  builds, and every 10 s when idle. The status line now leaves
  "Loading indexes..." without an app restart. A volume change or a finished
  load reruns the current query.
- **First run.** A service with no volumes reports the neutral
  `No drives indexed yet` state, not an error.
- **Content status text.** One set of messages now covers the status bar,
  results notice and Settings. A build shows
  `Content index: building N% (done of total files)`. A reconcile or fold shows
  `updating changes...` or `updating file types...`. A failure shows
  `Content index failed: <reason>`. Partial results name the size budget or
  disk reserve when it applies.
- **Apply steps.** The elevated helper writes one progress line per Apply
  phase and logs each phase with elapsed milliseconds to
  `%ProgramData%\seekfs\service.log`.

## Content search

- **Fewer opens.** Name-index metadata now decides every metadata-only
  inclusion rule before a file is opened: known binary extensions, lockfiles,
  JSON at or above 100 KiB, minified or bundle names, and container formats
  that a rich extractor claims when the file is over the raw cap. On the
  Chromium walk index this avoided 41,123 of 509,271 opens (8.1%). On the
  synthetic mixed corpus it avoided 41.4%.
- **Priority order.** Extraction runs by scope tier first: Git worktrees,
  configured roots, known folders (Documents, Desktop and Downloads), then the
  volume root. Within a tier, the newest mtime runs first. The final sidecar
  stays in FRN order, so published bytes do not change.
- **Early results.** After 2,048 documents or 8 MiB of extracted text, the
  build attaches that prefix as a partial base with `state=indexing`,
  `partial=true` and `build_done`/`build_total`. The final publish replaces it
  atomically. On the `blink/renderer/core` fixture, time to a queryable content
  base fell from 12.98 s to 2.46 s; build wall time stayed at 12.98 s.
- **Indexing speed.** Settings > Content search has a speed selector with
  `Balanced` (default), `Fast` and `Gentle`. Balanced uses up to 4 workers with
  a Low IO-priority hint while the user is active, the full cap when idle, and
  2 workers on battery. Fast always uses the full cap. Gentle uses one worker,
  the previous behavior. An in-flight query drops any mode to 1 worker for that
  query. Health and `loaded` report `build_workers`.
- **Resident small-file reads.** The experimental `mft_resident` option reads
  files of at most 1 KiB from the resident unnamed stream of the MFT record,
  without opening the file. It is off by default. Compressed, encrypted, sparse
  or reparse files, attribute-list or extension records, and records whose
  sequence or size does not match the name index fall back to a normal open.
  Data clusters are never read. Health and `loaded` report `resident_hits`,
  `resident_fallbacks` and per-reason fallbacks.
- **Memory.** Selection metadata keeps the mtime and size needed for priority
  and pre-open rules: 149.1 to 170.6 bytes per admitted file. The selected-doc
  entry fell from 96 to 64 bytes. Net selection and selected metadata is about
  106 MB after versus 112 MB before on the Chromium corpus.

## Name index and service

- **Lower build peak.** The MFT accumulator uses a record-number-indexed slice
  and one name byte arena instead of `map[uint64]mftEntry`. Accumulation peak
  fell from 303.9 to 120.7 bytes per record. The whole-build peak fell from
  646.7 to 447.7 bytes per record, 31% lower.
- **Name dedup.** The arena interns equal names at insert time. The token
  count, index file size, name blob, token table, `LOWR`, `PNGR` and `PNGC`
  sections match the legacy path byte for byte.
- **Path rank.** The path-rank stage stores component-id chains in one arena
  and joins lowercase paths only in comparator scratch. The whole-build peak
  fell from 415.0 to 373.1 bytes per record, 10% lower. Path rank is no longer
  the peak stage.
- **Memory release.** A settings-driven build releases the built `Index` as
  soon as the build returns and calls `debug.FreeOSMemory`. Retained heap after
  build and GC is 1.3 MB, equal to the baseline. The service logs
  `index-usn memory volume=... heap_inuse_mb=... released_mb=...`.

## UI automation

Test builds now include a UI automation harness. It has scenarios for startup,
search, resize, slow drag, settings, status and content search. The content
scenario builds a known-answer corpus, compares UI results with service
attribution, checks freshness, and writes JSON reports and screenshots. The
harness is inactive without the `uiautomation` build tag and its environment
variable.

## Compatibility and upgrade

- Name indexes keep version 10, and v9 indexes still load. Content sidecars
  keep version 13. This release does not change either format, so existing
  indexes load without a rebuild.
- Content builds now default to Balanced instead of the old single-worker
  behavior. Set `build_speed = "gentle"` in `[content]`, or choose Gentle in
  Settings, to keep the old behavior.
- New configuration keys are `build_speed` (also
  `SEEKFS_CONTENT_BUILD_SPEED`) and `mft_resident` (also
  `SEEKFS_CONTENT_MFT_RESIDENT`). `settings-get` and `settings-set` carry
  `build_speed`.
- Replace `seekfs.exe`, `seekfs-native.exe` and `seekfs-ui.exe` from the same
  zip, and restart the service. The Windows binaries are unsigned.
- The release includes the CLI, the desktop apps, documentation, and a SHA-256
  checksum for the zip archive.
