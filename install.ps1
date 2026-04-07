#Requires -RunAsAdministrator
<#
.SYNOPSIS
    ThingsPanel All-in-One — Windows Installer

.DESCRIPTION
    Auto-detect Docker Desktop, download configs, and start ThingsPanel.

.PARAMETER Version
    Specify version (default: latest)

.PARAMETER HttpPort
    Web port (default: 8080)

.PARAMETER MqttPort
    MQTT port (default: 1883)

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

$REPO          = "ThingsPanel/all-in-one-assembler"
$RAW_BASE      = "https://install.thingspanel.io"
$INSTALL_DIR   = "C:\ThingsPanel"
$MIN_DOCKER_VER = "20.10"

function Write-Info    ($m) { Write-Host "[INFO]  $m" -ForegroundColor Cyan }
function Write-Success ($m) { Write-Host "[OK]    $m" -ForegroundColor Green }
function Write-Warn    ($m) { Write-Host "[WARN]  $m" -ForegroundColor Yellow }
function Write-Step    ($m) { Write-Host "`n[>>] $m" -ForegroundColor White -BackgroundColor DarkBlue }
function Write-Err     ($m) { Write-Host "[ERROR] $m" -ForegroundColor Red; throw $m }

Write-Host ""
Write-Host "  ThingsPanel All-in-One Installer (Windows)" -ForegroundColor White
Write-Host ""

function Test-Docker {
    Write-Step "Checking Docker Desktop"

    if (-not (Get-Command "docker" -ErrorAction SilentlyContinue)) {
        Write-Err "Docker not found. Please install Docker Desktop first: https://www.docker.com/products/docker-desktop"
    }

    $out = docker version --format '{{.Server.Version}}' 2>$null
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrEmpty($out)) {
        Write-Err "Docker engine not running. Please start Docker Desktop and try again."
    }
    if ([Version]$out -lt [Version]$MIN_DOCKER_VER) {
        Write-Err "Docker version too old (current: $out, need >= $MIN_DOCKER_VER)"
    }
    Write-Success "Docker $out"

    $out = docker compose version 2>$null
    if ($LASTEXITCODE -ne 0) {
        Write-Err "docker compose (v2) not found. Please upgrade Docker Desktop."
    }
    Write-Success "Docker Compose v2 available"
}

function Test-Ports {
    Write-Step "Checking port availability"
    foreach ($port in @($HttpPort, $MqttPort)) {
        $conn = Get-NetTCPConnection -LocalPort $port -ErrorAction SilentlyContinue
        if ($conn) {
            Write-Warn "Port ${port} is in use (PID: $($conn[0].OwningProcess))"
        } else {
            Write-Success "Port $port is available"
        }
    }
}

function Resolve-TpVersion {
    Write-Step "Determining version"

    if ($Version -ne "") {
        $script:TpVersion = $Version
        Write-Info "Using specified version: $Version"
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
        Write-Warn "Cannot fetch latest version, using default: $($script:TpVersion)"
    }
    Write-Success "Installing version: $($script:TpVersion)"
}

function Initialize-Directories {
    Write-Step "Creating directory structure"
    New-Item -ItemType Directory -Force -Path $INSTALL_DIR | Out-Null
    Write-Success "Directory: $INSTALL_DIR"
}

function Get-Configs {
    Write-Step "Downloading config files"
    $client = New-Object System.Net.WebClient
    $client.Headers.Add("User-Agent", "ThingsPanel-Installer")

    $client.DownloadFile("$RAW_BASE/docker-compose.yml", "$INSTALL_DIR\docker-compose.yml")
    $client.DownloadFile("$RAW_BASE/upgrade.ps1", "$INSTALL_DIR\upgrade.ps1")
    $client.DownloadFile("$RAW_BASE/uninstall.ps1", "$INSTALL_DIR\uninstall.ps1")

    Write-Success "Config files downloaded to $INSTALL_DIR"
}

function Start-TpServices {
    Write-Step "Starting ThingsPanel services"
    Set-Location $INSTALL_DIR

    $imagesTar = Join-Path $INSTALL_DIR "images.tar"

    if (Test-Path $imagesTar) {
        Write-Info "Found local images.tar, loading (may take a few minutes)..."
        docker load -i $imagesTar
        if ($LASTEXITCODE -eq 0) {
            Write-Success "Local images loaded"
        } else {
            Write-Warn "Image load failed, will try online pull"
        }
    }

    if (-not (Test-Path $imagesTar) -or $LASTEXITCODE -ne 0) {
        Write-Info "Pulling images (first run may take 3-5 minutes)..."
        docker compose pull --quiet
        if ($LASTEXITCODE -ne 0) { Write-Err "Image pull failed" }
    }

    Write-Info "Starting services, waiting for health checks..."
    docker compose up -d --wait --timeout 180
    if ($LASTEXITCODE -ne 0) {
        Write-Err "Failed to start. View logs: docker compose -f `"$INSTALL_DIR\docker-compose.yml`" logs"
    }
    Write-Success "All services started"
}

function Test-Installation {
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
                return
            }
        } catch { }
        Start-Sleep -Seconds 2
        $waited += 2
        Write-Host -NoNewline "."
    }
    Write-Host ""
    Write-Warn "Web service not responding yet. Please visit http://localhost:$HttpPort shortly."
}

function New-DesktopShortcut {
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
}

Test-Docker
Test-Ports
Resolve-TpVersion
Initialize-Directories
Get-Configs
Start-TpServices
Test-Installation
New-DesktopShortcut

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
