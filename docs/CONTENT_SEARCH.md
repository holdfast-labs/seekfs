# Content Search — Design, Status, and Handoff

Branch: `content-search`. Status: P0–P5 complete and reviewed. P4's two planner
surfaces (`sort:relevance` and snippets) are implemented and tested; P5's final
polish (review minors, per-query budgets, entry-free counting, journal-reset
invalidation, freshness measurement, docs) landed. PF-3 (WP1d/WP1e) landed: the
service now builds/rebuilds and persists its own content index in the
background, so flag-on is content-ready without a manual CLI step and a journal
reset self-heals instead of staying `stale` (see §6e). P4 document-extraction
quality remains deferred (see §7). Content search is off by default; with
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
| `content_build.go` | Offline builders + options. `buildContentIndexFromDir` (walk, `origin=walk`, path-hash keys), `buildContentIndexForIndex` (USN, `origin=usn`, record-FRN keys), `assembleContentIndex` (streams postings through the external builder), `contentNormalizeText`, `contentPathKey`, `contentTermsOf`, `contentGramsOf`, path table codec. |
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
- **One normalization.** `contentNormalizeText` = lowercase + lossy repair; every
  builder and the delta extractor use it, and `ContentHash = sha256Of(normalized)`
  so base↔delta change detection compares like with like.
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
  it snaps to rune boundaries so a multibyte rune is never split. The mapping
  path `contentLossyFixups` (`contentSnippetWindowSource`) can render original
  case from a case-preserving source, but no current build supplies one, so
  snippets fall back to the normalized text (see §7). An inexact mapping also
  falls back rather than fabricating. The offline `content`
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

## 7. Carried debt / known gaps

- **Snippet case is not preserved on any current path.** The `.gsx` text store
  is lowercased (`contentNormalizeText`), and the offline reader's "source" is
  that same normalized text, so neither the service nor the offline reader has a
  case-carrying source to map back to and both fall back to the normalized text.
  `contentSnippetWindowSource` + `contentLossyFixups` are in place for a
  case-preserving source section, but a case-carrying source section must be
  built before any snippet can show original case.
- The service reads the whole `.gsx` into heap (`contentLoadFile`); mmap-ing it
  is deferred to the ARCHITECTURE_REVIEW R1/R5 engine work.
- `contentResolvePath` has no end-to-end test (needs a compact index with a
  parent chain); only its `contentLookupFRNColumn` fallback is tested.
- Runtime-added volumes (`replaceLoadedVolumeLocked`) get no content attach;
  they are rebound only if previously attached.
- No per-volume drain cancellation (loops end at `s.stop`).
- PDF extraction is a best-effort spike (no xref/object streams, Flate only,
  simple fonts only); quality is a P4 decision.
- Content query tests attach with `setReady` directly; an end-to-end test of
  `attachContentForVolume` (origin guard + persisted FRN resolver) is still
  missing.
- The posting decode in `contentCandidates` is not streamed, so a very common
  gram is fully decoded before the candidate cap; bounded per query but large.
- The remote response sanitizer omits `Content`, so remote callers of a partial
  content query see only `Complete=false`, not the degraded-volume detail.
- A biased content search (`RootBias`/`CWDIBias`) disables the incremental
  early-stop and scans up to the match budget before the limit is applied.

### Explicitly deferred (P5)

Each item below is intentionally out of scope for P5; the rationale is one line.

- **PDF extraction quality** (xref/object streams, CID/Type0 fonts): the
  extractor is a best-effort spike; a real PDF engine is its own project.
- **Posting-decode streaming in `contentCandidates`**: bounded per query today;
  streaming only matters for pathologically common grams, which the budget
  already caps.
- **Case-preserving snippet source**: needs a new `.gsx` source section and a
  build change; without it every snippet is correctly lowercased, never wrong.
- **mmap of `.gsx`**: the whole file is read into heap; mmap belongs with the
  ARCHITECTURE_REVIEW R1/R5 engine work.
- **Runtime-added-volume attach**: `replaceLoadedVolumeLocked` attaches content
  only for previously attached volumes; wiring attach-on-add is a broader
  service change.
- **`contentResolvePath` end-to-end test**: needs a compact index with a real
  parent chain; only its `contentLookupFRNColumn` fallback is unit-tested today.
- **Journal-reset content rebuild scheduling**: DONE in PF-3 (§6e) — a reset
  marks the index `stale` and the rebuild chain schedules and re-attaches a
  service-owned content build, so content self-heals.
- **Ranking the capped positive-content candidate set**: the cap is applied in
  FRN-hash order, so an incomplete positive search returns an arbitrary subset
  (flagged incomplete); ranking the capped set is future work.
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
