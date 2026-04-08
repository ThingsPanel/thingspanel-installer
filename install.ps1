#!/usr/bin/env pwsh
#
# ThingsPanel All-in-One — Windows bootstrap installer
#
# Usage:
#   irm https://install.thingspanel.io/install.ps1 | iex   # CDN 模式（可能被缓存）
#   irm https://raw.githubusercontent.com/ThingsPanel/thingspanel-installer/main/install.ps1 | iex  # GitHub 直链
#
# 执行方式：在管理员 PowerShell 中直接运行即可，会在当前窗口执行，不弹新窗口。
#

$URL1 = "https://raw.githubusercontent.com/ThingsPanel/thingspanel-installer/main/install.core.ps1"
$URL2 = "https://install.thingspanel.io/install.core.ps1"

# ── 强制 UTF-8 编码输出，防止中文乱码 ─────────────────────────────────────────
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
powershell -Command "chcp 65001 > `$null" 2>$null

# ── 下载函数：curl -fsSL 的 PowerShell 等价 ────────────────────────────────────
function Invoke-ScriptDownload($url) {
    try {
        $wc = New-Object System.Net.WebClient
        $wc.Headers.Add("User-Agent", "ThingsPanel-Installer")
        $content = $wc.DownloadString($url)
        if ([string]::IsNullOrWhiteSpace($content)) { return $null }
        return $content
    } catch {
        return $null
    }
}

Write-Host "[INFO]  Downloading installer from $URL1" -ForegroundColor Cyan

$scriptContent = Invoke-ScriptDownload $URL1
if ([string]::IsNullOrWhiteSpace($scriptContent)) {
    Write-Host "[INFO]  Primary URL failed, trying fallback..." -ForegroundColor Yellow
    $scriptContent = Invoke-ScriptDownload $URL2
}

if ([string]::IsNullOrWhiteSpace($scriptContent)) {
    Write-Host "[ERROR] Failed to download installer from both sources." -ForegroundColor Red
    exit 1
}

Write-Host "[INFO]  Running installer in current session..." -ForegroundColor Cyan
iex $scriptContent
