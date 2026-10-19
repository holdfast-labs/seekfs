# Content cursor v2 on overlay volumes

## 1. How one single-volume content page is ordered
Dispatch: `searchServiceVolumes` -> `searchContentServiceVolumes` (search.go:507-520) ->
`searchContentVolume` (content_verify.go:1685-1712).
Base side: `nameTermCandidates` builds a posting superset or bounded scan in `rankForQuery`
order. A content superset gets `sortCandidateIDs` then `promoteSymbolNamedCandidates`
(search_compact.go:1284-1288, 1346-1358); a bounded scan walks `orderForQuery`
(candidate_scans.go:423, 474-524). Verification stops at the page/window
(`verifyContentSupersetPage`, search_compact.go:935-1045); the v2 start is search_compact.go:951-969.
Hidden IDs: `snapshotHiddenBaseIDs` (search.go:733-742) is passed to the base search
(content_verify.go:1691-1692). Hidden IDs filter, they do not reorder: verify loops skip them
(search_compact.go:561; candidate_scans.go:449, 1016), so later candidates fill the holes.
`hiddenBlocksTruncation` disables top-N truncation (search_compact.go:212-218).
Overlay side: `overlayRankedMatches` walks `snap.records[:watermark]` (search.go:677-682), tests
the predicate, then sorts by `overlayEntryRank` and name/path (search.go:673-731).
Merge: the content limit is forced to 0 (search.go:630-636), base and overlay rows are
stable-sorted by `compareSearchAllEntries` (search.go:638-646), the incomplete frontier is
applied (search.go:653-663), and the caller trims to the user limit (content_verify.go:2023-2025).
A single volume is not sorted again (content_verify.go:2013-2021). The comparator is
search.go:331-405.
Result: the order is a mix. `compareSearchAllEntries` orders the union of the verified base
window and matching overlay rows; base candidate order only selects which base rows are verified
and admitted. Only when `len(snap.records)==0` does merge return base unchanged
(search.go:622-625), making the final order the base candidate order.
Anchor: v2 carries one FRN (content_cursor.go:67-81) and finds it only in base candidate order
(content_cursor.go:201-215). An overlay-only row has no base candidate; a shadowed overlay row
maps to the hidden base candidate, not to its overlay position. A page ending on an overlay row
gets `Found=false` (service_server.go:542-549) and restarts. Even for a base row, the last emitted
FRN is not a position in the merged comparator order. The sound anchor is a comparator key. A
`(base FRN, overlay key)` pair is also not exact unless base order is monotone in that key (see 3.2).

## 2. What invalidates a resume
Must invalidate: query change (content_cursor.go:105-117); base index swap or sidecar change,
covered by `Side` (Version, JournalID, CheckpointUSN, ScopeHash, BuiltAt, EncodedSize;
content_cursor.go:139-157) and `Name` (JournalID, Checkpoint, Records; 189-194); non-empty or
changed content delta (refused at 177-179, generation at 196); service restart (Instance at 197,
check at 133-135).
Safe with the keyed cursor (3.1), or when they cannot touch the query's candidates:
- `snapshotGen` bumps. `publishSnapshot` increments it before every publish
  (service_runtime.go:452-467), after every replay batch (service_replay.go:512-515) and at the
  persist swap (service_runtime.go:1241). It means "a publish happened", not "an order-relevant
  row changed".
- Publishes that add non-matching rows and do not hide a base candidate in the unverified suffix.
  They cannot change the emitted prefix.
- Added hidden base IDs: they filter remaining candidates and never reorder.
- Deletes and renames of rows already emitted.
A cheaper identity is sound only with a key cursor (3.1): `(Volume, Instance, Query, Side, Records,
Delta)`, with `SnapshotGen` dropped. Overlay freshness becomes the cursor key plus a bounded scan
of slots appended after the cursor watermark. For the current FRN anchor no cheaper identity is
sound in general: one matching overlay row can enter anywhere in the merged order, and only the
query-relevant overlay state proves that it did not. Keep `SnapshotGen`, or restrict v2 to
`len(snap.records)==0`.

## 3. Options

