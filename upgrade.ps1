#Requires -RunAsAdministrator
<#
.SYNOPSIS
    ThingsPanel All-in-One — Windows 升级脚本

.DESCRIPTION
    备份配置、拉取新镜像、重启服务。

.PARAMETER TargetVersion
    指定目标版本（默认获取最新版本）

.PARAMETER InstallDir
    安装目录（默认 C:\ThingsPanel）

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

$REPO     = "ThingsPanel/all-in-one-assembler"
$RAW_BASE = "https://install.thingspanel.io"

function Write-Info    ($m) { Write-Host "[INFO]  $m" -ForegroundColor Cyan }
function Write-Success ($m) { Write-Host "[OK]    $m" -ForegroundColor Green }
function Write-Warn    ($m) { Write-Host "[WARN]  $m" -ForegroundColor Yellow }
function Write-Step    ($m) { Write-Host "`n▶ $m" -ForegroundColor White -BackgroundColor DarkBlue }
function Write-Err     ($m) { Write-Host "[ERROR] $m" -ForegroundColor Red; exit 1 }

Write-Host ""
Write-Host "  ████████╗██╗  ██╗██╗███╗   ██╗ ██████╗ ███████╗" -ForegroundColor Cyan
Write-Host "     ██╔══╝██║  ██║██║████╗  ██║██╔════╝ ██╔════╝" -ForegroundColor Cyan
Write-Host "     ██║   ███████║██║██╔██╗ ██║██║  ███╗███████╗" -ForegroundColor Cyan
Write-Host "     ██║   ██╔══██║██║██║╚██╗██║██║   ██║╚════██║" -ForegroundColor Cyan
Write-Host "     ██║   ██║  ██║██║██║ ╚████║╚██████╔╝███████║" -ForegroundColor Cyan
Write-Host "     ╚═╝   ╚═╝  ╚═╝╚═╝╚═╝  ╚═══╝ ╚═════╝ ╚══════╝" -ForegroundColor Cyan
Write-Host ""
Write-Host "              PANEL  All-in-One  Upgrade  (Windows)" -ForegroundColor White
Write-Host ""

$ComposeFile = "$InstallDir\docker-compose.yml"
if (-not (Test-Path $ComposeFile)) {
    Write-Err "未找到 $ComposeFile，请先运行安装脚本"
}

if (-not (Get-Command "docker" -ErrorAction SilentlyContinue)) {
    Write-Err "Docker 未运行，请启动 Docker Desktop 后重试"
}

if (-not $TargetVersion) {
    try {
        $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$REPO/releases/latest" `
            -Headers @{ "User-Agent" = "ThingsPanel-Upgrader" } -TimeoutSec 10
        $TargetVersion = $rel.tag_name
    } catch {
        Write-Warn "无法获取最新版本，升级取消"
        exit 1
    }
}
Write-Info "目标版本: $TargetVersion"

Write-Step "备份配置文件"
$backup = "$ComposeFile.bak.$(Get-Date -Format 'yyyyMMddHHmmss')"
Copy-Item $ComposeFile $backup
Write-Success "已备份到 $backup"

Write-Step "下载新版本配置"
$client = New-Object System.Net.WebClient
$client.Headers.Add("User-Agent", "ThingsPanel-Upgrader")
$client.DownloadFile("$RAW_BASE/docker-compose.yml", "$ComposeFile")
$client.DownloadFile("$RAW_BASE/upgrade.ps1", "$InstallDir\upgrade.ps1")
$client.DownloadFile("$RAW_BASE/uninstall.ps1", "$InstallDir\uninstall.ps1")
Write-Success "配置文件已更新"

Write-Step "拉取新镜像"
docker compose pull --quiet
if ($LASTEXITCODE -ne 0) { Write-Err "镜像拉取失败" }
Write-Success "镜像拉取完成"

Write-Step "重启服务"
Set-Location $InstallDir
docker compose up -d --wait --timeout 180
if ($LASTEXITCODE -ne 0) {
    Write-Err "启动失败。查看日志: docker compose -f `"$ComposeFile`" logs"
}
Write-Success "升级完成，当前版本: $TargetVersion"
