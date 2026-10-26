// manifest 是一个 wasm 插件渠道的「签名过的那一半」。
//
// 关键设计：权威是 manifest，不是模块。字段表、工具清单、允许访问的主机都
// 来自这份签名数据，所以风险披露在任何第三方代码跑起来之前就能摆给用户，
// 模块也无法给自己扩权 —— 它改不了自己的 allowedHosts。
package wasmplug

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ABIVersion 是宿主实现的插件 ABI 版本。
//
// 这个数字一旦发布就是对外承诺：已发布的模块是照这套函数签名编译的，
// 改签名会让它们全部失效。所以 manifest 必须声明自己要哪一版，宿主
// 对不认识的版本要干净地拒绝并说清楚，而不是半残地跑起来。
const ABIVersion = 1

// Manifest 描述一个插件渠道。
type Manifest struct {
	ABI     int      `json:"abi"`
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Version int      `json:"version"`
	Icon    string   `json:"icon,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Tags    []string `json:"tags,omitempty"`

	// Module 是模块文件名，与 manifest 放在同一目录。
	Module string `json:"module"`

	// AllowedHosts 是这个插件被允许访问的主机白名单。宿主按它拦 fetch，
	// 模块无法越过。空列表意味着「不能联网」，不是「随便访问」。
	AllowedHosts []string `json:"allowedHosts"`

	// BaseURL / BaseURLField 和声明式渠道同义：固定接入点，或由用户填。
	BaseURL      string `json:"baseUrl,omitempty"`
	BaseURLField string `json:"baseUrlField,omitempty"`

	// Auth 说明凭据怎么带。注入由宿主完成，插件拿不到凭据本身 ——
	// 这样一个第三方模块只能「以这个身份干活」，不能把 token 带走。
	Auth Auth `json:"auth"`

	// Account / Secret 是页面上那两个输入框的文案。和声明式渠道同义：
	// Account 是凭据的定位键，Secret 是唯一进密码管理器的字段。
	Account Credential `json:"account"`
	Secret  Credential `json:"secret"`
	// Fields 是额外的非敏感配置项（站点地址、租户 id 之类）。
	Fields []Field `json:"fields,omitempty"`

	Tools []Tool `json:"tools"`
}

// Credential 是账号/密码输入框的文案。
type Credential struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
}

// Field 是一个额外配置项。它绝不会是密码类 —— 插件渠道只有一个凭据槽，
// 多出来的密码类字段会悄悄落进配置文件。
type Field struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Required    bool   `json:"required,omitempty"`
	Default     string `json:"default,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Advanced    bool   `json:"advanced,omitempty"`
}

// Param 是一个工具参数。宿主据它生成 JSON Schema 并校验必填项 ——
// 校验放在宿主而不是插件里，是因为「参数对不对」这件事不该由被审的那一方说。
type Param struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Type: string（默认）/ number / boolean / object / array
	Type     string   `json:"type,omitempty"`
	Required bool     `json:"required,omitempty"`
	Enum     []string `json:"enum,omitempty"`
}

// AllowWriteField 是写开关的字段名，和声明式渠道共用同一个名字。
const AllowWriteField = "allowWrite"

// ReservedAccountParam 是多实例时由工具层自动加上的参数名。
const ReservedAccountParam = "channel_account"

// paramTypes 是允许的参数类型。
var paramTypes = map[string]bool{
	"": true, "string": true, "number": true, "boolean": true, "object": true, "array": true,
}

// Auth 是宿主注入凭据的方式。
type Auth struct {
	// Kind: bearer | header | basic | none
	Kind string `json:"kind"`
	// Name 是 header 方式下的头名。
	Name string `json:"name,omitempty"`
}

// Tool 是插件暴露的一个工具。宿主只需要知道「叫什么、是不是只读、参数长
// 什么样」；具体怎么实现在模块里。
type Tool struct {
	Name        string  `json:"name"`
	Title       string  `json:"title,omitempty"`
	Description string  `json:"description"`
	ReadOnly    bool    `json:"readOnly,omitempty"`
	Params      []Param `json:"params,omitempty"`
}

