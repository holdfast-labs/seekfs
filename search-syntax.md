# seekfs search syntax

## Supported

`seekfs` supports a small agent-friendly query language:

- Case-insensitive substring matching by default.
- Accent-insensitive matching by default: `resume` finds `résumé`,
  NFD/NFC-insensitive. `case:` and `"quoted"` phrases stay byte-exact.
- Whitespace-separated terms are ANDed. Several words may spell one name
  across separators and camelCase (`chrome main` finds
  `chrome_main_delegate.cc`, in any order); use `-path` when terms span
  path vs name (`src main`).
- Name anchors: `'exact` (whole file name equals), `^prefix` (name starts
  with), `suffix$` (name ends with), `^both$` (both ends). Anchors always
  name the file, in filename and `-path` modes alike.
- Quoted phrases: `"foo bar"` is one literal term (spaces kept), with no
  accent folding and no typo correction.
- OR alternatives within a term with `a|b` (for example `ext:png|jpg`).
- Negation with `!term` or `-term` (for example `main !test`).
- Name search by default.
- Full-path search with `-path`.
- Result limit with `-n`.
- Count-only mode with `count`.
- Extension filters with `ext:go`.
- Directory/path segment filters with `dir:src`.
- Subtree scoping with `in:<dir>` (alias of `--under`; multiple roots must
  nest, disjoint roots match nothing; `!in:<dir>` excludes a subtree).
- Glob filters with `glob:*.py`. Brace sets choose alternatives
  (`glob:*.{c,h}`); a pattern containing `/` or `**` matches the full path
  (`glob:src/**/*.{c,h}`, `glob:**/OWNERS`; relative patterns match at any
  depth). Bare `src/**/*.{c,h}` works too.
- Regular expressions with `regex:<pattern>`.
- Size filters with `size:>100mb`, `size:>=1gb`, `size:<4k`, or `size:1024`.
- Modified-date filters with `dm:today`, `dm:yesterday`, `dm:thisweek`,
  `dm:lastweek`, a duration such as `dm:24h` / `dm:7d`, or a date `dm:2026-05-01`.
- Case-sensitive matching with `case:` or `--case`.
- Type filters with `type:file` and `type:dir`.
- Git status filters with `git:modified`, `git:staged`, `git:untracked`,
  `git:ignored`, `git:conflict`, `git:clean`, or `git:tracked`
  (comma-separated for OR, e.g. `git:modified,staged`; `!git:ignored` to
  exclude). Status resolves from `.git/index` + HEAD with pure-Go reads (no
  `git` spawn); matching hits carry a `git_status` field in `--json` output.
- Proximity anchor with `near:<path>` (or `--near <path>`): within a
  match-quality band, files fewer path hops from the anchor rank first. A
  file anchor measures from its own directory, so `near:<file>` favors its
  siblings.
- Current-file demotion with `--current <path>`: the file the user already
  has open ranks last within its match-quality band (fff `current_file`).
  Sets and counts never change (only order).
- Workspace scoping with `--under <path>`.
- Stale-result verification with `--exists`.
- Recency filters with `--recent 24h` or `--modified-after 2026-05-22`.
- Ranking bias with `--cwd-bias` or `--root-bias <path>`.
- Proximity anchor with `--near <path>` (see `near:` above).
- Fuzzy matching: when a single-term query returns no results at all, close
  matches (Damerau-Levenshtein distance, accent/fullwidth-folded) are
  returned, ranked by edit distance, then prefix alignment, then normal rank.
  For multi-term queries that return no results
  at all, terms that match nothing (or almost nothing) on their own are
  replaced by their best close-match variants and the query is re-run once
  (`reprot pdf` finds `report.pdf`, `discovery llog` finds `discovery log`);
  common terms stay as hard constraints. `term~` or `--fuzzy` opts in
  explicitly, regardless of how many exact results were found. Terms shorter
  than 3 characters are not fuzzed. Name anchors (`'exact`, `^prefix`,
  `suffix$`) and `"quoted"` terms never fuzzy-correct: they describe
  exact-tier intent and stay hard constraints. The typo budget is one edit
  per word, two for words of 8+ letters (fsearch rule); adjacent
  transpositions cost one (Damerau) and keep working.

