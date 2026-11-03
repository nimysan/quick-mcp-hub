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

// loginHost 是这一侧实际连的主机。两侧主机可以不同（imap.x / smtp.x），
// 日志里不写清是哪一台，就分不出「收信那台拒了」还是「发信那台拒了」。
func loginHost(proto string, cfg config.Config) string {
	if proto == "IMAP" {
		return fmt.Sprintf("%s:%d", cfg.IMAPHost, cfg.IMAPPort)
	}
	return fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
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
	// 先把服务器的原话记进日志，再翻译。
	//
	// 不记的话这类失败在日志里**查不到任何证据**：下面的凭据分支会重建一个
	// 新错误（不包原错），于是服务器到底回了什么，界面上没有、日志里也没有。
	// 而 AUTHENTICATIONFAILED 是个笼统的码（RFC 5530）——用户名错、密码错、
	// 「这里要用专用密码」都走它，服务器刻意不说是哪一种。所以下面那句人话
	// 是**我们的推断**，排查时需要的是服务器自己说了什么。
	//
	// 登录名一起记：它不是秘密，而它恰好是最常出错的那个事实（有的 Dovecot
	// 只认裸用户名，带 @域名 就被拒）。密码任何形式都不进日志。
	// 措辞刻意中立（「失败」而不是「被拒」）：这里也会收到 TLS 握手失败，
	// 那种情况连认证都没开始，断言「被拒」就是在替服务器下结论。
	logf("%s %s 失败：登录名 %q，原始错误：%v", proto, loginHost(proto, cfg), cfg.User, err)

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
