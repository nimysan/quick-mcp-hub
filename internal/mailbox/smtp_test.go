package mailbox

import (
	"errors"
	"io"
	"testing"

	gomail "github.com/wneessen/go-mail"

	"github.com/nimysan/mcp-hub/internal/config"
)

func smtpTestConfig(legacy bool) config.Config {
	return config.FromMailChannel(config.MailChannelInput{
		User:                 "a@example.com",
		SMTPHost:             "mail.example.com",
		SMTPPort:             465,
		Secret:               fixedSecret("pw"),
		LegacyRSAKeyExchange: legacy,
	})
}

// SMTP 必须和 IMAP 一样会自动降级，而且只在握手谈不拢时降级。
//
// 真实故障：mail.gotechcn.cn 上 IMAP 自动降级连上了，SMTP 却报
// 「SMTP 认证失败: EOF」—— 因为降级重试只接在 IMAP 那一侧（imap.go），
// SMTP 这边直接一次 Dial 失败就结束。实测证据：该服务器 465 端口用默认套件
// 握手失败（裸 EOF），放宽静态 RSA 后握手成功并回 Postfix banner。
func TestSMTPFallbackRetriesOnlyOnHandshakeFailure(t *testing.T) {
	// 握手失败 -> 降级重试一次 -> 这次通了。这是 gotechcn 的真实形状。
	t.Run("握手失败后重试一次就成功", func(t *testing.T) {
		calls := 0
		err := withSMTPFallback(smtpTestConfig(false), func([]gomail.Option) error {
			calls++
			if calls == 1 {
				return io.EOF
			}
			return nil
		})
		if err != nil {
			t.Errorf("降级后应当成功，得到 %v", err)
		}
		if calls != 2 {
			t.Errorf("应当尝试 2 次（原始 + 降级），实际 %d 次", calls)
		}
	})

	// 只重试一次。无限降级重试会把一次失败变成一串超时等待。
	t.Run("降级后仍失败就不再试", func(t *testing.T) {
		calls := 0
		err := withSMTPFallback(smtpTestConfig(false), func([]gomail.Option) error {
			calls++
			return io.EOF
		})
		if err == nil {
			t.Error("两次都失败时应当返回错误")
		}
		if calls != 2 {
			t.Errorf("最多尝试 2 次，实际 %d 次", calls)
		}
	})

	// 认证失败重试一次毫无意义 —— 密码不会因为换了套件就变对。
	t.Run("非握手失败不重试", func(t *testing.T) {
		for _, bad := range []error{
			errors.New("535 5.7.8 Error: authentication failed"),
			errors.New("dial tcp: i/o timeout"),
			errors.New("x509: certificate has expired or is not yet valid"),
		} {
			calls := 0
			_ = withSMTPFallback(smtpTestConfig(false), func([]gomail.Option) error {
				calls++
				return bad
			})
			if calls != 1 {
				t.Errorf("%v：不该重试，实际尝试 %d 次", bad, calls)
			}
		}
	})

	t.Run("成功就不重试", func(t *testing.T) {
		calls := 0
		if err := withSMTPFallback(smtpTestConfig(false), func([]gomail.Option) error {
			calls++
			return nil
		}); err != nil {
			t.Errorf("不该出错：%v", err)
		}
		if calls != 1 {
			t.Errorf("成功时只该尝试 1 次，实际 %d 次", calls)
		}
	})

	// 渠道本来就配了放宽：第一次用的已经是放宽后的套件，再试一次是纯浪费。
	t.Run("渠道已放宽时不重试", func(t *testing.T) {
		calls := 0
		_ = withSMTPFallback(smtpTestConfig(true), func([]gomail.Option) error {
			calls++
			return io.EOF
		})
		if calls != 1 {
			t.Errorf("已放宽的渠道不该再降级重试，实际尝试 %d 次", calls)
		}
	})
}
