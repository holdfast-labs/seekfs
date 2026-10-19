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

- Agent model since 2026-10-16: `opencode/deepseek-v4.1-flash-free` (free), mode `build`;
  `opencode/openai/gpt-6-luna` when its 5-hour window allows
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
| `perf/static-ranking` | staticrank | **Parked** at WIP commit `761db7b` (OR-lane case unresolved, suite not green); resume only with a fresh, narrow brief (G13) |

Merged 2026-10-19: `fix/phantom-orphans` (79f5266, 156ce46): the persist swap
carried post-snapshot overlay tombstone/shadow sets (base record IDs) into the
folded replacement base's different ID space, hiding unrelated records; the
next fold dropped them (the E:\bench\wt worktrees), and the path builder
re-rooted their orphaned children at the volume root (phantom `E:\.git`, which
made content auto scope treat all of E: as one repo). Carried IDs are now
remapped by FRN, and a record whose parent FRN is missing has no path instead
of a fabricated root path (self-parented roots still end the walk). Live E:
rebuilt from the MFT: content ready at 854,202 docs (junk under
wispd-target/SteamLibrary gone; skips now dependency 179k, build_output 40k,
gitignore 45k, root 20). `fix/content-inclusion-stats` (79a38a4): scope
reconcile refreshes the reported inclusion stats.

Merged 2026-10-19: `fix/content-drive-reconcile` (3f57280): reconcile charged
new files their raw size and latched full at the first misfit (327,669 files,
215 GB raw, left out of a ~19 GB reserve allowance); it now charges the
extracted-text cap like the build and keeps the reserve. Live E: content after
relaunch: 690,868 -> 939,321 docs, sidecar 1.10 -> 1.39 GB, E:\wisp-memory and
other non-repo trees searchable. Selection version 7. Open: `loaded` inclusion
stats are only rewritten by a full build, so after a reconcile they still show
the old scope (root=400173, no dependency key); recompute or tag them stale.
Merged: `fix/usn-frn-identity` (7168a2c, 65253b4, a33885e). Records, WAL frames
and content docs carry the NTFS sequence (index v10, WAL v2 with v1 still
replayed, content v13). A known sequence that differs from an event's means
another occupant: delete/rename-old/data/info events are ignored, and
create/rename-new replaces the old occupant and its subtree; 0 = unknown keeps
the old behavior. The baseCheckpoint floor stays as defense in depth. Owner
action: relaunching the live E: service rebuilds content for v13 (~1 h or more
cold, content unavailable meanwhile). Follow-up: the standalone `direct`
builder does not capture sequences yet.

Merged 2026-10-18: `fix/usn-frn-sequence` (66f4b78): journal records below the
base watermark are skipped. Records are keyed by MFT record number without the
sequence, so an old WAL replayed over a rescan after a journal wrap deleted a
reused record (E:\bench\wt) and cut its subtree off at the root (phantom
`E:\.git` made content auto scope treat all of E: as one repo). The live E:
index was rebuilt from the MFT (backup in E:\bench
t\idx_bak_20261018).
Follow-up: store the sequence per record (format change) so identity never
depends on the watermark. `fix/overlay-incomplete-prefix` (a3c149a): incomplete
overlay-merged pages are cut at the base frontier and stay a rank prefix.
`content/auto-drive-root` (383df2c): auto scope adds the volume root as the
last-priority root (repos > configured > known folders > drive), with
outside-repo skips for dependency/venv trees, Cargo internals, browser profiles,
game libraries, AppData and module caches, and more binary extensions. E:
estimate: 647k -> 903k files (5.6 -> 11.2 GB text upper bound).

Merged 2026-10-19: `fix/direct-builder-seq` (3789143: direct builder persists FRN
sequence numbers), `fix/count-client-timeout` (8b1438a/9f28b8f: service deadline 2 s
inside the client wait, partial page marked incomplete; cancel only on pipe-disconnect
errors), `fix/content-extract-errors` (8df7264: per-reason extraction error counts in
`loaded --json`, vanished files not errors, files past MAX_PATH now open with the extended-length prefix,
tests stay out of the ProgramData service log). Then `feat/content-cursor-v2` (4e12172):
v2 cursor carries the last emitted record ID plus name/sidecar/delta identity;
resume rebuilds the candidate order and verifies about one page; identity change
restarts with `cursor_restarted`. Only single volume, non-relevance sort, empty
name overlay and content delta; other shapes keep v1. Then `feat/content-cursor-v2-overlay` (6dff32f): content pages no longer re-sort
by name/path whenever the name overlay is non-empty, which had discarded rank order
and symbol-first promotion on the live service; overlay rows insert by rank and the
incomplete-page frontier cut is promotion-aware. Then `fix/pipe-stop-handle-race`
(9332502): service stop racing a client connect could hand a handler a closed pipe
handle whose number was reused (stray writes into another file). Then `feat/content-cursor-v2-live`
(78ccdd9): v2 pages through a populated name overlay and content delta; the cursor
carries the last base FRN and the last row key (tier, rank, tiebreak); overlay rows
at or before the key are skipped; snapshotGen/delta changes no longer restart. A file
renamed across the cursor may be missed (bounded, never duplicated). Then
`perf/cursor-overlay-page-price` (cd0956c): a v2 resume with hidden base rows stops
base verification once base hits plus earlier overlay rows fill the page (page 2 of
a 200-file fixture: 198 -> 18 disk reads). Running: `fix/content-regex-underfill`
(regexfill: diagnose first; no standalone services, read-only live queries). Owner action: relaunch
the live service to see the 1,826 extract errors split by reason.

