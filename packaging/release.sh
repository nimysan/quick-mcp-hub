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

# 平台表：GOOS GOARCH 可执行文件名
#
# 没有启动脚本了：双击主程序本身就会打开配置页面（程序按 stdin 是不是管道
# 判断自己是被用户双击还是被 AI 客户端 spawn 的）。少一个文件，就少一件
# 要在文档里解释的事。
targets=(
  "windows amd64 mcp-hub.exe"
  "windows arm64 mcp-hub.exe"
  "darwin  arm64 mcp-hub"
  "darwin  amd64 mcp-hub"
  "linux   amd64 mcp-hub"
)

# 要带进包里的插件先构建好。
if [ -n "$PLUGINS" ]; then
  # shellcheck disable=SC2086
  ./plugins/build.sh $PLUGINS >/dev/null
fi

# Windows 的 exe 图标只能靠嵌入资源，Go 自己不做这件事：rsrc 把 .ico 编成
# 一个 .syso，文件名里的 _windows_<arch> 后缀让它只在那个目标上被链接进去。
# 构建完就删 —— 留着会变成一个来源不明的二进制中间产物。
RSRC_VER=v0.10.2
cleanup() { rm -f rsrc_windows_*.syso; }
trap cleanup EXIT

for t in "${targets[@]}"; do
  read -r goos goarch exe <<<"$t"
  name="mcp-hub-${VERSION}-${goos}-${goarch}"
  dir="${OUT}/${name}"
  mkdir -p "$dir"

  # Windows 上额外加 -H windowsgui：链成 GUI 子系统程序，双击时**不弹控制台
  # 窗口**。那个黑框是业务用户最先抱怨的东西。代价是 stderr 没有地方可去，
  # 所以程序把日志同时写到文件（internal/applog），页面和托盘都有查看入口。
  #
  # 这不影响 MCP 服务模式：stdio 的句柄是客户端 spawn 时给的管道，子系统
  # 标志只决定要不要分配控制台。
  ldflags="$LDFLAGS"
  if [ "$goos" = "windows" ]; then
    ldflags="$LDFLAGS -H windowsgui"
    go run "github.com/akavel/rsrc@${RSRC_VER}" \
      -ico packaging/windows/mcp-hub.ico -arch "$goarch" \
      -o "rsrc_windows_${goarch}.syso"
  fi
  GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags="$ldflags" -o "$dir/$exe" .
  cp "packaging/快速开始.md" "$dir/快速开始.md"

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
