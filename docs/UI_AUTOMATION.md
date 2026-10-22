# UI automation harness

Test-only harness for the Gio native app (`cmd/seekfs-native`) and the
standalone service (`cmd/seekfs`). It measures startup, search, resize, and
Settings flows on Windows. It does not run in production builds.

## Safety rules

- Every harness run uses private pipes
  `\\.\pipe\seekfs-uidrive-<pid>-1` (service) and `-2` (automation).
  It never opens, stops, or changes the installed `seekfs` service or the
  default pipe `\\.\pipe\seekfs-service`.
- The harness starts both child processes itself. It kills them on exit and on
  every error path. Run it in the foreground; it exits by its `-timeout`.
- No elevation and no UAC. The automation endpoint refuses an Apply that would
  install the Windows service.
- The fixture service mode and the automation endpoint exist only in binaries
  built with `-tags uiautomation`. Normal builds compile a no-op stub.
- All output goes under `-out` (default `E:\bench\gotmp\uidrive-<timestamp>`).
  `SEEKFS_DIR` points inside that directory, so the run never writes
  `%ProgramData%\seekfs`.

## Run

```powershell
go build -p 1 -o E:\bench\gotmp\seekfs-uidrive.exe ./cmd/seekfs-uidrive
E:\bench\gotmp\seekfs-uidrive.exe -out E:\bench\gotmp\uidrive-all -timeout 5m -scenario all
```

The driver builds `seekfs.exe` and `seekfs-native.exe` with
`-tags uiautomation` into `<out>\bin`, creates a 20,000-file fixture tree,
starts the service on the private pipe, then starts the app. It writes
`report.json`, `summary.md`, `shots\`, and process logs under `<out>`.

Flags:

- `-out <dir>`: output directory.
- `-timeout 5m`: hard harness deadline; children are killed when it expires.
- `-scenario all|startup|search|resize|settings|status|content`.

## Automation commands

Built with `-tags uiautomation` and
`SEEKFS_NATIVE_AUTOMATION=<pipe>`, the app serves one JSON object per line and
returns one JSON object per line. All commands run on the UI goroutine.

- `{"cmd":"state"}`: window size, settings open/tab/saving/notice/err,
  current Apply step, `service_mode`, content mode, drive drafts (volume,
  indexed, state, entries, error, progress phase/done/total), rows, query,
  search sequence, health/message, entries, no-volumes, sidebar rows
  (kind/label/under), and the content-index state/progress.
- `{"cmd":"action","name":...,"arg":...}`:
  - `open-settings` (`drives`, `content`, `search`), `close-settings`;
  - `set-contents-mode` (bool) turns the Contents toggle on or off;
  - `type-query` (text) drives the typed/leading-edge searcher path without Enter;
  - `search-now` (text) sends immediately through the SearchNow path;
  - `toggle-drive` (volume), `set-install-service` (bool);
  - `apply-settings`; `set-query` (text; same path as typing then Enter);
  - `set-content-mode` (`auto`, `explicit`, `off`) for the settings scenario.
- `{"cmd":"screenshot","path":...}`: headless re-render of the current UI to
  PNG.
- `{"cmd":"hwnd"}`: top-level window handle.
- `{"cmd":"metrics"}`: frame count, p50/p95/max frame time, ten slowest frames,
  action-to-next-frame latency, and marks for Settings apply/fetch start and
  finish.
- `{"cmd":"metrics-reset"}`: clear the counters above.

Frame duration covers layout plus submit (`e.Frame`) for each `FrameEvent`.
The hooks are no-ops in normal builds.

## Service fixture mode

Built with `-tags uiautomation`, a standalone service reads
`SEEKFS_UIDRIVE_FIXTURE=<json>`:

```json
{"volumes":[{"volume":"X:","label":"Fixture","fs":"NTFS",
  "total_bytes":549755813888,"free_bytes":274877906944,
  "root":"<fixture dir>","per_entry_delay_ms":1}]}
