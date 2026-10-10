#Requires -RunAsAdministrator
<#
.SYNOPSIS
    ThingsPanel All-in-One — Windows Upgrade Script

.DESCRIPTION
    Backup config, pull new images, and restart services.

.PARAMETER TargetVersion
    Target version (default: latest from GitHub)

.PARAMETER InstallDir
    Install directory (default: C:\ThingsPanel)

.EXAMPLE
    .\upgrade.ps1
    .\upgrade.ps1 -TargetVersion v1.2.2
#>

[CmdletBinding()]
param(
    [string]$TargetVersion = "",
    [string]$InstallDir    = "C:\ThingsPanel"
)

$ErrorActionPreference = "Stop"
$MinComposeVersion = [Version]"2.30"

$REPO     = "ThingsPanel/thingspanel-installer"
$GITHUB_RAW_BASE = "https://raw.githubusercontent.com/$REPO"

function Write-Info    ($m) { Write-Host "[INFO]  $m" -ForegroundColor Cyan }
function Write-Success ($m) { Write-Host "[OK]    $m" -ForegroundColor Green }
function Write-Warn    ($m) { Write-Host "[WARN]  $m" -ForegroundColor Yellow }
function Write-Step    ($m) { Write-Host "`n[>>] $m" -ForegroundColor White -BackgroundColor DarkBlue }
function Write-Err     ($m) { Write-Host "[ERROR] $m" -ForegroundColor Red; throw $m }

Write-Host ""
Write-Host "  ThingsPanel All-in-One Upgrader (Windows)" -ForegroundColor White
Write-Host ""

$ComposeFile = "$InstallDir\docker-compose.yml"
if (-not (Test-Path $ComposeFile)) {
    Write-Err "Cannot find $ComposeFile. Please run the installer first."
}

if (-not (Get-Command "docker" -ErrorAction SilentlyContinue)) {
    Write-Err "Docker not found. Please start Docker Desktop and try again."
}
Set-Location $InstallDir
$composeOut = docker compose version --short 2>$null
try {
    if ($composeOut -notmatch '(\d+\.\d+(?:\.\d+)?)') { throw "unrecognized version" }
    $composeVersion = [Version]$Matches[1]
} catch { Write-Err "Could not determine Docker Compose version; upgrade stopped before modifying files." }
if ($composeVersion -lt $MinComposeVersion) { Write-Err "Docker Compose $composeVersion is too old; need >= $MinComposeVersion. Upgrade stopped before modifying files." }

if ($TargetVersion -and $env:TP_INSTALLER_REF -and $TargetVersion -ne $env:TP_INSTALLER_REF) {
    Write-Err "TargetVersion and TP_INSTALLER_REF differ; choose one exact installer ref."
}
$marker = Join-Path $InstallDir ".installer-release"
$savedChannel = ""
$savedRef = ""
if (Test-Path $marker) {
    foreach ($line in Get-Content -LiteralPath $marker) {
        if ($line -like "channel=*") { $savedChannel = $line.Substring(8) }
        if ($line -like "ref=*") { $savedRef = $line.Substring(4) }
    }
}
$channel = if ($env:TP_INSTALLER_CHANNEL) { $env:TP_INSTALLER_CHANNEL } elseif ($savedChannel) { $savedChannel } else { "stable" }
if ($channel -notin @("stable", "preview")) { Write-Err "TP_INSTALLER_CHANNEL must be stable or preview." }
$ref = if ($env:TP_INSTALLER_REF) { $env:TP_INSTALLER_REF } elseif ($TargetVersion) { $TargetVersion } else { "" }
if (-not $ref -and $channel -eq "preview") { $ref = $savedRef }
if (-not $ref -and $channel -eq "preview") {
    Write-Err "Preview upgrades require the saved ref, TP_INSTALLER_REF, or an explicit TargetVersion."
}
if (-not $ref) {
    try {
        $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$REPO/releases/latest" `
            -Headers @{ "User-Agent" = "ThingsPanel-Upgrader" } -TimeoutSec 10
        $ref = $rel.tag_name
    } catch {
        Write-Warn "Cannot fetch latest version. Upgrade cancelled."
        exit 1
    }
}
if ($ref -notmatch '^[A-Za-z0-9._/-]+$' -or $ref.StartsWith("-") -or $ref.Contains("..")) { Write-Err "Invalid installer ref." }
if (-not $TargetVersion) { $TargetVersion = $ref }
$env:TP_INSTALLER_CHANNEL = $channel
$env:TP_INSTALLER_REF = $ref
Write-Info "Target version: $TargetVersion"

