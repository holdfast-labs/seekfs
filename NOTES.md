# Footprint: filter rows + doc table (2026-10-16, perf/footprint-rows)

Follows the zero-copy pattern landed on perf/footprint-content: keep immutable
section bytes in the mapping and decode on access instead of materializing a Go
struct per document. Harness: E:\bench\scratch\harness\zz_footprint_local_test.go
(copied to cmd/seekfs, run with SEEKFS_FOOTPRINT_LOCAL=1, deleted before commit).
Index: v12 sidecar over E:\bench\chromium\chrome, attached read-only.

## TASK 1 — filter rows: done

`contentFilterSection.Rows []contentFilterRow` (32-byte header per document:
slice ptr+len+cap plus the class int) is replaced by the section bytes plus
`rowOff []uint32` (one offset per document at the class byte). `row(i)` decodes
a row on access (class = data[off], bits = data[off+1:off+1+classBytes]); bits
still alias the mapping. Decode-time validation is unchanged (class range,
truncation, exact length); only the offset table is retained per document
(4 bytes vs 32). Readers updated: `contentReader.filterRow` and the filter test.

## TASK 2 — doc table: skipped (not cheap/safe)

`contentDecodeDocTable` rows ARE fixed-width on the wire (52-byte CXDT
records), so the on-disk side is indexable in principle. The blocker is the
in-memory side: `contentIndex.Docs []contentDoc` is a first-class, frequently
MUTATED field, not a read-only decoded view. ~60 non-test sites thread it
through the builders (`content_build.go` appends, `contentDocSortByFRN` sorts,
`content_shards.go` and `content_service_fold.go` merge/append, dedupe and
scope reconcile rewrite), and `contentDoc` also carries v8-era fields
(RawSize/DocLen/ModUnix) that are set at build but absent from the wire. The
struct does have a layout distinct from the wire record (padding + extra
fields), so the mapped bytes cannot be aliased as `[]contentDoc`.

Replacing the field with a mapped view would touch every one of those sites
(sort/append/merge on a view type) in one change — not cheap and not safe to
land here. Recommended follow-up: introduce a `docTableView` (Count/DocID/FRN/
TextLen/FileSize/FileModNano/ContentHash accessors + FRN bisection over the
mapped CXDT) and convert the read-only consumers first, keeping the materialized
slice for builders. Also worth dropping the dead RawSize/DocLen/ModUnix fields
(~24 B/doc) once their test references are migrated.

## MEM lines (harness, heapInuse; before = integ ac8750d + harness)

Before (content_filter.go at integ):
- content-attached heapInuse=66.0 heapAlloc=64.1 sys=143.6 WS=213.6 private=116.9 MB
- idle-released   heapInuse=66.5 heapAlloc=64.6 sys=208.6 WS=218.3 private=119.2 MB

After (this change):
- content-attached heapInuse=63.4 heapAlloc=61.7 sys=143.6 WS=183.0 private=114.4 MB
- idle-released   heapInuse=64.1 heapAlloc=62.2 sys=196.4 WS=215.4 private=116.2 MB

Delta: content-attached heapInuse -2.6 MB, idle-released -2.4 MB (the
~92k-document offset table at 4 B/doc vs the prior 32 B/doc row headers,
28 B/doc saved).

## Tests

- Existing: `go test ./cmd/seekfs -run
  'ContentFilter|ContentIndex|ContentQuery|ContentRead|ContentShard|Corrupt|ContentBounded|ContentAccent|ContentSym'`
  — PASS (45.6 s).
- New: `TestContentFilterDecodeAlloc` — decoding a 100k-row section allocates
  < 1 MiB (runtime.MemStats TotalAlloc delta) — PASS.
