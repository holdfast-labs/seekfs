# Fold cost under no stored text
Design note for `content/fold-postings`, from `integ` 8db9bad. No product code changes.

## 1. What a fold rebuilds today
`runContentFold` rewrites only dirty shards (`cmd/seekfs/content_service_fold.go:315-332`). `contentShardFoldDiskSource` merges surviving base docs with the incoming delta, and `contentFoldDiskSource.next` re-extracts every base doc from disk (`cmd/seekfs/content_shards.go:571-592`, `cmd/seekfs/content_service_fold.go:594-734`). The unsharded path does the same for every doc, or re-reads all docs when it reshards (`cmd/seekfs/content_service_fold.go:238-310`, `:278-282`). The assembler tokenizes each doc and rebuilds every text-derived section (`cmd/seekfs/content_build.go:626-875`).

## 2. Text-derived sections
| Section | Source | Rebuild from encoded sections alone? |
|---|---|---|
| `CXTR` terms | folded words + per-term `df`, `content_build.go:774-777`, `content_termsdict.go:82-141` | keys partly; exact `df` for removed docs: no |
| `CXGR` grams | distinct trigrams per doc, `content_build.go:778-780`, `content_postings.go:253-333` | yes, scan/edit existing postings |
| `CXGP` pruned | gram `df > contentGramPruneDF`, `content_postings.go:50-62`, `content_build.go:848` | keep: yes; exact un-prune: no |
| `CXFL` filter | per-doc bloom over grams, `content_build.go:782-795`, `content_filter.go:93-130`, `:167-220` | unchanged rows verbatim; new docs need text |
| `CXDT` docs | DocID, FRN, class, hash, `TextLen`, fingerprint, `FRNSeq`, `content_index.go:475-495` | unchanged rows verbatim; new docs need text |
| `CXPT` paths | path list + `perm[DocID]`, `content_build.go:1386-1445` | yes |
| `CXAL` aliases | `(TextLen, ContentHash)` dedupe, `content_build.go:738-747`, `content_dedupe.go:56-133` | yes |
| `CXPL`/`CXSC`/`CXIS` | policy, scope, counters, `content_index.go:535-550` | yes |
There is no symbol or snippet section: `sym:` is matched at query time (`content_sym.go:109-180`, `content_verify.go:329-331`), and matches/snippets come from a fresh disk read (`content_read.go:87-91`). `DocLen` is not on the wire since v9; `TextLen` is (`content_index.go:480-495`). `CXRN`/`CXDX` tags exist but no writer emits them (`content_index.go:161-171`). Output is deterministic, in sorted tag order (`content_index.go:615-625`), with a CRC32C per section (`content_index.go:25-40`); small sections verify at load, large terms/grams/filter sections in the background pass (`content_index.go:67-80`, `content_integrity.go:46-112`). Unrecoverable: the **term set of a removed or replaced doc** (exact `CXTR` `df`) and the postings of grams pruned at build time. All other state is in the doc table, filter rows, paths, aliases, or postings.

## 3. Option A: posting-level fold
Per dirty shard, using the loaded `sh.idx`/`sh.reader` (`content_service_fold.go:315-332`):
1. Remap DocIDs. Deletions shift positions; `DocID == index in the FRN-sorted table` (`content_index.go:468-472`, `:709-713`), so FRN order is fixed.
2. Drop changed/deleted DocIDs by scanning `CXGR` entries/blocks. A per-doc gram list is not needed for removals; this corrects `docs/CONTENT_SEARCH.md:2506-2509`.
3. Re-extract only changed/added docs, then merge their postings. Postings are ascending IDs, delta-varint or bitmap per 4096-ID block with an absolute `firstDocID` (`content_postings.go:159-199`); re-encode per key, do not patch.
4. Carry unchanged `CXFL` rows verbatim, reordered by new DocID; derive rows for changed/new docs (`content_filter.go:167-220`). The filter is a necessary-only gate, so this stays safe.
5. Rebuild `CXPT` from surviving/new paths and recompute `CXAL` from doc-table `(len, hash)` groups. When an old primary is deleted/replaced, promote an alias and copy the old primary's postings (identical text by the dedupe key).
6. Carry `CXTR` keys/`df` and `CXGP`; add new terms/grams. Recompute dedupe totals as today.

