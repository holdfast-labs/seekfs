# Content Search — Design, Status, and Handoff

Branch: `content-search`. Status: P0–P5 complete and reviewed, and the PF-1–PF-7
parity packages landed. P4's two planner surfaces (`sort:relevance` and snippets)
are implemented and tested; P5's final polish (review minors, per-query budgets,
entry-free counting, journal-reset invalidation, freshness measurement, docs)
landed. PF-3 (WP1d/WP1e) landed the service-owned background build/rebuild, so
flag-on is content-ready without a manual CLI step and a journal reset self-heals
instead of staying `stale` (see §6e); PF-4 (WP4/WP6) landed legacy single-byte
encoding decode (instead of repair to `U+FFFD`), an explicit encoding override,
and the configurable per-file size cap (see §6f); PF-5c (WP10/M7) landed the
`.gsx` size cap and the bounded fold (see §6g); PF-6a/PF-7b landed the
case-preserving text store and the filename-only fallback for a content-unusable
volume. PB8 landed bounded content-candidate materialization, the rank-ordered
bounded scan fallback, and the biased/overlay early-stop fix (see §6h). The only
parity work not taken is the optional PB8 items 4–7 (global-lane integration for
compound content + selective-filename queries, §6h), plus the documented §7
residuals. `.gsx` is format **v4**. P4 document-extraction quality remains
deferred (see §7). Content search is off by default; with
`SEEKFS_CONTENT_SEARCH=1`, `content:` queries work through the service once the
service-owned build has attached an FRN-keyed `.gsx`.

This document is the committed handoff for an agent continuing the work. The
fuller working plan lives at `docs/CONTENT_SEARCH_PLAN.md`, which is
gitignored on purpose ("planning docs stay local"); this file is the durable,
committed summary.

## 1. What this is

An optional, default-off full-text content search subsystem for seekfs, which
otherwise indexes only file names/paths. It is an integrated native
implementation (not the Microsoft `tgrep` binary), reusing seekfs's own
trigram/posting/mmap machinery and its USN change stream for freshness.

Two user problem sets: **A** text/code content (developers), **B** user
documents (PDF/DOCX/XLSX/MD/email). The offline path (Set A for text/code)
works today; documents have extractors but are not wired into service queries
yet.

## 2. Enable and use

Everything is gated behind the environment variable:

```powershell
$env:SEEKFS_CONTENT_SEARCH = "1"
```

Offline build + query:

```powershell
# walk/path-keyed index (for a directory)
.\seekfs.exe content-index -root F:\some\dir -out F:\some\dir\.seekfs-content.gsx [-under F:\some\dir\src] [-ext .go,.md]
.\seekfs.exe content -db F:\some\dir\.seekfs-content.gsx -n 20 needle
.\seekfs.exe content -db <gsx> --json NEEDLE

# USN/FRN-keyed index (for a service volume's .gsi) — required for the service
.\seekfs.exe content-index -db C:\ProgramData\seekfs\indexes\seekfs_c.gsi
# writes C:\ProgramData\seekfs\indexes\seekfs_c.gsx with origin=2 (FRN-keyed)
```

With the flag unset, `content`/`content-index` exit 1 with "content search is
disabled"; the rest of seekfs is unchanged.

### 2b. Content matching semantics

A `content:` leaf is a **case-insensitive literal substring** match by default:
the query text is folded with Unicode `strings.ToLower` and tested for a byte
substring anywhere in the document's stored text (`bytes.Contains`). There is
**no tokenization, word-boundary check, or stemming**, so `content:foo` matches
a document containing `foobar` or `xfoox` but does **not** match one containing
only `fo` or `oo`. `content:"foo bar"` is a phrase: the space is part of the
literal substring. `case:true` selects case-sensitive matching (the raw,
case-preserving stored text is compared byte-for-byte with no fold). For
precision the index cannot express literally — word boundaries, alternation,
anchors — use the regex form `content:/pattern/`, which is compiled with `(?i)`
unless `case:true` is set. The fold is the same on both sides because the
builder folds documents and the query folds the leaf with the one shared
`contentFoldText`.

Service health: when the flag is on but no valid `.gsx` is attached, the service
reports `content.state = "unavailable"` — never `"ready"` with an unusable
index. Content queries are served once a volume is usable; when some (not all)
eligible volumes are unusable the response is marked degraded/incomplete, and
when none is usable the query errors rather than returning empty.

## 3. Architecture map

