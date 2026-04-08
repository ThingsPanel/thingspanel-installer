#!/usr/bin/env pwsh
#
# ThingsPanel All-in-One — Windows bootstrap installer
#
# Usage:
#   irm https://install.thingspanel.io/install.ps1 | iex   # CDN 模式（可能被缓存）
#   irm https://raw.githubusercontent.com/ThingsPanel/thingspanel-installer/main/install.ps1 | iex  # GitHub 直链
#
# This wrapper downloads the real installer and executes it with bash.
# 执行方式：在管理员 PowerShell 中直接运行即可，会在当前窗口执行，不弹新窗口。
#

$RAW_BASE = if ($env:RAW_BASE) { $env:RAW_BASE } else { "https://raw.githubusercontent.com/ThingsPanel/thingspanel-installer/main" }

# 主站 CDN（install.thingspanel.io）不稳定时切这里
$URL1 = "$RAW_BASE/install.core.ps1"
$URL2 = "https://install.thingspanel.io/install.core.ps1"

# ── 强制 UTF-8 编码输出，防止中文乱码 ─────────────────────────────────────────
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
powershell -Command "chcp 65001 > `$null" 2>$null

# ── 下载函数：curl -fsSL 的 PowerShell 等价 ────────────────────────────────────
# curl -f   = HTTP 错误码（非 200）抛异常
# curl -L   = follow redirects（WebClient 默认跟随）
# curl -sS  = silent + show errors（-ErrorAction Stop 替代）
# curl | bash 等价于：下载到字符串，传给解释器执行
function Invoke-ScriptDownload($url) {
    try {
        $wc = New-Object System.Net.WebClient
        $wc.Headers.Add("User-Agent", "ThingsPanel-Installer")
        $content = $wc.DownloadString($url)
        # DownloadString 不对 HTTP 状态码抛异常，手动检查内容
        if ([string]::IsNullOrWhiteSpace($content)) { return $null }
        return $content
    } catch {
        return $null
    }
}

Write-Host "[INFO]  Downloading installer from $URL1" -ForegroundColor Cyan

# 默认从 CDN 下载，THINGSPLUGIN_USE_LOCAL=1 时使用本地文件（开发/调试用）
$localCore = Join-Path $PSScriptRoot "install.core.ps1"
if (($env:THINGSPLUGIN_USE_LOCAL -eq "1") -and (Test-Path $localCore)) {
    Write-Host "[INFO]  Using local install.core.ps1 (CDN skipped)" -ForegroundColor Cyan
    $scriptContent = Get-Content $localCore -Raw -Encoding UTF8
} else {
    $scriptContent = Invoke-ScriptDownload $URL1
}

if ([string]::IsNullOrWhiteSpace($scriptContent)) {
    Write-Host "[INFO]  Primary URL failed, trying GitHub fallback..." -ForegroundColor Yellow
    $scriptContent = Invoke-ScriptDownload $URL2
}

if ([string]::IsNullOrWhiteSpace($scriptContent)) {
    Write-Host "[ERROR] Failed to download installer from both sources." -ForegroundColor Red
    Read-Host "Press Enter to exit"
    exit 1
}

Write-Host "[INFO]  Running installer in current session..." -ForegroundColor Cyan

# 和 install.sh 的 "curl -fsSL $url | bash" 完全等价：
# 没有临时文件，没有编码转换，字节流直接进解释器
iex $scriptContent
