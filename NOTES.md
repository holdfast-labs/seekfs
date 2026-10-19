# Content cursor v2 (design)

Status: implemented on `feat/content-cursor-v2` (v2) with v1 kept for out-of-scope shapes. v1 is `content/cursor` (607d38b, G10).

## 1. v1 code path

- `searchCursor` (`cmd/seekfs/search.go:17`) carries `Version=1`, `Offset`,
  and `Query` (`queryOptionsHash`, `search.go:23`). Encode is at `search.go:36`;
  decode at `search.go:41` rejects malformed tokens and query-hash mismatches.
- `service_server.go:324-338` applies the offset: decode at `:327`, then
  `opts.Limit = cursorOffset + pageLimit` at `:336`. The result is sliced at
  `:434-438`; `:455` emits a cursor when `len(matches) == pageLimit`.
- Content routes at `search.go:507-520` to `searchContentServiceVolumes`
  (`content_verify.go:1813`). `nameTermCandidates` (`search_compact.go:1212`)
  builds candidates; `verifyContentSupersetPage` (`:939`) or
  `verifyCompactContentParallel` (`:632`) reads disk in rank order and stops
  at the limit. `mergeOverlayMatches` (`search.go:612`) merges the delta and
  cuts at `contentFrontier` (`:653`, recorded by `types.go:1050`).
- `contentDiskVerifyBudgetOf` (`content_verify.go:124`) scales with
  `pq.Limit`, which v1 inflates by the offset. Page N verifies about `N*limit`
  candidates, including disk reads, and changes can shift rows.

## 2. Cursor v2 payload

- Use opaque base64url JSON with short keys: `v:2`, `q` (query hash), `vol`
  (one volume), `off` (emitted rows; fallback).
- `id`: the last emitted RECORD (FRN). A single-volume content result is
  emitted in candidate order (rank/symbol promotion is not expressible by the
  name/path comparator), so resume rebuilds that deterministic candidate
  list, locates this record, and verifies from the next candidate. A missing
  record restarts at offset zero.
- `name`: `JournalID`, base `Checkpoint`, record count, and
  `snapshotGen` (`service_protocol.go:607`), which advances on overlay publish
  (`service_runtime.go:452`).
- `side`: `Version`, `Origin`, `JournalID`, `CheckpointUSN`, `ScopeHash`,
  `BuiltAt`, `EncodedSize` (`content_index.go:286`). A fold swaps the sidecar
  through `setReady` (`content_service.go:822`) without a name-index change.
- `delta`: content-delta generation plus a service-instance ID.

Add `contentVolumeState.deltaGen atomic.Uint64`; bump it in `upsert`
(`content_service.go:230`), `delete` (`:256`), `removeUnchanged` (`:402`), and
delta replacement (`markStale`, `markIntegrityFailed`). Capture it at request
start and re-read before emitting; if it changed, do not emit v2. A random
per-start instance ID rejects a generation that reset to zero.

## 3. Correctness rules

- Static state: rebuild the same ordered candidate list (identity is
  unchanged) and verify from the candidate after the last emitted record.
  Earlier candidates were verified or emitted, so pages have no duplicates or
  gaps. CPU re-sorting is allowed; disk verification stays one page.
- Overlay: v2 is refused while a content delta or filename overlay is
  present. Those shapes keep the v1 offset cursor and the existing
  `contentFrontier` cut.
- Incomplete pages and valve: keep `ContentIncomplete`, `BudgetExceeded`, and
  partial-page semantics. Emit `next_cursor` only for a full page. A full page
  that hit the valve can advance: every unverified candidate ranks after the
  last emitted hit. Resume cost is about `limit + 4*workers`. For v2 set
  `opts.Limit = pageLimit`, so the valve prices one page.
- Change between pages: a name, sidecar, delta, volume, or instance mismatch
  invalidates the cursor. Recommended: restart at offset zero and set
  `cursor_restarted: true` with a reason; the CLI warns. Do not silently use
  v1 offsets: that hides a shift and still costs `O(offset)`. A query-hash
  mismatch remains an error.
- Source-only mutation: text can change on disk before USN replay reaches the
  index. Page N+1 reads fresh text at or after the frontier; emitted rows are
  not revisited. The divergence is bounded and cannot duplicate an emitted row.
- First version: v2 only for one eligible content volume and a non-relevance
  sort. Multi-volume, `sort:relevance`, and fuzzy top-up keep the v1 offset
  cursor. Relevance scores and fuzzy groups are post-passes, not rank keys.

## 4. Options, recommendation, plan

- Recommend opaque base64url JSON: consistent with v1, debuggable, stateless.
  Binary adds no value here. Reject tokens above 16 KiB.
- A server-side cache could also cache the ranked superset (CPU O(page)), but
  needs TTL, memory caps, invalidation, and dies on restart. Defer it.
- Plan: (1) add the v2 token, identity capture, delta generation, and
  `cursor_restarted`; keep v1 decode. (2) add `contentAfter`, apply it after
  candidate sort/promotion and overlay filtering, and emit the last result key.
- Tests: paged output equals one big page on a static fixture (default and
  explicit sorts, overlay delta); page N verifies about `limit` (`DiskVerified`);
  a delta or name-index mutation sets `cursor_restarted`; a low valve or
  deadline gives a partial page that resumes without duplicates, and a short
  page emits no cursor; multi-volume and relevance emit no v2; bad tokens fail.


