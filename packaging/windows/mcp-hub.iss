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
; 注意：程序里**没有**创建这个互斥量，所以这一行目前不起作用。别把它当成
; 「安装时会检测程序在跑」—— 真升级时撞上的是文件锁（见下面 InfoBeforeFile）。
; 要让它生效，得在 main.go 的 GUI 分支里建这个名字的 mutex，而且 stdio
; 子进程绝不能建：否则 Quick 跑着时 AppMutex 会直接拦住整个安装。
AppMutex=MCPHubSetupMutex

; 升级前必须先退出 Amazon Quick。
;
; Quick 以 stdio 方式 spawn 的那个 mcp-hub.exe 一直活着，Windows 锁住正在
; 运行的 exe 映像，所以 Inno 删不掉旧文件（DeleteFile failed; code 5），
; 对话框只给「重试 / 跳过 / 取消」。而默认开着的 CloseApplications 走
; Restart Manager，它要靠窗口消息优雅关闭进程 —— 那个子进程没有窗口，
; 所以必然关不掉；就算关掉了，Quick 还握着管道会立刻再 spawn 一个。
;
; 这里刻意只给一页说明，不做自动处理（改名避让、taskkill 都试过方案，
; 见 git 历史里的讨论）：一句话能说清的事不值得引入那套复杂度。
InfoBeforeFile=安装前须知.txt

; 许可与免责声明：这一页用户必须选「我同意」才能继续，这是免责条款真正
; 到达使用者的地方 —— 仓库里的 LICENSE 使用者永远不会看到。
;
; 内容是中文摘要 + 指向程序目录下的 LICENSE（Apache-2.0 全文是英文，
; 直接摆给财务和运营看等于没看）。
;
; 和 InfoBeforeFile 同理，.txt 必须带 UTF-8 BOM：Inno Setup 6.3 之前没有
; BOM 的 .txt 按系统 ANSI 代码页解读，中文会变乱码。
LicenseFile=许可与免责声明.txt

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
; 许可与归属声明必须随程序安装：Apache-2.0 第 4(a)(d) 条要求分发时附带许可证
; 副本和第三方归属声明（内含的 wazero 带 NOTICE）。装到程序目录而不是只放在
; 仓库里，使用者才真的拿到了它们。
Source: "{#SourceDir}\LICENSE"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourceDir}\NOTICE"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourceDir}\THIRD-PARTY-NOTICES.md"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\MCP Hub"; Filename: "{app}\mcp-hub.exe"
Name: "{group}\卸载 MCP Hub"; Filename: "{uninstallexe}"
Name: "{autodesktop}\MCP Hub"; Filename: "{app}\mcp-hub.exe"; Tasks: desktopicon

[Run]
; 双击主程序就会打开配置窗口（程序按 stdin 是不是管道判断自己是被用户双击
; 还是被 AI 客户端 spawn 的），所以这里不用带任何参数。
Filename: "{app}\mcp-hub.exe"; Description: "{cm:LaunchAfterInstall}"; \
  Flags: nowait postinstall skipifsilent
