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
   verifies from disk. Agent work: `content/filter-tier` (tier 2 with no
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
4. **Freshness:** edits go through the USN delta, drain and fold queue, which
   adds seconds of lag. Read changed or dirty files directly at query time.
5. **Footprint:** the service uses about 500 MB resident on E: (1.46M names)
   and about 3.1 GB for 23.7M entries. Loading took about 40 s for 1.46M
   entries.
6. **Lazy first use:** an unindexed folder has no content search until the
   budgeted build reaches it. Index on demand, folder first.

Accuracy and ranking (fff parity):

7. **No usage signals:** no open/frecency tracking, no `near`, no
   recently-used list, no query-history boost.
8. **No git awareness:** no `git:` filter (modified, staged, untracked …), no
   git-status boost, no recent-commit boost.
9. **No `sym:`** (definition search) and no definition-line bonus in content
   ranking.
10. **Grep features:** no fuzzy (Smith-Waterman) grep, no multi-pattern OR
    grep, no context lines, no per-query time budget or cursor pagination, and
    no smart case for content. Regex is Go RE2 with no lookaround.
11. **Query language:** no `'exact`, `^prefix` or `suffix$` operators, no
    `**` path globs with brace sets, no `in:` alias. Accent folding for names
    is unverified.
12. **Integration:** no MCP server (`find`/`grep`/`multi_grep` tools for
    agents) and no JSON-lines stdio mode.
13. **Reported metrics:** we measure one-typo and two-typo top-1 on Chromium
    (99.5% / 93.0%). Add a "right file first" metric for plain name queries,
    matching fsearch's report.

Where seekfs is already ahead or equal: NTFS USN change feed and resident
service on Windows; 100% recall against ripgrep inside indexed roots (real E:
index, 2026-10-13); name typo top-1 99.5% / two-typo 93.0% on Chromium; regex
trigram prefilter; exact counts; size/attrib/date/sort filters.
