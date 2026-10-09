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
