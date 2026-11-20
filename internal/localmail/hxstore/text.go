package hxstore

import (
	"html"
	"regexp"
	"strings"
)

// shortcut: 正则级 HTML 转文本，够 Outlook 生成的 HTML 用；遇到需要保留表格/链接结构时
// 换成 golang.org/x/net/html 的分词器。
var (
	reDropBlock = []*regexp.Regexp{
		regexp.MustCompile(`(?s)<!--.*?-->`),
		regexp.MustCompile(`(?is)<style\b.*?</style\s*>`),
		regexp.MustCompile(`(?is)<script\b.*?</script\s*>`),
		regexp.MustCompile(`(?is)<xml\b.*?</xml\s*>`),
		regexp.MustCompile(`(?is)<head\b.*?</head\s*>`),
	}
	reBreak  = regexp.MustCompile(`(?i)<\s*(br|hr)\b[^>]*>|</\s*(p|div|tr|li|h[1-6]|table|blockquote)\s*>`)
	reTag    = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces = regexp.MustCompile(`[ \t\x{00a0}]+`)
	reBlank  = regexp.MustCompile(`\n{3,}`)
)

func htmlToText(s string) string {
	if s == "" {
		return ""
	}
	for _, re := range reDropBlock {
		s = re.ReplaceAllString(s, "")
	}
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r", "")
	s = reSpaces.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = reBlank.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(s)
}
