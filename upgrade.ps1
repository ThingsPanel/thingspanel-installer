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
    .\upgrade.ps1 -TargetVersion v1.2.12.1
#>

[CmdletBinding()]
param(
    [string]$TargetVersion = "",
    [string]$InstallDir    = "C:\ThingsPanel"
)

$ErrorActionPreference = "Stop"

$REPO     = "ThingsPanel/thingspanel-installer"
$RAW_BASE = "https://install.thingspanel.io"

function Write-Info    ($m) { Write-Host "[INFO]  $m" -ForegroundColor Cyan }
function Write-Success ($m) { Write-Host "[OK]    $m" -ForegroundColor Green }
function Write-Warn    ($m) { Write-Host "[WARN]  $m" -ForegroundColor Yellow }
function Write-Step    ($m) { Write-Host "`n[>>] $m" -ForegroundColor White -BackgroundColor DarkBlue }
function Write-Err     ($m) { Write-Host "[ERROR] $m" -ForegroundColor Red; throw $m }

function Test-ExistingThingsVisDatabase($Directory) {
    if (-not (Test-Path (Join-Path $Directory 'docker-compose.yml'))) { return $true }
    Push-Location $Directory
    try {
        $rendered = & docker compose config --format json 2>$null
        if ($LASTEXITCODE -ne 0) { $rendered = & docker compose config --no-interpolate --format json 2>$null }
        if ($LASTEXITCODE -ne 0) { return $false }
        $configuration = ($rendered -join "`n") | ConvertFrom-Json
        $variables = $configuration.services.'thingsvis-server'.environment
        if ($variables -is [array]) {
            $values = @($variables | Where-Object { $_ -is [string] -and $_.StartsWith('DATABASE_URL=') } | ForEach-Object { $_.Substring(13) })
            if ($values.Count -ne 1) { return $false }
            $value = $values[0]
        } else {
            $value = $variables.DATABASE_URL
        }
        if (-not ($value -is [string] -and $value -match '^(?i:postgres(?:ql)?):')) { return $false }
        $backendVariables = $configuration.services.backend.environment
        $backend = @{}
        if ($backendVariables -is [array]) {
            foreach ($item in $backendVariables) {
                if ($item -isnot [string] -or -not $item.Contains('=')) { return $false }
                $separator = $item.IndexOf('=')
                $key = $item.Substring(0, $separator)
                if ($backend.ContainsKey($key)) { return $false }
                $backend[$key] = $item.Substring($separator + 1)
            }
        } elseif ($null -ne $backendVariables) {
            foreach ($property in $backendVariables.PSObject.Properties) { $backend[$property.Name] = $property.Value }
        } else { return $false }
        foreach ($required in @('GOTP_DB_PSQL_HOST','GOTP_DB_PSQL_PORT','GOTP_DB_PSQL_USERNAME','GOTP_DB_PSQL_DBNAME')) {
            if (-not $backend.ContainsKey($required) -or [string]::IsNullOrWhiteSpace([string]$backend[$required])) { return $false }
        }
        if ($backend['GOTP_DB_PSQL_HOST'] -eq 'postgres' -and $null -eq $configuration.services.postgres) { return $false }
        $selectors = @('GOTP_DB_PSQL_DBTYPE','GOTP_DB_DRIVER','GOTP_DB_PROVIDER','GOTP_DB_TYPE','DB_DRIVER','DB_PROVIDER','DB_TYPE','DATABASE_TYPE','DATABASE_PROVIDER','DATABASE_DRIVER')
        foreach ($key in $backend.Keys) {
            $name = ([string]$key).ToUpperInvariant()
            $entry = $backend[$key]
            if (($name.StartsWith('GOTP_DB_MYSQL_') -or $name.StartsWith('GOTP_DB_MARIADB_')) -and $null -ne $entry -and [string]$entry -ne '') { return $false }
            if ($selectors -contains $name -and @('postgres','postgresql','pg') -notcontains ([string]$entry).ToLowerInvariant()) { return $false }
            if ($name -eq 'DATABASE_URL' -and ($entry -isnot [string] -or $entry -notmatch '^(?i:postgres(?:ql)?):')) { return $false }
        }
        return $true
    } catch { return $false } finally { Pop-Location }
}

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
if (-not (Test-ExistingThingsVisDatabase $InstallDir)) {
    Write-Err "Cannot confirm that the existing backend and ThingsVis use supported PostgreSQL configurations; existing configuration was not changed."
}

if (-not $TargetVersion) {
    try {
        $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$REPO/releases/latest" `
            -Headers @{ "User-Agent" = "ThingsPanel-Upgrader" } -TimeoutSec 10
        $TargetVersion = $rel.tag_name
    } catch {
        Write-Warn "Cannot fetch latest version. Upgrade cancelled."
        exit 1
    }
}
Write-Info "Target version: $TargetVersion"

Write-Step "Backing up config"
$backup = "$ComposeFile.bak.$(Get-Date -Format 'yyyyMMddHHmmss')"
Copy-Item $ComposeFile $backup
Write-Success "Backed up to: $backup"

Write-Step "Downloading new config"
$client = New-Object System.Net.WebClient
$client.Headers.Add("User-Agent", "ThingsPanel-Upgrader")
$client.DownloadFile("$RAW_BASE/docker-compose.yml", "$ComposeFile")
$client.DownloadFile("$RAW_BASE/upgrade.ps1", "$InstallDir\upgrade.ps1")
$client.DownloadFile("$RAW_BASE/uninstall.ps1", "$InstallDir\uninstall.ps1")
Write-Success "Config files updated"

Write-Step "Pulling new images"
docker compose pull --quiet
if ($LASTEXITCODE -ne 0) { Write-Err "Image pull failed" }
Write-Success "Images pulled"

Write-Step "Restarting services"
Set-Location $InstallDir
docker compose up -d --wait --timeout 180
if ($LASTEXITCODE -ne 0) {
    Write-Err "Failed to start. View logs: docker compose -f `"$ComposeFile`" logs"
}
Write-Success "Upgrade complete! Version: $TargetVersion"
