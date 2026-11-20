package applemail

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	reDropBlock = regexp.MustCompile(`(?is)<(script|style|head|title)\b[^>]*>.*?</(script|style|head|title)\s*>`)
	reComment   = regexp.MustCompile(`(?s)<!--.*?-->`)
	reBreak     = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr|/h[1-6]|/table|/blockquote)\b[^>]*>`)
	reLi        = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	reTag       = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces    = regexp.MustCompile(`[ \t\x{00a0}]+`)
	reBlank     = regexp.MustCompile(`\n\s*\n\s*\n+`)
)

// htmlToText 把 HTML 正文粗转为纯文本：去 script/style、块级标签换行、反转义实体。
// shortcut: 正则实现，不处理表格对齐和链接地址；需要更好效果时换 x/net/html 遍历 DOM。
func htmlToText(s string) string {
	s = reComment.ReplaceAllString(s, "")
	s = reDropBlock.ReplaceAllString(s, "")
	s = reLi.ReplaceAllString(s, "\n- ")
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r", "")
	s = reSpaces.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = reBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// truncateUTF8 截到不超过 n 字节且不切断多字节字符。
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
