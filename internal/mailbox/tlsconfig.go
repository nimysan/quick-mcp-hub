package mailbox

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"

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

// handshakeFailed 判断这次失败是不是 TLS 握手谈不拢（而不是网络不通、
// 证书不对或者密码错）。
//
// 这是自动降级的触发条件：只有「双方一个共同套件都没有」才值得降级重试，
// 别的失败重试一次毫无意义，还会把一次失败变成两次等待。
//
// 按错误文本判断是难看的，但 crypto/tls 没有把 alert 码作为可判定错误暴露
// 出来（没有可用的哨兵错误或类型），所以只能这样。字符串来自 Go 自己产生的
// 错误和服务器回的 alert，列全了实际会出现的几种。
func handshakeFailed(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, probe := range []string{
		"handshake failure",                  // 服务器回的 alert 40，最常见的那个
		"no cipher suite supported",          // Go 侧先判出来没有交集
		"server selected unsupported",        // 服务器挑了个我们没启用的
		"protocol version not supported",     // 老服务器只有 TLS 1.0/1.1
		"tls: server does not support TLS 1", // 同上，另一种措辞
	} {
		if strings.Contains(s, probe) {
			return true
		}
	}

	// 裸 EOF：服务器**不回 alert，直接关掉连接**。
	//
	// 不是假设，是实测：mail.gotechcn.cn 的 Dovecot(993) 谈不拢时规规矩矩回
	// alert 40（所以 IMAP 一直能自动降级），而同一台机器的 Postfix(465) 直接
	// 断开，Go 只能报一个裸 EOF。于是 SMTP 永远触发不了降级，用户看到的是
	// 「SMTP 认证失败: EOF」—— 它连 TLS 都没建起来，和认证毫无关系。
	//
	// 为什么在这里判 EOF 是安全的：这个函数只有两个调用点，都在**拨号阶段**
	// （dialWithFallback 和 imap.go 的 DialTLS 之后），那一刻还没有读过任何
	// 应用数据，所以 EOF 只可能是「对方在握手中途把连接关了」。换到读写阶段
	// 判 EOF 就不成立了 —— 那里 EOF 是正常的流结束。
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// 有的库会把 EOF 重新包成不可 errors.Is 的错误，只剩文本。
	return s == "EOF" || strings.HasSuffix(s, ": EOF")
}

// dialWithFallback 先按安全的默认套件连，握手谈不拢时降级重试一次。
//
// 为什么要有这个：有些自建邮件服务器（实测存在）只提供静态 RSA 密钥交换的
// TLS 套件。Go 从 1.22 起把这类套件从默认集里移除了，理由是没有前向保密，
// 而 OpenSSL / Apple / Schannel 至今仍接受 —— 于是出现「Foxmail 和手机连
// 得上，这个程序连不上」。
//
// 以前的做法是在服务商表里给那台服务器写死一个开关。通用的「企业邮箱」渠道
// 做不到这件事：它面对的是任意一台服务器，而「你的服务器支不支持 ECDHE」是
// 用户答不出的问题。所以改成实测：默认那条路先试，只有确实谈不拢的那台才
// 降级，并且把降级这件事**报出来**（downgraded 返回值）而不是悄悄做掉。
func dialWithFallback(d *net.Dialer, host, addr string, legacy bool) (c *tls.Conn, downgraded bool, err error) {
	c, err = tls.DialWithDialer(d, "tcp", addr, tlsConfigFor(host, legacy))
	if err == nil || legacy || !handshakeFailed(err) {
		return c, false, err
	}
	// 谈不拢，用放宽的套件再试一次。
	c, err = tls.DialWithDialer(d, "tcp", addr, tlsConfigFor(host, true))
	if err != nil {
		return nil, false, err
	}
	return c, true, nil
}
