# sym worktree notes

WIP: `sym:` parses and uses whole-word postings, and definition verification is
implemented in `content_sym.go`. The required definition fixture tests, precise
`--lines` definition-only output, content definition ranking bonus, query-time
compiled-pattern table, and focused edge-case coverage remain unfinished. The
targeted command currently passes the existing matching tests, but it does not
cover the required new behavior. Do not treat G9 as complete until these gaps
are closed.