// HasWriteTools 报告这个插件是否含会改动外部系统的工具。
func (m Manifest) HasWriteTools() bool {
	for _, t := range m.Tools {
		if !t.ReadOnly {
			return true
		}
	}
	return false
}

// Field 按名字取额外配置项。
func (m Manifest) Field(name string) (Field, bool) {
	for _, f := range m.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Validate 检查 manifest 本身。
func (m Manifest) Validate() error {
	if m.ABI != ABIVersion {
		return fmt.Errorf("插件 %s 要求 ABI v%d，本程序实现的是 v%d —— 需要升级客户端",
			m.ID, m.ABI, ABIVersion)
	}
	if err := checkID(m.ID); err != nil {
		return err
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("插件 %s 缺少显示名", m.ID)
	}
	if m.Version < 1 {
		return fmt.Errorf("插件 %s 的 version 必须从 1 起", m.ID)
	}
	if strings.TrimSpace(m.Module) == "" {
		return fmt.Errorf("插件 %s 没有指定模块文件", m.ID)
	}
	if strings.ContainsAny(m.Module, `/\`) {
		// 模块必须和 manifest 同目录：允许路径就等于允许它指向目录外的任意文件。
		return fmt.Errorf("插件 %s 的 module 不能带路径", m.ID)
	}
	if m.BaseURL == "" && m.BaseURLField == "" {
		return fmt.Errorf("插件 %s 必须给出 baseUrl 或 baseUrlField", m.ID)
	}
	if m.BaseURL != "" {
		if err := checkHTTPSURL(m.BaseURL); err != nil {
			return fmt.Errorf("插件 %s 的 baseUrl %w", m.ID, err)
		}
	}
	if len(m.AllowedHosts) == 0 {
		return fmt.Errorf("插件 %s 必须声明 allowedHosts（要访问哪些主机），这是风险披露的依据", m.ID)
	}
	for _, h := range m.AllowedHosts {
		if err := checkHost(h); err != nil {
			return fmt.Errorf("插件 %s 的 allowedHosts %w", m.ID, err)
		}
	}
	if m.BaseURL != "" {
		// 自洽性：接入点自己不在白名单里，这个插件第一次调用就会被沙箱拦，
		// 属于写错了而不是风险，所以在加载期就说清楚。
		u, _ := url.Parse(m.BaseURL)
		host := strings.ToLower(u.Hostname())
		found := false
		for _, h := range m.AllowedHosts {
			if h == host {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("插件 %s 的 baseUrl 主机 %s 不在 allowedHosts 内", m.ID, host)
		}
	}
	if len(m.Tools) == 0 {
		return fmt.Errorf("插件 %s 没有声明任何工具", m.ID)
	}
	switch m.Auth.Kind {
	case "bearer", "basic", "none", "":
	case "header":
		if strings.TrimSpace(m.Auth.Name) == "" {
			return fmt.Errorf("插件 %s 的 auth.kind=header 必须给出 auth.name", m.ID)
		}
	default:
		return fmt.Errorf("插件 %s 的 auth.kind %q 不支持（bearer / header / basic / none）", m.ID, m.Auth.Kind)
	}
	seen := map[string]bool{}
	for _, t := range m.Tools {
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("插件 %s 有工具没有名字", m.ID)
		}
		if seen[t.Name] {
			return fmt.Errorf("插件 %s 的工具名 %s 重复", m.ID, t.Name)
		}
		seen[t.Name] = true
		if !strings.HasPrefix(t.Name, m.ID+"_") {
			// 工具名要带渠道前缀，否则两个插件会在同一个 MCP server 里撞名。
			return fmt.Errorf("工具名 %s 必须以 %s_ 开头", t.Name, m.ID)
		}
		if strings.TrimSpace(t.Description) == "" {
			return fmt.Errorf("工具 %s 缺少 description（那是给模型看的）", t.Name)
		}
		pseen := map[string]bool{}
		for _, p := range t.Params {
			if !fieldNameRe.MatchString(p.Name) {
				return fmt.Errorf("工具 %s 的参数名 %q 不合法", t.Name, p.Name)
			}
			if p.Name == ReservedAccountParam {
				// 多账号分派参数由工具层自动加上，业务参数占用它会让分派
				// 被业务值顶掉。
				return fmt.Errorf("工具 %s 不能用保留参数名 %s", t.Name, ReservedAccountParam)
			}
			if pseen[p.Name] {
				return fmt.Errorf("工具 %s 的参数 %s 重复", t.Name, p.Name)
			}
			pseen[p.Name] = true
			if !paramTypes[p.Type] {
				return fmt.Errorf("工具 %s 的参数 %s 类型 %q 不支持", t.Name, p.Name, p.Type)
			}
		}
	}
	if err := m.checkFields(); err != nil {
		return err
	}
	return nil
}

// checkFields 校验账号、凭据和额外字段的字段名不互相冲突。
func (m Manifest) checkFields() error {
	if !fieldNameRe.MatchString(m.Account.Name) || !fieldNameRe.MatchString(m.Secret.Name) {
		return fmt.Errorf("插件 %s 的 account.name 和 secret.name 必须是合法字段名", m.ID)
	}
	if m.Account.Name == m.Secret.Name {
		return fmt.Errorf("插件 %s 的 account 和 secret 不能是同一个字段", m.ID)
	}
	if strings.TrimSpace(m.Account.Label) == "" || strings.TrimSpace(m.Secret.Label) == "" {
		return fmt.Errorf("插件 %s 的 account 和 secret 都要有 label（页面上要显示）", m.ID)
	}
	seen := map[string]bool{m.Account.Name: true, m.Secret.Name: true, AllowWriteField: true}
	for _, f := range m.Fields {
		if !fieldNameRe.MatchString(f.Name) {
			return fmt.Errorf("插件 %s 的字段名 %q 不合法", m.ID, f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("插件 %s 的字段名 %q 与保留字段冲突", m.ID, f.Name)
		}
		seen[f.Name] = true
		if strings.TrimSpace(f.Label) == "" {
			return fmt.Errorf("插件 %s 的字段 %s 缺少 label", m.ID, f.Name)
		}
	}
	if m.BaseURLField != "" {
		if _, ok := m.Field(m.BaseURLField); !ok {
			return fmt.Errorf("插件 %s 的 baseUrlField %q 在 fields 里不存在", m.ID, m.BaseURLField)
		}
	}
	return nil
}

var fieldNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,31}$`)

// WriteTools 返回会改动对方数据的工具名，供风险披露和写开关用。
func (m Manifest) WriteTools() []string {
	var out []string
	for _, t := range m.Tools {
		if !t.ReadOnly {
			out = append(out, t.Name)
		}
	}
	return out
}

// Tool 按名字取工具。
func (m Manifest) Tool(name string) (Tool, bool) {
	for _, t := range m.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func checkID(id string) error {
	if len(id) < 2 || len(id) > 31 {
		return fmt.Errorf("插件 id %q 长度要在 2-31 之间", id)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
		if !ok {
			return fmt.Errorf("插件 id %q 只能用小写字母、数字和连字符", id)
		}
	}
	return nil
}

func checkHTTPSURL(raw string) error {
	if strings.Contains(raw, "{") {
		// 主机名绝不能由下发内容模板出来：那等于让一份声明把凭据送到任意地址。
		return fmt.Errorf("不能含占位符: %s", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("无法解析: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("必须是 https: %s", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("缺少主机名: %s", raw)
	}
	return nil
}

func checkHost(h string) error {
	if strings.TrimSpace(h) == "" {
		return fmt.Errorf("有空的主机名")
	}
	if h != strings.ToLower(h) {
		return fmt.Errorf("主机名要小写: %s", h)
	}
	if strings.Contains(h, "*") {
		// 通配符一旦允许，披露给用户的那句话就失去意义。
		return fmt.Errorf("不支持通配符: %s", h)
	}
	if strings.Contains(h, "/") || strings.Contains(h, ":") {
		return fmt.Errorf("只写主机名，不要带协议或路径: %s", h)
	}
	return nil
}
