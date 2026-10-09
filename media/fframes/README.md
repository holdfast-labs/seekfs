# seekfs videos with fframes

This standalone Rust project renders three videos through fframes 1.2.0 and
Skia/Vulkan. It is not part of the Go application or its release binaries.

## Windows setup

Install Rust and LLVM. Download the FFmpeg **9.0 shared** GPL build from
[BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds/releases/tag/latest).
Set `FFMPEG_DIR` to the extracted directory (with `bin`, `include` and `lib`):

```powershell
$env:FFMPEG_DIR = 'C:\ffmpeg-n9.0-latest-win64-gpl-shared-9.0'
$env:LIBCLANG_PATH = 'C:\Program Files\LLVM\bin'
```

From the seekfs repository root:

```powershell
.\media\fframes\scripts\render.ps1 -Demo launch -Command timeline
.\media\fframes\scripts\render.ps1 -Demo launch -Command inspect -RenderArgs @('--fail-on', 'warning')
.\media\fframes\scripts\render.ps1 -Demo launch -Command strip
.\media\fframes\scripts\render.ps1 -Demo launch -Command preview
.\media\fframes\scripts\render.ps1 -Demo launch
.\media\fframes\scripts\render.ps1 -Demo desktop
.\media\fframes\scripts\render.ps1 -Demo cli
```

The default render paths are `media/seekfs-{launch,desktop,cli}.mp4`.
`preview` opens the GPU player; Space pauses, h/l seek, j/k step, q exits.

## Edit and review

`src/lib.rs` contains the five launch shots, two desktop shots and three CLI
shots. `src/main.rs` configures the renderer and H.264 encoder. `--demo` selects
launch, desktop or cli. `media/` contains embedded fonts, the original logo and
the original soundtrack. Fonts' license files are in `licenses/`.

```powershell
.\media\fframes\scripts\render.ps1 -Demo launch -Command frame -RenderArgs @('3s,9s,15s,21s,27s')
.\media\fframes\scripts\render.ps1 -Demo launch -Command audio -RenderArgs @('analyze')
```

Review contact sheets and full-size key frames, run `inspect`, and use the
native preview to check playback. Render all final exports only after review.
The authored launch is 30 s / 900 frames; desktop 16 s / 480 frames; CLI 18 s /
540 frames. The hook is visible on frame zero. Music and transition cues share
the 5, 11, 17 and 23 second cuts.

Regenerate the original soundtrack (Python + FFmpeg) from the repository root:

```powershell
python media/fframes/scripts/soundtrack.py
ffmpeg -y -i media/fframes/media/launch-raw.wav -af loudnorm=I=-14:TP=-1.5:LRA=7 -ar 44100 media/fframes/media/launch.wav
```

Remove `launch-raw.wav` before building: every supported file in the media
folder is embedded. Do not put license text or generated review images there.

See [the media overview](../README.md) for fixture provenance and the tweet draft.
