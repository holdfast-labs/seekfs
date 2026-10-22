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

## Content right-file-first (`bench/content-rightfile`)

2026-10-16 local benchmark of the CONTENT pipeline's ability to put the
defining file of a symbol first. Harness: a scratch-only Go test in
`cmd/seekfs/zz_rightfile_local_test.go` (never committed), run with
`GOMAXPROCS=2`. It builds one content index over the corpus through the same
production service path as the build-profile harness (walkRoot + saveIndex +
loadIndexForService + ensureContentBuild, then waits for the ready state) and
reuses it for every label; per label it calls `searchServiceVolumes` with
`Limit 20` for four query forms and records the 1-based rank of the defining
file (0 = not in the top 20).

Corpus: `E:\bench\chromium\third_party\blink\renderer\core` (~9.1k files,
Defender-warm). Labels (100): C++ symbols defined in exactly one corpus file
and referenced (whole-word) in ≥ 3 other files — 60 classes/structs defined in
a `.h` (`^(class|struct) (CORE_EXPORT )?Name\b[^;]*$`) and 40 out-of-line
functions defined in a `.cc` (`^\S.*\bClass::Name\(`, first line ending in
`{`). A function is eligible only when exactly one qualified `Class::Name`
exists corpus-wide, and its query is the bare identifier so `sym:` accepts it.
Selection is deterministic (sorted by name, seed-42 shuffle). Ground truth is
the defining file.

Four forms per label: (a) `content:<sym>`, (b) `content:<sym>
sort:relevance` (definition-line bonus), (c) `sym:<sym>` (definition search),
(d) `<sym>` (plain filename query, reference baseline). Rates are of the
defining file ranking top-1 / top-5 / top-20; MRR is the mean reciprocal rank
(0 for a miss).

Baseline (integ ac8750d) vs symbol-named candidates first (191148d):

| form | kind | top-1 before | top-1 after | MRR before | MRR after |
| --- | --- | --- | --- | --- | --- |
| `content:<sym>` | overall | 17.0% | 70.0% | 0.342 | 0.771 |
| `content:<sym> sort:relevance` | overall | 63.0% | 86.0% | 0.788 | 0.923 |
| `sym:<sym>` | overall | 59.0% | 92.0% | 0.776 | 0.955 |
| `<sym>` (filename) | overall | 0.0% | 0.0% | 0.002 | 0.002 |
| `content:<sym>` | class | 0.0% | 88.3% | 0.209 | 0.924 |
| `content:<sym> sort:relevance` | class | 56.7% | 95.0% | 0.750 | 0.975 |
| `sym:<sym>` | class | 40.0% | 95.0% | 0.678 | 0.975 |
| `content:<sym>` | function | 42.5% | 42.5% | 0.542 | 0.542 |
| `content:<sym> sort:relevance` | function | 72.5% | 72.5% | 0.845 | 0.845 |
| `sym:<sym>` | function | 87.5% | 87.5% | 0.924 | 0.924 |

After: top-5 is 90% (default), 99% (relevance), 99% (sym). Function
definitions live in files not named after the function, so the name signal
does not move them; `sym:` and `sort:relevance` are the forms for functions.

The default content order rarely opens with the definition (17.0% top-1,
86.0% top-20): symbols referenced broadly are buried under files that merely
mention them. The definition-line bonus under `sort:relevance` is the single
largest lever (63.0% top-1, 100% top-20) and lands every label; `sym:`
definition search is close behind (59.0% top-1) and is strongest on functions
(87.5% top-1), whose names are unique here. Plain filename search cannot find
content symbols at all (0% top-1) because corpus files are snake_case: the
symbol name is not a substring of the basename.

Worst misses for (a) `content:<sym>` (10 labels whose defining file was not in
the top 20; all rank 0): `Attr`, `CSSVariableData`, `ContentLayoutObject`,
`ExecutionContext`, `InterpolationValue`, `LineBreaker`, `NavigationState`,
`NonInterpolableValue`, `QualifiedName`, `Sanitize`. Worst ranks for (b)
`content:<sym> sort:relevance` (no misses; every label in the top 20):
`top` 8, `DOMTokenList` 7, `PaintControllerPaintTest` 6, `ExecutionContext` 5,
then `AbortController`, `CSSFunctionRule`, `HasWebView`, `PageTransitionEvent`,
`PerformanceEventTiming`, `ScriptableDocumentParser` at 3. Several of these are
high-frequency generic names (`used` 1922 ref files, `top` 582,
`QualifiedName` 440), which the ≥ 3 reference rule admits; they are the
residual hard cases once the definition bonus applies.

