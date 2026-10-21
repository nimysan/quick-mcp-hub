// Package channels 定义「渠道」：一个可接入的外部系统，以及它需要哪些字段。
//
// 渠道页面不认识 QQ 邮箱、也不认识 Jira，它只照着 Descriptor 渲染表单、
// 照着 SecretField 决定哪个字段进系统密码管理器。所以后续接新系统时，
// 页面和存储都不用改，只加一个描述符。
package channels

import (
	"fmt"
	"strings"

	"github.com/nimysan/mcp-hub/internal/mailprovider"
)

// FieldKind 决定页面上渲染成什么控件，以及值怎么存。
type FieldKind string

const (
	KindText FieldKind = "text"
	// KindPassword 的值**绝不**写进配置文件，只进系统密码管理器。
	KindPassword FieldKind = "password"
	KindNumber   FieldKind = "number"
	KindBool     FieldKind = "bool"
	KindSelect   FieldKind = "select"
)

// Option 是 select 的一个选项。
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Field 是渠道表单上的一个字段。
type Field struct {
	Name        string    `json:"name"`
	Label       string    `json:"label"`
	Kind        FieldKind `json:"kind"`
	Required    bool      `json:"required"`
	Default     string    `json:"default,omitempty"`
	Placeholder string    `json:"placeholder,omitempty"`
	Help        string    `json:"help,omitempty"`
	Options     []Option  `json:"options,omitempty"`
	// Advanced 字段默认折叠起来，避免表单看着就让人放弃。
	Advanced bool `json:"advanced,omitempty"`
}

// Descriptor 描述一类渠道。
type Descriptor struct {
	// Type 是渠道类型 id，写进配置文件，不随显示名变化。
	Type string `json:"type"`
	Name string `json:"name"`
	Icon string `json:"icon"`
	// Summary 是选择列表上的一行说明。
	Summary string `json:"summary"`
	// Integration 是凭据在系统密码管理器里的分段名（credstore.Ref.Integration）。
	// 同一类资源共用一个分段，例如所有邮箱都是 "mail"。
	Integration string `json:"integration"`
	// AccountField 指出哪个字段是账号标识，它和 Integration 一起定位凭据。
	AccountField string `json:"accountField"`
	// SecretField 指出哪个字段是密码类，只有它进密码管理器。
	SecretField string `json:"secretField"`
	// Available 为 false 表示「待实现」，页面上灰掉且不可添加。
	Available bool     `json:"available"`
	Tools     []string `json:"tools,omitempty"`
	HelpURL   string   `json:"helpUrl,omitempty"`
	Fields    []Field  `json:"fields"`
}

// Field 按名字取字段。
func (d Descriptor) Field(name string) (Field, bool) {
	for _, f := range d.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Validate 校验一份表单值，并把非敏感值整理成待保存的 settings。
// 返回的 settings 一定不含密码类字段。
func (d Descriptor) Validate(values map[string]string) (account, secret string, settings map[string]string, err error) {
	settings = make(map[string]string, len(d.Fields))
	for _, f := range d.Fields {
		v := strings.TrimSpace(values[f.Name])
		if v == "" {
			v = f.Default
		}
		if f.Required && v == "" {
			return "", "", nil, fmt.Errorf("%s 不能为空", f.Label)
		}
		switch f.Name {
		case d.AccountField:
			account = v
		case d.SecretField:
			secret = v
		default:
			if f.Kind == KindPassword {
				// 描述符里除 SecretField 之外不应再有密码类字段：
				// 多出来的那个无处安放，宁可报错也不要落盘。
				return "", "", nil, fmt.Errorf("渠道 %s 的字段 %s 是密码类但未声明为 secretField", d.Type, f.Name)
			}
			if v != "" {
				settings[f.Name] = v
			}
		}
	}
	if account == "" {
		return "", "", nil, fmt.Errorf("缺少账号字段 %s", d.AccountField)
	}
	return account, secret, settings, nil
}

// mailDescriptor 由服务商表生成：各家只差默认主机端口和凭据叫法，
// 字段结构是同一套，所以不手写五份。
func mailDescriptor(p mailprovider.Provider) Descriptor {
	return Descriptor{
		Type:         p.Type,
		Name:         p.Name,
		Icon:         p.Icon,
		Summary:      fmt.Sprintf("IMAP / SMTP · %s", p.SecretLabel),
		Integration:  "mail",
		AccountField: "user",
		SecretField:  "password",
		Available:    true,
		Tools:        []string{"list_recent", "search_mail", "read_mail", "send_mail"},
		Fields: []Field{
			{
				Name: "user", Label: "邮箱地址", Kind: KindText, Required: true,
				Placeholder: "someone@" + p.Domains[0],
			},
			{
				Name: "password", Label: p.SecretLabel, Kind: KindPassword, Required: true,
				Help: p.SecretHelp,
			},
			{
				Name: "allowSend", Label: "允许发信（不可逆动作）", Kind: KindBool,
				Default: "false",
				Help:    "关闭时 send_mail 不会注册给模型，只有读取类工具可用",
			},
			{
				Name: "imapHost", Label: "IMAP 主机", Kind: KindText,
				Default: p.IMAPHost, Advanced: true,
			},
			{
				Name: "imapPort", Label: "IMAP 端口", Kind: KindNumber,
				Default: fmt.Sprint(p.IMAPPort), Advanced: true,
			},
			{
				Name: "smtpHost", Label: "SMTP 主机", Kind: KindText,
				Default: p.SMTPHost, Advanced: true,
			},
			{
				Name: "smtpPort", Label: "SMTP 端口", Kind: KindNumber,
				Default: fmt.Sprint(p.SMTPPort), Advanced: true,
				Help: "默认 465 直连 SSL。587 走 STARTTLS，在部分网络出口上不可达",
			},
		},
	}
}

// planned 是还没实现的渠道，放在页面上只为说明「这里能长出什么」，
// 不可添加。实现一个就把它从这里挪成真的描述符。
var planned = []Descriptor{
	{Type: "jira", Name: "Jira", Icon: "🎫", Summary: "待实现 · 工单读取与检索", Integration: "jira"},
	{Type: "feishu", Name: "飞书", Icon: "💬", Summary: "待实现 · 消息与文档", Integration: "feishu"},
	{Type: "custom", Name: "自定义 HTTP", Icon: "➕", Summary: "待实现 · 任意带 API Key 的服务", Integration: "custom"},
}

// All 返回全部渠道描述符，可用的在前。
func All() []Descriptor {
	out := make([]Descriptor, 0, len(mailprovider.All)+len(planned))
	for _, p := range mailprovider.All {
		out = append(out, mailDescriptor(p))
	}
	return append(out, planned...)
}

// ByType 按类型 id 查描述符。
func ByType(t string) (Descriptor, bool) {
	for _, d := range All() {
		if d.Type == t {
			return d, true
		}
	}
	return Descriptor{}, false
}
