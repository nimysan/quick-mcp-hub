#!/bin/sh
# 从 CHANGELOG.md 里取出某个版本那一节的正文，打到标准输出。
#
#   packaging/changelog.sh 0.1.13              # 默认读仓库根目录的 CHANGELOG.md
#   packaging/changelog.sh 0.1.13 path/to/CHANGELOG.md
#
# 找不到这一节、或者这一节是空的，就以非 0 退出：发布流水线靠它保证
# 「每个版本的 Release 页面上都有更新说明」，而不是静默发一个空的。
set -eu

ver="${1:?用法: $0 <版本号，如 0.1.13> [CHANGELOG.md]}"
ver="${ver#v}"
file="${2:-$(dirname "$0")/../CHANGELOG.md}"

[ -f "$file" ] || { echo "找不到 $file" >&2; exit 1; }

out=$(awk -v v="$ver" '
  # 下一个版本标题、或文末的链接定义，都是这一节的结尾。
  /^## \[/        { if (on) exit; if (index($0, "## [" v "]") == 1) on = 1; next }
  /^\[[^]]+\]: /  { if (on) exit }
  on {
    # 去掉开头的空行；中间的空行先攒着，后面有内容才放出来（顺带去掉结尾空行）。
    if ($0 ~ /^[[:space:]]*$/) { if (seen) blank++; next }
    while (blank > 0) { print ""; blank-- }
    print; seen = 1
  }
' "$file")

if [ -z "$out" ]; then
  echo "CHANGELOG.md 里没有版本 ${ver} 的一节（应当是「## [${ver}] - 日期」），或者这一节是空的。" >&2
  echo "发版前先把「未发布」改成这个版本号。" >&2
  exit 1
fi
printf '%s\n' "$out"