### 3.1 Keyed resume on the merged comparator (recommended)
Order base candidates by the `compareSearchAllEntries` key (sort column, lower name, lower path,
path, name; search.go:331-405). On overlay pages do not apply symbol promotion; the merge sort
already discards it (search.go:644-646). Merge overlay rows by the same key, verify base rows in
that order, emit until the page fills. The cursor stores the last emitted key; the FRN can remain
as a diagnostic.
Correctness: the key is total and the path is unique. On a static volume every later match has
key > K and every earlier one was emitted or is a non-match: no duplicates, no gaps. The
window/frontier still bounds verification (search_compact.go:935-1045). Churn: a new or renamed
row with key <= K is missed; an emitted row renamed to key > K can repeat. The bound is the number
of changed paths. Scan slots appended after the cursor watermark; report `cursor_diverged` when a
matching row lands at or before K. Restart is optional. Cost: key sort in place of rank sort, same
O(n log n); path reconstruction only on name ties; one page of disk reads. Complexity: medium
(candidate ordering, verify loop, cursor schema; v1 stays as fallback).

### 3.2 Base FRN + overlay frontier pair
Cursor = (last verified base FRN in base order, last overlay slot or key, cutoff key K). Resume base
after its FRN, overlay after its slot, merge, drop rows with key <= K.
Correctness: not exact. A match verified in an earlier page but pushed past K can have base rank <=
the anchor, so resuming after the anchor skips it forever. `promoteSymbolNamedCandidates`
(search_compact.go:1287, 1357) and the record-id versus path tie break (content_verify.go:2014-2020)
break key monotonicity. It is exact only when base order equals comparator order, which is 3.1.
Churn: appended overlay rows are cheap to check; renames across K duplicate. Cost: one page of I/O
but a full candidate rebuild plus extra state. Complexity: high for the proof. Not recommended.

### 3.3 Bounded acceptance on the FRN cursor
Keep v2. Relax the identity to `(Volume, Instance, Query, Side, Records, Delta)` and accept a
publish only when `len(snap.records)==0`, so `mergeOverlayMatches` returns base unchanged
(search.go:622-625) and the final order is the base candidate order. Never accept a matching
overlay row; a non-empty overlay re-sorts the page by the comparator (search.go:638-646), so a
base-order FRN anchor is no longer a position in the page.
Correctness: exact for the accepted set; `snapshotGen` and unrelated checkpoint advances no longer
invalidate. Cost: none beyond the existing `len` check. Complexity: low. Benefit: only the
empty-overlay moments between USN batches, not live E: churn.

Recommendation: 3.1. It matches the page definition that already exists, removes `SnapshotGen`, and
turns overlay churn into at most a bounded, reported divergence instead of an O(offset) v1 restart.

## 4. Test plan
Fixtures: `buildOverlayLimitFixture` (overlay_merge_test.go:15-57) plus `applyUSNChanges`, which
publishes the snapshot (service_replay.go:512-515). For content hits on overlay rows add
`contentDeltaDoc` text (content_service.go:275-298), or use the real-disk `contentFilterV11Fixture`
(content_filter_service_test.go:18-60). Reuse the helpers at content_cursor_v2_test.go:17-45.
1. Populated overlay equals one page. Base `needle-*` rows plus overlay creates and renames that
   interleave by name and path. Page with limit 3 to exhaustion. Assert concatenation equals a
   limit-1000 search, no duplicate, every cursor decodes v2, and resumed `DiskVerified` is at most
   page plus snippets (pattern at content_cursor_v2_test.go:86-120).
2. Publish between pages. (a) Append a non-matching row, and do a snapshotGen-only publish: assert
   no restart and pages still equal one big page. (b) Append a matching row keyed before the cursor:
   assert `CursorRestarted` or the divergence reason, and that post-restart pages equal one big page.
3. Delete/rename between pages. Delete an un-emitted base hit: it never appears and no other row
   duplicates. Rename an emitted hit across the cursor: divergence is reported (or restart happens)
   and the concatenation has no duplicate path. Also rename an un-emitted hit to before the cursor.
4. Disk valve mid-page. Clone `TestContentCursorV2ValveCappedPagesResumeWithoutGaps`
   (content_cursor_v2_test.go:231-281): 400 base hits plus several overlay hits, valve 150, limit
   100, page scale 1. Assert pages concatenate to the uncapped order, no gaps or duplicates, v2 on
   every page, and no restart under a static overlay.
