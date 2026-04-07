#Requires -RunAsAdministrator
<#
.SYNOPSIS
    ThingsPanel All-in-One — Windows 安装脚本

.DESCRIPTION
    自动检测 Docker Desktop、下载配置文件并启动 ThingsPanel。

.PARAMETER Version
    指定安装版本（默认获取最新版本）

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
    [int]$HttpPort      = 8080,
    [int]$MqttPort      = 1883
)

$ErrorActionPreference = "Stop"

$REPO      = "ThingsPanel/all-in-one-assembler"
$RAW_BASE  = "https://install.thingspanel.io"
$INSTALL_DIR = "C:\ThingsPanel"
$MIN_DOCKER_VERSION = "20.10"

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
Write-Host "              PANEL  All-in-One  Installer  (Windows)" -ForegroundColor White
Write-Host ""

function Test-Docker {
    Write-Step "检测 Docker Desktop"

    if (-not (Get-Command "docker" -ErrorAction SilentlyContinue)) {
        Write-Err "未找到 Docker。请先安装：https://www.docker.com/products/docker-desktop`n安装后重新运行此脚本。"
    }

    $dockerVer = docker version --format '{{.Server.Version}}' 2>$null
    if (-not $dockerVer) {
        Write-Err "Docker 引擎未运行。请启动 Docker Desktop 后重试。"
    }

    if ([Version]$dockerVer -lt [Version]$MIN_DOCKER_VERSION) {
        Write-Err "Docker 版本过低（当前: $dockerVer，需要 >= $MIN_DOCKER_VERSION）"
    }
    Write-Success "Docker $dockerVer"

    $composeTest = docker compose version 2>$null
    if ($LASTEXITCODE -ne 0) {
        Write-Err "未找到 'docker compose'（v2）。请升级 Docker Desktop 至最新版本。"
    }
    Write-Success "Docker Compose v2 可用"
}

function Test-Ports {
    Write-Step "检测端口占用"
    foreach ($port in @($HttpPort, $MqttPort)) {
        $conn = Get-NetTCPConnection -LocalPort $port -ErrorAction SilentlyContinue
        if ($conn) {
            Write-Warn "端口 ${port} 已被占用（PID: $($conn[0].OwningProcess)）"
        } else {
            Write-Success "端口 $port 可用"
        }
    }
}

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

function Initialize-Directories {
    Write-Step "创建目录结构"
    New-Item -ItemType Directory -Force -Path $INSTALL_DIR | Out-Null
    Write-Success "目录: $INSTALL_DIR"
}

function Get-Configs {
    Write-Step "下载配置文件"
    $client = New-Object System.Net.WebClient
    $client.Headers.Add("User-Agent", "ThingsPanel-Installer")

    $client.DownloadFile("$RAW_BASE/docker-compose.yml", "$INSTALL_DIR\docker-compose.yml")
    $client.DownloadFile("$RAW_BASE/upgrade.ps1", "$INSTALL_DIR\upgrade.ps1")
    $client.DownloadFile("$RAW_BASE/uninstall.ps1", "$INSTALL_DIR\uninstall.ps1")

    Write-Success "配置文件已下载到 $INSTALL_DIR"
}

function Start-TpServices {
    Write-Step "启动 ThingsPanel 服务"
    Set-Location $INSTALL_DIR

    $imagesTar = Join-Path $INSTALL_DIR "images.tar"

    if (Test-Path $imagesTar) {
        Write-Info "发现本地离线镜像 images.tar，正在加载（这可能需要几分钟）..."
        docker load -i $imagesTar
        if ($LASTEXITCODE -eq 0) {
            Write-Success "离线镜像已加载"
        } else {
            Write-Warn "镜像加载失败，将尝试在线拉取"
        }
    }

    if (-not (Test-Path $imagesTar) -or $LASTEXITCODE -ne 0) {
        Write-Info "拉取镜像（首次可能需要 3-5 分钟）..."
        docker compose pull --quiet
        if ($LASTEXITCODE -ne 0) { Write-Err "镜像拉取失败" }
    }

    Write-Info "启动服务，等待健康检查通过..."
    docker compose up -d --wait --timeout 180
    if ($LASTEXITCODE -ne 0) {
        Write-Err "启动失败。查看日志: docker compose -f `"$INSTALL_DIR\docker-compose.yml`" logs"
    }
    Write-Success "所有服务已启动"
}

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
                Write-Success "Web 服务已就绪: http://localhost:$HttpPort"
                return
            }
        } catch { }
        Start-Sleep -Seconds 2
        $waited += 2
        Write-Host -NoNewline "."
    }
    Write-Host ""
    Write-Warn "Web 服务尚未响应，但容器已在后台运行。请稍后访问 http://localhost:$HttpPort"
}

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

Write-Host ""
Write-Host "╔══════════════════════════════════════════════════════╗" -ForegroundColor Green
Write-Host "║        ThingsPanel 安装成功！                        ║" -ForegroundColor Green
Write-Host "╚══════════════════════════════════════════════════════╝" -ForegroundColor Green
Write-Host ""
Write-Host "  Web 界面:   http://localhost:$HttpPort" -ForegroundColor White
Write-Host "  MQTT:       localhost:$MqttPort" -ForegroundColor White
Write-Host ""
Write-Host "  安装目录:  $INSTALL_DIR" -ForegroundColor Gray
Write-Host ""
Write-Host "常用命令:" -ForegroundColor White
Write-Host "  查看状态:  docker compose -f `"$INSTALL_DIR\docker-compose.yml`" ps"
Write-Host "  查看日志:  docker compose -f `"$INSTALL_DIR\docker-compose.yml`" logs -f backend"
Write-Host "  停止服务:  docker compose -f `"$INSTALL_DIR\docker-compose.yml`" down"
Write-Host "  升级:      powershell -File `"$INSTALL_DIR\upgrade.ps1`""
Write-Host ""

Test-Docker
Test-Ports
Resolve-TpVersion
Initialize-Directories
Get-Configs
Start-TpServices
Test-Installation
New-DesktopShortcut