Limits:
- `CXGR`/`CXGP` match a rebuild except for **un-pruning**: a pruned key was never stored, so deletions that drop its `df` below `contentGramPruneDF` are invisible. Keeping it pruned is result-correct (`content_read.go:512-533`) but differs from a rebuild.
- `CXTR` exact `df` is not achievable for removed docs. Stale keys and high `df` change fuzzy correction ordering/truncation (`content_fuzzy.go:218-260`, `:441-451`). Exact/term/phrase/regex/`sym:` results are unaffected. Exactness needs a format addition (estimated several KB/doc) or a semantic change: stop using `df` for fuzzy ordering.
- A fold no longer re-reads unchanged docs, so it no longer repairs an out-of-band edit that never reached the delta. The base must be complete at the checkpoint; catch-up must finish first (already gated).
- Whole-file byte identity is **not required and not achievable**: `BuiltAt` is wall-clock (`content_index.go:602-610`) and `CXTR`/`CXGP` diverge. Required equivalence: same live doc set; identical `CXDT`/`CXPT`/`CXFL`/`CXAL`/`CXGR` (except pruned keys); identical exact query results; fuzzy either documented-approximate or made `df`-independent.

## 4. Option B: append segment plus tombstones
Keep base shards immutable. Write a small per-shard segment (v13 shard with its own doc table, grams, filter, terms, plus a tombstone section of FRNs) and publish it through the manifest (`content_shards.go:447-458`). Segment files use unique names like shards (`content_shards.go:389-405`); a crash before the manifest rename leaves orphans for `contentSweepShards` (`:464-495`). The in-memory delta already scans every live delta doc and verifies it directly (`content_verify.go:1479-1480`); persisted segments can join that scan, while base candidates check the tombstone/shadow FRN set first. Store the segment text (compressed, bounded) so scanning does not become one disk read per recent doc; query cost then grows by the segment live count, as today's delta does; without text, add one verify per segment doc under the disk budget. Compaction rewrites a shard with today's fold, or later with Option A. Complexity: manifest/attach, sweep, cursor identity, counts, fuzzy dictionary merge, scope reconcile, and durable bytes the scope budget must count. Steady-state disk grows by the segment bytes; compaction peak equals today's fold. This preserves the base verbatim and avoids the `CXTR`/prune problem until compaction, but is a larger query-path change than A.

## 5. Option C: cheaper shapes
1. **Smaller shards.** Fold time is linear in shard text. At 64 MiB (`content_shards.go:42`) a shard holds about 12k docs and costs about 65 s. 16 MiB gives about 16 s; 8 MiB about 8 s. Cost: more shard files, duplicated dictionaries/restart tables, more manifest entries, more query streams. `under:` shard masks limit query impact. Lowest-risk mitigation.
2. **Hot-folder shards.** Re-plan frequently dirty folders into smaller shards at the next reshard/rebuild. Needs delta-frequency tracking and pairs with B.
3. **Per-shard fold budget.** Fold a shard only when its delta share exceeds a fraction of the shard, not on the global 8192-doc/64 MiB trigger (`content_service_fold.go:23-54`). Needs a durable delta to free memory.
4. **Incremental publish.** Rewrite and publish one dirty shard per tick. Same total work, lower spikes and peak; pairs with A.

## 6. Peaks and time for one changed file in a 64 MiB shard
Chromium reference: 463,273 docs, 2609 MB text, 615 MB index (grams 341, filter 125, terms 115, doctable 23, paths 11), fold 43 min, 5.6 ms/doc (`docs/WORK_QUEUE.md:191-197`). A 64 MiB shard is about 2.6% of that corpus.
| | Today | A | B | C (16 MiB) |
|---|---|---|---|---|
| Fold CPU | ~12k extractions, ~65 s | 1 extraction + re-encode ~16 MB, ~0.2-0.5 s | segment write, ms; compaction later | ~4x less |
| Heap | up to ~250 MB/shard, 64-doc/64 MiB batch + 2x12 MiB arenas (`content_build.go:450-478`, `:562-565`) | old mmap + new tables + one doc, ~10-30 MB | base mmap + small segment index | scales down |
| Disk | old + scratch + new, ~2-3x encoded shard | old + new, ~2x encoded (~30 MB) | base + segments; compaction peak as today | scales down; sidecar slightly up |
Cold-cache (Defender first open) multiplies today's fold several times; A pays one file open, B pays none at fold time. The reserve is 10 GiB (`content_space.go:22-24`, `:280-291`) and the build peak projection is 0.8x extracted (`content_space.go:66-89`, `:138-157`). B's durable bytes must enter this accounting; A/B must keep the over-cap behavior (`content_service_fold.go:375-390`).

