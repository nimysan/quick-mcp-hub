#!/usr/bin/env python3
"""生成 MCP Hub 的图标。

设计只服从一个约束：**16×16 下还能认出来**。任务栏、开始菜单、文件列表里
它多半就是那么大，所以形状必须极简 —— 一个中心节点加三根辐条连到三个卫星
节点。语义正好对上产品：中间是这个程序，三个卫星是被接进来的系统。

为什么不画 SVG 再转：这台机器上没有任何 SVG 光栅化工具（没有 rsvg-convert、
没有 inkscape、没有 ImageMagick）。直接用 Pillow 画位图，4 倍超采样再缩，
省掉一整条转换链。

依赖 Pillow，不在仓库的 go.mod 里，所以这个脚本是**手动跑的**，产物（.ico
和 .png）入库。图标不会天天改，为它在 CI 里养一条 Python 依赖不值得。

用法：
    PYTHONPATH=<pillow 所在目录> python3 packaging/logo.py
"""
import os
import sys

from PIL import Image, ImageDraw

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)))

# 画布先放大 4 倍，最后缩回去 —— Pillow 的 draw 没有抗锯齿，
# 超采样是最省事的得到平滑边缘的办法。
SS = 4
SIZE = 1024
C = SIZE * SS

# 配色：深色底 + 高对比节点，小尺寸下才立得住。
# 中心用琥珀色（这个程序本身），卫星用白色（被接进来的系统）。
BG_TOP = (18, 30, 56)      # 深海军蓝
BG_BOTTOM = (14, 86, 104)  # 深青
NODE = (255, 255, 255)
CENTER = (245, 184, 66)    # 琥珀


def rounded_mask(size: int, radius: int) -> Image.Image:
    m = Image.new("L", (size, size), 0)
    ImageDraw.Draw(m).rounded_rectangle([0, 0, size - 1, size - 1], radius=radius, fill=255)
    return m


def gradient(size: int, top, bottom) -> Image.Image:
    g = Image.new("RGB", (1, size))
    for y in range(size):
        t = y / max(size - 1, 1)
        g.putpixel((0, y), tuple(round(a + (b - a) * t) for a, b in zip(top, bottom)))
    return g.resize((size, size), Image.NEAREST)


def draw_icon() -> Image.Image:
    img = gradient(C, BG_TOP, BG_BOTTOM).convert("RGBA")
    d = ImageDraw.Draw(img)

    cx = cy = C // 2
    r_center = int(C * 0.137)
    r_node = int(C * 0.076)
    dist = int(C * 0.283)
    spoke = int(C * 0.043)

    # 三个卫星：正上、右下、左下。三个比四个在 16px 下更清楚 —— 辐条之间
    # 留得出空隙，不会糊成一团。
    import math
    angles = (-90, 30, 150)
    pts = [(cx + dist * math.cos(math.radians(a)), cy + dist * math.sin(math.radians(a)))
           for a in angles]

    # 辐条先画，被节点盖住一截，看起来是连进节点里而不是压在上面。
    for x, y in pts:
        d.line([cx, cy, x, y], fill=NODE, width=spoke)
    for x, y in pts:
        d.ellipse([x - r_node, y - r_node, x + r_node, y + r_node], fill=NODE)
    d.ellipse([cx - r_center, cy - r_center, cx + r_center, cy + r_center], fill=CENTER)

    img = img.resize((SIZE, SIZE), Image.LANCZOS)
    img.putalpha(rounded_mask(SIZE, int(SIZE * 0.22)))
    return img


def main() -> int:
    icon = draw_icon()

    png = os.path.join(OUT, "logo.png")
    icon.save(png)

    # Windows 的 .ico：带上 16 到 256 的各档，让系统在每种显示场景下都能挑到
    # 不用缩放的那一档。Pillow 对大尺寸会用 PNG 压缩存，Vista 以后都认。
    ico = os.path.join(OUT, "windows", "mcp-hub.ico")
    os.makedirs(os.path.dirname(ico), exist_ok=True)
    icon.save(ico, format="ICO",
              sizes=[(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)])

    # 顺手出 macOS 的 iconset，以后做 .app / dmg 时 iconutil 直接用。
    iconset = os.path.join(OUT, "macos", "MCPHub.iconset")
    os.makedirs(iconset, exist_ok=True)
    for base in (16, 32, 128, 256, 512):
        icon.resize((base, base), Image.LANCZOS).save(
            os.path.join(iconset, f"icon_{base}x{base}.png"))
        icon.resize((base * 2, base * 2), Image.LANCZOS).save(
            os.path.join(iconset, f"icon_{base}x{base}@2x.png"))

    for p in (png, ico):
        print(f"{p}  {os.path.getsize(p):,} 字节")
    print(f"{iconset}/  {len(os.listdir(iconset))} 个文件")
    return 0


if __name__ == "__main__":
    sys.exit(main())
