# Grep-lines WIP

The pure line extraction helper and initial unit tests are started in
`cmd/seekfs/content_lines.go`. The targeted test run currently fails because
the test query parser produced no matching lines; diagnose the content-query
syntax and positive-leaf construction before continuing. CLI integration,
service-result re-reading, JSON `lines` output, documentation, and the requested
example run are not implemented yet.

Verification attempted (with `TMP`/`TEMP=E:\bench\scratch\tmp` and
`GOMAXPROCS=2`):

```powershell
go test ./cmd/seekfs -run 'ContentLines|CmdSearch|Snippet' -count=1 -p 1
```

Result: failed in `TestContentLinesMultipleMatchesContextAndLimit` with an empty
line result.
