#Requires -RunAsAdministrator
<#
.SYNOPSIS
    ThingsPanel All-in-One — Windows 卸载脚本

.DESCRIPTION
    停止容器、移除镜像，保留数据目录（可用 -Purge 同时删除数据）。

.PARAMETER Purge
    同时删除所有数据（不可恢复）

.EXAMPLE
    .\uninstall.ps1
    .\uninstall.ps1 -Purge
#>

[CmdletBinding()]
param(
    [switch]$Purge,
    [string]$InstallDir = "C:\ThingsPanel"
)

$ErrorActionPreference = "Stop"

function Write-Info    ($m) { Write-Host "[INFO]  $m" -ForegroundColor Cyan }
function Write-Success ($m) { Write-Host "[OK]    $m" -ForegroundColor Green }
function Write-Warn    ($m) { Write-Host "[WARN]  $m" -ForegroundColor Yellow }
function Write-Step    ($m) { Write-Host "`n▶ $m" -ForegroundColor White -BackgroundColor DarkBlue }

Write-Host ""
Write-Host "  ████████╗██╗  ██╗██╗███╗   ██╗ ██████╗ ███████╗" -ForegroundColor Cyan
Write-Host "     ██╔══╝██║  ██║██║████╗  ██║██╔════╝ ██╔════╝" -ForegroundColor Cyan
Write-Host "     ██║   ███████║██║██╔██╗ ██║██║  ███╗███████╗" -ForegroundColor Cyan
Write-Host "     ██║   ██╔══██║██║██║╚██╗██║██║   ██║╚════██║" -ForegroundColor Cyan
Write-Host "     ██║   ██║  ██║██║██║ ╚████║╚██████╔╝███████║" -ForegroundColor Cyan
Write-Host "     ╚═╝   ╚═╝  ╚═╝╚═╝╚═╝  ╚═══╝ ╚═════╝ ╚══════╝" -ForegroundColor Cyan
Write-Host ""
Write-Host "              PANEL  All-in-One  Uninstaller  (Windows)" -ForegroundColor White
Write-Host ""

if ($Purge) {
    Write-Host ""
    Write-Host "警告：-Purge 将删除所有数据，此操作不可恢复！" -ForegroundColor Red
    $confirm = Read-Host "请输入 'YES' 确认删除所有数据"
    if ($confirm -ne "YES") { Write-Info "已取消"; exit 0 }
} else {
    $confirm = Read-Host "`n确认卸载 ThingsPanel？(y/N)"
    if ($confirm -notmatch '^[yY]$') { Write-Info "已取消"; exit 0 }
}

if (-not (Get-Command "docker" -ErrorAction SilentlyContinue)) {
    Write-Warn "Docker 未运行，尝试直接删除目录..."
} else {
    Write-Step "停止并移除容器"
    Set-Location $InstallDir
    docker compose down --remove-orphans 2>$null
    Write-Success "容器已停止并移除"

    Write-Step "移除镜像"
    $images = docker images --format '{{.Repository}}:{{.Tag}}' 2>$null |
        Where-Object { $_ -match 'thingspanel|thingsvis|timescaledb' }
    if ($images) {
        $images | ForEach-Object { docker rmi --force $_ 2>$null }
        Write-Success "镜像已移除"
    } else {
        Write-Info "未找到 ThingsPanel 相关镜像"
    }
}

if ($Purge) {
    Write-Step "删除安装目录"
    Set-Location $env:USERPROFILE
    if (Test-Path $InstallDir) {
        Remove-Item $InstallDir -Recurse -Force
        Write-Success "安装目录已删除: $InstallDir"
    }
    Write-Success "所有数据已清除"
} else {
    Write-Warn "数据目录已保留。如需完全清除请运行: .\uninstall.ps1 -Purge"
}

$shortcut = "$env:USERPROFILE\Desktop\ThingsPanel.url"
if (Test-Path $shortcut) { Remove-Item $shortcut -ErrorAction SilentlyContinue }

Write-Host ""
Write-Host "  ThingsPanel 已卸载" -ForegroundColor Green
Write-Host ""