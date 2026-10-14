# Content Search — Design, Status, and Handoff

Branch: `integ/content-main`. Status: P0–P5 complete and reviewed, and the PF-1–PF-7
parity packages landed. P4's two planner surfaces (`sort:relevance` and snippets)
are implemented and tested; §6m landed the compact v7 index (blocked
compressed text, dictionary-only terms, docID-only grams); P5's final polish
(review minors, per-query budgets,
entry-free counting, journal-reset invalidation, freshness measurement, docs)
landed. PF-3 (WP1d/WP1e) landed the service-owned background build/rebuild, so
flag-on is content-ready without a manual CLI step and a journal reset self-heals
instead of staying `stale` (see §6e); PF-4 (WP4/WP6) landed legacy single-byte
encoding decode (instead of repair to `U+FFFD`), an explicit encoding override,
and the configurable per-file size cap (see §6f); PF-5c (WP10/M7) landed the
`.gsx` size cap and the bounded fold (see §6g); PF-6a/PF-7b landed the
case-preserving text store and the filename-only fallback for a content-unusable
volume. PB8 landed bounded content-candidate materialization, the rank-ordered
bounded scan fallback, and the biased/overlay early-stop fix (see §6h). WP11
landed the HTML/EML/mbox/RTF/MSG/PDF extractors (with per-parser fuzz targets),
and WP8 landed the M1 replay-bounded delta, M9 count/remote health, and the M1
delta observability. The PB8 items 4–7 (global-lane integration) has now landed:
a compound content + selective-filename lane (`cmd/seekfs/content_global_lane.go`)
drives the filename iterator and verifies content inline. It is **on by default**
(`SEEKFS_CONTENT_GLOBAL_LANE=0` is the kill switch back to the per-volume
content path) and engages single-volume compound queries whose filename side is
small (estimated drive at most max(1024, 4×limit)). It handles `under:`
(under-root narrowing on the filename side, shard-mask pruning on sharded
bases), `exists:`, overlay hidden-base filtering with the live-slot merge, and
biased order on pristine volumes — every newly in-scope shape proven by
randomized differentials against the slow path (identical sets, order, counts
and snippets). It still declines `sort:relevance` on search, content under
OR/NOT groups, case-sensitive filename parts, filename OR-unions, bare
non-path terms and other unsupported filename roots, multi-usable-volume
queries, truncated catch-up (`healthIncomplete`), a missing overlay snapshot,
and biased queries while the volume carries overlay activity (see the WP7
section of the roadmap for the precise list and rationale). The documented §7
residuals remain. `.gsx` doc tables are format **v10** (no stored text: the
index is trigram postings plus a per-file bloom filter, and every match and
snippet verifies by reading the file from disk — §6y; v9 and older sidecars
fail the version gate and rebuild) and sharded manifests are
**v5** (see §6i). P4 document-extraction
quality remains deferred (see §7). Content search is off by default; with
`[features.content] enabled = true` or `SEEKFS_CONTENT_SEARCH=1`, `content:`
queries work through the service once the
service-owned build has attached a `.gsx` — FRN-keyed for USN volumes,
stable-path-hash-keyed for walk volumes (see §6k).

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
documents (PDF/DOCX/XLSX/MD/email). Both use the shared extractors in offline and
service builds.

## 2. Enable and use

Enable the built-in plugin from the CLI:

```powershell
seekfs plugin add content
seekfs plugin doctor content --wait
```

This writes `plugin.toml` beside the service's config and starts indexing in the
background without restarting seekfs. `plugin config content --root <path>`
selects explicit roots; see [FEATURES.md](FEATURES.md) for setup and diagnostics.
Legacy `[features.content] enabled = true` in `seekfs.toml` still works.

The existing environment override also works:

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

With neither config nor the flag enabling content, `content`/`content-index`
exit 1 with "content search is disabled". See [FEATURES.md](FEATURES.md) for the
shared feature lifecycle and compatible companion-service backend.

The service's new content build scope defaults to detected Git worktrees plus
Documents, Desktop, and Downloads on the volume. It excludes common generated
directories and system paths. `content-index -db <gsi> --estimate --json`
reports the prospective scope without writing a sidecar; `--scoped` applies it
to an offline build. `--scoped` refuses an `off` scope and errors if the byte
budget is exceeded. A service build that reaches the budget marks content
health incomplete. Estimates apply the same root priority and use source size
as a proxy for extracted text; compressed documents can make the actual build
reach the budget earlier. `budget_hit` reports when the estimate's proxy reaches
the limit. Set `[content]` keys `mode` (`auto`, `explicit`, `off`),
`roots`, `exclude`, `exts`, `git`, `system_excludes`, and `budget_bytes` in
`seekfs.toml`; `[content."C:"]` overrides one volume. Environment overrides
are `SEEKFS_CONTENT_SCOPE`, `SEEKFS_CONTENT_ROOTS`,
`SEEKFS_CONTENT_EXCLUDE`, and `SEEKFS_CONTENT_BUDGET_BYTES`. Sidecars store
a fingerprint of their resolved scope; a sidecar whose scope moved (a repo
appeared or vanished, an ignore rule changed, roots/exts/budget edited) is
reconciled incrementally at attach instead of rebuilt: the current selection
is diffed against the doc table, newly selected files enqueue for extraction
and dropped ones tombstone for the next fold, which persists the reconciled
fingerprint (§6p). Only a diff past half the base docs, an unresolvable
scope, or an extractor/format incompatibility rebuilds from scratch. Only an
explicit `budget_bytes` joins the fingerprint.

Inside a detected worktree the scope follows git's ignore rules with git's
precedence: `.gitignore` files, then `.git/info/exclude`, then the global
excludes file (`core.excludesFile` from the repo's `.git/config` or the
user's `~/.gitconfig`, else `%USERPROFILE%\.config\git\ignore` /
`$XDG_CONFIG_HOME/git/ignore`). Patterns match case-sensitively, like
ripgrep and like git for every file that exists (git's `core.ignorecase`
only relaxes matching for paths with no on-disk entry); folding would
silently drop tracked source whenever a pattern differs in case
(`build/.gitignore`'s `LASTCHANGE*` vs the tracked `lastchange.gni`). The
global file joins the scope digest, and the drain loop polls its stat (it
lives outside the volume, so the USN stream never reports it); an edit
reconciles the selection difference under the new rules (§6p) instead of
rebuilding the sidecar.

The service selects Git repositories before configured roots, then known
folders. It spools accepted extracted text to temporary disk before assembling
the FRN-sorted sidecar, so a large scoped build needs temporary disk space up
to its extracted-text budget. Assembly itself is fully streamed: the text
store is spooled to a temp file as docs arrive, the term/gram posting sections
merge straight into section files, and the `.gsx` is written by streaming
those payloads with a running section table (see §6j). A 64 MiB shard builds
or folds under ~250 MB of heap.

File selection is ripgrep-style: unless `[content] exts` pins an explicit
allowlist, every file the scope admits is opened and head-sniffed at build
time — NUL in the first 8 KiB means binary (the same heuristic ripgrep uses;
BOM and BOM-less UTF-16 still count as text). Only known-binary extensions
(images, archives, media, objects, fonts, legacy OLE documents) are rejected
without opening. Unknown extensions and extensionless files (`OWNERS`,
`BUILD`, `DEPS`, `Makefile`) are text candidates: indexed when they sniff as
text, tallied as `binary` in the `.gsx` policy and content health when they
do not. `content-index -db <gsi> --estimate` reports `text_candidates` and
`binary_candidates` separately, and `--sniff-sample N` opens a stride sample
of the candidates to project the binary share (`projected_text_files`).

There is no default extracted-text budget: the default scope indexes
everything selected, and selection stops only when the projected index would
push the volume holding the sidecar below a free-space reserve — 5% of the
volume's capacity, clamped to [10 GiB, 20 GiB] (see `contentScopeReserveFor`
in `content_space.go`). Sharding bounds build and fold memory per 64 MiB shard,
so the budget is really disk space. Selection budgets the build's peak, not
just its output: the text spool (1× extracted) lives next to the sidecar until
the last shard is saved, so the projection scales extracted bytes by
`contentScopePeakMultiplier` (1.5 = spool + `contentScopeEncodedMultiplier`,
0.5, calibrated against the compact encoding at ~0.33 of extracted text),
plus in-flight shard scratch, and credits the bytes already spooled when it
refreshes mid-build. Each shard's scratch is removed as soon as the shard is
saved. The encode-time size-cap check stays the backstop. Corpora past the shard target build as folder shards, each
individually capped at the 4 GiB per-file sidecar cap (an over-cap shard
refuses to publish, like a single-file over-cap build), which is why totals
past 4 GiB are safe. A volume already at or below its reserve selects nothing
more (allowance 0 — no fail-open); the same reserve governs the write-time
guard, so selection and writes agree. Set `[content]`
`budget_bytes` (or `[content."E:"]` per volume, or
`SEEKFS_CONTENT_BUDGET_BYTES`) to pin an explicit extracted-text budget,
which is enforced exactly as before and wins over the reserve. Only an
explicit budget joins the sidecar identity: the reserve derives from live
disk figures, and fingerprinting it would rebuild on every drift. When a
limit does bite (explicit budget or reserve), the build still extracts the
excluded files and tallies their exact sizes, so the `.gsx` policy records
exact `scope_dropped` files and `scope_dropped_bytes` plus the enforced
limit (`budget_bytes`, or `reserve_bytes` when the reserve bit); content
health (`loaded --json`,
`scope_dropped`/`scope_dropped_bytes`/`budget_bytes`/`reserve_bytes`) and
the UI footer surface them as e.g. "12,345 files not indexed (budget 2 GiB)
— raise [features.content] budget_bytes" or "12,345 files not indexed (disk
reserve 20.0 GiB) — free up space or set [features.content] budget_bytes"
instead of a vague "partial results".

Walk-indexed volumes (`seekfs index -root <dir>`, FAT/exFAT/ReFS-without-journal
drives, network shares, non-Windows builds) get the same service-owned build
through the same scope machinery, keyed on the stable path hash instead of the
FRN (see §6k). Git worktree detection reads the index's `.git` markers, so a
walk root that is not itself inside a worktree (e.g. a subtree of a repo)
detects no repos under auto scope; pin it explicitly instead:

```powershell
$env:SEEKFS_CONTENT_SCOPE = "explicit"
$env:SEEKFS_CONTENT_ROOTS = "E:\bench\chromium\third_party\blink"
```

or `plugin config content --root <path>`.

### 2b. Content matching semantics

A `content:` leaf is a **case-insensitive literal substring** match by default:
the query text is folded with Unicode `strings.ToLower` and tested for a byte
substring anywhere in the document's verified bytes (`bytes.Contains`; v10
reads the file from disk — there is no stored text anymore, see §6y). There is
**no tokenization, word-boundary check, or stemming**, so `content:foo` matches
a document containing `foobar` or `xfoox` but does **not** match one containing
only `fo` or `oo`. `content:"foo bar"` is a phrase: the space is part of the
literal substring. `case:true` selects case-sensitive matching (the raw,
case-preserving verified bytes are compared byte-for-byte with no fold). For
precision the index cannot express literally — word boundaries, alternation,
anchors — use the regex form `content:/pattern/`, which is compiled with `(?i)`
unless `case:true` is set. The fold is the same on both sides because the
builder folds documents and the query folds the leaf with the one shared
`contentFoldText`.

**Typo tolerance.** A `content:` query that matches zero documents is retried
with per-word corrections from the term dictionary (the folded CXTR words):
each correctable word (4+ runes) of every positive term/phrase leaf is replaced
by the dictionary term within edit distance 1 (2 for 8+ rune words) with the
best document frequency, and the first retry that returns results wins (at most
a few trials: single-word rewrites, then the combined rewrite). Exact results
are never altered — fuzzy only fires on a total miss — and `case:true` queries,
regex leaves, and NOT leaves are never corrected. Sharded indexes merge
candidates across shards, and the resident delta vocabulary covers unflushed
changes. The response sets `fuzzy` plus `fuzzy_correction` (the corrected
words), so the UI summary says "Showing results for receive" and counts agree
with searches. Measured on the Chromium content sample (62k docs, 500 random
5+ letter words, one random edit each): 98.4% top-1 at p50 ~24 ms / p99
~40 ms per corrected term (`content_fuzzy.go`, `content_fuzzy_test.go`).

**Per-format scope.** Matching is against *extracted* text, not raw bytes. Plain
text/code is indexed as text. HTML indexes the charset-decoded source with its
markup (extractor v2, ripgrep parity for code search: tags, attributes and
inline script/style match; `contentHTMLText` still renders visible text for
HTML email parts). OOXML (docx/xlsx/pptx), PDF (text layer), RTF, email
(EML/mbox/MSG) and OOXML shared strings are indexed as their extracted
content.

Service health: when the flag is on but no valid `.gsx` is attached, the service
reports `content.state = "unavailable"` — never `"ready"` with an unusable
index. Content queries are served once a volume is usable; when some (not all)
eligible volumes are unusable the response is marked degraded/incomplete, and
when none is usable the query errors rather than returning empty.

## 3. Architecture map

