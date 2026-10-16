# Regex precision measurement (local Chromium, v12)

Built the walk index and v12 content sidecar over `E:\bench\chromium\chrome`; build and query harness is local-only under `E:\bench\scratch\regex2`.

| Query | Hits | DiskVerified | Source | First | Best-of-3 |
|---|---:|---:|---|---:|---:|
| `content:/mutex_\w+/` | 8 | 156 | content-candidates | 2.492 s | 2.466 s |
| `content:/Get\w+ForTesting/` | 0 | 512 | content-scan | 1.146 s | 758 ms |
| `content:/kMax\w+/` | 20 | 217 | content-candidates | 208 ms | 189 ms |
| `content:/TODO\(\w+\)/` | 20 | 288 | content-scan | 29 ms | 23 ms |
| `content:mutex_` | 19 | 156 | content-candidates | 2.668 s | 2.584 s |

Root cause analysis: the `mutex_` literal is retained intact and used by the candidate stream, so truncation/splitting is not the issue. The regex additionally requires a word character immediately after `_`; current indexed trigrams only require the literal, leaving literal-only occurrences among candidates. Harness did not yet instrument per-file gate rejects or exhaustive-verify comparison, and these figures do not establish the requested before/after performance target.

Harness issue found on initial attempt (fixed in scratch harness): `zz_regex2_measure_test.go:23:2: undefined: contentCatchUpSync`. Successful run log: `E:\bench\scratch\regex2\measure.log`; targeted test log: `E:\bench\scratch\regex2\targeted.log`.
