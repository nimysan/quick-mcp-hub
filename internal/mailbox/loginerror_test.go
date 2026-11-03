package mailbox

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/nimysan/mcp-hub/internal/config"
)

// 登录失败的提示必须跟着渠道走。
//
// 真实故障：提示写死成「QQ/163 需使用 16 位授权码而非登录密码」，于是在
// 「企业邮箱」渠道（自建服务器，填的就是登录密码）上密码填错时，页面教用户
// 去找一个那台服务器根本没有的东西。错的提示比没有提示更糟。
func TestLoginErrorNamesTheRightCredential(t *testing.T) {
	authErr := errors.New("imap: NO [AUTHENTICATIONFAILED] Authentication failed.")

	t.Run("授权码类渠道", func(t *testing.T) {
		cfg := config.Config{SecretLabel: "授权码"}
		got := loginError("IMAP", cfg, authErr).Error()
		if !strings.Contains(got, "授权码不正确") {
			t.Errorf("要说清是授权码不对，得到 %q", got)
		}
		if !strings.Contains(got, "不是邮箱的登录密码") {
			t.Errorf("授权码类渠道要提示别填登录密码，得到 %q", got)
		}
	})

	t.Run("登录密码类渠道", func(t *testing.T) {
		cfg := config.Config{SecretLabel: "邮箱密码"}
		got := loginError("IMAP", cfg, authErr).Error()
		if !strings.Contains(got, "邮箱密码不正确") {
			t.Errorf("要说清是邮箱密码不对，得到 %q", got)
		}
		// 这是这条用例的要点：绝不能对自建邮箱提授权码。
		if strings.Contains(got, "授权码") {
			t.Errorf("自建邮箱没有授权码这个东西，提示里不该出现它：%q", got)
		}
	})

	t.Run("没给叫法时兜底", func(t *testing.T) {
		got := loginError("IMAP", config.Config{}, authErr).Error()
		if !strings.Contains(got, "密码不正确") {
			t.Errorf("要有兜底说法，得到 %q", got)
		}
	})
}

// 不是凭据问题的失败，要把服务器原话带上 —— 那才是线索。
func TestLoginErrorKeepsServerWordsForOtherFailures(t *testing.T) {
	cfg := config.Config{SecretLabel: "邮箱密码"}
	// Dovecot 在账号被禁用时回的是这种，和密码错完全不同的处置办法。
	raw := "imap: NO [ALERT] Logging in as star.zhang is disabled"
	got := loginError("IMAP", cfg, errors.New(raw)).Error()
	if !strings.Contains(got, "disabled") {
		t.Errorf("非凭据问题要保留服务器原话（这里是账号被禁用），得到 %q", got)
	}
	if strings.Contains(got, "不正确") {
		t.Errorf("账号被禁用不该说成密码不正确：%q", got)
	}
}

// 前缀只能出现一次。
//
// 真实故障：mailbox 返回「IMAP 登录失败」，外层又包一层「IMAP: 」，
// 页面上显示「IMAP: IMAP 登录失败」—— 读起来像程序坏了。
func TestLoginErrorHasNoDoubledPrefix(t *testing.T) {
	cfg := config.Config{SecretLabel: "邮箱密码"}
	for _, err := range []error{
		errors.New("imap: NO [AUTHENTICATIONFAILED] Authentication failed."),
		errors.New("imap: NO [ALERT] something else"),
	} {
		got := loginError("IMAP", cfg, err).Error()
		if strings.Count(got, "IMAP") > 1 && strings.Contains(got, "IMAP: IMAP") {
			t.Errorf("前缀重复了：%q", got)
		}
	}
}

// authFailed 的判据要认得各家的说法，又不能把别的失败也算进去。
func TestAuthFailedRecognisesRealResponses(t *testing.T) {
	yes := []string{
		"imap: NO [AUTHENTICATIONFAILED] Authentication failed.", // Dovecot / QQ
		"535 5.7.8 Error: authentication failed",                 // Postfix SMTP
		"NO Invalid credentials",                                 // 另一种措辞
	}
	no := []string{
		"dial tcp 1.2.3.4:993: i/o timeout",       // 网络不通
		"x509: certificate has expired",           // 证书问题
		"imap: NO [ALERT] Logging in is disabled", // 账号被禁用
		"tls: handshake failure",                  // 套件谈不拢
	}
	for _, s := range yes {
		if !authFailed(errors.New(s)) {
			t.Errorf("该认出是凭据问题：%q", s)
		}
	}
	for _, s := range no {
		if authFailed(errors.New(s)) {
			t.Errorf("不该当成凭据问题：%q", s)
		}
	}
}

// 登录失败必须在日志里留下服务器的原话。
//
// 真实故障：企业邮箱渠道报「邮箱密码不正确」，而日志里关于这次失败
// **一个字都没有** —— 只有一行 TLS 降级记录。原因是下面这个分支会重建一个
// 新错误（不包原错），于是服务器自己说了什么，界面上没有、日志里也没有，
// 这条「密码不正确」成了无法核实的断言。
//
// 而 AUTHENTICATIONFAILED 是 RFC 5530 的笼统码：用户名错、密码错、
// 「这里要用专用密码」都走它，服务器刻意不说是哪一种。所以人话提示是推断，
// 排查要的是原话。
func TestLoginErrorLogsServerReply(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	cfg := config.FromMailChannel(config.MailChannelInput{
		User:        "star.zhang@example.com",
		IMAPHost:    "mail.example.com",
		SecretLabel: "邮箱密码",
	})
	_ = loginError("IMAP", cfg, errors.New("imap: NO [AUTHENTICATIONFAILED] Authentication failed."))

	got := buf.String()
	// 原话、登录名、主机：三样都要在。登录名不是秘密，而它恰好是最常出错的
	// 那个事实（有的 Dovecot 只认裸用户名，带 @域名 就被拒）。
	for _, want := range []string{
		"AUTHENTICATIONFAILED",   // 服务器原话
		"star.zhang@example.com", // 实际送出的登录名
		"mail.example.com",       // 哪一台服务器
	} {
		if !strings.Contains(got, want) {
			t.Errorf("日志里要有 %q，得到 %q", want, got)
		}
	}
}

// 日志里绝不能出现密码。
//
// 上面那条要求「记下原话和登录名」，这条是它的边界：诊断信息再有用，
// 凭据也不能落到文件里 —— 日志会被用户直接发给维护者。
func TestLoginErrorNeverLogsPassword(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	const secret = "hunter2-不该出现"
	cfg := config.FromMailChannel(config.MailChannelInput{
		User:     "a@example.com",
		IMAPHost: "mail.example.com",
		Secret:   fixedSecret(secret),
	})
	_ = loginError("IMAP", cfg, errors.New("imap: NO [AUTHENTICATIONFAILED] Authentication failed."))

	if strings.Contains(buf.String(), secret) {
		t.Errorf("日志里出现了密码：%q", buf.String())
	}
}

// fixedSecret 是个写死值的凭据，只用于上面那条「日志里不能有密码」的用例。
type fixedSecret string

func (s fixedSecret) Value() (string, error) { return string(s), nil }
func (s fixedSecret) Source() string         { return "测试" }