| File | Role |
| --- | --- |
| `content_encoding.go` | tgrep-ported text decoding: BOM sniff (UTF-8/16LE/16BE), lossy repair with maximal-subsequence semantics, and `contentLossyFixups` mapping repaired offsets back to source bytes. |
| `content_index.go` | `.gsx` container: magic/version, `Origin`, section table, 64-byte `contentDoc` records, encode/decode, atomic save, FRN bisection. |
| `content_postings.go` | Shared CXTR/CXGR block codec: `key -> []contentDocFreq{docID,tf}`, delta-varint docID + uvarint tf, 1024-doc blocks, dictionary + block meta, bounded lookup. |
| `content_external_build.go` | Bounded external spill/merge builder (`contentExternalSectionBuilder`) producing bytes identical to the in-memory encoder. |
| `content_extract.go` | `contentExtractor` interface, registry, `contentExtractorForPath` (extension → format sniffers → text fallback), binary sniff, size caps. |
| `content_extract_text.go` / `_ooxml.go` / `_pdf.go` | Extractors: text/code; OOXML/ODF/EPUB; best-effort PDF (spike quality). |
| `content_open_windows.go` / `_other.go` | `contentOpenNoRecall`: opens with `FILE_FLAG_OPEN_NO_RECALL` so cloud placeholders fail instead of hydrating. |
| `content_build.go` | Offline builders + options. `buildContentIndexFromDir` (walk, `origin=walk`, path-hash keys), `buildContentIndexForIndex` (USN, `origin=usn`, record-FRN keys), `assembleContentIndex` (streams postings through the external builder), `contentRepairText` (case-preserving stored text), `contentFoldText` (case-folded index view), `contentPathKey`, `contentTermsOf`, `contentGramsOf`, path table codec. |
| `content_read.go` | `contentReader`: decode sections, trigram-intersection candidates, substring verification, `contentHit{Path,DocID,Offset}`. |
| `content_resolver.go` | `contentResolver`: merges FRN-sorted docs against the base FRN column → `docID -> recordID` (`contentNoRecordID` = dropped). |
| `content_query.go` | `contentLeaf`, `contentSearchEnabled`, `contentUnavailableError`, `parseContentLeaf`, `queryHasPositiveContentLeaf`, deterministic `contentAssignLeafIDs`. |
| `content_tokenize.go` | Quote- and regex-span-aware query tokenizer (`content:/a b/` survives whitespace and `|`). |
| `content_service.go` | `contentVolumeState` (idx/reader/resolver/delta/state/health), `contentDelta`, `contentCoordinator` (dirty/promote/drain/invalidate), `contentExtractDeltaDoc`, `processQueue`, `contentEligibleForExtraction`, `contentHealth`, `sha256Of`. |
| `content_service_wiring.go` | Service glue: `contentIndexPathForDB`, `contentBaseFRNColumns`, `attachContentForVolume`, `contentDrainLoop`, `contentResolvePath`, `rebindContentAfterBaseSwap`, `contentLookupFRNColumn`. |
| `content_cli.go` | `cmdContentIndex`, `cmdContent`. |
| `content_spike_test.go` | Env-gated measurement/oracle harness (`SEEKFS_CONTENT_SPIKE_DIR`). |
| `cli.go`, `postings.go`, `query_parse.go`, `service_protocol.go`, `service_replay.go`, `service_runtime.go`, `service_server.go`, `types.go` | Small integration edits (command cases, `parsedQuery.Content`, `serviceResponse.Content`, coordinator hook, attach loop, resolver rebind). |

## 4. Invariants (do not break these)

- **Origin guard.** `.gsx` carries an `Origin`. `contentOriginWalk` = doc key is
  the FNV-1a hash of the relative path; `contentOriginUSN` = doc key is the NTFS
  FRN. `attachContentForVolume` attaches only `contentOriginUSN`; anything else
  stays `unavailable`. A walk-keyed index next to a `.gsi` would otherwise
  report `ready` while mapping nothing.
- **One doc per FRN, `DocID == index`** in the FRN-sorted doc table;
  `contentIndexDecode` enforces it. Cross-file content dedup is deferred.
- **One repair, one fold.** `contentRepairText` = lossy repair preserving case
  (the stored text and `ContentHash = sha256Of(repaired)`, so base↔delta change
  detection compares like with like); `contentFoldText` = `strings.ToLower`, the
  case-folded view the term/gram index and case-insensitive matching use. The
  prefilter is folded, verification is case-aware (`case:true` → byte-exact).
- **Lock order.** `contentResolvePath` takes `indexMu.RLock()` then `vol.mu`
  (matching the persist/rebuild swap). The drain passes a **private** path cache;
  it must never touch `vol.pathCache` (search trims that under `searchMu`).
- **Flag off is zero-impact.** `contentCoord` is nil, `observeChanges` is never
  called, the replay `applied` slice is not allocated, health is nil.
- **Unavailable, not empty.** A missing/invalid content index causes queries to
  be refused, never silently reported as "no matches".
- **Never hydrate cloud files.** Eligibility rejects directories and
  offline/recall/reparse files; the read uses `OPEN_NO_RECALL`.

## 5. Testing

