#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [string]$Version   = "",
    [int]$HttpPort      = 8080,
    [int]$MqttPort      = 1883
)

$ErrorActionPreference = "Stop"

$REPO          = "ThingsPanel/all-in-one-assembler"
$RAW_BASE      = "https://install.thingspanel.io"
$INSTALL_DIR   = "C:\ThingsPanel"
$MIN_DOCKER_VER = "20.10"

function Write-Info    ($m) { Write-Host "[INFO]  $m" -ForegroundColor Cyan }
function Write-Success ($m) { Write-Host "[OK]    $m" -ForegroundColor Green }
function Write-Warn    ($m) { Write-Host "[WARN]  $m" -ForegroundColor Yellow }
function Write-Step    ($m) { Write-Host "`n[>>] $m" -ForegroundColor White -BackgroundColor DarkBlue }
function Write-Err     ($m) { Write-Host "[ERROR] $m" -ForegroundColor Red; exit 1 }

Write-Host ""
Write-Host "  ThingsPanel All-in-One Installer (Windows)" -ForegroundColor White
Write-Host ""

# ── Docker 检查 ──────────────────────────────────────────────────────────────
Write-Step "Checking Docker Desktop"

if (-not (Get-Command "docker" -ErrorAction SilentlyContinue)) {
    Write-Err "Docker not found. Please install Docker Desktop first."
}

$out = docker version --format '{{.Server.Version}}'
if ([string]::IsNullOrEmpty($out)) {
    Write-Err "Docker engine not running. Please start Docker Desktop."
}
if ([Version]$out -lt [Version]$MIN_DOCKER_VER) {
    Write-Err "Docker version too old (current: $out, need >= $MIN_DOCKER_VER)"
}
Write-Success "Docker $out"

try { docker compose version 2>/dev/null } catch {
    Write-Err "docker compose (v2) not found. Please upgrade Docker Desktop."
}
Write-Success "Docker Compose v2 available"

# ── 端口检查 ──────────────────────────────────────────────────────────────────
Write-Step "Checking port availability"
foreach ($port in @($HttpPort, $MqttPort)) {
    $conn = Get-NetTCPConnection -LocalPort $port -ErrorAction SilentlyContinue
    if ($conn) {
        Write-Warn "Port ${port} is in use (PID: $($conn[0].OwningProcess))"
    } else {
        Write-Success "Port $port is available"
    }
}

