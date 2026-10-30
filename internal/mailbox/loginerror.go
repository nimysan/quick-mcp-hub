package mailbox

import (
	"fmt"
	"strings"

	"github.com/nimysan/mcp-hub/internal/config"
)

// authFailed 判断这次失败是不是「凭据不对」。
//
// 分出这一种是因为它的处置办法和别的失败完全不同：密码错要用户去改密码，
// 而「账号被停用」「这个账号没开 IMAP」要去找管理员。混成一句「登录失败」
// 等于让用户自己猜。
//
// AUTHENTICATIONFAILED 是 RFC 5530 定义的响应码，Dovecot / Exchange / QQ
// 都会回它，所以这是可靠的判据，比匹配英文句子稳。
func authFailed(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToUpper(err.Error())
	return strings.Contains(s, "AUTHENTICATIONFAILED") ||
		strings.Contains(s, "AUTHENTICATION FAILED") ||
		strings.Contains(s, "INVALID CREDENTIALS") ||
		strings.Contains(s, "LOGIN FAILED") ||
		// SMTP 的说法（RFC 4954）：535 5.7.8
		strings.Contains(s, "535 ")
}

// secretName 是这个渠道对凭据的叫法，用于说人话的错误提示。
func secretName(cfg config.Config) string {
	if s := strings.TrimSpace(cfg.SecretLabel); s != "" {
		return s
	}
	return "密码"
}

// loginError 把一次登录失败翻译成用户能据以行动的一句话。
//
// 以前这里写死「QQ/163 需使用 16 位授权码而非登录密码」。那条提示对 QQ 是
// 对的，对自建邮箱渠道则是**错的** —— 那种服务器根本没有授权码这个东西，
// 用户照着提示去找授权码只会更迷惑。所以凭据的叫法必须跟着渠道走。
//
// proto 是 "IMAP" 或 "SMTP"，用于说清是哪一侧失败：两侧用的是同一套凭据，
// 但一侧通一侧不通是真实存在的（有的服务器单独关掉发信）。
func loginError(proto string, cfg config.Config, err error) error {
	name := secretName(cfg)
	if !authFailed(err) {
		// 不是凭据问题：原样把服务器的回答带上。这类失败（账号停用、
		// 未开通 IMAP、要求改密码）的原话才是有用的线索，翻译反而丢信息。
		return fmt.Errorf("%s 登录失败: %w", proto, err)
	}
	// 凭据问题：把「该填什么」说清楚，不要再让人去读英文协议应答。
	hint := name + "不正确"
	if strings.Contains(name, "授权码") {
		// 这类服务商（QQ/163 等）最常见的错就是填了登录密码。
		hint += "。这里要填邮箱网页版生成的授权码，不是邮箱的登录密码"
	} else {
		// 自建服务器：填的就是登录密码，所以要提示的是别的常见错因。
		hint += "。确认用的是这个邮箱的登录密码；" +
			"有些公司的服务器要求用完整邮箱地址作为登录名"
	}
	return fmt.Errorf("%s: %s", proto, hint)
}
