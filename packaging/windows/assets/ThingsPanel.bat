@echo off
title ThingsPanel 控制台
color 1F

echo.
echo  ████████╗██╗  ██╗██╗███╗   ██╗ ██████╗ ███████╗
echo     ██╔══╝██║  ██║██║████╗  ██║██╔════╝ ██╔════╝
echo     ██║   ███████║██║██╔██╗ ██║██║  ███╗███████╗
echo     ██║   ██╔══██║██║██║╚██╗██║██║   ██║╚════██║
echo     ██║   ██║  ██║██║██║ ╚████║╚██████╔╝███████║
echo     ╚═╝   ╚═╝  ╚═╝╚═╝╚═╝  ╚═══╝ ╚═════╝ ╚══════╝
echo.
echo  ThingsPanel All-in-One 控制台
echo  ─────────────────────────────────────────────────
echo.
echo  [1] 查看服务状态
echo  [2] 查看后端日志
echo  [3] 查看所有日志（最近 50 行）
echo  [4] 重启所有服务
echo  [5] 停止所有服务
echo  [6] 打开 Web 界面
echo  [7] 升级 ThingsPanel
echo  [8] 退出
echo.

set /p choice="请选择操作 [1-8]: "

if "%choice%"=="1" (
    cd /d "%~dp0"
    docker compose ps
    pause
    goto :eof
)
if "%choice%"=="2" (
    cd /d "%~dp0"
    docker compose logs -f backend
    pause
    goto :eof
)
if "%choice%"=="3" (
    cd /d "%~dp0"
    docker compose logs --tail=50
    pause
    goto :eof
)
if "%choice%"=="4" (
    cd /d "%~dp0"
    docker compose restart
    pause
    goto :eof
)
if "%choice%"=="5" (
    cd /d "%~dp0"
    docker compose down
    pause
    goto :eof
)
if "%choice%"=="6" (
    start http://localhost:8080
    goto :eof
)
if "%choice%"=="7" (
    powershell -ExecutionPolicy Bypass -File "%~dp0upgrade.ps1"
    pause
    goto :eof
)
if "%choice%"=="8" (
    exit
)

echo 无效选项
pause