## Name-search lessons (`perf/name-lessons`, G11)

2026-10-14 local profile, chromium walk index (555,849 records),
standalone resident services on private pipes (base `01abdf3` vs new),
`GOMAXPROCS` unset on services, content builds off
(`SEEKFS_CONTENT_SCOPE=off`). Numbers are min-of-7 server-side
`search_ms` (`abms.py`, scratch-only): planner sources identical
base→new on every old-shape query, so lanes are unchanged; result sets
identical except the four new-operator shapes below (base returns empty).

| query | source | before | after |
| --- | --- | --- | --- |
| exact `BUILD.gn` | global:filename-pngr | 0.53 | 0.52 |
| substring `chrome` | global:filename-pngr | 1.06 | 1.58 |
| substring `browser` | global:filename-pngr | 1.05 | 1.58 |
| substring broad `test` | global:filename-pngr | 3.71 | 4.78 |
| multi-term `chrome browser` | global:filename-trigram | 4.77 | 4.48 |
| multi-term `net url` | global:filename-trigram | 1.57 | 2.62 |
| `ext:cc main` | global:filename-pngr | 7.30 | 6.79 |
| `ext:md` | global:ext:md | 0.52 | 0.52 |
| fuzzy 1-typo `chorme`/`broswer`/`browsre` | fuzzy tier | 7.9–8.4 | 7.8–8.4 |
| fuzzy 2-typo long `doccumnet` | fuzzy tier | 7.42 | 6.83 |
| short `a`/`go` | limited-single-term | 0.52–0.54 | 0.52–0.56 |
| short `lg` | limited-single-term | 7.60 | 6.25 |
| `car` | global:filename-pngr | 0.53 | 0.52 |
| `chrome sort:size` / `sort:modified` | global:filename-pngr | 2.17/1.05 | 2.09/1.06 |
| `ext:cc sort:extension` | global:ext:cc | 8.78 | 10.05 |
| `glob:*.mojom` | global:glob-ext:mojom | 0.52 | 0.52 |
| `ext:h` | global:ext:h | 0.52 | 0.52 |
| `-path src` | path-component-direct-top | 12.61 | 14.33 |
| `-path third_party blink` | path-component-multi-top | 28.93 | 27.24 |
| `-path go` | bounded-scan | 10.58 | 11.17 |
| `-path regex:chrome.*browser` | bounded-scan | 66.12 | 68.16 |
| `-path regex:.*test.*\.cc` | bounded-scan | 98.02 | 103.37 |
| `-path ext:cc dir:renderer frame` | global:components | 5.80 | 6.29 |
| `'OWNERS` (new) | exact-empty → global:filename-pngr | 5.23 (empty) | 2.08 |
| `^chrome` (new) | exact-empty → global:filename-pngr | 5.78 (empty) | 2.07 |
| `main.cc$` (new) | exact-empty → global:filename-pngr | 4.86 (empty) | 0.51 |
| `glob:*.{c,h}` (new) | glob-ext:{c,h} → planned:or-group | 0.56 (0 results) | 25.68 |
| `glob:src/**/*.{c,h}` (new) | planned:empty → global:boolean-iterator | 0.52 (0 results) | 41.74 |

Read the small deltas as machine noise, not code effects. Four min-of-7
runs (two binaries × resident/lowmem, same index) flip signs on identical
binaries: `lg` +5% → −47% → −16% → −18%, `car` +101% → −0% → −0% → −2%,
`chrome sort:modified` +50% → +0% → +1% → +1%, `ext:cc main` −5% → +12% →
−9% → −7%, `chrome browser` +14% → +11% → +32% → −6%. Other agents share
the box, and sub-ms rows quantize at the 0.52 ms timer tick. A
same-process benchmark of the verify path bounded the real per-entry cost
at +10–16% with a byte-loop scan; serving the SIMD substring first (hits
cost exactly one Contains, misses add only the ASCII flag scan) cut that
back, after which old-shape lanes are instruction-identical on ASCII input
plus a few length checks. Counts match 20/20 everywhere; `bench` result
hashes match except the four justified new-operator shapes (base returns
empty: it looked up a literal `'owners` term, a `{c,h}` extension, and
unsatisfiable brace patterns).

