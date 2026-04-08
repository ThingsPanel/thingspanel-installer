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
# 执行方式：在管理员 PowerShell 中直接运行即可，会在当前窗口执行，不弹新窗口。
#

$RAW_BASE = if ($env:RAW_BASE) { $env:RAW_BASE } else { "https://install.thingspanel.io" }

$URL1 = "$RAW_BASE/install.core.ps1"
$URL2 = "https://raw.githubusercontent.com/ThingsPanel/thingspanel-installer/main/install.core.ps1"

# ── 强制 UTF-8 编码输出，防止中文乱码 ─────────────────────────────────────────
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
powershell -Command "chcp 65001 > `$null" 2>$null

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

    Write-Host "[INFO]  Running installer in current session..." -ForegroundColor Cyan
    & $tempFile

} finally {
    Remove-Item $tempFile -Force -ErrorAction SilentlyContinue
}
