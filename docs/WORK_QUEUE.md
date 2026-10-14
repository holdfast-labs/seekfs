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
`fix/cli-silent-empty`, `content/idle-governor`, `content/spool-peak`, `perf/ranking-signals` (G7, G8, G13).

| Branch | Worktree | Scope |
|---|---|---|
| `test/suite-speed` | suitespeed | `cmd/seekfs` suite under ~5 min; harden `TestHeadlessFrameCost`; gofmt drift |
| `content/filter-tier` | tier2 | **Owner decision 2026-10-13: fsearch model is the default.** No stored text; trigram postings plus a per-file filter; matches and snippets verified from disk; edited files read directly. Stored text only as a justified opt-in (G1, G4 partial, §6y) |
| `content/inclusion-policy` | inclusion | Queued item 1: main-checkout-only worktrees, lower caps, lockfile/JSON/minified/cache skips, user skip list, content-hash dedupe (G3, §6z) |

## Queued, in order

0. **Correctness first: lossy short-term postings — done (`fix/short-term-postings`,
    queued item 0).** Short-term `planned:term:X` postings were lossy when used
    as a required source (`go type:file`, `go --exists` underfilled): the
    `seekfs direct` LOWR section was interleaved per record instead of split
    per token, so every token-addressed lower-name read on a mapped direct
    index was garbage. Fixed the writer to the canonical split layout, made
    the reader discard corrupt sections (exact fallback), and swept the same
    required-source rule everywhere (exact-name shortcuts, dropped postings,
    component self-hits, volume-satisfied terms). The ranked-posting filename
    lane was audited exact; its reported "rank-neighbor false positives" were
    auto-fuzzy top-up on partial exact sets, now zero-exact-only (explicit
    `~`/`--fuzzy` still tops up). Recall 100%, one-typo 99.5% / two-typo
    93.0% unchanged; chromium parity 0 diffs.
0b. **Static ranking accuracy (G13).** With no usage history, plain-name
   right-file-first top-1 is only 39% (partial-name 2%) on 100 labeled
   Chromium targets (`E:/bench/scratch/ranking/rightfile.py`). fsearch
   reports 93–97% for names (different metric). Improve the static ranker:
   exact basename first, shallower or shorter paths, source over generated
   files, test/vendor demotion, and so on. Typo top-1 (99.5% / 93.0%) must
   not regress.
1. **Grep features (G10, G9, G11).** Plain-literal grep via SIMD memmem and
   multi-pattern OR via Aho-Corasick (fff); fuzzy grep (Smith-Waterman per
   line); context lines; a per-query time budget (fff default 150 ms) with
   cursor pagination; smart case for content; ripgrep line semantics for
   regex (fsearch); `sym:` definition search and a definition-line bonus;
   extension and single-file filters for grep.
2. **Name-search lessons (G11).** A per-name letter bitmap that rules out
   most names before scoring (fsearch); several words spelling one name
   (fsearch); accent folding; `'exact`, `^prefix`, `suffix$`, `**` and brace
   globs, `in:` alias (fsearch); typo budget of one edit, or two for words of
   8+ letters (compare with ours); current-file deprioritization (fff).
3. **Freshness and footprint (G4, G5, G6).** Name-side changes visible in
   milliseconds (fsearch about 15 ms; dirty content reads land with the
   filter tier); on-demand indexing (searched folder first, fsearch); a
   restart replays only what changed; service resident memory (about 500 MB
   for 1.46M names, versus fsearch's 21–35 MB daemon), returned to the OS
   when idle; startup load time (about 40 s for 1.46M entries); a
   memory-mapped content cache (fff).

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
- **Save-time peak:** the write guard checks free space before each shard save,
  but a save briefly holds both the section files and the output (§6x
  residual). Account for it, or stream the save.
- **Dense-text spool:** the fastest zstd level stores high-entropy text
  uncompressed (1.000x). The default level gets 0.637x, about 28% more
  coverage on dense corpora for roughly +4 s of serial encode (§6x).
- **JSON-lines stdio mode (G12):** optional and low priority. **No MCP
  server**: the owner ruled it out on 2026-10-13.
- **Ranking store scope:** `ranking_signals.json` lives next to the service
  state (the LocalSystem service holds one store for all local users). Decide
  whether it should be per-user before shipping on multi-user machines.
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