$assetRoot = $env:TP_INSTALLER_LOCAL_ASSETS
if (-not $assetRoot -and $PSScriptRoot -and (Test-Path (Join-Path $PSScriptRoot ".git"))) { $assetRoot = $PSScriptRoot }
if (-not $assetRoot -and $savedRef -and $savedRef -eq $ref -and $PSScriptRoot) { $assetRoot = $PSScriptRoot }
$localAssets = $false
if ($assetRoot) {
    $localAssets = (Test-Path (Join-Path $assetRoot "docker-compose.yml")) -and
        (Test-Path (Join-Path $assetRoot "scripts/notification-init.sh")) -and
        (Test-Path (Join-Path $assetRoot "scripts/notification-init.ps1")) -and
        (Test-Path (Join-Path $assetRoot "scripts/notification-db-init.sh")) -and
        (Test-Path (Join-Path $assetRoot "scripts/notification-db-init.ps1")) -and
        (Test-Path (Join-Path $assetRoot "scripts/notification-nginx.sh")) -and
        (Test-Path (Join-Path $assetRoot "scripts/notification-nginx.ps1")) -and
        (Test-Path (Join-Path $assetRoot "scripts/notification-migrate.sh")) -and
        (Test-Path (Join-Path $assetRoot "scripts/notification-migrate.ps1")) -and
        (Test-Path (Join-Path $assetRoot "config/notification-runtime.json.template")) -and
        (Test-Path (Join-Path $assetRoot "config/notification-caddyfile")) -and
        (Test-Path (Join-Path $assetRoot "config/notification-routes.conf")) -and
        (Test-Path (Join-Path $assetRoot "config/thingmodel-runtime.json")) -and
        (Test-Path (Join-Path $assetRoot "init-db/notification-create-db.sql")) -and
        (Test-Path (Join-Path $assetRoot "init-db/00-timescale-extension.sh")) -and
        (Test-Path (Join-Path $assetRoot "init-db/01-create-thingmodel-db.sh")) -and
        (Test-Path (Join-Path $assetRoot "init-db/02-thingmodel-migrations.sh"))
}
function Install-Asset([string]$RelativePath, [string]$Destination) {
    New-Item -ItemType Directory -Force -Path (Split-Path $Destination) | Out-Null
    if ($localAssets) {
        $sourcePath = (Resolve-Path -LiteralPath (Join-Path $assetRoot $RelativePath)).Path
        $destinationPath = [System.IO.Path]::GetFullPath($Destination)
        if (-not [string]::Equals($sourcePath, $destinationPath, [System.StringComparison]::OrdinalIgnoreCase)) {
            Copy-Item -LiteralPath $sourcePath -Destination $destinationPath -Force
        }
    } else {
        $client = New-Object System.Net.WebClient
        $client.Headers.Add("User-Agent", "ThingsPanel-Upgrader")
        $client.DownloadFile("$GITHUB_RAW_BASE/$ref/$RelativePath", $Destination)
    }
}
$assetSource = if ($localAssets) { "local:$assetRoot" } else { "$GITHUB_RAW_BASE/$ref" }

Write-Step "Backing up config"
$backup = "$ComposeFile.bak.$(Get-Date -Format 'yyyyMMddHHmmss')"
Copy-Item $ComposeFile $backup
Write-Success "Backed up to: $backup"

