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
