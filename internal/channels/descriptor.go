// Package channels 定义「渠道」：一个可接入的外部系统，以及它需要哪些字段。
//
// 渠道页面不认识 QQ 邮箱、也不认识 Jira，它只照着 Descriptor 渲染表单、
// 照着 SecretField 决定哪个字段进系统密码管理器。所以后续接新系统时，
// 页面和存储都不用改，只加一个描述符。
package channels

import (
	"fmt"
	"strings"

	"github.com/nimysan/mcp-hub/internal/httpchan"
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
	// Kind 说明这个渠道由哪个执行器承载：
	// "mail" 是编译进来的 IMAP/SMTP 实现，"http" 是声明式 HTTPS 渠道。
	// 页面不关心这个值，Manager 靠它分派。
	Kind string `json:"kind"`
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
	Available bool `json:"available"`
	// NeedsReview 表示这份声明里有「方法本身不能证明无害」的只读工具
	// （见 httpchan.Spec.PostReads），页面上要标出来。
	NeedsReview bool     `json:"needsReview,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	HelpURL     string   `json:"helpUrl,omitempty"`
	Fields      []Field  `json:"fields"`
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
//
// 密码字段不在这里强制必填：编辑渠道时留空表示「不改密码」，此时凭据
// 应当已经在系统密码管理器里。「既没填又没存过」由 Manager.Save 判断，
// 它是唯一能去问密码管理器的地方。
func (d Descriptor) Validate(values map[string]string) (account, secret string, settings map[string]string, err error) {
	settings = make(map[string]string, len(d.Fields))
	for _, f := range d.Fields {
		v := strings.TrimSpace(values[f.Name])
		if v == "" {
			v = f.Default
		}
		if f.Required && v == "" && f.Name != d.SecretField {
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

// KindMail 是编译进程序的邮件实现；KindHTTP 是声明式 HTTPS 渠道。
const (
	KindMail = "mail"
	KindHTTP = "http"
)

// mailDescriptor 由服务商表生成：各家只差默认主机端口和凭据叫法，
// 字段结构是同一套，所以不手写五份。
func mailDescriptor(p mailprovider.Provider) Descriptor {
	return Descriptor{
		Type:         p.Type,
		Name:         p.Name,
		Icon:         p.Icon,
		Kind:         KindMail,
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

// httpDescriptor 把一份声明式渠道 Spec 变成页面要的描述符。
//
// 这是「远端加一个 connector，客户端不升级就能用」的接缝：Spec 是数据，
// 页面照描述符渲染，执行器照 Spec 发请求，中间没有任何一行渠道专属代码。
func httpDescriptor(s httpchan.Spec) Descriptor {
	fields := []Field{
		{
			Name: s.Account.Name, Label: s.Account.Label, Kind: KindText, Required: true,
			Placeholder: s.Account.Placeholder, Help: s.Account.Help,
		},
		{
			Name: s.Secret.Name, Label: s.Secret.Label, Kind: KindPassword, Required: true,
			Placeholder: s.Secret.Placeholder, Help: s.Secret.Help,
		},
	}
	if s.HasWriteTools() {
		// 写开关和邮件渠道的「允许发信」是同一个设计：会改外部系统的工具
		// 默认不注册给模型，必须在这里显式打开。
		var names []string
		for _, t := range s.Tools {
			if !t.ReadOnly {
				names = append(names, t.Name)
			}
		}
		fields = append(fields, Field{
			Name: httpchan.AllowWriteField, Label: "允许写入操作", Kind: KindBool, Default: "false",
			Help: "关闭时这些工具不会注册给模型：" + strings.Join(names, "、"),
		})
	}
	for _, f := range s.Fields {
		fields = append(fields, Field{
			Name: f.Name, Label: f.Label, Kind: KindText, Required: f.Required,
			Default: f.Default, Placeholder: f.Placeholder, Help: f.Help, Advanced: f.Advanced,
		})
	}

	tools := make([]string, 0, len(s.Tools))
	for _, t := range s.Tools {
		tools = append(tools, t.Name)
	}

	return Descriptor{
		Type:         s.Type,
		Name:         s.Name,
		Icon:         s.Icon,
		Kind:         KindHTTP,
		Summary:      s.Summary,
		Integration:  s.Integration,
		AccountField: s.Account.Name,
		SecretField:  s.Secret.Name,
		Available:    true,
		NeedsReview:  s.PostReads,
		Tools:        tools,
		HelpURL:      s.HelpURL,
		Fields:       fields,
	}
}

// planned 是还没实现的渠道，放在页面上只为说明「这里能长出什么」，
// 不可添加。实现一个就把它从这里挪成真的描述符 —— 声明式渠道只要往
// httpchan/catalog 里加一个 JSON 文件，不用碰这里的 Go 代码。
var planned = []Descriptor{
	{Type: "jira", Name: "Jira", Icon: "🎫", Summary: "待实现 · 工单读取与检索", Integration: "jira"},
	{Type: "feishu", Name: "飞书", Icon: "💬", Summary: "待实现 · 消息与文档", Integration: "feishu"},
}

// All 返回全部渠道描述符，可用的在前。
func All() []Descriptor {
	specs := httpchan.All()
	out := make([]Descriptor, 0, len(mailprovider.All)+len(specs)+len(planned))
	for _, p := range mailprovider.All {
		out = append(out, mailDescriptor(p))
	}
	for _, s := range specs {
		out = append(out, httpDescriptor(s))
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
