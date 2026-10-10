#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [string]$Version   = "",
    [int]$HttpPort      = 8080,
    [int]$MqttPort      = 1883
)

$ErrorActionPreference = "Stop"

# ── 强制 UTF-8 输出，防止中文乱码 ─────────────────────────────────────────────
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
if ($PSVersionTable.PSVersion.Major -lt 7) {
    powershell -Command "chcp 65001 > `$null" 2>$null
}

$REPO          = "ThingsPanel/thingspanel-installer"
$GITHUB_RAW_BASE = "https://raw.githubusercontent.com/$REPO"
$INSTALL_DIR   = "C:\ThingsPanel"
$MIN_DOCKER_VER = "20.10"
$MIN_COMPOSE_VER = [Version]"2.30"

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

$out = docker version --format '{{.Server.Version}}' 2>$null
if ([string]::IsNullOrEmpty($out)) {
    Write-Err "Docker engine not running. Please start Docker Desktop."
}
if ([Version]$out -lt [Version]$MIN_DOCKER_VER) {
    Write-Err "Docker version too old (current: $out, need >= $MIN_DOCKER_VER)"
}
Write-Success "Docker $out"

try { $composeOut = docker compose version --short 2>$null } catch {
    Write-Err "docker compose (v2) not found. Please upgrade Docker Desktop."
}
try {
    if ($composeOut -notmatch '(\d+\.\d+(?:\.\d+)?)') { throw "unrecognized version" }
    $composeVersion = [Version]$Matches[1]
} catch {
    Write-Err "Could not determine Docker Compose version."
}
if ($composeVersion -lt $MIN_COMPOSE_VER) {
    Write-Err "Docker Compose is too old (current: $composeVersion, need >= $MIN_COMPOSE_VER); no files were changed."
}
Write-Success "Docker Compose $composeVersion"

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
    $script:TpVersion = $env:TP_INSTALLER_REF
    if (-not $script:TpVersion -and $env:TP_INSTALLER_CHANNEL -eq "preview") {
        Write-Err "Preview installs require TP_INSTALLER_REF; stable assets will not be used."
    }
    if (-not $script:TpVersion) {
        try {
            $rel = Invoke-RestMethod `
                -Uri "https://api.github.com/repos/$REPO/releases/latest" `
                -Headers @{ "User-Agent" = "ThingsPanel-Installer" } `
                -TimeoutSec 10
            $script:TpVersion = $rel.tag_name
        } catch {
            Write-Err "Cannot resolve the stable installer release ref."
        }
    }
}
$channel = if ($env:TP_INSTALLER_CHANNEL) { $env:TP_INSTALLER_CHANNEL } else { "stable" }
if ($channel -notin @("stable", "preview")) { Write-Err "TP_INSTALLER_CHANNEL must be stable or preview." }
$ref = if ($env:TP_INSTALLER_REF) { $env:TP_INSTALLER_REF } else { $script:TpVersion }
if ($env:TP_INSTALLER_REF -and $Version -and $env:TP_INSTALLER_REF -ne $Version) {
    Write-Err "Version and TP_INSTALLER_REF must identify the same installer resource ref."
}
if ($ref -notmatch '^[A-Za-z0-9._/-]+$' -or $ref.StartsWith("-") -or $ref.Contains("..")) {
    Write-Err "Invalid installer ref."
}
$script:TpInstallerRef = $ref
$script:TpInstallerChannel = $channel
Write-Success "Installing version: $($script:TpVersion)"

# ── 创建目录 ──────────────────────────────────────────────────────────────────
Write-Step "Creating directory structure"
New-Item -ItemType Directory -Force -Path $INSTALL_DIR | Out-Null
Write-Success "Directory: $INSTALL_DIR"

function Get-InstallerAssetRoot {
    $localRoot = $env:TP_INSTALLER_LOCAL_ASSETS
    if (-not $localRoot) { $localRoot = $PSScriptRoot }
    if (-not $localRoot) { return @{ Local = $false; Root = ""; Base = "$GITHUB_RAW_BASE/$script:TpInstallerRef" } }
    if ((Test-Path (Join-Path $localRoot "docker-compose.yml")) -and
        (Test-Path (Join-Path $localRoot "scripts/notification-init.sh")) -and
        (Test-Path (Join-Path $localRoot "scripts/notification-init.ps1")) -and
        (Test-Path (Join-Path $localRoot "scripts/notification-db-init.sh")) -and
        (Test-Path (Join-Path $localRoot "scripts/notification-db-init.ps1")) -and
        (Test-Path (Join-Path $localRoot "scripts/notification-nginx.sh")) -and
        (Test-Path (Join-Path $localRoot "scripts/notification-nginx.ps1")) -and
        (Test-Path (Join-Path $localRoot "scripts/notification-migrate.sh")) -and
        (Test-Path (Join-Path $localRoot "scripts/notification-migrate.ps1")) -and
        (Test-Path (Join-Path $localRoot "config/notification-runtime.json.template")) -and
        (Test-Path (Join-Path $localRoot "config/notification-caddyfile")) -and
        (Test-Path (Join-Path $localRoot "config/notification-routes.conf")) -and
        (Test-Path (Join-Path $localRoot "config/thingmodel-runtime.json")) -and
        (Test-Path (Join-Path $localRoot "init-db/notification-create-db.sql")) -and
        (Test-Path (Join-Path $localRoot "init-db/00-timescale-extension.sh")) -and
        (Test-Path (Join-Path $localRoot "init-db/01-create-thingmodel-db.sh")) -and
        (Test-Path (Join-Path $localRoot "init-db/02-thingmodel-migrations.sh"))) {
        return @{ Local = $true; Root = $localRoot; Base = "" }
    }
    return @{ Local = $false; Root = ""; Base = "$GITHUB_RAW_BASE/$script:TpInstallerRef" }
}

function Install-Asset([string]$RelativePath, [string]$Destination) {
    $assetRoot = Get-InstallerAssetRoot
    New-Item -ItemType Directory -Force -Path (Split-Path $Destination) | Out-Null
    if ($assetRoot.Local) {
        $sourcePath = (Resolve-Path -LiteralPath (Join-Path $assetRoot.Root $RelativePath)).Path
        $destinationPath = [System.IO.Path]::GetFullPath($Destination)
        if (-not [string]::Equals($sourcePath, $destinationPath, [System.StringComparison]::OrdinalIgnoreCase)) {
            Copy-Item -LiteralPath $sourcePath -Destination $destinationPath -Force
        }
    } else {
        $client = New-Object System.Net.WebClient
        $client.Headers.Add("User-Agent", "ThingsPanel-Installer")
        $client.DownloadFile("$($assetRoot.Base)/$RelativePath", $Destination)
    }
}
$assetResolution = Get-InstallerAssetRoot
$assetSource = if ($assetResolution.Local) { "local:$($assetResolution.Root)" } else { $assetResolution.Base }

# ── 下载配置与通知资产 ────────────────────────────────────────────────────────
Write-Step "Downloading config files"
Install-Asset "docker-compose.yml" "$INSTALL_DIR\docker-compose.yml"
Install-Asset "upgrade.ps1" "$INSTALL_DIR\upgrade.ps1"
Install-Asset "uninstall.ps1" "$INSTALL_DIR\uninstall.ps1"
Install-Asset "scripts/notification-init.sh" "$INSTALL_DIR\scripts\notification-init.sh"
Install-Asset "scripts/notification-init.ps1" "$INSTALL_DIR\scripts\notification-init.ps1"
Install-Asset "scripts/notification-db-init.sh" "$INSTALL_DIR\scripts\notification-db-init.sh"
Install-Asset "scripts/notification-db-init.ps1" "$INSTALL_DIR\scripts\notification-db-init.ps1"
Install-Asset "scripts/notification-nginx.ps1" "$INSTALL_DIR\scripts\notification-nginx.ps1"
Install-Asset "scripts/notification-migrate.sh" "$INSTALL_DIR\scripts\notification-migrate.sh"
Install-Asset "scripts/notification-migrate.ps1" "$INSTALL_DIR\scripts\notification-migrate.ps1"
Install-Asset "config/notification-runtime.json.template" "$INSTALL_DIR\config\notification-runtime.json.template"
Install-Asset "config/notification-caddyfile" "$INSTALL_DIR\config\notification-caddyfile"
Install-Asset "config/notification-routes.conf" "$INSTALL_DIR\config\notification-routes.conf"
Install-Asset "config/thingmodel-runtime.json" "$INSTALL_DIR\config\thingmodel-runtime.json"
Install-Asset "init-db/notification-create-db.sql" "$INSTALL_DIR\init-db\notification-create-db.sql"
Install-Asset "init-db/00-timescale-extension.sh" "$INSTALL_DIR\init-db\00-timescale-extension.sh"
Install-Asset "init-db/01-create-thingmodel-db.sh" "$INSTALL_DIR\init-db\01-create-thingmodel-db.sh"
Install-Asset "init-db/02-thingmodel-migrations.sh" "$INSTALL_DIR\init-db\02-thingmodel-migrations.sh"
Write-Success "Config files downloaded to $INSTALL_DIR"

# ── 启动服务 ──────────────────────────────────────────────────────────────────
Write-Step "Starting ThingsPanel services"
Set-Location $INSTALL_DIR
if (-not $env:TP_NOTIFICATION_DATA_DIR) { $env:TP_NOTIFICATION_DATA_DIR = Join-Path $INSTALL_DIR "data\notification" }
if (-not $env:TP_INSTALLER_UID) { $env:TP_INSTALLER_UID = "0" }
if (-not $env:TP_INSTALLER_GID) { $env:TP_INSTALLER_GID = "0" }
if ($channel -eq "preview") { $env:COMPOSE_PATH_SEPARATOR = ':' }
if (Test-Path $env:TP_NOTIFICATION_DATA_DIR) {
    foreach ($relative in @("infra.json", "core.env", "db.env", "smtp.env", "migrate.env", "legacy-reader.env", "db.pgpass", "tls/ca.crt", "tls/server.crt", "tls/server.key", "tls/system-ca-bundle.pem")) {
        $privatePath = Join-Path $env:TP_NOTIFICATION_DATA_DIR $relative
        if ((Test-Path $privatePath) -and -not (Test-Path $privatePath -PathType Leaf)) { Write-Err "Notification secret path is not a file: $privatePath" }
        if (Test-Path $privatePath) {
            try { $stream = [System.IO.File]::OpenRead($privatePath); $stream.Dispose() } catch { Write-Err "Cannot read existing notification credential $privatePath; run as its owner or administrator. Permissions will not be widened." }
        }
    }
}

Write-Step "Initializing notification data and applying migrations"
$imageShardsDir = Join-Path $INSTALL_DIR "images.d"
$imageShards = @()
$imagesLoaded = $false
if (Test-Path $imageShardsDir -PathType Container) {
    $imageShards = @(Get-ChildItem -LiteralPath $imageShardsDir -Filter "*.tar.gz" -File | Sort-Object Name)
    if ($imageShards.Count -eq 0) { Write-Err "images.d exists but contains no .tar.gz image shards; refusing an online fallback." }
    foreach ($shard in $imageShards) {
        cmd /c "docker load -i `"$($shard.FullName)`" >NUL 2>&1"
        if ($LASTEXITCODE -ne 0) { Write-Err "Offline image shard failed to load: $($shard.Name); refusing an online fallback." }
    }
    $imagesLoaded = $true
} elseif (Test-Path (Join-Path $INSTALL_DIR "images.tar.gz")) {
    $imageArchive = Join-Path $INSTALL_DIR "images.tar.gz"
} elseif (Test-Path (Join-Path $INSTALL_DIR "images.tar")) {
    $imageArchive = Join-Path $INSTALL_DIR "images.tar"
} else {
    $parts = @(Get-ChildItem -Path (Join-Path $INSTALL_DIR "images.tar.part-*") -File -ErrorAction SilentlyContinue | Sort-Object Name)
    if ($parts.Count -gt 0) {
        $imageArchive = Join-Path $env:TEMP ("thingspanel-images-{0}.tar" -f [Guid]::NewGuid())
        $output = [System.IO.File]::Open($imageArchive, [System.IO.FileMode]::CreateNew, [System.IO.FileAccess]::Write)
        try {
            foreach ($part in $parts) {
                $input = [System.IO.File]::OpenRead($part.FullName)
                try { $input.CopyTo($output) } finally { $input.Dispose() }
            }
        } finally { $output.Dispose() }
        $removeImageArchive = $true
    } else {
        $imageArchive = $null
    }
}
if ($imagesLoaded) {
    Write-Success "Offline image shards loaded"
} elseif ($imageArchive) {
    Write-Info "Loading offline image archive (may take a few minutes)..."
    cmd /c "docker load -i `"$imageArchive`" >NUL 2>&1"
    $loadCode = $LASTEXITCODE
    if ($removeImageArchive) { Remove-Item -LiteralPath $imageArchive -Force }
    if ($loadCode -ne 0) { Write-Err "Offline image archive failed to load; refusing to pull fallback images." }
    Write-Success "Offline images loaded"
} else {
    Write-Info "Pulling images (first run may take 3-5 minutes)..."
    # 用 cmd /c 包裹，彻底绕过 PowerShell 对 native command 的错误处理
    # 进度文字全部走 cmd 的 stdout，不会触发 PowerShell 的 NativeCommandError
    cmd /c "docker compose pull >NUL 2>&1"
    if ($LASTEXITCODE -ne 0) { Write-Err "Image pull failed." }
}

cmd /c "docker compose run --rm --no-deps notification-setup >NUL 2>&1"
if ($LASTEXITCODE -ne 0) { Write-Err "Notification data initialization failed." }
cmd /c "docker compose up -d postgres >NUL 2>&1"
if ($LASTEXITCODE -ne 0) { Write-Err "Could not start the database for notification setup." }
$dbReady = $false
for ($i = 0; $i -lt 60; $i++) {
    cmd /c "docker compose exec -T postgres pg_isready -U postgres -d ThingsPanel >NUL 2>&1"
    if ($LASTEXITCODE -eq 0) { $dbReady = $true; break }
    Start-Sleep -Seconds 2
}
if (-not $dbReady) { Write-Err "Database did not become ready; notification migration was not run." }
& "$INSTALL_DIR\scripts\notification-db-init.ps1"
if ($LASTEXITCODE -ne 0) { Write-Err "Notification database initialization failed." }
& "$INSTALL_DIR\scripts\notification-migrate.ps1"
if ($LASTEXITCODE -ne 0) { Write-Err "Notification migrations failed; services were not started." }

Write-Info "Starting services, waiting for health checks..."
cmd /c "docker compose up -d --wait --timeout 180 >NUL 2>&1"
$exitCode = $LASTEXITCODE

$containers = docker compose ps -q 2>$null
if ($exitCode -ne 0 -or $LASTEXITCODE -ne 0 -or [string]::IsNullOrEmpty($containers)) {
    Write-Err "Failed to start. View logs: docker compose -f `"$INSTALL_DIR\docker-compose.yml`" logs"
}
& "$INSTALL_DIR\scripts\notification-nginx.ps1"
if ($LASTEXITCODE -ne 0) { Write-Err "Notification Nginx route setup failed; inspect the Nginx configuration before continuing." }
Write-Success "All services started"
@("channel=$channel", "ref=$ref", "source=$assetSource") | Set-Content -LiteralPath "$INSTALL_DIR\.installer-release" -Encoding ASCII

# ── 安装验证 ──────────────────────────────────────────────────────────────────
Write-Step "Verifying installation"
$url = "http://localhost:$HttpPort/health"
$maxWait = 60
$waited = 0
$ready = $false

Write-Info "Waiting for web service..."
while ($waited -lt $maxWait) {
    try {
        $resp = Invoke-WebRequest -Uri $url -TimeoutSec 3 -UseBasicParsing -ErrorAction Stop
        if ($resp.StatusCode -eq 200) {
            Write-Success "Web service ready: http://localhost:$HttpPort"
            $ready = $true
            break
        }
    } catch { }
    Start-Sleep -Seconds 2
    $waited += 2
    Write-Host -NoNewline "."
}
Write-Host ""
if (-not $ready) {
    Write-Warn "Web service not responding yet. Please visit http://localhost:$HttpPort shortly."
}

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
