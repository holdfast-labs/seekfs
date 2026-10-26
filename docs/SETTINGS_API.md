# Settings API (service pipe)

The desktop app reads and changes the service's indexing settings over the
service pipe. The CLI (`seekfs config`, `seekfs plugin config content`) stays
the reference path: the pipe commands reuse the same code, write the same files
(`seekfs.toml`, `plugin.toml`), and apply changes live.

## Commands

### `settings-get` (read-only)

Request: `{"command":"settings-get"}`

Response (`serviceResponse` with a new `settings` field):

```json
{
  "ok": true,
  "settings": {
    "can_write": false,
    "volumes": [
      {"volume": "C:", "indexed": true, "fs": "NTFS", "label": "OS",
       "total_bytes": 1000204886016, "free_bytes": 120000000000,
       "entries": 1460000, "state": "ready"},
      {"volume": "F:", "indexed": false, "fs": "NTFS", "label": "Data",
       "total_bytes": 2000398934016, "free_bytes": 900000000000,
       "entries": 0, "state": "not_indexed"},
      {"volume": "X:", "indexed": true, "fs": "NTFS", "label": "Fixture",
       "total_bytes": 549755813888, "free_bytes": 274877906944,
       "entries": 0, "state": "indexing",
       "progress": {"phase": "building", "done": 1200000, "total": 2900000,
                    "started_at": "2026-10-22T10:00:00Z"}}
    ],
    "content": {
      "enabled": true,
      "mode": "auto",
      "roots": ["E:\\work"],
      "exclude": ["E:\\work\\scratch"],
      "exts": [],
      "git": true,
      "system_excludes": true,
      "budget_bytes": 0,
      "build_speed": "balanced",
      "volumes": {"E:": {"mode": "explicit", "roots": ["E:\\src"]}}
    }
  }
}
```

- `volumes` lists every local fixed NTFS volume (so the app can offer drives
  that are not indexed yet), plus any indexed volume that is not fixed/NTFS.
  `state` is one of `ready`, `indexing`, `not_indexed`, `error`. `error`, when
  present, is the last background build failure for that volume.
- `progress` is present only while `state` is `indexing`. `phase` is
  `enumerating` (reading the MFT or USN journal), `building` (converting
  records), or `writing` (persisting the index). `done` and `total` count
  records where the phase can report a total (MFT enumeration); `total` is 0
  when the count is unknown (USN fallback). `started_at` is an RFC 3339
  timestamp. The app uses this for a determinate progress bar.
- `content` mirrors the `[content]` keys in docs/CONFIG.md, with
  `[plugins.content]` values from `plugin.toml` merged in; `volumes` holds the
  per-volume `[content."X:"]` overrides from `seekfs.toml` (only keys that are
  set). `budget_bytes` 0 means no explicit budget. `exts` empty means sniff
  (default). `build_speed` is `balanced` (default), `fast`, or `gentle`; it is
  global (not per volume) and the service reports the effective worker count as
  `build_workers` in the content health response while indexing.
- `can_write` is true when this caller may call `settings-set`.
- Remote callers are refused for both commands, so local paths in `volumes` and
  `content` never cross the remote transport.

### `settings-set` (mutate: elevated callers only)

Request: `{"command":"settings-set","settings":{...}}` with the same shape as
`settings-get`'s `settings`. Every field is optional; only present fields
change. `volumes` entries need only `volume` and `indexed`.

- Adding a volume (`indexed: true`) starts a USN index build for it in the
  background (the same path as `index-usn`) and returns immediately. The
  volume reports `state: "indexing"` while the build runs; a second add of the
  same volume is a no-op. On success the volume joins the service's volume
  list and the volume config is written; on failure it keeps `state: "error"`
  and `error` carries the build message. Removing a volume unloads it and drops
  it from the list (its index file is kept on disk); removing one whose build
  is still pending cancels and ignores that result.
- Content changes are validated (absolute roots on known volumes, mode in
  `auto|explicit|off`, `build_speed` in `balanced|fast|gentle`,
  `budget_bytes >= 0`). Global scope keys are written to
  `plugin.toml`; per-volume overrides are written to `[content."X:"]` in
  `seekfs.toml`. Both are applied with the same live reload as `plugin-reload`.
  `enabled` writes the content selection in `plugin.toml` when the content
  plugin is installed; otherwise it writes `[features.content] enabled` in
  `seekfs.toml`.
- Response: `{"ok": true, "settings": <settings after the change>}`. Validation
  failure: `ok: false` with a message naming the field; nothing is written.
- A non-elevated caller gets `ok: false`, `message: "settings-set requires an
  elevated caller"`. The app then runs `seekfs.exe settings set --json <file>`
  elevated (UAC), which sends the same request (with `--pipe` when the app uses
  a non-default pipe).
- A drive build reads the raw volume, so the service itself must run elevated
  (or as SYSTEM); `info` reports this as `elevated: true`. The desktop apps
  start their sibling standalone service unelevated, so when an elevated
  `settings set` adds a drive and the target is an unelevated standalone
  service from the same `seekfs.exe`, the helper restarts that service
  elevated (same `service -lowmem -pipe ... -sddl ...` command) and waits for
  it before sending the write. The installed Windows service is never
  restarted. If an unelevated service still attempts a build, the drive's
  `error` says the service is not running as administrator.
- `settings-get` also returns `service_mode` (`windows-service` or
  `standalone`), so the app can offer to install the Windows service.

### Installing the service from the app

`seekfs settings set --json <file> --install-service` (run elevated by the app)
first makes the installed Windows service own the pipe, then sends the write:

- If the installed service already answers on the pipe, nothing is installed.
- A standalone sibling from the same `seekfs.exe` is stopped; any other process
  on the pipe is an error.
- The `seekfs` service is created (or an existing one reconfigured in place)
  as LocalSystem, delayed automatic start, command
  `seekfs.exe service -pipe <pipe> -sddl <default>`. It passes no `-db`, so the
  service loads the configured drive list and drives added later in Settings
  survive a restart (`seekfs install` pins `-db` paths instead). When an
  existing service had pinned `-db` paths, they are first added to the `dbs`
  list of the `seekfs.toml` the service reads (`%ProgramData%\seekfs`, else
  next to `seekfs.exe`), so no index is dropped.
- The helper starts the service, waits until it answers as `windows-service`,
  and then applies the settings (an empty `{}` patch is valid for install only).

## CLI

- `seekfs settings get [--json]`
- `seekfs settings set --json <file> [--install-service]` (reads a `settings`
  object from the file; `--install-service` is described above)
