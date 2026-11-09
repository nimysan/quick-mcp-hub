#!/usr/bin/env python3
# 把 docs/使用说明.md 生成一份真正的 .docx。
#
# 为什么自己拼 OOXML：这台机器上没有 pandoc / LibreOffice，而 macOS 自带的
# textutil 虽然能输出 docx，它的 docx 写出器**不支持表格和超链接**（实测：
# html->docx 和 html->rtf->docx 两条路，w:tbl 都不出现，表格被摊平成段落）。
# 这份文档一半内容是表格和下载链接，摊平就没用了。
#
# 所以这里直接写 OOXML。只需要标准库，不装任何东西。
#
# 用法：
#   python3 docs/build-docx.py                      # 生成 docs/MCP-Hub-使用说明.docx
#   python3 docs/build-docx.py --drop-appendix      # 去掉末尾「附：截图清单」，给客户的版本
#   python3 docs/build-docx.py -o /tmp/别的名字.docx
#
# 截图：Markdown 里的 ![说明](images/xx.png) ——
#   文件存在   -> 按原比例嵌进 Word（最宽 15cm）
#   文件不存在 -> 画一个灰色占位框，写着该拍什么
# 补完截图后重跑一次即可。

import argparse
import os
import re
import struct
import sys
import zipfile

NS = (
    'xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" '
    'xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" '
    'xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" '
    'xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" '
    'xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture"'
)

EMU_PER_CM = 360000
# A4 宽 21cm 减左右各 2.54cm 页边距 = 15.92cm 正文宽。截图是界面，缩小了就看不清
# 字，所以默认按正文满宽放，不留装饰性余量。
MAX_IMG_CM = 15.5
# 正文宽度：A4 21cm 减左右各 2.54cm 页边距
CONTENT_TWIPS = 9638


def esc(s):
    return (s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
             .replace('"', "&quot;"))


# ---------------------------------------------------------------- PNG / JPEG 尺寸
def image_size_px(path):
    """读图片宽高，不依赖 PIL。认 PNG 和 JPEG，其它返回 None。"""
    with open(path, "rb") as f:
        head = f.read(32)
        if head[:8] == b"\x89PNG\r\n\x1a\n":
            w, h = struct.unpack(">II", head[16:24])
            return w, h
        if head[:2] == b"\xff\xd8":
            f.seek(2)
            while True:
                b = f.read(1)
                while b and b != b"\xff":
                    b = f.read(1)
                if not b:
                    return None
                marker = f.read(1)
                while marker == b"\xff":
                    marker = f.read(1)
                if not marker:
                    return None
                if marker[0] in (0xD8, 0xD9) or 0xD0 <= marker[0] <= 0xD7:
                    continue
                ln = struct.unpack(">H", f.read(2))[0]
                if 0xC0 <= marker[0] <= 0xCF and marker[0] not in (0xC4, 0xC8, 0xCC):
                    f.read(1)
                    h, w = struct.unpack(">HH", f.read(4))
                    return w, h
                f.seek(ln - 2, 1)
    return None


# ---------------------------------------------------------------- 行内解析
INLINE = re.compile(
    r"!\[(?P<ialt>[^\]]*)\]\((?P<isrc>[^)]+)\)"      # 图片
    r"|\[(?P<ltext>[^\]]+)\]\((?P<lurl>[^)]+)\)"     # 链接
    r"|\*\*(?P<bold>[^*]+)\*\*"                      # 加粗
    r"|`(?P<code>[^`]+)`"                            # 行内代码
)


