// Package mailprovider 是一张邮件服务商接入点表，没有任何项目内依赖。
//
// 独立成叶子包是为了让 config 和 channels 都能用它而不互相引用：
// config 要用它推断默认接入点，channels 要用它生成渠道选择列表。
package mailprovider

import "strings"

// Provider 是一家邮件服务商的默认接入点。
type Provider struct {
	// Type 是渠道类型 id，写进配置文件，不随显示名变化。
	Type string
	// Name 是界面上的显示名。
	Name string
	// Icon 是界面上的图标（emoji，避免引入图标资源）。
	Icon string
	// Domains 是归属这家服务商的邮箱域名。
	Domains []string

	IMAPHost string
	IMAPPort int
	SMTPHost string
	// SMTPPort 默认取 465 直连 SSL：587(STARTTLS) 在部分网络/出口上不可达，
	// 而 465 在实测中稳定可用。
	SMTPPort int

	// SecretLabel 是界面上密码字段的叫法。各家不同：QQ/网易叫「授权码」，
	// Gmail 叫「应用专用密码」，写错会让人拿登录密码去试。
	SecretLabel string
	// SecretHelp 告诉用户去哪里生成这个凭据。
	SecretHelp string
}

// All 是支持的服务商，顺序即界面上的展示顺序。
var All = []Provider{
	{
		Type: "qqmail", Name: "QQ 邮箱", Icon: "📧",
		Domains:  []string{"qq.com", "foxmail.com"},
		IMAPHost: "imap.qq.com", IMAPPort: 993,
		SMTPHost: "smtp.qq.com", SMTPPort: 465,
		SecretLabel: "授权码",
		SecretHelp:  "邮箱设置 → 账户 → 开启 IMAP/SMTP 服务 → 生成授权码（16 位，不是登录密码）",
	},
	{
		Type: "mail163", Name: "网易 163", Icon: "📧",
		Domains:  []string{"163.com"},
		IMAPHost: "imap.163.com", IMAPPort: 993,
		SMTPHost: "smtp.163.com", SMTPPort: 465,
		SecretLabel: "授权码",
		SecretHelp:  "设置 → POP3/SMTP/IMAP → 开启服务 → 新增授权密码",
	},
	{
		Type: "mail126", Name: "网易 126", Icon: "📧",
		Domains:  []string{"126.com"},
		IMAPHost: "imap.126.com", IMAPPort: 993,
		SMTPHost: "smtp.126.com", SMTPPort: 465,
		SecretLabel: "授权码",
		SecretHelp:  "设置 → POP3/SMTP/IMAP → 开启服务 → 新增授权密码",
	},
	{
		Type: "gmail", Name: "Gmail", Icon: "📧",
		Domains:  []string{"gmail.com", "googlemail.com"},
		IMAPHost: "imap.gmail.com", IMAPPort: 993,
		SMTPHost: "smtp.gmail.com", SMTPPort: 465,
		SecretLabel: "应用专用密码",
		SecretHelp:  "Google 账号 → 安全性 → 两步验证 → 应用专用密码",
	},
	{
		Type: "outlook", Name: "Outlook", Icon: "📧",
		Domains:  []string{"outlook.com", "hotmail.com", "live.com"},
		IMAPHost: "outlook.office365.com", IMAPPort: 993,
		SMTPHost: "smtp.office365.com", SMTPPort: 465,
		SecretLabel: "应用密码",
		SecretHelp:  "Microsoft 账号 → 安全信息 → 应用密码",
	},
}

// Default 是识别不出域名时的兜底（按 QQ 的接入点，并在名字里说明）。
func Default() Provider {
	p := All[0]
	p.Name = "未知服务商（按 QQ 默认值，请自行核对主机和端口）"
	return p
}

// ByType 按渠道类型 id 查。
func ByType(t string) (Provider, bool) {
	for _, p := range All {
		if p.Type == t {
			return p, true
		}
	}
	return Provider{}, false
}

// ByAddress 按邮箱地址的域名猜服务商。
func ByAddress(addr string) (Provider, bool) {
	i := strings.LastIndex(addr, "@")
	if i < 0 {
		return Provider{}, false
	}
	domain := strings.ToLower(addr[i+1:])
	for _, p := range All {
		for _, d := range p.Domains {
			if d == domain {
				return p, true
			}
		}
	}
	return Provider{}, false
}
