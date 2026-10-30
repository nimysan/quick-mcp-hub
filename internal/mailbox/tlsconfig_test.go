package mailbox

import (
	"crypto/tls"
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
