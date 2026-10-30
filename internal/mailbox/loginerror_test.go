package mailbox

import (
	"errors"
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
