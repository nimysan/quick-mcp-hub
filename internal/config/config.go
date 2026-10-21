// Package config 从环境变量和系统密码管理器装配运行配置。
//
// 约定：所有日志必须走 stderr。stdio 传输下 stdout 是 JSON-RPC 协议通道，
// 任何非 JSON 字节都会让客户端握手失败（Amazon Quick 报 handshake_failed）。
//
// 凭据不在本结构里以明文长驻：Config 只持有 credstore 的惰性句柄，
// 第一次真正需要时才去系统密码管理器取。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nimysan/qqmail-mcp/internal/credstore"
)

// Namespace 是写进系统密码管理器的 service 前缀。
const Namespace = "qqmail-mcp"

// IntegrationMail 是邮件集成的名字。后续接别的系统时，各自用自己的常量，
// 凭据在钥匙串里按 "qqmail-mcp:<集成名>" 分段存放。
const IntegrationMail = "mail"

// Provider 是一家邮件服务商的默认接入点。
type Provider struct {
	Name     string
	IMAPHost string
	IMAPPort int
	SMTPHost string
	SMTPPort int // 直连 SSL 端口（implicit TLS）
}

// providers 按邮箱域名给出默认接入点。
// SMTP 一律用 465 直连 SSL：587(STARTTLS) 在部分网络/出口上不可达，
// 而 465 在实测中稳定可用。需要 587 时用 QQMAIL_SMTP_PORT 显式覆盖。
var providers = map[string]Provider{
	"qq.com":      {"QQ邮箱", "imap.qq.com", 993, "smtp.qq.com", 465},
	"foxmail.com": {"Foxmail", "imap.qq.com", 993, "smtp.qq.com", 465},
	"163.com":     {"网易163", "imap.163.com", 993, "smtp.163.com", 465},
	"126.com":     {"网易126", "imap.126.com", 993, "smtp.126.com", 465},
	"gmail.com":   {"Gmail", "imap.gmail.com", 993, "smtp.gmail.com", 465},
	"outlook.com": {"Outlook", "outlook.office365.com", 993, "smtp.office365.com", 465},
	"hotmail.com": {"Outlook", "outlook.office365.com", 993, "smtp.office365.com", 465},
}

// Config 是服务器的完整运行配置。
type Config struct {
	Provider string
	User     string

	// secret 是授权码的惰性句柄。取得它不访问钥匙串，
	// 第一次 Password() 才去读，之后整个进程复用。
	secret *credstore.Secret

	IMAPHost string
	IMAPPort int

	SMTPHost string
	SMTPPort int

	// AllowSend 控制 send_mail 是否可用。发信不可逆，默认关闭，
	// 需显式设置 QQMAIL_ALLOW_SEND=true 才注册该 tool。
	AllowSend bool

	// MaxBodyBytes 限制 read_mail 返回的正文字节数，防止一封大信撑爆上下文。
	MaxBodyBytes int

	Timeout time.Duration
}

// IMAPAddr 返回 host:port 形式的 IMAP 地址。
func (c Config) IMAPAddr() string { return fmt.Sprintf("%s:%d", c.IMAPHost, c.IMAPPort) }

// MailRef 返回邮件凭据在密码管理器里的标识。
func (c Config) MailRef() credstore.Ref {
	return credstore.Ref{Integration: IntegrationMail, Account: c.User}
}

// Password 取出授权码。第一次调用会访问系统密码管理器，之后用缓存。
func (c Config) Password() (string, error) { return c.secret.Value() }

// SecretSource 返回凭据实际来自哪里（"keychain" 或 "env:XXX"），
// 仅在已取过之后有意义。不含凭据本身。
func (c Config) SecretSource() string { return c.secret.Source() }

// Load 从环境变量读取非敏感配置，并为授权码建立惰性句柄。
//
// 这里不访问系统密码管理器：凭据缺失会在第一次调用 tool 时才报错，
// 换来的是「用不到的集成不解密」。要立即验证就跑 -selftest。
func Load(store *credstore.Store) (Config, error) {
	c := Config{
		User:         firstEnv("QQMAIL_USER", "EMAIL_USER"),
		MaxBodyBytes: envInt("QQMAIL_MAX_BODY_BYTES", 32*1024),
		AllowSend:    envBool("QQMAIL_ALLOW_SEND", false),
		Timeout:      time.Duration(envInt("QQMAIL_TIMEOUT_SECONDS", 30)) * time.Second,
	}

	if c.User == "" {
		return c, fmt.Errorf("必须设置 QQMAIL_USER 为完整邮箱地址")
	}

	p, ok := providers[domainOf(c.User)]
	if !ok {
		p = providers["qq.com"]
		p.Name = "未知服务商（按 QQ 默认值，建议显式设置 QQMAIL_IMAP_HOST / QQMAIL_SMTP_HOST）"
	}
	c.Provider = p.Name
	c.IMAPHost = firstEnvOr(p.IMAPHost, "QQMAIL_IMAP_HOST", "EMAIL_IMAP_HOST")
	c.IMAPPort = envInt("QQMAIL_IMAP_PORT", p.IMAPPort)
	c.SMTPHost = firstEnvOr(p.SMTPHost, "QQMAIL_SMTP_HOST", "EMAIL_SMTP_HOST")
	c.SMTPPort = envInt("QQMAIL_SMTP_PORT", p.SMTPPort)

	// 环境变量是兜底：钥匙串里有就优先用钥匙串，所以把凭据搬进钥匙串
	// 只需 -set-secret 一次，不必同时去客户端面板里删环境变量。
	c.secret = store.Secret(c.MailRef(), "QQMAIL_PASSWORD", "EMAIL_PASSWORD")

	return c, nil
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func firstEnvOr(def string, keys ...string) string {
	if v := firstEnv(keys...); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
