package outlooklegacy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/emersion/go-message"
	// 注册 GBK/GB18030 等字符集解码器，国内邮件常用。
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

// olk15 文件（Message / MsgSource / MsgAttachment）共用的 40 字节文件头，
// 以小端 0x00000DD0（字节 d0 0d 00 00）开头；实测 MsgSource/MsgAttachment 的
// 载荷紧跟在第 40 字节之后。
const olkHeaderLen = 40

var olkMagic = []byte{0xd0, 0x0d, 0x00, 0x00}

const maxCacheFile = 64 << 20 // olk15Message 读取上限

// mimeMarkers 在文件头不认识时用来定位 MIME 起点（与 olk15-export 相同的启发式）。
var mimeMarkers = [][]byte{
	[]byte("Received:"), []byte("From:"), []byte("Return-Path:"), []byte("MIME-Version:"),
	[]byte("Date:"), []byte("Subject:"), []byte("Message-ID:"), []byte("Content-"),
}

// skipOLKHeader 让 br 停在载荷起点。
func skipOLKHeader(br *bufio.Reader) error {
	head, err := br.Peek(olkHeaderLen)
	if err == nil && bytes.Equal(head[:4], olkMagic) {
		_, err = br.Discard(olkHeaderLen)
		return err
	}
	buf, _ := br.Peek(8192)
	best := -1
	for _, m := range mimeMarkers {
		if i := bytes.Index(buf, m); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if best < 0 {
		return errors.New("不是可识别的 olk15 文件")
	}
	_, err = br.Discard(best)
	return err
}

type mimeResult struct {
	text, subject, messageID string
	date                     time.Time
	truncated                bool
	attachments              []Attachment
}

// readSource 流式解析 olk15MsgSource（40 字节头 + RFC822）。
func readSource(path string, maxBody int) (*mimeResult, error) {
	f, err := os.Open(path) // 只读
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	if err := skipOLKHeader(br); err != nil {
		return nil, err
	}
	// 实测 MsgSource/MsgAttachment 用老 Mac 的单 CR 换行，先规范成 CRLF 再交给 MIME 解析。
	mr, err := mail.CreateReader(bufio.NewReaderSize(&crlfReader{r: br}, 64<<10))
	if err != nil && !message.IsUnknownCharset(err) {
		return nil, fmt.Errorf("解析 MIME 失败: %w", err)
	}
	if mr == nil {
		return nil, errors.New("解析 MIME 失败")
	}
	defer mr.Close()

	res := &mimeResult{}
	res.subject, _ = mr.Header.Subject()
	res.date, _ = mr.Header.Date()
	res.messageID, _ = mr.Header.MessageID()

	var plain, html strings.Builder
	plainCut, htmlCut := false, false
	readText := func(dst *strings.Builder, r io.Reader, cut *bool) {
		room := int64(maxBody - dst.Len())
		if room <= 0 {
			*cut = true
			return
		}
		b, _ := io.ReadAll(io.LimitReader(r, room+1))
		if int64(len(b)) > room {
			b, *cut = b[:room], true
		}
		dst.Write(b)
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil && (part == nil || !message.IsUnknownCharset(err)) {
			break // 后面的分段坏了：保留已读到的
		}
		switch h := part.Header.(type) {
		case *mail.InlineHeader:
			ct, params, _ := h.ContentType()
			switch {
			case ct == "text/plain" || ct == "":
				readText(&plain, part.Body, &plainCut)
			case ct == "text/html":
				readText(&html, part.Body, &htmlCut)
			case params["name"] != "": // 内嵌图片等带名字的分段，按附件列出
				n, _ := io.Copy(io.Discard, part.Body)
				res.attachments = append(res.attachments, Attachment{Name: decodeName(params["name"]), ContentType: ct, Size: n})
			}
		case *mail.AttachmentHeader:
			name, _ := h.Filename()
			ct, _, _ := h.ContentType()
			if name == "" {
				name = "(未命名附件)"
			}
			n, _ := io.Copy(io.Discard, part.Body)
			res.attachments = append(res.attachments, Attachment{Name: name, ContentType: ct, Size: n})
		}
	}
	if t := strings.TrimSpace(plain.String()); t != "" {
		res.text, res.truncated = t, plainCut
	} else {
		res.text, res.truncated = htmlToText(html.String()), htmlCut
	}
	return res, nil
}

func decodeName(s string) string {
	h := mail.Header{}
	h.Set("X-Name", s)
	if v, err := h.Text("X-Name"); err == nil {
		return v
	}
	return s
}

// readCachedBody 从 olk15Message 里抠出正文。实测约 94% 的文件内含一段 UTF-16LE 的
// <html>…</html>；其余是日历邀请（VCALENDAR）或没有正文。closed 表示找到了结束标记。
func readCachedBody(path string) (text string, closed bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCacheFile))
	if err != nil {
		return "", false, err
	}
	if start := indexFoldUTF16(b, "<html", 0); start >= 0 {
		end := lastIndexFoldUTF16(b, "</html>", start)
		if end >= 0 {
			return htmlToText(decodeUTF16LE(b[start : end+14])), true, nil
		}
		return htmlToText(cutBinaryTail(decodeUTF16LE(b[start:]))), false, nil
	}
	if start := indexFoldUTF16(b, "BEGIN:VCALENDAR", 0); start >= 0 {
		s := cutBinaryTail(decodeUTF16LE(b[start:]))
		if i := strings.Index(s, "END:VCALENDAR"); i >= 0 {
			return strings.TrimSpace(s[:i+13]), true, nil
		}
		return strings.TrimSpace(s), false, nil
	}
	if start := bytes.Index(b, []byte("BEGIN:VCALENDAR")); start >= 0 {
		s := string(b[start:])
		if i := strings.Index(s, "END:VCALENDAR"); i >= 0 {
			return strings.TrimSpace(s[:i+13]), true, nil
		}
	}
	return "", false, nil
}

