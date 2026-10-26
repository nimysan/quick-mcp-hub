#!/usr/bin/env bash
# 构建 wasm 插件模块。
#
# 这些产物不入库（.gitignore 的 dist/）：它们是 3MB 级的二进制，而且
# 可重现 —— 真正该入库的是源码和那份 manifest。
#
# 用法：plugins/build.sh [插件名...]        不给参数就全部构建
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p plugins/dist

plugins=("$@")
if [ ${#plugins[@]} -eq 0 ]; then
  plugins=()
  for d in plugins/*/; do
    name=$(basename "$d")
    [ "$name" = "dist" ] && continue
    [ -f "$d/main.go" ] && plugins+=("$name")
  done
fi

for name in "${plugins[@]}"; do
  out="plugins/dist/$name/$name.wasm"
  mkdir -p "plugins/dist/$name"
  # -buildmode=c-shared 才能用 //go:wasmexport（模块入口变成 _initialize）。
  GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o "$out" "./plugins/$name"
  # manifest 和模块放在一起 —— 那是真实的分发形态，也是 -plugin-install
  # 期望的布局（它只在 manifest 旁边找模块）。
  cp "plugins/$name/manifest.json" "plugins/dist/$name/manifest.json"
  printf '%-34s %s\n' "$out" "$(du -h "$out" | cut -f1)"
  printf '  安装: mcp-hub -plugin-install plugins/dist/%s/manifest.json\n' "$name"
done
