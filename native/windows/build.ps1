#Requires -Version 7.0

<#
.SYNOPSIS
    Build the DeepSeek Harness Windows distribution.
.DESCRIPTION
    Builds the dsh CLI, Wails desktop app, easybrowser bridge, nm-host,
    and creates the NSIS installer. Run from the repository root.
.PARAMETER Config
    Build configuration: Debug or Release (default: Release)
.PARAMETER SkipDesktop
    Skip building the Wails desktop app.
.PARAMETER SkipInstaller
    Skip building the NSIS installer.
.PARAMETER Version
    Version string for the build (default: from VERSION file or git tag).
#>

param(
    [ValidateSet('Debug', 'Release')]
    [string]$Config = 'Release',

    [switch]$SkipDesktop,

    [switch]$SkipInstaller,

    [string]$Version = ''
)

$ErrorActionPreference = 'Stop'
$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot '..\..')
$DistRoot = Join-Path $RepoRoot 'dist'
$BuildDir = Join-Path $DistRoot 'windows-amd64'

# ---- Version ----
if (-not $Version) {
    $versionFile = Join-Path $RepoRoot 'VERSION'
    if (Test-Path $versionFile) {
        $Version = Get-Content $versionFile -Raw -Trim
    }
    else {
        $Version = git -C $RepoRoot describe --tags 2>$null
        if (-not $Version) { $Version = '0.1.0-rc.5' }
    }
}
Write-Host "Building DeepSeek Harness v$Version (Config: $Config)" -ForegroundColor Cyan

# ---- Step 1: Build dsh CLI (pnpm) ----
Write-Host "`n[1/5] Building dsh CLI..." -ForegroundColor Yellow
Push-Location $RepoRoot
try {
    pnpm install --no-frozen-lockfile
    pnpm run build:lib:host
    $dshDir = Join-Path $BuildDir 'dsh'
    New-Item -ItemType Directory -Force -Path $dshDir | Out-Null
    Copy-Item -Recurse -Force (Join-Path $RepoRoot 'apps\cli\lib') $dshDir
    Copy-Item -Force (Join-Path $RepoRoot 'apps\cli\package.json') $dshDir
}
finally {
    Pop-Location
}

# ---- Step 2: Build easybrowser bridge ----
Write-Host "`n[2/5] Building easybrowser bridge..." -ForegroundColor Yellow
$bridgeDir = Join-Path $RepoRoot 'native\easybrowser'
if (Test-Path $bridgeDir) {
    Push-Location $bridgeDir
    try {
        go build -o (Join-Path $BuildDir 'bridge.exe') .
        Write-Host "  bridge.exe built" -ForegroundColor Green
    }
    finally {
        Pop-Location
    }
}

# ---- Step 3: Build nm-host ----
Write-Host "`n[3/5] Building nm-host..." -ForegroundColor Yellow
$nmhostDir = Join-Path $RepoRoot 'native\easybrowser-nmhost'
if (Test-Path $nmhostDir) {
    Push-Location $nmhostDir
    try {
        go build -o (Join-Path $BuildDir 'nm-host.exe') .
        Write-Host "  nm-host.exe built" -ForegroundColor Green
    }
    finally {
        Pop-Location
    }
}

# ---- Step 4: Build Wails desktop app ----
if (-not $SkipDesktop) {
    Write-Host "`n[4/5] Building Wails desktop app..." -ForegroundColor Yellow
    Push-Location (Join-Path $RepoRoot 'apps\windows-desktop')
    try {
        Push-Location 'frontend'
        try {
            npm install
            npm run build
        }
        finally {
            Pop-Location
        }
        wails build -clean -platform windows/amd64
        $wailsOutput = Join-Path $PWD 'build\bin\DeepSeek Harness Setup.exe'
        if (Test-Path $wailsOutput) {
            Copy-Item $wailsOutput (Join-Path $BuildDir 'DeepSeek Harness.exe')
        }
    }
    finally {
        Pop-Location
    }
}
else {
    Write-Host "`n[4/5] Skipping desktop app" -ForegroundColor Gray
}

# ---- Step 5: Build NSIS installer ----
if (-not $SkipInstaller) {
    Write-Host "`n[5/5] Building NSIS installer..." -ForegroundColor Yellow
    # Copy Chrome extension
    $extDir = Join-Path $RepoRoot 'native\easybrowser-extension'
    if (Test-Path $extDir) {
        Copy-Item -Recurse -Force $extDir (Join-Path $BuildDir 'extension')
    }
    # Copy LICENSE
    Copy-Item -Force (Join-Path $RepoRoot 'LICENSE') $BuildDir
    # Build installer
    $nsisScript = Join-Path $PSScriptRoot 'installer.nsi'
    if (Get-Command 'makensis' -ErrorAction SilentlyContinue) {
        makensis -DSourceDir="$BuildDir" -DVersion="$Version" $nsisScript
        $installer = Join-Path $PSScriptRoot "dist\DeepSeek-Harness-Setup-$Version.exe"
        if (Test-Path $installer) {
            Write-Host "  Installer created: $installer" -ForegroundColor Green
        }
    }
    else {
        Write-Host "  makensis not found, skipping installer creation" -ForegroundColor Gray
    }
}

Write-Host "`nBuild complete!" -ForegroundColor Cyan
Write-Host "Output directory: $BuildDir"