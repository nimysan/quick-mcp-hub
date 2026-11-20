package applemail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // 注册 GB18030/GBK/Big5 等字符集解码
	gomail "github.com/emersion/go-message/mail"
)

// Attachment 只描述附件，不返回内容。
type Attachment struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"` // 解码后字节数；-1 表示未知
	ContentType string `json:"content_type,omitempty"`
	// External 为真表示附件本体不在 .emlx 里，而在旁边的 Attachments/ 目录。
	External bool `json:"external,omitempty"`
	stripped bool // .partial.emlx 里只剩头、本体被剥离的分段
}

// Message 是一封完整邮件：索引元数据 + .emlx 里解析出的头和纯文本正文。
type Message struct {
	Summary
	To          []string     `json:"to,omitempty"`
	Cc          []string     `json:"cc,omitempty"`
	MessageID   string       `json:"message_id,omitempty"`
	Text        string       `json:"text"`
	FromHTML    bool         `json:"from_html,omitempty"` // 正文由 HTML 转换而来
	Truncated   bool         `json:"truncated,omitempty"`
	Attachments []Attachment `json:"attachments"`
	Partial     bool         `json:"partial,omitempty"` // 来自 .partial.emlx（附件已剥离）
	// BodyError 非空表示索引里有这封信但正文读不出（.emlx 不在本机、只下了头等）。
	BodyError string `json:"body_error,omitempty"`
}

// DefaultMaxBodyBytes 是 GetMessage 正文的默认上限。
const DefaultMaxBodyBytes = 64 << 10

// GetMessage 取一封邮件。maxBodyBytes<=0 时用 DefaultMaxBodyBytes。
// 索引里没有该 id 返回 ErrNotFound；.emlx 缺失只填 BodyError，不算错误。
// TCC 拒绝读 .emlx 时返回 ErrFullDiskAccess。
func (s *Store) GetMessage(ctx context.Context, id int64, maxBodyBytes int) (*Message, error) {
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultMaxBodyBytes
	}
	sum, err := s.getSummary(ctx, id)
	if err != nil {
		return nil, err
	}
	msg := &Message{Summary: sum}
	fallback := func(reason string) (*Message, error) {
		msg.BodyError = reason
		msg.To, msg.Cc = s.dbRecipients(ctx, id)
		msg.Attachments = s.dbAttachments(ctx, id)
		if msg.Attachments == nil {
			msg.Attachments = []Attachment{}
		}
		return msg, nil
	}

	path, err := s.locateEmlx(sum.MailboxURL, id)
	if err != nil {
		if errors.Is(err, ErrFullDiskAccess) {
			return nil, err
		}
		return fallback("本机没有这封信的 .emlx 正文文件（可能只同步了邮件头）: " + err.Error())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if e := wrapFSErr(path, err); errors.Is(e, ErrFullDiskAccess) {
			return nil, e
		}
		return fallback("读取 " + path + " 失败: " + err.Error())
	}
	msg.Partial = strings.HasSuffix(path, ".partial.emlx")

	p, err := parseRFC822(emlxPayload(data), maxBodyBytes)
	if err != nil {
		return fallback(err.Error())
	}
	msg.To, msg.Cc, msg.MessageID = p.to, p.cc, p.messageID
	if len(msg.To) == 0 && len(msg.Cc) == 0 {
		msg.To, msg.Cc = s.dbRecipients(ctx, id)
	}
	if msg.Subject == "" {
		msg.Subject = p.subject
	}
	msg.Text, msg.FromHTML, msg.Truncated = p.text, p.fromHTML, p.truncated
	msg.Attachments = mergeExternal(p.attachments, externalAttachments(path, id))
	return msg, nil
}

// emlxPayload 去掉首行字节数和尾部 plist，返回 RFC 822 原文。
// 首行不是数字时按普通 .eml 处理，整份返回。
func emlxPayload(data []byte) []byte {
	nl := bytes.IndexByte(data, '\n')
	if nl <= 0 {
		return data
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data[:nl])))
	if err != nil || n < 0 {
		return data
	}
	body := data[nl+1:]
	if n < len(body) {
		body = body[:n]
	}
	return body
}

type parsed struct {
	subject, messageID string
	to, cc             []string
	text               string
	fromHTML           bool
	truncated          bool
	attachments        []Attachment
}

