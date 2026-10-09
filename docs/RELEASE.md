# Release Plan

## Artifact

GitHub release artifact:

```text
seekfs-windows-amd64.zip
```

Contents:

- `seekfs.exe`
- `seekfs-ui.exe`
- `README.md`
- `LICENSE`
- `NOTICE.md`
- `docs/`

Indexes and benchmark output are not included.

## Cutting a release

Releases are automated by `.github/workflows/release.yml`. Push a v-prefixed tag
from a commit that is green on CI, and the workflow runs `scripts/build.ps1`,
verifies the archive, writes a SHA-256 checksum, and publishes a GitHub release:

```powershell
git tag v1.11.0
git push origin v1.11.0
```

The release body comes from `docs/RELEASE_NOTES_<version>.md` when that file
exists (version without the leading `v`); otherwise GitHub generates the notes
from the previous release. The tag is passed through verbatim, so
`seekfs version` reports `seekfs v1.11.0`.

## Current Release

Current release: `v1.11.0`.

Release notes:

```text
docs/RELEASE_NOTES_1.11.0.md
```

## Signing

The release artifact is unsigned. Document this in release notes and expect
normal Windows warnings for unsigned binaries. Add Authenticode signing before
wider distribution beyond trusted local users.
