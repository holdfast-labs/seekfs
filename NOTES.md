# Drive-root reconcile investigation (fix/content-drive-reconcile)

- Root cause: the scope reconcile holds NEW files to the build's transient
  reserve allowance (`contentReconcileNewLimits` in content_reconcile_budget.go),
  so on a volume whose free space is below the 10 GiB reserve it trims almost
  every file the new whole-volume auto root admits and still adopts the new
  fingerprint. The live E: log proves it:
  `content scope reconcile volume=E: left out 327669 files (215066008567 bytes)
  past the budget` then `content scope reconcile volume=E: enqueue=29567 drop=0`
  (later 272112 left out, enqueue=15673), matching the flat 640665 -> 690868
  doc growth. `seekfs loaded` keeps root=400173 and no dependency key because a
  reconcile never rebuilds the CXSE/CXIS selection stats; only a build or fold
  rewrites them, and the fold keeps base.Inclusion.
- The gate was added for a reserve-truncated base (real E:, 2026-10-13:
  41,724-doc base, 925,450 enqueued, rebuild loop). Its header already records
  the truth: `Policy.ReserveBytes > 0` iff the reserve actually bit the build,
  `Policy.ScopeDropped` the exact files dropped.
- Selected rules, scan, estimate, and fingerprint all agree: the fast parallel
  scan admits exactly the legacy allows() set on a large synthetic drive index
  (legacy=39997 fast=39997), and the drive root is in `Roots` (5 roots).
- Fix: apply the reserve text allowance only when the attached base was itself
  reserve-truncated; a base that fit must not drop newly admitted files because
  the disk filled later. Down-stream drain additions carry no whole-corpus spool
  peak, and the fold keeps `contentCheckDiskReserve` as the write-time guard.
- Also bump `contentScopeSelectionVersion` 6 -> 7 because the built-in rules
  changed (whole-volume auto root + outside-repo dependency/binary skips).
