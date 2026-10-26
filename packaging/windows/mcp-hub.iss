; MCP Hub 的 Windows 安装包（Inno Setup 6）。
;
; 两个关键决定：
;
; 1. **装到用户目录，不要管理员权限**（PrivilegesRequired=lowest）。
;    目标用户是公司电脑上的财务和运营，他们常常没有管理员权限 —— 一个弹
;    UAC 的安装包对他们等于装不上。装进 %LOCALAPPDATA%\Programs 不需要任何
;    权限，开始菜单快捷方式也落在用户自己的配置里。
;
; 2. **固定安装路径**。以后页面上那个「在 Claude 里启用」按钮要把这个 exe
;    的绝对路径写进客户端配置；装在固定位置，那条路径就不会因为用户把文件夹
;    挪走而失效。这是安装包相对绿色版的实际好处，不只是观感。
;
; 为什么不是 MSI：Inno Setup 自带中文界面和「立即运行」勾选框，而 MSI 要靠
; WiX 的 UI 扩展才有，作者工作量大一档。企业批量部署需要的静默安装这边也有
; （/SILENT、/VERYSILENT）。真到有客户 IT 要求用 GPO 推 MSI 时再补一个。

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif
#ifndef VersionInfo
  #define VersionInfo "0.0.0.0"
#endif
#ifndef SourceDir
  #define SourceDir "."
#endif

[Setup]
; AppId 一经发布不可更改：Windows 靠它认出「这是同一个程序的新版本」，
; 改了就会变成两份并存的安装。
AppId={{8F3A6C21-5B47-4E1D-9C2A-7D0E4F6B8A33}
AppName=MCP Hub
AppVersion={#AppVersion}
VersionInfoVersion={#VersionInfo}
AppPublisher=MCP Hub
AppMutex=MCPHubSetupMutex

; 装到用户目录：不需要管理员权限。
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
DefaultDirName={localappdata}\Programs\mcp-hub
DefaultGroupName=MCP Hub
DisableProgramGroupPage=yes

; 一个单文件程序没什么可选的，装到哪也不用问 —— 少一页就少一次犹豫。
DisableDirPage=yes
DisableReadyPage=no

OutputDir={#SourceDir}\out
OutputBaseFilename=mcp-hub-{#AppVersion}-windows-amd64-setup
Compression=lzma2/max
SolidCompression=yes
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
WizardStyle=modern
UninstallDisplayName=MCP Hub
; 图标：安装程序自己的、以及「添加或删除程序」里显示的那个。
; 装好之后开始菜单和桌面快捷方式的图标来自 exe 内嵌的资源（见 release.sh）。
SetupIconFile=mcp-hub.ico
UninstallDisplayIcon={app}\mcp-hub.exe

[Languages]
Name: "chinese"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"
Name: "english"; MessagesFile: "compiler:Default.isl"

[CustomMessages]
chinese.LaunchAfterInstall=安装完成后打开配置页面
chinese.DesktopIcon=在桌面创建快捷方式
english.LaunchAfterInstall=Open the settings page when done
english.DesktopIcon=Create a desktop shortcut

[Tasks]
Name: "desktopicon"; Description: "{cm:DesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Files]
Source: "{#SourceDir}\mcp-hub.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourceDir}\快速开始.md"; DestDir: "{app}"; Flags: ignoreversion isreadme

[Icons]
Name: "{group}\MCP Hub"; Filename: "{app}\mcp-hub.exe"
Name: "{group}\卸载 MCP Hub"; Filename: "{uninstallexe}"
Name: "{autodesktop}\MCP Hub"; Filename: "{app}\mcp-hub.exe"; Tasks: desktopicon

[Run]
; 双击主程序就会打开配置窗口（程序按 stdin 是不是管道判断自己是被用户双击
; 还是被 AI 客户端 spawn 的），所以这里不用带任何参数。
Filename: "{app}\mcp-hub.exe"; Description: "{cm:LaunchAfterInstall}"; \
  Flags: nowait postinstall skipifsilent
