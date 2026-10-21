package mailbox

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"strings"

	gomail "github.com/wneessen/go-mail"

	"github.com/nimysan/qqmail-mcp/internal/config"
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

// Send 发送一封邮件。
//
// 用 465 直连 SSL（implicit TLS）：587 的 STARTTLS 在部分网络不可达。
// 端口由配置决定，非 465 时走 STARTTLS 策略。
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

	opts := []gomail.Option{
		gomail.WithPort(cfg.SMTPPort),
		gomail.WithUsername(cfg.User),
		gomail.WithPassword(cfg.Password),
		gomail.WithSMTPAuth(gomail.SMTPAuthLogin),
		gomail.WithTimeout(cfg.Timeout),
		gomail.WithTLSConfig(&tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12}),
	}
	if cfg.SMTPPort == 465 {
		opts = append(opts, gomail.WithSSL())
	} else {
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}

	client, err := gomail.NewClient(cfg.SMTPHost, opts...)
	if err != nil {
		return fmt.Errorf("构建 SMTP 客户端失败: %w", err)
	}
	if err := client.DialAndSendWithContext(ctx, msg); err != nil {
		return fmt.Errorf("发信失败（%s:%d，QQ/163 需使用 16 位授权码）: %w", cfg.SMTPHost, cfg.SMTPPort, err)
	}
	return nil
}

// SelfTest 分别验证 IMAP 登录和 SMTP 认证，供 CLI 的 -selftest 使用。
// 它不发送任何邮件。
func SelfTest(ctx context.Context, cfg config.Config) (imapErr, smtpErr error) {
	if s, err := connect(cfg); err != nil {
		imapErr = err
	} else {
		if _, err := s.selectBox("INBOX"); err != nil {
			imapErr = err
		}
		s.close()
	}

	opts := []gomail.Option{
		gomail.WithPort(cfg.SMTPPort),
		gomail.WithUsername(cfg.User),
		gomail.WithPassword(cfg.Password),
		gomail.WithSMTPAuth(gomail.SMTPAuthLogin),
		gomail.WithTimeout(cfg.Timeout),
		gomail.WithTLSConfig(&tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12}),
	}
	if cfg.SMTPPort == 465 {
		opts = append(opts, gomail.WithSSL())
	} else {
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}
	client, err := gomail.NewClient(cfg.SMTPHost, opts...)
	if err != nil {
		return imapErr, fmt.Errorf("构建 SMTP 客户端失败: %w", err)
	}
	if err := client.DialWithContext(ctx); err != nil {
		return imapErr, fmt.Errorf("SMTP 认证失败: %w", err)
	}
	if err := client.Close(); err != nil {
		logf("关闭 SMTP 连接: %v", err)
	}
	return imapErr, nil
}
