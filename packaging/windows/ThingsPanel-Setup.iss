; ─────────────────────────────────────────────────────────────────────────────
; ThingsPanel All-in-One — Windows Installer (Inno Setup 6.x)
;
; 构建方法:
;   1. 安装 Inno Setup 6: https://jrsoftware.org/isinfo.php
;   2. iscc.exe packaging\windows\ThingsPanel-Setup.iss
; ─────────────────────────────────────────────────────────────────────────────

#define AppName      "ThingsPanel"
#define AppVersion   GetEnv('TP_VERSION')
#if AppVersion == ""
  #define AppVersion "v1.1.13.6"
#endif
#define AppPublisher "ThingsPanel Community"
#define AppURL       "https://thingspanel.io"
#define AppExeName   "ThingsPanel.exe"
#define InstallDir   "{autopf}\ThingsPanel"

[Setup]
AppId={{A3B2C1D4-E5F6-7890-ABCD-EF1234567890}
AppName={#AppName}
AppVersion={#AppVersion}
AppPublisher={#AppPublisher}
AppPublisherURL={#AppURL}
AppSupportURL={#AppURL}/docs
AppUpdatesURL=https://github.com/ThingsPanel/all-in-one-assembler/releases
DefaultDirName={autopf}\ThingsPanel
DefaultGroupName={#AppName}
AllowNoIcons=yes
; 需要管理员权限（Docker 命令需要）
PrivilegesRequired=admin
; 输出设置
OutputDir=..\..\dist\windows
OutputBaseFilename=ThingsPanel-Setup-{#AppVersion}
; 界面
WizardStyle=modern
WizardSmallImageFile=assets\sidebar.bmp
SetupIconFile=assets\thingspanel.ico
UninstallDisplayIcon={app}\thingspanel.ico
; 压缩
Compression=lzma2/ultra64
SolidCompression=yes
; 最低 Windows 版本：Windows 10
MinVersion=10.0.17763

[Languages]
Name: "chinesesimplified"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"
Name: "english"; MessagesFile: "compiler:Default.isl"

[CustomMessages]
chinesesimplified.CheckingDocker=正在检查 Docker Desktop...
chinesesimplified.DockerNotFound=未找到 Docker Desktop。请先安装 Docker Desktop 后重新运行安装程序。%n%n下载地址：https://www.docker.com/products/docker-desktop
chinesesimplified.DockerNotRunning=Docker Desktop 未运行。是否立即启动？
chinesesimplified.InstallingServices=正在启动 ThingsPanel 服务（首次启动需要下载镜像，约 3-5 分钟）...
chinesesimplified.InstallComplete=ThingsPanel 安装完成！%n%n访问地址：http://localhost:8080
english.CheckingDocker=Checking Docker Desktop...
english.DockerNotFound=Docker Desktop not found. Please install Docker Desktop first.%n%nDownload: https://www.docker.com/products/docker-desktop
english.DockerNotRunning=Docker Desktop is not running. Start it now?
english.InstallingServices=Starting ThingsPanel services (first start requires downloading images, ~3-5 minutes)...
english.InstallComplete=ThingsPanel installed successfully!%n%nAccess at: http://localhost:8080

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked
Name: "startmenuicon"; Description: "创建开始菜单快捷方式"; GroupDescription: "{cm:AdditionalIcons}"; Flags: checkedonce

[Files]
; 核心脚本
Source: "..\..\docker-compose.yml";  DestDir: "{app}";           Flags: ignoreversion
Source: "..\..\install.ps1";         DestDir: "{app}";           Flags: ignoreversion
Source: "..\..\upgrade.ps1";         DestDir: "{app}";           Flags: ignoreversion
Source: "..\..\uninstall.ps1";       DestDir: "{app}";           Flags: ignoreversion
Source: "..\..\nginx\nginx.conf";    DestDir: "{app}\nginx";     Flags: ignoreversion
Source: "assets\thingspanel.ico";    DestDir: "{app}";           Flags: ignoreversion
; 启动脚本（批处理包装）
Source: "assets\ThingsPanel.bat";    DestDir: "{app}";           Flags: ignoreversion

[Icons]
Name: "{group}\ThingsPanel 控制台";    Filename: "{app}\ThingsPanel.bat"; IconFilename: "{app}\thingspanel.ico"
Name: "{group}\ThingsPanel 卸载";      Filename: "{uninstallexe}"
Name: "{group}\打开 Web 界面";         Filename: "http://localhost:8080"
Name: "{commondesktop}\ThingsPanel";   Filename: "{app}\ThingsPanel.bat"; IconFilename: "{app}\thingspanel.ico"; Tasks: desktopicon

[Run]
; 安装后执行 PowerShell 安装脚本
Filename: "powershell.exe"; \
    Parameters: "-ExecutionPolicy Bypass -File ""{app}\install.ps1"" -InstallDir ""{app}"""; \
    WorkingDir: "{app}"; \
    StatusMsg: "{cm:InstallingServices}"; \
    Flags: runasoriginaluser waituntilterminated

; 安装完成后在浏览器打开
Filename: "http://localhost:8080"; \
    Description: "打开 ThingsPanel Web 界面"; \
    Flags: shellexec postinstall skipifsilent

[UninstallRun]
Filename: "powershell.exe"; \
    Parameters: "-ExecutionPolicy Bypass -File ""{app}\uninstall.ps1"""; \
    WorkingDir: "{app}"; \
    RunOnceId: "StopServices"

[Code]
// ── 安装前检查 Docker ────────────────────────────────────────────────────────
function InitializeSetup(): Boolean;
var
  ResultCode: Integer;
begin
  Result := True;

  // 检查 Docker 是否安装
  if not FileExists(ExpandConstant('{pf}\Docker\Docker\Docker Desktop.exe')) and
     not FileExists(ExpandConstant('{pf64}\Docker\Docker\Docker Desktop.exe')) then
  begin
    if MsgBox(CustomMessage('DockerNotFound'), mbError, MB_OK) = IDOK then
      ShellExec('open', 'https://www.docker.com/products/docker-desktop', '', '', SW_SHOW, ewNoWait, ResultCode);
    Result := False;
    Exit;
  end;

  // 检查 Docker 是否运行
  if Exec('powershell.exe', '-Command "docker info 2>$null; exit $LASTEXITCODE"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
  begin
    if ResultCode <> 0 then
    begin
      if MsgBox(CustomMessage('DockerNotRunning'), mbConfirmation, MB_YESNO) = IDYES then
      begin
        ShellExec('open', ExpandConstant('{pf64}\Docker\Docker\Docker Desktop.exe'), '', '', SW_SHOW, ewNoWait, ResultCode);
        MsgBox('Docker Desktop 正在启动，请等待其完全加载后点击继续。', mbInformation, MB_OK);
      end;
    end;
  end;
end;
