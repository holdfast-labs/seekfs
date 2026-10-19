# Phantom orphans: live E: index investigation

## Harness findings

A gated local harness (`zz_phantom_local_test.go`, `SEEKFS_PHANTOM_LOCAL=1`)
loaded `E:\bench\scratch\phantom\seekfs_e.gsi`, built the service volume and
replayed `seekfs_e.gsi.wal`. Quoted output (trimmed):

```
base: volume="E:" source="usn" records=1511631 checkpoint=50627676320 baseCheckpoint=50627676320 compact=true
after replay: checkpoint=50631446720 overlay=57954 tombstone=3 shadowed=8
roots=["E:\\"] derived.FRNs=1511630 derived.FRNRecordIDs=1511630 frns=1511630 frnToID=0 derived.FRNSeq=1511631 childOffsets=1511632 childIDs=1510255 subtreeOrder=1511631
base records with nonzero Seq=3440 of 1511631
search FRN 0xc00000016e7e4 (low 0x16e7e4):
search FRN 0x12b000000162023 (low 0x162023):
search FRN 0x62000000170b52 (low 0x170b52):
search FRN 0xa00000016fce2 (low 0x16fce2): derived[1504053]=frn=0x16fce2 id=1495130 name=".git" path="E:\\.git";;
search FRN 0x44000000162024 (low 0x162024): derived[1447829]=frn=0x162024 id=1442049 name=".git" path="E:\\.git";;
search FRN 0x69000000170b53 (low 0x170b53): derived[1507463]=frn=0x170b53 id=1507442 name=".git" path="E:\\.git";;
record id=1495130 name=".git" frn=0x16fce2 parentFRN=0x16e7e4 parentID=-1 parentName=".git" seq=0x0 deleted=false mode=0x22 path="E:\\.git"
record id=1442049 name=".git" frn=0x162024 parentFRN=0x162023 parentID=-1 parentName=".git" seq=0x0 deleted=false mode=0x22 path="E:\\.git"
record id=1507442 name=".git" frn=0x170b53 parentFRN=0x170b52 parentID=-1 parentName=".git" seq=0x0 deleted=false mode=0x22 path="E:\\.git"
children of missing parent 0x16e7e4: 23
  id=1495913 name="go.mod" frn=0x1700dd parentFRN=0x16e7e4 seq=0x0 deleted=false
  id=1495206 name="cmd" frn=0x16fd75 parentFRN=0x16e7e4 seq=0x0 deleted=false
  ... 21 more (docs, examples, feature, go.sum, media, scripts, ...)
children of missing parent 0x162023: 16
children of missing parent 0x170b52: 20
.git base live=150 root-level=0
overlay live records re-rooted at volume root=0
subtree self-test id=1436424 hasRange=true start=0 end=1511631 tombstone 3 -> 1511631 (+1511628)
```

Interpretation:

- All three worktree **directory records are absent from the base** (no
  record for FRN 0x16e7e4, 0x162023, 0x170b52), but their **entire child sets
  survive**: 23, 16 and 20 children, including `.git`, `go.mod` and `cmd`.
  The children carry `ParentFRN` to the missing directory, `Parent = -1`.
- The lost records are **not** tombstoned and are **not** in the overlay: the
  current replay tombstone/shadow sets do not cover them. They were dropped
  by an earlier fold, one record at a time (the children survive, so no
  subtree cascade ran).
- All base records for these trees have `Seq = 0` (only 3,440 of 1,511,631
  base records carry a sequence). This is the v9 -> v10 load: the sequence
  was never captured in the base, so sequence identity is unknown for the
  replayed events.
- The `.git` children have `Parent = -1` with a nonzero `ParentFRN`, and
  `reconstructCompactPathCached` terminates the chain at `parent < 0` and
  then prefixes the volume root. That is the phantom `E:\.git`.

## Root cause

The persist swap carries the post-snapshot overlay to the replacement volume
with `carryOverlayDelta`. Overlay tombstone and shadow sets are **base record
IDs**. The fold that runs while replay continues removes base records, so the
replacement base has a different ID space. `carryOverlayDelta` copies the
post-snapshot ID deltas unchanged into that new ID space.

The stale IDs then name unrelated records in the replacement base. The next
fold skips those records (a stale tombstone/shadow is treated as a hide), so
they disappear without a cascade: exactly one directory record each, children
untouched. The same stale IDs also keep unrelated records hidden while the
process runs.

The path builder then turns the newly orphaned children into root-level paths
(`E:\.git`), because `reconstructCompactPathCached` and `overlayRecordPath`
treat "parent id not found" as "child of the volume root".

## Fixes

1. Remap the carried tombstone/shadow base IDs from the old base ID space to
   the replacement base by FRN (`remapOverlayDerivedIDs`) before the swap.
2. Treat an unresolvable nonzero parent FRN as an orphan: return an empty path
   instead of a volume-root path, and skip empty-path entries in results.

## Tests

- `TestPersistOverlayCarryRemapsBaseIDsAfterFold` (fails before the fix: the
  stale IDs hide `innocent` and `keep.txt` and let the deleted worktree
  subtree survive).
- `TestOrphanParentDoesNotResolveToVolumeRoot` (fails before the fix: the
  orphan directory and `.git` child return paths).
- `TestPersistOverlayCarryKeepsPostSnapshotChanges` (existing carry test,
  updated for the new signature).

After the fix the harness prints `path=""` for all three orphaned `.git`
records and still prints `overlay live records re-rooted at volume root=0`;
`reconstructCompactPathCached` no longer invents `E:\.git`.
