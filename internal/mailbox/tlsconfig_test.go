package mailbox

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/nimysan/mcp-hub/internal/config"
)

// 默认那条路**绝不能**带上静态 RSA 的套件：那些套件没有前向保密，
// 服务器私钥泄露后历史抓包就能全部解密。Go 从 1.22 起默认不要它们，
// 我们不该把这个决定整体翻过来。
func TestDefaultTLSHasNoStaticRSA(t *testing.T) {
	c := tlsConfigFor("mail.example.com", false)
	if c.ServerName != "mail.example.com" {
		t.Errorf("ServerName = %q", c.ServerName)
	}
	if c.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x，不该低于 TLS 1.2", c.MinVersion)
	}
	// 不设 CipherSuites 就是用 Go 的默认集，那里面没有静态 RSA。
	if c.CipherSuites != nil {
		for _, id := range c.CipherSuites {
			if isStaticRSA(id) {
				t.Errorf("默认配置里出现了静态 RSA 套件: %s", tls.CipherSuiteName(id))
			}
		}
	}
	if c.InsecureSkipVerify {
		t.Error("绝不能跳过证书校验")
	}
}

// 放宽那条路要真的把静态 RSA 加进去，否则对只提供它的服务器仍然连不上。
func TestLegacyTLSAddsStaticRSAButKeepsForwardSecrecyFirst(t *testing.T) {
	c := tlsConfigFor("mail.gotechcn.cn", true)
	if len(c.CipherSuites) == 0 {
		t.Fatal("放宽时必须显式给出套件列表")
	}
	if c.InsecureSkipVerify {
		t.Error("放宽密钥交换不等于放弃证书校验")
	}
	if c.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x，放宽套件不该顺带降低协议版本", c.MinVersion)
	}

	var firstRSA, lastFS = -1, -1
	for i, id := range c.CipherSuites {
		if isStaticRSA(id) {
			if firstRSA < 0 {
				firstRSA = i
			}
		} else {
			lastFS = i
		}
	}
	if firstRSA < 0 {
		t.Fatal("没有加上任何静态 RSA 套件，只提供它的服务器仍然连不上")
	}
	// 顺序要紧：带前向保密的排在前面，服务器哪天加上 ECDHE 就会自动走更好的
	// 那条，不需要再改代码。
	if lastFS > firstRSA {
		t.Errorf("静态 RSA 排在了前向保密套件之前（首个 RSA 在 %d，最后一个 FS 在 %d）", firstRSA, lastFS)
	}
}

// 两个入口各用自己的主机名：ServerName 和实际连的那台不一致会导致证书校验失败。
func TestImapAndSmtpUseTheirOwnHostnames(t *testing.T) {
	cfg := config.FromMailChannel(config.MailChannelInput{
		User:                 "a@example.com",
		IMAPHost:             "imap.example.com",
		SMTPHost:             "smtp.example.com",
		LegacyRSAKeyExchange: true,
	})
	if got := imapTLS(cfg).ServerName; got != "imap.example.com" {
		t.Errorf("imapTLS ServerName = %q", got)
	}
	if got := smtpTLS(cfg).ServerName; got != "smtp.example.com" {
		t.Errorf("smtpTLS ServerName = %q", got)
	}
	// 标志必须真的传到了这一层，否则放宽等于没做。
	if len(imapTLS(cfg).CipherSuites) == 0 {
		t.Error("LegacyRSAKeyExchange 没有传到 imapTLS")
	}
	if len(smtpTLS(cfg).CipherSuites) == 0 {
		t.Error("LegacyRSAKeyExchange 没有传到 smtpTLS")
	}
	// 不开标志时不该有任何放宽。
	plain := config.FromMailChannel(config.MailChannelInput{IMAPHost: "imap.example.com"})
	if imapTLS(plain).CipherSuites != nil {
		t.Error("没开标志却改了套件列表")
	}
}

func isStaticRSA(id uint16) bool {
	switch id {
	case tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_RSA_WITH_RC4_128_SHA:
		return true
	}
	return false
}

// 裸 EOF 也必须算握手失败，否则 SMTP 永远触发不了自动降级。
//
// 真实故障：mail.gotechcn.cn 上 IMAP 能自动降级、SMTP 不能。同一台机器，
// 两个守护进程脾气不同 —— Dovecot(993) 谈不拢时回 alert 40「handshake
// failure」，Postfix(465) 直接关掉连接，Go 只剩一个裸 EOF。于是用户看到
// 「SMTP 认证失败: EOF」，而它连 TLS 都没建起来，和认证毫无关系。
//
// 在拨号阶段判 EOF 是安全的：那一刻还没读过任何应用数据，EOF 只可能是
// 对方在握手中途关了连接。
func TestHandshakeFailedRecognisesBareEOF(t *testing.T) {
	t.Run("该认的", func(t *testing.T) {
		for _, err := range []error{
			io.EOF,
			io.ErrUnexpectedEOF,
			fmt.Errorf("dial tcp 1.2.3.4:465: %w", io.EOF), // 被包过一层
			errors.New("EOF"), // 重新造的，errors.Is 认不出
			errors.New("smtp: connect error: EOF"),
			errors.New("remote error: tls: handshake failure"),
			errors.New("tls: no cipher suite supported by both client and server"),
		} {
			if !handshakeFailed(err) {
				t.Errorf("要当成握手失败：%v", err)
			}
		}
	})

	// 边界：别的失败重试一次毫无意义，只会把一次等待变成两次。
	// 尤其 EOF 的判断不能宽到把超时、证书错、密码错也卷进来。
	t.Run("不该认的", func(t *testing.T) {
		for _, err := range []error{
			nil,
			errors.New("dial tcp 1.2.3.4:465: i/o timeout"),
			errors.New("x509: certificate has expired or is not yet valid"),
			errors.New("x509: cannot validate certificate for 221.4.223.109 because it doesn't contain any IP SANs"),
			errors.New("imap: NO [AUTHENTICATIONFAILED] Authentication failed."),
			errors.New("535 5.7.8 Error: authentication failed"),
			errors.New("connection refused"),
			// 「EOF」出现在中间而不是结尾：不是拨号阶段那种对方直接断开。
			errors.New("EOF reached while reading message body"),
		} {
			if handshakeFailed(err) {
				t.Errorf("不该当成握手失败：%v", err)
			}
		}
	})
}
