#!/bin/bash
# 打开 MCP Hub 的本机配置页面（只监听 127.0.0.1）。
cd "$(dirname "$0")" || exit 1
echo "正在启动配置页面..."
echo "浏览器打开 http://127.0.0.1:7801"
echo "按 Ctrl-C 停止。"
echo
command -v xdg-open >/dev/null && (sleep 1; xdg-open http://127.0.0.1:7801) &
./mcp-hub -ui