What drove the numbers (adopted vs rejected):

- Adopted, accent folding (comparison-side only, no `.gsi` change):
  folded verification in `entryMatchesScalar`, `compactRecordPrecheck`,
  `recordMatchesNonPath`, and the parent-chain walk; the accent union in
  `nameTermPosting` (inherited by `pathTermPosting`) plus/AddsMatches
  lane declines that fire only when the union would add a candidate, so
  ASCII corpora keep byte-identical lanes. Same-process verify bench:
  +10–16% per entry with a byte-loop scan, cut back by serving the SIMD
  substring first (hits cost exactly one Contains; only misses pay the
  ASCII flag scan, only non-ASCII haystacks pay NFD).
- Adopted, operators: `'exact`/`^prefix`/`suffix$` append their stripped
  text to Terms (posting lanes engage; the anchor is verified on the
  name), so anchored queries answer from postings — faster than the base
  exact-empty scans above. `in:` merges into Under at parse (nesting
  rule; disjoint roots are impossible with no scan). `**`/brace path
  globs verify from precomputed expansions and contribute brace-common
  literals to Dirs plus an ext OR for the posting lanes; `*.{ext}`
  filename braces rewrite to an ext OR group. `glob:*.{c,h}` (64k
  ext-union candidates, ~26 ms) and `glob:src/**/*.{c,h}` (2 results,
  38–46 ms) cost with the ext-posting union, not with volume size: no
  full scans, bounded by posting sizes.
- Adopted, regex letter-mask prefilter: per-regex masks over
  provably-required literal disjuncts (`regexRequiredLiteralAlternatives`,
  widening-sound; case-blind `memoMaskOf`), checked before MatchString.
  Same-process microbench over 2000 sampled chromium paths, identical
  hit counts on/off: sparse `chrome.*browser` −13%, dense
  `.*test.*\.cc` −3%, alternation `README\.(md|txt)` −4%. Never loses
  (masks are a necessary condition); service-level regex rows land
  within noise because literal postings already narrow them.
- Adopted as specified, typo budget: the tree already implements the
  fsearch rule (1 edit, 2 for 8+ via `fuzzyThresholdForTerm`, Damerau
  transpositions); pinned by unit tests, no code change. Anchored and
  `"quoted"` terms never fuzzy-correct (exact-tier intent).
- Adopted, `--current` demotion: last within-band key in rank_blend.go
  (fff `current_file`); sets/counts provably unchanged (reorder-only,
  empty-Current pages byte-identical).
- Rejected, per-record letter mask in substring scan verify: strictly
  weaker than the existing exact-substring precheck — it would add a
  pass with zero pruning. No code change.
- Rejected, letter masks for 1–2 char terms: 1–2 bits are ubiquitous,
  the subset test is vacuous (`a`/`go` flat ±2%). No code change.
- Rejected, per-record path masks in broad-path scans: needs full path
  reconstruction per record, defeating the purpose; the memo lane's
  folded dir-bits already cover this design point. No code change.
- Not persisted: every mask is computed on the fly (ns) or held in the
  existing background identity; the `.gsi` format is untouched, so old
  readers need no ignore/rebuild path. The per-index foldable-ID list
  builds lazily once per generation (one linear pass over lower names;
  empty on ASCII corpora, where every gate short-circuits).

Correctness gates (same services/index): `parity.py` (76 queries ×
search 20/300/count) 0 set/count differences — the only 3 diffs are the
`case:Makefile` rejection message, which now lists the new `in:` filter
(same exit 1); recall 100% on 14 terms incl. broad `test` (51,684);
one-typo top-1 99.5% (N=200 seed 42), two-typo 93.0% (N=100 seed 7);
right-file-first byte-identical to the Table above (plain 39.0/95.0,
partial 2.0/12.0, typo 39.0/98.0 and 38.5/92.7; seeded 66.0/95.0 and
13.0/21.0; opened 20→47, control 19→19).

Caveats: services ran `SEEKFS_MEMORY_MODE=resident`
(`build_flavor: cli,service`); lowmem runs show the same lanes at
similar times. The `-path regex` rows (~70–120 ms bounded-scan here vs
0.5 ms in the profile harness) reflect this box's cold resident
structures, identically on both builds — compare base↔new, not against
the harness table.

## Freshness and footprint (`perf/freshness-footprint`, G4/G5/G6 name side)

