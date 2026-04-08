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

# PowerShell 5.1 中 .ps1 文件应该用 UTF-8 无 BOM 写入
# & 执行时 PS 会自动识别 UTF-8（无 BOM），绝对不要加 BOM！
$utf8NoBom = New-Object System.Text.UTF8Encoding $false

# ── 下载函数：检查 HTTP 状态码，非 200 视为失败，走 fallback ───────────────────
function Get-ScriptContent($url) {
    $resp = try {
        Invoke-WebRequest -Uri $url `
            -Headers @{ "User-Agent" = "ThingsPanel-Installer" } `
            -TimeoutSec 30 -UseBasicParsing `
            -ErrorAction Stop
    } catch {
        # 网络级错误（DNS、连接超时、证书等）
        return $null
    }
    if ($resp.StatusCode -eq 200) {
        return $resp.Content
    }
    # HTTP 4xx / 5xx 也走 fallback
    return $null
}

try {
    Write-Host "[INFO]  Downloading installer from $URL1" -ForegroundColor Cyan
    $scriptContent = Get-ScriptContent $URL1

    if ([string]::IsNullOrWhiteSpace($scriptContent)) {
        Write-Host "[INFO]  Primary URL failed, trying GitHub fallback..." -ForegroundColor Yellow
        $scriptContent = Get-ScriptContent $URL2
    }

    if ([string]::IsNullOrWhiteSpace($scriptContent)) {
        Write-Host "[ERROR] Failed to download installer from both sources." -ForegroundColor Red
        Read-Host "Press Enter to exit"
        exit 1
    }

    # 写入时用 UTF-8 无 BOM，这是 .ps1 文件的标准格式
    [System.IO.File]::WriteAllText($tempFile, $scriptContent, $utf8NoBom)

    Write-Host "[INFO]  Running installer in current session..." -ForegroundColor Cyan
    powershell -ExecutionPolicy Bypass -File $tempFile

} finally {
    Remove-Item $tempFile -Force -ErrorAction SilentlyContinue
}
