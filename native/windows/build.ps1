#Requires -Version 7.0

<#
.SYNOPSIS
    Build the DeepSeek Harness Windows distribution.
.DESCRIPTION
    Builds the dsh CLI, Wails desktop app, easybrowser bridge, and creates
    the NSIS installer. Run from the repository root.
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
        # Try git describe
        $Version = git -C $RepoRoot describe --tags 2>$null
        if (-not $Version) { $Version = '0.1.0-rc.5' }
    }
}
Write-Host "Building DeepSeek Harness v$Version (Config: $Config)" -ForegroundColor Cyan

# ---- Step 1: Build dsh CLI (pnpm) ----
Write-Host "`n[1/4] Building dsh CLI..." -ForegroundColor Yellow
Push-Location $RepoRoot
try {
    pnpm install --frozen-lockfile
    pnpm run build
    # Bundle dsh with its dependencies using pkg or node单文件打包
    # For now, create a node_modules bundle
    $dshDir = Join-Path $BuildDir 'dsh'
    New-Item -ItemType Directory -Force -Path $dshDir | Out-Null
    # Copy the built artifacts
    Copy-Item -Recurse -Force (Join-Path $RepoRoot 'apps\cli\lib') $dshDir
    Copy-Item -Recurse -Force (Join-Path $RepoRoot 'apps\cli\package.json') $dshDir
    # Copy node_modules (pruned for production)
    pnpm deploy --filter @deepseek-ai/dsh --prod (Join-Path $BuildDir 'dsh\node_modules')
}
finally {
    Pop-Location
}

# ---- Step 2: Build easybrowser bridge ----
Write-Host "`n[2/4] Building easybrowser bridge..." -ForegroundColor Yellow
$easybrowserDir = Join-Path $RepoRoot 'vendor\easybrowser'
if (Test-Path $easybrowserDir) {
    Push-Location $easybrowserDir
    try {
        # Build bridge using easybrowser's build script
        if ($IsWindows) {
            & "$easybrowserDir\scripts\build.ps1" -ProfileName prod -BuildGUI
        }
        else {
            Write-Host "  Skipping easybrowser build (not on Windows)" -ForegroundColor Gray
        }
        # Copy bridge binary to build output
        $bridgeBin = Join-Path $easybrowserDir 'bin\bridge-prod.exe'
        if (Test-Path $bridgeBin) {
            Copy-Item $bridgeBin (Join-Path $BuildDir 'bridge.exe')
        }
        $nmHostBin = Join-Path $easybrowserDir 'bin\nm-host-prod.exe'
        if (Test-Path $nmHostBin) {
            Copy-Item $nmHostBin (Join-Path $BuildDir 'nm-host.exe')
        }
    }
    finally {
        Pop-Location
    }
}
else {
    Write-Host "  easybrowser submodule not found, skipping" -ForegroundColor Gray
}

# ---- Step 3: Build Wails desktop app ----
if (-not $SkipDesktop) {
    Write-Host "`n[3/4] Building Wails desktop app..." -ForegroundColor Yellow
    Push-Location (Join-Path $RepoRoot 'apps\windows-desktop')
    try {
        # Build frontend
        Push-Location 'frontend'
        try {
            npm install
            npm run build
        }
        finally {
            Pop-Location
        }
        # Build Wails app
        wails build -clean -platform windows/amd64
        # Copy to dist
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
    Write-Host "`n[3/4] Skipping desktop app" -ForegroundColor Gray
}

# ---- Step 4: Build NSIS installer ----
if (-not $SkipInstaller) {
    Write-Host "`n[4/4] Building NSIS installer..." -ForegroundColor Yellow
    $nsisScript = Join-Path $PSScriptRoot 'installer.nsi'
    if (Get-Command 'makensis' -ErrorAction SilentlyContinue) {
        makensis -DSourceDir="$BuildDir" -DVersion="$Version" $nsisScript
        $installer = Join-Path $DistRoot "DeepSeek-Harness-Setup-$Version.exe"
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