Merged 2026-10-17: `content/verify-frontier` (e271c17): parallel verifiers stop at
the first unverified candidate when the disk cap runs out (no page gaps), exact
concurrent read accounting, default 512 reads with no implicit deadline,
snippets/`--lines` share the cap and deadline, large-page cap fix. Regex
underfill fixed in 2138aa8 (default valve scales with the page: 512 reads per
32 slots, max 8,192; warm `-n 520` regex 684 ms, cold 9.7 s). The shared-name widen
is now multi-volume only (8283924; warm regex n=20 340 -> 131 ms). A large explicit
`-content-verify-budget` no longer over-reads (e3c6102, 30a1adb): completeness
window probes (unknown-probe widen; the full window forced by a pending WAL
overlay) are capped at the default page-scaled valve. Live E: regex n=100 with
budget 20000: 26 s -> 0.21 s. Open: cold first runs after a relaunch are slow
(regex n=100 6.9 s, n=520 10.6 s; Defender first-open cost); `count` past the client timeout now returns a partial page (8b1438a); incomplete
overlay-merged pages keep a rank prefix (fixed earlier, a3c149a);
cursor v2 merged 2026-10-19 (4e12172), overlay/delta pages still v1.

Merged 2026-10-16: `perf/footprint-shards` (b329cd3: lazy front-coded paths with a
locked restart cache, one composite doc table; heap after content attach
63.5 -> 45.3 MB on chrome/), `perf/query-transients` (231b7f0: pooled verify read
and fold buffers; 20 queries allocate 1662 -> 1184 MB, private 283 -> 237 MB).
Content attach heap on chrome/ today: 91 -> 45 MB. Next: doc table view (~7 MB,
~60 mutation sites), name index (~28 MB).

Merged 2026-10-16: symbol-named candidates first (191148d; content right-file-first
top-1 17% -> 70% default, 63% -> 86% relevance, 59% -> 92% sym:, BENCHMARKS.md),
`perf/footprint-rows` (zero-copy filter rows, heap 66 -> 63 MB on chrome/),
`perf/footprint-content` (63f43ee: zero-copy posting index;
chrome/ heap after content attach 91 -> 66 MB; measurement notes on the branch).

Merged 2026-10-15: `perf/content-regex` (2247ca3: per-rune cached accent fold,
byte-identical; parallel superset page; regex on the full superset; chrome/
`mutex_\w+` 2.47 s -> 0.40 s), `content/sym` (2ea5d74, G9: `sym:Name` definition search by
per-language patterns at query time; `sort:relevance` +1 for defining files),
`content/accent-fold` (69a061a, content format **v12**: terms,
grams and filter bits are accent-folded; `resume` finds `résumé`, an accented query
letter stays exact, regex stays accent-sensitive; v11 sidecars rebuild),
`content/fuzzy-grep` (ef020f3, `content~:word` / `content~N:word` over dictionary
variants, whole words, case-insensitive), `content/typo-scope` (da7def5: the typo fallback never corrects
a word present in any content dictionary or delta), `content/cursor` (607d38b, G10: `--cursor` / `next_cursor` offset
pagination bound to a query hash; each page re-runs up to offset+limit),
`content/time-budget` (a3a94f8, G10: opt-in `--budget 150ms` /
`budget_ms` stops new disk verifies at the deadline; partial page in order,
`incomplete` + `budget_exceeded`; counts ignore it; cursor pagination done in content/cursor),
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

- **Whole-drive coverage, build peak: CLOSED 2026-10-15** (`content/stream-postings`
  NOTES.md). Spooling posting runs instead of text: raw runs are ~1.02x of text
  (worse than the 0.27x text spool). Dropping the per-group spool reframe
  (shared sparse spool + hole punch): `chrome/` 354 MB, 6 shards, peak 0.70x ->
  0.96x (the shared spool lives to the end; holes do not free interleaved
  frames), so reverted. Coverage lever taken instead: reserve cap 20 -> 10 GiB
  (65ff317), ~6x build headroom on E:.