## Examples

```powershell
.\seekfs.exe search -service -path -n 20 "ext:go dir:cmd main"
.\seekfs.exe search -service -path -n 20 "glob:*.py"
.\seekfs.exe search -service -path -n 20 "regex:README\\.(md|txt)"
.\seekfs.exe search -service -path --under F:\git\seekfs "type:file ext:go"
.\seekfs.exe search -service -path --exists --recent 24h "ext:md"
.\seekfs.exe search -service -path --cwd-bias "main"
.\seekfs.exe search -service --json -n 20 "edit.go git:modified"
.\seekfs.exe search -service -n 20 "deep.go near:F:\git\seekfs\src"
.\seekfs.exe open F:\git\seekfs\src\main.go --query "main"
.\seekfs.exe recent -n 20
.\seekfs.exe count  -service -path "type:dir docs"
```

## Notes
- `ext:` matches exact file extensions without the leading dot.
- `glob:` matches the file name, unless the pattern contains `/` or `**`,
  in which case it matches the full path (relative patterns at any depth).
- Bare wildcard filename tokens such as `*_test.go` are treated as filename
  globs too; use `glob:` when you want that behavior to be explicit.
  Bare `**` paths (`src/**/*.{c,h}`) become path globs.
- `dir:` is a path substring filter.
- `parent:` matches entries whose immediate parent directory name equals the
  supplied value; it accepts one directory name, not a path or glob.
- `attrib:` matches file attributes. Supported flags are `R`, `H`, `S`, `D`,
  and `A`; combined flags such as `attrib:HS` require all listed bits.
- `regex:` evaluates against the normalized full path.
- Omitting the `search` subcommand is accepted for search-like invocations, for
  example `seekfs --under F:\git\seekfs "main.go"`.
- `--exists` calls `os.Stat` and is slower, but filters stale index entries.
  Rootless service `--exists` uses a complete global verification fallback and
  is intentionally outside the bounded R5 planner performance claim.
- `size:` units are 1024-based (`kb`, `mb`, `gb`, `tb`; the trailing `b` is
  optional). `size:`, `dm:`, and `attrib:` require an index built with file
  metadata; NTFS service indexes capture this from the MFT. Querying them
  against an index that lacks required metadata returns a clear error rather
  than no results.
- Unsupported `name:` style filters are rejected with an error instead of being
  treated as literal text.

## Ranking signals

`seekfs open <path>` (or opening a result in the UI) records a usage event
in a small local store (`ranking_signals.json` next to the service state;
top 5000 files / 2000 queries, 30-day score half-life, local only). Signals
blend into name ranking as bounded within-band tie-breakers:

- Bands, best first: for single-term filename queries, exact name, name
  prefix, name substring, then matched-via-other-constraints; multi-term and
  `-path` queries band by file name; fuzzy top-up entries band by distance
  group (distance order never inverts). A weak band never outranks a strong
  one, no matter the signals.
- Within a band: same-query picks (≥ 3 repeats), `near:` hop distance,
  frecency, git dirt (dirty before clean), recent-commit membership,
  modification recency, `--current` demotion (the open file sinks to the
  band bottom), then path.
- Result sets and counts never change (only order), unless a `git:` filter
  narrows them. Explicit `sort:` and content queries never reorder. With an
  empty store and no `git:`/`near:`, order is byte-identical to the base
  ranker.
- `git:` status needs `.git/index` + HEAD; repos that cannot be read resolve
  as unknown (positive predicates do not match). Directories never report
  modified/staged/conflict (file-only properties).

## Not Implemented Yet

- Everything-compatible ranking.
