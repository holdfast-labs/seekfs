# Service memory footprint, content enabled (2026-10-15, integ 02b8190)

Index: v12 over E:\bench\chromium\chrome (~65 MB content sidecar), attached
read-only (harness: E:\bench\scratch\harness\zz_footprint_local_test.go).
Go heap after runtime.GC(); WS/private from GetProcessMemoryInfo.

| Point | HeapInuse | Sys | Working set | Private |
|---|---|---|---|---|
| start | 1.2 MB | 12 MB | 12 MB | 48 MB |
| name index loaded | 28 MB | 57 MB | 40 MB | 76 MB |
| content attached | 91 MB | 188 MB | 239 MB | 143 MB |
| after 20 content queries | 91 MB | 273 MB | 381 MB | 283 MB |
| serviceReleaseIdleCaches + FreeOSMemory | 91 MB | 273 MB | 243 MB | 144 MB |

Content attach adds ~57 MB of Go heap (inuse_space, heap-content-attached):

| Retainer | MB | What |
|---|---|---|
| decodeContentPostingIndex | 25.5 | gram/term posting entries + block metas decoded into Go slices |
| contentComposeShards | 8.6 flat | shard composition tables |
| decodePaths / legacy front-coded keys | 6.8 | doc paths |
| contentDecodeDocTable | 6.3 | doc table |
| decodeContentFilterSection | 4.5 | per-file filter rows copied to heap |
| decodeContentTermDict | 3.7 | term dictionary |

Working set beyond heap after attach (~150 MB) is mmapped sidecar pages
touched by the decode; queries add transient read/fold buffers (private
+140 MB) that the idle release returns. Full Chromium (~8x this sidecar)
extrapolates to ~450 MB heap: the owner-drive ~500 MB.

Opportunities (largest first):
1. Zero-copy posting index (~25 MB here, ~200 MB full Chromium): entries are
   fixed 12-byte records sorted by key and block metas fixed-size; binary
   search them in the mapped section instead of decoding to []struct.
   Risk: low-moderate (accessor change, same bytes).
2. Filter rows and doc table read from the mapping (~11 MB here): fixed-width
   rows; decode on access.
3. Paths: keep front-coded on the mapping and decode per lookup (~6 MB).
4. Shard composition (8.6 MB): check for per-doc maps that could be slices.
5. Query transients: per-verify file buffers (+140 MB private across 20
   queries); a pooled read buffer would cap the peak.

## Zero-copy postings (2026-10-15, perf/footprint-content)

Footprint harness before (commit b9fc845): `MEM content attached heapInuse=91 MB`
(WS=239 MB, private=143 MB). After zero-copy entry/block views:
`MEM content-attached heapInuse=66.3 MB` (WS=206.9 MB, private=117.7 MB).
The mapped section now supplies both tables directly; the decoder retains only
the block-offset prefix array (bounded by block count).

## Done: zero-copy posting index (opportunity 1)

Entries/block metas read from the mapped section; one uint32 offset per block.
chrome/ v12: heap after content attach 91 -> 66 MB; private after idle
release 144 -> 119 MB; WS after idle release 243 -> 218 MB.
