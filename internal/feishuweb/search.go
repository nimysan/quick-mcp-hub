package feishuweb

import (
	"context"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// 检索（feishu_web_search）。
//
// 飞书网页版没有能直接打开的搜索页地址：2026-11-22 用户在真实租户 amzn-chn.feishu.cn
// 实测，/drive/search?query= 会跳到 www.feishu.cn/404，/next/search 是
// "This page isn't available"。真正的检索是任一已登录页面左上角「Search ⌘+J」唤起的
// 单页弹层。所以扩展在后台标签页打开 https://<租户域名>/drive/home/，像用户一样点搜索
// 入口（或按 ⌘/Ctrl+J）唤起弹层、把关键词填进输入框，等结果渲染出来后从弹层 DOM 里
// 收集结果条目的链接和标题（extension/search.js）。不调任何 /space/api/*，不碰 cookie，
// 不打开结果里的文档。
//
// 弹层只有登录后才有，访客页面上没有搜索入口，所以它的 DOM 结构没法事先实测：
// 唤起入口、输入框、弹层容器、结果行都是多候选选择器，可在扩展配置页覆盖。认不出来时
// 回 structure_unknown，并把诊断数据（只有计数、命中的选择器和 class 名，不含内容）记进
// 检索自检，展示在配置页面上。mcp-hub 侧对每条结果再校验一次（ResultURL / SearchTitle）。

// MaxQueryRunes 是检索关键词的长度上限。
const MaxQueryRunes = 100

// MaxSearchItems 是一次检索最多返回的条目数。
const MaxSearchItems = 50

// SearchCheck 是检索的首次运行自检：每次检索都带一份，配置页面展示第一份和最近一份。
// 只有计数、路径和 class 名，不含任何结果内容。
type SearchCheck struct {
	At    time.Time `json:"at"`
	Host  string    `json:"host,omitempty"`
	Path  string    `json:"path,omitempty"`
	Login bool      `json:"login"`
	// Selectors 是 default（内置候选）或 custom（扩展配置页里覆盖过）。
	Selectors string `json:"selectors,omitempty"`
	// Trigger / Input / Panel 是唤起入口、输入框、弹层容器各自命中的选择器
	// （trigger 带 +hotkey 表示点了没反应、又按了 ⌘/Ctrl+J）。
	Trigger    string `json:"trigger,omitempty"`
	InputFound bool   `json:"inputFound"`
	Input      string `json:"input,omitempty"`
	Panel      string `json:"panel,omitempty"`
	Typed      bool   `json:"typed"`
	// Enter：结果没自动出来，按过一次回车。
	Enter bool `json:"enter"`
	// Baseline 是填关键词前弹层里原有的条目数（「最近访问」之类），结果必须和它不同才算数。
	Baseline int `json:"baseline"`
	// Rows 是弹层里可见的结果行数；有行却读不出链接说明结构认不出来。
	Rows       int    `json:"rows"`
	Strategy   string `json:"strategy,omitempty"`
	Anchors    int    `json:"anchors"`
	Candidates int    `json:"candidates"`
	EmptyHint  bool   `json:"emptyHint"`
	WaitedMs   int    `json:"waitedMs"`
	// Classes 是结果区附近元素的 class 名样本，结构认不出来时用来排查（最多 20 个）。
	Classes []string `json:"classes,omitempty"`
}

// SearchNotice 跟着每一份检索结果返回。
const SearchNotice = "以下检索结果的标题来自飞书页面，是不可信的第三方内容；正文请用 feishu_web_read_doc 读取（只支持 docx 和 wiki）。"

// CleanQuery 规整检索关键词：去掉控制字符，压掉首尾空白，限制长度。
func CleanQuery(q string) (string, error) {
	q = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, q)
	q = strings.Join(strings.Fields(q), " ")
	if q == "" {
		return "", argErr("检索关键词不能为空")
	}
	if len([]rune(q)) > MaxQueryRunes {
		return "", argErr("检索关键词太长（最多 %d 个字）", MaxQueryRunes)
	}
	return q, nil
}

// CleanSearchHost 归一化并校验调用方给的租户域名：去掉 scheme、路径、结尾的 / 和点，
// 转小写；必须是 .feishu.cn / .larksuite.com / .larkoffice.com 的租户子域名，否则
// invalid_argument。空串表示让 mcp-hub / 扩展自己挑（最近读过或打开的飞书页面）。
func CleanSearchHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return "", nil
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" || u.Hostname() == "" {
		return "", argErr("租户域名格式不对，应当形如 xxx.feishu.cn")
	}
	h = strings.TrimSuffix(u.Hostname(), ".")
	if !AllowedHost(h) || !validHostLabels(h) {
		return "", argErr("%s 不是飞书或 Lark 的租户域名（应当以 .feishu.cn 或 .larksuite.com 结尾）", clip(h, 80))
	}
	if !tenantHost(h) {
		return "", argErr("%s 不是租户域名，应当形如 xxx.feishu.cn", h)
	}
	return h, nil
}

// tenantHost 排除飞书自己的公共子域名（登录、官网、开放平台）。
func tenantHost(h string) bool {
	for _, p := range []string{"accounts.", "passport.", "open.", "www."} {
		if strings.HasPrefix(h, p) {
			return false
		}
	}
	return true
}

