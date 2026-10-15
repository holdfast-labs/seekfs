# Work queue

This is the single list of open work on `integ/content-main`. Agents are Paseo
`opencode/command-code/meta/muse-spark-1.3-contributor` in `build` mode, created in
the seekfs workspace. The `opencode-go/` route hit its weekly limit on 2026-10-14
and resets about 2026-10-18. Each agent gets its own worktree at `E:\bench\wt\<name>`
on its own branch from integ. Every merge needs a review plus a parity or recall
check where results could change. The coordinator runs the full suite with
`-timeout 40m` after merges; the default tier takes about 3–5 min, and CI also
runs the soak tier. Gap numbers (G1 … G13) refer to `docs/REFERENCE_TOOLS.md`.

## Coordinator rules (provider usage is nearly exhausted)

The owner approved these rules on 2026-10-14. The biggest past costs were
long debugging loops with no checkpoint, multi-hour sessions resending a
growing context, full-suite output pasted into context, polling, coordinator
"FYI"/"continue" prompts, and 4 agents re-reading the same files.

- Agent model since 2026-10-15: `opencode/openai/gpt-6-luna`, mode `build`
  (Muse routes are out of credits).
- Run 1–2 agents at a time, not 4. Do not enable paid provider balance without
  asking the owner.
- Give each agent a narrow brief with an explicit stopping point: "commit and stop
  when X passes".
- Agents commit as soon as a piece works. After about 2 failed attempts at the
  same problem, they commit WIP, write what they tried in `NOTES.md` in the
  worktree, and stop.
- Agents run only targeted tests (`go test -run <pattern>` on the affected
  area). The coordinator runs the full suite once, at merge time.
- Send test, build and benchmark output to a file and read only the tail or
  failures. Wait on the process instead of polling.
- Coordinators send no FYI messages to agents; only prompts that contain work.
- On command-code Muse, any agent message without a tool call ends its turn
  (agents stopped mid-task after a progress line). Every brief must say: "No
  progress narration; reply without a tool call only for the final report."
- Prefer a fresh session with a short handoff note over resuming a very long one.
- The coordinator does small finishing work (commit, merge, suite run)
  directly instead of spawning an agent for it.

## Running (2026-10-15)

Merged so far this round: `perf/content-underfill`, `perf/name-fuzzy`,
`fix/cli-silent-empty`, `content/idle-governor`, `content/spool-peak`, `perf/ranking-signals` (G7, G8, G13), `content/inclusion-policy` (G3, §6z,
format v10), `perf/freshness-footprint` (G4/G5/G6 name side: walk changes visible in
about 0.3 s, meta in about 0.6 s, restart serves in about 0.3 s, idle memory
about 72 MB for 1.46M names, `--under` folder-first search),
`perf/name-lessons` (G11: `'exact ^prefix suffix$`, `**`/brace globs,
`in:`, accent folding for names, `--current`, regex letter masks),
`test/suite-speed` (default suite ~3–5 min; soak tier
`SEEKFS_TEST_SOAK=1` in CI), `fix/short-term-postings` (direct-index LOWR writer
corruption, exact required sources, auto-fuzzy only on zero exact hits).

| Branch | Worktree | Scope |
|---|---|---|
| `content/stream-postings` | streampost | Build peak: candidate B (no per-group reframe copy, direct-offset spool reads, hole-punch after shard save) measured on a >= 6-shard Chromium subtree; keep only if peak/extracted drops >= 0.1 with identical output, then lower the peak multiplier |
| `perf/static-ranking` | staticrank | **Parked** at WIP commit `761db7b` (OR-lane case unresolved, suite not green); resume only with a fresh, narrow brief (G13) |

Merged 2026-10-15: `content/time-budget` (a3a94f8, G10: opt-in `--budget 150ms` /
`budget_ms` stops new disk verifies at the deadline; partial page in order,
`incomplete` + `budget_exceeded`; counts ignore it; cursor pagination still open),
`content/grep-lines` (4daa2b3, G10: `seekfs search --lines
[-C N | -A N -B N] [--max-per-file N]` prints rg-style `path:LINE:text`, client-side,
`--json` adds `lines`), `content/smart-case` (074313a, G10: an uppercase literal in
every content leaf makes content matching case-sensitive unless `case:` is
explicit; name/path terms unaffected), the 10 GiB reserve cap (65ff317), and
`content/verify-precision` (e045d37): a default-order page
whose posting superset exceeds the relevance window verifies it in rank order
and stops at the page instead of a name-order scan. Full Chromium limit 20:
`content:mutex` 2,157 -> 228 verifies, 591 -> 142 ms, 20 hits; `ext:cc mutex`
155 -> 52 ms. A superset past the window stays `incomplete` (conservative).