class Doc:
    def __init__(self, base_dir):
        self.base = base_dir
        self.body = []
        self.rels = []        # (rid, type, target, mode)
        self.media = {}       # 包内部件名 -> 磁盘路径
        self._partnames = {}  # 磁盘路径 -> 包内部件名（同一张图复用一份）
        self.exts = set()
        self._rid = 0
        self.missing = []

    def rid(self):
        self._rid += 1
        return f"rId%d" % (self._rid + 10)

    def add_link(self, url):
        r = self.rid()
        self.rels.append((r, "hyperlink", url, "External"))
        return r

    def part_name(self, path, ext):
        """包内部件名必须是 ASCII 的合法 URI 片段。

        截图文件名经常带中文和空格（比如「yunzhijia-点击 连接金蝶云之家.png」）
        —— 直接拿它当 OPC 部件名，Word 会判包损坏。所以这里统一改写成
        ASCII，并加序号保证唯一；原文件名不动。
        """
        if path in self._partnames:
            return self._partnames[path]
        stem = os.path.splitext(os.path.basename(path))[0]
        safe = re.sub(r"[^A-Za-z0-9._-]+", "-", stem).strip("-.") or "img"
        name = f"media/{len(self._partnames) + 1:02d}-{safe}.{ext}"
        self._partnames[path] = name
        return name

    def add_image(self, src):
        p = os.path.normpath(os.path.join(self.base, src))
        if not os.path.isfile(p):
            return None
        ext = os.path.splitext(p)[1].lower().lstrip(".")
        if ext == "jpg":
            ext = "jpeg"
        self.exts.add(ext)
        name = self.part_name(p, ext)
        self.media[name] = p
        r = self.rid()
        self.rels.append((r, "image", name, None))
        size = image_size_px(p)
        if size:
            w, h = size
            cm_w = min(MAX_IMG_CM, w / 96 * 2.54)
            cm_h = cm_w * h / w
        else:
            cm_w, cm_h = MAX_IMG_CM, MAX_IMG_CM * 0.6
        return r, int(cm_w * EMU_PER_CM), int(cm_h * EMU_PER_CM)

    # ---------------- 运行块
    def runs(self, text, bold=False, code=False, style=None):
        out = []
        pos = 0
        for m in INLINE.finditer(text):
            if m.start() > pos:
                out.append(self.run(text[pos:m.start()], bold, code, style))
            if m.group("ltext"):
                r = self.add_link(m.group("lurl"))
                out.append(f'<w:hyperlink r:id="{r}">'
                           + self.run(m.group("ltext"), bold, code, "Hyperlink")
                           + "</w:hyperlink>")
            elif m.group("bold"):
                out.append(self.run(m.group("bold"), True, code, style))
            elif m.group("code"):
                out.append(self.run(m.group("code"), bold, True, style))
            elif m.group("ialt") is not None:
                out.append(self.run(m.group("ialt"), bold, code, style))
            pos = m.end()
        if pos < len(text):
            out.append(self.run(text[pos:], bold, code, style))
        return "".join(out)

    def run(self, text, bold=False, code=False, style=None):
        if not text:
            return ""
        rpr = []
        if style:
            rpr.append(f'<w:rStyle w:val="{style}"/>')
        if code:
            rpr.append('<w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" '
                       'w:eastAsia="\u5fae\u8f6f\u96c5\u9ed1"/>'
                       '<w:shd w:val="clear" w:fill="F2F3F5"/>')
        if bold:
            rpr.append("<w:b/><w:bCs/>")
        pr = f"<w:rPr>{''.join(rpr)}</w:rPr>" if rpr else ""
        return (f"<w:r>{pr}<w:t xml:space=\"preserve\">{esc(text)}</w:t></w:r>")

    # ---------------- 块级
    def para(self, inner, style=None, extra=""):
        pr = ""
        bits = ""
        if style:
            bits += f'<w:pStyle w:val="{style}"/>'
        bits += extra
        if bits:
            pr = f"<w:pPr>{bits}</w:pPr>"
        self.body.append(f"<w:p>{pr}{inner}</w:p>")

    def heading(self, level, text):
        self.para(self.runs(text), style=f"Heading{min(level,3)}")

    def text_para(self, text):
        self.para(self.runs(text))

    def bullet(self, text):
        self.para(self.runs(text),
                  extra='<w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr>')

    def quote(self, lines):
        for i, ln in enumerate(lines):
            self.para(self.runs(ln), style="Quote")

    def code_block(self, lines):
        for ln in lines:
            self.para(self.run(ln or " ", code=True), style="CodeBlock")

    def hr(self):
        self.body.append(
            '<w:p><w:pPr><w:pBdr><w:bottom w:val="single" w:sz="6" '
            'w:space="1" w:color="D0D5DD"/></w:pBdr></w:pPr></w:p>')

    def image(self, alt, src):
        got = self.add_image(src)
        if not got:
            self.missing.append((src, alt))
            self.body.append(
                '<w:p><w:pPr><w:jc w:val="center"/>'
                '<w:pBdr>'
                '<w:top w:val="dashed" w:sz="6" w:space="6" w:color="B0B7C3"/>'
                '<w:bottom w:val="dashed" w:sz="6" w:space="6" w:color="B0B7C3"/>'
                '<w:left w:val="dashed" w:sz="6" w:space="6" w:color="B0B7C3"/>'
                '<w:right w:val="dashed" w:sz="6" w:space="6" w:color="B0B7C3"/>'
                '</w:pBdr>'
                '<w:shd w:val="clear" w:fill="FAFBFC"/>'
                '<w:spacing w:before="160" w:after="160"/></w:pPr>'
                '<w:r><w:rPr><w:color w:val="667085"/><w:sz w:val="19"/></w:rPr>'
                f'<w:t xml:space="preserve">[\u622a\u56fe\u4f4d\u7f6e] {esc(alt)}'
                f'  \uff08{esc(os.path.basename(src))}\uff09</w:t></w:r></w:p>')
            return
        r, cx, cy = got
        self.body.append(
            '<w:p><w:pPr><w:jc w:val="center"/>'
            '<w:spacing w:before="160" w:after="60"/></w:pPr><w:r><w:drawing>'
            f'<wp:inline distT="0" distB="0" distL="0" distR="0">'
            f'<wp:extent cx="{cx}" cy="{cy}"/>'
            f'<wp:docPr id="{self._rid}" name="{esc(alt) or "image"}"/>'
            '<a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/'
            'drawingml/2006/picture"><pic:pic>'
            f'<pic:nvPicPr><pic:cNvPr id="{self._rid}" name="{esc(alt) or "image"}"/>'
            '<pic:cNvPicPr/></pic:nvPicPr>'
            f'<pic:blipFill><a:blip r:embed="{r}"/><a:stretch><a:fillRect/>'
            '</a:stretch></pic:blipFill>'
            f'<pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="{cx}" cy="{cy}"/>'
            '</a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr>'
            '</pic:pic></a:graphicData></a:graphic></wp:inline>'
            "</w:drawing></w:r></w:p>")
        # 图下的说明文字（图注）：比正文小一号、灰色，和正文区分开
        if alt:
            self.body.append(
                '<w:p><w:pPr><w:jc w:val="center"/>'
                '<w:spacing w:after="240"/></w:pPr>'
                '<w:r><w:rPr><w:color w:val="667085"/><w:sz w:val="18"/>'
                f'<w:i/></w:rPr><w:t xml:space="preserve">{esc(alt)}'
                "</w:t></w:r></w:p>")

    def table(self, rows):
        ncol = max(len(r) for r in rows)
        w = CONTENT_TWIPS // ncol
        grid = "".join(f'<w:gridCol w:w="{w}"/>' for _ in range(ncol))
        border = ('<w:tblBorders>'
                  + "".join(f'<w:{s} w:val="single" w:sz="4" w:space="0" '
                            'w:color="D0D5DD"/>'
                            for s in ("top", "left", "bottom", "right",
                                      "insideH", "insideV"))
                  + "</w:tblBorders>")
        out = ['<w:tbl><w:tblPr><w:tblW w:w="5000" w:type="pct"/>'
               + border +
               '<w:tblCellMar><w:top w:w="60" w:type="dxa"/>'
               '<w:left w:w="110" w:type="dxa"/>'
               '<w:bottom w:w="60" w:type="dxa"/>'
               '<w:right w:w="110" w:type="dxa"/></w:tblCellMar>'
               '</w:tblPr><w:tblGrid>' + grid + "</w:tblGrid>"]
        for i, row in enumerate(rows):
            head = i == 0
            cells = []
            for j in range(ncol):
                cell = row[j] if j < len(row) else ""
                shd = ('<w:shd w:val="clear" w:fill="F2F4F7"/>' if head else "")
                cells.append(
                    f'<w:tc><w:tcPr><w:tcW w:w="{w}" w:type="dxa"/>{shd}'
                    '<w:vAlign w:val="center"/></w:tcPr>'
                    f'<w:p><w:pPr><w:spacing w:before="20" w:after="20"/></w:pPr>'
                    f"{self.runs(cell, bold=head)}</w:p></w:tc>")
            trpr = '<w:trPr><w:tblHeader/></w:trPr>' if head else ""
            out.append(f"<w:tr>{trpr}{''.join(cells)}</w:tr>")
        out.append("</w:tbl>")
        self.body.append("".join(out))
        # 表格后留一个空段，否则紧跟的表格会粘在一起
        self.body.append('<w:p><w:pPr><w:spacing w:after="0"/></w:pPr></w:p>')


