#Requires -RunAsAdministrator
<#
.SYNOPSIS
    ThingsPanel All-in-One — Windows 卸载脚本
.PARAMETER Purge
    同时删除所有数据（不可恢复）
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
function Write-Step    ($m) { Write-Host "`n▶ $m" -ForegroundColor White }

Write-Host "`nThingsPanel — 卸载程序" -ForegroundColor White

if ($Purge) {
    Write-Host "`n警告：--Purge 将删除所有数据，不可恢复！" -ForegroundColor Red
    $confirm = Read-Host "请输入 'YES' 确认"
    if ($confirm -ne "YES") { Write-Info "已取消"; exit 0 }
} else {
    $confirm = Read-Host "`n确认卸载 ThingsPanel？(y/N)"
    if ($confirm -notmatch '^[yY]$') { Write-Info "已取消"; exit 0 }
}

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
}

if ($Purge) {
    $envFile  = "$InstallDir\.env"
    $DataDir  = (Get-Content $envFile -ErrorAction SilentlyContinue |
                 Where-Object { $_ -match '^DATA_DIR=' }) -replace 'DATA_DIR=', ''
    if ($DataDir -and (Test-Path $DataDir)) {
        Write-Step "删除数据目录: $DataDir"
        Remove-Item $DataDir -Recurse -Force
    }
    Write-Step "删除安装目录: $InstallDir"
    Set-Location $env:USERPROFILE
    Remove-Item $InstallDir -Recurse -Force
    Write-Success "所有文件已删除"
} else {
    Write-Warn "数据目录已保留。如需完全清除数据请运行: .\uninstall.ps1 -Purge"
}

# 移除桌面快捷方式
$shortcut = "$env:USERPROFILE\Desktop\ThingsPanel.url"
if (Test-Path $shortcut) { Remove-Item $shortcut }

Write-Host "`n✓ ThingsPanel 已卸载" -ForegroundColor Green