## 7. Recommendation
- Short term: lower the shard target (for example 16 MiB) for new/resharded volumes. Low risk, about 4x faster folds, lower peaks. Do not re-plan live volumes just for this.
- Medium term: Option A for stable-shape shards. Implemented on `content/fold-postings`; see section 10. It removes the O(shard docs) disk extraction and keeps one canonical format, the existing atomic publish, and the unchanged query path. Exact `CXDT`/`CXPT`/`CXFL`/`CXAL`/`CXGR` reconstruction with a documented `CXTR`/prune approximation; fuzzy ordering stays `df`-dependent until the fallback resets it.
- Option B only if durable deltas or exact base immutability are required. It fits query-time freshness and compaction scheduling, but touches manifest, attach, cursor, count, and fuzzy paths. Keep the re-extract reshard fallback for rare growth (`:278-282`).

## 8. Test plan
1. **Sections and queries:** fold a fixture after edits/deletes/renames/adds, rebuild the same corpus from disk, and compare canonical sections (`CXDT`/`CXPT`/`CXFL`/`CXAL` exact; `CXGR`/`CXGP` differ only for pruned keys; `CXTR` by policy). Run term, phrase, case/accent, regex, `sym:`, `under:`, counts, and cursors against both; assert equal results and `search == count`. Reuse `content_bounded_differential_test.go:92`.
2. **Remap and dedupe:** delete first/middle/last docs; verify postings, filter rows, paths, and aliases resolve the same FRNs. Test duplicates, primary deletion/change, and a lower-DocID duplicate; compare with a rebuild.
3. **Pruning:** cross `contentGramPruneDF`; assert no false negatives (see `TestContentPrunedIndexMatchesUnpruned`) and document the un-prune divergence.
4. **Crash and checksums:** inject failures after shard write/before manifest and after publish/before sweep; reopen and assert the old or new base, never mixed. Flip one byte per rebuilt section; load must fail with `contentSectionCRCError` (`content_index.go:42-53`, `content_integrity.go:46-88`).
5. **Peaks/idempotence:** extend `content_save_peak_test.go:156-240` for fold heap/disk; fold twice and assert no section or result change; an empty delta must not rewrite shards.
6. **Timing:** benchmark a 1-file change in a 64 MiB shard warm and cold; record `search_ms`/`source` on the release binary.

## 9. Open issues
- `CXTR` exactness vs fuzzy behavior is the main decision; without a format addition, fuzzy correction ordering can differ. Pruned-gram un-pruning is not recoverable, so keep the conservative behavior.
- Option B needs a durable-delta budget so scope selection still bounds disk.
- A must not advance the checkpoint or publish before durability; keep the temp+fsync+rename ordering (`content_index.go:991-1073`, `:1075-1100`; `content_shards.go:447-458`).

## 10. Implementation on `content/fold-postings`
- `content_fold_postings.go`: posting fold for a stable-shape shard and for a
  stable single-file sidecar (the latter stores its counter in the sidecar).
  Unchanged docs carry `CXDT`/`CXPT`/`CXFL` rows; removed/changed DocIDs are
  dropped; changed/added docs are re-extracted once (delta text is the
  unreadable-file fallback); `CXAL` is recomputed from doc-table hashes; a
  promoted alias copies the old primary's posting row.
- Counters live in the manifest's `CXFC` section, keyed by shard file name
  (single-file key `""`). `contentPostingFoldMaxFolds = 32` and
  `contentPostingFoldMaxChangedPercent = 25` trigger the full re-extract path,
  which resets the counters and restores exact `CXTR` `df` and un-pruned grams.
  The pending fold's changed/removed docs count toward the 25% share, so a
  single large fold falls back immediately.
  A reshard/build omits `CXFC`, so it also resets them. No format-version bump:
  older binaries ignore the unknown section.
- `contentShardPostingFold` applies only when the shard has filter/path
  sections and no unexpected alias shape; otherwise the existing re-extract
  path runs. The shard save stays temp+fsync+rename, and the manifest is still
  the commit point.
- Tests are in `content_fold_postings_test.go`: section/query equivalence vs a
  from-disk rebuild across three shards; delete first/middle/last and dedupe
  promotion; pruned-gram no-false-negatives; failed-manifest restart and
  per-section CRC corruption; 25% and 32-fold fallback with exact `CXTR`; one
  changed doc reads exactly one file (sharded and single-file).
- Residual: `CXTR` `df` and the pruned set are stale until the full fallback.
  An out-of-band edit not present in the delta is not repaired by a posting
  fold (unchanged docs are not re-read); a fold whose changed share crosses 25%
  still falls back to the full re-read.