# ---------------------------------------------------------------- Markdown 解析
def split_row(line):
    s = line.strip()
    if s.startswith("|"):
        s = s[1:]
    if s.endswith("|"):
        s = s[:-1]
    return [c.strip() for c in s.split("|")]


def is_sep(line):
    return bool(re.fullmatch(r"\|?[\s:\-|]+\|?", line.strip())) and "-" in line


def parse(md, doc, drop_appendix=False):
    lines = md.split("\n")
    i = 0
    n = len(lines)
    while i < n:
        raw = lines[i]
        s = raw.strip()

        if drop_appendix and re.match(r"^#+\s*\u9644[\uff1a:]", s):
            break

        if not s:
            i += 1
            continue

        if s.startswith("```"):
            i += 1
            buf = []
            while i < n and not lines[i].strip().startswith("```"):
                buf.append(lines[i])
                i += 1
            i += 1
            doc.code_block(buf)
            continue

        if re.fullmatch(r"-{3,}|\*{3,}|_{3,}", s):
            doc.hr()
            i += 1
            continue

        m = re.match(r"^(#{1,6})\s+(.*)$", s)
        if m:
            doc.heading(len(m.group(1)), m.group(2).strip())
            i += 1
            continue

        m = re.fullmatch(r"!\[([^\]]*)\]\(([^)]+)\)", s)
        if m:
            doc.image(m.group(1), m.group(2))
            i += 1
            continue

        if s.startswith(">"):
            buf = []
            while i < n and lines[i].strip().startswith(">"):
                buf.append(lines[i].strip().lstrip(">").strip())
                i += 1
            # 折叠成整段，保留换行语义
            merged = []
            for ln in buf:
                if ln:
                    if merged and merged[-1]:
                        merged[-1] += " " + ln
                    else:
                        merged.append(ln)
                else:
                    merged.append("")
            doc.quote([x for x in merged if x])
            continue

        if s.startswith("|") and i + 1 < n and is_sep(lines[i + 1]):
            rows = [split_row(lines[i])]
            i += 2
            while i < n and lines[i].strip().startswith("|"):
                rows.append(split_row(lines[i]))
                i += 1
            doc.table(rows)
            continue

        if re.match(r"^[-*]\s+", s):
            while i < n and re.match(r"^[-*]\s+", lines[i].strip()):
                doc.bullet(re.sub(r"^[-*]\s+", "", lines[i].strip()))
                i += 1
            continue

        # 普通段落：连续非空行合成一段
        buf = [s]
        i += 1
        while i < n:
            t = lines[i].strip()
            if (not t or t.startswith(("#", ">", "|", "```", "- ", "* "))
                    or re.fullmatch(r"-{3,}", t)
                    or re.fullmatch(r"!\[([^\]]*)\]\(([^)]+)\)", t)):
                break
            buf.append(t)
            i += 1
        doc.text_para(" ".join(buf))


