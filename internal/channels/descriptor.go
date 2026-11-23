// Package channels 定义「渠道」：一个可接入的外部系统，以及它需要哪些字段。
//
// 渠道页面不认识 QQ 邮箱、也不认识 Jira，它只照着 Descriptor 渲染表单、
// 照着 SecretField 决定哪个字段进系统密码管理器。所以后续接新系统时，
// 页面和存储都不用改，只加一个描述符。
package channels

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/nimysan/mcp-hub/internal/catalog"
	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/httpchan"
	"github.com/nimysan/mcp-hub/internal/mailprovider"
)

// specs 返回当前生效的声明式渠道。来源是 catalog 的三层合并结果
// （本地 > 远端 > 内置），而不是只有编译进来的那层 —— 远端下发的渠道
// 必须和内置的一样出现在页面上。
//
// 每次调用都重读磁盘：配置页面是常驻进程，用户往 catalog 目录里放一份声明
// 后应当刷新页面就能看到，不该要求重启。
func specs() []httpchan.Spec { return catalog.Load(config.Namespace).Specs() }

// SpecByID 按渠道标识取生效的声明。
func SpecByID(id string) (httpchan.Spec, bool) {
	return catalog.Load(config.Namespace).ByID(id)
}

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

// ToolMeta 是一个工具对用户的说明。
//
// 设计稿的「渠道工具」清单要逐条列出工具，并对关掉的那些说明原因
// （「需开启『允许发送邮件』」）—— 光给一个 snake_case 名字对业务人员等于乱码。
type ToolMeta struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Desc  string `json:"desc,omitempty"`
	// Write 为真表示它受写开关控制，关着时不会注册给模型。
	Write bool `json:"write,omitempty"`
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
	// Version 是渠道声明的版本号（邮件这类编译进来的固定为 1）。
	Version int `json:"version"`
	// Tags 是分类标签，页面用它做筛选。
	Tags []string `json:"tags,omitempty"`
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
	// （见 httpchan.Spec.PostReads）。
	//
	// 它是发布这份声明时的把关信号，不摆在渠道卡片上：那是我们的内部流程
	// 状态，对用户不构成任何可做的决定。页面以后要加「显示技术细节」那类
	// 视图时，它归在那边。
	NeedsReview bool     `json:"needsReview,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	// ToolInfo 是每个工具的标题和说明，供详情页的「渠道工具」清单用。
	// 和 Tools 并存而不是替换它：Tools 是名字数组，别处（计数、写开关判断）
	// 都在用。
	ToolInfo []ToolMeta `json:"toolInfo,omitempty"`
	// WriteTools 是 Tools 里会改动外部系统的那些，WriteSwitch 是控制它们的
	// 开关字段名。页面靠这两样算出「客户端实际能看到几个工具」—— 开关关着
	// 的工具不会注册，面板上的数字必须和真实状态对得上。
	WriteTools  []string `json:"writeTools,omitempty"`
	WriteSwitch string   `json:"writeSwitch,omitempty"`
	HelpURL     string   `json:"helpUrl,omitempty"`
	// OAuth 非空时页面上会多一个「连接」按钮：用户不必去对方系统手工生成
	// 一个长期令牌，点一下、在对方网站上确认即可。
	OAuth *OAuthInfo `json:"oauth,omitempty"`
	// ThirdPartyCode 表示这个渠道会在本机执行第三方代码（wasm 插件）。
	// AllowedHosts 是它被允许访问的主机。这两件事必须在用户点「添加」之前
	// 看得见 —— 沙箱挡得住读文件和连任意地址，挡不住它把拿到的数据发给
	// 自己声明的那台主机。
	ThirdPartyCode bool     `json:"thirdPartyCode,omitempty"`
	AllowedHosts   []string `json:"allowedHosts,omitempty"`
	// LocalProgram 表示这个渠道连的不是某个网站，而是用户自己机器上另一个
	// 程序开的本地接口（见 httpchan.Spec.Local）。这件事同样必须在点「添加」
	// 之前看得见：它意味着「还要先去装一个别的东西」，而不是填完就能用。
	LocalProgram bool `json:"localProgram,omitempty"`
	// Platform 是服务所在的平台（runtime.GOOS），只有文案随平台变的渠道（本机邮件）才填。
	Platform string  `json:"platform,omitempty"`
	Fields   []Field `json:"fields"`
}

// OAuthInfo 是页面渲染「连接」按钮要知道的事。
//
// AuthHost 是风险披露：用户要被送到哪台主机去输自己的账号密码。这件事必须
// 在点按钮之前就看得见 —— 一份被篡改的目录最直接的攻击就是把它指向别处。
type OAuthInfo struct {
	Flow          string   `json:"flow"`
	ClientIDField string   `json:"clientIdField,omitempty"`
	Scopes        []string `json:"scopes,omitempty"`
	SetupURL      string   `json:"setupUrl,omitempty"`
	Help          string   `json:"help,omitempty"`
	AuthHost      string   `json:"authHost"`
	// ClientSecret 非空时连接区多一个「应用密钥」输入框（飞书）。它不在
	// Fields 里：值只随连接请求走，最后进密码管理器，不进配置文件。
	ClientSecret *Field `json:"clientSecret,omitempty"`
	// Register 为真表示没填 App ID 时点连接会先一键建应用。
	Register bool `json:"register,omitempty"`
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

// KindMail 是编译进程序的邮件实现；KindHTTP 是声明式 HTTPS 渠道；
// KindPlugin 是 wasm 插件渠道（见 plugin.go）。
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
		Version:      1,
		Tags:         []string{"email"},
		Summary:      fmt.Sprintf("IMAP / SMTP · %s", p.SecretLabel),
		Integration:  "mail",
		AccountField: "user",
		SecretField:  "password",
		Available:    true,
		Tools:        []string{"list_recent", "search_mail", "read_mail", "send_mail"},
		WriteTools:   []string{"send_mail"},
		// 标题和说明用业务人员的话，不是 snake_case 的函数名。
		ToolInfo: []ToolMeta{
			{Name: "list_recent", Title: "最近邮件", Desc: "获取最近收到的邮件"},
			{Name: "search_mail", Title: "搜索邮件", Desc: "按关键词检索邮件"},
			{Name: "read_mail", Title: "读取邮件", Desc: "查看指定邮件的正文"},
			{Name: "send_mail", Title: "发送邮件", Desc: "需开启「允许发送邮件」", Write: true},
		},
		WriteSwitch: "allowSend",
		Fields: []Field{
			{
				Name: "user", Label: "邮箱地址", Kind: KindText, Required: true,
				Placeholder: addrPlaceholder(p.Domains),
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
				// 有预设主机时收进「高级设置」：那是已经验证过的值，用户不该
				// 被迫看见。没有预设时（通用的企业邮箱渠道）必须露出来并标为
				// 必填 —— 它们是空的，藏在折叠里等于让人对着一个连不上的表单
				// 猜哪里不对。
				Name: "imapHost", Label: "IMAP 主机", Kind: KindText,
				Default: p.IMAPHost, Advanced: p.IMAPHost != "",
				Required:    p.IMAPHost == "",
				Placeholder: hostPlaceholder(p.IMAPHost, "imap"),
				Help:        hostHelp(p.IMAPHost),
			},
			{
				Name: "imapPort", Label: "IMAP 端口", Kind: KindNumber,
				Default: fmt.Sprint(p.IMAPPort), Advanced: p.IMAPHost != "",
			},
			{
				Name: "smtpHost", Label: "SMTP 主机", Kind: KindText,
				Default: p.SMTPHost, Advanced: p.SMTPHost != "",
				Required:    p.SMTPHost == "",
				Placeholder: hostPlaceholder(p.SMTPHost, "smtp"),
			},
			{
				Name: "smtpPort", Label: "SMTP 端口", Kind: KindNumber,
				Default: fmt.Sprint(p.SMTPPort), Advanced: p.SMTPHost != "",
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
	secretField := Field{
		Name: s.Secret.Name, Label: s.Secret.Label, Kind: KindPassword, Required: true,
		Placeholder: s.Secret.Placeholder, Help: s.Secret.Help,
	}
	var oauthInfo *OAuthInfo
	if s.OAuth != nil {
		// 有「连接」按钮时手填令牌就不是必填了：两条路取其一。
		// 不说「上面/下面」——按钮排在表单前还是后由页面按渠道决定
		// （不需要先填任何东西就能授权的渠道，按钮排在最前面）。
		secretField.Required = false
		secretField.Help = strings.TrimSpace(secretField.Help + "。留空则用「连接」按钮授权")
		host := ""
		authURL := s.OAuth.DeviceCodeURL
		if s.OAuth.Flow == httpchan.FlowImplicit {
			authURL = s.OAuth.AuthorizeURL
		}
		if u, err := url.Parse(authURL); err == nil {
			host = u.Hostname()
		}
		oauthInfo = &OAuthInfo{
			Flow:          s.OAuth.Flow,
			ClientIDField: s.OAuth.ClientIDField,
			Scopes:        s.OAuth.Scopes,
			SetupURL:      s.OAuth.SetupURL,
			Help:          s.OAuth.Help,
			AuthHost:      host,
			Register:      s.OAuth.RegisterURL != "",
		}
		if cs := s.OAuth.ClientSecret; cs != nil {
			oauthInfo.ClientSecret = &Field{
				Name: cs.Name, Label: cs.Label, Kind: KindPassword,
				Placeholder: cs.Placeholder, Help: cs.Help,
			}
		}
	}
	fields := []Field{
		{
			Name: s.Account.Name, Label: s.Account.Label, Kind: KindText, Required: true,
			Placeholder: s.Account.Placeholder, Help: s.Account.Help,
		},
		secretField,
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
		help := "关闭时这些工具不会注册给模型：" + strings.Join(names, "、")
		if s.WriteHelp != "" {
			help = s.WriteHelp
		}
		fields = append(fields, Field{
			Name: httpchan.AllowWriteField, Label: "允许写入操作", Kind: KindBool, Default: "false",
			Help: help,
		})
	}
	for _, f := range s.Fields {
		fields = append(fields, Field{
			Name: f.Name, Label: f.Label, Kind: KindText, Required: f.Required,
			Default: f.Default, Placeholder: f.Placeholder, Help: f.Help, Advanced: f.Advanced,
		})
	}

	tools := make([]string, 0, len(s.Tools))
	toolInfo := make([]ToolMeta, 0, len(s.Tools))
	var writeTools []string
	for _, t := range s.Tools {
		tools = append(tools, t.Name)
		if !t.ReadOnly {
			writeTools = append(writeTools, t.Name)
		}
		// 标题取声明里的 title（已经是人话），说明取 description 的第一句 ——
		// 完整的 description 是写给模型的，常常好几行，列表里放不下。
		toolInfo = append(toolInfo, ToolMeta{
			Name: t.Name, Title: firstNonEmpty(t.Title, t.Name),
			Desc: firstSentence(t.Description), Write: !t.ReadOnly,
		})
	}
	writeSwitch := ""
	if len(writeTools) > 0 {
		writeSwitch = httpchan.AllowWriteField
	}

	return Descriptor{
		Type:         s.ID,
		Name:         s.Name,
		Icon:         s.Icon,
		Kind:         KindHTTP,
		Version:      s.Version,
		Tags:         s.Tags,
		Summary:      s.Summary,
		Integration:  s.Integration,
		AccountField: s.Account.Name,
		SecretField:  s.Secret.Name,
		Available:    true,
		NeedsReview:  s.PostReads,
		LocalProgram: s.Local,
		Tools:        tools,
		WriteTools:   writeTools,
		ToolInfo:     toolInfo,
		WriteSwitch:  writeSwitch,
		HelpURL:      s.HelpURL,
		OAuth:        oauthInfo,
		Fields:       fields,
	}
}

// planned 是还没实现的渠道，放在页面上只为说明「这里能长出什么」，
// 不可添加。实现一个就把它从这里挪成真的描述符 —— 声明式渠道只要往
// httpchan/catalog 里加一个 JSON 文件，不用碰这里的 Go 代码。
//
// planned 是「待实现」的占位卡片。
//
// 现在刻意为空：配置页面上只出现真实验证过的渠道。一张「待实现」的卡片对
// 用户没有价值 —— 他看到的是一个点不动的东西，而那是我们的内部状态。
//
// Version 留 0：0 表示「还没有任何一版声明」，页面上也就不显示版本角标。
var planned = []Descriptor{}

// All 返回全部渠道描述符，可用的在前。
func All() []Descriptor {
	specs := specs()
	mans := pluginManifests()
	// +2：本机邮件、飞书网页文档各一张卡片。
	out := make([]Descriptor, 0, len(mailprovider.All)+len(specs)+len(mans)+len(planned)+2)
	for _, p := range mailprovider.All {
		out = append(out, mailDescriptor(p))
	}
	out = append(out, localMailDescriptor())
	for _, s := range specs {
		out = append(out, httpDescriptor(s))
	}
	// 插件排在声明式之后：同 id 时声明式赢（ByType 取第一个命中）。一个
	// 插件不该靠占用某个 id 顶掉内置或下发的声明式渠道。
	for _, m := range mans {
		out = append(out, pluginDescriptor(m))
	}
	out = append(out, feishuWebDescriptor())
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

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// firstSentence 取一句话。
//
// 声明里的 description 是写给模型的，常常好几行、带使用时机和注意事项；
// 详情页的工具清单每条只有一行的位置，塞整段进去会把表格撑烂。
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 中英文句号都算句末；换行也算 —— 多行描述的第一行通常就是摘要。
	if i := strings.IndexAny(s, "。\n"); i >= 0 {
		s = s[:i]
	}
	const max = 40 // 超过这个长度在一行里会挤掉右边的状态徽标
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// hostPlaceholder 给没有预设主机的渠道一个像样的提示。
//
// 不写 "imap.example.com" 这种占位域名 —— 用户会照着填。写成「你公司的」
// 这种话，意思是「这里要填你自己的东西」。
func hostPlaceholder(preset, kind string) string {
	if preset != "" {
		return ""
	}
	return "例如 mail.你的公司域名 或 " + kind + ".你的公司域名"
}

// hostHelp 只在需要用户自己填的时候出现。
func hostHelp(preset string) string {
	if preset != "" {
		return ""
	}
	// 多数自建服务器就是 mail.<域名>，所以先给这条最可能对的，
	// 再说实在不知道该问谁 —— 而不是让用户在表单前卡住。
	return "大多数公司是 mail.公司域名（填完邮箱地址会自动猜一个，可以改）。" +
		"不确定就问公司 IT，或者看你现在用的邮件客户端里的「收件服务器」"
}

// addrPlaceholder 给邮箱地址字段一个提示。
//
// 预设服务商用它自己的域名（someone@qq.com 一看就懂）。通用渠道没有域名，
// 这时**不能**写 someone@example.com —— 用户会照着填一个假域名。写成
// 「你的公司邮箱」才说得清这里要填什么。
func addrPlaceholder(domains []string) string {
	if len(domains) > 0 {
		return "someone@" + domains[0]
	}
	return "你的公司邮箱地址，例如 name@公司域名"
}
