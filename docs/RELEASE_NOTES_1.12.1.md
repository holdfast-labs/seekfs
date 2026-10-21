# seekfs v1.12.1

The desktop app can now index drives on a machine without the seekfs service:
Settings > Drives installs the service with one administrator prompt. Drives
that fail to index now show the reason.

## Fixed

- **Drives added from the desktop app showed "Error" and 0 entries.** Without
  the installed Windows service, `seekfs-native.exe` starts its own service as
  the signed-in user. Apply asked for administrator approval, but only the
  short-lived helper that sends the change ran elevated. The drive build ran in
  the unelevated service, could not open the volume ("Access is denied"), and
  the app showed only "Error". Now, when a change adds a drive and the app's
  own service is not elevated, the helper restarts that service as
  administrator before applying the change.
- **The failure reason was hidden.** The Drives list now shows each drive's
  error under its name. A drive that cannot be read because the service is not
  running as administrator says so.
- **Go 1.26.10.** The toolchain moves to Go 1.26.10 for GO-2026-6809
  (`crypto/x509`), which `govulncheck` reported on the 1.12.0 build.

## New

- **Install the service from the app.** Settings > Drives offers "Install the
  seekfs service (recommended)", checked on first run. Apply installs the
  `seekfs` Windows service (LocalSystem, delayed automatic start), starts it,
  and starts indexing the chosen drives, with one administrator prompt. The app
  itself keeps running without administrator rights. The service follows the
  configured drive list, so drives added later in Settings are still indexed
  after a restart.
- `seekfs settings set --install-service` is the CLI form the app runs.
  `settings-get` reports `service_mode`, and `info` reports `elevated`.

## Upgrading

Stop the running service (or close the app and its service) before replacing
`seekfs.exe`, `seekfs-native.exe` and `seekfs-ui.exe`. Ship all three from the
same zip.

Services installed with `seekfs install`, `seekfs launch` or
`seekfs index-volumes -launch` list their index files on the service command
line and ignore drives added later in Settings. To switch an existing
installation to the configured drive list, run `seekfs stop`, then
`seekfs settings set --json <file> --install-service` with a file containing
`{}`, both from an administrator shell. The command copies the listed index
files into `seekfs.toml`, reconfigures the service in place for automatic
start, and starts it. The app does the same if it finds that service stopped
and you install from Settings > Drives.
