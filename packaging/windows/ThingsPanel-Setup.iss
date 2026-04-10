; ThingsPanel Installer for Windows

#define AppName "ThingsPanel"
#define AppVersion GetEnv('TP_VERSION')
#if AppVersion == ""
  #define AppVersion "v1.1.14"
#endif
#define AppPublisher "ThingsPanel Community"
#define AppURL "https://thingspanel.io"
#define InstallDir "C:\\ThingsPanel"

[Setup]
AppId={{A3B2C1D4-E5F6-7890-ABCD-EF1234567890}
AppName={#AppName}
AppVersion={#AppVersion}
AppPublisher={#AppPublisher}
AppPublisherURL={#AppURL}
AppSupportURL={#AppURL}/docs
AppUpdatesURL=https://github.com/ThingsPanel/thingspanel-installer/releases
DefaultDirName={#InstallDir}
DefaultGroupName={#AppName}
AllowNoIcons=yes
PrivilegesRequired=admin
OutputDir=..\..\dist\windows
OutputBaseFilename=ThingsPanel-Setup-{#AppVersion}
WizardStyle=modern
WizardSmallImageFile=assets\sidebar.bmp
Compression=lzma2/ultra64
SolidCompression=yes
MinVersion=10.0.17763

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[CustomMessages]
english.DockerNotFound=Docker Desktop not found. Please install Docker Desktop first.%n%nDownload: https://www.docker.com/products/docker-desktop
english.DockerNotRunning=Docker Desktop is not running. Start it now?
english.InstallingServices=Starting ThingsPanel services. The first start may take several minutes.

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked

[Files]
Source: "..\..\docker-compose.yml"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\install.ps1"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\install.core.ps1"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\upgrade.ps1"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\uninstall.ps1"; DestDir: "{app}"; Flags: ignoreversion
Source: "assets\ThingsPanel.bat"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\ThingsPanel Console"; Filename: "{app}\ThingsPanel.bat"
Name: "{group}\Open Web UI"; Filename: "http://localhost:8080"
Name: "{commondesktop}\ThingsPanel"; Filename: "{app}\ThingsPanel.bat"; Tasks: desktopicon

[Run]
Filename: "powershell.exe"; Parameters: "-ExecutionPolicy Bypass -File ""{app}\install.core.ps1"" -Version ""{#AppVersion}"""; WorkingDir: "{app}"; StatusMsg: "{cm:InstallingServices}"; Flags: waituntilterminated
Filename: "http://localhost:8080"; Description: "Open ThingsPanel Web UI"; Flags: shellexec postinstall skipifsilent

[UninstallRun]
Filename: "powershell.exe"; Parameters: "-ExecutionPolicy Bypass -File ""{app}\uninstall.ps1"""; WorkingDir: "{app}"; RunOnceId: "StopServices"

[Code]
function InitializeSetup(): Boolean;
var
  ResultCode: Integer;
begin
  Result := True;

  if not FileExists(ExpandConstant('{pf}\Docker\Docker\Docker Desktop.exe')) and
     not FileExists(ExpandConstant('{pf64}\Docker\Docker\Docker Desktop.exe')) then
  begin
    MsgBox(CustomMessage('DockerNotFound'), mbError, MB_OK);
    Result := False;
    Exit;
  end;

  if Exec('powershell.exe', '-Command "docker info 2>$null; exit $LASTEXITCODE"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
  begin
    if ResultCode <> 0 then
    begin
      if MsgBox(CustomMessage('DockerNotRunning'), mbConfirmation, MB_YESNO) = IDYES then
      begin
        ShellExec('open', ExpandConstant('{pf64}\Docker\Docker\Docker Desktop.exe'), '', '', SW_SHOW, ewNoWait, ResultCode);
      end;
    end;
  end;
end;
