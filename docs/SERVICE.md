# Service Setup

## Install

Build the binary first:

```powershell
go build -o seekfs.exe ./cmd/seekfs
```

Install the service:

```powershell
.\seekfs.exe install
.\seekfs.exe start
```

Service install, launch, start, stop, and restart require an elevated shell
because the service runs as LocalSystem. Search commands do not require
elevation once the service is running.

## Build Indexes

```powershell
.\seekfs.exe index-volumes -volume C: -volume F:
.\seekfs.exe index-volumes --dry-run --json
```

Without `-volume`, `index-volumes` indexes fixed local drives by default and
stores generated indexes under:

```text
%ProgramData%\seekfs\indexes
```

## Configure Resident Search

Launch the service with the DB paths it should keep loaded:

```powershell
.\seekfs.exe launch -db F:\seekfs_c.gsi -db F:\seekfs_f.gsi
```

`launch` installs or reinstalls the service, starts it, waits for the named pipe,
and runs the same health checks as `doctor`.

Check health:

```powershell
.\seekfs.exe status --json
.\seekfs.exe loaded --json
```

`status` verifies the Windows service and the pipe. `loaded` shows the process
serving the pipe and the loaded DB state. If a DB reports `state: "stale"`, the
service is answering from the index but journal replay is not active.

## Walk-indexed content search

`content:` queries also work on walk-indexed databases (`seekfs index -root
<dir> -db <file.gsi>`, served with `-db`), which covers filesystems without a
USN journal (FAT/exFAT/ReFS-without-journal, network shares) and non-Windows
builds. Enable content the same way (`seekfs plugin add content` or
`SEEKFS_CONTENT_SEARCH=1`); the service builds, attaches, and freshens a
stable-path-hash-keyed sidecar with the same `indexing`/`ready`/`degraded`/
`stale` states and progress reporting as NTFS volumes (see
[CONTENT_SEARCH.md §6k](CONTENT_SEARCH.md)). Change detection diffs the live
index against the sidecar after every watcher-triggered filename rebuild plus
a periodic safety net, so an edit typically becomes findable within seconds
(mostly the filename rewalk itself). If the indexed tree has no `.git`
markers under it, auto scope finds no roots — pin the tree with
`plugin config content --root <path>` (or `SEEKFS_CONTENT_SCOPE=explicit` +
`SEEKFS_CONTENT_ROOTS=<path>`) or the build attaches an empty index.

## Query Semantics

In path-scoped queries, short dotted tokens such as `.pdf`, `.raw`, and `.nrrd`
are treated as Everything-compatible extension shorthands. For example,
`path:Downloads .pdf` and `dir:Reports .pdf` match files whose extension is
exactly `pdf`; they do not match `manual.pdf.bak`. Outside path mode, the same
dotted token remains a normal name substring, so `.pdf` can match
`manual.pdf.bak`. Use `ext:pdf` when exact extension matching is desired in any
mode.

Every index seekfs writes uses the v9 container with mapped derived sections for
rank, children, subtree intervals, FRNs, lowercase names, and posting metadata.
Older v8 indexes are no longer readable; rebuild them by re-indexing:

```powershell
.\seekfs.exe index-volumes -volume C:
```

`loaded --json` reports `derived_sections` and `derived_bytes` for v9 indexes.
Live update WAL appends use CRC-protected binary frames; the replay path still
accepts the older JSON WAL format.

## Incremental Durability

The service keeps recent USN updates in memory for low search latency and
debounces full `.gsi` rewrites. Before applying each replay batch, it appends the
batch to a sidecar file beside the index:

```text
seekfs_c.gsi.wal
```

On service startup, seekfs replays the sidecar before reading newer NTFS journal
changes. A successful full index save removes the sidecar. Keep `.gsi.wal` files
with their matching `.gsi` files when copying or backing up a live index.

Use `info --json` to inspect index layout and size contributors:

```powershell
.\seekfs.exe info -db C:\ProgramData\seekfs\indexes\seekfs_c.gsi --json
```

## Upgrade

1. Build or unpack the new `seekfs.exe`.
2. Record current DB paths.
3. Run `seekfs launch` with the same `-db` arguments.

## Stopping the service

```powershell
.\seekfs.exe stop
```

Stop is bounded: the service reports `STOP_PENDING` with a wait hint and
increments its checkpoint as each teardown phase starts (signal, content
cancel, content drain, content release, companions), so the SCM can tell a
slow stop from a wedged one, and the whole stop completes within seconds.

What happens on stop, in order:

1. The stop signal retires the USN replay loops, the background persist
   loops, the stale-recovery loop, the walk watchers, and the pipe/remote
   listeners.
2. Content activations are cancelled, which aborts an in-flight drain tick,
   service-owned build, delta fold, integrity pass, or restart catch-up at
   its next per-document check. The content-task drain itself is
   deadline-bound: a wedged extraction cannot hold the stop.
3. Content sidecar mappings and locks are released best-effort under bounded
   locks, and companion processes are stopped.
4. No final blocking persist runs on the way out. The write-ahead log holds
   every replayed batch, so durability is preserved without it; a persist or
   content save killed mid-flight is safe (temp file + atomic rename, and the
   WAL rewrite keeps the post-snapshot frames for the next startup replay).

## Uninstall

```powershell
.\seekfs.exe stop
.\seekfs.exe uninstall
```

Remove index files manually only if you no longer need them.
