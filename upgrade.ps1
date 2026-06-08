#Requires -RunAsAdministrator
<#
.SYNOPSIS
    ThingsPanel All-in-One — Windows Upgrade Script

.DESCRIPTION
    Backup config, pull new images, and restart services.

.PARAMETER TargetVersion
    Target version (default: latest from GitHub)

.PARAMETER InstallDir
    Install directory (default: C:\ThingsPanel)

.EXAMPLE
    .\upgrade.ps1
    .\upgrade.ps1 -TargetVersion v1.2.0
#>

[CmdletBinding()]
param(
    [string]$TargetVersion = "",
    [string]$InstallDir    = "C:\ThingsPanel"
)

$ErrorActionPreference = "Stop"

$REPO     = "ThingsPanel/thingspanel-installer"
$RAW_BASE = "https://install.thingspanel.io"

function Write-Info    ($m) { Write-Host "[INFO]  $m" -ForegroundColor Cyan }
function Write-Success ($m) { Write-Host "[OK]    $m" -ForegroundColor Green }
function Write-Warn    ($m) { Write-Host "[WARN]  $m" -ForegroundColor Yellow }
function Write-Step    ($m) { Write-Host "`n[>>] $m" -ForegroundColor White -BackgroundColor DarkBlue }
function Write-Err     ($m) { Write-Host "[ERROR] $m" -ForegroundColor Red; throw $m }

Write-Host ""
Write-Host "  ThingsPanel All-in-One Upgrader (Windows)" -ForegroundColor White
Write-Host ""

$ComposeFile = "$InstallDir\docker-compose.yml"
if (-not (Test-Path $ComposeFile)) {
    Write-Err "Cannot find $ComposeFile. Please run the installer first."
}

if (-not (Get-Command "docker" -ErrorAction SilentlyContinue)) {
    Write-Err "Docker not found. Please start Docker Desktop and try again."
}

if (-not $TargetVersion) {
    try {
        $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$REPO/releases/latest" `
            -Headers @{ "User-Agent" = "ThingsPanel-Upgrader" } -TimeoutSec 10
        $TargetVersion = $rel.tag_name
    } catch {
        Write-Warn "Cannot fetch latest version. Upgrade cancelled."
        exit 1
    }
}
Write-Info "Target version: $TargetVersion"

Write-Step "Backing up config"
$backup = "$ComposeFile.bak.$(Get-Date -Format 'yyyyMMddHHmmss')"
Copy-Item $ComposeFile $backup
Write-Success "Backed up to: $backup"

Write-Step "Downloading new config"
$client = New-Object System.Net.WebClient
$client.Headers.Add("User-Agent", "ThingsPanel-Upgrader")
$client.DownloadFile("$RAW_BASE/docker-compose.yml", "$ComposeFile")
$client.DownloadFile("$RAW_BASE/upgrade.ps1", "$InstallDir\upgrade.ps1")
$client.DownloadFile("$RAW_BASE/uninstall.ps1", "$InstallDir\uninstall.ps1")
Write-Success "Config files updated"

Write-Step "Pulling new images"
docker compose pull --quiet
if ($LASTEXITCODE -ne 0) { Write-Err "Image pull failed" }
Write-Success "Images pulled"

Write-Step "Restarting services"
Set-Location $InstallDir
docker compose up -d --wait --timeout 180
if ($LASTEXITCODE -ne 0) {
    Write-Err "Failed to start. View logs: docker compose -f `"$ComposeFile`" logs"
}
Write-Success "Upgrade complete! Version: $TargetVersion"
