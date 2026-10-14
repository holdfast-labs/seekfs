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

Remaining hotspots (after `perf/name-fuzzy` below): fuzzy queries
(7–16 ms, ~0.2–0.9k allocs, now dominated by DP compute over trigram
candidates plus posting expansion); `-path under:` component queries
(~19–31 ms, ~2k allocs, now dominated by the scoped term-side build plus
rank-ordered verification); short-term declines (~8 ms of posting + DP
compute for no match, allocation-free).

## Name-fuzzy follow-up (`perf/name-fuzzy`)

2026-10-13 local profile, same chromium walk index (555,849 records),
`GOMAXPROCS=2`, same harness. Before (S3, `perf/name-query` end) vs after
(S4, all changes below), min/p50 ms with allocations per query (fixed
37-query table; untouched lanes bounce ±20% run to run under parallel
load, same as before):

| query | before | after | allocs before → after |
| --- | --- | --- | --- |
| fuzzy 1-typo `chorme` | 24.02/27.73 | 8.97/10.15 | ~90k → ~0.5k |
| fuzzy 1-typo `broswer` | 34.60/50.75 | 15.84/16.62 | ~122k → ~0.6k |
| fuzzy 2-typo `chrmae` (no match, tau=1) | 14.60/15.69 | 7.40/8.59 | ~58k → ~0.3k |
| fuzzy swap `browsre` | 36.18/45.54 | 13.36/14.95 | ~123k → ~0.6k |
| fuzzy 2-typo long `doccumnet` | 18.34/19.39 | 7.46/8.19 | ~54k → ~0.9k |
| `-path under:` (`blink render` under blink) | 81.37/96.40 | 18.69/20.85 | ~129k → ~2k |
| `-path under:` (`core frame` under renderer) | 8.74/10.21 | 3.01/3.52 | ~4.9k → ~0.4k |

Untouched lanes (exact/trigram/`-path` broad/regex/sort/count) measure
flat within the usual noise. Recall 100% and typo top-1 (one-typo 99.5%
N=200 seed 42, two-typo 93.0% N=100 seed 7, per-query identical before
and after) unchanged per `E:\bench\scripts\chromium_eval.py`; result
parity old-vs-new over `parity.py` (50 queries × search 20/300/count):
0 differences (one intermediate revision differed on fuzzy `unitest`
top-N selection and was fixed before merge — see below).

What drove each win (four focused commits):

- Distance buffer reuse (`fuzzy.go`): one `fuzzyDistanceScratch` per
  query threads reusable DP rows plus the rune decode buffer through
  every candidate; ASCII names run a byte DP with no rune conversion;
  names shorter than `len(term)-tau` decline before any DP; the
  persisted lower name replaces the per-candidate `ToLower` copy; the
  whole-name and window checks reuse the same scratch.
- Cheaper verifier + parallel scoring (`fuzzy.go`): triangular-banded
  semi-global DP (columns `j < i-tau` provably exceed tau; validated
  against the unbanded window oracle on 5000 random larger inputs),
  fused letter-mask/ASCII scan, and chunked parallel candidate scoring
  with ordered merge (identical output by construction, pinned by a
  serial-vs-parallel test).
- Lazy materialization (`fuzzy.go`, global components lane
  `global_rank.go`): fuzzy survivors sort by path-free keys and only
  winners build entries; the components top-N verifies iterator
  candidates at the record level (scalar precheck plus parent-chain
  term/dir walk, a strict superset of the entry gate), sorts
  lightweight `(id, rank)` pairs, then materializes entries in rank
  order with path-tied groups and the exact `entryMatches` gate.
- Under-scoped iterators (`global_query_iterators.go`): each probe
  term's component roots are partitioned against the under intervals
  (inside/ancestor/outside); satisfied terms (covering every under
  root) drop out of the conjunction, in-scope roots plus
  scope-filtered name hits drive it, so the 225k-record blink subtree
  is never materialized when a term side is smaller. Any doubt
  declines to the legacy assembly.
