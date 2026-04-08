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

function fetch_and_exec {
    param([string]$Url)

    Write-Host "[INFO]  Downloading installer from $Url" -ForegroundColor Cyan
    try {
        $scriptContent = Invoke-WebRequest -Uri $Url `
            -Headers @{ "User-Agent" = "ThingsPanel-Installer" } `
            -TimeoutSec 30 -UseBasicParsing `
            | Select-Object -ExpandProperty Content
        if ([string]::IsNullOrWhiteSpace($scriptContent)) {
            Write-Host "[WARN]  Empty response from $Url, trying fallback..." -ForegroundColor Yellow
            return $false
        }

        $tempFile = [System.IO.Path]::GetTempFileName() + ".ps1"
        [System.IO.File]::WriteAllText($tempFile, $scriptContent, [System.Text.Encoding]::UTF8)

        $process = Start-Process -FilePath "pwsh.exe" `
            -ArgumentList "-ExecutionPolicy", "Bypass", "-File", $tempFile `
            -Wait -NoNewWindow -PassThru
        $exitCode = $process.ExitCode
        Remove-Item $tempFile -Force -ErrorAction SilentlyContinue
        return ($exitCode -eq 0)
    } catch {
        Write-Host "[WARN]  Failed to fetch from $Url : $_" -ForegroundColor Yellow
        return $false
    }
}

if (-not (Get-Command "pwsh" -ErrorAction SilentlyContinue)) {
    Write-Host "[ERROR] pwsh (PowerShell Core) is required." -ForegroundColor Red
    Write-Host "Please install from: https://github.com/PowerShell/PowerShell" -ForegroundColor White
    Write-Host ""
    Write-Host "Alternative: download the script to a file first:" -ForegroundColor White
    Write-Host "  irm https://install.thingspanel.io/install.ps1 -OutFile install.ps1" -ForegroundColor Gray
    Write-Host "  .\install.ps1" -ForegroundColor Gray
    exit 1
}

if (-not (fetch_and_exec -Url $URL1)) {
    Write-Host "[INFO]  Primary URL failed, trying GitHub fallback..." -ForegroundColor Cyan
    if (-not (fetch_and_exec -Url $URL2)) {
        Write-Host "[ERROR] Both sources failed. Please check your network." -ForegroundColor Red
        exit 1
    }
}
