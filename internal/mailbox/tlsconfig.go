package mailbox

import (
	"crypto/tls"

	"github.com/nimysan/mcp-hub/internal/config"
)

// tlsConfigFor 造 IMAP / SMTP 用的 TLS 配置。
//
// 默认那条路就是 Go 的默认套件：只有 ECDHE 和 TLS 1.3，都有前向保密。
//
// LegacyRSAKeyExchange 那条路是给**只提供静态 RSA 密钥交换**的老服务器开的。
// Go 从 1.22 起把 TLS_RSA_* 从默认列表里移除了，理由就是没有前向保密：服务器
// 私钥哪天泄露，攻击者拿历史抓包就能把以前的邮件全部解出来。别的 TLS 栈
// （OpenSSL、Apple、Schannel）至今仍默认接受它，所以同一台服务器用手机和
// Outlook 连得上，而 Go 连不上 —— 实测 mail.gotechcn.cn 就只接受 8 个套件，
// 全是静态 RSA，一个 ECDHE 都没有。
//
// 这个放宽**按渠道生效，不是全局**：一台老服务器不该把所有渠道一起降级。
// 哪个渠道需要它，写在 mailprovider 的那张表里，并且在页面上说明代价。
func tlsConfigFor(host string, legacyRSA bool) *tls.Config {
	c := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if !legacyRSA {
		return c
	}
	// 显式列出来，而不是靠 GODEBUG=tlsrsakex=1：环境变量是进程级的，会把
	// 所有连接一起放宽，而这里只影响这一个渠道的这一次连接。
	//
	// 仍然保留 Go 的默认套件在前：服务器要是哪天加上了 ECDHE，就会自动用上
	// 更好的那条，不需要再改代码。
	c.CipherSuites = append(defaultSuites(),
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	)
	return c
}

// defaultSuites 是 Go 当前默认启用的 TLS 1.2 套件（全部带前向保密）。
//
// 用 tls.CipherSuites() 动态取，而不是手抄一份：Go 调整默认集时这里自动
// 跟上，不会悄悄落后。
func defaultSuites() []uint16 {
	var out []uint16
	for _, s := range tls.CipherSuites() {
		out = append(out, s.ID)
	}
	return out
}

// imapTLS / smtpTLS 是两个调用点的入口，host 取各自的主机名 —— ServerName
// 必须和实际连的那台一致，否则证书校验会失败。
func imapTLS(cfg config.Config) *tls.Config {
	return tlsConfigFor(cfg.IMAPHost, cfg.LegacyRSAKeyExchange)
}

func smtpTLS(cfg config.Config) *tls.Config {
	return tlsConfigFor(cfg.SMTPHost, cfg.LegacyRSAKeyExchange)
}
