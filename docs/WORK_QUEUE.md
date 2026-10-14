# Work queue

This is the single list of open work on `integ/content-main`. Up to 4 agents
run at once (more exhausted commit memory on the dev box). Agents are Paseo
`opencode/opencode-go/muse-spark-1.3-contributor` in `build` mode, created in
the seekfs workspace. Each gets its own worktree at `E:\bench\wt\<name>` on its
own branch from integ. When a slot frees, start the next queued item. Every
merge needs a review plus a parity or recall check where results could change.
Run the full suite with `-timeout 40m` after merges. Gap numbers (G1 … G13)
refer to `docs/REFERENCE_TOOLS.md`.

## Running (2026-10-13)

Merged so far this round: `perf/content-underfill`, `perf/name-fuzzy`,
`fix/cli-silent-empty`, `content/idle-governor`.

| Branch | Worktree | Scope |
|---|---|---|
| `test/suite-speed` | suitespeed | `cmd/seekfs` suite under ~5 min; harden `TestHeadlessFrameCost`; gofmt drift |
| `content/spool-peak` | spoolpeak | Build peak ≈ final size, so more coverage per GB (G2, §6x) |
| `content/filter-tier` | tier2 | Filter tier with no stored text, verified from disk; evaluate it as the default (G1, §6y) |
| `content/inclusion-policy` | inclusion | Queued item 1: main-checkout-only worktrees, lower caps, lockfile/JSON/minified/cache skips, user skip list, content-hash dedupe (G3, §6z) |

## Queued, in order

1. **fff ranking signals (G7, G8).** Open/frecency tracking with an `open`
   API and boost; `near` proximity; a recently-used list; a query-history
   boost; a `git:` filter (modified/staged/untracked/…); git-status and
   recent-commit boosts; a "right file first" metric for name queries (G13).
2. **Agent integration (G12).** An MCP server (find, grep, multi_grep) and a
   JSON-lines stdio mode.
3. **Grep features (G10, G9, G11).** Fuzzy grep, multi-pattern OR, context
   lines, a per-query time budget with cursor pagination, smart case for
   content, `sym:` definition search and a definition-line bonus, the
   `'exact`, `^prefix` and `suffix$` operators, `**`/brace globs and accent
   folding.
4. **Freshness and footprint (G4, G5, G6).** Read changed/dirty files directly
   at query time; on-demand indexing (searched folder first); service
   resident memory (about 500 MB for 1.46M names); startup load time (about
   40 s for 1.46M entries).

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
- **User input seen from the service:** the governor's input signal is
  unavailable from session 0 on this Windows build (WTS `LastInputTime` is 0,
  `GetLastInputInfo` sees only session 0; §6w, `loaded --json` governor
  section). Fix it by having `seekfs-ui` (runs in the user session) report
  its session's last-input time to the service over the pipe.
- **Slow differential test:** `TestContentGlobalLaneScopeDifferential` takes
  300–666 s. `test/suite-speed` tiers it; confirm CI still runs the full tier.

## Owner actions (not agent work)

- **Patch release decision:** the CLI silent-empty bug (service errors exit 0
  with no output) is in released v1.11.0. The fix is merged into integ (two
  lines in `cmdSearch`) and could be cherry-picked to `main` for v1.11.1.
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
