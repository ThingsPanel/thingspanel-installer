@echo off
title ThingsPanel Console
color 1F

echo.
echo  ThingsPanel Installer Console
echo  =============================
echo.
echo  [1] Status
echo  [2] Backend logs
echo  [3] Restart
echo  [4] Stop
echo  [5] Open Web UI
echo  [6] Upgrade
echo  [7] Exit
echo.

set /p choice="Select [1-7]: "

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
  docker compose restart
  pause
  goto :eof
)
if "%choice%"=="4" (
  cd /d "%~dp0"
  docker compose down
  pause
  goto :eof
)
if "%choice%"=="5" (
  start http://localhost:8080
  goto :eof
)
if "%choice%"=="6" (
  powershell -ExecutionPolicy Bypass -File "%~dp0upgrade.ps1"
  pause
  goto :eof
)
if "%choice%"=="7" exit

echo Invalid option
pause
