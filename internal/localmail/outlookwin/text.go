package outlookwin

import (
	"errors"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// shortcut: 正则级 HTML 转文本（和 outlooklegacy 同一份），够 Outlook 生成的 HTML 用；
// 遇到需要保留表格/链接结构时换成 golang.org/x/net/html 的分词器。
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

// rtfPrebuf 是 [MS-OXRTFCP] 2.1.2.1 规定的字典初始内容（207 字节）。
const rtfPrebuf = "{\\rtf1\\ansi\\mac\\deff0\\deftab720{\\fonttbl;}" +
	"{\\f0\\fnil \\froman \\fswiss \\fmodern \\fscript \\fdecor MS Sans SerifSymbolArialTimes New RomanCourier" +
	"{\\colortbl\\red0\\green0\\blue0\r\n\\par \\pard\\plain\\f0\\fs20\\b\\i\\u\\tab\\tx"

const (
	rtfCompressed   = 0x75465A4C // "LZFu"
	rtfUncompressed = 0x414C454D // "MELA"
	maxRTFBytes     = 16 << 20
)

// decompressRTF 解 PidTagRtfCompressed（[MS-OXRTFCP]）。
func decompressRTF(b []byte) ([]byte, error) {
	if len(b) < 16 {
		return nil, errors.New("压缩 RTF 太短")
	}
	compSize, rawSize, typ := int(le.Uint32(b)), int(le.Uint32(b[4:])), le.Uint32(b[8:])
	if rawSize > maxRTFBytes {
		return nil, errors.New("RTF 过大")
	}
	data := b[16:]
	if compSize-12 < len(data) && compSize >= 12 {
		data = data[:compSize-12]
	}
	switch typ {
	case rtfUncompressed:
		if rawSize < len(data) {
			data = data[:rawSize]
		}
		return append([]byte(nil), data...), nil
	case rtfCompressed:
	default:
		return nil, errors.New("不认识的 RTF 压缩类型")
	}
	var dict [4096]byte
	copy(dict[:], rtfPrebuf)
	wp := len(rtfPrebuf)
	out := make([]byte, 0, rawSize)
	for i := 0; i < len(data); {
		ctrl := data[i]
		i++
		for bit := 0; bit < 8 && i < len(data); bit++ {
			if ctrl&(1<<bit) == 0 {
				c := data[i]
				i++
				out = append(out, c)
				dict[wp] = c
				wp = (wp + 1) & 0xFFF
				continue
			}
			if i+1 >= len(data) {
				return out, nil
			}
			ref := int(data[i])<<8 | int(data[i+1])
			i += 2
			off, n := ref>>4, ref&0xF+2
			if off == wp {
				return out, nil // 结束标记
			}
			for k := 0; k < n; k++ {
				c := dict[(off+k)&0xFFF]
				out = append(out, c)
				dict[wp] = c
				wp = (wp + 1) & 0xFFF
			}
			if len(out) > maxRTFBytes {
				return nil, errors.New("RTF 过大")
			}
		}
	}
	return out, nil
}

// rtfToText 粗略把 RTF 转成纯文本：丢掉控制字和 {\* ...}、字体表等目标组，
// 保留 \par、\'hh、\uN。由 HTML 封装来的 RTF（\fromhtml1）取 \htmlrtf0 之外的文字，效果近似。
// shortcut: 不处理表格、字段等复杂结构；够做正文预览和检索。
func rtfToText(b []byte, cp int) string {
	var out strings.Builder
	var pending []byte // 连续的 \'hh，按代码页一起解码
	flush := func() {
		if len(pending) > 0 {
			out.WriteString(decode8(pending, cp))
			pending = pending[:0]
		}
	}
	type state struct{ skip bool }
	stack := []state{{}}
	skipDest := map[string]bool{"fonttbl": true, "colortbl": true, "stylesheet": true, "info": true, "pict": true, "object": true, "header": true, "footer": true, "listtable": true, "listoverridetable": true, "generator": true}
	ucSkip := 1 // RTF 规范的默认值
	htmlrtf := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		cur := &stack[len(stack)-1]
		switch c {
		case '{':
			flush()
			stack = append(stack, *cur)
			continue
		case '}':
			flush()
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			continue
		case '\r', '\n':
			continue
		case '\\':
			if i+1 >= len(b) {
				continue
			}
			n := b[i+1]
			if n == '\'' && i+3 < len(b) {
				if v, err := strconv.ParseUint(string(b[i+2:i+4]), 16, 8); err == nil && !cur.skip && !htmlrtf {
					pending = append(pending, byte(v))
				}
				i += 3
				continue
			}
			if n == '*' {
				cur.skip = true
				i++
				continue
			}
			if !isAlpha(n) {
				if !cur.skip && !htmlrtf && (n == '\\' || n == '{' || n == '}') {
					flush()
					out.WriteByte(n)
				}
				i++
				continue
			}
			j := i + 1
			for j < len(b) && isAlpha(b[j]) {
				j++
			}
			word := string(b[i+1 : j])
			k := j
			if k < len(b) && (b[k] == '-' || isDigit(b[k])) {
				k++
				for k < len(b) && isDigit(b[k]) {
					k++
				}
			}
			param := string(b[j:k])
			if k < len(b) && b[k] == ' ' {
				k++
			}
			i = k - 1
			flush()
			switch {
			case skipDest[word]:
				cur.skip = true
			case word == "htmlrtf":
				htmlrtf = param != "0"
			case cur.skip || htmlrtf:
			case word == "par" || word == "line":
				out.WriteByte('\n')
			case word == "tab":
				out.WriteByte('\t')
			case word == "uc":
				ucSkip, _ = strconv.Atoi(param)
			case word == "u":
				if v, err := strconv.Atoi(param); err == nil {
					if v < 0 {
						v += 65536
					}
					out.WriteRune(rune(v))
					// 跳过 \uN 后面的替代字符
					for s := ucSkip; s > 0 && i+1 < len(b); s-- {
						if b[i+1] == '\\' && i+2 < len(b) && b[i+2] == '\'' {
							i += 4
						} else {
							i++
						}
					}
				}
			}
			continue
		}
		if !cur.skip && !htmlrtf {
			flush()
			out.WriteByte(c)
		}
	}
	flush()
	s := strings.ReplaceAll(out.String(), "\r", "")
	s = reBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
