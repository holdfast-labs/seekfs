# fix/usn-frn-sequence — notes

## Findings (hypothesis check)

- The USN parser drops the 16-bit sequence: `fileReferenceRecordNumber` masks to
  the low 48 bits (`index_build.go:723`), and the same mask is used by
  `parseUSNRecordFields` (`index_build.go:688-699`), `parseFileName`
  (`mft_windows.go:228`) and `fileReferenceFromFileID` (`config.go:162`). Every
  record is keyed by MFT record number only, so an event for a previous occupant
  of a reused record is indistinguishable from one for the current occupant.
- `recordOverlayChange`'s delete branch (`service_replay.go:608-653`) tombstones
  the current base record for that number and cascades the whole subtree
  (`tombstoneBaseSubtree` / `cascadeOverlayDelete`), detaching every descendant.
- The hypothesis holds in this form: pre-snapshot/target events are re-applied
  with no per-record floor. `applyUSNChanges` (`service_replay.go:435`) applies
  every change it is handed; catch-up (`service_runtime.go:647`) and WAL replay
  (`service_state.go:62`) gate only on the batch/head USN, never on each change's
  USN against the fresh scan's watermark (`idx.Checkpoint = journal.NextUsn`,
  `index_build.go:277`). A stale DELETE at/below that watermark therefore
  deletes the live record after a rescan.
- The smallest safe fix (a): never apply a change whose USN is at or below the
  volume's persisted snapshot watermark (`baseCheckpoint`) — those events are
  already reflected in the base records. Sequence-aware identity (b) needs the
  sequence stored per record, which is a format change, so it is deferred.
- Implemented in `applyUSNChanges` (`service_replay.go`): a per-change floor
  `change.USN > 0 && floor > 0 && change.USN <= floor` skips records at or below
  the snapshot watermark before they reach the overlay/tombstone path. The
  reproducing test is `TestApplyUSNStaleDeleteForReusedRecordKeepsLiveSubtree`.