Write-Step "Downloading new config"
Install-Asset "docker-compose.yml" $ComposeFile
Install-Asset "upgrade.ps1" "$InstallDir\upgrade.ps1"
Install-Asset "uninstall.ps1" "$InstallDir\uninstall.ps1"
Install-Asset "scripts/notification-init.sh" "$InstallDir\scripts\notification-init.sh"
Install-Asset "scripts/notification-init.ps1" "$InstallDir\scripts\notification-init.ps1"
Install-Asset "scripts/notification-db-init.sh" "$InstallDir\scripts\notification-db-init.sh"
Install-Asset "scripts/notification-db-init.ps1" "$InstallDir\scripts\notification-db-init.ps1"
Install-Asset "scripts/notification-nginx.ps1" "$InstallDir\scripts\notification-nginx.ps1"
Install-Asset "scripts/notification-migrate.sh" "$InstallDir\scripts\notification-migrate.sh"
Install-Asset "scripts/notification-migrate.ps1" "$InstallDir\scripts\notification-migrate.ps1"
Install-Asset "config/notification-runtime.json.template" "$InstallDir\config\notification-runtime.json.template"
Install-Asset "config/notification-caddyfile" "$InstallDir\config\notification-caddyfile"
Install-Asset "config/notification-routes.conf" "$InstallDir\config\notification-routes.conf"
Install-Asset "config/thingmodel-runtime.json" "$InstallDir\config\thingmodel-runtime.json"
Install-Asset "init-db/notification-create-db.sql" "$InstallDir\init-db\notification-create-db.sql"
Install-Asset "init-db/00-timescale-extension.sh" "$InstallDir\init-db\00-timescale-extension.sh"
Install-Asset "init-db/01-create-thingmodel-db.sh" "$InstallDir\init-db\01-create-thingmodel-db.sh"
Install-Asset "init-db/02-thingmodel-migrations.sh" "$InstallDir\init-db\02-thingmodel-migrations.sh"
Write-Success "Config files updated"

if (-not $env:TP_NOTIFICATION_DATA_DIR) { $env:TP_NOTIFICATION_DATA_DIR = Join-Path $InstallDir "data\notification" }
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

Write-Step "Pulling or loading service images"
$imageArchive = $null
$removeImageArchive = $false
$imageShardsDir = Join-Path $InstallDir "images.d"
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
} elseif (Test-Path (Join-Path $InstallDir "images.tar.gz")) {
    $imageArchive = Join-Path $InstallDir "images.tar.gz"
} elseif (Test-Path (Join-Path $InstallDir "images.tar")) {
    $imageArchive = Join-Path $InstallDir "images.tar"
} else {
    $parts = @(Get-ChildItem -Path (Join-Path $InstallDir "images.tar.part-*") -File -ErrorAction SilentlyContinue | Sort-Object Name)
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
    }
}
if ($imagesLoaded) {
    Write-Success "Offline image shards loaded"
} elseif ($imageArchive) {
    cmd /c "docker load -i `"$imageArchive`" >NUL 2>&1"
    $loadCode = $LASTEXITCODE
    if ($removeImageArchive) { Remove-Item -LiteralPath $imageArchive -Force }
    if ($loadCode -ne 0) { Write-Err "Offline image archive failed to load; refusing to pull fallback images." }
} else {
    cmd /c "docker compose pull --quiet >NUL 2>&1"
    if ($LASTEXITCODE -ne 0) { Write-Err "Image pull failed" }
}
Write-Success "Service images are ready"

Write-Step "Initializing notification data and applying migrations"
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
& "$InstallDir\scripts\notification-db-init.ps1"
if ($LASTEXITCODE -ne 0) { Write-Err "Notification database initialization failed." }
& "$InstallDir\scripts\notification-migrate.ps1"
if ($LASTEXITCODE -ne 0) { Write-Err "Notification migrations failed; services were not upgraded." }

Write-Step "Restarting services"
docker compose up -d --wait --timeout 180
if ($LASTEXITCODE -ne 0) {
    Write-Err "Failed to start. View logs: docker compose -f `"$ComposeFile`" logs"
}
& "$InstallDir\scripts\notification-nginx.ps1"
if ($LASTEXITCODE -ne 0) { Write-Err "Notification Nginx route setup failed; inspect the Nginx configuration before continuing." }
Write-Success "Upgrade complete! Version: $TargetVersion"
@("channel=$channel", "ref=$ref", "source=$assetSource") | Set-Content -LiteralPath $marker -Encoding ASCII
