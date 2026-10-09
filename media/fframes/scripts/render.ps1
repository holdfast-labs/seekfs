param(
    [ValidateSet('launch', 'desktop', 'cli')][string]$Demo = 'launch',
    [ValidateSet('render', 'preview', 'timeline', 'inspect', 'strip', 'frame', 'audio')]
    [string]$Command = 'render',
    [string[]]$RenderArgs = @()
)
$ErrorActionPreference = 'Stop'
$videoProject = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$videoRepo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
if (!$env:FFMPEG_DIR) {
    $videoLocalFFmpeg = Join-Path $videoRepo '.codex-tmp/fframes/ffmpeg/ffmpeg-n9.0-latest-win64-gpl-shared-9.0'
    if (Test-Path $videoLocalFFmpeg) { $env:FFMPEG_DIR = $videoLocalFFmpeg }
}
if (!$env:FFMPEG_DIR) { throw 'Set FFMPEG_DIR to an FFmpeg 9 shared installation. See the video README.' }
if (!$env:LIBCLANG_PATH) { $env:LIBCLANG_PATH = 'C:\Program Files\LLVM\bin' }
$env:PATH = "$env:FFMPEG_DIR\bin;$env:PATH"
$videoDefaultExport = $Command -eq 'render' -and $RenderArgs.Count -eq 0
if ($videoDefaultExport) {
    $RenderArgs = @('-o', (Join-Path $videoRepo "media/seekfs-$Demo.mp4"))
}
Push-Location $videoProject
try {
    cargo run --release -- --demo $Demo $Command @RenderArgs
    if ($LASTEXITCODE -ne 0) { throw "fframes $Command failed with exit code $LASTEXITCODE" }
    if ($videoDefaultExport) {
        $videoOutput = Join-Path $videoRepo "media/seekfs-$Demo.mp4"
        $videoStreaming = Join-Path $videoRepo "media/seekfs-$Demo-streaming.mp4"
        ffmpeg -hide_banner -loglevel error -y -i $videoOutput -c copy -movflags +faststart $videoStreaming
        if ($LASTEXITCODE -ne 0) { throw 'Fast-start remux failed' }
        Move-Item -LiteralPath $videoStreaming -Destination $videoOutput -Force
    }
} finally { Pop-Location }
