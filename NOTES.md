# Overlay incomplete-page rank-prefix gap (design only)

Status: analysis only; no product code changed. Branch `fix/overlay-incomplete-prefix`.

## 1. Code path (incomplete default-order content page with an overlay)

Volume with a pending overlay (non-empty `snapshotHiddenBaseIDs` and/or overlay
records), default order, page ends incomplete (disk-verify cap or `--budget`).

- Base verify stop (page path): `verifyContentSupersetPage`,
  `cmd/seekfs/search_compact.go:919`; `stopped()` guard `search_compact.go:926`;
  early return `search_compact.go:946-948`; unverified frontier
  `search_compact.go:987-990`; reached from `nameTermCandidates`
  `search_compact.go:1250`. Parallel producer frontier
  `verifyCompactContentParallel` `search_compact.go:625` at `:761-767`. Both
  emit only the contiguous verified prefix, so a base-only page IS an exact
  rank prefix.
- Overlay search: `overlayRankedMatches` `search.go:656` (matcher `:670-673`,
  match `:680`); rank `overlayEntryRank` `search_overlay.go:123`.
- Merge + re-sort: `mergeOverlayMatches` `search.go:612`, called at
  `content_verify.go:1655`; re-sort `search.go:644-646`; truncation
  `search.go:647-649`; content leaf sets `limit=0` (`search.go:631-636`).
- Complete/incomplete decided: window probe `content_verify.go:1947-1951`
  (`setContentIncomplete` `types.go:1136`); cap latch `acquireDiskRead`
  `types.go:886-904`; `stopDiskVerify` `types.go:972`.
- `capDiskBudgetForProbe` (`types.go:829-847`) called at
  `content_verify.go:1874` and `:1928`, gated by
  `reduced := probeForPage && hidden.empty()` (`content_verify.go:1865`). When
  hidden is non-empty the base pass is not reduced, so the window probe runs and
  lowers the shared reservation after it, shrinking the base frontier further.
  The overlay merge is unaffected (delta text reads do not consume it; disk
  reads past it are denied). Net: the gap widens.

## 2. Minimal non-prefix scenario (confirmed, throwaway test deleted)

Base `a.txt`,`b.txt`,`c.txt` (all `needle`); overlay `bc.txt` with content-delta
text `needle`. Rank order `a<b<bc<c`. Cap = 1 read. Base verifies `a`, denies
`b`, frontier at `b`; base page `[a]`. Overlay `bc` matched from the delta (no
read). Merge re-sorts `[a, bc]`. Complete = `[a, b, bc, c]`.

```
SCRATCH incomplete=true capped=true verified=1 got=[a.txt bc.txt] complete=[a.txt b.txt bc.txt c.txt]
SCRATCH got_is_prefix_of_complete=false
```

Root cause: overlay `bc` ranks after unverified base candidate `b`, dropped by
the truncated base page; the merge re-sort cannot know `b` is a potential match.

## 3. Fix options

(a) Truncate the merged page at the base frontier rank.
- Correctness: exact prefix; drop overlay entries ranking at or after the first
  unverified base candidate.
- Cost: low. Expose the frontier rank on the trace; add a cutoff to the merge.
- Files: `search.go`, `search_compact.go`/`content_verify.go` (plumb),
  `types.go` (trace field). Test: the scenario plus a capped-vs-complete prefix
  differential across limits.

(b) Verify overlay entries inside the same rank-ordered frontier.
- Correctness: exact and complete; best result.
- Cost: high. Overlay verification joins the base candidate order and shares
  the disk budget. Files: `search_compact.go`, `content_verify.go`,
  `search.go`. Test: full differential against the uncapped oracle.

(c) Mark such pages incomplete and document "not a prefix".
- Correctness: no prefix guarantee; callers accept a partial union.
- Cost: near zero. Files: docs; optionally `types.go`. Test: assert the flag.

## 4. Recommendation

Option (a). It restores the prefix guarantee cheaply, reuses the existing
frontier, and leaves base-only behavior unchanged. Option (b) is ideal but a
much larger pipeline change; option (c) only codifies the defect.