2026-10-14 standalone-service measurements, `GOMAXPROCS=2`, content
disabled via an isolated `SEEKFS_DIR` (production's ~500 MB working set
on E: includes the content build; name-side numbers below isolate the
name index). Harness scripts lived in `E:\bench\scratch\footprint\`
(scratch-only, deleted after).

Visibility latency (walk scratch volume, 200 files, name changes →
visible in `count`; db outside the watched root):

| change | before | after |
| --- | --- | --- |
| create | 685 ms | 83–350 ms (box noise; fast path) |
| rename (new name) | 360 ms | 82 ms |
| delete (gone) | 642 ms | 59 ms |
| write-only edit → `size:` filter | never without a later name change* | ~600 ms |

\* Review fix: the first cut skipped metadata-only batches entirely,
which would have left Size/ModUnix stale forever — breaking `size:` /
`dm:` filters, size/modified sorts (load-built rank vectors),
`--recent`, and the content walk reconcile fingerprints
(`content_walk.go` diffs `rec.Size`/`rec.ModUnix`). Metadata-only
batches now rebuild on a 500 ms coalescing debounce
(`walkWatcherMetaDebounce`, reason `watch-meta`): save bursts merge,
while filters, sorts, and content re-extraction lag a single edit by
~0.5 s plus the re-walk. Pinned by
`TestWalkMetaRefreshUpdatesSizeAndMod` and
`TestWalkMetaRefreshRearmsContent`.

Drivers: walk watcher debounce 250 ms → 50 ms (`walkWatcherDebounce`)
for name changes; the batch classifier (`walkClassifyNotify`) is
tri-state and fails safe — truncation, garbled offsets, unknown
actions, and overflow all rebuild on the fast path
(`TestWalkClassifyNotify`, `TestWalkRebuildPlan`; a missed rebuild
would silently drop a rename, a spurious one only costs a re-walk).
The USN steady-state wake was already event-driven (blocking
`FSCTL_READ_USN_JOURNAL` with `BytesToWaitFor=1`, ~0 idle CPU); the WAL
open/write/fsync/close moved after the overlay apply + publish, so the
fsync no longer sits between the change and the query-visible snapshot
(durability unchanged: the on-disk checkpoint advances only on fold, so
a crash before the WAL lands replays the same USN range). USN end-
to-end latency was not measurable here (volume handles need elevation);
the publish-before-fsync order and the synchronous-publish invariant
are pinned by `TestOverlayApplyIsImmediatelyVisible`.

Operational note: a walk db stored under its own watched root
self-triggers (each rebuild stages a uniquely-named tmp → ADDED).
Pre-existing wart, not introduced here; keep walk dbs outside the
indexed root.

Restart (1.46M-entry copy of the live E: index,
`seekfs_e_copy.gsi`, 274 MB file):

| stage | before | after |
| --- | --- | --- |
| base load (`loaded ... elapsed=`) | 118 ms | 286 ms (noise; mmap, no catch-up in either non-elevated run) |
| first query | 0.55 s | 1.17 s (process start + pipe; same shape) |
| USN catch-up vs first query | catch-up blocks the swap (the live 40 s restart) | catch-up runs behind serving (`loaded` logs before `catch-up complete`) |

`loadConfiguredVolume` no longer calls `catchUpServiceVolume`
synchronously; `backgroundCatchUpVolume` drains `[checkpoint, head)`
after the base + WAL overlay are already served (each batch publishes
as it applies), then attaches content (`featuresVolumeReady`) and
starts the replay/persist loops or stale recovery, logging
`catch-up complete ... replayed_usn=N elapsed=... state=...`. Restart
still replays only what changed (WAL replay stays synchronous and
bounded; file checkpoint advances only on fold) — verified by code,
pinned for the serve-early half by
`TestLoadConfiguredVolumeDefersUSNCatchUp`. Elevated-box validation of
the 40 s → ~1 s time-to-first-query is owner work (needs the live
journal).

Resident memory (same 1.46M copy, lowmem, 40 warm-up queries, idle
release at `SEEKFS_IDLE_RELEASE_SECONDS=90`):

| metric | before release | after release |
| --- | --- | --- |
| heap_alloc | 73.2 MB | 50.1 MB |
| heap_released | 4.3 MB | 28.2 MB |
| working set | 250.9 MB | 200.6 MB → ~72 MB after a few idle minutes |
| decoded posting-block cache | 5.5 MB | 0 MB |

The idle loop now clears the global posting-block LRU (new
`clear`/`liveBytes`; default cap 128 MB heap) plus every volume's
term/path-term/ext/under caches (`clearSearchCachesLocked` previously
had no caller) before `debug.FreeOSMemory`. All of it re-decodes from
mmap, so results are unchanged; search and count both note query
activity, so the release cannot fire mid-benchmark. `loaded --json`
gains `memory.cache_bytes` (per-volume posting-list caches, folded
into `known_bytes`) and `runtime.block_cache_bytes`. On a service the
per-volume term caches stay ~0 (each request searches through a
short-lived view with its own maps); the persistent service-side cache
is the global block LRU. `lowmem` vs `resident` meanings unchanged (no
default flipped; `GOMEMLIMIT` stays opt-in via
`SEEKFS_GO_MEM_LIMIT_MB`). For reference, same-index load with the
default content-enabled config measured heap 130 MB / WS 218 MB on the
556k chromium index (content build adds ~100 MB heap) — content-side
footprint belongs to the filter-tier work.

On-demand (G6 name side): with no index loaded, a search scoped
`--under` a real folder now answers from the same bounded folder-first
walk (100k entries / 2 s, read-only) the single-volume path uses as
its zero-hit backstop, instead of erroring. Counts and folder-less
queries keep `service has no search indexes loaded`. A full background
index for the folder is deliberately not started (it would race the
budgeted builder and stall the query behind a build); pinned by
`TestOnDemandFolderFirstSearch`.

Result parity for all of the above, re-run after the review fix
(fail-safe classifier + metadata rebuild path): `parity.py` base-vs-new
over the chromium copy, 76 queries × search 20/300/count: 0 differences
(209 non-empty base results); recall 100% on 20 terms; one-typo top-1
99.5% (N=200 seed 42) and two-typo top-1 93.0% (N=100 seed 7) with zero
non-timing per-query diffs. Full suite green
(`go test -p 1 -timeout 40m ./...`).

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

## Name-Index Build Memory (MFT/USN)

2026-10-22, branch `ux/build-memory`. Synthetic MFT records (8-40 char names,
about 8% directories, depth about 8) fed through the same parse/accumulate
code that `enumMFT` uses, then `buildRecordsFromMFT`, `buildOrders`, and
`stageIndexFile` into a temp file. Run on an unelevated shell with no raw
volume access:

```powershell
$env:SEEKFS_MFT_BENCH_N='1000000'
go test ./cmd/seekfs -tags uiautomation -run '^$' -bench '^BenchmarkMFTBuildMemory$' -benchtime=1x -v
```

Bytes per record at each stage (peak HeapInuse). The before column was
measured at 200,000 records before the change; the after column at 1,000,000
records. Both shapes scale linearly, so the per-record values compare:

| Stage | Before B/record | After B/record | Change |
| --- | ---: | ---: | ---: |
| MFT accumulate (`parseMFTRecordInto`) | 303.9 | 120.7 | -60% |
| `buildRecordsFromMFT` | 260.9 | 147.2 | -44% |
| `buildOrders` | 132.4 | 115.2 | -13% |
| `stageIndexFile` | 646.7 | 447.7 | -31% |
| Build peak (stage max) | 646.7 | 447.7 | -31% |

Retained heap after the build returns and `runtime.GC()` runs:
1.3 MB before and after, a 0.0 MB delta from baseline. The former
`map[uint64]mftEntry` is replaced by a 40-byte record slice indexed by MFT
record number plus one name byte arena. Names are not copied again for the
Index: `CompactRecord.Name` views `Index.NameBlob`, and `writeIndexFile`
publishes that arena as the token table directly.

What still dominates: `stageIndexFile` and its path-rank derived section.
`buildCompactPathOrderRank` materialises a full lowercase path string for
every record (about 150-200 MB at 1M records), so the whole-build peak stays
above the 40% target even though accumulation and record construction now
clear it. Reducing that needs an arena or trie path key, which this change
does not attempt.

Service path: a settings-driven build now releases the built `Index` as soon
as `buildIndexUSNVolume` returns. `runSettingsVolumeBuild` defers a single
`debug.FreeOSMemory()` and logs
`index-usn memory volume=... heap_inuse_mb=... released_mb=...`.
`TestSettingsBuildReleasesBuiltIndex` proves the stub Index is finalizable
after the build, the installed volume is the reloaded mmap index, and
HeapInuse returns within a generous baseline bound.
