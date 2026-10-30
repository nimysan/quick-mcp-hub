// Package mailbox 封装 IMAP 读取与 SMTP 发送。
package mailbox

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net"
	"runtime"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomsgmail "github.com/emersion/go-message/mail"
	"github.com/nimysan/mcp-hub/internal/config"

	// 导入即把 message.CharsetReader 设为 go-message 的全套字符集解码器。
	// 国内邮件常用 GB2312/GBK/GB18030，不注册就是一片乱码。
	"github.com/emersion/go-message/charset"
)

// Summary 是一封邮件的信头摘要，不含正文。
type Summary struct {
	UID     uint32   `json:"uid" jsonschema:"邮件在该邮箱内的唯一编号，read_mail 用它取正文"`
	Subject string   `json:"subject" jsonschema:"邮件主题"`
	From    string   `json:"from" jsonschema:"发件人"`
	To      []string `json:"to,omitempty" jsonschema:"收件人列表"`
	Date    string   `json:"date" jsonschema:"发信时间，RFC3339"`
	Size    int64    `json:"sizeBytes" jsonschema:"邮件总字节数"`
	Unread  bool     `json:"unread" jsonschema:"是否未读"`
}

// Message 是一封邮件的完整内容。
type Message struct {
	Summary
	Text        string   `json:"text" jsonschema:"纯文本正文，已做字符集解码"`
	Truncated   bool     `json:"truncated" jsonschema:"正文是否因超长被截断"`
	Attachments []string `json:"attachments,omitempty" jsonschema:"附件文件名列表，不含内容"`
}

// SearchQuery 是 search_mail 的检索条件，字段之间是「且」关系。
type SearchQuery struct {
	From    string
	Keyword string
	Since   time.Time
	Before  time.Time
	Unread  bool
}

// session 是一次已登录并已选中邮箱的 IMAP 连接。
type session struct {
	c   *imapclient.Client
	cfg config.Config
}

// connect 建立 TLS 连接、发 ID、登录。
//
// ID 命令（RFC 2971）必须发：163/126 不发 ID 会直接拒登（"Unsafe Login"），
// QQ 目前宽容但不保证，无条件发代价为零。
func connect(cfg config.Config) (*session, error) {
	// 第一次走到这里才去系统密码管理器取授权码；之后整个进程复用缓存。
	password, err := cfg.Password()
	if err != nil {
		return nil, err
	}

	opts := &imapclient.Options{
		TLSConfig:   imapTLS(cfg),
		Dialer:      &net.Dialer{Timeout: cfg.Timeout},
		WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader},
	}

	c, err := imapclient.DialTLS(cfg.IMAPAddr(), opts)
	if err != nil && !cfg.LegacyRSAKeyExchange && handshakeFailed(err) {
		// 这台服务器只给静态 RSA 套件（老 Dovecot 的常见配置）。降级重试一次，
		// 并把这件事写进日志 —— 这条连接没有前向保密，不该悄悄发生。
		logf("IMAP %s 不接受默认加密套件，降级重试（这条连接无前向保密）", cfg.IMAPHost)
		opts.TLSConfig = tlsConfigFor(cfg.IMAPHost, true)
		c, err = imapclient.DialTLS(cfg.IMAPAddr(), opts)
	}
	if err != nil {
		return nil, fmt.Errorf("连接 IMAP %s 失败: %w", cfg.IMAPAddr(), err)
	}

	if _, err := c.ID(&imap.IDData{
		Name:      "mcp-hub",
		Version:   Version,
		OS:        runtime.GOOS,
		Vendor:    "mcp-hub",
		Address:   cfg.User,
		Command:   "mcp-hub",
		Arguments: "mcp-stdio",
	}).Wait(); err != nil {
		// ID 失败不致命（服务器可能不支持该扩展），继续登录。
		logf("IMAP ID 命令未被接受（服务器可能不支持），继续登录: %v", err)
	}

	if err := c.Login(cfg.User, password).Wait(); err != nil {
		c.Close()
		return nil, loginError("IMAP", cfg, err)
	}
	return &session{c: c, cfg: cfg}, nil
}

func (s *session) close() {
	if err := s.c.Logout().Wait(); err != nil {
		logf("IMAP logout: %v", err)
	}
	s.c.Close()
}

// selectBox 选中邮箱，只读打开以免改动已读标记。
func (s *session) selectBox(name string) (*imap.SelectData, error) {
	if name == "" {
		name = "INBOX"
	}
	data, err := s.c.Select(name, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil, fmt.Errorf("选中邮箱 %q 失败: %w", name, err)
	}
	return data, nil
}

var summaryFetch = &imap.FetchOptions{
	UID: true, Envelope: true, Flags: true, InternalDate: true, RFC822Size: true,
}

// ListRecent 返回邮箱里最近 limit 封邮件的摘要，最新的在前。
func ListRecent(cfg config.Config, mailbox string, limit int) ([]Summary, error) {
	s, err := connect(cfg)
	if err != nil {
		return nil, err
	}
	defer s.close()

	box, err := s.selectBox(mailbox)
	if err != nil {
		return nil, err
	}
	if box.NumMessages == 0 {
		return []Summary{}, nil
	}

	hi := box.NumMessages
	lo := uint32(1)
	if uint32(limit) < hi {
		lo = hi - uint32(limit) + 1
	}
	var set imap.SeqSet
	set.AddRange(lo, hi)

	msgs, err := s.c.Fetch(set, summaryFetch).Collect()
	if err != nil {
		return nil, fmt.Errorf("拉取邮件摘要失败: %w", err)
	}
	return toSummaries(msgs), nil
}

