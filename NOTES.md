# perf/footprint-shards — content attach heap footprint

Base: integ 0bf58c5. Worktree only; harness (`zz_footprint_local_test.go`) was
copied into cmd/seekfs, run with `SEEKFS_FOOTPRINT_LOCAL=1`, then deleted before
commit. Index measured: `E:\bench\scratch\regex2\chromium.gsi` (Chromium
`chrome/`), Mapped v12 sidecar (sharded manifest over a shard dir).

## MEM (content-attached heapInuse, after runtime.GC)

| stage            | before  | after   |
|------------------|---------|---------|
| start            |  1.3 MB |  1.3 MB |
| name-loaded      | 28.3 MB | 28.3 MB |
| content-attached | 63.5 MB | 45.3 MB |
| after-queries    | 64.2 MB | 46.0 MB |

Content attach cost dropped from ~35.2 MB to ~17.0 MB (≈18 MB, 51%).

`pprof -inuse_space` top, before → after:
- `decodeLegacyFrontCodedKey` 12.0 MB → gone (below top-20)
- `contentComposeShards` (combined doc table append) 8.6 MB → 7.5 MB (single copy)
- `contentDecodeDocTable` 7.2 MB → gone (per-shard tables freed)
- `decodeContentPaths` (flat) 0.6 MB → gone

## What changed

### 1. Lazy path decode (`content_build.go`, `content_read.go`)
- Replaced `decodeContentPaths` (materialized every stored path into a
  `[]string`) with `openContentPaths` returning a `*contentPathTable` that keeps
  the CXPT section (aliases the mapped file) and decodes a path on access from
  the nearest restart (interval 64), mirroring `contentTermDict.keyAt`.
- Open-time validation kept fail-closed for structure (header, restart offsets,
  65535-truncated restart keys, exact permutation) without decoding all keys.
  The CXPT section's CRC is verified eagerly at load, so blob-level corruption
  is still caught.
- Per-reader cache holds the current restart run (`cacheRun`/`cacheKeys`,
  ≤ interval strings) so sequential access (fold re-reads, result listing)
  costs one decode per run, not per path.
- `contentReader.paths` changed `[]string` → `*contentPathTable`; only
  `docPath` touched it. Walk-origin joins are unaffected: FRN≈`contentWalkKey`
  is stamped at build, and cross-volume join columns come from the filename
  index, not doc paths.

### 2. One doc table instead of two (`content_shards.go`, `content_dedupe.go`)
- `contentComposeShards` now preallocates the composite `docs` to the exact
  total and, after appending a shard's docs, points that shard's `idx.Docs` at
  its slice of the composite. The shard's own decoded doc table is freed, so a
  composite holds one doc table (~7.5 MB) instead of two (~15.8 MB).
- Shard-local structures (paths, filter rows, postings, aliases) stay
  local-keyed; the two sites that read a shard doc's `DocID` as a local ID now
  use the position: `contentShardFoldDiskSource` (docPath by index) and
  `contentDedupeTotalsForIndex` (alias match by index). `DocID==position` for
  every non-composite index, so the dedupe change is behavior-neutral there.

## Skipped / not changed
- `contentDoc` layout and the doc-table encode/decode (`contentDocFixedSize`,
  `contentDecodeDocTable`): `idx.Docs` is read/mutated in ~60 sites and the
  decoder is shared by single-file loads; left as-is.
- Filename index sections (`readIndexWithReaderAt`,
  `readDerivedSectionsFromReaderAt`) are ~28 MB of the remaining footprint and
  out of scope for this task.

## Tests
Required set green (`go test ./cmd/seekfs -run
'ContentShard|ContentIndex|ContentQuery|ContentRead|ContentWalk|ContentPaths|Corrupt|ContentBounded|ContentAccent|ContentSym|ContentFilter|ContentDelta|ContentFold' -count=1 -p 1`).
Extended set green (`ContentService|ContentReconcil|ContentResolver|ContentVerify|ContentStream|ContentRestart|ContentPrefetch|ContentExtractor|ContentAttach|ContentVolume|ContentDedupe|ContentCompact|ContentEncoding|ContentScope`).
gofmt clean; `go vet ./cmd/seekfs` clean.
