#Requires -RunAsAdministrator
<#
.SYNOPSIS
    ThingsPanel All-in-One — Windows 升级脚本
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
function Write-Step    ($m) { Write-Host "`n▶ $m" -ForegroundColor White }
function Write-Err     ($m) { Write-Host "[ERROR] $m" -ForegroundColor Red; exit 1 }

$EnvFile = "$InstallDir\.env"
Test-Path $EnvFile | Out-Null

Write-Host "`nThingsPanel — 升级程序" -ForegroundColor White

# 备份 .env
Write-Step "备份配置"
$backup = "$EnvFile.bak.$(Get-Date -Format 'yyyyMMddHHmmss')"
Copy-Item $EnvFile $backup
Write-Success ".env 已备份到 $backup"

# 读取当前版本
$currentVersion = (Get-Content $EnvFile | Where-Object { $_ -match '^TP_VERSION=' }) -replace 'TP_VERSION=', ''
Write-Info "当前版本: $currentVersion"

# 确定目标版本
if (-not $TargetVersion) {
    try {
        $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$REPO/releases/latest" `
            -Headers @{ "User-Agent" = "ThingsPanel-Upgrader" } -TimeoutSec 10
        $TargetVersion = $rel.tag_name
    } catch {
        $TargetVersion = $currentVersion
        Write-Warn "无法获取最新版本，保持当前版本"
    }
}
Write-Info "目标版本: $TargetVersion"

if ($currentVersion -eq $TargetVersion) {
    Write-Warn "已是最新版本，无需升级"
    exit 0
}

# 更新版本号
Write-Step "更新版本配置"
(Get-Content $EnvFile) `
    -replace "^TP_VERSION=.*", "TP_VERSION=$TargetVersion" `
    -replace "^TP_VUE_VERSION=.*", "TP_VUE_VERSION=$TargetVersion" `
    -replace "^TP_BACKEND_VERSION=.*", "TP_BACKEND_VERSION=$TargetVersion" |
    Set-Content $EnvFile -Encoding UTF8
Write-Success "版本号更新为 $TargetVersion"

# 更新配置文件
Write-Step "更新配置文件"
$client = New-Object System.Net.WebClient
$client.Headers.Add("User-Agent", "ThingsPanel-Upgrader")
$client.DownloadFile("$RAW_BASE/docker-compose.yml", "$InstallDir\docker-compose.yml")
$client.DownloadFile("$RAW_BASE/nginx/nginx.conf", "$InstallDir\nginx\nginx.conf")
Write-Success "配置文件已更新"

# 重启服务
Write-Step "升级服务"
Set-Location $InstallDir
Write-Info "拉取新版本镜像..."
docker compose pull --quiet
Write-Info "重启服务..."
docker compose up -d --wait --timeout 180
Write-Success "升级完成！当前版本: $TargetVersion"
