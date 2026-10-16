# Reference tools and gaps

seekfs aims to be **as fast as fsearch and as accurate as fff**. These are the
two reference implementations. Read this file before working on search speed,
ranking, typo tolerance, content coverage or index size. Compare against these
projects by design and documented behavior; head-to-head benchmark runs on this
machine are not required.

## Which projects these are

- **fsearch** means **Noah Dunnagan's fsearch**:
  <https://github.com/noahdunnagan/fsearch> (Rust, macOS daemon; a Windows port
  is <https://github.com/satwiksamanta77/fsearch-windows>). It is *not*
  cboxdoerfer/fsearch, the unrelated Linux GTK app inspired by Everything.
- **fff** means **Dmitriy Kovalenko's fff**, the fast file finder:
  <https://github.com/dmtrKovalenko/fff> (Rust core plus fff.nvim, MCP server,
  C/Node/Python bindings; Linux, macOS and Windows). As of October 2026 it
  ships only nightly builds (`0.11.1-nightly.*`).

## fsearch: what makes it fast and small (per its README)

- **Content index:** a trigram index plus a per-file filter. A search reads
  only files likely to match. **No file text is stored**: matches are re-read
  from disk, and files edited since the last sync are read directly.
- **Identical files** are indexed once. Deleted entries are compacted away.
- **Inclusion policy:** text files up to 1 MB (10 MB inside git repos), minus
  what git ignores. It skips lockfiles, JSON of 100 KB or more,
  minified/bundled assets and build-output folders outside repos. **Linked git
  worktrees are skipped; only the main checkout is indexed.** Users can add
  folders to `content-skip.txt`.
- **Freshness:** a native change feed (FSEvents). Changes are visible in about
  15 ms, and an edited file is greppable in about 1.3 ms. A restart replays
  only what changed. The first search in an unindexed folder indexes that
  folder first.
- **Footprint:** a 21–35 MB daemon that returns memory when idle; 0.77–0.94 GB
  peak while indexing; about 600 MB of index for the whole Chromium tree.
