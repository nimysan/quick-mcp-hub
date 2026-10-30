package mailbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/nimysan/mcp-hub/internal/config"
)

// CheckStep 是分步连接检查里的一步。
type CheckStep struct {
	// Key 稳定不变，页面按它对号入座（文案可以改，key 不能）。
	Key   string `json:"key"`
	Label string `json:"label"`
	// State: ok / fail / skip。skip 表示前一步没过，这一步没必要做。
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// Note 是「通过了，但有话要说」。目前只有一种：连接被降级到老式加密套件。
	// 和 Error 分开，因为它不该让这一步显示成失败 —— 连接确实是通的。
	Note string `json:"note,omitempty"`
	// Took 是这一步耗时，毫秒。慢在哪一步本身就是诊断信息。
	Took int64 `json:"tookMs"`
}

// Check 分三步检查一个邮件渠道，而不是只回「成或败」。
//
// 为什么要分步：合成一句的话，失败时用户只知道「连不上」——分不清是网络不通、
// 密码错了、还是 SMTP 单独坏了，而这三种的处理完全不同（找 IT / 重新生成授权码
// / 检查发信设置）。这三条是设计稿要求的，也是实际排查时真正需要的切分。
//
// 顺序是刻意的：服务不可达时跳过凭据验证——那时登录失败说明不了任何问题。
// apply 把一次拨号的结果写进这一步。
//
// 单独抽出来是因为「降级」这个状态有三个调用点，而它既不是成功也不是失败：
// 连接是通的（State=ok），但有话必须对用户说（Note）。写三遍一定会漏一处。
func apply(step *CheckStep, err error, start time.Time) {
	step.Took = time.Since(start).Milliseconds()
	switch {
	case err == nil:
		step.State = "ok"
	case errors.Is(err, errDowngraded):
		step.State, step.Note = "ok", errDowngraded.Error()
	default:
		step.State, step.Error = "fail", err.Error()
	}
}

func Check(ctx context.Context, cfg config.Config) []CheckStep {
	steps := []CheckStep{
		{Key: "imap_reach", Label: "IMAP 服务可达性"},
		{Key: "credentials", Label: "邮箱凭据验证"},
		{Key: "smtp_reach", Label: "SMTP 服务可达性"},
	}

	// ① IMAP 端口通不通。只建 TCP+TLS，不登录 —— 这一步失败是网络/防火墙问题，
	//    和密码无关，分开报才能让用户知道该找谁。
	t0 := time.Now()
	if err := dialTLS(ctx, cfg.IMAPHost, cfg.IMAPPort, cfg, cfg.Timeout); err != nil {
		steps[0].State, steps[0].Error = "fail", err.Error()
		steps[0].Took = ms(t0)
		// 服务都不可达时，凭据那一步做了也没有诊断价值。
		steps[1].State, steps[1].Error = "skip", "IMAP 服务不可达，未验证凭据"
		// SMTP 仍然查 —— 两个服务可能一个通一个不通，那本身是有用的信息。
		t2 := time.Now()
		if err := dialTLS(ctx, cfg.SMTPHost, cfg.SMTPPort, cfg, cfg.Timeout); err != nil {
			steps[2].State, steps[2].Error = "fail", err.Error()
		} else {
			steps[2].State = "ok"
		}
		steps[2].Took = ms(t2)
		return steps
	}
	steps[0].State = "ok"
	steps[0].Took = ms(t0)

	// ② 真登录一次并打开 INBOX。这一步失败几乎总是凭据问题
	//    （QQ 系要授权码而不是登录密码，是最常见的那一种）。
	t1 := time.Now()
	if s, err := connect(cfg); err != nil {
		steps[1].State, steps[1].Error = "fail", err.Error()
	} else {
		if _, err := s.selectBox("INBOX"); err != nil {
			steps[1].State, steps[1].Error = "fail", err.Error()
		} else {
			steps[1].State = "ok"
		}
		s.close()
	}
	steps[1].Took = ms(t1)

	// ③ SMTP。只查可达性，**绝不发信** —— 稿子上也写着「不会发送邮件」，
	//    一次测试把邮件发出去是不可逆的。
	t2 := time.Now()
	apply(&steps[2], dialTLS(ctx, cfg.SMTPHost, cfg.SMTPPort, cfg, cfg.Timeout), t2)
	return steps
}

// dialTLS 只验证「这个端口上有一个能完成 TLS 握手的服务」。
//
// 用和真实连接同一套 TLS 配置（含按渠道生效的老服务器放宽），否则这一步会
// 和实际连接的结果不一致 —— 那比不检查更糟。
func dialTLS(ctx context.Context, host string, port int, cfg config.Config, timeout time.Duration) error {
	if host == "" || port == 0 {
		return fmt.Errorf("没有配置主机或端口")
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	d := &net.Dialer{Timeout: timeout}
	conn, downgraded, err := dialWithFallback(d, host, addr, cfg.LegacyRSAKeyExchange)
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	conn.Close()
	if downgraded {
		// 不是错误，但必须让用户知道：这条连接没有前向保密。
		return errDowngraded
	}
	return nil
}

// errDowngraded 不是失败，是「连上了，但只能用老式加密套件」。
//
// 用一个哨兵错误而不是多一个返回值：dialTLS 有两个调用点，加返回值要改两处
// 且容易漏判，而这个状态**必须**传到界面上 —— 悄悄降级等于骗用户。
var errDowngraded = errors.New(
	"服务器只接受老式加密套件（无前向保密），已降级连接")

func ms(t time.Time) int64 { return time.Since(t).Milliseconds() }