```powershell
go build -o seekfs.exe ./cmd/seekfs
go vet ./...
go test ./... -count=1                      # ~2 min
go test ./cmd/seekfs -run 'TestContent' -v  # content subsystem
# optional measurement (real corpus):
$env:SEEKFS_CONTENT_SPIKE_DIR = "F:\git\seekfs"; go test ./cmd/seekfs -run TestContentSpike -v
```

The offline path has oracle parity vs brute-force file reads. `-race` needs a C
toolchain (not available in this environment). Current measurement on the seekfs
source tree: ~3.2 MB encoded `.gsx` for ~5 MB raw, peak heap ~34 MB.

## 6. P3 — ACCEPTED (planner integration)

`content:` now works through the service. What landed (see the git history for
exact files):

- **Parser** parses `content:` term/phrase/regex leaves and the disabled-flag
  error is preserved; `sort:relevance` is accepted (stored, not yet ranked).
- **Inline verification** (`entryMatchesWithContent`, new `content_verify.go`)
  mirrors `entryMatches` and enforces content at every recursion level: all
  positive top-level leaves, OR groups satisfied by the *same* alternative,
  NOT groups dropped on a full content-aware match. It runs BEFORE an entry is
  appended or counted, so the user limit counts only content matches.
- **Candidates** (`contentCandidates`) come from term/trigram postings plus the
  delta; a mixed-OR / regex / negated query falls back to an ordered scan.
- **Boundedness:** `contentCandidateBudget` / `contentScanVisitBudget` (4M
  default) and a 4096-entry path memo. An incomplete scan is never silent —
  search marks the trace/health incomplete (`Complete=false`, `degraded`), and
  count returns `errContentIncomplete`.
- **Multi-volume** content search + count run per-volume and merge; a volume
  with no usable index is skipped and named in the degraded set; only an
  all-unusable query errors.
- **Safety:** every fast/global/fuzzy/count lane declines content queries;
  non-content behavior is unchanged with the flag off.

Review history: four rounds; blockers found and fixed were (1) the limit being
applied before content verification, (2) multi-volume decline, (3) mixed-OR
dropped matches, (4) unbounded candidate/Entry materialization, (5) unbounded
fallback path memo.

## 6b. P4 — remaining work

1. **Snippets.** LANDED (see §6c): a bounded local-only window around the first
   matching term/phrase leaf. A `Matches` span list is still open.
2. **`sort:relevance`.** LANDED (see §6c): bounded post-verify content rank.
   BM25 stays gated on the engine review.
3. **Document-extraction quality:** PDF xref/object-streams and font coverage
   (the current extractor is a spike); decide on legacy OLE. DEFERRED — see §7.
4. **Scale follow-ups from review:** stream/cap the posting decode in
   `contentCandidates` before the budget (a common-gram list can be large) —
   DEFERRED, see §7; per-query/options scan budgets instead of mutable package
   vars — DONE in P5 (§6d); rank the capped positive-content candidate set (it
   currently caps in FRN-hash order) — open; entry-free counting so count does
   not materialize `[]Entry` — DONE in P5 (§6d).
5. **Freshness:** measure the p95 ≤ 5 s USN→result bound — DONE in P5 (§6d).
6. **Tests:** multi-leaf AND (`content:a content:b`) and content nested inside
   OR/NOT.

## 6c. P4 surfaces — implemented (reviewed)

- **`sort:relevance`.** Only meaningful for content queries; ranked post-verify
  over the bounded result set by `contentRelevanceOf` / `sortContentEntriesByRelevance`
  (`planner_rank.go`), called from `searchContentServiceVolumes`. Rule (minimal,
  deterministic): score = number of matched positive content leaves under the
  SAME joint-OR rule as verification (top-level AND leaves plus the leaves of the
  alternative that actually satisfied each OR group; a failing alternative never
  inflates the score; NOT leaves excluded); higher score first, then earlier
  earliest first-match offset among term/phrase leaves, then the existing
  name/path order. No BM25 or corpus statistics. The rank is not an index-wide
  per-record array, so `rankForQuery` deliberately returns nil for a content
  query with `sort:relevance` (a record scan would be unbounded); the content
  path applies the bounded sort instead. A non-content query with
  `sort:relevance` keeps the default order, like the other sort columns.
  Ranking must not be a top-of-page illusion: before scoring, each volume is
  fetched with an enlarged window `min(candidateBudget, max(limit*100, 4096))`
  (overflow-safe), ranked, then trimmed to the user limit. A per-volume
  match count that reaches the window means more matches may exist and is
  surfaced as incomplete/degraded, never a silent truncation.
- **Snippets.** `Entry.Snippet` (and `jsonResult.Snippet`) is filled by
  `attachContentSnippets` after the result set is bounded by the limit, so the
  work is O(results). It is a ~200-rune window around the first matching
  term/phrase content leaf, with `...` on a truncated side; regex-only matches
  yield no snippet. The window decodes only the bytes around the match
  (`contentSnippetWindow`), so per-result work is O(window), not O(docLen), and
  it snaps to rune boundaries so a multibyte rune is never split. The window is
  rendered from the case-preserving stored text (PF-6a), so it shows original
  case even though the match is found by folding the haystack; the folded match
  offset is mapped back to raw bytes by rune index. The offline `content`
  command keeps `results` as an array of path strings and adds a parallel
  `snippets` array in `--json`; plain stdout stays path-only unless `--snippet`
  is passed.
