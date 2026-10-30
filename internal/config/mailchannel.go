package config

import "time"

// MailChannelInput 是一个邮件渠道实例装配成 Config 所需的全部输入。
// 由 channels 包填好后传进来，避免 config 反向依赖 channels。
type MailChannelInput struct {
	User         string
	Secret       Secret // 惰性凭据句柄（可能来自钥匙串，也可能是表单里的临时值）
	IMAPHost     string
	IMAPPort     int
	SMTPHost     string
	SMTPPort     int
	AllowSend    bool
	ProviderName string
	// LegacyRSAKeyExchange 见 Config 上的同名字段。
	LegacyRSAKeyExchange bool
	MaxBodyBytes         int           // 0 取默认
	Timeout              time.Duration // 0 取默认
}

// Secret 是凭据的惰性句柄接口，由 credstore 实现。
// 在这里重新声明为接口，纯粹是为了不让 config 依赖 credstore 的具体类型，
// 从而让 channels 能把自己构造的 handle（含表单临时值）传进来。
type Secret interface {
	Value() (string, error)
	Source() string
}

// FromMailChannel 把一个邮件渠道实例装配成运行配置。
// 不读取凭据 —— 和 Load 一样保持惰性。
func FromMailChannel(in MailChannelInput) Config {
	c := Config{
		Provider:             in.ProviderName,
		User:                 in.User,
		secret:               in.Secret,
		IMAPHost:             in.IMAPHost,
		IMAPPort:             in.IMAPPort,
		SMTPHost:             in.SMTPHost,
		SMTPPort:             in.SMTPPort,
		AllowSend:            in.AllowSend,
		LegacyRSAKeyExchange: in.LegacyRSAKeyExchange,
		MaxBodyBytes:         in.MaxBodyBytes,
		Timeout:              in.Timeout,
	}
	if c.IMAPPort == 0 {
		c.IMAPPort = 993
	}
	if c.SMTPPort == 0 {
		c.SMTPPort = 465
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 32 * 1024
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	return c
}