# ---------------------------------------------------------------- 打包
STYLES = '''<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:styles %s>
<w:docDefaults><w:rPrDefault><w:rPr>
<w:rFonts w:ascii="Segoe UI" w:hAnsi="Segoe UI" w:eastAsia="\u5fae\u8f6f\u96c5\u9ed1" w:cs="Segoe UI"/>
<w:sz w:val="21"/><w:szCs w:val="21"/>
</w:rPr></w:rPrDefault>
<w:pPrDefault><w:pPr><w:spacing w:after="120" w:line="300" w:lineRule="auto"/></w:pPr></w:pPrDefault>
</w:docDefaults>
<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>
<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/>
<w:basedOn w:val="Normal"/><w:pPr><w:keepNext/><w:spacing w:before="360" w:after="200"/>
<w:outlineLvl w:val="0"/></w:pPr>
<w:rPr><w:b/><w:sz w:val="40"/><w:color w:val="101828"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/>
<w:basedOn w:val="Normal"/><w:pPr><w:keepNext/><w:spacing w:before="320" w:after="160"/>
<w:pBdr><w:bottom w:val="single" w:sz="6" w:space="4" w:color="E4E7EC"/></w:pBdr>
<w:outlineLvl w:val="1"/></w:pPr>
<w:rPr><w:b/><w:sz w:val="30"/><w:color w:val="1D2939"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading3"><w:name w:val="heading 3"/>
<w:basedOn w:val="Normal"/><w:pPr><w:keepNext/><w:spacing w:before="260" w:after="120"/>
<w:outlineLvl w:val="2"/></w:pPr>
<w:rPr><w:b/><w:sz w:val="24"/><w:color w:val="344054"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="CodeBlock"><w:name w:val="Code Block"/>
<w:basedOn w:val="Normal"/>
<w:pPr><w:shd w:val="clear" w:fill="F8F9FB"/>
<w:spacing w:after="0" w:line="260" w:lineRule="auto"/>
<w:ind w:left="200"/></w:pPr>
<w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:eastAsia="\u5fae\u8f6f\u96c5\u9ed1"/>
<w:sz w:val="19"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Quote"><w:name w:val="Quote"/>
<w:basedOn w:val="Normal"/>
<w:pPr><w:pBdr><w:left w:val="single" w:sz="18" w:space="8" w:color="7C4DFF"/></w:pBdr>
<w:shd w:val="clear" w:fill="F7F5FF"/><w:ind w:left="160"/>
<w:spacing w:before="140" w:after="140"/></w:pPr>
<w:rPr><w:color w:val="3F2E70"/></w:rPr></w:style>
<w:style w:type="character" w:styleId="Hyperlink"><w:name w:val="Hyperlink"/>
<w:rPr><w:color w:val="1A5FD6"/><w:u w:val="single"/></w:rPr></w:style>
</w:styles>''' % NS