- **Speed (author's medians, M4):** name lookup 0.05–0.12 ms; first page of
  grep 0.33–0.65 ms. It searches 99.8–99.9% of the files ripgrep would.
- **Names:** fuzzy matching that forgives a typo at any letter, two in words of
  8+ letters. A per-name letter bitmap prunes names before scoring. Several
  words can spell one name, and accents are folded.
- **Query language:** `ext: type: kind: in: size: mtime: re: path: grep:
  regex: sym: limit: git:`, `'exact ^prefix suffix$ !exclude`, globs such as
  `src/**/*.{c,h}`.
- **Usage signals:** `open` events boost what you open (`"boost": true`),
  `near` favors files close to a path, and `recent` lists recently used
  files. It reports "right file first" rates: names 93%/97%, two-typo
  74%/93%.
- **Integration:** JSON lines over a socket or stdio, an MCP server
  (`find_files`, `grep`, `multi_grep`) and a Rust crate.

## fff: what makes it accurate (per its README)

- **Matching:** a frizbee-derived SIMD fuzzy matcher that tolerates dropped
  and reordered characters. Zero-match queries retry fuzzy.
- **Ranking:** frecency (an access score plus a modification score per file);
  git status for each file, cached via libgit2, with sorting and filtering on
  it; a boost for files in the last N commits (default 10, skipping commits
  touching more than 50 files); a query-history combo boost (at least 3
  repeats, multiplier 100); current-file proximity; and a bonus for lines that
  look like definitions (`fn struct class def impl`).
- **Grep modes:** SIMD memmem for plain text, Rust `regex`, fuzzy grep
  (Smith-Waterman per line) and multi-pattern OR (SIMD Aho-Corasick). Also
  smart case, context lines, cursor pagination, a per-query time budget
  (150 ms default) and extension/file filters.
- **Index:** a resident in-memory content index of about 360 bytes per file
  (about 36 MB per 100k files), and a background watcher. ripgrep is still the
  right tool for a one-off shell grep.
- **Integration:** MCP tools (`ffgrep`, `fffind`, `fff-multi-grep`), editor
  pickers and bindings.

## seekfs gaps (as of 2026-10-13, integ/content-main)

Coverage and size (top priority: small index, content search everywhere):

1. **Stored text** is about 58% of the v9 index. fsearch stores none and
   verifies from disk. **Owner decision (2026-10-13): adopt the no-stored-text,
   verify-from-disk model as the default.** Agent work: `content/filter-tier` (tier 2 with no
   stored text, measured as a possible default).
2. **The build's transient spool peak** (1.5× extracted) caps coverage, while
   the final index is about 0.33×. Agent work: `content/spool-peak`.
3. **Inclusion policy:** DONE (`content/inclusion-policy`, §6z). We index
   only the main checkout (`[content] worktrees = "main"`, `all` opts in),
   identical files once (content-hash dedupe, CXAL section, v10), text
   capped at ~1 MiB outside repos (~10 MiB inside, explicit caps still
   override), and skip lockfiles, JSON ≥ 100 KiB, minified/bundled assets
   (names plus sampled avg line length > 2048), build-output dirs outside
   repos, well-known cache trees, and a user `content-skip` list
   (`plugin config content --skip`). Measured on Chromium: selection
   468,772 files / 3.37 GB → 468,212 / 3.21 GB; full build 464,032 docs /
   800 MB → 463,275 / 738 MB (26,195 aliases saving 25.7 MB of text);
   E:\bench: 479,218 / 3.54 GB → 468,212 / 3.21 GB with 27 linked
   worktrees contributing nothing. Recall inside the rules is 100% vs
   ripgrep on 10 terms (only documented extractor-limit diffs). Every
   decision is tallied (CXIS → `loaded --json` `inclusion`, `--estimate`,
   build lines).
4. **Freshness:** name side mostly done (`perf/freshness-footprint`).
   The USN wake is event-driven (blocking journal read, ~0 idle CPU);
   the overlay now publishes before the WAL fsync, so    fsync latency is
   off the visibility path (durability unchanged — crash replays from
   the journal). Walk volumes: debounce 250 ms → 50 ms for name
   changes; write-only batches rebuild on a 500 ms coalescing debounce
   so Size/ModUnix stay fresh for filters, sorts, and the content walk
   reconcile (200-file scratch: create 685→~100–350 ms, rename 360→82
   ms, delete 642→59 ms, write-only edit → `size:` ~600 ms); the batch
   classifier fails safe (any framing doubt rebuilds). A restart
   replays only `[checkpoint, head)` and now
   serves the mmapped base + WAL overlay while the catch-up drains
   behind queries (1.46M copy: base load ~0.3 s, first query ~1.1 s;
   the live 40 s catch-up block moves off the query path). Remaining:
   edited-file direct reads at query time (content side, with
   `content/filter-tier`) and elevated-box validation of USN
   end-to-end latency (~15 ms target).
5. **Footprint:** name side instrumented and partly cut
   (`perf/freshness-footprint`, `loaded --json` `cache_bytes` /
   `block_cache_bytes`). The idle loop now drops the decoded
   posting-block LRU (default cap 128 MB) and per-volume query caches
   before `FreeOSMemory` (1.46M names, content-disabled: heap 73→50 MB,
   working set 251→201 MB → ~72 MB idle). `lowmem` vs `resident`
   unchanged. Still open: the ~500 MB E: working set is dominated by
   the content build (name-only chromium 556k is ~67 MB WS); further
   content-side wins belong to `content/filter-tier`; fsearch's 21–35 MB
   daemon remains the far bar (mmap file pages always count in WS).
6. **Lazy first use:** name side done (`perf/freshness-footprint`). A
   search scoped `--under` a real folder with no index loaded answers
   from a bounded folder-first walk (100k entries / 2 s, read-only)
   instead of erroring; counts and folder-less queries keep the
   no-index error. No background folder index is started on the query
   path (it would race the budgeted builder and stall the query).
   Content-side on-demand (unindexed folder has no content search until
   the budgeted build reaches it) stays open with `content/filter-tier`.

Accuracy and ranking (fff parity):

7. **Usage signals: done (`perf/ranking-signals`).** `seekfs open <path>
    [--query q]` records access events (the UI's Open action reports them
    with the current query); `seekfs recent` lists recently opened files. A
    bounded persisted frecency store (top 5000 files / 2000 queries, 30-day
    half-life, `ranking_signals.json` next to the service state, local only)
    plus a normalized-query → pick combo boost (≥ 3 repeats, fff's
    threshold). `near:<path>` / `--near` ranks fewer path hops first. All
    blend as within-band tie-breakers (see 13).
8. **Git awareness: done (`perf/ranking-signals`).** A `git:` filter
    (modified, staged, untracked, ignored, conflict, clean, tracked;
    comma-OR, `!` negation) with per-hit `git_status` in `--json`. Status
    resolves per path from `.git/index` + HEAD via go-git (already in
    go.mod) — never a `git` spawn, never a worktree walk; per-repo cache
    invalidated by HEAD/index/packed-refs/branch-ref stat checks (≤ 1 batch
    per repo per query; the USN/walk feed observes the same `.git`
    changes). Ignored uses the existing contentGitIgnore matcher. Boosts: git
    dirt (dirty first within a band) and recent-commit files (last 10
    commits, skipping commits touching > 50 files, small tree diffs only).
    Approximations: dirs never report modified/staged/conflict; unreadable
    repos resolve unknown (positive predicates fail closed); short/broad
    terms on tiny direct indexes were lossy via the planned term posting
    (fixed in `fix/short-term-postings`: the direct LOWR writer now emits the
    canonical split per-token layout, and corrupt sections fall back to exact
    decoding).
9. **Definition search: DONE (`content/sym`).** `sym:Name` finds definition
    lines by source extension without changing the v12 index; plain identifier
    `content:Name` is intended to favor defining files.
10. **Grep features:** indexed fuzzy word grep (`content~:word`, optional
        `content~2:word`) done (G10); no Smith-Waterman scoring. Multi-pattern OR
        grep remains open. **Per-query time budget and cursor pagination: done (G10):**
      content searches accept `--budget <duration>`; search JSON supports offset
      cursors through `next_cursor` / `--cursor` (each page reruns the query up to
      its offset, so cost grows with depth).
     **Line output and context lines: done (G10, `content/grep-lines`):**
     `seekfs search --lines [-C N | -A N -B N] [--max-per-file N]` prints
     ripgrep-style `path:LINE:text` / `path-LINE-text` with `--` separators.
     **Smart case for content: done (G10).** Uppercase literal content patterns
     enable query-wide sensitivity unless explicitly overridden by `case:`;
     regex escapes and classes do not trigger it. Regex is Go RE2 with no lookaround.
11. **Query language: done (`perf/name-lessons`, §G11-notes).**
    `'exact`, `^prefix`, `suffix$` name anchors (always on the file name;
    stripped text joins Terms so posting lanes engage); `**` path globs
    with brace sets (`glob:src/**/*.{c,h}`, bare `src/**/*.{c,h}`,
    `**/OWNERS`; relative patterns match at any depth; `*.{ext}`
    filename sets ride the ext postings as an ext OR); `in:<dir>` alias
    of `--under` (nested roots collapse, disjoint are impossible,
    `!in:` excludes); `"quoted"` phrases (one literal term, byte-exact);
    accent folding for names (`resume` finds `résumé`, NFD/NFC-insensitive,
    comparison-side only, no index change; `case:`/quoted stay exact).
    Multi-word queries already AND across separators/camelCase in any
    order; path-vs-name spanning needs `-path` (documented). Typo budget
    already matches fsearch (1 edit, 2 for 8+ letters, Damerau
    transpositions; `chromium_eval.py` one-typo 99.5% / two-typo 93.0%
    unchanged). `--current <path>` demotes the open file within its band
    (fff `current_file`; sets/counts unchanged). Letter bitmaps: adopted
    in the fuzzy verifier, the memo lane, and a new regex-verify
    prefilter (provably-required disjunct literals; −13% sparse, no loss
    dense); rejected for substring scans (weaker than the substring
    precheck), 1–2 char terms (vacuous), and per-record path masks
    (needs path reconstruction; memo dir-bits cover it). Nothing
    persisted: `.gsi` format untouched. Parity vs `01abdf3`:
    0 set/count diffs (76 queries × search 20/300/count); recall 100%;
    latency table in `docs/BENCHMARKS.md`.
12. **Integration:** no JSON-lines stdio mode (optional, low priority).
    **Out of scope: an MCP server.** The owner ruled it out on 2026-10-13;
    do not build one, even though fsearch and fff ship MCP tools.
13. **Reported metrics: right-file-first added (`perf/ranking-signals`).**
    One-typo / two-typo top-1 stay 99.5% / 93.0% (N=200 seed 42 / N=100 seed
    7, chromium). New path-level metric on 100 labeled chromium targets at
    `-n 100` (`E:\bench\scratch\ranking\rightfile.py`, scratch-only): plain
    top-1 39.0% → 66.0% after opening half the targets (opened 20→47/50,
    control 19→19), partial top-1 2.0% → 13.0%; typo shapes unchanged by
    usage (fuzzy distance bands never invert). See docs/BENCHMARKS.md.

Where seekfs is already ahead or equal: NTFS USN change feed and resident
service on Windows; 100% recall against ripgrep inside indexed roots (real E:
index, 2026-10-13); name typo top-1 99.5% / two-typo 93.0% on Chromium; regex
trigram prefilter; exact counts; size/attrib/date/sort filters.
