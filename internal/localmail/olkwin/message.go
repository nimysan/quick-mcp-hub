package olkwin

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Attachment 是附件元数据。附件内容不在 IndexedDB 里（在 WebView2 的磁盘缓存中），不读。
type Attachment struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"` // -1 表示未知
	Inline      bool   `json:"inline,omitempty"`
}

// Recipient 是一个收件人。
type Recipient struct {
	Type    string `json:"type"` // to / cc / bcc
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
}

// Message 是一封邮件的详情。
type Message struct {
	Summary
	MessageID     string       `json:"message_id,omitempty"`
	Recipients    []Recipient  `json:"recipients,omitempty"`
	Body          string       `json:"body"`
	BodySource    string       `json:"body_source"` // html / text / preview / none
	BodyTruncated bool         `json:"body_truncated"`
	Attachments   []Attachment `json:"attachments,omitempty"`
}

// Get 取一封邮件。id 是 List 返回的 ID。
func (s *Store) Get(ctx context.Context, id int64) (*Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ref, ok := s.byMsg[id]
	if !ok {
		return nil, fmt.Errorf("%w: 邮件 %d", ErrNotFound, id)
	}
	m := &Message{Summary: s.index[ref.summary]}
	m.Incomplete = append([]string(nil), m.Incomplete...)
	if !ref.hasBody {
		// 只有列表行没有完整邮件：正文只能给摘要，收件人和附件拿不到。
		m.Body, m.BodySource = m.Preview, "preview"
		m.Incomplete = append(m.Incomplete, "body", "recipients", "attachments", "message_id")
		return m, nil
	}
	v, err := ref.db.decode(ref.dbID, ref.bodies, ref.rec)
	o, _ := v.(*jsObject)
	if err != nil || o == nil {
		m.Body, m.BodySource = m.Preview, "preview"
		m.Incomplete = append(m.Incomplete, "body", "recipients", "attachments", "message_id")
		return m, nil
	}
	full := summaryOf(o)
	// 列表行缺的字段用完整邮件补上
	if m.From == "" && m.FromAddress == "" {
		m.From, m.FromAddress = full.From, full.FromAddress
	}
	if m.Received.IsZero() {
		m.Received = full.Received
	}
	if m.Sent.IsZero() {
		m.Sent = full.Sent
	}
	m.Incomplete = intersect(m.Incomplete, full.Incomplete)
	m.MessageID = str(o, "InternetMessageId")
	if m.MessageID == "" {
		m.Incomplete = append(m.Incomplete, "message_id")
	}
	for _, rt := range [][2]string{{"to", "ToRecipients"}, {"cc", "CcRecipients"}, {"bcc", "BccRecipients"}} {
		arr, _ := o.get(rt[1]).([]any)
		for _, e := range arr {
			if r, ok := e.(*jsObject); ok {
				m.Recipients = append(m.Recipients, Recipient{Type: rt[0], Name: str(r, "Name"), Address: str(r, "EmailAddress")})
			}
		}
	}
	var cc []string
	for _, r := range m.Recipients {
		if r.Type == "cc" {
			cc = append(cc, displayOf(r))
		}
	}
	if len(cc) > 0 {
		m.Cc = cc
	}
	if len(m.To) == 0 {
		for _, r := range m.Recipients {
			if r.Type == "to" {
				m.To = append(m.To, displayOf(r))
			}
		}
	}

	// 正文：NormalizedBody → Body → UniqueBody，取第一个有值的；服务器只给了截断的正文时标出来。
	m.BodySource = "none"
	for _, k := range []string{"NormalizedBody", "Body", "UniqueBody"} {
		b := obj(o, k)
		val, ok := b.get("Value").(string)
		if !ok {
			continue
		}
		if strings.EqualFold(str(b, "BodyType"), "HTML") {
			m.Body, m.BodySource = htmlToText(val), "html"
		} else {
			m.Body, m.BodySource = strings.TrimSpace(val), "text"
		}
		if t, _ := boolean(b, "IsTruncated"); t {
			m.BodyTruncated = true
			m.Incomplete = append(m.Incomplete, "body_full")
		}
		break
	}
	if m.BodySource == "none" {
		m.Body, m.BodySource = m.Preview, "preview"
		m.Incomplete = append(m.Incomplete, "body")
	}
	var cut bool
	m.Body, cut = truncate(m.Body, s.maxBody)
	m.BodyTruncated = m.BodyTruncated || cut
	if m.Preview == "" {
		m.Preview = preview(firstBytes(m.Body, 2048))
	}

	switch atts := o.get("Attachments").(type) {
	case []any:
		for _, e := range atts {
			a, ok := e.(*jsObject)
			if !ok {
				continue
			}
			at := Attachment{Name: str(a, "Name"), ContentType: str(a, "ContentType"), Size: -1}
			if v, ok := a.get("Size").(float64); ok {
				at.Size = int64(v)
			}
			at.Inline, _ = boolean(a, "IsInline")
			m.Attachments = append(m.Attachments, at)
		}
	case nil:
		// Attachments 为 null：服务器说没有附件（HasAttachments 决定列表标记）
	default:
		m.Incomplete = append(m.Incomplete, "attachments")
	}
	if len(m.Attachments) > 0 {
		m.HasAttachment = true
	}
	return m, nil
}

func displayOf(r Recipient) string {
	if r.Name != "" {
		return r.Name
	}
	return r.Address
}

// intersect 只保留两边都缺的字段。
func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		for _, y := range b {
			if x == y {
				out = append(out, x)
				break
			}
		}
	}
	return out
}

func firstBytes(s string, n int) string {
	t, _ := truncate(s, n)
	return t
}

// truncate 按字节截断，不切断 UTF-8 字符。
func truncate(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// shortcut: 正则级 HTML 转文本（和 outlookwin / outlooklegacy 同一份），够 OWA 生成的 HTML 用；
// 需要保留表格/链接结构时换成 golang.org/x/net/html 的分词器。
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