- Parity fix (`fuzzy.go`): the collection cap (`need*fuzzyCollectFactor`)
  is checked only at variant boundaries like the original loop — the
  in-flight variant always over-collects past the cap and that tail
  participates in the final sort. Stopping mid-variant changed fuzzy
  top-N selection (`unitest`); pinned by
  `TestAppendFuzzyCollectsPastEnoughBoundary`.

Tried and rejected: whole-name prefix/suffix and bigram-presence
pre-filters for piece-driven decline candidates (unsound against
mid-name windows, or vacuous on `chr*` names that already share the
piece bigrams); pair-gram retrieval tightening for short variants
(unnecessary once the verifier went allocation-free); capping DP
verifications (cannot prove emptiness — recall risk); Myers
bit-parallel distance (no transposition support — recall risk on swaps).

## Right file first (`perf/ranking-signals`, G13)

2026-10-14 local benchmark against the chromium walk index (555,849 records),
standalone service on a private pipe (`seekfs service -pipe
\\.\pipe\seekfs-ranking -db E:\bench\chromium.gsi`), `GOMAXPROCS=2`. Harness:
`E:\bench\scratch\ranking\rightfile.py` (scratch-only, never committed)
samples 100 labeled targets from `E:\bench\truth.tsv` (60 whose basename
occurs ≥ 2×, deliberately not the alphabetically-first copy, plus 40 unique
names; seed 20261014) and issues, per target at `-n 100`: a plain-name query
(basename), a partial-name query (longest stem token ≥ 4 chars, else first 6
of stem), a 1-typo and a 2-typo query (mutations as in
`E:\bench\scripts\chromium_eval.py`). Top-1/top-5 mean the target PATH ranks
first / in the first five — stricter than the typo table below, which scores
the target NAME first.

EMPTY runs with a fresh signals state; SEEDED runs after `seekfs open` on
every other target (4× each with `--query <basename>`, so the ≥ 3 combo
boost fires, plus partial-query opens for a quarter of targets):

| query shape | n | EMPTY top-1/top-5 | SEEDED top-1/top-5 |
| --- | --- | --- | --- |
| plain name | 100 | 39.0% / 95.0% | 66.0% / 95.0% |
| partial name | 100 | 2.0% / 12.0% | 13.0% / 21.0% |
| 1-typo | 100 | 39.0% / 98.0% | 39.0% / 98.0% |
| 2-typo | 96 | 38.5% / 92.7% | 38.5% / 92.7% |

Opened targets (n=50): plain top-1 20 → 47, 27 rose; partial top-1 2 → 13,
17 rose. Control targets (n=50, never opened): plain 19 → 19, partial 0 → 0 —
no blast radius (pages without signal evidence return untouched). Typo
queries keep distance order by design (fuzzy bands never invert), so usage
neither helps nor hurts them here.

No-regress check on the committed typo metric (name-level, per
`chromium_eval.py`, EMPTY store): one-typo top-1 99.5% (N=200 seed 42) and
two-typo top-1 93.0% (N=100 seed 7) — exactly the Table 1 baselines.

Result-set parity vs the pre-signals build (`parity.py`, 50 queries ×
search 20/300/count over the same index, empty signals): 147/150 identical;
the 3 diffs are all the `case:Makefile` error case, where the base exits 0
with no output (the old silent-empty bug) and the new build exits 1 with the
rejection message — the pre-merged `fix/cli-silent-empty` behavior, not a
ranking change. All 140 non-empty base results (search pages and counts) are
identical including order.

Latency: the 37-query name profile re-run on the signals build with an
empty store lands within the box's run-to-run noise (repeat runs of the same
binary swing ±40% under parallel-agent load, e.g. path-multi 32.08 →
21.84 ms, sort-extension 13.45 → 8.50 ms; per-query allocations identical).
Active-path cost is bounded by `BenchmarkSignalReorderPage`: ~112 µs for a
100-entry page with half the hits carrying frecency evidence (~1 µs/hit +
page sort); the empty-store gate is a few map lookups.

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
