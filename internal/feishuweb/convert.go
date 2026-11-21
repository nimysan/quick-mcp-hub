package feishuweb

// 块树 -> 中间树（mdast 形状）-> Markdown。
//
// 规则移植自 cloud-document-converter（MIT，Copyright (c) 2024 - present Junji Lu，
// https://github.com/whale4113/cloud-document-converter ，packages/lark/src/docx.ts
// 的 Transformer、mergeListItems、transformOperationsToPhrasingContents），
// 见 THIRD-PARTY-NOTICES.md。和原版的差别都是为了「输出当不可信内容」：
//
//   - 不输出任何来自文档的原始 HTML：iframe 改成普通链接，下划线、高亮丢掉样式；
//   - 链接只保留 http / https / mailto，javascript: 之类一律只留文字；
//   - 文字里的 Markdown 控制字符全部转义，文档内容造不出链接、图片或 HTML；
//   - 图片、附件只给占位文字：签名地址两小时就失效，而下载文件要额外请求；
//   - 分栏（grid）按顺序摊平，不拼成表格；
//   - 块数、层数、输出长度都有上限。

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Block 是扩展从 PageMain 块树里抄出来的一个块（见 extension/extract.js 的
// serialize）。只抄转换要用的字段，不抄 record id、用户 id 这类东西。
type Block struct {
	Type     string   `json:"type"`
	Ops      []Op     `json:"ops,omitempty"`
	Text     string   `json:"text,omitempty"` // zoneState.allText，代码块用
	Children []*Block `json:"children,omitempty"`

	Seq      string `json:"seq,omitempty"`      // 标题编号 / 有序列表序号
	SeqLevel string `json:"seqLevel,omitempty"` // 标题编号层级，"auto" 表示多级
	Done     bool   `json:"done,omitempty"`     // 待办是否完成
	Language string `json:"language,omitempty"` // 代码块语言
	Columns  int    `json:"columns,omitempty"`  // 表格列数
	Name     string `json:"name,omitempty"`     // 图片 / 附件文件名
	Caption  string `json:"caption,omitempty"`  // 图片说明
	URL      string `json:"url,omitempty"`      // iframe 地址
	Mermaid  string `json:"mermaid,omitempty"`  // 文本绘图（ISV）
}