NUMBERING = '''<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:numbering %s>
<w:abstractNum w:abstractNumId="0"><w:multiLevelType w:val="hybridMultilevel"/>
<w:lvl w:ilvl="0"><w:start w:val="1"/><w:numFmt w:val="bullet"/>
<w:lvlText w:val="\u2022"/><w:lvlJc w:val="left"/>
<w:pPr><w:ind w:left="420" w:hanging="220"/></w:pPr>
<w:rPr><w:rFonts w:ascii="Segoe UI" w:hAnsi="Segoe UI" w:hint="default"/></w:rPr>
</w:lvl></w:abstractNum>
<w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num>
</w:numbering>''' % NS


def build(doc, out_path):
    sect = ('<w:sectPr><w:pgSz w:w="11906" w:h="16838"/>'
            '<w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" '
            'w:header="851" w:footer="992" w:gutter="0"/></w:sectPr>')
    document = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
                f"<w:document {NS}><w:body>" + "".join(doc.body) + sect
                + "</w:body></w:document>")

    rel_items = [
        '<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/'
        'officeDocument/2006/relationships/styles" Target="styles.xml"/>',
        '<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/'
        'officeDocument/2006/relationships/numbering" Target="numbering.xml"/>',
    ]
    base = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"
    for rid, kind, target, mode in doc.rels:
        extra = f' TargetMode="{mode}"' if mode else ""
        rel_items.append(f'<Relationship Id="{rid}" Type="{base}{kind}" '
                         f'Target="{esc(target)}"{extra}/>')
    doc_rels = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
                '<Relationships xmlns="http://schemas.openxmlformats.org/'
                'package/2006/relationships">' + "".join(rel_items)
                + "</Relationships>")

    defaults = ['<Default Extension="rels" ContentType="application/'
                'vnd.openxmlformats-package.relationships+xml"/>',
                '<Default Extension="xml" ContentType="application/xml"/>']
    for e in sorted(doc.exts):
        defaults.append(f'<Default Extension="{e}" ContentType="image/{e}"/>')
    ct = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
          '<Types xmlns="http://schemas.openxmlformats.org/package/2006/'
          'content-types">' + "".join(defaults) +
          '<Override PartName="/word/document.xml" ContentType="application/'
          'vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>'
          '<Override PartName="/word/styles.xml" ContentType="application/'
          'vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>'
          '<Override PartName="/word/numbering.xml" ContentType="application/'
          'vnd.openxmlformats-officedocument.wordprocessingml.numbering+xml"/>'
          "</Types>")

    root_rels = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
                 '<Relationships xmlns="http://schemas.openxmlformats.org/'
                 'package/2006/relationships"><Relationship Id="rId1" '
                 f'Type="{base}officeDocument" Target="word/document.xml"/>'
                 "</Relationships>")

    with zipfile.ZipFile(out_path, "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr("[Content_Types].xml", ct)
        z.writestr("_rels/.rels", root_rels)
        z.writestr("word/document.xml", document)
        z.writestr("word/styles.xml", STYLES)
        z.writestr("word/numbering.xml", NUMBERING)
        z.writestr("word/_rels/document.xml.rels", doc_rels)
        for name, path in doc.media.items():
            z.write(path, "word/" + name)


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    ap = argparse.ArgumentParser()
    ap.add_argument("-i", "--input",
                    default=os.path.join(here, "\u4f7f\u7528\u8bf4\u660e.md"))
    ap.add_argument("-o", "--output", default=None)
    ap.add_argument("--drop-appendix", action="store_true",
                    help="\u53bb\u6389\u672b\u5c3e\u300c\u9644\uff1a\u622a\u56fe"
                         "\u6e05\u5355\u300d\uff08\u7ed9\u5ba2\u6237\u7684\u7248"
                         "\u672c\u7528\u8fd9\u4e2a\uff09")
    a = ap.parse_args()

    out = a.output or os.path.join(
        here, "MCP-Hub-\u4f7f\u7528\u8bf4\u660e.docx")
    with open(a.input, encoding="utf-8") as f:
        md = f.read()

    doc = Doc(os.path.dirname(os.path.abspath(a.input)))
    parse(md, doc, drop_appendix=a.drop_appendix)
    build(doc, out)

    print(f"\u751f\u6210\uff1a{out}  ({os.path.getsize(out):,} \u5b57\u8282)")
    print(f"  \u6bb5\u843d/\u8868\u683c\u5757 {len(doc.body)}"
          f"  \u5d4c\u5165\u56fe\u7247 {len(doc.media)}"
          f"  \u5916\u94fe {sum(1 for r in doc.rels if r[1]=='hyperlink')}")
    if doc.missing:
        print(f"  \u5f85\u8865\u622a\u56fe {len(doc.missing)} \u5f20"
              f"\uff08\u5df2\u7528\u5360\u4f4d\u6846\u4ee3\u66ff\uff09\uff1a")
        for src, alt in doc.missing:
            print(f"    {src}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
