#Requires -RunAsAdministrator
<#
.SYNOPSIS
    ThingsPanel All-in-One — Windows 安装脚本

.DESCRIPTION
    自动检测 Docker Desktop、下载配置文件、生成随机密码并启动 ThingsPanel。

.PARAMETER Version
    指定安装版本（默认获取最新版本）

.PARAMETER InstallDir
    安装目录（默认 C:\ThingsPanel）

.PARAMETER DataDir
    数据目录（默认 C:\ThingsPanel\data）

.PARAMETER HttpPort
    Web 服务端口（默认 8080）

.PARAMETER MqttPort
    MQTT 端口（默认 1883）

.EXAMPLE
    .\install.ps1
    .\install.ps1 -Version v1.2.0 -HttpPort 9090
    powershell -ExecutionPolicy Bypass -File install.ps1
#>

[CmdletBinding()]
param(
    [string]$Version   = "",
    [string]$InstallDir = "C:\ThingsPanel",
    [string]$DataDir    = "",
    [int]$HttpPort      = 8080,
    [int]$MqttPort      = 1883
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# ── 常量 ──────────────────────────────────────────────────────────────────────
$REPO    = "ThingsPanel/all-in-one-assembler"
$RAW_BASE = "https://raw.githubusercontent.com/$REPO/main"

if (-not $DataDir) { $DataDir = Join-Path $InstallDir "data" }

# ── 颜色输出 ──────────────────────────────────────────────────────────────────
function Write-Info    ($msg) { Write-Host "[INFO]  $msg" -ForegroundColor Cyan }
function Write-Success ($msg) { Write-Host "[OK]    $msg" -ForegroundColor Green }
function Write-Warn    ($msg) { Write-Host "[WARN]  $msg" -ForegroundColor Yellow }
function Write-Step    ($msg) { Write-Host "`n▶ $msg" -ForegroundColor White -BackgroundColor DarkBlue }
function Write-Err     ($msg) { Write-Host "[ERROR] $msg" -ForegroundColor Red; exit 1 }

function Write-Banner {
    Write-Host ""
    Write-Host "  ████████╗██╗  ██╗██╗███╗   ██╗ ██████╗ ███████╗" -ForegroundColor Cyan
    Write-Host "     ██╔══╝██║  ██║██║████╗  ██║██╔════╝ ██╔════╝" -ForegroundColor Cyan
    Write-Host "     ██║   ███████║██║██╔██╗ ██║██║  ███╗███████╗" -ForegroundColor Cyan
    Write-Host "     ██║   ██╔══██║██║██║╚██╗██║██║   ██║╚════██║" -ForegroundColor Cyan
    Write-Host "     ██║   ██║  ██║██║██║ ╚████║╚██████╔╝███████║" -ForegroundColor Cyan
    Write-Host "     ╚═╝   ╚═╝  ╚═╝╚═╝╚═╝  ╚═══╝ ╚═════╝ ╚══════╝" -ForegroundColor Cyan
    Write-Host ""
    Write-Host "              PANEL  All-in-One  Installer  (Windows)" -ForegroundColor White
    Write-Host ""
}

# ── 生成随机 hex 字符串 ────────────────────────────────────────────────────────
function New-RandomHex([int]$Length = 32) {
    $bytes = New-Object byte[] $Length
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    return ($bytes | ForEach-Object { $_.ToString("x2") }) -join ""
}

# ── 版本比较 ──────────────────────────────────────────────────────────────────
function Compare-Version([string]$v1, [string]$v2) {
    try {
        $a = [Version]($v1 -replace '^v', '')
        $b = [Version]($v2 -replace '^v', '')
        return $a.CompareTo($b)
    } catch { return 0 }
}

# ── 检测 Docker ────────────────────────────────────────────────────────────────
function Test-Docker {
    Write-Step "检测 Docker Desktop"

    if (-not (Get-Command "docker" -ErrorAction SilentlyContinue)) {
        Write-Err "未找到 Docker Desktop。请先安装：https://www.docker.com/products/docker-desktop`n安装后重新运行此脚本。"
    }

    try {
        $dockerVer = (docker version --format '{{.Server.Version}}' 2>$null)
        Write-Success "Docker $dockerVer"
    } catch {
        Write-Err "Docker 引擎未运行。请启动 Docker Desktop 后重试。"
    }

    # 检测 compose v2
    $composeTest = docker compose version 2>$null
    if ($LASTEXITCODE -ne 0) {
        Write-Err "未找到 'docker compose'（v2）。请升级 Docker Desktop 至最新版本。"
    }
    Write-Success "Docker Compose v2 可用"
}

# ── 检测端口 ──────────────────────────────────────────────────────────────────
function Test-Ports {
    Write-Step "检测端口占用"
    foreach ($port in @($HttpPort, $MqttPort)) {
        $conn = Get-NetTCPConnection -LocalPort $port -ErrorAction SilentlyContinue
        if ($conn) {
            Write-Warn "端口 $port 已被占用（PID: $($conn.OwningProcess)）"
        } else {
            Write-Success "端口 $port 可用"
        }
    }
}

# ── 确定版本 ──────────────────────────────────────────────────────────────────
function Resolve-TpVersion {
    Write-Step "确定安装版本"

    if ($Version -ne "") {
        $script:TpVersion = $Version
        Write-Info "使用指定版本: $Version"
        return
    }

    try {
        $rel = Invoke-RestMethod `
            -Uri "https://api.github.com/repos/$REPO/releases/latest" `
            -Headers @{ "User-Agent" = "ThingsPanel-Installer" } `
            -TimeoutSec 10
        $script:TpVersion = $rel.tag_name
    } catch {
        $script:TpVersion = "v1.1.13.6"
        Write-Warn "无法获取最新版本，使用默认: $($script:TpVersion)"
    }
    Write-Success "将安装版本: $($script:TpVersion)"
}

# ── 创建目录 ──────────────────────────────────────────────────────────────────
function Initialize-Directories {
    Write-Step "创建目录结构"
    $dirs = @(
        $InstallDir,
        "$DataDir\postgres",
        "$DataDir\redis",
        "$DataDir\gmqtt",
        "$DataDir\backend\files",
        "$DataDir\backend\configs",
        "$InstallDir\nginx"
    )
    foreach ($dir in $dirs) {
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
    }
    Write-Success "目录: $InstallDir"
}

# ── 下载配置文件 ───────────────────────────────────────────────────────────────
function Get-Configs {
    Write-Step "下载配置文件"
    $client = New-Object System.Net.WebClient
    $client.Headers.Add("User-Agent", "ThingsPanel-Installer")

    $client.DownloadFile("$RAW_BASE/docker-compose.yml", "$InstallDir\docker-compose.yml")
    $client.DownloadFile("$RAW_BASE/nginx/nginx.conf", "$InstallDir\nginx\nginx.conf")
    $client.DownloadFile("$RAW_BASE/upgrade.ps1", "$InstallDir\upgrade.ps1")
    $client.DownloadFile("$RAW_BASE/uninstall.ps1", "$InstallDir\uninstall.ps1")

    Write-Success "配置文件已下载到 $InstallDir"
}

# ── 生成 .env ──────────────────────────────────────────────────────────────────
function New-EnvFile {
    Write-Step "生成环境变量"
    $envFile = "$InstallDir\.env"

    if (Test-Path $envFile) {
        Write-Warn ".env 已存在，跳过（如需重置请删除 $envFile 后重新运行）"
        return
    }

    $pgPass    = New-RandomHex 32
    $redisPass = New-RandomHex 32
    $authSec   = New-RandomHex 32
    $timestamp = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

    # Windows 路径转 Docker 路径格式 (C:\ThingsPanel\data → /c/ThingsPanel/data)
    $dockerDataDir = $DataDir -replace "\\", "/" -replace "^([A-Za-z]):", { "/" + $_.Value[0].ToString().ToLower() }

    $envContent = @"
# ThingsPanel All-in-One — 自动生成于 $timestamp
# 请妥善保管此文件，其中包含数据库密码等敏感信息

TP_VERSION=$($script:TpVersion)
TP_VUE_VERSION=$($script:TpVersion)
TP_BACKEND_VERSION=$($script:TpVersion)
TP_GMQTT_VERSION=v1.1.6
TP_REDIS_VERSION=6.2.7
TP_MODBUS_VERSION=v1.0.6.1
TP_HTTP_ADAPTER_VERSION=v1.0.0
TP_THINGSVIS_SERVER_VERSION=v1.0.4
TP_THINGSVIS_STUDIO_VERSION=v1.0.4
TP_TIMESCALEDB_VERSION=14

POSTGRES_PASSWORD=$pgPass
REDIS_PASSWORD=$redisPass
AUTH_SECRET=$authSec

DATA_DIR=$dockerDataDir
HTTP_PORT=$HttpPort
MQTT_PORT=$MqttPort
MODBUS_TCP_PORT=502
MODBUS_RTU_PORT=503

TZ=Asia/Shanghai
TP_LOG_LEVEL=error
"@

    Set-Content -Path $envFile -Value $envContent -Encoding UTF8
    Write-Success ".env 已生成（密码随机生成）"
}

# ── 启动服务 ──────────────────────────────────────────────────────────────────
function Start-TpServices {
    Write-Step "启动 ThingsPanel 服务"
    Set-Location $InstallDir

    Write-Info "拉取镜像（首次可能需要 3-5 分钟）..."
    docker compose pull --quiet
    if ($LASTEXITCODE -ne 0) { Write-Err "镜像拉取失败" }

    Write-Info "启动服务，等待健康检查通过..."
    docker compose up -d --wait --timeout 180
    if ($LASTEXITCODE -ne 0) {
        Write-Err "启动失败。查看日志: docker compose -f `"$InstallDir\docker-compose.yml`" logs"
    }
    Write-Success "所有服务已启动"
}

# ── 验证安装 ──────────────────────────────────────────────────────────────────
function Test-Installation {
    Write-Step "验证安装"
    $url = "http://localhost:$HttpPort/health"
    $maxWait = 60
    $waited = 0

    Write-Info "等待 Web 服务就绪..."
    while ($waited -lt $maxWait) {
        try {
            $resp = Invoke-WebRequest -Uri $url -TimeoutSec 3 -UseBasicParsing -ErrorAction Stop
            if ($resp.StatusCode -eq 200) {
                Write-Success "Web 服务就绪: http://localhost:$HttpPort"
                return
            }
        } catch { }
        Start-Sleep -Seconds 2
        $waited += 2
        Write-Host -NoNewline "."
    }
    Write-Host ""
    Write-Warn "Web 服务尚未响应，请稍后访问 http://localhost:$HttpPort"
}

# ── 创建桌面快捷方式 ───────────────────────────────────────────────────────────
function New-DesktopShortcut {
    Write-Step "创建桌面快捷方式"
    try {
        $wsh = New-Object -ComObject WScript.Shell
        $shortcut = $wsh.CreateShortcut("$env:USERPROFILE\Desktop\ThingsPanel.url")
        $shortcut.TargetPath = "http://localhost:$HttpPort"
        $shortcut.Save()
        Write-Success "桌面快捷方式已创建"
    } catch {
        Write-Warn "创建快捷方式失败（非关键错误）"
    }
}

# ── 完成提示 ──────────────────────────────────────────────────────────────────
function Write-Finish {
    Write-Host ""
    Write-Host "╔══════════════════════════════════════════════════════╗" -ForegroundColor Green
    Write-Host "║        ThingsPanel 安装成功！                        ║" -ForegroundColor Green
    Write-Host "╚══════════════════════════════════════════════════════╝" -ForegroundColor Green
    Write-Host ""
    Write-Host "  🌐  Web 界面:   http://localhost:$HttpPort" -ForegroundColor White
    Write-Host "  📡  MQTT:       localhost:$MqttPort" -ForegroundColor White
    Write-Host ""
    Write-Host "  📁  安装目录:  $InstallDir" -ForegroundColor Gray
    Write-Host "  💾  数据目录:  $DataDir" -ForegroundColor Gray
    Write-Host ""
    Write-Host "常用命令:" -ForegroundColor White
    Write-Host "  查看状态:  docker compose -f `"$InstallDir\docker-compose.yml`" ps"
    Write-Host "  查看日志:  docker compose -f `"$InstallDir\docker-compose.yml`" logs -f backend"
    Write-Host "  停止服务:  docker compose -f `"$InstallDir\docker-compose.yml`" down"
    Write-Host "  升级:      powershell -File `"$InstallDir\upgrade.ps1`""
    Write-Host ""
}

# ── 主流程 ────────────────────────────────────────────────────────────────────
function Main {
    Write-Banner
    Test-Docker
    Test-Ports
    Resolve-TpVersion
    Initialize-Directories
    Get-Configs
    New-EnvFile
    Start-TpServices
    Test-Installation
    New-DesktopShortcut
    Write-Finish
}

Main