| File | Role |
| --- | --- |
| `content_encoding.go` | tgrep-ported text decoding: BOM sniff (UTF-8/16LE/16BE), lossy repair with maximal-subsequence semantics, and `contentLossyFixups` mapping repaired offsets back to source bytes. |
| `content_index.go` | `.gsx` container: magic/version, `Origin`, section table (with per-section CRC32C), 64-byte `contentDoc` records, encode/decode, atomic save (in-memory and streaming), FRN bisection. |
| `content_integrity.go` | Post-attach checksum verification, stale-marking and rebuild scheduling for corrupt sidecars. |
| `content_postings.go` | CXGR trigram block codec: `key -> []docID`, delta-varint docID lists or per-block bitmaps (encoder picks smaller), 4096-doc blocks with first-docID skip metadata, inline 3-byte keys, bounded lookup. No tf: no query path reads it. |
| `content_external_build.go` | Bounded external spill/merge builder for grams (`contentExternalSectionBuilder`) producing bytes identical to the in-memory encoder. |
| `content_extract.go` | `contentExtractor` interface, registry, `contentExtractorForPath` (extension → format sniffers → text fallback), binary sniff, size caps. |
| `content_extract_text.go` / `_ooxml.go` / `_pdf.go` | Extractors: text/code; OOXML/ODF/EPUB; best-effort PDF (spike quality). |
| `content_open_windows.go` / `_other.go` | `contentOpenNoRecall`: opens with `FILE_FLAG_OPEN_NO_RECALL` so cloud placeholders fail instead of hydrating. |
| `content_build.go` | Offline builders + options. `buildContentIndexFromDir` (walk, `origin=walk`, path-hash keys), `buildContentIndexForIndex` (USN, `origin=usn`, record-FRN keys), `assembleContentIndex` (streams postings through the external builders), `contentRepairText` (case-preserving extraction), `contentFoldText` (case-folded index view), `contentPathKey`, `contentTermsOf`, `contentGramsOf`, v7 path table codec (sorted + front-coded + permutation). |
| `content_filter.go` | v10 per-file bloom filter (§6y): size classes, frozen hash, CXFL section codec, uniform prefilter gate over query leaves, verify-direct disk reader with sharing-violation retry, test seams. |
| `content_textblock.go` | REMOVED in v10 (was: blocked compressed text store, CXST v2/v3). |
| `content_textdict.go` | REMOVED in v10 (was: per-shard trained text dictionary, CXTD v8). |
| `content_termsdict.go` | Dictionary-only terms section (CXTR v2): front-coded keys with restarts, u8 frequencies + sorted overflow table; serves the fuzzy dictionary. |
| `content_dict_build.go` | Bounded external builder for the term dictionary (key counting, no postings). |
| `content_shards.go` | Sharded sidecars: manifest codec (CXSH, format v5), folder shard planner and longest-prefix assigner, composite load, shard-aware candidate streams and `under:` shard mask, per-shard fold planning/source, shard file sweep. |
| `content_read.go` | `contentReader`: decode sections, trigram-intersection candidates, per-file filter gate, substring verification from disk, `contentHit{Path,DocID,Offset}`. |
| `content_resolver.go` | `contentResolver`: merges FRN-sorted docs against the base FRN column → `docID -> recordID` (`contentNoRecordID` = dropped). |
| `content_query.go` | `contentLeaf`, `contentSearchEnabled`, `contentUnavailableError`, `parseContentLeaf`, `queryHasPositiveContentLeaf`, deterministic `contentAssignLeafIDs`. |
| `content_fuzzy.go` | Typo tolerance for zero-result `content:` queries: dictionary correction per word (pigeonhole/bitmap prefilters + bounded Damerau-Levenshtein, reusing `fuzzy.go`), merged across shards and the delta vocabulary, retried through the exact search/count paths; `fuzzy_correction` surfaces the corrected words. |
| `content_tokenize.go` | Quote- and regex-span-aware query tokenizer (`content:/a b/` survives whitespace and `|`). |
| `content_service.go` | `contentVolumeState` (idx/reader/resolver/delta/state/health), `contentDelta`, `contentCoordinator` (dirty/promote/drain/invalidate), `contentExtractDeltaDoc`, `processQueue`, `contentEligibleForExtraction`, `contentHealth`, `sha256Of`. |
| `content_service_wiring.go` | Service glue: `contentIndexPathForDB`, `contentBaseFRNColumns`, `attachContentForVolume`, `contentDrainLoop`, `contentResolvePath`, `rebindContentAfterBaseSwap`, `contentLookupFRNColumn`. |
| `content_cli.go` | `cmdContentIndex`, `cmdContent`. |
| `content_spike_test.go` | Env-gated measurement/oracle harness (`SEEKFS_CONTENT_SPIKE_DIR`). |
| `cli.go`, `postings.go`, `query_parse.go`, `service_protocol.go`, `service_replay.go`, `service_runtime.go`, `service_server.go`, `types.go` | Small integration edits (command cases, `parsedQuery.Content`, `serviceResponse.Content`, coordinator hook, attach loop, resolver rebind). |

## 4. Invariants (do not break these)

- **Origin guard.** `.gsx` carries an `Origin`. `contentOriginWalk` = doc key is
  the stable path hash (`contentWalkKey`: FNV-1a of the cleaned absolute
  path); `contentOriginUSN` = doc key is the NTFS FRN. `attachContentForVolume`
  attaches a sidecar only when its origin matches the volume source (USN↔USN,
  walk↔walk); anything else stays `unavailable`/`stale`, never `ready` with an
  unmapped resolver. A cross-origin sidecar would otherwise report `ready`
  while mapping nothing. The query path derives the key the same way
  (`contentTextForEntry` branches on the attached origin *and* the volume
  source), so results can never resolve to the wrong record.
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
toolchain (not available in this environment). Chromium sample (64,970 files,
557.6 MB text): 185.1 MB total, 0.332x — see §6m.

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

- **Content bypasses the global planner lanes for broad filename sides (PB8
  items 4–7).** The compound lane (see intro) engages only when the filename
  side is small (at most max(1024, 4×limit) estimated candidates): driving the
  filename iterator and verifying every candidate inline is only cheaper than
  the bounded content scan below that line (measured: an 8-doc ext side against
  a 50k-doc broad term answers ~143× faster; a 50k-doc side would cost ~640×
  more than the 16-candidate slow path, so it stays on the content path, at
  identical results). The old "prototype the `globalRecordID` ↔ content-docID
  join" note is obsolete: no join was needed, since `contentCandidatesBounded`
  already maps to local record IDs and the lane drives the other direction.
- **OR-alternative candidate union.** `contentAltCandidateDocs` still
  materializes each alternative's doc set (`budget=0`); a k-way streamed union is
  an optimization tail.
- **`.gsx` mmap (done).** `contentLoadFile` maps the sidecar read-only
  (`contentLoadFileMapped`) and decodes only the doc/path metadata; sections are
  zero-copy views over the mapping, released on replace. A mapping failure falls
  back to `contentLoadFileHeap`. The base `.gsi` still loads into heap unless
  the service runs with `-lowmem`.
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
- **PDF extraction** is a bounded text-layer parser (classic + xref-stream +
  ObjStm, Flate(+predictors)/LZW/ASCII85/ASCIIHex/RunLength, `/ToUnicode` +
  `/Differences`, and text drawn through `/Form` XObjects via `Do`). Limits: no
  OCR; encrypted → `Skipped`; Type0/CID without a `/ToUnicode` CMap is dropped;
  a producer ToUnicode that maps a ligature to a single base letter is lossy in
  the PDF itself.
- **Dead code.** `contentPostingIndex.lookup`/`forEach` and
  `contentLossyFixups.toSourceOffset`/`isEmpty` are test-only (annotated).

## 6i. Sharded sidecars (done)