- **Authorization.** Snippets are local-only. The remote projection
  (`remoteResponseFromService` → `remoteResultRow`) copies only
  path/size/modified/is-dir, and `resp.Results` is path-only, so neither the
  matched text nor any other `Entry` text field reaches a remote caller. No new
  capability is granted.

## 6d. P5 — final polish (done)

- **Reviewer minors.** `contentSnippetWindow` no longer panics at
  `off == len(text)` and its rune-snap loop is bounds-safe
  (`TestContentSnippetWindowAtEndOffset`). `contentLeafFirstOffset` now takes
  the matcher's hoisted lowercased needle instead of re-lowercasing per call.
  Tests cover the relevance window-full incomplete path
  (`TestContentServiceRelevanceWindowFullMarksIncomplete`) and the offline JSON
  `results`-paths-only + `snippets` keys (`TestContentCLIJSONKeepsResultsAndSnippetsKeys`).
- **Per-query budgets.** `queryOptions.ContentCandidateBudget` /
  `ContentScanVisitBudget` (copied into `parsedQuery`, `contentDefaultCandidateBudget`
  / `contentDefaultScanVisitBudget` = 4,000,000 defaults) replace the mutable
  package vars; tests set the option, not a global. No production behavior
  change (the service pipe does not carry the fields, so they default).
- **Entry-free counting.** `countContentVolume` tallies content matches through
  `queryOptions.contentCount` and the compact scan counts in place, skipping
  path reconstruction unless the query reads `Entry.Path`; the overlay delta is
  counted by the same predicate. `count == len(search)` is unchanged.
- **Journal-reset invalidation.** `replaceServiceVolumeContents` (the same call
  site as `rebindContentAfterBaseSwap`) now calls
  `invalidateContentAfterBaseReset` when the base journal id actually changed,
  dropping the FRN-keyed content index and pending delta so it is never served
  stale; a same-journal swap still only rebinds the resolver
  (`TestContentInvalidatedOnJournalReset`).
- **Freshness measurement.** `TestContentFreshnessUSNWriteCloseLatency` drives
  the write-close → drain → query path, asserts the changed content becomes
  findable, and reports the per-round latency (p95, max) without asserting a
  wall-clock bound. On this machine it reports single-digit milliseconds for the
  in-process drain path, well inside the 5 s target; the production cadence adds
  only the ≤2 s quiet-window drain tick.

## 6e. PF-3 — service-owned background build (WP1d/WP1e, done)

The service no longer only loads a hand-built sidecar:

- **Trigger/state machine.** `ensureContentBuild` (`content_service_build.go`)
  first attaches an existing valid `.gsx`; if none is usable and the volume is a
  ready USN volume with a live watermark, it schedules exactly one background
  build (`contentBuildBusy` CAS). It is hooked at startup
  (`loadConfiguredIndexes`), after a filename rebuild/journal reset
  (`rebuildVolumeInPlace`, so `replayVolumeLoop` and `staleRecoveryLoop` both
  self-heal), and on runtime `index-usn`/`replaceLoadedVolume`. The volume
  enters a real `indexing` state (`contentStateIndexing`, previously never
  assigned) with `build_done`/`build_total` progress; `markDrained` no longer
  clobbers `indexing` or `stale`. A failed/partial/canceled build leaves the
  volume `degraded`/`unavailable`/`indexing`, never `ready` with an unusable
  index.
- **Snapshot-and-release locking (chunked).** The build copies
  FRN/path/size/modtime metadata in `contentBuildSnapshotBatch`-record windows,
  releasing and reacquiring `s.indexMu.RLock` between batches (with a
  `contentBuildPathCacheMax`-bounded path memo) so a persist/rebuild's
  `indexMu.Lock` proceeds between batches instead of waiting for an O(records)
  pass. Each batch is read only after re-checking the index pointer and the
  base generation/journal under the read lock, so it never reads a torn or freed
  mmap; a mid-pass change aborts and reschedules. Extraction opens no file under
  `indexMu` or `vol.mu` and never touches the mmap. The copy is stamped with
  `baseCheckpoint` (a `serviceVolumeIndex` field), so a base with an unfolded
  overlay cannot overclaim; restart catch-up replays the overlay changes
  afterward. `contentBuildCurrent` aborts the build if `replayGen` (atomic, per
  file) or the journal id (per batch and before publish) changed.
