# Save-time peak (content/save-peak)

Branch `content/save-peak` from integ `2fbab97`. Work item: docs/WORK_QUEUE.md
"Save-time peak", docs/CONTENT_SEARCH.md section 6x "Residual".

## Phase 1: measurement (no product code change)

Fixture: `TestContentSavePeakMeasure` in `cmd/seekfs/content_save_peak_test.go`.
It assembles one synthetic shard (240 docs, pseudo-random word text), samples
the bytes on disk in the assembly scratch directory and the shard output
directory in a tight loop through `contentSaveShard`, and reports before,
peak, and after. Text and section sizes are deterministic.

Measured (GOMAXPROCS=2):

| quantity | bytes | x extracted | x encoded |
|---|---:|---:|---:|
| extracted text | 3,788,428 | 1.000 | 2.117 |
| scratch before save | 2,264,625 | 0.598 | 1.265 |
| encoded shard | 1,789,747 | 0.472 | 1.000 |
| peak during save | 4,054,372 | 1.070 | 2.265 |
| after save | 1,789,747 | 0.472 | 1.000 |

The measured peak equals `scratch + encoded` (4,054,372 bytes): the save keeps
every section file while the encoded temp output grows beside it, so the
save-time transient is the full duplication of the section data.

## Is the transient counted?

Partly, but not as a sum:

- Selection budgets extracted bytes at `contentScopePeakMultiplier = 0.8`
  (`cmd/seekfs/content_space.go:81`). This projects spool + output as a mix,
  not scratch + output.
- Selection also subtracts `contentScopeScratchSlack()` =
  `contentShardBuildConcurrency * contentShardTargetBytes`
  (`cmd/seekfs/content_space.go:134-135`) in `contentScopeHeadroom`
  (`cmd/seekfs/content_space.go:169`), and stops at
  `n > allowance-used` (`cmd/seekfs/content_service_build.go:995`), where the
  allowance comes from `contentScopeAllowance` (`content_service_build.go:898`)
  or `contentScopeAllowanceWithMultiplier` (`content_service_build.go:992`).
  The slack covers the scratch files of in-flight shards only while each
  shard is at or below the target.
- The write-time guard `contentCheckDiskReserve`
  (`cmd/seekfs/content_space.go:280`) compares free space with the reserve
  only. It runs before each shard save (`content_service_build.go:1155` and
  `:1181`) and on a byte cadence during extraction (`:1011`), never during a
  save.
- `contentSaveShard` (`cmd/seekfs/content_shards.go:397`) calls
  `contentSaveAssembled` (`cmd/seekfs/content_index.go:988`), which collects
  the section files with their sizes, copies each payload into the temp output
  (`content_index.go:1015`), and renames it (`:1057`). No section file is
  removed during the copy, so scratch and output coexist until the rename.

Conclusion: the encoded output at save time is covered only by the 0.8x
projection (as a spool+output mix), and the section scratch only by the fixed
slack; the *sum* is not accounted. For target-sized shards the slack absorbs
it. For an oversize shard (one folder unit past the target) or a tight
reserve, the sum exceeds the accounted bound. This matches the documented
residual in docs/CONTENT_SEARCH.md section 6x.