// indexFoldUTF16 在 b 里按 UTF-16LE、ASCII 不区分大小写查找 pat（pat 必须是 ASCII）。
func indexFoldUTF16(b []byte, pat string, from int) int {
	n := len(pat) * 2
	for i := from; i+n <= len(b); i++ {
		if matchFoldUTF16(b[i:i+n], pat) {
			return i
		}
	}
	return -1
}

func lastIndexFoldUTF16(b []byte, pat string, min int) int {
	n := len(pat) * 2
	for i := len(b) - n; i >= min; i-- {
		if matchFoldUTF16(b[i:i+n], pat) {
			return i
		}
	}
	return -1
}

func matchFoldUTF16(w []byte, pat string) bool {
	for k := 0; k < len(pat); k++ {
		if w[2*k+1] != 0 || lower(w[2*k]) != lower(pat[k]) {
			return false
		}
	}
	return true
}

func lower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 32
	}
	return c
}

func decodeUTF16LE(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

// cutBinaryTail 截掉解码后尾部的二进制残渣（连续 3 个以上控制字符处）。
func cutBinaryTail(s string) string {
	run := 0
	for i, r := range s {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			run++
			if run >= 3 {
				return s[:i-2]
			}
		} else {
			run = 0
		}
	}
	return s
}

// readAttachmentMeta 读 olk15MsgAttachment 的 MIME 分段头，拿文件名；
// 大小按 base64 载荷长度估算（实测分段头里没有 size= 参数）。
func readAttachmentMeta(path string) (Attachment, error) {
	total, err := fileSize(path)
	if err != nil {
		return Attachment{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Attachment{}, err
	}
	defer f.Close()
	cr := &countingReader{r: f}
	br := bufio.NewReaderSize(cr, 64<<10)
	if err := skipOLKHeader(br); err != nil {
		return Attachment{}, err
	}
	skipped := cr.n - int64(br.Buffered())
	buf, _ := br.Peek(64 << 10)
	hdrLen := -1
	for _, sep := range []string{"\r\n\r\n", "\n\n", "\r\r"} {
		if i := bytes.Index(buf, []byte(sep)); i >= 0 && (hdrLen < 0 || i+len(sep) < hdrLen) {
			hdrLen = i + len(sep)
		}
	}
	if hdrLen < 0 {
		return Attachment{}, errors.New("附件头过长或缺少空行")
	}
	th, err := textproto.ReadHeader(bufio.NewReader(&crlfReader{r: bufio.NewReader(bytes.NewReader(buf[:hdrLen]))}))
	if err != nil {
		return Attachment{}, fmt.Errorf("附件头解析失败: %w", err)
	}
	ah := mail.AttachmentHeader{Header: message.Header{Header: th}}
	a := Attachment{SizeEstimated: true}
	a.Name, _ = ah.Filename()
	ct, params, _ := ah.ContentType()
	a.ContentType = ct
	if a.Name == "" && params["name"] != "" {
		a.Name = decodeName(params["name"])
	}
	if a.Name == "" {
		a.Name = "(未命名附件)"
	}
	body := total - skipped - int64(hdrLen)
	if body < 0 {
		body = 0
	}
	a.Size = body
	if strings.EqualFold(strings.TrimSpace(th.Get("Content-Transfer-Encoding")), "base64") {
		// 用已读到的那段载荷算出 base64 字符占比（扣掉换行），再按 3/4 折算。
		sample := buf[hdrLen:]
		if len(sample) > 0 {
			n := 0
			for _, c := range sample {
				if c != '\r' && c != '\n' && c != ' ' && c != '\t' {
					n++
				}
			}
			a.Size = int64(float64(body) * float64(n) / float64(len(sample)) * 3 / 4)
		}
	}
	return a, nil
}

// crlfReader 把单独的 CR 或 LF 都规范成 CRLF。
type crlfReader struct {
	r       *bufio.Reader
	pending []byte
	lastCR  bool
}

func (c *crlfReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(c.pending) > 0 {
			k := copy(p[n:], c.pending)
			c.pending = c.pending[k:]
			n += k
			continue
		}
		b, err := c.r.ReadByte()
		if err != nil {
			if c.lastCR {
				c.lastCR = false
				c.pending = []byte{'\n'}
				continue
			}
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		switch {
		case c.lastCR && b == '\n':
			c.lastCR = false
			c.pending = []byte{'\n'}
		case c.lastCR && b == '\r':
			c.pending = []byte{'\n', '\r'}
		case c.lastCR:
			c.lastCR = false
			c.pending = []byte{'\n', b}
		case b == '\r':
			c.lastCR = true
			c.pending = []byte{'\r'}
		case b == '\n':
			c.pending = []byte{'\r', '\n'}
		default:
			p[n] = b
			n++
		}
	}
	return n, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