- **Streaming assembly (bounded memory).** Extracted docs stream one at a time
  into `assembleContentIndexStream`; the build never accumulates a
  `[]contentBuildDoc` of the raw corpus. Each doc's raw text is normalized into
  the text store and tokenized into the bounded external posting builders as it
  arrives, then released. Peak transient heap is one document's raw text plus
  the two `contentBuildArenaBytes` posting arenas, the text store being
  assembled, and O(docs) metadata; the offline `content-index` path still builds
  identical `.gsx` bytes.
- **Retry budget.** Consecutive generation-change reschedules are capped at
  `contentBuildMaxAttempts` with `contentBuildRetryBackoff` pacing; hitting the
  cap leaves the volume `degraded` with a `BuildError` instead of spinning. An
  external trigger (`ensureContentBuild`) opens a fresh budget.
- **Indexing is unusable.** `usableForQuery` refuses an `indexing` volume even
  with a populated retained delta, so a content query cannot serve delta-only
  partial results with `health.Incomplete=false` while the fresh base builds.
- **IO governor.** Serial extraction paused every `contentBuildBatchSize` for
  `contentBuildBatchPause`, cancelable through `s.stop` and the build
  generation.
- **Default scope (M5).** `defaultServiceContentBuildOptions` sets an extension
  allowlist: the curated `contentDefaultTextExtensions` text/code/config set
  unioned with every registered extractor's `Extensions()`. The offline CLI
  still indexes any extractable file by default; the service does not.
- **Crash-safety.** The built index is written with `contentSaveFile`
  (temp + fsync + rename) and the v2 header carries `JournalID` +
  `CheckpointUSN`, so a crash leaves either the old or a complete new sidecar,
  never a half-written one that attaches.

Tests: `content_service_build_test.go` covers build-and-attach with observable
indexing/progress, the default allowlist, journal-reset self-heal, re-attach
after a not-ready volume becomes ready, lock release across extraction,
mid-build journal-change abort, and shutdown cancellation. PB2 (delta folding
into the persisted base) stays in PF-5/§7.

## 6f. PF-4 — encoding breadth + size policy (WP4/WP6, done)

Both changes alter normalized text, so the `.gsx` format version is now **3**; a
v2 sidecar fails decode (magic+version gate) and is never attached, and the
PF-3 path rebuilds it.

- **Encoding (PB6).** A BOM (UTF-8/16LE/16BE) and valid UTF-8 are decoded
  exactly as before. The extractor passes its real truncation signal (a
  plain-text file whose size exceeds the raw cap) into the decoder, and only
  then -- and only when the buffer is otherwise a UTF-8 prefix whose sole
  invalid rune is a multibyte tail cut mid-rune -- is it decoded as UTF-8 with
  the incomplete tail repaired to `U+FFFD`, never treated as legacy. The signal
  is the actual `size > maxRaw` cut, not a guess from the byte shape, so a
  genuine Latin-1/CP1252 file ending in `caf\xe9` (no cut) is decoded, not
  repaired. Any other invalid buffer is decoded **hybridly**: every valid UTF-8
  span is kept byte-exact and only the invalid spans are decoded as a legacy
  single-byte encoding. That keeps a mostly-UTF-8 file with one stray invalid
  byte searchable (its `café` stays `café`) while still recovering the stray
  span, instead of re-decoding the whole file as CP1252 and mojibaking it. One
  consequence: an over-cap UTF-8 file that also contains an earlier stray byte
  lacks the tail-only shape, so it takes the hybrid path and its cut tail is
  legacy-decoded rather than repaired; index and search still agree because the
  stored text is produced under this same auto policy. Each invalid span
  decodes as Windows-1252, except a span whose CP1252
  decode yields `U+FFFD` -- one of CP1252's five undefined bytes
  (0x81/0x8D/0x8F/0x90/0x9D), which Go's `charmap` maps to `U+FFFD` -- which is
  decoded as Latin-1 (ISO-8859-1) instead, so no source byte is dropped.
  `-encoding <WHATWG label>` on offline `content-index` and
  `SEEKFS_CONTENT_ENCODING` for the service build + USN delta override it via the
  existing `parseContentEncoding` (a BOM still wins; `none` keeps raw bytes).
  The NUL-based binary sniff is unchanged, so binary files are still rejected.
- **Size policy (PB7).** Caps are configurable (`-max-raw`, `-max-text`;
  `contentBuildOptions.MaxRaw/MaxText`), defaulting to the historical 32 MiB /
  16 MiB with hard ceilings (512 MiB / 256 MiB) that clamp any larger request. A
  plain-text file over the raw cap is indexed as a bounded prefix (memory bounded
  by the cap) and marked `Truncated`; a container (zip/OOXML, PDF) that cannot be
  prefixed is `Skipped` with a reason. Neither is silent: the build tallies them
  into a new CXPL policy section in the `.gsx`, and content health surfaces
  `max_raw`, `max_text`, `skipped`, `truncated`. The service build and USN delta
  always use the 32 MiB / 16 MiB defaults; `-max-raw`/`-max-text` are
  offline-CLI flags (the service caps are not env-exposed yet).

