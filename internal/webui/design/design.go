// Package design 承载从 Figma 取出的设计 token。
//
// 为什么单独一个包：页面要「完全符合 Figma 的设计」，而做到这件事的唯一办法
// 是让颜色和图标**有一个可追溯的来源**，不是手抄进 CSS。tokens.json 由
// Figma Dev Mode MCP 的 get_variable_defs / 规格里的 SVG 地址导出，改了设计
// 重新导一次即可；手改那个文件就失去了这个性质。
//
// 两套颜色都来自稿子（深浅各一个画板），所以深色不是推导的。唯一的例外写在
// tokens.json 的 _danger_dark 里。
package design

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed tokens.json app.css
var files embed.FS

// Tokens 是 tokens.json 的结构。
type Tokens struct {
	Light  map[string]string `json:"light"`
	Dark   map[string]string `json:"dark"`
	Icons  map[string]string `json:"icons"`
	Source string            `json:"_source"`
}

// Load 读出设计 token。解析失败是编译期就该发现的错误（文件是 embed 的），
// 所以直接 panic —— 没有合理的运行时降级。
func Load() Tokens {
	var t Tokens
	data, err := files.ReadFile("tokens.json")
	if err != nil {
		panic("design: 读不到 tokens.json: " + err.Error())
	}
	if err := json.Unmarshal(data, &t); err != nil {
		panic("design: tokens.json 解析失败: " + err.Error())
	}
	return t
}

// CSSVars 把两套 token 渲染成 CSS 自定义属性。
//
// 变量名从 Figma 的 "text/primary" 转成 "--text-primary"：保留原名才能和设计
// 稿一一对照，改设计时不用猜哪个变量对应哪个。
//
// 深色走 prefers-color-scheme，同时支持 html[data-theme] 覆盖 —— 稿子上有
// 「Quick 浅色 / Quick 深色」切换器，所以用户的显式选择要能压过系统设置。
func (t Tokens) CSSVars() string {
	var b strings.Builder
	b.WriteString(":root{\n")
	writeVars(&b, t.Light)
	b.WriteString("}\n")
	b.WriteString("@media (prefers-color-scheme:dark){:root:not([data-theme=light]){\n")
	writeVars(&b, t.Dark)
	b.WriteString("}}\n")
	b.WriteString(":root[data-theme=dark]{\n")
	writeVars(&b, t.Dark)
	b.WriteString("}\n")
	return b.String()
}

func writeVars(b *strings.Builder, m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 顺序稳定，diff 才读得懂
	for _, k := range keys {
		fmt.Fprintf(b, "  --%s:%s;\n", strings.ReplaceAll(k, "/", "-"), m[k])
	}
}

// IconSprite 把图标渲染成一个 SVG sprite（一组 <symbol>），页面用
// <svg><use href="#i-Mail"/></svg> 引用。
//
// 用 sprite 而不是每处内联一遍：22 个图标里有的要用在多处，内联会把同一段
// path 重复好几次。图标的 stroke 已经改成 currentColor，所以颜色跟着文字走，
// 深色模式不用另做一套。
func (t Tokens) IconSprite() string {
	keys := make([]string, 0, len(t.Icons))
	for k := range t.Icons {
		// Figma 的提取会带出无名条目（稿子里的开关控件图形就是这样进来的），
		// 那会生成一个 id="i-" 的 symbol：页面永远引用不到它，但它会跟着每个
		// 页面发出去。tokens.json 保持是 Figma 的忠实快照（重新取就能覆盖），
		// 所以在这里过滤，而不是手改那个文件。
		if strings.TrimSpace(k) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(`<svg style="display:none" aria-hidden="true">`)
	for _, k := range keys {
		fmt.Fprintf(&b, `<symbol id="i-%s" viewBox="0 0 24 24" fill="none" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round">%s</symbol>`, k, t.Icons[k])
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// Stylesheet 返回照稿写的样式表。
//
// 它只用 var(--...)，不写死任何颜色 —— 颜色全部来自 CSSVars()，也就是来自
// Figma。稿子的规格是绝对定位的（281 处 shrink-0、零处 grow），照搬会得到一个
// 窗口一变就坏的页面，所以这份样式按稿子的视觉结果用 flex / grid 实现，
// 尺寸和间距取稿子的实测值。
func (t Tokens) Stylesheet() string {
	data, err := files.ReadFile("app.css")
	if err != nil {
		panic("design: 读不到 app.css: " + err.Error())
	}
	return string(data)
}
