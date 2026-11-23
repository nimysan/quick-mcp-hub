package outlookwin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// 正文来源。
const (
	BodyText = "text" // PidTagBody 纯文本
	BodyHTML = "html" // PidTagHtml 转成的纯文本
	BodyRTF  = "rtf"  // PidTagRtfCompressed 解压后粗略去掉 RTF 控制字
	BodyNone = "none"
)

// Attachment 只描述附件，不给内容。
type Attachment struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"` // -1 表示未知
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
	BodySource    string       `json:"body_source"`
	BodyTruncated bool         `json:"body_truncated"`
	Attachments   []Attachment `json:"attachments,omitempty"`
}

// Get 取一封邮件。id 是 List 返回的 ID（邮件的 NID）。
func (s *Store) Get(ctx context.Context, id int64) (*Message, error) {
	if err := s.load(); err != nil {
		return nil, wrap(err)
	}
	if id <= 0 || id > 0xFFFFFFFF || uint32(id)&0x1F != nidTypeNormalMessage {
		return nil, fmt.Errorf("%w: 邮件 %d", ErrNotFound, id)
	}
	n, parent, err := s.p.openNode(uint32(id))
	if errors.Is(err, errNoNode) {
		return nil, fmt.Errorf("%w: 邮件 %d", ErrNotFound, id)
	}
	if err != nil {
		return nil, wrap(err)
	}
	pc, err := readPC(n)
	if err != nil {
		return nil, wrap(err)
	}
	cp := s.cp(pc)
	m := &Message{Summary: summaryOf(pc, cp)}
	m.ID, m.FolderID = id, int64(parent)
	m.MessageID = pc.str(pidInternetMessageID, cp)
	m.Body, m.BodySource = s.body(pc, cp)
	m.Body, m.BodyTruncated = truncate(m.Body, s.maxBody)
	m.Preview = preview(firstBytes(m.Body, 2048))

	if rt, err := n.sub(nidRecipientTable); err == nil {
		if t, err := readTC(rt); err == nil {
			for _, r := range t.bags() {
				m.Recipients = append(m.Recipients, recipientOf(r, cp))
			}
		}
	}
	if len(m.Recipients) > 0 { // 收件人表比 DisplayTo/Cc 更准：有地址
		m.To, m.Cc = nil, nil
		for _, r := range m.Recipients {
			v := r.Name
			if r.Address != "" && r.Address != r.Name {
				v = fmt.Sprintf("%s <%s>", r.Name, r.Address)
			}
			switch r.Type {
			case "to":
				m.To = append(m.To, v)
			case "cc":
				m.Cc = append(m.Cc, v)
			}
		}
	}
	if at, err := n.sub(nidAttachTable); err == nil {
		if t, err := readTC(at); err == nil {
			for _, r := range t.bags() {
				m.Attachments = append(m.Attachments, attachmentOf(n, r, cp))
			}
		}
	}
	if len(m.Attachments) > 0 {
		m.HasAttachment = true
	}
	return m, nil
}

func recipientOf(b propBag, cp int) Recipient {
	r := Recipient{Name: b.str(pidDisplayName, cp)}
	switch t, _ := b.int(pidRecipientType); t & 0x0F {
	case 2:
		r.Type = "cc"
	case 3:
		r.Type = "bcc"
	default:
		r.Type = "to"
	}
	r.Address = b.str(pidSmtpAddress, cp)
	if r.Address == "" && strings.EqualFold(b.str(pidAddrType, cp), "SMTP") {
		r.Address = b.str(pidEmailAddress, cp)
	}
	if r.Name == "" {
		r.Name = r.Address
	}
	return r
}

// attachmentOf 先用附件表的列；表里没有文件名时打开附件对象的 PC 再找。
func attachmentOf(msg *nodeData, row propBag, cp int) Attachment {
	a := Attachment{Size: -1}
	fill := func(b propBag) {
		if a.Name == "" {
			for _, id := range []uint16{pidAttachLongFilename, pidAttachFilename, pidDisplayName} {
				if v := b.str(id, cp); v != "" {
					a.Name = v
					break
				}
			}
		}
		if a.ContentType == "" {
			a.ContentType = b.str(pidAttachMimeTag, cp)
		}
		if a.Size < 0 {
			if v, ok := b.int(pidAttachSize); ok {
				a.Size = v
			}
		}
	}
	fill(row)
	if a.Name == "" || a.ContentType == "" {
		if v, ok := row.int(pidLtpRowID); ok {
			if an, err := msg.sub(uint32(v)); err == nil {
				if pc, err := readPC(an); err == nil {
					fill(pc)
				}
			}
		}
	}
	return a
}

// body 按 纯文本 → HTML → RTF 的顺序取正文。
func (s *Store) body(pc propBag, cp int) (string, string) {
	if v := pc.str(pidBody, cp); strings.TrimSpace(v) != "" {
		return strings.ReplaceAll(v, "\r\n", "\n"), BodyText
	}
	if h := pc.bin(pidHTML); len(h) > 0 {
		hcp := cp
		if v, ok := pc.int(pidInternetCodepage); ok && v > 0 {
			hcp = int(v)
		}
		return htmlToText(decode8(h, hcp)), BodyHTML
	}
	if r := pc.bin(pidRtfCompressed); len(r) > 0 {
		if raw, err := decompressRTF(r); err == nil {
			return rtfToText(raw, cp), BodyRTF
		}
	}
	return "", BodyNone
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
