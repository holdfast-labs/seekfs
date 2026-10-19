# FRN sequence identity design notes

## Problem

NTFS file references are 64-bit: low 48 = MFT record number, high 16 =
sequence. seekfs keys by the 48-bit number only, so a reused record aliases
its previous occupant and a stale USN event (DELETE/RENAME_OLD) hits the live
record. On 2026-10-18 a replayed WAL deleted `E:\bench\wt` and its subtree this
way. Commit 66f4b78 adds a `baseCheckpoint` floor in `applyUSNChanges`
(`service_replay.go:435`, floor `:463-470`). That fix depends on the watermark;
other stale-event paths stay exposed.

## 1. Where FRNs are produced, stored, compared

- USN parse: `parseUSNRecordFields` (`index_build.go:682-707`) masks the file
  and parent refs via `fileReferenceRecordNumber` (`:723-725`).
- MFT scan: `parseMFTRecord` sets `entry.frn = recordNumber`
  (`mft_windows.go:149-151`); `parseFileName` masks the parent ref (`:227-228`).
  The record header holds the sequence at bytes 16-17; seekfs drops it.
  Owned-dir refs: `config.go:162`.
- `CompactRecord.FRN/ParentFRN` (`types.go:1238-1249`); `PackedRecords.FRNs`
  and `ParentFRNExtras` (`types.go:1251-1266`).
- FRNS section: emit `index_format.go:1064`; `encodeFRNSection` (`:1274-1284`)
  = 8-byte FRN + 4-byte id; decode `index_mmap.go:372`;
  `Index.Derived.FRNs/FRNRecordIDs` (`types.go:411-412`); direct builder
  `direct_topology.go:516-546`.
- WAL/overlay: `usnChange` (`types.go:1311-1318`), `walBatch` (`:1320-1323`),
  binary frames `service_state.go:259-320`, overlay `service_protocol.go:610-620`.
- Lookup: `idForFRN` (`service_runtime.go:681-695`), `addFRNID` (`:697-708`);
  callers `candidate_scans.go:156,190,216,333,380`; path build
  `index_build.go:735+`.
- Content `.gsx` keyed by FRN: `contentDoc.FRN` (`content_index.go:254-274`,
  write `:476`, read `:508`), version 12 (`:123`), `contentLookupFRN` (`:859`),
  `content_resolver.go:71`, `content_service.go:218`,
  `content_catchup_reconcile.go:197-239`, `content_service_wiring.go:819,859`.
- Feature companion: `feature.Record.FRN` (`feature/protocol.go:28`),
  `Response.FRNs` (`:61`), `Version = 1` (`:13`).

## 2. Options

(a) Full 64-bit reference everywhere. Stop masking; key and join by the full
ref. FRNS and content doc FRN slots are already 8 bytes, so no growth; bump
`indexVersion` 9->10 and the content version; add a WAL `Seq` and bump the
magic. Old index fails the gate (rebuild); old WAL magic ignored (frames
already folded); old sidecar rebuilds. ~0 extra memory. Broad blast radius
(paths, children, subtrees, content, feature). Risk: high.

(b) Keep 48-bit keys; persist the sequence beside each record; check on apply.
Add `Seq uint16` to `CompactRecord`/`usnChange` plus a SEQN column. On apply:
older sequence => ignore; newer => new occupant, replace; equal => current.
Bump `indexVersion` 9->10 (+2 B/record); WAL `Seq` + magic bump;
`contentDoc.FRNSeq` + content version bump. Old index loads with Seq 0
(unknown, accept); old WAL ignored; old sidecar rebuilds. ~3 MB persisted +
~3 MB resident per 1.5M records. Files: parse/scan, types, index format, WAL,
apply, content doc. Risk: medium; identity no longer depends on the watermark.

(c) Keep keys; validate at apply with an in-memory side table. No on-disk
change, but the table is not durable, so a post-restart replay cannot validate
a base record: safe only within one scan-to-replay session. ~15 MB resident.
Risk: high (silent on restart).

## 3. Recommendation

Use (b). It fixes identity at apply time independent of the watermark and
leaves every 48-bit-keyed structure unchanged. Cost is one uint16 per record
plus one WAL field. Keep the 66f4b78 floor as defense in depth. (a) is a later
consolidation if the full reference is wanted elsewhere.

Brief 1 - capture and persist the sequence.
- Add `Seq uint16` to `CompactRecord`/`usnChange`; `mftEntry.seq` from
  `record[16:18]`; `parseUSNRecordFields` returns the file-ref sequence.
- New `indexSectionSEQN` + encode/decode + `Index.Derived.FRNSeq`; bump
  `indexVersion` to 10. Add `Seq` to the binary WAL frame; bump the magic.
- Tests: SEQN round-trip; WAL `Seq` round-trip; old-magic frame dropped;
  `parseUSNRecordFields` returns the sequence.

Brief 2 - enforce identity on apply; fix the content join.
- `applyUSNChanges`/`recordOverlayChange`: ignore an event whose sequence is
  below the record's known sequence (base or newest overlay slot) even above
  the floor; a higher sequence is an occupant replacement (tombstone the old
  subtree, then apply).
- Add `FRNSeq` to `contentDoc`; compare in the content joins/catch-up; bump
  `contentIndexVersion` to 13.
- Tests: stale delete with an older sequence ignored even above the watermark;
  new occupant with a newer sequence replaces the old record; an old index
  without sequences loads and works.
- Follow-up brief: bump `feature.Version`, carry the sequence in the companion.