A corpus with more than `contentShardTargetBytes` (64 MiB) of extracted text is
split by folder. `<gsx>.shards\` holds immutable shard files and the `.gsx`
becomes a small **manifest** (format v5, section `CXSH`) listing each shard's
file name, doc count and owned folder prefixes. Smaller corpora still produce a
single file (v6 doc tables since §6k). Older binaries reject v5/v6, so they
rebuild rather than attaching an empty index.

- **Ownership.** A path belongs to the shard owning its longest matching
  folder prefix. The root prefix `""` always exists, so folders created after
  the build have an owner. The planner keeps a folder whole when its subtree
  fits. A bigger folder keeps its own files as one unit and is split by child
  folder. Units are then packed into shards in path order, so neighbouring
  folders share a shard.
- **Composite load.** `contentLoadFile` on a manifest maps every shard and
  returns one `contentIndex` whose `Docs` concatenate the shards with DocIDs
  rebased. The reader dispatches `docText`/`docPath`/candidate streams by DocID
  range. The resolver sorts an FRN permutation because the composite is
  FRN-sorted only within each shard. Health, attach and queries are unchanged.
- **Build.** Each shard is assembled and saved in turn, then the manifest is
  saved atomically, so peak memory is bounded by one shard.
- **Fold.** `planContentShardedFold` marks dirty the shards that hold a delta
  FRN today and the shards owning each delta doc's path. A rename across
  folders therefore drops the old copy and adds the new one. Only dirty shards
  are rewritten, and a new manifest is published. Shard files no manifest
  names are swept after publish; a file still mapped on Windows is left for a
  later sweep.
- **`under:`.** `shardMaskUnder` limits content candidates to the shards that
  can hold docs under the folder: the folder's owner plus any shard owning a
  prefix inside it, including a shard that owns `under` itself (equality
  counts: the folder's own files live in its owner's shard, so excluding it
  dropped true matches — caught by the compound-lane differential on a sharded
  corpus).
- **Measured (Chromium, 62k files / 508 MB text):** with a single file, every
  fold re-assembled everything in about 2m13s with a 3.4 GB heap peak. With 64
  MiB shards (10 shards, 6% larger on disk), a 100-file edit in one folder took
  13 s, a clustered 8,192-file edit took 15 s (2 shards), and a spread
  8,192-file edit took 1m43s (all shards), each peaking near 1 GB.
- **Reshard on fold.** A single-file base that grows past twice the target
  through folds is split into shards by the fold that crosses the line.
- **Parallel extraction.** Builds extract on `contentBuildWorkers` files
  concurrently (`NumCPU`, capped at 16; 1 while the user queries or the
  machine is on battery) through `contentPrefetchExtract`, which returns
  results in item order. Selection, budget and abort behaviour are therefore
  unchanged. See §6l for the current throughput work (single-read slurp,
  text fast path, parallel tokenize/sort/shards, adaptive IO priority).
- **Not yet:** shards are not rebalanced between rebuilds. The offline
  `content-index` CLI still writes a single file, but it now spools extracted
  text to disk during the walk and streams the save like the service paths.

## 6j. Streaming build/save + per-section integrity (done)

**Streaming.** `assembleContentIndexStreamed` (`content_build.go`) consumes
documents exactly like the in-memory assembler but never retains the corpus:
each doc's repaired text is appended to a temp text-store file as it arrives,
and the term/gram builders merge straight into section files
(`FinishFile` in `content_external_build.go`) instead of returning whole
`[]byte`. `contentSaveAssembled` (`content_index.go`) streams those payloads
into the `.gsx` with a running section table and patches the header, computing
each section's CRC32C as it goes. The temp-then-rename atomicity, the
`contentGSXSizeError` cap (checked from the payload sizes before anything is
written), the deterministic section order and the byte-identical output for
the same input all match the in-memory save (a test builds the same corpus
both ways and compares bytes). Callers that need the index after a build
(attach, fold publish) reload the saved file through the mmap loader instead
of keeping heap copies. The in-memory API stays for tests and small corpora.
Peak transient heap per shard is one document's text plus the two posting
arenas (now 12 MiB each) plus O(docs) metadata and the posting entry/block
tables; the per-document tokenizer map hints are capped so one huge document
cannot preallocate gigabytes of empty buckets (hints are allocation-only and
do not change the output).

**Integrity.** Every section-table entry carries a CRC32C (Castagnoli,
hardware-accelerated via `hash/crc32`) of its payload in the entry's trailing
word, flagged by `contentSectionFlagCRC`. The format version stays 4: older
binaries ignore both words and keep loading, and a file without the flag is a
legacy (pre-checksum) section that loads and is upgraded with checksums on
the next save. Decode verifies the small sections (doc table, paths, policy,
scope, manifest) immediately, so a corrupt small section fails the load; the
large text/term/gram sections are verified by a background pass after attach
(`verifyContentAfterAttach`), so startup does not page in gigabytes. The pass
hashes each index (and each shard index) under its read pin, and `Release`
refuses new pins and waits for pinned reads before unmapping, so the unmap
can never race an in-flight section read even outside `indexMu` (a mapping
torn down mid-pass stops the pass early with an error instead of faulting).
A mismatch drops the base (stale — queries are refused, never served from
corrupt bytes) and schedules a full rebuild (v1 has no per-shard rebuild),
capped at `contentIntegrityMaxRebuilds` per failure streak so a disk that
corrupts every write leaves the volume visibly stale with `integrity_error`
in health instead of spinning rebuilds. The offline `content` command
verifies the whole file synchronously before searching.

**Residual:** a fold that reads a base corrupted after its attach-time pass
re-encodes the corrupt bytes with fresh (valid) checksums, so the corruption
persists until the next rebuild rather than tripping the pass. Closing it
needs per-document checksums or a pre-fold verify (which pages the base);
deferred.

## 6k. Walk-indexed volumes (done)

Content search works through the service for walk-indexed volumes and roots
(`seekfs index -root` databases, FAT/exFAT/ReFS-without-journal drives,
network shares, non-Windows builds), with the same states, progress, sharded
layout, and gitignore scope as USN volumes.

- **Stable key.** Walk record FRNs are positional (`FRN = i+1` in walk order)
  and renumber on every rescan, so docs join on `contentWalkKey` — the FNV-1a
  hash of the cleaned, slash-normalized absolute path, stored in the doc's FRN
  field (no new key field). The key is case-sensitive: two case-differing names
  on a case-sensitive filesystem never share one, while a Windows case-only
  rename becomes a delete+create pair (correct, not a misjoin). The service
  builds a key→recordID join column per base generation (`walkKeySet`,
  refreshed at attach/rebind, shared read-only with search snapshots); the
  persisted `.gsi` FRN column is untouched.
- **Change detection without a journal.** `contentReconcileWalkVolume` diffs
  (path, size, mtime) of the live filename index against the doc table and
  enqueues changed keys / tombstones deleted keys into the existing
  delta/fold pipeline, whose extraction hash-verifies before upserting. The
  diff compares the v6 fingerprint (`FileSize` source bytes +
  `FileModNano` source mtime nanos, stamped by every builder and carried by
  every fold); same-second FAT/exFAT rewrites are still caught whenever the
  size differs. Out-of-scope files are skipped silently (never tombstoned —
  phantom tombstones once bloated the delta and forced constant folds), empty
  files are never enqueued, unindexable files are remembered per fingerprint
  (coordinator decline hook) until they change, and a pre-fold restore
  rescinds its tombstone. Only the drop side is rebuild-gated: past half the
  base docs vanishing, a clean rebuild replaces the reconcile (§6p); changed
  files always re-extract incrementally.
- **Freshness layers.** The existing `ReadDirectoryChangesW` watcher rebuilds
  the filename index on change; the rebuild path rebinds the resolver and
  reconciles immediately after. The drain loop reconciles on every filename
  generation change, and a 60 s safety net (`contentWalkReconcilePeriod`)
  covers a missed watcher event and platforms without a watcher. New files are
  discovered by the diff (no extra directory walk); renames are delete+create
  pairs. Cost per reconcile is one metadata index scan; extraction stays
  serial/IO-governed through `processQueue`.
- **Origin-appropriate watermark.** Walk bases stamp `JournalID = 0` and
  `CheckpointUSN` = wall-clock build/reconcile nanos. Attach skips journal
  validation (scope + extractor + zero-join guards still apply, plus a nonzero
  watermark requirement so hand-built sidecars rebuild under the service's key
  convention); restart freshness is a reconcile at attach; folds restamp the
  watermark instead of advancing a USN checkpoint. USN invariants (§4) are
  unchanged — every walk branch is additive.
- **Format v6.** Doc records widen 64→80 bytes with the fingerprint. Older
  sidecars fail the version gate and rebuild; manifests stay v5; shards, folds,
  and the offline CLI carry the fields unchanged.

Attach also runs the background integrity pass (per-section CRC32C, §6j),
and the sharded layout, streaming save, fuzzy variants, and compound lane all
operate on the walk base unchanged (the lane verifies through the same
walk-keyed resolution).

Tests: `content_service_walk_test.go` mirrors the USN ones (build/attach/query,
edit/create/delete/rename via watcher-equivalent rebuild + drain, fold,
restart, sharded variant, zero-join-stays-stale, steady-state idempotence,
unindexable convergence, restore-before-fold, compound-lane parity).

Measured on `E:\bench\chromium\third_party\blink` (225,303 filename entries):
service build ~19 min for 178,595 files → 177,311 docs in a 669 MB sharded
sidecar (plus `binary: 873` declinations in health), identical with and
without readiness polling — polling was exonerated as a slowdown (`loaded`
never trips the query-activity governor, and the 4-worker count is sampled
once at build start; the time is cold-IO extraction plus sequential
streaming assembly on a shared box). `content:PaintLayer` answers in ~40 ms
through the service and ~0.5 s cold through the offline `content -db` path
(which verifies section CRCs before searching); create→findable 6.2 s and
delete→unfindable 4.7 s end to end (dominated by the ~5.5 s full filename
rewalk the pre-existing watcher performs on this tree); converged
steady-state reconciles are silent and queue nothing.

## 6l. Build throughput (done)

The §6k 19-minute blink build was profiled per stage (warm and cold, CPU +
block profiles, `zz_buildperf_profile_test.go`, since removed) and rebuilt
for throughput. Output is byte-identical to the old builder on the same
inputs — proven by unit tests per fast path and by a full-corpus
old-vs-new sidecar comparison (177,311 docs, 7 shards: doc tables, stored
text, and every shard's text/terms/grams sections identical). Recall,
memory bounds (~250 MB per 64 MiB shard) and the per-section CRCs are
unchanged.

**Profile (2,000 Chromium files, per-file means).** Cold: `open` 7.5 ms
(first-touch MFT + Defender, 93% of per-file cost), everything else <0.4 ms
combined. Warm: grams 0.32 ms, terms 0.07 ms, open 0.04 ms, the two reads +
decode + hash + fold ~0.15 ms. Assembly (20k files / 151 MB text): 24.4 s
old, dominated by the serial spill-run sorts (~50%), the posting merge and
single-threaded tokenize.

**What changed (all in the build path; query paths untouched):**

- **Workers scale to the hardware.** `contentBuildWorkers` is `NumCPU`
  (capped at 16), not half the CPUs capped at 4. The adaptive throttle is
  unchanged: 1 worker while the user queries or the machine is on battery,
  and the tokenize / run-sort / shard stages reuse the same adaptive count.
- **One read per file.** `contentSlurpNoRecall` opens with `OPEN_NO_RECALL`,
  stats, and reads the whole (capped) file once; the extractor aliases the
  slurped bytes instead of reading twice (head + body) and copying. Over-cap
  files keep the streaming handle path.
- **No per-file goroutine/timer for text.** `contentExtractSafely` runs the
  plain-text extractor inline with panic isolation (it is a bounded read +
  decode and cannot hang); container parsers keep the full
  goroutine + deadline supervision, and the hang/spin/panic drain tests still
  pass.
- **Cheaper normalize.** The assembler aliases already-decoded stored text
  (`contentStoredText`, exact on BOM/UTF-16-NUL edges by construction),
  folds ASCII in one pass (`contentFoldText`), skips the redundant per-word
  lower (`contentTermsOfFolded`), and keys grams as `uint32`
  (`contentGramsOfU32`, rendered only for distinct grams).
- **Parallel tokenize.** `assembleContentIndexStreamed` pulls a bounded
  read-ahead batch (64 docs / 64 MiB, vars for tests) and normalizes,
  hashes and tokenizes it on the worker pool, emitting in docID order. Each
  doc's bytes are copied at pull time because a source may reuse its backing
  buffer (`TestContentAssembleStreamConsumesSourceBuffer` pins this).
- **Parallel run sort.** Spill runs sort by MSD radix partition on the first
  key byte: 3-byte gram buffers take a comparison-free stable counting path
  (input docID order is ascending per key, so stability is the tiebreak);
  term buffers sort each group independently across workers with no
  cross-group merge. Runs are byte-identical at any worker count.
- **Parallel section finish and shard assembly.** Term and gram
  `FinishFile` run concurrently; sharded builds assemble up to
  `contentShardBuildConcurrency` (2) shards at once, each in a private
  scratch directory (the streamer names its term/gram files with fixed
  names — sharing one directory corrupted a shard, caught by the attach
  checksum gate and fixed).
- **IO priority is adaptive.** The Low disk-priority hint now applies only
  while the user is active or on battery (`SEEKFS_CONTENT_IOPRIORITY`
  forces it); an idle build keeps normal priority.

**Measured (same 20k-file / 151 MB benchmark, GOMAXPROCS=16):** extraction
warm 16-worker 0.032 → 0.015 ms/file; assembly 24.4 s → 6.5 s (3.75×).
Service build of blink (178,595 files → 177,311 docs, 7 shards, warm cache,
loaded box): extraction ~75 s, assembly + save + attach ~40 s, **ready in
~2 min** vs the §6k 19 min cold baseline; cold-cache full pass on the same
box took ~16 min (first-touch opens dominate: 468k × ~7.5 ms / 16 workers).
`content:PaintLayer` still answers in ~40 ms through the service.

Not yet: the posting merge (`mergeInto`) is still serial per section (few
runs per shard keep it small); shards are not rebalanced between rebuilds;
the offline `content-index` CLI still writes a single file.

## 6m. Compact index (done)

The `.gsx` is format **v7**. The v6 index was bigger than the text it
covered; v7 is ~0.33x, with identical recall/results, the per-shard memory
bound kept, and section-level CRC32C integrity unchanged (every new byte
range sits inside a checksummed section).

**What changed:**

- **Stored text is blocked zstd** (`content_textblock.go`, CXST v2). The
  logical stream is unchanged — the doc table's `TextOff`/`TextLen` still
  address it, so folds, reshards, walk volumes, snippets and verification
  are untouched — but physically it is independently-decodable zstd blocks
  (~512 KiB uncompressed, cut at doc boundaries, `BestCompression`) with a
  block table up front. Verification and snippets decompress only the
  blocks covering the candidate docs; candidates arrive in docID (hence
  block) order, so sequential reads share blocks through a 4-entry
  decompressed-block LRU per loaded shard (~2 MiB). A corrupt offset or
  undecodable block reads as absent text, never a wrong slice.
- **Term postings are gone** (`content_termsdict.go`, CXTR v2). No query path
  read them (candidates come from grams, relevance re-verifies, fuzzy needs
  only the dictionary + df), so CXTR is the dictionary alone: front-coded
  keys with a restart every 32 keys plus a u8 frequency per key and a small
  sorted overflow table for df ≥ 255 (measured: 58% of dfs are 1, 99.7% fit
  a byte). Fuzzy lookup stays logarithmic, scans stay sequential.
- **Gram postings are docIDs only** (`content_postings.go`, CXGR v2). tf was
  always 1 and nothing reads it; maxTF metadata went with it. Each
  4096-doc block (up from 1024) encodes as a delta-varint list or a bitmap,
  whichever is smaller; the streaming iterator expands either and stops
  mid-block, so bounded candidate materialization is unchanged. Fixed
  3-byte keys sit inline in the 16-byte entries (no key blob); block metas
  shrank 24 → 16 bytes (u32 offsets; sections never exceed the 4 GiB cap).
- **Paths are sorted + front-coded** (`encodeContentPaths`): a sorted copy
  with prefix compression plus a docID → sorted-position permutation for
  O(1) lookup.
- **Doc table unchanged** (80-byte records, `TextOff`/`TextLen` logical).
  `RawSize`/`DocLen`/`ModUnix` are still stamped but nothing reads them
  (verified by grep); dropping them would save ~20 B/doc and is deferred.

**Measured (same input both sides: 64,970 Chromium files selected by the
offline walk builder with the service extension allowlist):**

| section | v6 | v7 | v7/v6 |
|---|---|---|---|
| text (CXST), logical 557.6 MB | 552.8 MB raw | 103.9 MB zstd (0.186x) | 0.19 |
| terms (CXTR) | 112.1 MB | 18.8 MB dict | 0.17 |
| grams (CXGR) | 158.9 MB | 55.7 MB | 0.35 |
| paths (CXPT) | 4.6 MB | 1.5 MB | 0.33 |
| doc table | 5.2 MB | 5.2 MB | 1.00 |
| **file total / text** | **833.6 MB / 1.50x** | **185.1 MB / 0.332x** | **0.22** |

(The v6 baseline predates the HTML-source extractor (v2), which adds ~4.7 MB
of markup text to the same file set; the v7 column includes it. Build time
9–13 min cold, ~80 s warm on this box — extraction-dominated either way;
zstd `BestCompression` adds ~20 s single-threaded. Query latency below.)

**Sharded layout.** The service shards past 64 MiB of text for fold
isolation; resharing the same corpus gives 11 shards totaling 204.3 MB
(0.366x) — a 10.3% overhead over the single file (6% in the v6 era). The tax
is structural and shard-local: every shard carries its own term dictionary
and gram entries, so corpus-wide vocabulary (common words, dense trigrams)
replicates once per shard instead of living in one global table. Sharing one
dictionary across shards would make every fold rewrite every shard, which
is exactly what sharding exists to avoid — so the sharded total sits just
over the 0.35 line while the encoding itself is 0.332x. Per-shard peak
memory stays bounded (slowest shard assembles in ~8 s warm).

**Query latency (same file set, offline `contentReader`, limit 20 unless
noted; candidates identical modulo the HTML-markup additions):**

| query | v6 | v7 |
|---|---|---|
| `content:struct` (14.5k candidates) | 1.51 ms | 1.10 ms |
| `content:EINVAL` (4.4k) | 30.5 ms | 59.8 ms |
| `content:cityhash_v103` (10) | 1.55 ms | 10.8 ms |
| `content:"reassemble"` (125) | 40.5 ms | 134.6 ms |
| regex `mutex_\w+` full scan (10 hits) | 15.2 s | 17.9 s |
| `ext:cc content:mutex` full-verify (1557 → 37) | 130 ms | 699 ms |

Recall is identical (same candidate sets modulo markup, same hit counts and
order). Interactive limit-bounded shapes stay ms-scale: selective queries
pay ~1 ms per cold 512 KiB block decompressed (cityhash +9 ms absolute);
dense-broad queries verify few contiguous docs (struct faster — fewer bytes
paged). Shapes that verify hundreds–thousands of scattered docs (sparse
queries with heavy trigram false positives, unbounded compounds, broad
counts) pay one compressed pass: ~0.2–0.7 s on this corpus (compound,
full-scan regex within run noise + decompress). The cost is compulsory
block decompresses (~500 MB/s), not the LRU (single-pass working sets);
smaller blocks would halve it at ~+10 MB of ratio (256 KiB/`Best` measured
0.205 → 0.349x total, inside the budget but with no margin — rejected).

**Levers measured and rejected:** per-block bit-packing of delta lists
saves only 6% of list bytes (deltas are already ~1.15 B/uvarint); a shared
zstd dictionary per shard was not needed once 512 KiB blocks captured
cross-file redundancy; dropping `RawSize`/`DocLen`/`ModUnix` (~20 B/doc)
is deferred.

**No stored text at all (measured, then shipped as v10 in §6y).** Verifying against the
files on disk shrinks the index to postings + tables only (81 MB, 0.147x)
and warm verification costs ~0.11 ms/file — comparable to indexed warm —
but cold verification pays Defender first-touch (~7.5 ms/file, §6l):
`content:struct` count 14.5k candidates → ~109 s, `content:EINVAL` →
~33 s, the `ext:cc` compound → ~12 s cold, versus ~1 s or less indexed.
It also trades the `ContentHash` change detection for stat-and-revalidate
races on every verify (stale benevolence: a file rewritten between
candidate generation and verification verifies wrong). Not worth it as a
default or an opt-in *in the §6m design* (no prefilter before reads):
2.3x smaller for 30–100x worse cold broad queries
and unsound freshness. No code was added *then*; §6y ships it WITH the
per-file filter gate, per-query disk budget, and verify-direct freshness,
which is what bounds the cold shapes the §6m sketch left unbounded.

**Upgrade:** old sidecars fail the version gate on load and are never
attached; the PF-3 path rebuilds them (covered by
`TestContentIndexRejectsPreviousVersion`). Manifests stay v5 (they name
shard files; each shard carries v7). Fold, reshard, integrity, walk
reconcile and the compound lane all go through `docText`/the assembler
and work unchanged (proven by the existing differentials on v7 corpora).

## 6n. Query-latency recovery (done)

The v7 index regressed verify-heavy shapes (per-block zstd decompression
where v6 sliced mmap): EINVAL 30→60 ms, reassemble 40→135 ms,
`ext:cc content:mutex` 130→699 ms on the 65k-file sample. Size stays
byte-identical (185.1 MB — no encoding change, no rebuild). All fixes are
read-path only; recall is untouched.

**Measured root causes** (same corpus, offline reader): zstd decodes at
~730 MB/s effective here (~870 µs fresh / ~720 µs pooled per 512 KiB
block, single-threaded), so every cold block touch costs ~0.7–0.9 ms and
verify-heavy queries are decompress-bound (cityhash: 9 blocks for 10 docs;
compound: 629 blocks for 932 `.cc` docs averaging 55 KiB). The 4-entry LRU
thrashed on any working set past 2 MiB; parallel verifiers raced the same
block (duplicate decodes); each verified doc re-allocated the needle string
and walked the fold offsets twice; the service fetched text even for entries
its scalar side already rejected.

**What changed (all read-path; size byte-identical at 185.1 MB):**

- **Shared block cache** (`content_textblock.go`): one bounded LRU per
  reader (shared across a composite's shards) holding decompressed block
  copies — never mmap aliases, so the pin discipline
  (`pinRead`/`Release`) is untouched and dropping the reader drops the
  cache. Cap defaults to TotalRAM/64 clamped to [32 MiB, 128 MiB],
  overridable with `SEEKFS_CONTENT_BLOCK_CACHE_MB` (clamped to
  [8 MiB, 256 MiB]). Repeat queries, typing refinements and the snippet
  pass after a search hit warm blocks.
- **Pooled decode output**: one ~150 µs fresh-512 KiB allocation per miss
  removed; duplicate inserts recycle. Pool discipline is size-capped so a
  lone huge document's block never pollutes it.
- **Block singleflight**: parallel verifiers racing the same cold block
  decode it once (joiners wait, then hit); claim/install/close accounting
  transfers every buffer exactly once, with a private-decode fallback.
- **Parallel offline search** (`content_read.go`): feeder (sole stream
  puller) + `min(GOMAXPROCS, 8)` verifiers + ordered merger that restores
  exact docID order and the exact limit (over-verification bounded by
  channel + workers). Serial stays for 1 worker. Proven identical to
  serial across terms × limits × worker counts by
  `TestContentParallelMatchesSerial` (which caught a real over-limit bug
  and an empty-term edge before they shipped).
- **Verify micros**: needle hoisted to bytes (no per-doc string alloc),
  both fold-offset boundaries mapped in one rune walk
  (`contentFoldSpanToRaw`, proven equal to the two-call form, also used by
  the service snippet path).
- **Scalar short-circuit** (`entryMatchesWithContentMatcher`): entries the
  name/scalar side already rejects never fetch text (pure predicate
  reorder; the service compound skips every non-`.cc` candidate's
  decompress). The service's bounded scan stays serial early-exit by
  design (see the `verifyCompactCandidateOrderParallel` comment); it
  inherits the cache, pooled decode and short-circuit automatically.

**Lever reports:** (1) shared cache as above (typing refinements hit warm
blocks; sizing from RAM with env override; pin-safe copies; reader-owned
release); (2) parallel verification as above (GOMAXPROCS-bounded, ordered,
exact); (3) candidates were already block-ordered (ascending docIDs) so
each block decompresses once per query — plus singleflight for races;
(4) s2/LZ4-class measured and rejected (1.6x faster decode at 1.6x worse
ratio: 104 → ~170 MB text — size would move); pooled zstd output kept;
(5) snippets needed no change (offline computes them inline from already
verified text; the service pass goes through the hot cache).

**Re-measured table** (same 6 shapes, min-of-N; GOMAXPROCS=2 through
`gorun.sh`; v6 baselines from §6m; this box is a loaded 16-thread desktop,
so cold numbers carry contention — min approximates the uncontended floor):

| query | v6 | v7 warm (128 MB) | v7 cold | 1.5x target |
|---|---|---|---|---|
| `content:struct` | 1.51 ms | 0.5 ms | 2.1–2.6 ms | ≤2.27 ms |
| `content:EINVAL` | 30.5 ms | 10–13 ms | 43–48 ms | ≤45.75 ms |
| `content:cityhash_v103` | 1.55 ms | 1.0–2.3 ms | ~5–6 ms | ≤2.33 ms |
| `content:"reassemble"` | 40.5 ms | 18–23 ms | ~66 ms | ≤60.75 ms |
| regex full scan | 15.2 s | ~15–16 s | ~15 s | ≤22.8 s |
| `ext:cc content:mutex` full-verify | 130 ms | ~700 ms (LRU pathology, below) | ~600 ms | <200 ms |

Warm meets every target except the compound: its 322 MB cyclic working set
LRU-thrashes any cache below ~322 MB (measured ~725 ms warm at both 128 MB
and 256 MB caps — 0% hits, structural, not a bug). Cold meets
struct/EINVAL/regex and sits ~1.1x over on reassemble (contention noise
either way); cityhash-cold and compound-cold are physics-bound at 512 KiB
blocks and ~0.7 ms measured decode: 9 and ~629 compulsory misses ÷
2 workers. s2/LZ4-class would decode ~1.6x faster but costs 1.6x the ratio
(text 104 → ~170 MB) — rejected; the size stays byte-identical at
185.1 MB. At 8 workers (production projection, labeled runs): struct
1.0–2.1, EINVAL ~20, cityhash-cold 2.7 (0.4 ms over — 9 misses over
8 workers is two waves; 16 workers make it one), reassemble 23 ms,
regex ~16 s, and full-verify through the production parallel search
~230 ms contention-inflated (~100–130 ms on a quiet box, under 200). The
16-worker production service (whose candidate path parallelizes already)
clears even cityhash-cold by balance.
No stored-text change was needed and none is proposed.

**Budget policy (done, with the reserve code in `content_space.go`).**
There is no default byte budget: the default scope indexes everything
selected and stops only when the build's projected peak (extracted × 1.5:
spool plus index, see §6t) would breach the volume's free-space reserve — 5% of capacity in [10 GiB, 20 GiB],
monotonic (it never grows as free space shrinks) and shared by selection and
the write-time guard, with no fail-open (allowance 0 at or below the
reserve; unknown free space leaves selection unlimited with the write guard
as backstop). Explicit `budget_bytes` still caps
extracted bytes exactly as before. Both record exact `scope_dropped` /
`scope_dropped_bytes`; the policy carries the enforced `budget_bytes`
(0 when reserve-governed) or `reserve_bytes`, surfaced in
`loaded --json` and the UI footer with the matching operator action.
`--estimate` applies the same rule and projection. Covered by
`TestContentScopeReserve`, `TestContentScopeEstimateReserveTruncates`,
`TestContentServiceBuildReserveDropsExactFilesAndBytes` and the health/UI
summary tests.

## 6o. Small blocks + trained dictionaries (done)

Format **v8**: 64 KiB doc-aligned text blocks (docs larger than a block get
their own frames) plus one zstd dictionary trained per shard at build time,
stored as a CRC-covered CXTD section. Small blocks cut the compulsory
decompression per scattered candidate ~8x (a 55 KiB doc touches one 64 KiB
block, ~170 µs, instead of one 512 KiB block, ~0.9 ms); the dictionary
recovers most of the ratio small blocks lose. The verify working set itself
shrinks ~5x (a broad compound's blocks go from ~322 MB toward ~60 MB),
which is what fits it back under the shared cache.

**Dictionary design.** No in-process zstd trainer exists in the module set,
so the trainer (`contentTrainTextDict`) produces dictionary *content* only —
whole repeated source lines by (−count, line) total order to a 96 KiB
budget — and stores it raw: klauspost supports raw-content dicts on both
sides (`WithEncoderDictRaw`/`WithDecoderDictRaw`), which need no entropy
tables (content is initial history). Training is deterministic (doc order
in, total order out): the assembler samples the first 8 MiB of stored text
riding the normal emit path (no buffering, no reorder) and trains once at
the cap or the exhausted source; below 2 MiB logical (a var for tests) or
with no repeats it stays plain. The encoder level follows the dict
(BetterCompression, which has a dict path — BestCompression ignores dicts)
with a 1 MiB window; sidecars with dict and plain frames coexist
decodably, and missing CXTD fails closed (no text, never wrong text).
Pooled decoders each own a private copy of the dict content, so there is no
lifetime coupling to the mapped section beyond the section CRC. Fold,
reshard, walk reconcile, integrity and the compound lane all funnel through
the assembler/reader and work unchanged.

**Curve** (same 65k-file sample, frames-only ratios; dict rows pair Better
with the line trainer, plain rows are Best):

| block | none | dict 32K | dict 64K | dict 96K |
|---|---|---|---|---|
| 64 KiB | 0.2117 | 0.2083 | 0.2050 | **0.2030** |
| 128 KiB | 0.2020 | 0.2037 | 0.2009 | 0.1992 |
| 256 KiB | 0.1941 | 0.1999 | 0.1977 | 0.1962 |
| 512 KiB | 0.1874 | 0.1967 | 0.1949 | 0.1938 |

The dictionary helps most where blocks are smallest (64 KiB: −0.0087; at
256 KiB+ the weaker Better level costs more than the dict returns, so those
cells lose to plain Best). Dict decode runs ~380–430 MB/s vs ~480–650 plain
(longer history matches); per touch still wins 4–5x (170 µs vs 790 µs).
Training costs ~40 ms; the dict encoder is actually faster than Best
(~9 s vs ~20 s per 553 MB — noise next to extraction either way). A shared
(per-shard, not global) dictionary was chosen for fold isolation; per-block
bit-packing re-measured at 6% and stayed rejected.

**Shipped point: 64 KiB + 96 KiB dict.** Same 64,970-file input
(logical 557.6 MB): text 110.0 MB (0.197x), terms 18.8 MB, grams 55.7 MB,
paths 1.5 MB, doctable 5.2 MB, dict ~0.1 MB, file **191.3 MB (0.343x)** —
targets ≤0.36× single-file and ≤0.40× sharded both hold (sharded reshare:
11 shards, 212.2 MB, 0.381x, +10.9% shard-independence tax as in §6m).
Build time is extraction-dominated as before (~9 min cold here).

Query table, same 6 shapes, min-of-N, GOMAXPROCS=2 (v6 baselines §6m):

| query | v6 | v8 warm (128 MB) | v8 cold |
|---|---|---|---|---|
| `content:struct` | 1.51 ms | 1.0 ms | 1.5 ms |
| `content:EINVAL` | 30.5 ms | 10.2 ms | 36.8 ms |
| `content:cityhash_v103` | 1.55 ms | 2.2 ms | 2.6 ms |
| `content:"reassemble"` | 40.5 ms | 23.4 ms | 52.7 ms |
| regex full scan | 15.2 s | 15.2 s | 15.5 s |
| `ext:cc content:mutex` full-verify | 130 ms | 353 ms (128 MB) / 143.8 ms (256 MB) | 372 ms |
| `mutex` unlimited (production parallel) | — | ~367 ms | 375 ms (G2) |

Warm meets 1.5x everywhere except the serial compound at the default
128 MB cap: its ~110 MB block set fits, but once prior queries' blocks are
also resident it thrashes exactly like the 322 MB set did at 512 KiB
blocks. A 256 MB run measures compound warm at 143.8 ms (under 200 ms) —
the small-block working set is what makes the top of the allowed cache
range sufficient. Cold meets struct,
EINVAL, reassemble and regex; cityhash-cold (2.64 vs 2.33) misses by
0.3 ms — 10 scattered docs over 9 compulsory 64 KiB misses — and the
serial compound (372 ms) by the straddle math below. At 8 workers
(production projection, labeled runs on the same file): struct ~0,
EINVAL 11.0/3.9, cityhash 2.11/2.03, reassemble 15.5/7.5 ms, regex
~16–17 s, serial compound unchanged (~377 ms, serial loop), and
full-verify through the production parallel search 103 ms — every
production shape under its target. The 16-worker service clears the rest
by balance.

Why the serial compound lands at 372 ms: the 932 `.cc` docs average
55 KiB, so ~86% straddle two 64 KiB blocks (~1700 touches × ~170 µs dict
decode ≈ 290 ms) plus ~100 ms of folds v6 also pays plus intersection.
Folds + intersection alone are ~105 ms, so serial needs ~95 ms of
decompression for ~1700 touches (56 µs/touch ≈ 1.1 GB/s) — past this
loaded box (~380 MB/s dict) but reachable on a quiet one, warm with a
≥140 MB-effective cache (~125 ms), or parallel (~40–135 ms at 8–16
workers). No block size fixes it serially on this box (per-touch floor);
no ratio budget exists to buy back with (see curve).

Covered by `TestContentTrainTextDict*`, `TestContentTextDictBuild*`
(mid-stream train, determinism, tiny-skips-dict, missing-CXTD-fails-closed)
and the full suite; `TestContentIndexRejectsPreviousVersion` now guards
v7. The v8 work keeps the §6m budget policy, reserve semantics (§6m,
monotonic 5%/[10,20] GiB shared by selection and writes), pin discipline
(dict decoders own private copies), fold/reshard/walk paths (all funnel
through the sampling assembler and the dict-aware reader) green.

## 6p. Scope reconcile + journal-wrap recovery (done)

On a busy developer machine the scope fingerprint (repo list plus ignore
digest) can move during a long build — a repo appears, a `.gitignore` edit
admits or drops files — and a USN journal routinely wraps under a finished
base. Both used to discard the sidecar (`stale` + full rebuild), potentially
forever. Both now reconcile the selection difference through the existing
delta/drain/fold pipeline instead.

- **Attach + drift scope reconcile** (`content_scope_reconcile.go`). A
  sidecar whose fingerprint differs from the resolved scope diffs the current
  fast-scan selection against its doc table: newly selected keys enqueue for
  extraction, dropped keys tombstone for the next fold, and the fold persists
  the reconciled fingerprint — converging without re-extracting the corpus.
  The steady-state drain loop re-checks the fingerprint every
  `contentScopeDriftCheckPeriod` (30 s) plus a forced check on any in-volume
  ignore edit, so config/env/ignore drift reconciles without a rebuild. Past
  `contentScopeReconcileRebuildFrac` (half the base docs) the diff rebuilds
  instead; in-scope ignore files themselves are never selection (their rules
  are read from disk, and they would burn threshold budget as phantom
  enqueues). The volume stays `ready` and usable throughout.
- **Journal-wrap metadata reconcile** (`content_catchup_reconcile.go`). A
  wrapped journal of the same generation keeps FRN identity, so catch-up
  diffs the live filename index (base records plus the replay overlay, which
  holds the wrap window's creates/modifies/deletes) against the doc table by
  FRN: changed/new keys enqueue, deleted keys tombstone, and the fold claims
  the reconciled volume checkpoint past the lost range. This covers wraps
  during the build, while stopped, and mid-catch-up reads. The volume keeps
  serving (`ready`, health `reconciling` with scan progress) until the fold.
  A journal-ID change (new generation, FRNs unsafe) still goes stale, as do
  sidecars predating the v6 file fingerprint, which cannot diff.
- **Fast scan + build phases** (`content_scope_scan.go`). Selection is a
  memoized parallel scan over the filename index (per-parent decision memo,
  ~4x the legacy loop; differential tests bind it to `allows()`+`priority()`
  on randomized, flat, and drive-root trees). The build reports
  `scanning` (with the record total), `extracting`, and `assembling` phases
  in health/CLI/UI instead of sitting at 0%, and a wrap reconcile reports
  `reconciling` with progress. `BuildPhase`/`BuildTotal` flow through
  `loaded`, the CLI status line, and the UI footer.

Tests: `content_scope_reconcile_test.go` (contract/expand/flood/drift/phases),
`content_catchup_reconcile_test.go` (stopped-wrap, overlay merge, ID-change
stays stale, pre-v6 rebuilds, mid-loop wrap), `content_scope_scan_test.go`
(flat + drive-root differentials, unfilled-cap regression), and the updated
`below-lowest-valid` attach branch (ready + usable, journal never re-read).

## 6q. Index size reduction (done)

With text already at 0.197x (§6o), the remaining mass was terms+grams
(~75 MB, ~39% of the 191 MB single-file point) and the filename index's
derived sections (86 MB of the 125 MB `chromium.gsi`, 556k entries;
resident derived ≈ 78 MB here, scaling to the live service's ≈ 222 MB
at 1.46M entries). Everything below ships with identical recall (differentials
and full-corpus hit equality) and resident query arrays that are
byte-identical, so query latency moves only where noted.

**Content index: format v9** (v8 sidecars fail the version gate and rebuild
through the PF-3 path; manifests stay v5). Measured on identical inputs
(86,213 docs / 640 MB raw selected from `chromium.gsi`, single file):

| section | v8 | v9 | v9/v8 |
|---|---|---|---|
| file total | 220,504,115 | 208,377,748 | 0.945 (−5.50%) |
| text (CXST) | 120,696,117 | 120,696,117 | 1.00 |
| grams (CXGR) | 68,579,142 | 58,462,043 | 0.852 (−14.75%) |
| terms (CXTR) | 22,202,096 | 21,914,876 | 0.987 (−1.29%) |
| paths (CXPT) | 2,031,190 | 2,031,190 | 1.00 |
| doctable (CXDT) | 6,897,044 | 5,172,784 | 0.75 (−25.0%) |
| pruned set (CXGP) | — | 2,184 (728 keys) | new |

What changed:

- **Slim gram rows** (`content_postings.go`, both the in-memory encoder
  and the streaming assembler): entries drop `count`+`firstBlock` and
  block metas drop `byteOffset` (16 → 12 byte rows). The decoder rebuilds
  all three as prefix sums over the key-ordered tables and rejects any
  mismatch (trailing blocks, short blob, out-of-range entry).
- **Singleton gram entries**: ~1/3 of gram keys on code corpora occur in
  exactly one document; they now ride inline (docID in the entry, no
  blocks) instead of paying a 16-byte meta plus a block payload.
- **Nibble-packed term lengths** (`content_termsdict.go`): both front-code
  lengths ride one header byte when each fits in 14, else 0xFF plus the two
  uvarints. Paths keep the legacy two-uvarint framing in a separate
  decoder: repository paths share long prefixes, so the escape cost +1 B
  per path (measured, reverted).
- **Doctable 80 → 60 bytes**: `RawSize`/`DocLen`/`ModUnix` leave the wire
  (no query path reads them; verified by grep). `FileSize`/`FileModNano`
  stay for the walk reconcile.
- **Dense-gram pruning** (`contentGramPruneDF` = 25000, a var): grams past
  the threshold are dropped from CXGR and listed in CXGP. The reader skips
  pruned grams as constraints (verification still decides); a term left
  with no usable gram scans like a sub-3-byte term. The builder buffers
  whole keys so the prune decision precedes any flush (transient peak =
  densest key; the buffer resets to `[:0]` to keep the bounded-heap
  guarantee — a reslicing regression was caught by
  `TestContentExternalBoundedBuffer` before it shipped).

 pruning curve (86k docs, blob bytes at/above df): ≥1k 81.5%, ≥5k 57.1%,
≥10k 37.9%, ≥20k 17.7%, ≥30k 9.7%, ≥40k 5.5%. The 25k line prunes 728
keys (~8.5 MB blob). Benchmark rarest grams stay indexed (struct 17.2k,
EINVAL 11.3k, mutex 4.4k, reassemble 591, cityhash 144), so no flagship
query goes all-pruned; all-pruned degenerate terms (`the`) scan to the
limit and terminate fast exactly because they are common (measured
2–5 ms either way).

Query table, min-of-9, GOMAXPROCS=2, baselines re-measured in the same
sessions (this box carries ±25% same-file contention noise, so only
consistent directional deltas count; hit lists are byte-identical on
every shape, including the all-pruned scan):

| query | v8 | v9 |
|---|---|---|
| `content:struct` (20) | 3.2 ms | 2.6–3.1 ms |
| `content:EINVAL` (20) | 32.5 ms | 36.0 ms (+11%, see below) |
| `content:cityhash_v103` (10) | 2.0–2.4 ms | 0.5–1.0 ms (faster) |
| `content:"reassemble"` (6) | 37–60 ms | 41–50 ms (even, noisy) |
| `mutex` unlimited (109) | 454–510 ms | 509–645 ms (even, noisy) |
| `mutex` (20) | 25–26 ms | 25–26 ms |
| `the` all-pruned (20) | ~5 ms | 0–2 ms |

EINVAL is the one regression over 10% (+11%, +3.5 ms absolute, from 33
extra scattered verify candidates after dropping 2 of its 4 constraints).
It is kept with justification: it stays inside the §6n 1.5x-of-v6 budget
(36.0 vs 45.75 ms), every other shape is even or faster, recall is
identical, and it buys ~8.5 MB. The threshold is a var operators can
raise; raising to 40k would un-prune EINVAL's grams entirely at ~5 MB
less savings.

**Filename index (no version bump).** Readers accept old and new layouts
(the SUBT 3→8 precedent); writers emit the compact forms. Resident arrays
stay byte-identical, so queries cannot move. Measured on
`E:\bench\chromium.gsi` (125,416,892 B) by recoding its sections, plus a
synthetic 556k-entry direct build for the writer proof:

| section | before | after | delta |
|---|---|---|---|
| RANK/SRNK/MRNK/ERNK/TRNK/PRNK (6x) | 26.68 MB (order+rank) | 13.34 MB (order only) | −13.34 MB |
| PATR (5 attr lists) | 9.08 MB raw u32 | 2.27 MB delta-varint | −6.81 MB |
| SUBT (8 parts) | 17.79 MB | 15.56 MB (7 parts) | −2.23 MB |
| **file total** | **125.42 MB** | **~103.0 MB** | **−17.9%** |

Rank sections persist the order alone; `decodeRankSection` rebuilds the
rank as its exact inverse (deleted IDs keep rank 0, bit-identical to the
stored array). PATR reuses the existing `encodeDeltaUvarint32` under a
`PTR2` magic with the legacy framing as fallback. SUBT drops `start`
(the inverse of the persisted order). Load pays one O(n) inversion per
family (~ms; cold CLI open of a 99 MB new-format index is 0.69 s
including the file read). Old-format indexes load unchanged (verified by
CLI search/count against the fixture). Resident is unchanged by design:
ranks, subtree arrays and PNGR are all live-queried in both directions,
and halving resident would trade O(1) access for binary searches on hot
paths.

**Tried and rejected/deferred (with reason):**

- Lower prune thresholds (10k: −23 MB): struct (17k) and EINVAL (11k)
  go all-pruned and fall to scans — flagship latency, not just bytes.
- Nibble-packed path lengths: +1 B/doc (long shared prefixes always take
  the escape); reverted, paths keep legacy framing.
- Elias-Fano posting lists: worse than delta-varint at the measured
  ~1.15 B/posting gap distribution; needs skip-based intersection.
- 8192-doc gram blocks: halves metas but coarsens bounded-decode
  granularity for little net gain.
- Reassigning docIDs for smaller gaps (e.g. path order): breaks the
  DocID-is-FRN-index invariant the resolver joins on.
- Dropping df==1 terms: breaks fuzzy correction recall (98.4% top-1).
- Dropping full term keys for hashes: same reason.
- Larger shard target (fewer shards, less +10.9% tax): grows fold heap
  past the per-shard bound; single-file builds unaffected.
- Shared cross-shard dictionaries: breaks fold isolation (as in §6m).
- Recomputing SUBT minima / SUBS / subtree intervals at load (would drop
  ~15 MB more): every CLI open pays 5+ O(n) passes over the child graph;
  kept on disk.
- CHLD offsets→counts varint (~1.6 MB) and LOWR offset packing (~0.4 MB):
  real but small for the format churn; deferred.
- s2/LZ4 text and smaller text blocks: already rejected in §6n/§6o.

Tests: prune parity/codec/differential tests (`TestContentPostingPrunesDenseKeys`,
`TestContentExternalPruneParity`, `TestContentPrunedGramsCodec`,
`TestContentPrunedIndexMatchesUnpruned`, `TestContentServicePrunedIndexParity`),
prefix-sum validation (`TestContentPostingV9PrefixSumsValidated`,
`TestContentPostingSingletonEntry`), nibble framing
(`TestContentTermDictNibbleFraming`), rank forms
(`TestDecodeRankSectionCompactAndLegacy`,
`TestDirectRankSectionsPersistOrderOnly`), SUBT/PATR forms
(`TestDecodeSubtreeSectionCompactAndLegacy`,
`TestAttrPostingSectionDeltaAndLegacy`); `TestContentIndexRoundTrip` now
expects the trimmed wire fields as zero and
`TestContentIndexRejectsPreviousVersion` guards v8.

## 6r. Build profile + throughput (done)

Where the v8 build time goes, and what got faster. All numbers are the
65k–77k-file Chromium sample on the same box (16-thread desktop shared with
other agents, so every figure is noisy: interleaved same-session runs,
min/median reported, GOMAXPROCS=4, 16 build workers unless noted). The
600 MB budget selects 76,637 files / 572 MB raw → 76,465 docs /
571 MB extracted → a 190 MB sidecar (text 103.9, grams 58.8, terms 19.6,
paths 6.1 MB); the 120 MB budget selects 15,577 files for quick iterations.
The filename-index-driven scope scan visits all 555,849 records regardless
of budget, so scan numbers are budget-independent.

**Method.** `zz_buildspeed_profile_local_test.go` (local-only, never
committed) runs the production shape — legacy select, prefetch extract +
spool, streamed assemble, streaming save, then an 8192-doc spread fold —
with per-phase wall timers (`contentAssemblePhaseTimes`, test-gated, nil in
production: tokenize/emit/train/spillSort/merge/write/encode/paths/fsync),
CPU + alloc + block + mutex profiles (`SEEKFS_PROFILE=1`), a warm
slurp-vs-extract micro split, and sha256 section fingerprints.
`zz_buildspeed_base_local_test.go` is the HEAD-compatible coarse harness
for the stashed baseline. Correctness guard: the optimized 600 MB build
carries the same 76,465 doc records and byte-identical section payloads as
`fixtures/v8-sample.gsx` (all tags compared, plus identical hit lists on 5
query shapes), and the 120 MB section hashes are stable across runs; the
cross-worker assemble/spill identity tests plus the new matchFile and pipe
tests stay green.

**Claim check: "extraction-dominated".** Half true. Interleaved 600 MB
medians (2 baseline + 2 optimized runs; extraction code paths unchanged, so
the extract column is contention noise, not a win):

| phase | baseline (eb2f8cb) | optimized | delta |
|---|---|---|---|
| scope scan (full index) | ~25 s, ~47 µs/record | ~7–14 s (7.9/10.7 at 600 MB) | 3.7x same-session |
| extract (76.6k files) | 76.0 s | 68.2 s (noise) | — |
| assemble (571 MB text) | 60.5 s | 37.2 s | **38%** |
| save (190 MB, incl. fsync ~0.5 s) | 0.9 s | 0.8 s | — |
| BUILD total | ~137 s | ~106 s | ~23% |
| 8192-doc spread fold | ~61 s | ~38 s | **38%** |

Assembly was the other half all along; the scan was the largest *fixed*
cost (paid in full on every build). The old "~9 min cold" is first-touch
opens (MFT + Defender, ~ms each) plus an uncontended-box assemble; on this
shared box cold-vs-warm cannot be separated honestly (no elevation to drop
caches, siblings churn them): first prefetch batches after churn show
multi-ms/file opens while the warm micro split shows slurp 97–160 µs/file
vs full extract 105–238 µs/file (open+read ≈ 85% of warm cost). Reads are
cheap; **opens are the extraction**: 59 s of CreateFile syscalls in one
120 MB profile, 74% of all sampled CPU under load.

**What the profiles proved.** CPU: go-git ignore matching was ~40–70% of
build CPU (`filepath.Match` per pattern per file per level); syscalls
(CreateFile) dominate under contention. Allocs: term/gram tokenizer maps
are ~41% of all allocated bytes (2.3 GB of 6.9 GB at 120 MB). Block/mutex:
no lock contention anywhere (71 ms total); the prefetch feeder blocks only
on its pacing bound (no pipeline pathology).

**What changed (all output byte-identical; query paths untouched):**

- **Scope scan 25.2 s → 6.9 s (3.7x, same session, set-identical).**
  `matchFile`: file-only dispatch over floating patterns (exact-name map,
  prefix/suffix affixes, rest) merged by descending line; negations and
  dirOnly keep the full Match because an intermediate-component match is
  load-bearing for precedence (skipping them dropped 18 `.gemini/` files;
  pinned by `TestContentIgnoreMatchFileMatchesMatch`). One ignore parse
  cache shared by a scan's workers (43,638 dirs, 5.2 M hits — was: every
  worker re-read every touched file). The `segHit` memo replaces the
  per-file full-path system-segment recheck (same predicate: the basename
  is spared). Binary-extension reject precomputed once. Parent-path memo
  (one split per file, no per-file chain walk) measured ~3%, inside noise —
  kept for the ~400 MB of garbage it removes per full scan.
- **Assemble 60.5 s → 37.2 s at 600 MB (38%; fold likewise).** An ordered
  text pipe (`content_textpipe.go`) moves zstd encoding to its own thread
  while emit keeps doing posting Adds (emit 5.6–6.3 s → 4.0–4.3 s at
  120 MB; 120 MB BUILD 8.8–9.5 s → 7.5–8.1 s, ~15%). Spill runs use fixed
  8-byte records (no uvarint per posting), 8 KiB run reads / 64 KiB run
  writes, and an allocation-free merge reader (spill ~5% here; the varint
  removal materializes on quiet IO).
- 600 MB sub-phases (optimized medians): tokenize 8.4 s, zstd encode
  ~16.4 s (now overlapped with Adds), spill/sort 14.5 s (87 spills),
  merge 7.9 s, dict train ~0.06 s, paths ~0.03 s, save 0.8 s.

**Fold + shards.** Single-file 8192-doc spread fold re-assembles
everything: 61 s → 38 s. Sharded (120 MB corpus, 16 MB target, 9 shards,
+22% size tax at this small target vs ~6% at the 64 MB production target):
serial assemble 10.3 s (slowest shard 1.4 s, so concurrency-2 lands
~5–6 s); spread 8192 (5/9 dirty) 4.7 s, docID-clustered 8192 (5/9) 4.4 s,
one-folder-100 (1/9) 1.0 s — 8x over the single-file fold for localized
edits. Shard folds inherit the pipe/run-format wins (same assembler).

**Tried and rejected (measured, not assumed).** Sort+dedupe tokenize
(slices over hash maps, no per-occurrence strings, no fold copy): tokenize
1.2 s → 2.7 s at 120 MB (2.2x *slower*, BUILD +19%) — Go maps win on CPU
here; reverted. Bigger run buffers died in `TestContentExternalBoundedBuffer`
(8 KiB reads kept). Zero-size-file skip at selection: 103 of 76,637 files
(0.13%) — not worth the diff. `mmap` for large files: moot on this corpus
(`truncated=0`, no over-cap files). Bare-`*`/glob affixes and dirOnly
skips in the dispatch: unsound for precedence (the 18-file divergence).
More tokenize/encode level tweaks: the BetterCompression+dict point is
already optimal (§6o); only threads were left, and the pipe took them.

**Remaining bottlenecks.** (1) Extraction opens: irreducible per-file OS
cost (~100 µs warm, ms cold); the service build additionally sleeps
`contentBuildBatchPause` (25 ms per 64 docs ≈ 30 s on 76k files —
unmeasured here since harnesses bypass the governor; making it idle-aware
is open). (2) zstd encode (~16 s at 600 MB, now overlapped not reduced —
a second encode worker would need ordered frame handoff). (3) Merge run
reads under contention. (4) Scan residual: per-level ignore match after
the dispatch (~7 s). Prefetch/assemble/show/save/fsync are all minor.

## 6s. Content query profile (done)

Branch `perf/content-query`. A repeatable latency profile over the fixed
query set users hit, then read-path-only optimizations against what it
found. Size is byte-identical throughout (no encoding change); recall,
result sets, order, snippets and counts are proven identical by golden
fingerprints plus lane/serial/parallel/cold parities (see Correctness).

**Corpus.** The shared read-only fixtures: `E:\bench\fixtures\v8-sample.gsx`
(76,465 docs, 189.9 MB, v8, single-file) over `E:\bench\chromium.gsi`
(555,849 records, walk source). The build scope excludes `third_party`
(0 of 76,465 docs live there — under-shapes use `base`/4 instead), and
the sidecar keys docs by the `.gsi`'s positional FRNs rather than path
hashes, so the harness joins on FRN equality (in-memory origin
reinterpretation only; the file is untouched) and logs the join health
(76465/76465). If the fixture is rebuilt walk-keyed, the same harness
takes the walk join with no code change.

**Harness.** `cmd/seekfs/zz_content_queryprofile_local_test.go` is LOCAL
ONLY (never committed): service volumes over the real `.gsi`+`.gsx`,
fixed shapes (rare/common literals, quoted phrase, `ext:`/`under:`
compounds with the lane on and off, limit 20 vs the service default page
100 — a service `limit=0` normalizes to 100, so true unlimited is the
offline reader's limit 0 plus exact counts — regex full scan, typo/fuzzy
through the production underfill sequence, 1–2 char terms,
`sort:relevance`), min-of-N (N=7, GOMAXPROCS=2, `go test -p 1`), warm
(hot block cache) and cold (fresh reader = empty block cache; the OS
page cache stays warm, so cold isolates block-cache + decompress cost),
per-shape block-cache touches (new lifetime counters on
contentBlockCache: hits/misses/decoded bytes), a per-stage breakdown
(parse, full-budget candidate gen, decompress/fold/match over hits,
snippet attach, rank), CPU + alloc profiles for the slowest shapes, and
result fingerprints (ordered paths, per-path snippets, counts,
planner/source/completeness/fuzzy flags) with golden compare
(`SEEKFS_PROFILE_GOLDEN`). Timings are min-of-N on a shared box: always
re-measure the baseline in the same session as a change.

**Latency table** (min-of-7, GOMAXPROCS=2; before = §6o code at 128 MB
cache cap, after = this branch at the default cap, which the §6s cache
work moved to ~254 MB on a 16 GiB box; counts are single-shot exact
totals):

| query | before warm | after warm | before cold | after cold |
|---|---|---|---|---|
| `content:cityhash_v103` (10) | 2.5 ms | 2.1 ms | 2.7 ms | 2.7 ms |
| `content:struct` (20-page) | 8–12 ms | 7.1 ms | 12.6 ms | 10.0 ms |
| `content:struct` count (15,116) | 7.1 s | 2.3 s | — | — |
| `content:EINVAL` (20-page) | 297–371 ms | 141 ms | 477–656 ms | 248 ms |
| `content:EINVAL` count (422) | 5.6 s | 2.0 s | — | — |
| `content:"reassemble"` (5) | 73–113 ms | 68 ms | 156–204 ms | 110 ms |
| `ext:cc content:mutex` (20-page) | 110–153 ms | 115 ms | 303–401 ms | 338 ms |
| `ext:cc content:mutex` (48, page 100) | 167–278 ms | 165 ms | 442–625 ms | 398 ms |
| `ext:cc content:mutex` under `base` | — | 13 ms | — | 84 ms |
| `content:mutex` under `base` (51) | — | 13–25 ms | — | 82 ms |
| `content:/mutex_\w+/` (14) | 21–32 s | 0.40 s | 22.5–32 s | 0.59 s |
| typo `mutxe`→`mute` (20) | 1017 ms | 185 ms | — | — |
| typo `recieve` (19 literal) | 161 ms | 126 ms | — | — |
| typo `strucure`→`structure` (20) | 1861 ms | 227 ms | — | — |
| `content:a` / `content:en` (20-page) | 1.5–3 ms | 1–3.5 ms | 2–11 ms | 2–4 ms |
| `content:mutex sort:relevance` | 684–761 ms | 119 ms | 711–1066 ms | 389 ms |
| GOMAXPROCS=8 labeled: einval-20 335→126 ms, compound-100 191→49 ms, relevance 806→197 ms | | | | |

(Before/after per change were also measured head-to-head in-session;
see the commit messages. The compound lane engages only for small
filename sides — `ext:cc` (26,622 docs) stays on the per-volume path
while `under:agents` (490 docs) runs the lane at ~11–70 ms with
identical results to the slow path.)

**Stage breakdown** (representative, ms/op on the hot cache): parse ~0;
full-budget candidate gen 1–6 (trigram lookup + intersection + record
mapping; 0 for the regex, which had no candidates before); decompress
over hits ~0.1; fold+match over hits 0.1–5.3; snippet attach ≤0.6
(20 results); rank ≤0.5 (20 results; the window rank runs pre-trim and
costs more — see relevance). End-to-end is dominated by verification:
block touches × decode + folds, e.g. relevance 454 hits + 1320 misses
(135 MB decoded) per op at 128 MB, 1774/0 at 256 MB.

**What changed (all read-path; size byte-identical):**

- **Allocation-free ASCII verification** (`content_verify.go`,
  `content_read.go`): `asciiCaseIndex` decides ASCII needle-in-ASCII-text
  with no allocation at identical offsets; anything else takes the
  historical fold path. Match, snippet and offline verify share it.
  Differentials over ASCII/non-ASCII corpora (incl. fold-expanding U+0130,
  which correctly stays on the fold path — it caught a real offset bug
  before it shipped). struct-20 13.7→7.1, compound-20 125.7→77.9,
  relevance 820→521 ms.
- **Trigram prefilter for regex leaves** (`content_verify.go`,
  `search_compact.go`): pure-regex queries intersect the required
  literals' posting streams (`regexRequiredLiteralAlternatives`, also
  inside OR alternatives) instead of full-scanning; leaves without
  provable literals still scan, mixed term+regex keeps term-driven
  behavior, and prefiltered candidates keep scan order
  (`sortCandidateIDsByScanOrder`) so results are byte-identical on
  volumes where rank and scan orders differ (the fixture does).
  Regex-20 19.5 s→387 ms warm (50×), 21.5 s→403 ms cold. pprof showed
  ~85% of the scan in Go's backtracking engine (only ~6% in zstd), so
  fewer documents (not faster matching) is the lever.
- **Walk scan/count recall fix** (`search_compact.go`): the path-free
  scan entry and the entry-free count never resolve walk content (keyed
  on the entry path), so broad/capped content queries and all counts on
  walk volumes returned 0. Walk-attached volumes keep full entries;
  USN unchanged. Found by the profile (0-result under-shapes) and
  proven by a negative control (0/0 without, 20/4200 with).
- **Parallel scan/verify/count** (`candidate_scans.go`,
  `search_compact.go`): ordered-merge parallel for the bounded scan
  early-stop and the candidate verify (exact order + limit, bounded
  over-verify), sharded tallies for counts; serial preserved for small
  sets and for 2-worker early-stop pages. GOMAXPROCS=2: einval-20
  356→172, relevance 650→405, counts 1.3–2×; GOMAXPROCS=8:
  einval-20 335→126, compound-100 191→49, relevance 806→197.
- **Cache policy + RAM probe fix** (`content_textblock.go`,
  `pngc_augment.go`): `totalRamBytes` built a 72-byte MEMORYSTATUSEX
  (the real struct is 64), so the probe always failed and every default
  cap was the 64 MB fallback; pinned with a sizeof assertion. The
  TotalRAM/64 ceiling is raised 128→256 MiB: a relevance window touches
  ~135 MB, which thrashed perpetually at 128 (75% misses) and fits at
  256 (1774/0, relevance 405→125 ms warm). Small machines keep
  TotalRAM/64 down to the 32 MiB floor.
- **Block-cache lifetime counters** for the touches×µs attribution.

**Tried and rejected:** s2/LZ4-class codecs stay rejected (decode
tradeoff for ratio, §6n); verifying in docID/block order for early-stop
pages (breaks minimal-verify — rank order must drive); parallelizing the
2-worker early-stop candidate verify (sync overhead cancels it; serial
preserved); lowering the relevance window (changes ranking/recall);
capping fuzzy trials (changes results); a bigger default cache than 256
(small-machine memory); per-doc ASCII pre-transform of the corpus
(format change — not this branch).

**Remaining hotspots:** cold compound pages (~340 ms, compulsory
dict-decodes ~170 ms + folds on a loaded 2-worker box; ~50–70 ms warm at
8 workers); 0-result compound queries pay ~1.2 s of underfill fuzzy
trials (each trial is a full re-search — inherits the wins above);
cold relevance (~390 ms, same physics); short-term pages are complete
only to the window by design (conservative incomplete flag).

**Format-level suggestions (for the format/size owner; not implemented
here):** per-block query-frequency stats to drive a hotter ordering
(rank-ordered verify fights LRU by construction); content-aware block
cuts (doc-boundary cuts already exist — co-locating same-extension docs
would tighten compound working sets); skipping decompression is
unsound from postings alone (trigram presence ≠ substring), so cold
verify is bounded below by compulsory decodes — the lever is decode
parallelism (shipped) and codec choice (rejected on ratio).

**Correctness:** `go vet ./...` clean; full `go test -p 1 ./...` passes
except `TestHeadlessFrameCost` (`cmd/seekfs-native`, a wall-clock GPU
assertion failing under shared-box load in a package that does not
import the changed code). Golden fingerprints identical for the whole
query set before/after (only the regex source label changed
scan→candidates, by design); lane on/off, serial/parallel,
GOMAXPROCS 1/2/4/8, warm/cold and count==search parities all green.

## 6t. Reserve budgets the build peak (done)

The first auto-scope build of the real E: index (1.46M records, ~29 GB free
against the 20 GiB reserve) extracted 656k files and then failed while
assembling: `content index stopped: only 20.0 GB free on E: (keeping 20.0 GB
free)`. Selection had budgeted only the finished index (extracted × 0.5), but
at the build's peak the volume also holds the whole text spool (1×) and,
until the build ended, every assembled shard's private scratch (its text,
term and gram section files, which the save copies into the shard). Peak use
was therefore ~1.7× extracted, not 0.5×. The mid-build allowance refresh also
read current free space, which already had the spool subtracted, so it
charged spooled bytes twice and stopped at an inconsistent point.

Fix: the allowance is now `(free + spooled − reserve − shard scratch) /
contentScopePeakMultiplier` (1.5 = spool + output), where shard scratch is
`contentShardBuildConcurrency × contentShardTargetBytes`; `--estimate` uses
the same rule with nothing spooled; and each shard's scratch is removed as
soon as it is saved, so scratch never exceeds the in-flight shards. The
write-time guard and its monotonic reserve are unchanged. Covered by
`TestContentScopeReserve` (peak allowance, spool credit, fits/allowance
agreement).

## 6u. Reconcile honors the budget (done)

Right after the §6t fix let the real E: index publish a reserve-truncated
base (41,724 docs; 877,932 files / 11.9 GB dropped), the build's own drops
came back as drift. The journal had wrapped during the build, and the
metadata reconcile enqueued 925,450 files (every in-scope file without a
doc). An ignore-file change then sent the scope reconcile past the rebuild
fraction, which rebuilt and truncated the same way: a loop on any busy
volume.

Both reconciles now hold keys new to the base to the budget the build used
(`content_reconcile_budget.go`): the explicit `budget_bytes` minus the text
the base holds, or the reserve allowance on the sidecar volume. They are
admitted in the build's priority order (`contentBuildItemLess`) while their
file sizes fit, with the same latch as the build (once one misses, the rest
stay out). Changed docs already in the base always re-extract. The scope
rebuild fraction weighs only the trimmed diff. Covered by
`TestContentReconcileTrimNew`,
`TestContentScopeReconcileHonorsReserveForNewFiles` and
`TestContentWrapReconcileHonorsReserveForNewFiles`; both reconcile tests
fail without the gate (stale + rebuild; the new file extracted past the
reserve).

Both reconcile scans also hold background persists for their duration
(`holdBackgroundPersists`, the build snapshot's hold from §6p). A persist
swaps `vol.index`, which reads as a moved base; on the busy E: volume
persists ran every 15-30 s, faster than a full selection scan, so every
metadata reconcile gave up after three attempts and silently scheduled a
rebuild. `contentCatchUpFailed` now logs its reason. Covered by
`TestContentReconcileScansHoldBackgroundPersists` (fails without the hold).

## 6v. Content underfill + cold pages (done)

Branch `perf/content-underfill`, from `integ/content-main` at 47f3911.
It takes the two hotspots §6s left open — 0-result content compounds paying
~1.2 s of underfill fuzzy trials, and decode-bound cold first pages — with
read-path-only changes: result sets, order, counts and snippets are proven
identical by golden fingerprints plus lane/serial/parallel parities.

**Fresh baseline.** §6s numbers are stale: the base now ships format v9
(smaller grams, pruned dense grams, §6q) and the build-speed work (§6r), and
the previous fixture (`fixtures/v8-sample.gsx`) fails the v9 version gate.
The profile below re-runs the full §6s harness unchanged (it needed no v9
fixes) against `E:\bench\scratch\underfill\v9-sample.gsx` — a copy of the
ready v9 build `E:\bench\scratch\review\merged-v9.gsx` (76,465 docs, same
input; join health 76465/76465). Min-of-7, GOMAXPROCS=2,
`go test -p 1`; before/after claims are same-session A/B pairs (this box
carries heavy same-file contention noise — cross-session absolutes move
±2x, so only in-session deltas and the golden identity count).

| query | fresh baseline warm | fresh baseline cold |
|---|---|---|
| `content:cityhash_v103` (10) | 2.1 ms | 5.7 ms |
| `content:struct` (20-page) | 7.9 ms | 13.1 ms |
| `content:EINVAL` (20-page) | 144.6 ms | 461 ms |
| `content:"reassemble"` (5) | 82 ms | 238 ms |
| `ext:cc content:mutex` (20-page) | 116.8 / 139.1 ms (lanes 1/0) | 541.6 ms |
| `ext:cc content:mutex` (48, page 100) | 177.8 / 187.6 ms | 678.6 ms |
| `ext:cc content:mutex` under `base` | 13.5 / 14.0 ms | 133.4 ms |
| `content:mutex` under `base` (51) | 14.3 / 15.2 ms | 132.3 ms |
| `content:/mutex_\w+/` (14) | 836 ms | 933.6 ms |
| typo `mutxe`→`mute` (20) | 276.9 ms (fuzzy 307 ms) | — |
| typo `recieve` (19 literal, no fuzzy) | 192.2 ms | — |
| typo `strucure`→`structure` (20) | 952.6 ms (fuzzy 1052 ms, 218 MB decoded) | — |
| 0-result compound under `agents`, lane off | 1648.6 ms (fuzzy 1787 ms) | — |
| 0-result compound under `agents`, lane on | 79.3 ms (fuzzy ~105 ms) | — |
| `content:mutex sort:relevance` | 214.4 ms | 402.6 ms |

Shape-for-shape this matches §6s within noise (einval-20 141→145,
compound-20 115→117, struct-20 7.1→7.9); the regex shape reads slower
(400→836 ms) purely from parallel-suite contention in the baseline session.
Counts are unchanged (struct 15,116, einval 422, mutex-under-base 51).

**What changed (all read-path; size byte-identical):**

- **Fuzzy trials take the posting-candidate path** (`postings.go`,
  `content_fuzzy_trial_path_test.go`). `parseQuery`'s `parsedOverride`
  branch returned the cloned retry tree without the request's per-search
  scalars. A trial search threads per-volume fields
  (`contentWindow`/`contentProbe`, budgets, `Trace`, deadline) through
  `queryOptions` *after* the tree was built, so every trial saw
  `contentWindow=0`: the bounded candidate path capped posting
  materialization at the page limit, declared every broad superset capped,
  and fell back to a full-corpus rank-ordered bounded scan — identical
  results at ~50x the cost. The branch now overlays those scalars onto the
  clone (leaf fields stay with the tree). Same-session min-of-7:
  0-result compound lane-off 1648.6→99.7 ms (losing trials 850→8 ms each:
  candidates + scalar filter prove empty with zero block decodes),
  `strucure` 1106.6→104 ms (218 MB decoded → ~0), `mutxe` 258.8→93.1 ms.
  Full-table after: compound-under-small lane-off 18.8 ms,
  `mutxe` 14.5 ms, `strucure` 24.7 ms, all golden-identical.
- **Halved + memoized the correction dict scan** (`content_fuzzy.go`,
  `content_termsdict.go`, `content_fuzzy_scancache_test.go`). The scan is
  inherently O(dict) — top-2-by-(distance, df) needs the full evaluation —
  so the levers are per-key cost and repeats. First, the sound gates were
  reordered: the letter bitmap runs before the pigeonhole piece-substring
  tests (the scan's hottest cost, ~57% of samples), and ASCII keys get
  their exact length free from the mask pass, rejecting out-of-range
  lengths before any substring search (trials-build 76–137→47–53 ms
  single-shot across six shapes). Second, per-dictionary scans memoize by
  (dict generation, word, tau): dicts are immutable after decode and the
  generation is a global atomic issued once per decode, so a hit is
  exact-equivalent to a rescan even across address reuse; the mutable delta
  vocabulary still merges fresh, cached maps are never mutated after
  insert, the LRU holds 64 entries × 2048 terms, and empty scans memoize
  too. Repeat typo builds measure 0.0 ms (cache hit).
- **Parallelize verify-heavy early-stop pages at 2 workers**
  (`search_compact.go`). The gate kept the serial walk for every early-stop
  page at GOMAXPROCS=2 on the theory that merge/sync overhead cancels the
  win. Same-session A/B shows the overhead only matters with no verify work
  to spread: verify-heavy pages win ~2x warm *and* cold (compound-20 warm
  99/95→53/53 ms, cold 346→177 ms; under-shapes to ~9–10 ms warm,
  ~74–87 ms cold), while collect-only pages (pre-verified bounded scan,
  `skipEntryMatches`) are even (einval-20 144→147 ms warm, 461→405 ms
  cold). The gate now parallelizes at 2 workers unless the page is
  collect-only; 3+ workers and limit-covers-candidates are untouched.
  GOMAXPROCS=8 labeled: einval-20 60 ms, compound-20 23–30 ms (cold
  65 ms), relevance 58 ms (cold 164 ms), `strucure` 18 ms.

After table (min-of-7, GOMAXPROCS=2, quiet-box session; golden-identical
to the baseline on every shape, lane, and warm/cold arm): rare 1.6/3.2,
struct-20 9.1/12.2, einval-20 146.6/405.1, phrase 67.7/162.3, compound-20
52.8–58.4/174.6, compound-100 82.5–92.8/278.7, compound-under 9.1–9.2/73.9,
content-under 9.7/86.6, compound-under-small lane-off 18.8 (lane-on 0.5),
`mutxe` 14.5, `recieve` 85.1 (no fuzzy either way),
`strucure` 24.7, relevance 145.8/452.5, regex 609.1/686.5.

**Tried and rejected (measured, not assumed):**

- Skipping provably-empty trials via `contentCandidatesBounded`: corrections
  come from dict terms with df > 0, so trial candidate sets are essentially
  never empty — the gate would almost never fire. The windowed candidates
  path *is* the cheap emptiness proof (8 ms, zero decodes for the losing
  compound trials). Not implemented.
- Reordering trials cheapest-first / capping below 4: changes which trial
  wins when several match — changes results. Not implemented (§6s already
  rejected capping for the same reason).
- Block prefetch for cold pages: with 2 workers already decoding distinct
  blocks concurrently, a prefetcher adds a third concurrent decoder on a
  2-CPU box — contention, no headroom. The remaining cold cost is true
  decode work. Not implemented.
- Decode-order/block-locality changes and smaller relevance windows: change
  minimal-verify order / ranking — rejected in §6s, still rejected.
- Codec change (s2/LZ4-class): still rejected on ratio (§6n/§6o/§6s).

**Remaining hotspots:** cold relevance (~450 ms) and cold einval (~405 ms)
are compulsory window/scan decodes; the first-touch correction scan
(~50 ms) is irreducible without an auxiliary index (format change); repeat
typos are now free via the memo. Short-term pages stay complete-only-to-
the-window by design.

**Correctness:** `go vet ./...` clean; `go test -p 1 -timeout 40m
./cmd/seekfs ./feature` passes except `TestHeadlessFrameCost`
(`cmd/seekfs-native`, the known load-sensitive wall-clock failure in a
package that does not import the changed code). New tests:
`TestContentFuzzyTrialParityWithNormalParse`,
`TestContentFuzzyWinningTrialTakesCandidatePath` (decoy fixture forces
candidates ⊃ matches with an unfilled page so no widen masks the first
search's path), `TestParseQueryOverrideCarriesSearchScalars`,
`TestContentFuzzyScanCacheParity` (raw vs cold vs warm, hit counting,
second-dict generation) — the first three fail without the overlay fix.
Serial/parallel identity for the newly parallelized gate is covered by the
existing `TestContentParallelContentMatchesSerial` (limits × workers).

## 6y. No stored text by default: postings + per-file filter (done)

Owner decision (2026-10-13): seekfs adopts fsearch's model as the DEFAULT —
no stored file text. The index is trigram postings (CXGR, dense grams pruned
into CXGP as before) plus a per-file bloom filter (new CXFL section) over the
same folded trigrams, and every match and snippet is verified by reading the
file from disk. The filter tier built as an overflow sidecar (§6y draft: a
`<gsx>.filter` file covering only budget-dropped files) is superseded by this
section: the filter is primary, covering (nearly) all files, and there is no
tier split anymore.

**Format v10** (v9 and older sidecars fail the version gate and rebuild
through the PF-3 path; manifests stay v5). A v10 `.gsx` carries CXGR/CXGP
(postings, unchanged codecs), CXTR (term dictionary, unchanged — fuzzy still
needs it), CXPT (paths, unchanged), CXDT (doc table, 60 → 52 bytes: TextOff
gone with the store; TextLen stays for budget accounting, FileSize/FileModNano
stay for the walk reconcile), CXPL/CXSC (policy/scope, unchanged), and the new
CXFL: count + build caps + walk root + encoding label, then one
(class u8, class bits) row per document in DocID order. Rows carry no keys,
paths, or fingerprints — they join by DocID to the doc table (keys), CXPT
(paths) and the doc fingerprint. CXST/CXTD sections are gone (their tag
constants stay defined, retired); the textblock/textdict/textpipe sources and
their tests are deleted. The dedupe agent's alias section (`content/inclusion-
policy`) is an additive tag beside CXFL by design, so that merge is a
renumber, not a redesign.

**Filter design.** One k=1 bloom signature per document over its folded
trigrams (k=1 measured best at every size: k=2 saturates small filters and
loses on every query shape). Classes are 1024<<c bits (128 B–2 KiB),
sized by the MEASURED distinct trigram count (smallest class with bits ≥ 2n,
n/m ≤ 0.5); past class 4 (n > 8192) the filter saturates gracefully (higher
FP, never a false negative). Sizing by n — not by text length — was forced by
measurement: text-length classes saturate dense files (minified assets, test
data: distinct ≈ windows) into uselessness. The builder computes the gram set
once and reuses it for postings and bits (no extra pass). The hash
(SplitMix64, masked — power-of-two sizes, no modulo) is frozen by
`TestContentFilterHashIsFrozen`; uniformity on 200k sequential trigrams at
m=2048 is mean 97.7 / max 135 (ideal 97.7), so the residual FP below is
trigram-inherent, not hash skew.

**Query path.** Candidates come from the posting intersection exactly as
before (pruned grams skipped as constraints). Every content verify then
passes one uniform gate — the per-file bits must contain every positive
leaf's grams (pure-regex leaves: some disjunct's runs; OR groups: some
alternative; NOT leaves: no constraint; leaf grams precomputed once in the
matcher, so the per-record cost is bit tests, ~130 ns/file) — and on a pass
reads the file from disk (open → extract with the build policy → match with
the exact historical predicate, ASCII fast path included). The gate is
skipped for delta-covered keys (delta text is fresher than the bits) and
never rejects a key outside the base docs. Snippets render from the verified
bytes (original case preserved); `sort:relevance` and fuzzy re-verify through
the same path. The compound lane, the bounded scans, and the parallel
verifiers (which now parallelize disk reads — the owner's parallel-reads
requirement falls out of the existing fan-out) all funnel through the one
choke point (`entryMatchesWithContentMatcher`), so every path shares the
gate and the disk policy with no per-path forks.

**Freshness (verify-direct).** A size/mtime mismatch against the indexed
fingerprint means stale postings/filter — but the bytes on disk still
decide: a file edited since the last sync is read directly and matches on
its current bytes (fsearch's ~1 ms greppable edits; proven by
`TestContentVerifyDirectServesEditedFile`). The drain still re-extracts
through the USN/walk pipeline into the delta (which wins over disk while
present), so the prefilter converges. Terms whose grams the edit introduces
are a documented re-index window (postings cannot nominate unknown grams);
the drain closes it within seconds on a live volume. A missing file reads as
absent (fail-closed — a deleted file is unfindable even before its tombstone
lands, instead of serving stale bytes). A doc-table path that went stale
across a rename falls back to the live record-index path for missing files
only (extraction failures never fall back, so a corrupt file cannot serve
another path's bytes). A transient Windows sharing violation retries once
(~20 ms); anything else fails closed.

**Boundedness (the §6m regression must not come back).** Dropping stored
text recovers the §6m failure mode (cold broad queries of 12–109 s) unless
three things hold, and all three ship: (1) candidate narrowing before any
read — postings ∩ per-file filter, so a query touches only files that can
match (a 4-gram common term prefilters 183k files to ~68k bit tests ≈ 9 ms
before the first open); (2) early stop at the result limit in scan order —
selective pages stop after tens of verifies; (3) a per-query disk
verification budget (`contentDiskVerifyBudget` = 512, a var; delta hits never
count) with honest `complete:false` — past it the search stops with a
visible subset and the count is refused with `errContentIncomplete`, never a
silent truncation or a partial number. The trace carries `DiskVerified` /
`DiskCapped` alongside the candidate-budget signals.

**Measured (E:\bench\chromium minus third_party, service-allowlist
selection: 184,321 files → 183,804 docs, 1416.5 MB extracted; GOMAXPROCS=2,
`go test -p 1`; first = warm-OS-cache first touch incl. Defender, best =
min-of-5 hot; a true cold page-cache drop needs elevation and is not
measured — cold numbers are analytic: verifies × first-touch opens):**

| section | MB | share |
|---|---|---|
| filter (CXFL) | 64.0 | 0.045x |
| terms (CXTR) | 42.9 | 0.030x |
| grams (CXGR) | 97.1 | 0.069x |
| paths (CXPT) | 3.9 | 0.003x |
| doctable (52 B × docs) | 9.1 | 0.006x |
| **file total / extracted** | **217.1 MB** | **0.153x** |

(v9 on its 65k-file sample: 0.332x single-file, 0.381x sharded; the §6m
no-text postings-only point was 0.147x — v10 adds the filter for +0.006x
net. Filter alone is 365 B/file mean; whole-file mean is 1238 B/file
(postings + terms amortized). Classes: 128 B: 59,757 files; 256 B: 60,147;
512 B: 46,019; 1 KiB: 15,871; 2 KiB: 2,010.)

| query (limit 20) | grams | hits | passes | bloom ε | first | best | complete |
|---|---|---|---|---|---|---|---|
| `content:cityhash_v103` (rare) | 11 | 11 | 54 | 43 (FPR 0.0002) | 57 ms | 1.1 ms | true |
| `content:struct` (common) | 4 | 20 | 67,985 | ~29k | 313 ms | 3.5 ms | true (early stop) |
| `content:"reassemble"` (phrase) | 8 | 20 | 1,292 | ~842 (FPR 0.0046) | 3.09 s / 448 ms | 462 / 418 ms | true |
| `content:mutex` (common) | 3 | 20 | 12,982 | ~9k (FPR 0.049) | 3.48 s / 132 ms | 111 / 115 ms | true |
| `content:paintlayer` | 8 | 20 | 1,730 | ~986 (FPR 0.0054) | 850 / 93 ms | 84 / 86 ms | true |
| `content:the` (degenerate) | 1 | 20 | 163,885 | — (cf. v9 all-pruned scan) | 74 / 2.2 ms | 1.7 / 1.6 ms | true |
| `content:"foo bar"` (phrase) | 5 | 20 | 1,780 | ~1,116 (FPR 0.0061) | 584 / 71 ms | 77 / 59 ms | true |
| `content:ab` (<3 B, no constraint) | 0 | 20 | 183,804 | — (by design) | 25 / 5 ms | 3.7 ms | true |

(First = first touch after the build warmed the OS cache (two sessions:
cold-ish 3.09 s vs warm 448 ms for the scattered phrase show the Defender +
cold-read spread); best = min-of-5 hot. "passes" counts bit-test passes;
ε splits bloom collisions from trigram-inherent positional loss measured by
recomputing true gram sets on a bounded sample: struct's 68k passes are
~40k positional + ~29k bloom — the positional share is what the full tier's
own candidates would verify, not a tier-2 regression. For rare literals
(11-gram cityhash) positional ≈ 0 and ε = 43 files.)

Counts: cityhash 11/11 complete; struct and mutex refuse past the 512 cap
(`complete:false` + `errContentIncomplete`) — broad counts over disk stay
refused rather than burning minutes, exactly the §6m lesson. Regex
(production prefilter approximated offline):
`mutex_\w+` 8 hits / 512 verifies / 885 ms incomplete;
`cityhash_\w+` 2 / 512 / 3.77 s incomplete (cold files);
`zzz\d+qqq` zero-result 0 / 512 / 1.27 s incomplete (bounded, vs v9's ~15 s
full scan). Compound `ext:cc content:mutex` (offline approx): 13 hits /
512 verifies / 117 ms incomplete.

v9 baselines for the same shapes (§6s warm: cityhash-20 2.1 ms, struct-20
7.9 ms, phrase 68–82 ms, compound-20 53–117 ms, compound-100 82–93 ms,
regex 609–836 ms, relevance ~146 ms; cold: struct ~13 ms, compound ~175 ms,
regex ~687 ms): v10 warm is within ~1–3x on postings-narrowed shapes
(struct-20 3.5 ms actually beats v9's 7.9 ms — early stop needs fewer
verifies than v9's block decompresses), and 2–5x slower cold on scattered
shapes (reassemble cold 3.09 s vs ~162 ms — hundreds of cold opens where v9
paid block decodes; the cap bounds the worst case at 512 files ≈ ≤4 s cold
instead of §6m's 12–109 s). Regex and unfilled compounds regress most
(disk per candidate vs decompress); selective literals, phrases that fill
the page, and common terms are at parity or better.

Recall: `cityhash_v103` answers 11/11 with 0 served false positives, and
every gap vs `rg -l` is a documented selection rule (non-allowlisted
extensions `.gn`/extensionless READMEs/`.sky`, top-level `third_party`
scope) — recall inside indexed files is 100% vs ripgrep, as before. The
committed differential (`TestContentFilterV10ServiceDifferential`, 17 query
shapes incl. AND/OR/NOT/regex/case/compounds, search==count) plus the gate
soundness proof (`TestContentFilterGateSoundness`: gate-reject ⟹
predicate-false over the shape matrix) carry the exactness argument; lane
on/off parity holds on real files.

**Budget/reserve.** The v10 output is ~0.15x, so the budget governs a much
smaller index — but the build PEAK is still the text spool (1.0x) plus
output: coverage per GB of headroom is ~88k files at the current 1.5x peak
multiplier (≈104k at the measured-correct 1.25x — left for the spool-peak
owner, who is reworking `content_space.go`; the estimate CLI stays
conservative until then). E: real (909k in-scope files, 14.5 GB text):
steady-state v10 index ≈ 2.2 GB (fits easily), but the spool-bound peak is
≈18–21 GB against ~8 GB of headroom past the 20 GiB reserve — so even v10
reserve-truncates without per-shard spooling. That is now the binding
whole-drive constraint, not the index format; recommended follow-ups are a
per-shard spool (extract→assemble→save→delete per shard) and/or an explicit
`budget_bytes` for the volume. `scope_dropped` keeps its meaning (files with
no coverage at all) and health is unchanged in shape.

**Folds.** A fold re-extracts every base doc the delta does not cover
(parallel prefetch, ordered merge with the delta; renames resolve live on
stored-path failure; unreadable docs skip with a bounded log) and
re-assembles postings + filter from scratch — measured ≈ full re-extract +
assemble on the sample (~5 min vs v9's ~38 s single-file fold that
re-encoded resident text). Shard isolation is unchanged (only dirty shards
re-read). The designed-but-unbuilt optimization is a postings row-merge:
carry unchanged docs' rows and filter bits verbatim, merge only delta
postings — it needs exact per-doc gram lists for removals, which v10 does
not store, so it stays a design note until folds prove too slow in
production. Fold cadence is the lever in the meantime (raise
`contentDeltaFoldMaxDocs/Bytes`).

**Freshness.** Verify-direct as above; the drain→delta→fold pipeline is
unchanged (delta wins over disk while present; reconciles and the §6u
budget gate are untouched — with no filter tier there is nothing to exempt).

**Tried and rejected (measured, not assumed):** a `store_text` opt-in —
removed, not shipped. It would double the format/test matrix (CXST
encode/decode, dual fold paths, dict) to reintroduce the worst case for
shapes the measurements show are already at parity warm and bounded cold;
snippets and relevance render from verified bytes and need no store.
s2/LZ4-class codecs stay rejected on ratio (§6n); k=2 stays rejected
(saturates small filters); text-length filter classes stay rejected
(saturate dense files); the `<gsx>.filter` sidecar tier is superseded by
the CXFL section (one file, atomic publish, shard-local, no extra lock or
sweep); per-file exact gram lists (row-merge enabler) stay rejected at
~3 B/gram ≈ text size.

**Open risks.** (1) Fold re-read cost (above) — watch production fold
times; row-merge is the escape hatch. (2) E: whole-drive coverage is
spool-bound (above) — needs the spool-peak work, not more format. (3) Cold
scattered queries pay seconds (Defender first-touch × verifies); the cap
bounds it, prefetch parallelism does not hide first-touch latency at
GOMAXPROCS=2. (4) Missing-file fail-closed: a transiently locked file
retries once on sharing violation, then misses until its next change (v9
served stale text — a deliberate freshness-over-recall trade). (5) FRN
reuse across journal generations is guarded as before (journal-ID checks);
a reused FRN joining the wrong doc would verify wrong — same exposure as
v9's resolver. (6) Broad counts refuse past the disk cap (by design).
(7) Relevance windows re-read up to thousands of files (not separately
measured — inherits the per-verify cost).

**Correctness:** `go vet ./...` clean; `go test -p 1 -timeout 40m
./cmd/seekfs ./feature` green (the known `TestHeadlessFrameCost`
wall-clock exclusion aside, which lives in a package that does not import
the changed code). New tests: CXFL codec round-trip, hash freeze, class
boundaries, no-false-negatives matrix, gate soundness over the shape
matrix, verify-direct freshness + missing-file fail-closed + disk-cap
(search incomplete, count refused), full-path service differential (17
shapes, search==count, lane on/off parity), budget-drops-uncovered, fold
re-read convergence, constructor/fixture ports (ASCII fast path, pruned
parity, stream/shard round-trips, integrity CXFL, reshard-via-disk).
v9 sidecars are rejected by `TestContentIndexRejectsPreviousVersion` (now
guarding v9) and the v10 round-trip test.

## 7. Carried debt / known gaps

- **Snippet case is preserved; no fixup-aware path remains.** Extraction
  preserves case (`contentRepairText`, PF-6a), and both the service
  `contentSnippet` and the offline reader render the window directly from the
  verified bytes, so snippets keep original case. `contentSnippetWindowSource` and
  its `contentSnippetSource`/fixup mapping were dead once raw text was rendered
  directly and have been removed; `contentLossyFixups` remains for the decoder.
  (v10: there is no text store; "stored text" below means verified bytes.)
- The `.gsx` is mmapped (`contentLoadFileMapped`); only its doc/path metadata is
  decoded into heap. The **base `.gsi`** is still read into heap unless the
  service runs with `-lowmem`.
- **A file extracted while still being written.** The drain promotes a dirty file
  on a close/USN-quiet window (≤2 s tick), so a file written in place without a
  close can be indexed from a partial state. The next USN change (or the close)
  re-extracts it; there is no in-progress-write guard.
- **Intra-volume rename keeps its indexed path until re-extraction.** An
  intra-volume rename preserves the FRN, so the base/delta doc keeps the old
  `Path` (the delta is not re-extracted for a rename). Content search resolves
  entry paths through the record index, and v10 verification falls back to the
  doc-table path for missing files, so matching survives either side lagging —
  this is a stale stored-path/ordering field, not a match gap.
- PDF extraction is a bounded text-layer parser (xref tables + streams, ObjStm,
  Flate/LZW/ASCII85/ASCIIHex/RunLength, ToUnicode/Differences); text-layer only,
  no OCR, encrypted → `Skipped`.
- **PDF `readStreamBytes` without `/Length`.** When a stream's `/Length` is
  absent or untrusted, the payload is delimited by the `endstream` keyword.
  Occurrences are indexed once per document (O(maxRaw)) and looked up by binary
  search, so omitting `/Length` on many objects no longer costs
  O(objects × maxRaw). Fuzzing previously drove multi-second extractions on
  ~1 MiB inputs; the index removed that stall.
- **RTF output is repaired to valid UTF-8.** The byte-level RTF scanner can split
  a multibyte rune (a control symbol consumes only the lead byte, then the
  continuation bytes are emitted as literals), so the emitted buffer is run
  through the shared UTF-8 repair before truncation. Found by
  `FuzzContentRTFExtractor`; the crashing input is committed as a seed.
- **PDF `/Encrypt` detection can be evaded by a later `/Prev` section that fails
  to parse.** The loader merges trailer keys first-wins as it walks `/Prev`; if a
  newer section parses without `/Encrypt` and an older section (which held it)
  then fails to parse, the walk stops and the document is treated as unencrypted.
  The reason degrades to "no extractable text" rather than "encrypted" — not a
  content leak, because the objects stay encrypted and decode to nothing.
- **A delta parked at its hard ceiling freezes the persisted checkpoint (M1).**
  Distinct changes beyond the hard ceiling (docs/bytes) are parked by FRN and
  replayed once the delta drains, so the resident delta is bounded in every
  case. While any are parked a fold keeps the persisted checkpoint at the base's
  value, so a restart replays those USNs instead of skipping them — the parked
  changes are delayed and surfaced incomplete (`deferred_docs`, `incomplete`),
  never lost. The trade-off: a restart during a stall redundantly re-extracts the
  USNs since the base checkpoint, and the parked set is held in memory (one FRN
  per changed file, ~50 B each — bounded by the number of distinct files changed
  during the stall, not by time; a delete burst is tombstoned in the delta
  directly and is not parked). Folds still publish the admitted delta, so a
  recovered fold drains the park set batch by batch.
- **Moved-class invalidation.** Extractor invalidation keys on the stored
  `(ContentType class, ExtractorVersion)`. A base built while an extension was
  text-indexed (class Text, e.g. `.html` before the HTML extractor) is neither
  refreshed nor rebuilt when that extension later gains a dedicated extractor:
  the old class still has a registered extractor at the same version, so
  `contentIndexExtractorMismatch` sees no mismatch and those docs keep serving
  raw/markup text until a rebuild or a USN change to each file. Fresh installs
  stamp the new class on the first build. This is the systemic case for any
  format whose extension is in the text allowlist (EML/mbox/RTF/MSG are not).
  Possible fix: compare the resolved path's *current* extractor class against the
  stored class (not only version), or force a rebuild when the class mapping for
  an extension changes.
- **Forced fold has no "attempted" latch.** `foldDue` forces a fold whenever a
  targeted extractor refresh is pending and the queue is drained, with no latch
  recording that a forced fold was already attempted. The refresh is exhaustive
  (every stale base FRN is enqueued), so `refreshPending` always clears when the
  refreshed base publishes and the branch cannot spin; a future partial/dropped
  refresh would re-force a fold every drain tick (still paced by the fold-retry
  backoff), which is why it is worth a note.
- **HTML per-token cap is the text budget, clamped to [1 MiB, 16 MiB].** The
  tokenizer's per-token buffer is `maxText` clamped to `contentHTMLMaxToken`
  (floor) and `contentHTMLMaxTokenCeiling` (16 MiB), so a large-but-in-cap token
  no longer trips `ErrBufferExceeded` and discards every token after it (the
  1 MiB fixed cap silently dropped the tail of e.g.
  `cmd/trace/static/trace_viewer_full.html`). A token larger than the (bounded)
  cap still stops extraction as a `Truncated` bounded prefix, not silently.
- **RTF group properties and fields fixed (extractor v2).** `\ucN` and
  `\ansicpgN` are restored at group boundaries; Unicode fallback ends at a
  brace. Fields retain visible `\fldrslt` text while skipping `\fldinst`.
  Existing RTF and MSG documents (which share the RTF parser) are refreshed
  through the extractor-version mechanism.
- **RTF: DBCS codepages.** `\ansicpg932/936/949/950` (CJK) are not mapped and
  fall back to CP1252, yielding mojibake; v1 covers single-byte pages only.

### Production-corpus findings (offline walk path, real corpora)

Black-box batteries over the Go stdlib tree (~8k docs), the repo, and real
PDF/DOCX/PPTX/XLSX corpora. Independent oracles: `rg -F -i` (text) and
PyMuPDF/pypdf + OOXML unzip (documents). A PDF recall battery vs PyMuPDF over 45
real in-cap PDFs measured mean recall **0.933 → 0.980** and perfect files
**31/45 → 38/45** after the WP12 fixes below.

**Fixed:**

- **Form-XObject-only pages extracted no text.** Many LaTeX/print PDFs put the
  whole page in a `/Form` XObject drawn by `Do` with the page carrying only
  `/XObject` (no `/Font`); a real 32-page paper was silently skipped. `Do` now
  recurses with the XObject's resources, bounded by `contentPDFMaxFormDepth` and
  a shared per-page `contentPDFMaxXObjectInvocations` budget (depth alone does
  not bound fan-out).
- **Ligature loss.** `ff/fi/fl/ffi/ffl` mapped to Unicode presentation forms
  (U+FB00–FB06) are expanded to ASCII; a `/ToUnicode` destination with several
  runes (producers spelling `fi` as `00660069`) now keeps the whole sequence
  rather than the first rune; `/Differences` names that are plain Latin letters
  (A–Z/a–z) now map. `workflows`/`configurations`/`Efficient` are searchable.
- **Per-glyph placement letter-spaced words.** Producers that place each glyph
  with its own `Td`/`Tm` used to come out `A B C`; spacing is now width-aware
  (`/Widths` //W` vs the accumulated `advance`), so an intra-word move does not
  insert a space while a real word gap still does.
