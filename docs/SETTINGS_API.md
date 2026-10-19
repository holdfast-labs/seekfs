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
       "entries": 0, "state": "not_indexed"}
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
      "volumes": {"E:": {"mode": "explicit", "roots": ["E:\\src"]}}
    }
  }
}
```

- `volumes` lists every local fixed NTFS volume (so the app can offer drives
  that are not indexed yet), plus any indexed volume that is not fixed/NTFS.
  `state` is one of `ready`, `indexing`, `not_indexed`, `error`.
- `content` mirrors the `[content]` keys in docs/CONFIG.md; `volumes` holds the
  per-volume `[content."X:"]` overrides (only keys that are set). `budget_bytes`
  0 means no explicit budget. `exts` empty means sniff (default).
- `can_write` is true when this caller may call `settings-set`.

### `settings-set` (mutate: elevated callers only)

Request: `{"command":"settings-set","settings":{...}}` with the same shape as
`settings-get`'s `settings`. Every field is optional; only present fields
change. `volumes` entries need only `volume` and `indexed`.

- Adding a volume (`indexed: true`) starts a USN index build for it (the same
  path as `index-usn`) and adds it to the service's volume list; removing one
  unloads it and drops it from the list (its index file is kept on disk).
- Content changes are validated (absolute roots on known volumes, mode in
  `auto|explicit|off`, `budget_bytes >= 0`), written to `plugin.toml`, and
  applied with the same live reload as `plugin-reload`. `enabled: false`
  writes `[features.content] enabled = false`.
- Response: `{"ok": true, "settings": <settings after the change>}`. Validation
  failure: `ok: false` with a message naming the field; nothing is written.
- A non-elevated caller gets `ok: false`, `message: "settings-set requires an
  elevated caller"`. The app then runs `seekfs.exe settings set --json <file>`
  elevated (UAC), which sends the same request.

## CLI

- `seekfs settings get [--json]`
- `seekfs settings set --json <file>` (reads a `settings` object from the file)
