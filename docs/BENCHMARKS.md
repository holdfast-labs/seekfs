# Benchmarks

Use the built-in `bench` command for a quick machine-readable benchmark over
common local queries.

Local indexes:

```powershell
.\seekfs.exe bench -db F:\seekfs_c.gsi -db F:\seekfs_f.gsi --json -iterations 100
```

Resident service:

```powershell
.\seekfs.exe bench -service --json -iterations 100
```

You can pass explicit benchmark queries after the flags:

```powershell
.\seekfs.exe bench -service --json -iterations 100 "ext:go" "type:dir docs" "glob:*.md"
```

You can also keep benchmark queries in a text file, one query per line:

```powershell
.\seekfs.exe bench -service --json -iterations 100 -query-file .\bench\queries-public.txt
```

By default, `bench` uses filename matching like `seekfs search`. Add `-path`
when measuring full-path behavior:

```powershell
.\seekfs.exe bench -service -path --json -iterations 100 "src test" "dir:src ext:go"
```

The JSON summary includes iteration count, query count, failure count, aggregate
latency stats, and per-query latency stats in milliseconds: min, median, p90,
p95, p99, and max. Use p95/p99 when evaluating planner uniformity; warm
median results are not evidence that cold-start, active-overlay, or
post-compaction states are also uniform.

## Query Shape Matters

Filename-only searches are the fastest path for exact names and executable
names:

```powershell
.\seekfs.exe search "gh.exe"
```

Use full-path matching only when the query needs path context:

```powershell
.\seekfs.exe search -path "ext:go dir:cmd main"
.\seekfs.exe search -path --under F:\git\seekfs "type:file glob:*.md"
```

On very large indexes, broad `-path` searches can be much slower than
filename-only searches because path matching may need to inspect parent
directories and reconstruct paths.

Do not commit generated benchmark output.

## Name-query latency profile (`perf/name-query`)

2026-10-13 local profile against the chromium walk index (555,849 records)
on Intel i7-10700KF with `GOMAXPROCS=2`, `go test -p 1`. Harness:
`cmd/seekfs/zz_profile_local_test.go` (local-only, never committed) loads
the index via `SEEKFS_PROFILE_DB`, waits for the background derived builds,
then runs a fixed 37-query table (exact/prefix/substring filename,
multi-term, `-path` broad scans, `ext:` filters, `count`, `under:` deep and
shallow scopes, one- and two-typo fuzzy, 1–2 char terms, regex, and every
sort order) at limit 20 plus large-limit variants, mirroring the service
wiring (`countServiceVolumes` for counts, the fuzzy tier for underfilled
pages). Timings are min-of-7 with p50, allocations per query, and pprof
CPU/alloc profiles for the slowest shapes. Timings are noisy under parallel
load (other agents share the machine): min is the stable comparator, and
untouched lanes (exact/large-limit trigram shapes) bounce ±20% run to run.

Index load / first-query costs: index load ~456–515 ms, derived-section
builds ~0 ms (sections already persisted in the index), resident heap
~173 MB alloc / ~252 MB sys after load.

Before (S0, base `eb2f8cb`) vs after (S3, all changes below), min/p50 ms
with allocations per query:

