#!/bin/bash
# 打开 MCP Hub 的本机配置页面（只监听 127.0.0.1）。
# 不要直接双击 mcp-hub：那启动的是给 AI 客户端用的服务。
cd "$(dirname "$0")" || exit 1
echo "正在启动配置页面..."
echo "浏览器打开 http://127.0.0.1:7801"
echo "按 Ctrl-C 或关掉这个窗口即停止。"
echo
(sleep 1; open http://127.0.0.1:7801) &
./mcp-hub -ui
