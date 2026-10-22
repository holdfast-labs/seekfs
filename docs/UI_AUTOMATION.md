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
- `-scenario all|startup|search|resize|settings`.

## Automation commands

Built with `-tags uiautomation` and
`SEEKFS_NATIVE_AUTOMATION=<pipe>`, the app serves one JSON object per line and
returns one JSON object per line. All commands run on the UI goroutine.

- `{"cmd":"state"}`: window size, settings open/tab/saving/notice/err,
  current Apply step, `service_mode`, content mode, drive drafts (volume,
  indexed, state, entries, error, progress phase/done/total), rows, query,
  search sequence, health/message, entries, no-volumes.
- `{"cmd":"action","name":...,"arg":...}`:
  - `open-settings` (`drives`, `content`, `search`), `close-settings`;
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