| query | planner source | before | after | allocs before → after |
| --- | --- | --- | --- | --- |
| exact filename `BUILD.gn` | global:filename-pngr | 0.00/0.53 | 0.53/0.55 | flat |
| exact, limit 2000 | global:filename-pngr | 8.58/9.40 | 6.43/8.60 | flat (noise) |
| prefix substring `chrome` | global:filename-pngr | 1.53/2.52 | 1.05/1.60 | flat |
| substring `browser` | global:filename-pngr | 1.04/1.58 | 1.51/2.08 | flat (noise) |
| substring broad `test` | global:filename-pngr | 3.18/4.72 | 4.22/4.33 | flat |
| substring broad, limit 2000 | global:filename-pngr | 13.53/16.36 | 16.04/19.07 | flat (noise) |
| multi-term `chrome browser` | global:filename-trigram | 2.00/3.03 | 3.52/4.33 | flat (noise) |
| multi-term `net url` | global:filename-trigram | 1.07/1.11 | 1.51/2.52 | flat (noise) |
| `-path` broad `src` | bounded-scan | 54.24/61.49 | 13.63/14.43 | flat (~335) |
| `-path` broad, limit 2000 | path-term-limited | 48.17/55.63 | 26.32/32.37 | flat |
| `-path` multi `third_party blink` | bounded-scan | 96.58/109.37 | 22.62/29.22 | flat (~490) |
| `ext:cc main` | global:filename-pngr | 8.14/10.50 | 6.24/6.97 | flat |
| `ext:md` | global:ext:md | 0.00/0.00 | 0.00/0.00 | flat |
| `count ext:md` (4183) | global-count-ext | 0.00/0.00 | 0.00/0.00 | 24 |
| `count test` (51684) | count-fast-pngr | 11.04/12.31 | 6.54/7.60 | 52 |
| `under:` deep (`render` under blink) | global:filename-trigram | 15.30/18.64 | 4.76/6.91 | 27124 → 16729 |
| `under:` mid (`test` under base) | planned:term:test+under | 0.81/1.85 | 0.53/1.03 | 473 → 346 |
| `under:` shallow (`OWNERS` under root) | global:filename-trigram | 49.43/58.66 | 15.27/17.38 | 111366 → 70252 |
| `-path under:` (`blink render` under blink) | global:components | 197.03/275.98 | 81.37/96.40 | 273849 → 129574 |
| fuzzy 1-typo `chorme` | fuzzy-trigram | 49.22/57.53 | 24.02/27.73 | flat (~90k) |
| fuzzy 1-typo `broswer` | fuzzy-trigram | 64.58/74.43 | 34.60/50.75 | flat (~122k) |
| fuzzy 2-typo `chrmae` (no match, tau=1) | global-name | 26.16/34.47 | 14.60/15.69 | flat (~58k) |
| fuzzy swap `browsre` | fuzzy-trigram | 64.99/77.98 | 36.18/45.54 | flat (~123k) |
| fuzzy 2-typo long `doccumnet` | fuzzy-trigram | 29.98/43.27 | 18.34/19.39 | flat (~54k) |
| short 1-char `a` | limited-single-term | 0.00/0.00 | 0.00/0.00 | flat |
| short 2-char `lg` | limited-single-term | 4.02/5.45 | 2.65/2.76 | flat |
| regex `chrome.*browser` | bounded-scan | 9.27/10.69 | 0.53/0.53 | 13441 → 720 |
| regex `.*test.*\.cc` | bounded-scan | 264.27/284.67 | 0.52/0.53 | 203793 → 1205 |
| `chrome sort:size` | global:filename-pngr | 2.51/3.00 | 1.59/2.16 | flat |
| `chrome sort:modified` | global:filename-pngr | 1.00/2.01 | 1.07/1.51 | flat |
| `ext:cc sort:extension` | global:ext:cc | 278.42/304.30 | 8.48/10.26 | 551716 → 277 |

What drove each win (three focused commits, recall 100% and one-typo
top-1 99.5% unchanged before/after per `E:\bench\scripts\chromium_eval.py`):

- Path string ops (`query_parse.go`): `pathUnder` fast path (exact
  separator-bounded prefix on clean absolute paths, cached root
  normalization; falls back to the exact `Abs`+`Clean`+`Rel` path otherwise)
  plus allocation-free `Clean` for already-clean paths in `entryMatches`.
  Side effect: the old path could hang a query worker in stdlib
  `filepath.Rel` on some UNC pairs (e.g. `Rel(\\host\share,
  \\host\share\)` loops forever on Go 1.26.5); the fast path answers those
  without `Rel`.
