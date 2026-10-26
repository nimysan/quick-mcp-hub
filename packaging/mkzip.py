#!/usr/bin/env python3
"""把一个目录打成 zip，并保证文件名用 UTF-8 标志位存储。

为什么不用 zip(1)：macOS 自带的 Info-ZIP 3.0 把非 ASCII 文件名按原始字节存，
不置 EFS（UTF-8）标志位。中文 Windows 的资源管理器会按系统代码页（GBK）去解，
解出来就是乱码文件名 —— 对一个要发给中文 Windows 用户的包来说这是硬伤。

Python 的 zipfile 对非 ASCII 名会自动置 flag_bits 0x800，Windows 据此按 UTF-8
解码。顺带把可执行位写进 external_attr，macOS / Linux 解压后无需再 chmod。

用法: mkzip.py <输出.zip> <要打包的目录>
"""
import os
import sys
import zipfile


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    out, src = sys.argv[1], sys.argv[2].rstrip("/")
    base = os.path.dirname(src) or "."
    top = os.path.basename(src)

    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for root, dirs, files in os.walk(src):
            dirs.sort()
            for name in sorted(files):
                full = os.path.join(root, name)
                rel = os.path.relpath(full, base)
                info = zipfile.ZipInfo.from_file(full, rel)
                # 保留可执行位：主程序和启动脚本解压后要能直接跑。
                mode = os.stat(full).st_mode & 0o777
                info.external_attr = mode << 16
                info.compress_type = zipfile.ZIP_DEFLATED
                with open(full, "rb") as f:
                    z.writestr(info, f.read())
        assert top  # 目录名进了每个条目的路径前缀
    return 0


if __name__ == "__main__":
    sys.exit(main())