// Op 是富文本的一段（Quill delta 风格）。属性值在飞书里一般是字符串 "true"。
type Op struct {
	Insert     string         `json:"insert"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

func (o Op) attr(name string) string {
	switch v := o.Attributes[name].(type) {
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// Limits 约束一次转换。零值用默认。
type Limits struct {
	MaxBlocks int // 默认 20000
	MaxDepth  int // 默认 64
	MaxRunes  int // 输出上限，默认 200000
}

func (l Limits) withDefaults() Limits {
	if l.MaxBlocks <= 0 {
		l.MaxBlocks = 20000
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = 64
	}
	if l.MaxRunes <= 0 {
		l.MaxRunes = 200000
	}
	return l
}

// Converted 是一次转换的结果。
type Converted struct {
	Markdown  string
	Truncated bool
	Counts    Counts
}

// Convert 把块树转成 Markdown。root 一般是 type=page 的根块。
func Convert(root *Block, lim Limits) Converted {
	lim = lim.withDefaults()
	c := &converter{lim: lim}
	var tree *node
	if root == nil {
		tree = &node{kind: "root"}
	} else if root.Type == "page" {
		tree = c.transform(root, 0)[0]
	} else {
		tree = &node{kind: "root", children: c.mergeList(c.transform(root, 0))}
	}
	md := strings.TrimSpace(renderBlocks(tree.children, ""))
	out := Converted{Truncated: c.truncated}
	md, cut := truncateRunes(md, lim.MaxRunes)
	if cut {
		out.Truncated = true
	}
	if out.Truncated {
		md += "\n\n…（内容过长，已截断）"
	}
	out.Markdown = md
	out.Counts = Counts{Blocks: c.blocks, Skipped: c.skipped, OutputRunes: utf8.RuneCountInString(md)}
	return out
}

// CleanTitle 规整标题：去掉控制字符和零宽字符，限制长度。标题是不可信的，
// 它会原样出现在工具结果里。
func CleanTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || r == '\u200b' || r == '\ufeff' || r == '\u2060' || r == '\u200e' || r == '\u200f' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// ── 中间树 ─────────────────────────────────────────────────────

type node struct {
	kind     string // root heading paragraph list listItem code blockquote hr table row cell text strong emphasis delete inlineCode inlineMath link br
	children []*node
	value    string
	depth    int
	// 列表
	ordered bool
	start   int
	checked *bool
	seq     string // listItem：""（无序）、"auto" 或数字
	// 代码 / 链接
	lang string
	url  string
}

type converter struct {
	lim       Limits
	blocks    int
	skipped   int
	truncated bool
	sequences []string // 标题编号状态
}

func isTextLike(t string) bool {
	switch t {
	case "text", "heading1", "heading2", "heading3", "heading4", "heading5",
		"heading6", "heading7", "heading8", "heading9":
		return true
	}
	return false
}

// flatChildren 对应 CDC 的 transformParentBlock 里那段：文字和标题块的子块
// 提成兄弟，同步块取内容，分栏按顺序摊平。
func (c *converter) flatChildren(children []*Block, depth int) []*Block {
	var out []*Block
	if depth > c.lim.MaxDepth {
		c.truncated = true
		return nil
	}
	for _, ch := range children {
		if ch == nil {
			continue
		}
		switch {
		case isTextLike(ch.Type):
			out = append(out, ch)
			out = append(out, c.flatChildren(ch.Children, depth+1)...)
		case ch.Type == "synced_source" || ch.Type == "synced_reference":
			out = append(out, c.flatChildren(ch.Children, depth+1)...)
		case ch.Type == "grid":
			for _, col := range ch.Children {
				if col != nil {
					out = append(out, c.flatChildren(col.Children, depth+1)...)
				}
			}
		default:
			out = append(out, ch)
		}
	}
	return out
}

func (c *converter) transformChildren(b *Block, depth int) []*node {
	var out []*node
	for _, ch := range c.flatChildren(b.Children, depth+1) {
		out = append(out, c.transform(ch, depth+1)...)
	}
	return out
}

func boolPtr(v bool) *bool { return &v }

var digits = regexp.MustCompile(`^[0-9]+$`)

func (c *converter) transform(b *Block, depth int) []*node {
	if c.blocks >= c.lim.MaxBlocks || depth > c.lim.MaxDepth {
		c.truncated = true
		return nil
	}
	c.blocks++

	switch b.Type {
	case "page":
		return []*node{{kind: "root", children: c.mergeList(c.transformChildren(b, depth))}}
	case "divider":
		return []*node{{kind: "hr"}}
	case "heading1", "heading2", "heading3", "heading4", "heading5", "heading6":
		d := int(b.Type[len(b.Type)-1] - '0')
		h := &node{kind: "heading", depth: d, children: phrasing(b.Ops)}
		if b.Seq != "" {
			if len(c.sequences) > d {
				c.sequences = c.sequences[:d]
			}
			for len(c.sequences) < d {
				c.sequences = append(c.sequences, "")
			}
			if b.Seq == "auto" {
				prev, _ := strconv.Atoi(c.sequences[d-1])
				c.sequences[d-1] = strconv.Itoa(prev + 1)
			} else {
				c.sequences[d-1] = b.Seq
			}
			var seqs []string
			if b.SeqLevel == "auto" {
				for _, s := range c.sequences[:d] {
					if s != "" {
						seqs = append(seqs, s)
					}
				}
			} else {
				seqs = []string{b.Seq}
			}
			sep := " "
			if len(seqs) == 1 {
				sep = ". "
			}
			h.children = append([]*node{{kind: "text", value: strings.Join(seqs, ".") + sep}}, h.children...)
		}
		return []*node{h}
	case "text", "heading7", "heading8", "heading9":
		return []*node{{kind: "paragraph", children: phrasing(b.Ops)}}
	case "code":
		return []*node{{kind: "code", lang: strings.ToLower(b.Language), value: strings.TrimSuffix(b.Text, "\n")}}
	case "quote_container", "callout":
		return []*node{{kind: "blockquote", children: c.mergeList(c.transformChildren(b, depth))}}
	case "quote":
		return []*node{{kind: "blockquote", children: []*node{{kind: "paragraph", children: phrasing(b.Ops)}}}}
	case "bullet", "ordered", "todo":
		li := &node{kind: "listItem"}
		switch b.Type {
		case "todo":
			li.checked = boolPtr(b.Done)
		case "ordered":
			li.seq = "auto"
			if digits.MatchString(b.Seq) {
				li.seq = b.Seq
			}
		}
		li.children = append([]*node{{kind: "paragraph", children: phrasing(b.Ops)}},
			c.mergeList(c.transformChildren(b, depth))...)
		return []*node{li}
	case "image":
		label := firstNonEmpty(b.Caption, b.Name, "图片")
		return []*node{placeholder("图片：" + label)}
	case "file":
		return []*node{placeholder("附件：" + firstNonEmpty(b.Name, "未命名"))}
	case "view":
		// view 里装的是附件块
		return c.transformChildren(b, depth)
	case "table":
		return []*node{c.table(b, depth)}
	case "table_cell", "grid_column":
		// 只该出现在 table / grid 里，单独出现时按内容摊平
		return c.transformChildren(b, depth)
	case "iframe":
		if u := safeURL(b.URL); u != "" {
			link := &node{kind: "link", url: u, children: []*node{{kind: "text", value: "嵌入内容"}}}
			return []*node{{kind: "paragraph", children: []*node{link}}}
		}
		c.skipped++
		return nil
	case "isv":
		if b.Mermaid != "" {
			return []*node{{kind: "code", lang: "mermaid", value: b.Mermaid}}
		}
		c.skipped++
		return nil
	case "sheet":
		c.skipped++
		return []*node{placeholder("内嵌电子表格，未读取")}
	case "bitable", "base_refer":
		c.skipped++
		return []*node{placeholder("内嵌多维表格，未读取")}
	case "whiteboard", "diagram":
		c.skipped++
		return []*node{placeholder("画板 / 流程图，未读取")}
	case "mindnote":
		c.skipped++
		return []*node{placeholder("思维笔记，未读取")}
	case "chat_card":
		c.skipped++
		return []*node{placeholder("群名片，未读取")}
	}
	// 不认识的块：有文字就当段落，没有就跳过。
	c.skipped++
	if len(b.Ops) > 0 {
		return []*node{{kind: "paragraph", children: phrasing(b.Ops)}}
	}
	return nil
}

func placeholder(s string) *node {
	return &node{kind: "paragraph", children: []*node{{kind: "text", value: "[" + s + "]"}}}
}

// table 把单元格按列数切行。单元格里的段落用 <br> 连起来（GFM 表格里唯一的
// 换行办法），列表、代码这类块内容摊成文字。<br> 是转换器自己生成的，不来自文档。
func (c *converter) table(b *Block, depth int) *node {
	t := &node{kind: "table"}
	cols := b.Columns
	var cells []*node
	for _, cb := range b.Children {
		if cb == nil {
			continue
		}
		cell := &node{kind: "cell"}
		for i, n := range c.transformChildren(cb, depth+1) {
			if i > 0 {
				cell.children = append(cell.children, &node{kind: "br"})
			}
			cell.children = append(cell.children, inlineOf(n)...)
		}
		cells = append(cells, cell)
	}
	if cols <= 0 {
		cols = len(cells)
	}
	if cols == 0 {
		return t
	}
	for i := 0; i < len(cells); i += cols {
		end := i + cols
		if end > len(cells) {
			end = len(cells)
		}
		row := &node{kind: "row", children: append([]*node(nil), cells[i:end]...)}
		for len(row.children) < cols {
			row.children = append(row.children, &node{kind: "cell"})
		}
		t.children = append(t.children, row)
	}
	return t
}

// inlineOf 把一个块节点压成行内内容，给表格单元格用。
func inlineOf(n *node) []*node {
	switch n.kind {
	case "paragraph", "heading":
		return n.children
	case "code":
		return []*node{{kind: "inlineCode", value: n.value}}
	case "hr":
		return nil
	}
	var out []*node
	for i, ch := range n.children {
		if i > 0 {
			out = append(out, &node{kind: "text", value: " "})
		}
		out = append(out, inlineOf(ch)...)
	}
	return out
}

func listKind(n *node) string {
	if n.checked != nil {
		return "todo"
	}
	if n.seq != "" {
		return "ordered"
	}
	return "bullet"
}

// mergeList 对应 CDC 的 mergeListItems：相邻的同类列表项合成一个列表；
// 有序列表还要求序号接得上（或是 auto）。
func (c *converter) mergeList(nodes []*node) []*node {
	var out []*node
	for i := 0; i < len(nodes); {
		n := nodes[i]
		if n.kind != "listItem" {
			out = append(out, n)
			i++
			continue
		}
		j := i + 1
		for j < len(nodes) && nodes[j].kind == "listItem" && sameList(nodes[j-1], nodes[j]) {
			j++
		}
		l := &node{kind: "list", children: nodes[i:j]}
		if listKind(n) == "ordered" {
			l.ordered = true
			l.start = 1
			if v, err := strconv.Atoi(n.seq); err == nil {
				l.start = v
			}
		}
		out = append(out, l)
		i = j
	}
	return out
}

func sameList(a, b *node) bool {
	ka, kb := listKind(a), listKind(b)
	if ka != kb {
		return false
	}
	if ka != "ordered" {
		return true
	}
	if a.seq == "auto" {
		return b.seq == "auto"
	}
	if b.seq == "auto" {
		return true
	}
	x, _ := strconv.Atoi(a.seq)
	y, _ := strconv.Atoi(b.seq)
	return x+1 == y
}

// ── 富文本 ─────────────────────────────────────────────────────

type segment struct {
	text                 string
	code, math           bool
	bold, italic, strike bool
	link                 string
}

func (s segment) sameStyle(o segment) bool {
	return s.code == o.code && s.math == o.math && s.bold == o.bold &&
		s.italic == o.italic && s.strike == o.strike && s.link == o.link
}

// phrasing 对应 CDC 的 transformOperationsToPhrasingContents。
func phrasing(ops []Op) []*node {
	var segs []segment
	for _, op := range ops {
		if op.attr("fixEnter") != "" {
			continue
		}
		if len(op.Attributes) == 0 && op.Insert == "\n" {
			continue
		}
		insert := op.Insert
		link := op.attr("link")
		if ic := op.attr("inline-component"); ic != "" {
			var comp struct {
				Type string `json:"type"`
				Data struct {
					RawURL string `json:"raw_url"`
					Title  string `json:"title"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(ic), &comp) == nil {
				switch comp.Type {
				case "mention_doc":
					link = comp.Data.RawURL
					insert += comp.Data.Title
				case "user":
					// 原版拿用户 id 再去查名字；这里不发额外请求，只标出「提到了一个人」。
					insert = "@用户"
				}
			}
		}
		s := segment{
			text:   insert,
			bold:   op.attr("bold") != "",
			italic: op.attr("italic") != "",
			strike: op.attr("strikethrough") != "",
			code:   op.attr("inlineCode") != "",
		}
		if eq := op.attr("equation"); eq != "" {
			s.math, s.text = true, strings.TrimSuffix(eq, "\n")
		}
		if link != "" {
			// 飞书把链接 URL 编码后存，原版也是先 decodeURIComponent。
			if d, err := url.QueryUnescape(link); err == nil {
				link = d
			}
			s.link = safeURL(link)
		}
		if n := len(segs); n > 0 && segs[n-1].sameStyle(s) {
			segs[n-1].text += s.text
			continue
		}
		segs = append(segs, s)
	}

	var out []*node
	for _, s := range segs {
		var n *node
		switch {
		case s.math:
			n = &node{kind: "inlineMath", value: s.text}
		case s.code:
			n = &node{kind: "inlineCode", value: s.text}
		default:
			n = &node{kind: "text", value: s.text}
		}
		if s.strike {
			n = &node{kind: "delete", children: []*node{n}}
		}
		if s.italic {
			n = &node{kind: "emphasis", children: []*node{n}}
		}
		if s.bold {
			n = &node{kind: "strong", children: []*node{n}}
		}
		if s.link != "" {
			n = &node{kind: "link", url: s.link, children: []*node{n}}
		}
		out = append(out, n)
	}
	return out
}

