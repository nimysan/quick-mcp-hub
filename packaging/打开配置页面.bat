@echo off
chcp 65001 >nul
rem 打开 MCP Hub 的本机配置页面（只监听 127.0.0.1）。
rem 不要直接双击 mcp-hub.exe：那启动的是给 AI 客户端用的服务，窗口会一闪就关。
cd /d "%~dp0"
echo 正在启动配置页面...
echo 浏览器打开 http://127.0.0.1:7801
echo 关掉这个窗口即停止。
echo.
start "" http://127.0.0.1:7801
mcp-hub.exe -ui
echo.
echo 配置页面已退出。
pause
