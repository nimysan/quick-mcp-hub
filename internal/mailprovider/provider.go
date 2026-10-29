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
//
// 只留真实验证过的那一个。163 / 126 / Gmail / Outlook 走的是**完全相同**的
// IMAP/SMTP 实现，只是主机和授权码入口不同，所以它们极可能本来就能用 ——
// 但「极可能」不是「验证过」，而配置页面上出现一个没连通过的卡片，代价是
// 用户照着它走到一半卡住。验证过一个就把它加回来，这里加一条即可。
//
// 注意 internal/config/config.go 里还有一张域名→主机的映射表，仍然认
// gmail.com / outlook.com：那是环境变量那条老路（MCPHUB_MAIL_USER）的
// 自动识别，和配置页面无关，刻意保留 —— 删掉会让已经这么配的客户端直接坏掉。
var All = []Provider{
	{
		Type: "qqmail", Name: "QQ 邮箱", Icon: "📧",
		Domains:  []string{"qq.com", "foxmail.com"},
		IMAPHost: "imap.qq.com", IMAPPort: 993,
		SMTPHost: "smtp.qq.com", SMTPPort: 465,
		SecretLabel: "授权码",
		SecretHelp:  "QQ邮箱网页版 → 设置 → 账号与安全 → 安全设置 → 开启服务（会收一条短信）→ 生成授权码（16 位小写字母，不是 QQ 登录密码）",
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