## 6g. PF-5c — `.gsx` size cap + fold over-cap hardening (WP10/M7, done)

The sidecar is capped at `contentGSXMaxBytes` (4 GiB default; a package var so
tests can lower it). `contentSaveFile` refuses an encoded index over the cap
with `contentGSXSizeError` and writes nothing, so an over-cap build or fold is
never a truncated sidecar: a build's volume goes degraded, and a fold keeps the
previous base and the delta (content stays correct and usable) with the volume
health degraded. Health surfaces `sidecar_bytes` and `sidecar_cap` alongside the
size-cap `build_error`, so the condition is visible in `loaded --json`. A
base-indexed file deleted without a prior delta entry gets an in-memory tombstone
(M7/WP10), so the next fold evicts it instead of re-encoding it forever;
`foldDue` counts all delta entries, so a burst of deletes still triggers the
pruning fold.

An over-cap fold retries on `contentFoldCappedBackoff` (15 min), not the normal
5 s fold retry, so the whole-corpus assembly + encode is not re-run every 2 s
drain tick. While capped the resident delta is bounded by
`contentDeltaHardMaxDocs` / `contentDeltaHardMaxBytes`: past that ceiling a
distinct new document is deferred and the volume is marked incomplete/degraded,
rather than the delta growing without bound. Existing entries are still updated,
so no acknowledged content is dropped, and a deferred change is not lost — the
persisted base checkpoint is not advanced, so it is replayed once recovery
happens.

**Recovery (P6-2, accepted limitation).** A plain over-cap fold (the delta is
over the cap but has not hit the hard bound) self-heals: `health.Incomplete` is
still clear, so the 15-minute capped retry re-encodes base+delta and publishes
as soon as the content fits again (files deleted or shrunk). Once the delta hits
the hard bound, `markDeltaCapped` sets `health.Incomplete`, which gates every
fold, so only a fresh attach (rebuild or restart) can clear it. No rebuild is
scheduled automatically: `runContentBuild` calls `markIndexing`, which drops the
still-usable base+delta, and a rebuild at the same cap fails over cap again
whenever the live corpus still exceeds `contentGSXMaxBytes` — so an automatic
attempt would take the volume from degraded-but-usable to unusable without
guaranteed benefit. The operator action is to lower the indexed content or raise
the cap, then restart; the condition and the action are surfaced in
`loaded --json` via `sidecar_bytes`/`sidecar_cap` and a `build_error` naming the
operator action.

## 6h. PB8 — bounded content candidates + residual list (done)

PB8 bounds a broad content query's cost to the result window instead of the
whole posting superset. It also carries the count-parity, remote-content-health
(item 8), and biased-early-stop (item 9) items.

- **Bounded candidate materialization.** `contentCandidatesBounded`
  (`content_verify.go`) takes an explicit cap: a non-count, non-relevance,
  overlay-clean query caps the posting intersection at the completeness window
  (`max(Limit, contentWindow)`, `search_compact.go`), so a broad term no longer
  materializes its full posting list. A cap hit means the set is "not an ordered
  superset", so `nameTermCandidates` falls through to the **rank-ordered bounded
  scan** (`boundedScanCandidates`), which walks the result order and verifies
  inline, stopping at the window. The materialization cap is deliberately
  separate from the completeness signal: only a true budget overrun or an
  evaluated window sets `ContentIncomplete`; a page/window cap is compensated by
  the scan and is not itself a completeness failure.
- **Cheaper verification.** `matchRawFirst` (`content_verify.go`) skips the fold
  when the lowercased needle already appears verbatim in the raw text (an
  already-lowercase corpus never folds), and the scan verifies path-free unless
  the query reads `Entry.Path` (`scanCandidateMatches`, gated by
  `queryNeedsEntryPath`).
- **Biased early-stop.** A biased query (`RootBias`/`CWDBias`) early-stops in
  biased order once the limit is filled (the root subtree's matches first, the
  deferred non-root matches after), except where it must keep the full set
  (`sort:relevance`, the `contentFullCandidates` differential arm). Every
  early-stop — biased and not — is guarded on `pq.hidden.empty()`: a scan page
  filled by overlay-hidden records would otherwise under-fill the result. The
  captured hidden set is threaded through `parsedQuery.hidden` so the scan and
  the verify loop drop exactly the same records (no snapshot-republish race).
  This guard is a shared-path correctness fix: it also fixes a filename
  (non-content) under-fill that exists on `main`, not a content-gated one.
- **Measured (50k-doc synthetic corpus, `BenchmarkContentVsFilename`).** Single
  content-broad 18.7 ms/818 MB → ~0.34–0.54 ms/175 KB (~15× filename-broad);
  multi 84.7 ms/331 MB → ~1.3–2.0 ms/583 KB (~9×); content-selective ~64–96 µs.
  A `contentFullCandidates` differential arm proves the bounded path
  byte-identical to the historical full-candidate logic (single/multi, every
  sort column, limits 20/5000, hidden overlays).