- **Build speed: profiled 2026-10-15** (`perf/build-profile` NOTES.md, 799edc8). Cold
  builds are bound by Defender real-time scanning on first open (~7-13 ms per
  file on NVMe; warm opens 0.03 ms): blink/renderer/core 69.6 s cold vs 6.6 s
  warm, CPU work ~6 s. 16 open workers only ~1.5x. Levers: (1) owner decision on
  a Defender process exclusion for the service binary (~10x cold); (2) decouple
  I/O concurrency from CPU workers: TRIED (`perf/build-io-concurrency` 871319b,
  not merged): 175 vs 179 files/s cold, the scan rate is the ceiling;
  (3) tokenize+emit CPU (~5 s of 6), only matters warm or after (1).
- **Fold cost under no stored text:** a fold re-reads every doc in the shard
  from disk (5.6 ms/doc). Keep shards small, or fold from postings only.
- **Delta-varint spill runs: DONE** (562fe42). Grouped per key with
  delta-varint docIDs. blink/renderer/core (79.9 MB extracted, 2 shards): spill
  run peak 77.1 -> 24.3 MB (-69%), total build disk peak 174.2 -> 121.4 MB
  (2.18x -> 1.52x incl. the fixed 128 MiB scratch slack), final sections
  byte-identical (sha256 34ee9a5d...). Spill runs sit inside the fixed
  per-shard scratch slack (2 x 64 MiB), not the 0.8x per-extracted-byte peak
  multiplier, so this mainly cuts build IO/scratch; selection is unchanged.
  The live E: auto-scope build (640,581 docs) drops nothing for budget, so E:
  coverage is now bound by scope/inclusion rules, not disk.
- **Host-sensitive reconcile tests:** `TestContentAttachReconciles{WorktreePolicyToMain,
  WorktreePolicyToAll,VanishedWorktree}` end `stale` when TEMP is on C:
  (nearly full) and pass with TEMP on E: (fails back to at least 878b6d2, so
  environmental). Free space/reserve is not the cause (reserve is 0 in tests).
  Run suites with TMP/TEMP=E:\bench\scratch\tmp until found.
- **Typo fallback scope:** DONE (`content/typo-scope`, da7def5).
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
- **Dense-text spool: CLOSED 2026-10-19** (`content/spool-level` NOTES.md, 9868e01).
  Selection already charges the measured staged spool bytes
  (`content_service_build.go` ~990, credited back in `content_space.go` ~166);
  the ratio only moves the peak multiplier, whose 0.8 floor binds below r = 0.325.
  Measured per-doc ratios on E:\bench\chromium: code 0.244 (Fastest) vs 0.231
  (Default, 2.2x slower encode); dense/minified/generated 0.260 vs 0.242. Default
  would select the same docs. Only a corpus with aggregate r > 0.35 gains
  (the earlier 1.000 -> 0.637 sample would gain 28%); rerun the benchmark if one shows up.
- **JSON-lines stdio mode (G12):** optional and low priority. **No MCP
  server**: the owner ruled it out on 2026-10-13.
- **Live restart validation:** the deferred USN catch-up (restart serves in
  about 0.3 s) was measured on a copy. Validate it on the live E: service at
  the next relaunch (elevated), since the live load used to take about 40 s.
- **Content accent folding:** DONE (`content/accent-fold`, v12).
- **Ranking store scope:** DONE (`feat/ui-input-and-user-signals`). Per-user
  `ranking_signals-<SID>.json` files next to the service state, keyed by the
  pipe client's SID, ACL-restricted to SYSTEM + Administrators + owner;
  `open`/`recent`/ranking use the caller's store; legacy shared file migrates
  to the first opener. See `search-syntax.md` "Ranking signals".
- **Single-line JSON follow-up:** the 1 MiB / 10 MiB caps and JSON skip from
  `content/inclusion-policy` should cover the 11.4 MB single-line
  `tokenizer.json`. Verify on the next real-index recall run.

## Owner actions (not agent work)

- **Defender and build speed (owner deprioritized 2026-10-15: features first):** content builds spend ~90% of cold
  build time waiting on Defender scanning each file at first open. A process
  exclusion for the seekfs service binary would remove it (it only reads
  files), at the cost of not scanning files as seekfs opens them. Admin-only.
- **Relaunch the live E: service on integ >= 69a061a** (elevated). Also triggers the v12 content rebuild (accent folding). The build
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
