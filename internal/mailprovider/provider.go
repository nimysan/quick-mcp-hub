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

	// LegacyRSAKeyExchange 为真时，连这家服务商要额外允许静态 RSA 密钥交换的
	// TLS 套件（见 internal/mailbox/tlsconfig.go）。
	//
	// 只给**只提供**静态 RSA 的老服务器开，代价是那条连接没有前向保密：服务器
	// 私钥哪天泄露，历史抓包就能解密。Go 从 1.22 起默认不要这类套件，而别的
	// TLS 栈（OpenSSL / Apple / Schannel）至今还接受 —— 所以会出现「手机和
	// Outlook 连得上，这个程序连不上」的情形。
	//
	// 置为 true 之前请把服务器实际接受的套件列一遍确认过，并在 SecretHelp 或
	// 卡片说明里把代价写出来。能让对方加上 ECDHE 就不该用这个开关。
	LegacyRSAKeyExchange bool
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
	{
		// GoTech 的自建邮件服务器：Postfix + Dovecot，不是 Exchange。
		// 探测结论（2026-10-30）：
		//   MX      mail.gotechcn.cn -> 221.4.223.109（SPF 只授权这一个 IP，-all）
		//   SMTP    220 mail.gotechcn.cn ESMTP Postfix，25/465/587 开
		//   IMAP    * OK [CAPABILITY IMAP4rev1 ... AUTH=PLAIN AUTH=LOGIN] Dovecot ready.
		//           993 开，143 关（不给明文 IMAP）
		//   TLS     只接受 8 个套件，全是静态 RSA；没有 ECDHE、没有 TLS 1.3
		//   证书    CN=mail.gotechcn.cn，DigiCert DV
		// autodiscover.gotechcn.cn 虽然解析到同一个 IP，但那是个幌子 ——
		// Postfix/Dovecot 不做 Exchange 的 autodiscover，所以这是普通 IMAP/SMTP
		// 渠道，不需要 EWS 或 Graph。
		Type: "gotechmail", Name: "GoTech 邮箱", Icon: "📧",
		Domains:  []string{"gotechcn.cn"},
		IMAPHost: "mail.gotechcn.cn", IMAPPort: 993,
		SMTPHost: "mail.gotechcn.cn", SMTPPort: 465,
		// Dovecot/Postfix 没有「授权码」这个概念，填的就是登录密码本身 ——
		// 这件事必须说清楚：它不能单独吊销，和 QQ 的授权码性质不同。
		SecretLabel: "邮箱密码",
		SecretHelp: "就是你登录这个邮箱用的密码（这台服务器没有「授权码」机制）。" +
			"注意它不能像授权码那样单独吊销，所以这台机器上保存的就是你的真实密码——" +
			"它只进系统密码管理器，不写配置文件、不进日志。",
		// 这台服务器只提供静态 RSA 密钥交换，Go 默认连不上。实测：默认设置
		// handshake failure，显式启用静态 RSA 套件后才通。代价是这条连接没有
		// 前向保密。正解是让对方在 Dovecot/Postfix 上加 ECDHE，加上之后这个
		// 开关会自动失效（默认套件排在前面，服务器支持就会优先用）。
		LegacyRSAKeyExchange: true,
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