func parseRFC822(raw []byte, maxBody int) (*parsed, error) {
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) {
		return nil, fmt.Errorf("解析 MIME 失败: %w", err)
	}
	if mr == nil {
		return nil, fmt.Errorf("解析 MIME 失败: %v", err)
	}
	defer mr.Close()

	p := &parsed{attachments: []Attachment{}}
	h := mr.Header
	p.subject, _ = h.Subject()
	p.messageID, _ = h.MessageID()
	p.to = addrList(h, "To")
	p.cc = addrList(h, "Cc")

	var plain, html bytes.Buffer
	htmlLimit := maxBody * 4 // HTML 转文本会缩水，多读一些
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil && !message.IsUnknownCharset(err) {
			break // 后面的分段坏了，保留已解析的部分
		}
		if part == nil {
			break
		}
		var mh message.Header
		var name string
		isAttachment := false
		switch hh := part.Header.(type) {
		case *gomail.AttachmentHeader:
			mh = hh.Header
			name, _ = hh.Filename()
			isAttachment = true
		case *gomail.InlineHeader:
			mh = hh.Header
		}
		ct, params, _ := mh.ContentType()
		if !isAttachment && ct != "" && ct != "text/plain" && ct != "text/html" {
			// 内嵌图片等：Content-Disposition: inline 但不是正文
			_, dp, _ := mh.ContentDisposition()
			name, isAttachment = dp["filename"], true
		}
		if name == "" && isAttachment {
			name = params["name"]
		}
		if isAttachment {
			if name == "" {
				name = "(未命名附件)"
			}
			n, _ := io.Copy(io.Discard, part.Body)
			stripped := n == 0
			if stripped {
				n = appleContentLength(&mh)
			}
			p.attachments = append(p.attachments, Attachment{Name: name, Size: n, ContentType: ct, stripped: stripped})
			continue
		}
		target, limit := &plain, maxBody
		if ct == "text/html" {
			target, limit = &html, htmlLimit
		}
		if target.Len() >= limit {
			p.truncated = true
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(part.Body, int64(limit-target.Len()+1)))
		if target.Len() > 0 {
			target.WriteString("\n")
		}
		target.Write(b)
	}

	text := strings.TrimSpace(plain.String())
	if text == "" && html.Len() > 0 {
		text, p.fromHTML = htmlToText(html.String()), true
		if html.Len() > htmlLimit {
			p.truncated = true
		}
	}
	if len(text) > maxBody {
		text, p.truncated = truncateUTF8(text, maxBody), true
	}
	p.text = text
	return p, nil
}

// appleContentLength 读 .partial.emlx 里被剥离分段上的 X-Apple-Content-Length。
func appleContentLength(h interface{ Get(string) string }) int64 {
	if v := strings.TrimSpace(h.Get("X-Apple-Content-Length")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return -1
}

func addrList(h gomail.Header, key string) []string {
	as, err := h.AddressList(key)
	if err != nil || len(as) == 0 {
		if v, _ := h.Text(key); v != "" {
			return []string{v}
		}
		return nil
	}
	out := make([]string, 0, len(as))
	for _, a := range as {
		if a.Name != "" {
			out = append(out, a.Name+" <"+a.Address+">")
		} else {
			out = append(out, a.Address)
		}
	}
	return out
}

// externalAttachments 列出 <Messages 的上级>/Attachments/<id>/ 下的文件。
func externalAttachments(emlxPath string, id int64) []Attachment {
	dir := filepath.Join(filepath.Dir(filepath.Dir(emlxPath)), "Attachments", strconv.FormatInt(id, 10))
	var out []Attachment
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if info, err := d.Info(); err == nil {
			out = append(out, Attachment{Name: d.Name(), Size: info.Size(), External: true})
		}
		return nil
	})
	return out
}

func mergeExternal(mime, ext []Attachment) []Attachment {
	for _, e := range ext {
		merged := false
		for i := range mime {
			// 外置文件的真实大小优先于 X-Apple-Content-Length
			if mime[i].Name == e.Name && (mime[i].Size <= 0 || mime[i].stripped) {
				mime[i].Size, mime[i].External, merged = e.Size, true, true
				break
			}
		}
		if !merged {
			mime = append(mime, e)
		}
	}
	return mime
}