// Search 按条件检索邮件，返回最近 limit 封的摘要，最新的在前。
func Search(cfg config.Config, mailbox string, q SearchQuery, limit int) ([]Summary, error) {
	s, err := connect(cfg)
	if err != nil {
		return nil, err
	}
	defer s.close()

	if _, err := s.selectBox(mailbox); err != nil {
		return nil, err
	}

	crit := &imap.SearchCriteria{}
	if q.From != "" {
		crit.Header = append(crit.Header, imap.SearchCriteriaHeaderField{Key: "From", Value: q.From})
	}
	if q.Keyword != "" {
		crit.Text = append(crit.Text, q.Keyword)
	}
	if !q.Since.IsZero() {
		crit.Since = q.Since
	}
	if !q.Before.IsZero() {
		crit.Before = q.Before
	}
	if q.Unread {
		crit.NotFlag = append(crit.NotFlag, imap.FlagSeen)
	}

	data, err := s.c.UIDSearch(crit, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("IMAP 检索失败: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return []Summary{}, nil
	}
	if len(uids) > limit {
		uids = uids[len(uids)-limit:] // UID 递增，尾部是最新的
	}

	msgs, err := s.c.Fetch(imap.UIDSetNum(uids...), summaryFetch).Collect()
	if err != nil {
		return nil, fmt.Errorf("拉取检索结果摘要失败: %w", err)
	}
	return toSummaries(msgs), nil
}

// Read 按 UID 取一封邮件的纯文本正文。
//
// BODY.PEEK[] 不设 \Seen 标记，所以本操作对邮箱是只读的。
func Read(cfg config.Config, mailbox string, uid uint32) (*Message, error) {
	s, err := connect(cfg)
	if err != nil {
		return nil, err
	}
	defer s.close()

	if _, err := s.selectBox(mailbox); err != nil {
		return nil, err
	}

	section := &imap.FetchItemBodySection{Peek: true}
	msgs, err := s.c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{
		UID: true, Envelope: true, Flags: true, InternalDate: true, RFC822Size: true,
		BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("拉取邮件 UID %d 失败: %w", uid, err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("邮箱 %q 中找不到 UID %d", mailbox, uid)
	}

	buf := msgs[0]
	out := &Message{Summary: toSummary(buf)}
	text, atts, err := parseBody(buf.FindBodySection(section), cfg.MaxBodyBytes)
	if err != nil {
		return nil, err
	}
	out.Text, out.Attachments = text, atts
	if len(out.Text) >= cfg.MaxBodyBytes {
		out.Truncated = true
	}
	return out, nil
}

// parseBody 走一遍 MIME 树，取出 text/plain（没有就退回 text/html 的原文），
// 并列出附件文件名。字符集由 go-message/charset 自动解码为 UTF-8。
func parseBody(raw []byte, maxBytes int) (string, []string, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	mr, err := gomsgmail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return "", nil, fmt.Errorf("解析 MIME 失败: %w", err)
	}
	defer mr.Close()

	var plain, html strings.Builder
	var atts []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			logf("跳过无法解析的 MIME 分段: %v", err)
			break
		}
		switch h := part.Header.(type) {
		case *gomsgmail.InlineHeader:
			ct, _, _ := h.ContentType()
			target := &plain
			if ct == "text/html" {
				target = &html
			} else if ct != "text/plain" && ct != "" {
				continue
			}
			if target.Len() >= maxBytes {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(part.Body, int64(maxBytes-target.Len())))
			if err != nil {
				logf("读取正文分段失败: %v", err)
				continue
			}
			target.Write(b)
		case *gomsgmail.AttachmentHeader:
			name, _ := h.Filename()
			if name == "" {
				name = "(未命名附件)"
			}
			atts = append(atts, name)
		}
	}

	text := strings.TrimSpace(plain.String())
	if text == "" {
		text = strings.TrimSpace(html.String())
	}
	return text, atts, nil
}

func toSummaries(msgs []*imapclient.FetchMessageBuffer) []Summary {
	out := make([]Summary, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- { // 倒序：最新的在前
		out = append(out, toSummary(msgs[i]))
	}
	return out
}

func toSummary(b *imapclient.FetchMessageBuffer) Summary {
	s := Summary{UID: uint32(b.UID), Size: b.RFC822Size, Unread: true}
	for _, f := range b.Flags {
		if f == imap.FlagSeen {
			s.Unread = false
		}
	}
	if e := b.Envelope; e != nil {
		s.Subject = e.Subject
		s.From = joinAddrs(e.From)
		if to := e.To; len(to) > 0 {
			s.To = strings.Split(joinAddrs(to), ", ")
		}
		if !e.Date.IsZero() {
			s.Date = e.Date.Format(time.RFC3339)
		}
	}
	if s.Date == "" && !b.InternalDate.IsZero() {
		s.Date = b.InternalDate.Format(time.RFC3339)
	}
	return s
}

func joinAddrs(addrs []imap.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		addr := a.Addr()
		if a.Name != "" {
			addr = fmt.Sprintf("%s <%s>", a.Name, addr)
		}
		parts = append(parts, addr)
	}
	return strings.Join(parts, ", ")
}
