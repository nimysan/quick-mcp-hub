#!/usr/bin/env bash
# 打一份可以直接发给客户的发布包。
#
# 每个平台一个 zip，里面是：主程序 + 双击就能用的启动入口 + 快速开始文档。
# 不含任何凭据、不含配置文件 —— 那些在客户自己的机器上生成。
#
# 用法：
#   packaging/release.sh                    # 默认版本 0.1.0，不含插件
#   VERSION=0.2.0 packaging/release.sh      # 指定版本
#   PLUGINS=ghwasm packaging/release.sh     # 把指定的 wasm 插件一起打进去
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-0.1.0}"
PLUGINS="${PLUGINS:-}"
OUT="dist/pkg"

export CGO_ENABLED=0
# proxy.golang.org 在本机不可达，依赖已在模块缓存里，但保留镜像以防要拉。
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"

LDFLAGS="-s -w -X main.version=${VERSION}"

rm -rf "$OUT"
mkdir -p "$OUT"

# 平台表：GOOS GOARCH 可执行文件名 启动入口文件名
targets=(
  "windows amd64 mcp-hub.exe 打开配置页面.bat"
  "windows arm64 mcp-hub.exe 打开配置页面.bat"
  "darwin  arm64 mcp-hub     打开配置页面.command"
  "darwin  amd64 mcp-hub     打开配置页面.command"
  "linux   amd64 mcp-hub     打开配置页面.sh"
)

# 要带进包里的插件先构建好。
if [ -n "$PLUGINS" ]; then
  # shellcheck disable=SC2086
  ./plugins/build.sh $PLUGINS >/dev/null
fi

for t in "${targets[@]}"; do
  read -r goos goarch exe launcher <<<"$t"
  name="mcp-hub-${VERSION}-${goos}-${goarch}"
  dir="${OUT}/${name}"
  mkdir -p "$dir"

  GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags="$LDFLAGS" -o "$dir/$exe" .
  cp "packaging/$launcher" "$dir/$launcher"
  cp "packaging/快速开始.md" "$dir/快速开始.md"

  # 启动入口要可执行；Windows 的 .bat 不需要，但要 CRLF 换行。
  case "$goos" in
    windows) : ;;
    *) chmod +x "$dir/$launcher" ;;
  esac

  for p in $PLUGINS; do
    mkdir -p "$dir/plugins/$p"
    cp "plugins/dist/$p/manifest.json" "plugins/dist/$p/$p.wasm" "$dir/plugins/$p/"
  done

  # 用 Python 打包而不是 zip(1)：macOS 的 Info-ZIP 不给非 ASCII 文件名置
  # UTF-8 标志位，中文 Windows 解出来是乱码文件名。
  python3 packaging/mkzip.py "${OUT}/${name}.zip" "$dir"
  rm -rf "$dir"
  printf '%-40s %s\n' "${name}.zip" "$(du -h "${OUT}/${name}.zip" | cut -f1)"
done

# 校验和：客户可以据此确认下载的包没被改过。
(cd "$OUT" && shasum -a 256 ./*.zip > SHA256SUMS.txt)

echo
echo "输出目录: $OUT"
echo "版本: $VERSION"
if [ -n "$PLUGINS" ]; then
  echo "含插件: $PLUGINS（客户需运行 mcp-hub -plugin-install plugins/<id>/manifest.json）"
else
  echo "不含插件（要带上示范插件：PLUGINS=ghwasm packaging/release.sh）"
fi