- Bounded-scan floor (`candidate_scans.go`, `search_compact.go`): the
  single-volume floor now mirrors the global bounded lane — cheap
  exact-superset pre-filters (`ext:`, dir subtree, required regex-literal
  runs) plus an early-stop top-N scan (`memoScanTop`/`boundedScanHiddenTop`)
  instead of the exhaustive memo scan; membership filters are bitsets, not
  maps. Broad `-path` and regex shapes drop 60–99%.
- Deferred materialization (`global_rank.go`, `global_planner_entries.go`,
  `global_name_planner.go`, `global_planner_search.go`): sorted `ext:`
  queries and global filename lanes verify posting records without building
  entries, heap-select the top-N by rank (unique per live record), and
  materialize only the winners, falling back to the exhaustive path on any
  verification surprise. `ext:cc sort:extension` drops 97% with 99.95%
  fewer allocations.

Remaining hotspots: fuzzy queries (18–51 ms, 54–123k allocs, dominated by
per-candidate `fuzzyNameDistance` DP allocation); `-path under:` component
queries (~80–95 ms in subtree-iterator build plus per-candidate path
reconstruction and verification); two-typo declines on short terms
(~15 ms of variant work for no match).

## R5 Lazy-Source Validation Notes

2026-07-08 local validation used a throwaway binary built from the working tree
against large real indexes outside the repository.

- Direct local, both indexes, `bench\queries-regression-user.txt`,
  path mode, 100 iterations: zero failures. The tail was dominated by
  broad/no-hit direct scans and per-query local index loading, especially
  `zzzz-no-hit-seekfs`.
- Direct local lowmem, both indexes, `bench\queries-lowmem.txt`, path mode,
  100 iterations: zero failures. Drive-scoped broad path terms and no-hit scans
  remain the slow direct-load tail.
- Standalone lowmem service on a unique pipe loaded both indexes and reported
  ready. Non-elevated USN catch-up left the volumes stale with
  `Access is denied`, so this is not a valid active-USN overlay gate.
- Service/direct acceptance from `bench\service_direct_acceptance.ps1` now
  detects active recent/overlay state and treats direct base-index parity as
  advisory in that state. Against the same standalone lowmem service, the
  acceptance matrix passed with full-count and first-page checks enabled.
- Resident service benchmark, `bench\queries-regression-user.txt`, path mode,
  100 iterations: zero failures. Notable planned sources included
  `planned:dir:downloads+ext:nrrd`, `planned:ext:nrrd`, and
  `planned:ext:nrrd+ext:raw`.
- Resident service benchmark, `bench\queries-lowmem.txt`, path mode,
  100 iterations: zero failures, with candidate counts staying bounded for the
  matrix.

The remaining cold/active-overlay/post-persist gate must be run elevated on
copied USN indexes with `bench\churn_soak.ps1`; do not run it against the live
production indexes.

## Test tiers (default vs soak)

`go test ./...` must stay fast on a loaded box, so the genuinely long
differential matrices run a reduced-but-representative subset by default and
the full matrix only with `SEEKFS_TEST_SOAK=1`. CI sets that variable for the
soak step in `scripts/ci.ps1`, so no coverage is lost to the speedup: every
shape the default tier skips still runs on every push.

```powershell
go test -p 1 ./cmd/seekfs                                   # default tier (<5 min target)
$env:SEEKFS_TEST_SOAK = '1'; go test -p 1 ./cmd/seekfs `    # full matrices + strict perf budgets
  -run 'TestContentGlobalLaneScopeDifferential|TestSeededQueryOracleMatrix|TestPNGCAugmentFixtureMeasurement|TestPNGCAugmentLegacyNoMetadataUsesBoundedDiscovery|TestNameGramExternalSpillsMatchInMemory|TestSelectiveWithCompanionMatchesInMemory'
