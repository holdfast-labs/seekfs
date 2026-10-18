# Repository Instructions

## Using seekfs in this repository

- seekfs searches indexed file names and paths, not file contents. For
  text-content search, definitions, import references, or exact line
  matches, use `rg`.
- Full-text content search is under development on the `content-search`
  branch and off by default. With `SEEKFS_CONTENT_SEARCH=1`, the service
  builds and serves a scoped content index. See `docs/CONTENT_SEARCH.md`
  for design, status, and the agent continuation handoff.
- Prefer `seekfs search`/`seekfs count` against the resident service for
  file discovery by name or path. Put flags before the query, and quote
  multi-term queries.
- Use a plain indexed filename/path term for discovery (do not add shell
  wildcards such as `*`); constrain results with `--under`, and prefer
  adding a file term/filter over a directory-only `-path` query when the
  intent is to list a tree.
- For repository-local file discovery, scope searches with `--under` to
  the repository so results stay relevant and fast.
- If `seekfs` is not on PATH in a fresh shell, call the repository binary
  directly.

## Reference tools: fsearch and fff

- The product bar is **as fast as fsearch, as accurate as fff**, with a small
  index so content search can cover the whole drive.
- "fsearch" always means **Noah Dunnagan's fsearch**
  (<https://github.com/noahdunnagan/fsearch>), never the unrelated Linux GTK
  app cboxdoerfer/fsearch. "fff" means **dmtrKovalenko/fff**
  (<https://github.com/dmtrKovalenko/fff>, fff.nvim).
- Noah's fsearch supports indexed content search (smart-case and line-based
  content regex). Its content index is a trigram index plus a per-file filter
  that narrows candidates; matching text is read **fresh from source files on
  disk**, and files edited since the last index sync are read directly. Content
  search is therefore **not index-only**: candidate-file disk verification is
  consistent with the reference architecture, so evaluate candidate
  selectivity, bounded-page/verification behavior, correctness, and latency
  rather than assuming any source-file read violates the design goal. Check the
  relevant source revision (e.g. `src/content.rs`, `src/query.rs`) before making
  implementation-specific claims; do not attribute the GTK project's limits to
  Noah's.
- `docs/REFERENCE_TOOLS.md` summarizes how each one works and lists seekfs's
  current gaps against them. Read it before working on speed, ranking, typo
  tolerance, content coverage or index size, and update its gap list when you
  close one.
- Do not build an MCP server for seekfs; the owner ruled it out.
- `docs/WORK_QUEUE.md` is the single list of open work (running agents,
  queued items in order, follow-ups, owner actions). Update it when you
  finish or discover work.

## Release builds (`dist/`)

- `dist\seekfs-windows-amd64\seekfs.exe` is the Windows service binary (the
  installed `seekfs` service runs this path) and the CLI. `seekfs-ui.exe` in
  the same dir is the desktop UI and embeds its own copy of the service: it
  spawns `service` from its own exe and enforces exe-path + hash identity on
  the pipe. Always rebuild and ship both together; never mix versions.
- Stop the `seekfs` Windows service before replacing binaries (locked file).
- Full release build (wipes and recreates the target dir, rebuilds both
  exes, refreshes README/LICENSE/NOTICE + tracked docs, re-zips):
  `powershell -ExecutionPolicy Bypass -File scripts\build.ps1 -Version "main-<short-sha>-local" -OutDir dist`.
  Back up anything worth keeping to OUTSIDE `dist` first — the wipe deletes
  it (e.g. `*.prev` rollback copies).
- Single-binary fast path (preserves the rest of the dir, same flags as
  `build.ps1`):
  `go build -trimpath -ldflags "-s -w -X main.version=<ver> -X main.commit=<sha> -X main.date=<utc>" -o dist\seekfs-windows-amd64\seekfs.exe ./cmd/seekfs`.
- `lowmem` is a runtime mode (service `-lowmem` flag / `SEEKFS_MEMORY_MODE`
  env), not a build tag. `seekfs service` defaults to lowmem when the env
  is unset/empty (`resident` opts out); the install/setup-service/launch
  path passes no flag, so it gets the default. The UI always spawns with
  `-lowmem` explicitly.
- Verify after building: `seekfs.exe version` shows the stamp;
  `seekfs loaded` after launch shows the flavor; re-run a known query with
  `--json` and check `search_ms`/`source`.
- Run only one launcher at a time: the UI-managed standalone service and
  the installed Windows service share the default pipe and will fight over
  identity. Pick the UI or `start-service`, not both.
