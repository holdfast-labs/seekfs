# v1.11.0 validation

Local Windows validation completed on 2026-10-08:

- Full `scripts/ci.ps1 -Stage test`: Go coverage tests, tagged UI tests,
  frontend tests, vet, formatting, build and CLI integration passed.
- Staticcheck and govulncheck passed; no reachable vulnerabilities reported.
- Expanded concurrency/race subset passed, including memo and deferred-ranking
  regression tests. The final indexing-count fix passed the indexing tests.
- Built the release zip with `scripts/build.ps1 -Version v1.11.0` and verified
  its 47 entries, including both executables, license, notices and release notes.
- Ran CLI integration against the packaged CLI. A six-entry fixture returned
  exactly two matching files, the expected count, no results for a missing term,
  and correct results for default, path, size, modified, extension and type
  ordering through the packaged UI's resident service. Size ordering put the
  seven-byte file before the 5,002-byte file. A combined path/name query selected
  the correct file.
- Launched the packaged native desktop UI. It rendered the polished prompt,
  table headers and footer, loaded the isolated six-entry index, and displayed
  healthy index status. The resident endpoint confirmed the packaged executable
  and six loaded entries.

The native desktop driver could capture screenshots and accessibility text,
but could not activate the application or set the search input. A standard-user
fixture launch had the same limitation. Native keyboard/mouse search, selection
and context-menu checks remain unverified. Browser preview input/clear checks
and automated UI/frontend tests passed, but do not replace those native checks.

The package smoke test also caught and fixed the walk-index command reporting
zero entries after compact conversion; it now reports the actual record count.

## Published archive

[v1.11.0](https://github.com/holdfast-labs/seekfs/releases/tag/v1.11.0)
was published from `00daffe` after all Windows CI jobs passed for the final
runtime code at `ecfc16c`; the intervening commit only adds validation notes.
The release workflow also passed.

Downloaded the published archive and matched its checksum:

```text
3679fc1edd4b1a0a7c3d6d60eb8ddc52246f5c00da0a07e3cdf9aa9b546e919a
```

CLI integration passed against the downloaded binary. The downloaded desktop
executable also launched and loaded the isolated six-entry index with healthy
status. Its service identified itself as `v1.11.0`, commit `00daffe`, and returned
the two matching files in ascending size order with count two. The native
input limitation above remained reproducible. Temporary package configuration
was removed after testing. The canonical local zip in `dist` is the downloaded
published archive.