Residual / known limitations (all explicit deferrals):

- **Content bypasses the global planner lanes (PB8 items 4–7).** A compound
  content + selective-filename query (`content:<common> ext:.go under:<dir>`)
  does not lane-intersect: the content path runs per-volume and the cost is
  driven by the content candidates. Deferred/optional; if pursued, prototype the
  `globalRecordID` ↔ content-docID join first.
- **OR-alternative candidate union.** `contentAltCandidateDocs` still
  materializes each alternative's doc set (`budget=0`); a k-way streamed union is
  an optimization tail.
- **`.gsx` read into heap.** `contentLoadFile` reads the whole sidecar; mmap is
  deferred to the engine R1/R5 work.
- **Completeness residual.** A window-capped superset whose true matches fall in
  `[userLimit, window)` reports `incomplete` conservatively (the safe direction).
- **M2.** `contentExtractSafely` runs extraction in a goroutine and returns on
  `ectx.Done()`, so a context-ignoring extractor cannot wedge the serial drain,
  the service build, or the offline walk builder. The residual is the abandoned
  goroutine and its close-race with the caller's deferred `f.Close()`: safe for
  `*os.File` (concurrency-safe `ReadAt`/`Close`) but not for a non-close-safe
  library `ReaderAt`; P4's out-of-process path would replace both with a hard
  kill (see the `ponytail:` comment on `contentExtractSafely`).
- **M10 (fixed, P6-1).** A service that loses the `.gsx.lock` race at startup
  re-attempts acquisition on the drain cadence (`retryContentVolumeLock`), so it
  takes ownership once the CLI build releases it. The failure is logged once and
  the retry is a single non-blocking `LockFileEx`.
- **Over-cap fold (accepted limitation, P6-2).** A volume whose live content
  exceeds `contentGSXMaxBytes` is kept correct and usable (previous base +
  delta), with a 15-minute capped backoff that recovers a transient over-cap.
  Once the delta hard bound sets `health.Incomplete`, folds are gated and no
  rebuild is scheduled: `runContentBuild`'s `markIndexing` would drop the usable
  base, and a rebuild at the same cap fails again while the corpus is too large.
  The operator must lower the indexed content or raise the cap, then restart;
  `markDeltaCapped` now records that action in `build_error`.
- **Capped/aborted catch-up (verified, P6-3).** A successful self-heal rebuild
  re-attaches and re-runs catch-up to the live checkpoint, which clears
  `catchUpPending` and `health.Incomplete` (asserted in
  `TestContentCatchUpCapSelfHealsRebuild`). It stays set only when the bounded
  rebuild budget is exhausted, leaving the volume gated until a restart.
- **Per-volume drain cancellation (fixed, P6-4).** The coordinator owns a `done`
  channel; `contentDrainLoop` selects on it and on `s.stop`, and
  `stopContentDrain` retires one volume's loop without affecting shutdown.
- **Binary-sniff edges (P6-5).** BOM-less UTF-16 is now recognized by a NUL-byte
  parity heuristic (one-sided, ≥40% NULs, textual guard, minimum sample length)
  and decoded. A NUL-free binary file is still indexed as text — a general binary
  classifier is an accepted limitation, not a heuristic rabbit hole. Because the
  text extractor's `Version()` is intentionally not bumped, a BOM-less UTF-16
  file previously skipped as binary is not re-indexed by the sniff change alone:
  it becomes findable only after a rebuild or a USN change to the file.
- **`contentResolvePath` (fixed, P6-6).** Covered end-to-end by
  `TestContentResolvePathParentChain` against a compact index with a real parent
  chain, including the low-memory FRN-column fallback and an overlay rename.
- **PDF extraction** remains a best-effort spike (no xref/object streams, Flate
  only).
- **Dead code.** `contentPostingIndex.lookup`/`forEach` and
  `contentLossyFixups.toSourceOffset`/`isEmpty` are test-only (annotated).

## 7. Carried debt / known gaps

- **Snippet case is preserved; no fixup-aware path remains.** The `.gsx` text
  store preserves case (`contentRepairText`, PF-6a), and both the service
  `contentSnippet` and the offline reader render the window directly from that
  stored text, so snippets keep original case. `contentSnippetWindowSource` and
  its `contentSnippetSource`/fixup mapping were dead once raw text was rendered
  directly and have been removed; `contentLossyFixups` remains for the decoder.
- The service reads the whole `.gsx` into heap (`contentLoadFile`); mmap-ing it
  is deferred to the ARCHITECTURE_REVIEW R1/R5 engine work.