- **Large-token HTML loss** and the **swallowed root walk error** (earlier
  fixes; see §7).
- **RTF non-BMP `\u`** surrogate pairs are reassembled instead of becoming two
  U+FFFD.
- **Truncation accounting** counts a bounded extraction that produced no text
  (e.g. a depth-cap hit) instead of dropping it.

**Open:**

- **Type0/Identity-H without `/ToUnicode`** (e.g. `ProximaNova-Regular`) drops
  the page's text with no per-result signal; recovering it needs the embedded
  font program's cmap/charset (PyMuPDF reads that; seekfs does not). 3 real
  files; the fitz oracle text for them is itself unreliable.
- **Rotated per-glyph runs that restart with `Tm` mid-word** can still insert a
  gap (1 real file, `EXPRESS` in a rotated appearance stream).
- **Skip accounting (offline CLI now reconciles).** `Skipped` counts only
  claimed-but-declined containers (PDF/OOXML). The offline build now also reports
  `scan: eligible=<n> declined=<n>` (files with no extractor — binary/unsupported
  — or whose extractor produced no text, e.g. empty), so `indexed == eligible -
  declined - skipped`. Not persisted to the `.gsx` (the policy section is a fixed
  size), so the service health view does not carry it yet.
- **Fold-after-corruption window (see §6j).** A base corrupted after its
  attach-time checksum pass is re-encoded by a fold with fresh checksums, so
  the corruption persists until a rebuild. Per-document checksums or a
  pre-fold verify would close it; deferred.