```

In this mode:

- local `settings-set` is allowed without elevation;
- `settings-get` lists the synthetic volumes;
- a volume build walks `root`, sleeps `per_entry_delay_ms` per entry, and
  returns a real compact `Index` for the normal service install path.

Without the tag or without the environment variable, none of this code is
active.

## Scenarios

- `startup`: app process start to automation pipe, first frame, and first
  health state.
- `search`: three queries; action to `rows > 0` (state polled every 20 ms).
- `resize`: 40 `SetWindowPos` steps alternating 900x600 and 1600x1000 at 16 ms,
  then a hold. Reports frame p50/p95/max, slow-frame count, `SetWindowPos`
  durations (WM_SIZE blocking), time to a quiet gap, and the max frame after
  settle.
- `settings`: drives list latency; fixture drive off then on with Apply;
  action to saving cleared, indexing, and ready (service truth and panel);
  progress samples and increases while the fixture build runs; screenshots at
  open, indexing, mid-build progress, and done; then a content-mode Apply. The
  panel must reach `ready` without being reopened, and its lag after the
  service is reported.
- `status`: turns the fixture drive off, starts a service-side build, then
  starts the app while the build still runs. It requires the status line to
  reach healthy `ok` and the sidebar to list `X:` without an app restart, and
  records app-start-to-ready, service-ready, status lag, and sidebar timing.
  Screenshots: `status-loading.png` and `status-ready.png`.
- `all` does not include `status`; run it on its own so its app restart
  timing does not disturb the other scenarios.

If the app misbehaves, the harness records it under Findings and still exits
0. A non-zero exit means the harness itself could not set up or reach the app.

## Tests

```powershell
go test -p 1 -tags uiautomation ./cmd/seekfs-native -run TestAutomation
go test -p 1 ./cmd/seekfs-native -run TestAutomationDisabled
go test -p 1 -tags uiautomation ./cmd/seekfs -run TestUIDriveFixture
go test -p 1 ./cmd/seekfs -run TestUIDriveFixtureDisabled
go test -p 1 ./cmd/seekfs-uidrive -run TestGenerateFixtureTree
```

## Results

Measured on branch `ux/perf` (base `cf67f8a`) on Windows with the 20,000-file
fixture. The "before" values are from the base revision.

| scenario | metric | before | after |
|---|---|---:|---:|
| resize | SetWindowPos p50 | 240-277 ms | 12.9 ms |
| resize | SetWindowPos max | 316-4466 ms | 274.0 ms |
| resize | frame p95 | 252-271 ms | 13.2 ms |
| resize | frames over 100 ms | 40 | 1 |
| resize | frame p50 | - | 9.9 ms |
| resize | time to settle | ~4.97 s | ~4.99 s |
| search | action to rows p50 | ~175 ms | 16.0 ms |
| status | app start to ready, mid-build start | - | 28.3 s |
| status | status lag after service ready | - | 206 ms |

The resize numbers come from `-scenario all`; the search p50 is from
`-scenario search` (queries 12.9, 19.6, and 16.0 ms in the first run).

Notes:

- The presenter keeps one headless window, grows it in 256 px buckets, and
  never shrinks it. It converts RGBA to BGRA once into a reusable buffer and
  blits only the visible rectangle.
- The one 267-305 ms frame and the 274-315 ms `SetWindowPos` are the one-time
  creation of the larger headless GPU context when the window crosses a bucket
  boundary (1024 to 1792 px wide in this sweep). The other frames stay near
  10-13 ms.
- `time to settle` does not improve. Every automation command calls
  `u.invalidate()` (`uiauto.go`), so the harness `metrics` poll at 20 ms makes
  a frame every 20 ms. The 300 ms quiet-gap condition never occurs and the loop
  stops at its 5 s limit. The base revision shows the same ~5 s value, so this
  is a harness measurement effect, not presenter time.
- `-scenario resize` alone can report `no window handle` when it calls `hwnd`
  before the app handles its first `ViewEvent`. `-scenario all` waits for the
  first frame and measures reliably.

## Content scenario

`-scenario content` (and `all`) generates a known-answer content corpus under
the existing fixture tree: unique needles, a phrase in three files, smart-case
variants, a regex, a fuzzy typo, a `sym:` definition, a 1 MiB+ text file, a
binary file with a needle, `node_modules` and `.gitignore` negatives, and a
nested git repository. The expected answers are written to
`content-expected.json` next to `fixture.json`.

The service runs with `SEEKFS_CONTENT_SEARCH=1`, an explicit scope rooted at
the fixture, and a small explicit content budget (the default disk reserve
would refuse a build on a nearly full test volume). The scenario:

1. enables Contents mode and records the building notice and rows from an
   early typed query;
2. waits for the service content index to reach `ready`;
3. runs every expected query through both `type-query` and `search-now`,
   compares result paths exactly, and checks snippets;
4. runs binary/excluded/ignored/oversized negative queries;
5. rewrites one fixture file (old token out, new token in) and polls for up
   to 60 s;
6. writes screenshots, latency p50/p95/max, `report.json`, and `summary.md`.

When a query is wrong, the harness repeats it directly against its private
service pipe and records whether the UI or the service is at fault.

Run the fixture on a volume with free space above the content reserve (10 GiB
plus scratch) or the content build stops before writing a sidecar.