// locateEmlx 找到 ROWID 对应的 .emlx / .partial.emlx。先按布局直接拼路径，
// 再在该邮箱目录内有限深度地扫描。所有候选路径都在 s.base 之内。
func (s *Store) locateEmlx(mailboxURL string, id int64) (string, error) {
	segs, scheme, account, ok := mailboxSegments(mailboxURL)
	if !ok {
		return "", fmt.Errorf("%w: 无法解析邮箱 URL %q", ErrNotFound, mailboxURL)
	}
	idStr := strconv.FormatInt(id, 10)
	names := []string{idStr + ".emlx", idStr + ".partial.emlx"}

	var roots []string
	if account != "" {
		roots = append(roots, filepath.Join(s.base, account))
	}
	if scheme == "local" || account == "" {
		roots = append(roots, filepath.Join(s.base, "Mailboxes"))
	}
	var mboxDirs []string
	for _, r := range roots {
		mboxDirs = append(mboxDirs, mboxPath(r, segs))
	}
	// 账户目录名和 URL 里的 id 对不上时，退回到所有顶层目录下找同名邮箱路径。
	if entries, err := os.ReadDir(s.base); err == nil {
		for _, e := range entries {
			if e.IsDir() && e.Name() != "MailData" && !versionDirRe.MatchString(e.Name()) {
				d := mboxPath(filepath.Join(s.base, e.Name()), segs)
				if !contains(mboxDirs, d) {
					mboxDirs = append(mboxDirs, d)
				}
			}
		}
	} else if errors.Is(err, fs.ErrPermission) {
		return "", wrapFSErr(s.base, err)
	}

	for _, d := range mboxDirs {
		if p, err := directCandidate(d, idStr, names); p != "" || err != nil {
			return p, err
		}
	}
	for _, d := range mboxDirs {
		if p := scanMbox(d, names); p != "" {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: 邮件 %d 的 .emlx（邮箱 %s）", ErrNotFound, id, mailboxURL)
}

func mboxPath(root string, segs []string) string {
	p := root
	for _, s := range segs {
		p = filepath.Join(p, s+".mbox")
	}
	return p
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// bucketSegments：ROWID 去掉末三位后逐位倒序。194184 → [4 9 1]；<1000 → 空。
func bucketSegments(id string) []string {
	if len(id) <= 3 {
		return nil
	}
	head := id[:len(id)-3]
	out := make([]string, 0, len(head))
	for i := len(head) - 1; i >= 0; i-- {
		out = append(out, head[i:i+1])
	}
	return out
}

func directCandidate(mbox, id string, names []string) (string, error) {
	try := func(dir string) (string, error) {
		for _, n := range names {
			p := filepath.Join(dir, n)
			_, err := os.Stat(p)
			if err == nil {
				return p, nil
			}
			if errors.Is(err, fs.ErrPermission) {
				return "", wrapFSErr(p, err)
			}
		}
		return "", nil
	}
	if p, err := try(filepath.Join(mbox, "Messages")); p != "" || err != nil {
		return p, err
	}
	entries, err := os.ReadDir(mbox)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return "", wrapFSErr(mbox, err)
		}
		return "", nil
	}
	bucket := filepath.Join(bucketSegments(id)...)
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".mbox") {
			continue
		}
		child := filepath.Join(mbox, e.Name())
		for _, dir := range []string{
			filepath.Join(child, "Data", bucket, "Messages"),
			filepath.Join(child, "Messages"),
		} {
			if p, err := try(dir); p != "" || err != nil {
				return p, err
			}
		}
	}
	return "", nil
}

// scanMbox 在一个邮箱目录内（不进入子邮箱 .mbox）有限深度地找文件名。
// shortcut: 每次线性扫描，大邮箱慢；需要时再加按邮箱缓存的 ROWID→路径索引。
func scanMbox(mbox string, names []string) string {
	const maxDepth = 9
	var found string
	_ = filepath.WalkDir(mbox, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != mbox && strings.HasSuffix(d.Name(), ".mbox") {
				return filepath.SkipDir
			}
			if rel, _ := filepath.Rel(mbox, p); strings.Count(rel, string(filepath.Separator)) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if contains(names, d.Name()) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
