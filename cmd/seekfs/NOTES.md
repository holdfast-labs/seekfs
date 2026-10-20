# Regex zero hits: phase 1 numbers and phase 2 fix

Worktree: `E:\bench\wt\regexzero`, branch `fix/regex-zero-hits`, base
`3708c64`. The installed Windows service stayed up. All live queries were
read-only and used the default pipe.

## Phase 1: live service (whole E:, 895,964 docs)

Queries used `E:\bench\rt\seekfs.exe search --json -n 5`.

| query | hits | candidates | search_ms | source | complete |
|---|---|---|---|---|---|
| `content:TabStripModel` | 5 | 3,205 | 1,210 | `content-candidates` | false |
| `content:Tab content:Model` | 5 | 71,400 | 1,050 | `content-candidates` | false |
| `content:/Tab\w+Model/` | 0 | 71,401 | 786 | `content-candidates` | false |
| `content:/Tab\w+Model/`, explicit budget 8,192 | 0 | 71,418 | 28,110 | `content-candidates` | false |

The first result of `content:TabStripModel` is
`E:\bench\chromium\chrome\browser\ui\tabs\tab_strip_model.h`.

### Required literals and trigram AND

`regexRequiredLiteralAlternatives("Tab\w+Model")` returns one disjunct with
two runs: `tab` and `model`. The `\w+` gap breaks the run, so no adjacency
gram exists. The regex candidate count (71,401) equals the two-literal-leaf
count (71,400) plus delta docs. Therefore the trigram AND of `tab` and
`model` is applied. The prefilter is sound but weak: it proves presence, not
order or distance.

The per-file filter row (CXFL) stores only trigram membership bits. It cannot
prove adjacency or order, so it cannot reject "Tab ... Model" files without a
read.

### Ordering

All positive content leaves are regex, so `contentLeavesAllRegex` is true.
The old code called `sortCandidateIDsByScanOrder`, which uses name order.
The first 512 to 8,192 name-order candidates contain both literals but do not
match the pattern. The matching `tab_strip_model*` names sort under `t`, after
those candidates. The page-scaled valve (512 reads here) stops first:
zero hits, `complete=false`.

This is a candidate-order bug, not an expected low hit rate. An explicit
8,192-read budget still returns zero hits and takes 28 s.

## Phase 2: fix

New `promoteRegexNamedCandidates` (in `planner_rank.go`) stably moves
candidates whose file name matches a positive regex leaf to the front,
headers first. The name match is case-folded even for a case-sensitive query:
`tab_strip_model.h` is the likely definition of `TabStripModel`, and a wrong
hint costs one verified read only. Verification still decides every hit, so
results stay exact.

`search_compact.go` now keeps the scan order for pure-regex candidates (it is
also the bounded-scan resume order, so frontier resume stays consistent),
then promotes pattern-named files for the default order. Explicit sort columns
keep their sort and do not promote.

The fixture in `content_regex_common_literal_test.go` uses 600 files with
`Tab control Model` and five `tab_strip_model*` files with `TabStripModel`.
Failure before the fix:

```
content_regex_common_literal_test.go:51: default valve: hits=0 disk_verified=512 candidates=605; want 5 hits from the tab_strip_model files
```

After the fix: 5 hits, 13 disk verifies, 605 candidates, same default valve.

## Tests

- `go test -p 1 -run 'TestContentRegexCommonLiteralHitsWithinDefaultValve' ./cmd/seekfs/` passes.
- `go test -p 1 -run 'Regex|Frontier|Bounded|Superset|Valve|Budget|Prune' ./cmd/seekfs/` passes (52 s).
- `go vet ./cmd/seekfs/` is clean.
- `gofmt -l` is clean for the changed files.

## Limits

The live service cannot be redeployed here, so the after numbers come from the
fixture. The fix promotes pattern-named files only. A regex whose true hits
have names unrelated to the pattern still depends on rank order and the disk
valve.