Merged 2026-10-14: `feat/ui-input-and-user-signals`, `integ/filter-tier-merge`
(v11, no stored text by default), `perf/adaptive-build-workers`,
`content/regex-prune`, `content/index-size-budget`.

**Full Chromium on v11 (2026-10-14, 600 MiB explicit budget):** 463,273 docs
indexed, 0 dropped (was 77,713), 2609 MB extracted, 615 MB index (0.236x;
grams 341, filter 125, terms 115, doctable 23, paths 11 MB), build 44 min at
GOMAXPROCS=2, peak disk 0.56x extracted. Limit-20 warm/first: rare literal
13/274 ms, phrase 62/346 ms, zero-result 1 ms, `mutex` 591 ms/8.3 s
(incomplete), `ext:cc mutex` 155 ms (incomplete), regex `mutex_\w+`
1.27/4.2 s. A full fold re-reads every file: 43 min (5.6 ms/doc).

## Queued, in order

1. **Grep features (G10, G9, G11). Unblocked: `content/filter-tier` merged
   2026-10-14** (both touch the content verify and snippet path). Plain-literal grep via SIMD memmem and
   multi-pattern OR via Aho-Corasick (fff); fuzzy grep (Smith-Waterman per
   line); context lines; a per-query time budget (fff default 150 ms) with
   cursor pagination; smart case for content; ripgrep line semantics for
   regex (fsearch — v11 verify already anchors `^`/`$` per line; the
   `content/regex-prune` branch also fixed the regex required-literal
   prune: literals constrain tier-1/tier-2/verify in all queries, DONE);
   `sym:` definition search and a definition-line bonus;
   extension and single-file filters for grep.

## Smaller follow-ups

- **Whole-drive coverage: stream postings without a full text spool.** The
  reserve-governed default (no explicit budget, the real E: drive) is limited
  by the build's transient peak, extracted text x1.5. Peak was 0.56x on
  Chromium. Build postings without spooling all text first, so the reserve
  admits far more of the drive.
- **Build speed:** 44 min for 2.6 GB on Chromium at GOMAXPROCS=2. Profile
  extraction vs tokenize vs assembly.
- **Fold cost under no stored text:** a fold re-reads every doc in the shard
  from disk (5.6 ms/doc). Keep shards small, or fold from postings only.
- **Delta-varint spill runs:** the external section builder spills raw 8-byte
  (key, docID) records: 1.02x of extracted text per shard on a blink subtree,
  vs ~0.18x if delta+varint coded per key. Shrinks per-shard scratch (fixed
  slack) and build IO about 5x (streampost NOTES.md, 2026-10-15).
- **Host-sensitive reconcile tests:** `TestContentAttachReconciles{WorktreePolicyToMain,
  WorktreePolicyToAll,VanishedWorktree}` end `stale` when TEMP is on C:
  (nearly full) and pass with TEMP on E: (fails back to at least 878b6d2, so
  environmental). Free space/reserve is not the cause (reserve is 0 in tests).
  Run suites with TMP/TEMP=E:ench\scratch	mp until found.
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
  filter tier (`content/filter-tier`) so the work buys coverage. Mostly
  moot after `content/index-size-budget`: an explicit budget now charges
  projected index bytes (~0.15x text), so it admits ~6x more files. The
  reserve-governed default is still limited by the transient spool peak
  (extracted text x1.5); streaming postings without a full text spool is the
  next lever for whole-drive coverage.
- **Single worker under load:** DONE (`perf/adaptive-build-workers`).
  Extraction re-samples the desired worker count at every governor batch
  boundary (recent/in-flight query, input incl. UI heartbeat, or battery ->
  1 worker; quiet -> back to the GOMAXPROCS-aware cap), so load only slows
  the build while it is actually present instead of serializing it to 60+ min.
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
- **Live restart validation:** the deferred USN catch-up (restart serves in
  about 0.3 s) was measured on a copy. Validate it on the live E: service at
  the next relaunch (elevated), since the live load used to take about 40 s.
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

- **Relaunch the live E: service on integ >= 65ff317** (elevated). The build
  reserve is now 10 GiB (was 20 GiB; approved 2026-10-15), which raises build
  headroom on E: from ~2 GB to ~12 GB; the live service keeps the old reserve
  until it runs the new binary.
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
