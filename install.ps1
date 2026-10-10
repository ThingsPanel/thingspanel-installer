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

$REPO = "ThingsPanel/thingspanel-installer"

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

$channel = if ($env:TP_INSTALLER_CHANNEL) { $env:TP_INSTALLER_CHANNEL } else { "stable" }
if ($channel -notin @("stable", "preview")) {
    Write-Host "[ERROR] TP_INSTALLER_CHANNEL must be stable or preview." -ForegroundColor Red
    exit 1
}
$scriptDir = $PSScriptRoot
$localCheckout = $scriptDir -and (Test-Path (Join-Path $scriptDir "install.core.ps1")) -and
    (Test-Path (Join-Path $scriptDir "docker-compose.yml")) -and
    (Test-Path (Join-Path $scriptDir "scripts/notification-init.sh")) -and
    (Test-Path (Join-Path $scriptDir "scripts/notification-init.ps1")) -and
    (Test-Path (Join-Path $scriptDir "scripts/notification-db-init.sh")) -and
    (Test-Path (Join-Path $scriptDir "scripts/notification-db-init.ps1")) -and
    (Test-Path (Join-Path $scriptDir "scripts/notification-nginx.sh")) -and
    (Test-Path (Join-Path $scriptDir "scripts/notification-nginx.ps1")) -and
    (Test-Path (Join-Path $scriptDir "scripts/notification-migrate.sh")) -and
    (Test-Path (Join-Path $scriptDir "scripts/notification-migrate.ps1")) -and
    (Test-Path (Join-Path $scriptDir "config/notification-runtime.json.template")) -and
    (Test-Path (Join-Path $scriptDir "config/notification-caddyfile")) -and
    (Test-Path (Join-Path $scriptDir "config/notification-routes.conf")) -and
    (Test-Path (Join-Path $scriptDir "config/thingmodel-runtime.json")) -and
    (Test-Path (Join-Path $scriptDir "init-db/notification-create-db.sql")) -and
    (Test-Path (Join-Path $scriptDir "init-db/00-timescale-extension.sh")) -and
    (Test-Path (Join-Path $scriptDir "init-db/01-create-thingmodel-db.sh")) -and
    (Test-Path (Join-Path $scriptDir "init-db/02-thingmodel-migrations.sh"))
$ref = $env:TP_INSTALLER_REF
if (-not $ref -and $env:TP_VERSION) { $ref = $env:TP_VERSION }
if (-not $ref -and $localCheckout -and (Get-Command git -ErrorAction SilentlyContinue)) {
    $ref = git -C $scriptDir rev-parse HEAD 2>$null
}
if (-not $ref -and $channel -eq "preview") {
    Write-Host "[ERROR] Preview installs require TP_INSTALLER_REF; stable assets will not be used." -ForegroundColor Red
    exit 1
}
if (-not $ref) {
    try {
        $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$REPO/releases/latest" `
            -Headers @{ "User-Agent" = "ThingsPanel-Installer" } -TimeoutSec 10
        $ref = $rel.tag_name
    } catch {
        Write-Host "[ERROR] Cannot resolve the stable installer release ref." -ForegroundColor Red
        exit 1
    }
}
if ($ref -notmatch '^[A-Za-z0-9._/-]+$' -or $ref.StartsWith("-") -or $ref.Contains("..")) {
    Write-Host "[ERROR] Invalid installer ref." -ForegroundColor Red
    exit 1
}
$env:TP_INSTALLER_CHANNEL = $channel
$env:TP_INSTALLER_REF = $ref

Write-Host "[INFO]  Using $channel installer ref: $ref" -ForegroundColor Cyan
if ($localCheckout) {
    $env:TP_INSTALLER_LOCAL_ASSETS = $scriptDir
    $scriptContent = Get-Content -Raw -LiteralPath (Join-Path $scriptDir "install.core.ps1")
} else {
    $scriptContent = Invoke-ScriptDownload "https://raw.githubusercontent.com/$REPO/$ref/install.core.ps1"
}

if ([string]::IsNullOrWhiteSpace($scriptContent)) {
    Write-Host "[ERROR] Failed to download installer from both sources." -ForegroundColor Red
    exit 1
}

Write-Host "[INFO]  Running installer in current session..." -ForegroundColor Cyan
iex $scriptContent
