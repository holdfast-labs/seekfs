# perf/query-transients: cut content-verify transient allocations

Base: integ 0bf58c5. Worktree E:\bench\wt\qtrans. Harness:
`zz_footprint_local_test.go` (local only, deleted before commit) attaches a v12
content index over Chromium `chrome/`, runs 20 content queries, prints
`MEM <label>` and writes `heap-<label>.pprof`.

## What changed (verify/read path only)

1. `contentSlurpNoRecallVerify` (content_extract.go): the verify read path draws
   its buffered whole-file read from `contentVerifyBufPool` (sync.Pool, buffers
   > 4 MiB dropped on return) instead of `make([]byte, size)` per file.
   `contentSlurpNoRecall` keeps the old allocation for the build/drain paths.
   Safe because every extractor returns `Text: []byte(text)` (a fresh copy);
   the stored text a caller keeps never aliases the returned read buffer.
2. `contentFoldAccentAppend(dst, src)` (content_build.go): append-style fold,
   `contentFoldAccentAppend(dst[:0], x)` byte-identical to `contentFoldAccent(x)`.
   `matchRawFirst` and `attachContentSnippets` now fold into a pooled
   `contentFoldScratchPool` buffer (same 4 MiB cap) via
   `entryMatchesWithContentMatcher`. `matchRawFirst`'s signature is unchanged, so
   its tests are untouched.
3. New test `TestContentFoldAccentAppendMatchesContentFoldAccent` checks append
   parity for the inputs of `TestContentFoldAccentMatchesWholeStringFold`
   (shared helper `accentFoldParityInputs`), including with a reused-capacity
   scratch buffer and with a nil dst.

No result changes: fold output is byte-identical, read bytes are identical,
only the allocation strategy changed.

## Before -> after (this run, GOMAXPROCS=2)

MEM (after 20 content queries):
- Go Sys:          248.6 MB -> 200.6 MB  (-48.0 MB, -19.3%)
- private bytes:   282.7 MB -> 237.4 MB  (-45.3 MB, -16.0%)
- (content-attached baseline was sys=139.6 / private=114.5-114.8 both runs)

alloc_space over the query phase (`go tool pprof -sample_index=alloc_space
-base heap-content-attached.pprof heap-after-queries.pprof -top`):
- total:                   1662.16 MB -> 1184.17 MB (-478.0 MB, -28.8%)
- per query (20):           83.1 MB -> 59.2 MB
- contentSlurpNoRecall:    451.23 MB -> 95.57 MB (contentSlurpNoRecallMode)
- contentFoldAccent:       214.65 MB -> 43.19 MB (contentFoldAccentAppend)
- contentVerifyBufGet:     0        -> 32.02 MB (pool misses after GC)

Remaining alloc_space is dominated by the extraction decode itself
(contentTextExtractor.Extract 780 MB, contentDecodeLegacy 398 MB) which the
task scope (read/fold transients) does not touch.

## Tests

`go test ./cmd/seekfs -run
'ContentQuery|ContentVerify|ContentAccent|FoldAccent|ASCII|MatchRawFirst|ContentRegex|ContentBounded|ContentSym|ContentLines|Snippet'
-count=1 -p 1` -> ok (45.1s). Includes the new append-parity test.
