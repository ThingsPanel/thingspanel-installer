#Requires -RunAsAdministrator
<#
.SYNOPSIS
    ThingsPanel All-in-One — Windows Uninstall Script

.DESCRIPTION
    Stop containers and remove images. Data is kept unless -Purge is used.

.PARAMETER Purge
    Also delete all data (irreversible)

.EXAMPLE
    .\uninstall.ps1
    .\uninstall.ps1 -Purge
#>

[CmdletBinding()]
param(
    [switch]$Purge,
    [string]$InstallDir = "C:\ThingsPanel"
)

$ErrorActionPreference = "Stop"

function Write-Info    ($m) { Write-Host "[INFO]  $m" -ForegroundColor Cyan }
function Write-Success ($m) { Write-Host "[OK]    $m" -ForegroundColor Green }
function Write-Warn    ($m) { Write-Host "[WARN]  $m" -ForegroundColor Yellow }
function Write-Step    ($m) { Write-Host "`n[>>] $m" -ForegroundColor White -BackgroundColor DarkBlue }

Write-Host ""
Write-Host "  ThingsPanel All-in-One Uninstaller (Windows)" -ForegroundColor White
Write-Host ""

if ($Purge) {
    Write-Host ""
    Write-Host "WARNING: -Purge will delete ALL data. This cannot be undone!" -ForegroundColor Red
    $confirm = Read-Host "Type 'YES' to confirm deletion"
    if ($confirm -ne "YES") { Write-Info "Cancelled."; exit 0 }
} else {
    $confirm = Read-Host "`nUninstall ThingsPanel? (y/N)"
    if ($confirm -notmatch '^[yY]$') { Write-Info "Cancelled."; exit 0 }
}

if (-not (Get-Command "docker" -ErrorAction SilentlyContinue)) {
    Write-Warn "Docker not found. Will delete directory directly..."
} else {
    Write-Step "Stopping and removing containers"
    Set-Location $InstallDir
    docker compose down --remove-orphans 2>$null
    Write-Success "Containers stopped and removed"

    Write-Step "Removing images"
    $images = docker images --format '{{.Repository}}:{{.Tag}}' 2>$null |
        Where-Object { $_ -match 'thingspanel|thingsvis|timescaledb' }
    if ($images) {
        $images | ForEach-Object { docker rmi --force $_ 2>$null }
        Write-Success "Images removed"
    } else {
        Write-Info "No ThingsPanel images found"
    }
}

if ($Purge) {
    Write-Step "Deleting install directory"
    Set-Location $env:USERPROFILE
    if (Test-Path $InstallDir) {
        Remove-Item $InstallDir -Recurse -Force
        Write-Success "Deleted: $InstallDir"
    }
    Write-Success "All data cleared"
} else {
    Write-Warn "Data directory preserved. To remove all data: .\uninstall.ps1 -Purge"
}

$shortcut = "$env:USERPROFILE\Desktop\ThingsPanel.url"
if (Test-Path $shortcut) { Remove-Item $shortcut -ErrorAction SilentlyContinue }

Write-Host ""
Write-Host "  ThingsPanel has been uninstalled." -ForegroundColor Green
Write-Host ""