# ── 版本解析 ──────────────────────────────────────────────────────────────────
Write-Step "Determining version"
if ($Version -ne "") {
    $script:TpVersion = $Version
    Write-Info "Using specified version: $Version"
} else {
    try {
        $rel = Invoke-RestMethod `
            -Uri "https://api.github.com/repos/$REPO/releases/latest" `
            -Headers @{ "User-Agent" = "ThingsPanel-Installer" } `
            -TimeoutSec 10
        $script:TpVersion = $rel.tag_name
    } catch {
        $script:TpVersion = "v1.1.13.6"
        Write-Warn "Cannot fetch latest version, using default: $($script:TpVersion)"
    }
}
Write-Success "Installing version: $($script:TpVersion)"

# ── 创建目录 ──────────────────────────────────────────────────────────────────
Write-Step "Creating directory structure"
New-Item -ItemType Directory -Force -Path $INSTALL_DIR | Out-Null
Write-Success "Directory: $INSTALL_DIR"

# ── 下载配置 ──────────────────────────────────────────────────────────────────
Write-Step "Downloading config files"
$client = New-Object System.Net.WebClient
$client.Headers.Add("User-Agent", "ThingsPanel-Installer")
$client.DownloadFile("$RAW_BASE/docker-compose.yml", "$INSTALL_DIR\docker-compose.yml")
$client.DownloadFile("$RAW_BASE/upgrade.ps1",    "$INSTALL_DIR\upgrade.ps1")
$client.DownloadFile("$RAW_BASE/uninstall.ps1",   "$INSTALL_DIR\uninstall.ps1")
Write-Success "Config files downloaded to $INSTALL_DIR"

# ── 启动服务 ──────────────────────────────────────────────────────────────────
Write-Step "Starting ThingsPanel services"
Set-Location $INSTALL_DIR

$imagesTar = Join-Path $INSTALL_DIR "images.tar"

if (Test-Path $imagesTar) {
    Write-Info "Found local images.tar, loading (may take a few minutes)..."
    docker load -i $imagesTar 2>&1 | Out-Null
    if ($LASTEXITCODE -eq 0) {
        Write-Success "Local images loaded"
    } else {
        Write-Warn "Image load failed, will try online pull"
        $imagesTar = $null
    }
}

if (-not (Test-Path $imagesTar)) {
    Write-Info "Pulling images (first run may take 3-5 minutes)..."
    docker compose pull --quiet 2>&1 | Out-Null
}

Write-Info "Cleaning up any existing services..."
docker compose down --remove-orphans 2>&1 | Out-Null

Write-Info "Starting services, waiting for health checks..."
$errFile = "$env:TEMP\tp_docker_err_$PID.txt"
docker compose up -d --wait --timeout 180 2> $errFile | Out-Null
$exitCode = $LASTEXITCODE
if ((Test-Path $errFile) -and (Get-Content $errFile -Raw) -match "error|failed") {
    $errContent = Get-Content $errFile -Raw
    Remove-Item $errFile -Force -ErrorAction SilentlyContinue
    Write-Err "Docker compose failed: $errContent"
}
Remove-Item $errFile -Force -ErrorAction SilentlyContinue

$containers = docker compose ps -q
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrEmpty($containers)) {
    Write-Err "Failed to start. View logs: docker compose -f `"$INSTALL_DIR\docker-compose.yml`" logs"
}
Write-Success "All services started"

# ── 安装验证 ──────────────────────────────────────────────────────────────────
Write-Step "Verifying installation"
$url = "http://localhost:$HttpPort/health"
$maxWait = 60
$waited = 0

Write-Info "Waiting for web service..."
while ($waited -lt $maxWait) {
    try {
        $resp = Invoke-WebRequest -Uri $url -TimeoutSec 3 -UseBasicParsing -ErrorAction Stop
        if ($resp.StatusCode -eq 200) {
            Write-Success "Web service ready: http://localhost:$HttpPort"
            break
        }
    } catch { }
    Start-Sleep -Seconds 2
    $waited += 2
    Write-Host -NoNewline "."
}
Write-Host ""
Write-Warn "Web service not responding yet. Please visit http://localhost:$HttpPort shortly."

# ── 创建桌面快捷方式 ──────────────────────────────────────────────────────────
Write-Step "Creating desktop shortcut"
try {
    $wsh = New-Object -ComObject WScript.Shell
    $shortcut = $wsh.CreateShortcut("$env:USERPROFILE\Desktop\ThingsPanel.url")
    $shortcut.TargetPath = "http://localhost:$HttpPort"
    $shortcut.Save()
    Write-Success "Desktop shortcut created"
} catch {
    Write-Warn "Failed to create shortcut (non-critical)"
}

# ── 完成 ─────────────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "========================================" -ForegroundColor Green
Write-Host "   ThingsPanel installed successfully!" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor Green
Write-Host ""
Write-Host "  Web UI:   http://localhost:$HttpPort" -ForegroundColor White
Write-Host "  MQTT:     localhost:$MqttPort" -ForegroundColor White
Write-Host ""
Write-Host "  Install dir: $INSTALL_DIR" -ForegroundColor Gray
Write-Host ""
Write-Host "Common commands:" -ForegroundColor White
Write-Host "  Status:  docker compose -f `"$INSTALL_DIR\docker-compose.yml`" ps"
Write-Host "  Logs:    docker compose -f `"$INSTALL_DIR\docker-compose.yml`" logs -f backend"
Write-Host "  Stop:    docker compose -f `"$INSTALL_DIR\docker-compose.yml`" down"
Write-Host "  Upgrade: powershell -File `"$INSTALL_DIR\upgrade.ps1`""
Write-Host ""