- Confirmed clean: encrypted/image-only PDFs → `skipped` with no false hits;
  OOXML oracle parity 13/13 under the 32 MiB cap; RTF/EML/mbox/MSG end-to-end
  synthetic parity (only `\fldrslt`, non-allowlisted headers and encoded blobs
  excluded by design); snippets valid UTF-8; BOM-less UTF-16 recovered under
  `-encoding auto`; `-n` caps; broad queries bounded (~3 s for ~5.8k hits over
  ~8k docs); corrupt/truncated `.gsx` and a missing term fail cleanly with no
  panic; a byte-exact extractor-faithful oracle matched 29/29 on an adversarial
  corpus.

### Explicitly deferred (P5)

Each item below is intentionally out of scope for P5; the rationale is one line.

- **PDF extraction quality**: DONE in WP11 — a bounded text-layer parser
  (xref tables/streams, ObjStm, Flate/LZW/ASCII85/ASCIIHex/RunLength,
  ToUnicode/Differences). Remaining limits: no OCR, no decryption, Type0/CID
  without ToUnicode dropped, ligature→first rune, and no object-scan fallback
  when `startxref` is missing (malformed → `Skipped`).
- **Posting-decode streaming in `contentCandidates`**: DONE in M6 — the posting
  codec's `postings`/`forEach` page the 1024-doc blocks and `content_read.go`'s
  `intersectDocIDStreams` merge-intersects lazy per-trigram streams, so a broad
  gram is never fully decoded (peak memory is O(budget + #trigrams)).
- **mmap of `.gsx`**: DONE in `9c06ecc` — `contentLoadFileMapped` maps the
  sidecar read-only with a heap fallback; see `content_mmap_test.go`.
- **Base `.gsi` mmap**: still heap-loaded unless the service runs with `-lowmem`.
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
  skipping (the postings are 4096-doc block-paged with first-docID skip
  metadata since §6m), and `.gsx` mmap.