// safeURL 只放行 http / https / mailto，返回可以直接放进 Markdown 链接括号里的
// 形式；其余（javascript:、data:、相对地址、带控制字符的）一律返回空。
func safeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return ""
		}
	case "mailto":
	default:
		return ""
	}
	s := u.String()
	return strings.NewReplacer("(", "%28", ")", "%29", " ", "%20", "<", "%3C", ">", "%3E").Replace(s)
}

// ── 渲染 ───────────────────────────────────────────────────────

func renderBlocks(nodes []*node, indent string) string {
	var parts []string
	for _, n := range nodes {
		if s := renderBlock(n, indent); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

func indentLines(s, first, rest string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		p := rest
		if i == 0 {
			p = first
		}
		if lines[i] == "" && i > 0 {
			continue
		}
		lines[i] = p + lines[i]
	}
	return strings.Join(lines, "\n")
}

func renderBlock(n *node, indent string) string {
	switch n.kind {
	case "root":
		return renderBlocks(n.children, indent)
	case "heading":
		text := oneLine(renderInline(n.children))
		if text == "" {
			return ""
		}
		return strings.Repeat("#", n.depth) + " " + text
	case "paragraph":
		return escapeLineStarts(strings.Trim(renderInline(n.children), "\n"))
	case "hr":
		return "---"
	case "code":
		fence := "```"
		for strings.Contains(n.value, fence) {
			fence += "`"
		}
		return fence + safeLang(n.lang) + "\n" + n.value + "\n" + fence
	case "blockquote":
		inner := renderBlocks(n.children, "")
		if inner == "" {
			return ""
		}
		lines := strings.Split(inner, "\n")
		for i, l := range lines {
			if l == "" {
				lines[i] = ">"
			} else {
				lines[i] = "> " + l
			}
		}
		return strings.Join(lines, "\n")
	case "list":
		var items []string
		for i, li := range n.children {
			marker := "- "
			switch {
			case li.checked != nil && *li.checked:
				marker = "- [x] "
			case li.checked != nil:
				marker = "- [ ] "
			case n.ordered:
				marker = strconv.Itoa(n.start+i) + ". "
			}
			// 列表项里的块用单换行连接，保持紧凑列表
			var parts []string
			for _, ch := range li.children {
				if p := renderBlock(ch, ""); p != "" {
					parts = append(parts, p)
				}
			}
			body := strings.Join(parts, "\n")
			pad := strings.Repeat(" ", len(marker))
			if li.checked != nil {
				pad = "  "
			}
			items = append(items, indentLines(body, marker, pad))
		}
		return strings.Join(items, "\n")
	case "table":
		if len(n.children) == 0 {
			return ""
		}
		var b strings.Builder
		for i, row := range n.children {
			b.WriteString("|")
			for _, cell := range row.children {
				b.WriteString(" " + oneLine(renderInline(cell.children)) + " |")
			}
			b.WriteString("\n")
			if i == 0 {
				b.WriteString("|" + strings.Repeat(" --- |", len(row.children)) + "\n")
			}
		}
		return strings.TrimSuffix(b.String(), "\n")
	}
	return oneLine(renderInline([]*node{n}))
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
}

var langRe = regexp.MustCompile(`^[a-z0-9_+#.-]{1,32}$`)

func safeLang(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if langRe.MatchString(l) {
		return l
	}
	return ""
}

func renderInline(nodes []*node) string {
	var b strings.Builder
	for _, n := range nodes {
		switch n.kind {
		case "text":
			b.WriteString(escapeText(n.value))
		case "br":
			b.WriteString("<br>")
		case "inlineCode":
			b.WriteString(inlineCode(n.value))
		case "inlineMath":
			b.WriteString("$" + strings.ReplaceAll(escapeText(oneLine(n.value)), "$", `\$`) + "$")
		case "strong":
			b.WriteString(wrap(renderInline(n.children), "**"))
		case "emphasis":
			b.WriteString(wrap(renderInline(n.children), "*"))
		case "delete":
			b.WriteString(wrap(renderInline(n.children), "~~"))
		case "link":
			text := oneLine(renderInline(n.children))
			if text == "" {
				text = escapeText(n.url)
			}
			if n.url == "" {
				b.WriteString(text)
			} else {
				b.WriteString("[" + text + "](" + n.url + ")")
			}
		default:
			b.WriteString(renderInline(n.children))
		}
	}
	return b.String()
}

// wrap 加强调标记，并把首尾空白挪到标记外面 ——「** 粗体**」在 Markdown
// 里不算粗体。
func wrap(s, mark string) string {
	core := strings.TrimSpace(s)
	if core == "" {
		return s
	}
	i := strings.Index(s, core)
	return s[:i] + mark + core + mark + s[i+len(core):]
}

func inlineCode(v string) string {
	v = strings.ReplaceAll(v, "\n", " ")
	if v == "" {
		return ""
	}
	longest, run := 0, 0
	for _, r := range v {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(v, "`") || strings.HasSuffix(v, "`") {
		v = " " + v + " "
	}
	return fence + v + fence
}

var mdEscaper = strings.NewReplacer(
	`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`,
	"<", `\<`, ">", `\>`, "~", `\~`, "|", `\|`, "!", `\!`,
)

// escapeText 转义文字里所有能造出格式、链接、图片或 HTML 的字符。
func escapeText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return mdEscaper.Replace(s)
}

// escapeLineStarts 转义段落每行开头的标题 / 列表 / setext 记号：段落里的
// 「# xx」「- xx」「1. xx」不该变成标题或列表。只对段落做，标题里的编号
// 「1. 概述」本来就不会被当成列表。
func escapeLineStarts(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		t := strings.TrimLeft(l, " ")
		if t == "" {
			continue
		}
		lead := l[:len(l)-len(t)]
		switch t[0] {
		case '#', '-', '+', '=':
			lines[i] = lead + `\` + t
		default:
			if m := orderedRe.FindString(t); m != "" {
				lines[i] = lead + m[:len(m)-1] + `\` + t[len(m)-1:]
			}
		}
	}
	return strings.Join(lines, "\n")
}

var orderedRe = regexp.MustCompile(`^[0-9]{1,9}[.)]`)

func truncateRunes(s string, max int) (string, bool) {
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	r := []rune(s)[:max]
	cut := string(r)
	if i := strings.LastIndex(cut, "\n"); i > len(cut)/2 {
		cut = cut[:i]
	}
	return cut, true
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
