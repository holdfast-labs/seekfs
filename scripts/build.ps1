param(
    [string]$Version = "dev",
    [string]$OutDir = "dist"
)

$ErrorActionPreference = "Stop"

# $ErrorActionPreference='Stop' does not turn a native command's non-zero exit
# into a terminating error, so every go/git invocation is checked explicitly.
function Assert-LastExit {
    param([string]$Step)
    if ($LASTEXITCODE -ne 0) {
        throw "build step failed: $Step (exit $LASTEXITCODE)"
    }
}

$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

$Commit = "unknown"
try {
    $Commit = (git rev-parse --short HEAD 2>$null).Trim()
} catch {
}

$Date = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$Target = Join-Path $OutDir "seekfs-windows-amd64"

if (Test-Path $Target) {
    Remove-Item $Target -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $Target | Out-Null

$LdFlags = "-s -w -X main.version=$Version -X main.commit=$Commit -X main.date=$Date"
go build -trimpath -ldflags $LdFlags -o (Join-Path $Target "seekfs.exe") ./cmd/seekfs
Assert-LastExit "go build (seekfs.exe)"

# The UI is launched by double-click and must run elevated so its spawned
# service can open raw volumes for USN-based index rebuild/verification.  Embed
# a requireAdministrator manifest into the UI binary.  The manifest and icon
# must live in the SAME .rsrc section, so the shared cmd/seekfs/rsrc.syso is
# temporarily regenerated with icon+manifest for the UI build and then restored,
# leaving the CLI build (and all future builds) unaffected.
$UiPkg = Join-Path $Root "cmd\seekfs"
$UiSyso = Join-Path $UiPkg "rsrc.syso"
$UiSysoBackup = Join-Path $env:TEMP "seekfs-rsrc.syso.bak"
$UiIcon = Join-Path $UiPkg "ui_frontend\assets\seekfs.ico"
$UiManifest = Join-Path $UiPkg "seekfs-ui.manifest"
Copy-Item $UiSyso $UiSysoBackup -Force
try {
    go run ./scripts/ui-rsrcgen -arch amd64 -ico $UiIcon -manifest $UiManifest -o $UiSyso
    Assert-LastExit "ui resource generation"
    go build -trimpath -tags "seekfs_ui production" -ldflags "$LdFlags -H windowsgui" -o (Join-Path $Target "seekfs-ui.exe") ./cmd/seekfs
    Assert-LastExit "go build (seekfs-ui.exe)"
} finally {
    Copy-Item $UiSysoBackup $UiSyso -Force
    Remove-Item $UiSysoBackup -Force -ErrorAction SilentlyContinue
}

# Native Gio desktop UI. Its manifest (comctl32 v6 + per-monitor-v2 DPI,
// asInvoker) and icon are regenerated deterministically into
// cmd/seekfs-native/rsrc.syso, which is also committed for dev builds.
$NativePkg = Join-Path $Root "cmd\seekfs-native"
$NativeSyso = Join-Path $NativePkg "rsrc.syso"
$NativeIcon = Join-Path $UiPkg "ui_frontend\assets\seekfs.ico"
$NativeManifest = Join-Path $NativePkg "seekfs-native.manifest"
go run ./scripts/ui-rsrcgen -arch amd64 -ico $NativeIcon -manifest $NativeManifest -o $NativeSyso
Assert-LastExit "native resource generation"
go build -trimpath -ldflags "$LdFlags -H windowsgui" -o (Join-Path $Target "seekfs-native.exe") ./cmd/seekfs-native
Assert-LastExit "go build (seekfs-native.exe)"

Copy-Item README.md,LICENSE,NOTICE.md -Destination $Target

# Copy only files tracked by git so local research notes, private benchmark
# data, and other untracked docs cannot leak into release artifacts.
$DocFiles = git ls-files docs
Assert-LastExit "git ls-files docs"
foreach ($doc in $DocFiles) {
    $dest = Join-Path $Target $doc
    $destDir = Split-Path -Parent $dest
    New-Item -ItemType Directory -Force -Path $destDir | Out-Null
    Copy-Item $doc -Destination $dest
}

$Zip = Join-Path $OutDir "seekfs-windows-amd64.zip"
if (Test-Path $Zip) {
    Remove-Item $Zip -Force
}
Compress-Archive -Path (Join-Path $Target "*") -DestinationPath $Zip

Write-Host "Built $Zip"
