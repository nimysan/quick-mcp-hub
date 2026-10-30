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
		// 通用的企业邮箱：主机和端口由用户填。
		//
		// 不绑定任何域名、不预置任何厂商的主机 —— 自建邮件服务器（Postfix +
		// Dovecot 这类）没有「服务商」可言，每家的主机名都不一样，写死一家
		// 等于只服务一家。页面上就是一张「企业邮箱」卡，客户填自己公司的
		// 那两个地址。
		//
		// 端口给 993 / 465 的默认值：这两个是 IMAPS 和 SMTPS 的标准端口，
		// 绝大多数服务器就是它们，填错了也能在同一个表单里改。
		//
		// 这里刻意**不开** LegacyRSAKeyExchange。老服务器只给静态 RSA 套件
		// 这件事是真实存在的（实测有这样的服务器），但它不该由一张静态表来
		// 断言 —— 通用渠道面对的是任意一台服务器。改成握手失败时自动降级
		// 重试一次（见 internal/mailbox/tlsconfig.go 的 handshakeFailed），
		// 这样默认连上的都是有前向保密的那条路，只有确实谈不拢的那台才降级，
		// 而且降级会报出来。
		Type: "custommail", Name: "企业邮箱", Icon: "📧",
		Domains:  nil,
		IMAPHost: "", IMAPPort: 993,
		SMTPHost: "", SMTPPort: 465,
		// 自建服务器通常没有「授权码」这个概念，填的就是登录密码本身 ——
		// 这件事必须说清楚：它不能单独吊销，和 QQ 的授权码性质不同。
		SecretLabel: "邮箱密码",
		SecretHelp: "就是你登录这个邮箱用的密码。自建邮件服务器一般没有「授权码」机制，" +
			"所以它不能像授权码那样单独吊销——这台机器上保存的就是你的真实密码，" +
			"它只进系统密码管理器，不写配置文件、不进日志。",
	},
}

// hostHints 是域名到接入点的提示表，只用于**自动填写**，不在界面上露出。
//
// 为什么不做成渠道卡片：一个客户的自建邮箱不该在所有人的配置页上占一张卡。
// 但「我公司邮箱的 IMAP 主机叫什么」又是财务人员答不出的问题，所以这里留一张
// 提示表：填了邮箱地址之后主机自动填好，仍然可改。
//
// 命名约定（mail./imap./smtp. + 域名）覆盖了大多数自建服务器，所以兜底就是
// 按约定猜，而不是留空让用户去问 IT。
var hostHints = map[string]struct{ IMAP, SMTP string }{
	// 实测过的：MX 指向 221.4.223.109，Dovecot 的 IMAP 在 993，
	// Postfix 的 SMTP 在 465，证书 CN=mail.gotechcn.cn。
	"gotechcn.cn": {IMAP: "mail.gotechcn.cn", SMTP: "mail.gotechcn.cn"},
}

// GuessHosts 按邮箱地址猜 IMAP / SMTP 主机，供表单自动填写。
//
// 先查实测过的提示表，查不到就按 mail.<域名> 这个最常见的约定猜。猜错的代价
// 很小（用户在同一个表单里改掉），猜对的收益很大（不用去问 IT）。
func GuessHosts(addr string) (imapHost, smtpHost string) {
	i := strings.LastIndex(addr, "@")
	if i < 0 || i == len(addr)-1 {
		return "", ""
	}
	domain := strings.ToLower(strings.TrimSpace(addr[i+1:]))
	if h, ok := hostHints[domain]; ok {
		return h.IMAP, h.SMTP
	}
	return "mail." + domain, "mail." + domain
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