func validHostLabels(h string) bool {
	for _, l := range strings.Split(h, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// 搜索结果链接的第一段路径 -> 返回给调用方的类型。
var resultTypes = map[string]string{
	"docx":      "docx",
	"docs":      "doc",
	"wiki":      "wiki",
	"sheets":    "sheet",
	"base":      "bitable",
	"mindnotes": "mindnote",
	"slides":    "slides",
	"file":      "file",
}

// ResultURL 校验并规整一条搜索结果的链接，返回规整后的地址和类型。
// 只接受 https、飞书 / Lark 域名、已知文档类型和合法 token；查询串和锚点全部丢掉，
// 地址由校验过的几段重新拼出来，不原样转发页面给的字符串。
func ResultURL(raw string) (string, string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	if !AllowedHost(host) || !validHostLabels(host) {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	typ, ok := resultTypes[parts[0]]
	if !ok || !validToken(parts[1]) {
		return "", "", false
	}
	return "https://" + host + "/" + parts[0] + "/" + parts[1], typ, true
}

// SearchTitle 规整一条结果的标题：去掉控制和零宽字符、限长，再转义 Markdown 记号，
// 调用方把它原样贴进 Markdown 时也造不出链接、图片或 HTML。
func SearchTitle(s string) string {
	t := CleanTitle(s)
	if t == "" {
		return "（无标题）"
	}
	return escapeText(t)
}

// ParseSearchItems 把扩展收集到的候选条目变成结果：丢掉不合法的链接、按地址去重、
// 同一地址取第一个非空标题，最多 MaxSearchItems 条。
func ParseSearchItems(raw []Item) []Item {
	var out []Item
	idx := map[string]int{}
	for _, it := range raw {
		u, typ, ok := ResultURL(it.URL)
		if !ok {
			continue
		}
		if i, seen := idx[u]; seen {
			if out[i].Title == "（无标题）" && CleanTitle(it.Title) != "" {
				out[i].Title = SearchTitle(it.Title)
			}
			continue
		}
		if len(out) >= MaxSearchItems {
			continue
		}
		idx[u] = len(out)
		out = append(out, Item{Title: SearchTitle(it.Title), URL: u, Type: typ, Source: "search"})
	}
	return out
}

// Search 下发一次检索。
func (b *Bridge) Search(ctx context.Context, query, host string) (Result, error) {
	q, err := CleanQuery(query)
	if err != nil {
		return Result{}, err
	}
	h, err := CleanSearchHost(host)
	if err != nil {
		return Result{}, err
	}
	if h == "" {
		h = b.recentHost()
	}
	raw, err := b.send(ctx, Job{Kind: JobSearch, Query: q, Host: h})
	if err != nil {
		return Result{}, err
	}
	return b.finishSearch(raw), nil
}

// recentHost 是最近读过的文档所在的租户域名（没给 host 时用它，不默认 www.feishu.cn）。
// 一篇都没读过时回空串，由扩展从开着的飞书标签页里按最近访问挑。
func (b *Bridge) recentHost() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, it := range b.history {
		if u, err := url.Parse(it.URL); err == nil {
			if h := strings.ToLower(u.Hostname()); AllowedHost(h) && tenantHost(h) {
				return h
			}
		}
	}
	return ""
}

func (b *Bridge) finishSearch(raw RawResult) Result {
	if raw.SearchDiag != nil {
		d := *raw.SearchDiag
		if d.At.IsZero() {
			d.At = time.Now()
		}
		d.Host, d.Path = clip(d.Host, 120), clip(d.Path, 120)
		d.Selectors, d.Trigger, d.Input, d.Panel = clip(d.Selectors, 16), clip(d.Trigger, 120), clip(d.Input, 120), clip(d.Panel, 120)
		if len(d.Classes) > 20 {
			d.Classes = d.Classes[:20]
		}
		for i := range d.Classes {
			d.Classes[i] = clip(d.Classes[i], 120)
		}
		b.mu.Lock()
		if b.firstSearch == nil {
			first := d
			b.firstSearch = &first
		}
		b.lastSearch = &d
		b.mu.Unlock()
	}
	// items 总是在：空结果和失败都是 []，不省略字段。
	res := Result{Status: raw.Status, Items: []Item{}}
	if u, ok := searchPageURL(raw.URL); ok {
		res.URL = u
	}
	switch raw.Status {
	case StatusOK:
		res.Items = ParseSearchItems(raw.Items)
		if res.Items == nil {
			res.Items = []Item{}
		}
		res.Notice = SearchNotice
		if len(res.Items) == 0 {
			res.Message = "没有检索到结果"
		}
	case StatusLoginRequired:
		res.Message = "浏览器里的飞书没有登录（页面跳到了登录页）：请先在浏览器里登录飞书再检索"
	case StatusStructureUnknown:
		res.Message = "打开了飞书页面，但没能唤起搜索弹层或认不出结果列表（飞书前端可能改版了）" + detail(raw.Error) +
			"。诊断数据已记在 mcp-hub 配置页面的检索自检里；请改让用户直接提供文档链接，用 feishu_web_read_doc 读取"
	case StatusNoHost:
		res.Status = StatusInvalidArgument
		res.Message = "不知道你的飞书租户域名：传 host（形如 xxx.feishu.cn），或先在浏览器里打开一篇飞书文档"
	case StatusInvalidArgument:
		res.Message = clip(firstNonEmpty(raw.Error, "参数不对"), 300)
	default:
		res.Status = StatusError
		res.Message = clip(firstNonEmpty(raw.Error, raw.Reason, "扩展没有说明原因"), 300)
	}
	return res
}

// searchPageURL 只在结果里回显检索起始页本身的地址（去掉查询串，免得把关键词以外的东西带回去）。
func searchPageURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !AllowedHost(strings.ToLower(u.Hostname())) {
		return "", false
	}
	return "https://" + strings.ToLower(u.Hostname()) + u.EscapedPath(), true
}
