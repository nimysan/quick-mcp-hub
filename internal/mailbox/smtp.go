package mailbox

import (
	"context"
	"fmt"
	"log"
	"strings"

	gomail "github.com/wneessen/go-mail"

	"github.com/nimysan/mcp-hub/internal/config"
)

// Version 由构建时的 -ldflags 注入，缺省为 dev。
var Version = "dev"

// Logf 统一走 stderr。stdio 传输下 stdout 是 JSON-RPC 协议通道，
// 往那里写一个字节的日志就会撞坏握手。
// Go 标准库 log 默认就写 stderr，这里只是把入口收拢到一处。
func Logf(format string, args ...any) { log.Printf(format, args...) }

func logf(format string, args ...any) { Logf(format, args...) }

// SendRequest 是 send_mail 的入参。
type SendRequest struct {
	To      []string
	Cc      []string
	Subject string
	Body    string
	HTML    bool
}

// smtpOptions 组装 SMTP 客户端选项。授权码在这里第一次被取出（惰性）。
func smtpOptions(cfg config.Config) ([]gomail.Option, error) {
	return smtpOptionsWith(cfg, cfg.LegacyRSAKeyExchange)
}

// smtpOptionsWith 和 smtpOptions 一样，但由调用方决定 TLS 要不要放宽。
// 自动降级重试需要这个口子：第二次尝试用的是放宽后的套件集合。
func smtpOptionsWith(cfg config.Config, legacy bool) ([]gomail.Option, error) {
	password, err := cfg.Password()
	if err != nil {
		return nil, err
	}
	opts := []gomail.Option{
		gomail.WithPort(cfg.SMTPPort),
		gomail.WithUsername(cfg.User),
		gomail.WithPassword(password),
		gomail.WithSMTPAuth(gomail.SMTPAuthLogin),
		gomail.WithTimeout(cfg.Timeout),
		gomail.WithTLSConfig(tlsConfigFor(cfg.SMTPHost, legacy)),
	}
	// 465 是 implicit TLS（连上就是 TLS）；其他端口走 STARTTLS 且强制要求加密。
	if cfg.SMTPPort == 465 {
		opts = append(opts, gomail.WithSSL())
	} else {
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}
	return opts, nil
}

// withSMTPFallback 跑一次 SMTP 操作，握手谈不拢时用放宽的套件重试一次。
//
// IMAP 那侧早就有这个（见 imap.go），SMTP 侧没有 —— 于是同一台只给静态 RSA
// 的服务器，收信能自动降级连上，发信却直接失败。两边必须对齐。
//
// 为什么把整个操作包起来重试，而不是只重拨一次连接：gomail 自己管拨号，
// 连接拿不出来单独复用。好在 handshakeFailed 只认握手阶段的失败 —— 那一刻
// 还没有发出任何邮件，所以重试一次不会把信发两遍。
func withSMTPFallback(cfg config.Config, run func(opts []gomail.Option) error) error {
	opts, err := smtpOptionsWith(cfg, cfg.LegacyRSAKeyExchange)
	if err != nil {
		return err
	}
	err = run(opts)
	if err == nil || cfg.LegacyRSAKeyExchange || !handshakeFailed(err) {
		return err
	}
	// 降级这件事绝不能悄悄发生：这条连接没有前向保密。
	logf("SMTP %s 不接受默认加密套件，降级重试（这条连接无前向保密）", cfg.SMTPHost)
	relaxed, err := smtpOptionsWith(cfg, true)
	if err != nil {
		return err
	}
	return run(relaxed)
}

// Send 发送一封邮件。
//
// 用 465 直连 SSL（implicit TLS）：587 的 STARTTLS 在部分网络不可达。
func Send(ctx context.Context, cfg config.Config, req SendRequest) error {
	if len(req.To) == 0 {
		return fmt.Errorf("收件人不能为空")
	}
	if strings.TrimSpace(req.Subject) == "" {
		return fmt.Errorf("主题不能为空")
	}

	msg := gomail.NewMsg(gomail.WithCharset(gomail.CharsetUTF8))
	if err := msg.From(cfg.User); err != nil {
		return fmt.Errorf("发件人地址 %q 无效: %w", cfg.User, err)
	}
	if err := msg.To(req.To...); err != nil {
		return fmt.Errorf("收件人地址无效: %w", err)
	}
	if len(req.Cc) > 0 {
		if err := msg.Cc(req.Cc...); err != nil {
			return fmt.Errorf("抄送地址无效: %w", err)
		}
	}
	msg.Subject(req.Subject)
	ct := gomail.TypeTextPlain
	if req.HTML {
		ct = gomail.TypeTextHTML
	}
	msg.SetBodyString(ct, req.Body)

	err := withSMTPFallback(cfg, func(opts []gomail.Option) error {
		client, err := gomail.NewClient(cfg.SMTPHost, opts...)
		if err != nil {
			return fmt.Errorf("构建 SMTP 客户端失败: %w", err)
		}
		return client.DialAndSendWithContext(ctx, msg)
	})
	if err != nil {
		return loginError("SMTP", cfg, err)
	}
	return nil
}

// SelfTest 分别验证 IMAP 登录和 SMTP 认证，供 CLI 的 -selftest 使用。
// 它会强制取出凭据（因此会触发钥匙串授权），但不发送任何邮件。
func SelfTest(ctx context.Context, cfg config.Config) (imapErr, smtpErr error) {
	if s, err := connect(cfg); err != nil {
		imapErr = err
	} else {
		if _, err := s.selectBox("INBOX"); err != nil {
			imapErr = err
		}
		s.close()
	}

	err := withSMTPFallback(cfg, func(opts []gomail.Option) error {
		client, err := gomail.NewClient(cfg.SMTPHost, opts...)
		if err != nil {
			return fmt.Errorf("构建 SMTP 客户端失败: %w", err)
		}
		if err := client.DialWithContext(ctx); err != nil {
			return err
		}
		if err := client.Close(); err != nil {
			logf("关闭 SMTP 连接: %v", err)
		}
		return nil
	})
	if err != nil {
		// 刻意不再写死「SMTP 认证失败」。实测这里拿到的常常根本不是认证错误：
		// 静态 RSA 的 Postfix 在默认套件握不上手时直接关连接，Go 报裸 EOF ——
		// 连 TLS 都没建起来，和认证毫无关系，而原来那句标签把用户引向密码。
		// loginError 会分清这两种，并把服务器的原话记进日志。
		return imapErr, loginError("SMTP", cfg, err)
	}
	return imapErr, nil
}
