#!/usr/bin/env pwsh
#
# ThingsPanel All-in-One — Windows bootstrap installer
#
# Usage:
#   irm https://install.thingspanel.io/install.ps1 | iex
#
# This wrapper downloads the real installer and executes it with -File,
# so that [CmdletBinding()], param(), and #Requires work correctly —
# unlike when the script body is piped through Invoke-Expression (iex).
#

$RAW_BASE = if ($env:RAW_BASE) { $env:RAW_BASE } else { "https://install.thingspanel.io" }

$URL1 = "$RAW_BASE/install.core.ps1"
$URL2 = "https://raw.githubusercontent.com/ThingsPanel/thingspanel-installer/main/install.core.ps1"

if (-not (Get-Command "pwsh" -ErrorAction SilentlyContinue)) {
    Write-Host "[ERROR] pwsh (PowerShell Core) is required." -ForegroundColor Red
    Write-Host "Please install from: https://github.com/PowerShell/PowerShell" -ForegroundColor White
    Write-Host ""
    Write-Host "Alternative — download the script to a file first:" -ForegroundColor White
    Write-Host "  irm https://install.thingspanel.io/install.ps1 -OutFile install.ps1" -ForegroundColor Gray
    Write-Host "  .\install.ps1" -ForegroundColor Gray
    Read-Host "Press Enter to exit"
    exit 1
}

$tempFile = "$env:TEMP\thingspanel_install_$PID.ps1"
try {
    Write-Host "[INFO]  Downloading installer from $URL1" -ForegroundColor Cyan
    $scriptContent = Invoke-WebRequest -Uri $URL1 `
        -Headers @{ "User-Agent" = "ThingsPanel-Installer" } `
        -TimeoutSec 30 -UseBasicParsing `
        | Select-Object -ExpandProperty Content

    if ([string]::IsNullOrWhiteSpace($scriptContent)) {
        Write-Host "[INFO]  Primary URL empty, trying GitHub fallback..." -ForegroundColor Yellow
        $scriptContent = Invoke-WebRequest -Uri $URL2 `
            -Headers @{ "User-Agent" = "ThingsPanel-Installer" } `
            -TimeoutSec 30 -UseBasicParsing `
            | Select-Object -ExpandProperty Content
    }

    if ([string]::IsNullOrWhiteSpace($scriptContent)) {
        Write-Host "[ERROR] Failed to download installer from both sources." -ForegroundColor Red
        Read-Host "Press Enter to exit"
        exit 1
    }

    [System.IO.File]::WriteAllText($tempFile, $scriptContent, [System.Text.Encoding]::UTF8)

    Start-Process -FilePath "pwsh.exe" `
        -ArgumentList "-ExecutionPolicy", "Bypass", "-File", $tempFile `
        -Wait -PassThru | Out-Null

} finally {
    Remove-Item $tempFile -Force -ErrorAction SilentlyContinue
}
