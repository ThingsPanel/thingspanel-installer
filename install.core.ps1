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
$RAW_BASE      = "https://install.thingspanel.io"
$INSTALL_DIR   = "C:\ThingsPanel"
$MIN_DOCKER_VER = "20.10"
$UPDATE_ENGINE_SHA256 = "f9a411d3abe743d05ed16808947e41540bef3e062f1a9b058815a4a0dfa179e2"

function Write-Info    ($m) { Write-Host "[INFO]  $m" -ForegroundColor Cyan }
function Write-Success ($m) { Write-Host "[OK]    $m" -ForegroundColor Green }
function Write-Warn    ($m) { Write-Host "[WARN]  $m" -ForegroundColor Yellow }
function Write-Step    ($m) { Write-Host "`n[>>] $m" -ForegroundColor White -BackgroundColor DarkBlue }
function Write-Err     ($m) { Write-Host "[ERROR] $m" -ForegroundColor Red; exit 1 }

function Invoke-SafeUpdate($Directory, $ExpectedVersion) {
    $python = Get-Command python3 -ErrorAction SilentlyContinue
    $pythonArgs = @()
    if (-not $python) { $python = Get-Command python -ErrorAction SilentlyContinue }
    if (-not $python) { $python = Get-Command py -ErrorAction SilentlyContinue; $pythonArgs = @('-3') }
    if (-not $python) { throw 'Python 3 is required; existing configuration was not changed.' }
    & $python.Source @pythonArgs -c 'import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)'
    if ($LASTEXITCODE -ne 0) { throw 'Python 3.8 or newer is required.' }
    $engine = Join-Path $Directory 'config/native/update.py'
    if ($PSScriptRoot -and (Test-Path (Join-Path $PSScriptRoot 'config/native/update.py'))) {
        $engine = Join-Path $PSScriptRoot 'config/native/update.py'
    }
    $temporary = $null
    try {
        if (-not (Test-Path -LiteralPath $engine) -or (Get-FileHash -LiteralPath $engine -Algorithm SHA256).Hash.ToLowerInvariant() -ne $UPDATE_ENGINE_SHA256) {
            $temporary = Join-Path ([System.IO.Path]::GetTempPath()) ('thingspanel-updater-' + [guid]::NewGuid().ToString('N'))
            New-Item -ItemType Directory -Path $temporary | Out-Null
            $engine = Join-Path $temporary 'update.py'
            $client = New-Object System.Net.WebClient
            try { $client.DownloadFile("$RAW_BASE/config/native/update.py", $engine) } finally { $client.Dispose() }
            if ((Get-FileHash -LiteralPath $engine -Algorithm SHA256).Hash.ToLowerInvariant() -ne $UPDATE_ENGINE_SHA256) {
                throw 'Updater digest mismatch; existing configuration was not changed.'
            }
        }
        $arguments = $pythonArgs + @($engine, '--root', $Directory, '--base-url', $RAW_BASE)
        if ($ExpectedVersion) { $arguments += @('--expected-version', $ExpectedVersion) }
        & $python.Source @arguments
        if ($LASTEXITCODE -ne 0) { throw 'Installation did not complete. Preserve .thingspanel-update for recovery.' }
    } finally {
        if ($temporary) { Remove-Item -LiteralPath $temporary -Recurse -Force }
    }
}

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

try { docker compose version 2>$null } catch {
    Write-Err "docker compose (v2) not found. Please upgrade Docker Desktop."
}
Write-Success "Docker Compose v2 available"
if (-not (Test-ExistingThingsVisDatabase $INSTALL_DIR)) {
    Write-Err "Cannot confirm that the existing backend and ThingsVis use supported PostgreSQL configurations; existing configuration was not changed."
}

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
    # Component images are selected by the published Compose, not by an
    # unrelated installer release tag or a hard-coded historical fallback.
    $script:TpVersion = ""
    Write-Info "Using the component versions in the published Compose configuration"
}

# ── 创建目录 ──────────────────────────────────────────────────────────────────
Write-Step "Creating directory structure"
New-Item -ItemType Directory -Force -Path $INSTALL_DIR | Out-Null
Write-Success "Directory: $INSTALL_DIR"

# ── 同一配置事务、同一进程锁内调用既有 native 启动流程 ───────────────────────
Write-Step "Staging and validating the complete installer payload"
Invoke-SafeUpdate $INSTALL_DIR $script:TpVersion
Write-Success "All services started"

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
