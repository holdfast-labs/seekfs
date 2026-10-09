# seekfs v1.11.0

This release combines faster resident searches, deferred result materialization,
and the minimal prompt-style desktop UI.

## Search

- Distinct filenames are scored once per query with character masks and cached
  term bits. Ancestor bits serve supported path queries without reconstructing
  every path. Heap-backed compact indexes load into deduplicated packed storage.
- Broad component queries keep record IDs through bounded local rank heaps,
  then materialize at most `limit × volumes` base entries for global sorting.
  Unsupported queries retain the existing verifier, and overlays are merged
  with hidden base records excluded.
- Corrected volume/root prefix handling, filename-only prefix matches,
  directory aggregate-size filters, memo identity lifetime, and cancellation
  during name scoring, directory folding and global candidate verification.
- Added memory reporting for the identity and memo arrays. Low-memory mode
  leaves this lane disabled; `SEEKFS_NAME_MEMO=0` also disables it.

## Desktop UI

- Minimal dark prompt, compact logo header, footer counts and index health,
  quieter result rows, and context-menu shortcut hints.
- Ctrl+K or `/` focuses search; Escape clears a nonempty prompt. The clear
  button resets the query and sort and returns focus to the prompt.
- Reuse the existing multi-resolution icon for the favicon, excluding a 2 MB
  raster-wrapper SVG from the desktop executable.

## Performance evidence

On a synthetic packed 50,000-file fixture with a warmed broad path query and
limit 20, the new memo/deferred path measured 4.85 ms versus 11.96 ms for the
existing verifier. Allocated bytes fell from 41.1 MB to 0.82 MB per query.
These are sample microbenchmark results, not a verified 10x end-to-end gain
or a measured real-volume resident-memory reduction. See
[the review and reproduction command](SEARCH_MEMO_REVIEW.md).

## Included since the last published release

The unreleased [v1.10.3 changes](RELEASE_NOTES_1.10.3.md) are included: the
`direct` command alias, simplified service environment names, watch command
paths with spaces, refactoring, dependency updates and expanded CI gates.
Remove the deprecated `-skip-startup-sync` flag from old service launch scripts.

## Upgrade

No index rebuild is required. Replace both executables and restart the service
to load the new resident engine. Existing query syntax and index formats remain
compatible. The Windows binaries are unsigned. The release includes the CLI,
desktop UI, documentation, and a SHA-256 checksum for the zip archive.