- **A file extracted while still being written.** The drain promotes a dirty file
  on a close/USN-quiet window (≤2 s tick), so a file written in place without a
  close can be indexed from a partial state. The next USN change (or the close)
  re-extracts it; there is no in-progress-write guard.
- **Intra-volume rename keeps its stored path until re-extraction.** An
  intra-volume rename preserves the FRN, so the base/delta doc keeps the old
  `Path` (the delta is not re-extracted for a rename). Content search resolves
  paths through the record index/resolver, so matching is unaffected — this is a
  stale stored-path/ordering field, not a match gap.
- PDF extraction is a best-effort spike (no xref/object streams, Flate only,
  simple fonts only); quality is a P4 decision.

### Explicitly deferred (P5)

Each item below is intentionally out of scope for P5; the rationale is one line.

- **PDF extraction quality** (xref/object streams, CID/Type0 fonts): the
  extractor is a best-effort spike; a real PDF engine is its own project.
- **Posting-decode streaming in `contentCandidates`**: DONE in M6 — the posting
  codec's `postings`/`forEach` page the 1024-doc blocks and `content_read.go`'s
  `intersectDocIDStreams` merge-intersects lazy per-trigram streams, so a broad
  gram is never fully decoded (peak memory is O(budget + #trigrams)).
- **mmap of `.gsx`**: the whole file is read into heap; mmap belongs with the
  ARCHITECTURE_REVIEW R1/R5 engine work.
- **Runtime-added-volume attach**: DONE in PF-3 (§6e) — `ensureContentBuild`
  runs after `index-usn` (`service_server.go:513`) and after
  `replaceLoadedVolume` (`service_server.go:548`), so a runtime-added or
  re-indexed volume is attached or scheduled instead of staying unavailable.
- **`attachContentForVolume` end-to-end test**: DONE — the origin guard
  (walk-keyed sidecar refused, USN-keyed attached) and the persisted FRN
  resolver are covered end-to-end in `content_service_integration_test.go`, and
  the attach + restart catch-up path in `content_restart_test.go`.
- **`contentResolvePath` end-to-end test**: DONE (P6-6) —
  `TestContentResolvePathParentChain` covers a compact index with a real parent
  chain plus the FRN-column fallback and an overlay rename.
- **Journal-reset content rebuild scheduling**: DONE in PF-3 (§6e) — a reset
  marks the index `stale` and the rebuild chain schedules and re-attaches a
  service-owned content build, so content self-heals.
- **Ranking the capped positive-content candidate set**: DONE in PB8 (§6h) — a
  capped posting superset is no longer used as the result order; the bounded
  path falls through to the rank-ordered bounded scan, which is a complete
  superset in result order.
- **Multi-leaf AND / deep OR/NOT nesting tests**: `content:a content:b` and
  content nested inside OR/NOT are covered only in combination today.
- **`count == len(search)` exception**: for `sort:relevance` whose per-volume
  window fills, search is marked incomplete while count remains exact; the two
  intentionally differ in that one case.

## 8. Review process used

Each phase was reviewed by a `reviewer` subagent and only advanced on
acceptance. Defects the reviews caught and that are now fixed: an operator-
precedence bug in the eligibility gate; observe-without-drain (unbounded map);
a delta lock-regime race; missing `OPEN_NO_RECALL`; a use-after-unmap race in
path resolution; walk-volume misattachment; a false `ready` on a missing FRN
column; the absent FRN-keyed builder; unpopulated/inconsistent `ContentHash`; a
low-memory resolution path that dropped delta docs; an unscoped whole-volume
builder; and a `-under` prefix over-match.

## 9. Non-goals (explicit)

No bundled/forked tgrep binary; no semantic/embedding layer (see
`docs/FUZZY_RANKED_SEARCH_PLAN.md`); no CID/Type0 CJK PDF text; no AST/symbol
search; no network/client-server content search; content stays opt-in and
size-capped, never whole-disk by default.

### Considered and rejected

- **LLM/BPE tokenizers (e.g. `gigatoken`, `tiktoken`, HuggingFace
  `tokenizers`, SentencePiece) as the index tokenizer.** These encode text into
  model-specific subword IDs for language-model input. Our index is lexical:
  `contentTermsOf` word runs plus `contentGramsOf` trigrams as a candidate
  prefilter, with literal-substring verification (§2b). BPE merges subwords
  against a model vocab, so `content:foo` would stop matching `foobar`, the
  index would be bound to one model's vocab, and the Rust/Python-ABI ecosystem
  does not fit the Windows-first, no-C-toolchain, single-binary build. It also
  solves a cost we do not have (PB8 showed candidate materialization, not
  tokenization, was the bottleneck). A fast tokenizer becomes relevant only if
  semantic/embedding search is ever added — a separate design (embeddings +
  vector store), and a current non-goal. If lexical indexing needs more speed,
  the levers are SIMD trigram extraction, posting compression with block-max
  skipping (the postings are already 1024-doc block-paged), and `.gsx` mmap.
