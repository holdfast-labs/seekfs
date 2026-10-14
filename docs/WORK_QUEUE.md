# Work queue

This is the single list of open work on `integ/content-main`. Up to 4 agents
run at once (more exhausted commit memory on the dev box). Agents are Paseo
`opencode/opencode-go/muse-spark-1.3-contributor` in `build` mode, created in
the seekfs workspace. Each gets its own worktree at `E:\bench\wt\<name>` on its
own branch from integ. When a slot frees, start the next queued item. Every
merge needs a review plus a parity or recall check where results could change.
Run the full suite with `-timeout 40m` after merges. The default tier takes
about 3–5 min; CI also runs the soak tier. Gap numbers (G1 … G13)
refer to `docs/REFERENCE_TOOLS.md`.

## Running (2026-10-13)

Merged so far this round: `perf/content-underfill`, `perf/name-fuzzy`,
`fix/cli-silent-empty`, `content/idle-governor`, `content/spool-peak`, `perf/ranking-signals` (G7, G8, G13), `content/inclusion-policy` (G3, §6z,
format v10), `perf/name-lessons` (G11: `'exact ^prefix suffix$`, `**`/brace globs,
`in:`, accent folding for names, `--current`, regex letter masks),
`test/suite-speed` (default suite ~3–5 min; soak tier
`SEEKFS_TEST_SOAK=1` in CI), `fix/short-term-postings` (direct-index LOWR writer
corruption, exact required sources, auto-fuzzy only on zero exact hits).

| Branch | Worktree | Scope |
|---|---|---|
| `perf/static-ranking` | staticrank | Queued item 0b: static ranker (no usage) right-file-first above 39% top-1, on two labeled sets; sets and counts unchanged (G13) |
| `integ/filter-tier-merge` | tiermerge | Merging `content/filter-tier` (no stored text by default) with inclusion v10 + spool-peak; one format v11; dedupe under no-stored-text; re-measure; list regressions |
| `perf/freshness-footprint` | footprint | Name-side change visibility (fsearch ~15 ms), restart/load time (~40 s for 1.46M), resident memory (~500 MB), idle release, on-demand folder-first indexing (G4, G5, G6) |

## Queued, in order

1. **Grep features (G10, G9, G11). Start after `content/filter-tier`
   merges** (both touch the content verify and snippet path). Plain-literal grep via SIMD memmem and
   multi-pattern OR via Aho-Corasick (fff); fuzzy grep (Smith-Waterman per
   line); context lines; a per-query time budget (fff default 150 ms) with
   cursor pagination; smart case for content; ripgrep line semantics for
   regex (fsearch); `sym:` definition search and a definition-line bonus;
   extension and single-file filters for grep.

## Smaller follow-ups

- **Typo fallback scope:** content typo fallback fires on scoped zero-result
  queries even when exact matches exist elsewhere (`async` → `sync`). Decide
  whether it should only fire when the whole index has no exact match (from
  the recall run, `E:\bench\scratch\recall\REPORT.md`).
- **Single-line JSON:** an 11.4 MB single-line `tokenizer.json` makes every
  broad query 4–5× slower. Covered by item 1's caps; verify after.
- **Extraction past the budget:** after the budget bites, builds keep
  extracting every in-scope file just to tally exact drop counts (468k files
  extracted for a 71k-doc budget, `content/idle-governor` report). Either
  estimate the drops from file sizes, or feed those extractions into the
  filter tier (`content/filter-tier`) so the work buys coverage.
- **Single worker under load:** builds drop to 1 worker when queries are
  active at build start, which serializes a loaded build to 60+ min. Adapt
  workers during the build instead of fixing the count at the start.
- **User input seen from the service:** DONE (`feat/ui-input-and-user-signals`).
  `seekfs-ui` reports its session's `GetLastInputInfo` time over the pipe
  (`input-heartbeat`, local-only, 5 s background heartbeat, 30 s expiry);
  the service treats the freshest report as the governor input signal and
  `loaded --json` names the source (`ui-heartbeat`/`wts`/`lastinput`/
  `unavailable`). See §6w.
- **Save-time peak:** the write guard checks free space before each shard save,
  but a save briefly holds both the section files and the output (§6x
  residual). Account for it, or stream the save.
- **Dense-text spool:** the fastest zstd level stores high-entropy text
  uncompressed (1.000x). The default level gets 0.637x, about 28% more
  coverage on dense corpora for roughly +4 s of serial encode (§6x).
- **JSON-lines stdio mode (G12):** optional and low priority. **No MCP
  server**: the owner ruled it out on 2026-10-13.
- **Content accent folding:** names fold accents (`resume` → `résumé`), but
  content matching is still case-folded only. Folding content needs an
  index-format change; fold it into the next format bump.
- **Ranking store scope:** DONE (`feat/ui-input-and-user-signals`). Per-user
  `ranking_signals-<SID>.json` files next to the service state, keyed by the
  pipe client's SID, ACL-restricted to SYSTEM + Administrators + owner;
  `open`/`recent`/ranking use the caller's store; legacy shared file migrates
  to the first opener. See `search-syntax.md` "Ranking signals".
- **Single-line JSON follow-up:** the 1 MiB / 10 MiB caps and JSON skip from
  `content/inclusion-policy` should cover the 11.4 MB single-line
  `tokenizer.json`. Verify on the next real-index recall run.

## Owner actions (not agent work)

- **Patch release decision:** two bugs in released v1.11.0 are fixed on
  integ and could be cherry-picked to `main` for v1.11.1:
  - CLI silent-empty: service errors exit 0 with no output (two lines in
    `cmdSearch`, `fix/cli-silent-empty`).
  - `seekfs direct` writes a corrupt LOWR (folded-name) section, so
    direct-built indexes return too few results for short or broad terms
    (`go` 6 of 21). The fix also discards corrupt sections on load
    (`fix/short-term-postings` commit 8205201). USN-built and walk-built
    indexes are not affected.
- **Paseo sidebar:** 14 stale worktree projects are marked archived in
  `~\.paseo\projects\projects.json` (backup at
  `E:\bench\scratch\paseo-projects.backup.json`). This takes effect on the
  next Paseo daemon restart, ideally between agent rounds. If they reappear,
  rerun `E:\bench\scratch\review\archive_paseo_projects.py`.
- **Test service on E::** a real-index test service was installed by
  `index-volumes`. Remove it when real-index testing is done.

## After each round of merges

- Full suite on integ (`-timeout 40m`).
- Relaunch the E: test service with the integ build (`E:\bench\rt\relaunch.ps1`,
  elevated).
- Confirm the content index publishes and survives a journal wrap without
  rebuilding.
- Spot-check zero-result content latency on the real index.
- Rerun the real-index recall check against ripgrep if content results could
  have changed.