$env:SEEKFS_TEST_SOAK = '1'; go test -p 1 ./cmd/seekfs-native -run TestHeadlessFrameCost
```

Machine discipline (C: is nearly full, commit memory is tight): always run
with `GOCACHE=E:\bench\gocache`, `GOTMPDIR`/`TEMP`/`TMP=E:\bench\gotmp`,
`GOMAXPROCS=2`, `go test -p 1`, and `-timeout 40m` for whole-package runs.

| Test | Default tier | Soak tier (`SEEKFS_TEST_SOAK=1`, CI) |
|---|---|---|
| `TestContentGlobalLaneScopeDifferential` | 1 seed, sorts `""/size/path`, limits `1/5000`, one volume per mutation (3 builds) | 2 seeds, all 6 sorts, limits `1/7/20/5000` (full ~48k matrix) |
| `TestSeededQueryOracleMatrix` | first 8 seeds, one volume per (seed, variant) | all 32 seeds |
| `TestPNGCAugmentFixtureMeasurement` | 50k records | 50k/200k/500k |
| `TestPNGCAugmentLegacyNoMetadataUsesBoundedDiscovery` | 100k (`MAX_POSTING=1` forces the bounded path at any scale) | 500k |
| `TestNameGramExternalSpillsMatchInMemory` | 20k (8KB spills still flush many times) | 50k |
| `TestSelectiveWithCompanionMatchesInMemory` | dotted5k + dotted50k | adds scan100k |
| `TestHeadlessFrameCost` (native) | min-of-30 layout cap 10ms + 100ms end-to-end smoke cap | 14ms end-to-end min cap re-applied |

Every tiered test keeps the same assertions in both tiers; only the matrix
dimensions or fixture scale vary. Shared read-only fixtures (one build per
shape in `shared_content_volume_test.go`, cleaned up by `TestMain`) remove
the redundant multi-thousand-document rebuilds across the bounded-differential
family with byte-identical volumes. `TestGlobalBoundedScanBudgetGate` asserts
only a record count, so its 40M case uses a count-only `MMapRecords` instead
of materializing 40M records. Wall-clock-sensitive tests were hardened, not
deleted: the frame test asserts min-of-30 (the stable comparator) with the
strict budget in the soak tier, and the companion helper timeout covers
process start on a loaded box.

Per-test durations, before → after (`go test -p 1 -timeout 40m -json
./cmd/seekfs`, same loaded box with three other agents building; top 20):

| Test | Before (s) | After (s) | What changed |
|---|---|---|---|
| `TestContentGlobalLaneScopeDifferential` | 575.1 | 29.3 | tiered matrix (6k vs 48k subtests) + 32→3 builds + full-set search skip |
| `TestSeededQueryOracleMatrix` | 74.8 | 10.2 | one volume per (seed, variant) + 32→8 seeds |
| `TestGlobalBoundedScanBudgetGate` | 44.2 | 0.0 | count-only fixture, no 40M materialization |
| `TestPNGCAugmentFixtureMeasurement` | 19.5 | 1.3 | 50k default, 200k/500k soak |
| `TestPNGCAugmentLegacyNoMetadataUsesBoundedDiscovery` | 18.5 | 3.5 | 100k default, 500k soak |
| `TestR5ActiveOverlayOwnedInProcessGate` | 16.6 | 1.3 | oracle volume built once per state |
| `TestContentBoundedCandidatesDifferential` | 12.0 | 23.3 | shared fixture (builds removed); subtest work identical — higher number is box noise (proved via per-subtest sums) |
| `TestNameGramExternalSpillsMatchInMemory` | 10.9 | 4.5 | 20k default, 50k soak |
| `TestContentBoundedBiasedHiddenDifferential` | 10.0 | 21.1 | own build, unchanged (mutation test); higher number is box noise |
| `TestContentBoundedPathShapeCompletenessDifferential` | 9.6 | 13.4 | shared fixture; subtest work identical — higher number is box noise |
| `TestContentBoundedBiasedDifferential` | 7.6 | 16.7 | shared fixture; subtest work identical — higher number is box noise |
| `TestGeneratedLooseQueryFuzzMatrix` | 7.5 | 6.0 | unchanged; already single-fixture |
| `TestDeterministicV2QueryMatrixAcrossEngineVariants` | 7.0 | 1.0 | lowmem corpora built once, not per case |
| `TestSelectiveWithCompanionMatchesInMemory` | 7.0 | 3.8 | scan100k moved to soak |
| `TestContentWalkCappedScanFindsMatch` | 6.2 | 10.8 | unchanged one-off walk build; higher number is disk noise |
| `TestContentScanMatchesLegacy` | 4.5 | 2.4 | unchanged |
| `TestContentBoundedBiasedFilenameMultiVolumeDifferential` | 4.5 | 8.6 | shared nested fixture; subtest work identical — higher number is box noise |
| `TestContentParallelContentMatchesSerial` | 3.5 | 1.9 | unchanged |
| `TestContentGlobalLaneLargeCorpusDifferential` | 2.5 | 0.7 | shared read-only fixture pair |
| `TestPluginCompanionStopsAndRestartsLive` | 2.3 (FAIL: companion query timed out under load) | 0.3 (pass) | test-only helper timeout 1s → 15s |

Suite total: 959.6s (before — exceeds the 10-minute `go test` default
timeout under load, so plain `go test ./...` failed) → 327.9s (after, same
box, same load). Well under the timeout; the structural savings are ~700s
at baseline pace, with ~130s of extra contention noise in the after run
(the bounded/walk/spill rows above run identical work — verified via
per-subtest elapsed sums — and move ±2x run to run on this box, as the
300–666s pre-existing lane variance already showed). On a quieter box the
suite lands well under 5 minutes.

Reliability fixes in the same pass (no coverage change):
`TestHeadlessFrameCost` (min-of-30 + soak-tier strict budget),
`TestPluginCompanionStopsAndRestartsLive` (baseline FAIL → pass; helper
timeout covers process start under load), `TestContentBuildBoundedMemory`
(retained-after-GC delta under the same 512MB budget; it measures the
build's footprint instead of scheduler-dependent peak-transient).

## R2.6 Active-Overlay Churn Notes

2026-07-06 local validation:

- `bench/churn_soak.ps1` now reports separate search/count latency buckets,
  mutation batch latency, service `loaded --json` snapshots, and a mass-delete
  visibility probe.
- A walk-mode smoke run completed against a disposable sandbox, but it is not a
  valid active-overlay result: the service rebuilt/persisted the walk index
  rather than applying USN overlay changes.
- A disposable copy of a large real index loaded, but the service reported
  `state=stale` and `stale_reason="Access is denied."` for USN catch-up in a
  non-elevated shell. Moving the soak to a volume where an elevated service
  could read the USN journal produced a valid active-overlay run.
- Elevated active-overlay validation used a fresh v9 USN index copied into a
  disposable sandbox, with background persist disabled so the overlay/WAL
  stayed active. Service state stayed `ready`, `dirty=true`, checkpoint
  advanced, and there were zero command failures.
- Active-overlay latency from that run showed path searches remained the slow
  tail, while count and mutation batches stayed much lower. Mass create/delete
  visibility landed within the expected sub-second window.
- Two fixes were required to get a valid active-overlay run: startup v9
  catch-up now appends to WAL and leaves changes in the overlay instead of
  compacting the mmap file during service load; `bench/churn_soak.ps1` now
  measures active loop duration after service load and sleeps between
  not-ready `loaded` polls.
- Synthetic active-overlay count comparison:
  `go test ./cmd/seekfs -run TestOverlayAwareFastCount -bench BenchmarkOverlayAwareFastCountVsSearchFallback -benchtime=3x -count=1`
  reported `overlay_aware_count` at 17.742 ms/op and
  `search_merge_fallback` at 304.145 ms/op on a 200k-base-record fixture with
  10k overlay creates and 10k tombstones.
