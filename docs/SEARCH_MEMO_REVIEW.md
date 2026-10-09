# Name memo and deferred materialization review

Distinct-name IDs and character masks score repeated filenames once per query.
A single cached query memo folds ancestor term bits through a parents-first
directory order. Unsupported syntax and unresolved graph regions retain the
existing verifier. Low-memory mode keeps this additional resident state off.
`SEEKFS_NAME_MEMO=0` disables the lane for comparisons or troubleshooting.

The global component verifier retains IDs in a bounded heap per volume when
exact memo matching and complete local ranks are available. Only the local
top-N records become entries, then the existing comparator merges volumes and
overlay records. Base entry materialization is bounded by N times the volume
count; unsupported records may still require path verification. Live aggregate
directory-size changes disable this optimization for size sorts unless the
query selects files only.

Review fixes cover filename versus path-prefix matching, configured roots
replacing volume prefixes, path-mode negation in filename queries, outstanding
volume constraints, directory aggregate size filters, identity lifetime during
background builds, and cancellation after draining global iterators. The
identity used to match records is pinned to the query memo rather than reloaded
from the mutable cache. Resident memory diagnostics include the new arrays.

## Reproducible benchmark

Run:

```powershell
go test ./cmd/seekfs -run '^$' -bench '^BenchmarkGlobalMemoDeferredMaterialization$' -benchtime=3x -count=1
```

Windows amd64, Go 1.26.5, Intel i7-10700KF, 16 logical CPUs; synthetic packed
50,000-file fixture, broad `workspace` path query, limit 20, warmed memo:

| Mode | Time/query | Bytes/query | Allocations/query |
| --- | ---: | ---: | ---: |
| Existing verifier, memo off | 11.96 ms | 41,078,477 | 297,401 |
| Memo and deferred heap | 4.85 ms | 824,616 | 153 |

This sample measures about 2.5x lower latency and 98% fewer allocated bytes.
It does not establish a 10x end-to-end speedup or a resident-memory percentage
on real volumes. Cold identity construction and distinct query rotation have
different costs; rerun on the deployment indexes before extrapolating.

Regression coverage compares results against the existing verifier across
default, path, size, modified, extension and type ordering, hidden IDs, empty
matches, cancellation, prefix semantics, aggregate sizes, mmap records and
mutation. Trace terms with source `memo-rank-heap` report the number of selected
base entries materialized.
