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
