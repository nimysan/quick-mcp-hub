package applemail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Mailbox 是一个邮箱/文件夹。
type Mailbox struct {
	ID      int64  `json:"id"`
	URL     string `json:"url"`     // 原始 URL，如 imap://<账户UUID>/INBOX
	Scheme  string `json:"scheme"`  // imap / ews / local …
	Account string `json:"account"` // 账户 UUID（目录名）
	Path    string `json:"path"`    // 解码后的文件夹路径，如 "Archive/2024"
	Total   int64  `json:"total"`   // -1 表示库里没有该统计列
	Unread  int64  `json:"unread"`
}

// ListMailboxes 列出全部邮箱，按 URL 排序。
func (s *Store) ListMailboxes(ctx context.Context) ([]Mailbox, error) {
	total, unread := "-1", "-1"
	if s.has("mailboxes", "total_count") {
		total = "COALESCE(total_count,0)"
	}
	if s.has("mailboxes", "unread_count") {
		unread = "COALESCE(unread_count,0)"
	}
	rows, err := s.db.QueryContext(ctx, "SELECT ROWID, COALESCE(url,''), "+total+", "+unread+" FROM mailboxes ORDER BY url")
	if err != nil {
		return nil, fmt.Errorf("查询邮箱失败: %w", err)
	}
	defer rows.Close()
	var out []Mailbox
	for rows.Next() {
		var m Mailbox
		if err := rows.Scan(&m.ID, &m.URL, &m.Total, &m.Unread); err != nil {
			return nil, err
		}
		m.Scheme, m.Account, m.Path = parseMailboxURL(m.URL)
		out = append(out, m)
	}
	return out, rows.Err()
}

// parseMailboxURL 拆出 scheme、账户和解码后的路径；解析失败时尽量原样返回。
func parseMailboxURL(raw string) (scheme, account, path string) {
	segs, scheme, account, ok := mailboxSegments(raw)
	if !ok {
		return scheme, account, raw
	}
	return scheme, account, strings.Join(segs, "/")
}

// mailboxSegments 返回解码后的路径段；任何段含 / 或是 . / .. 都视为非法（防目录穿越）。
func mailboxSegments(raw string) (segs []string, scheme, account string, ok bool) {
	i := strings.Index(raw, "://")
	if i < 0 {
		return nil, "", "", false
	}
	scheme = raw[:i]
	rest := raw[i+3:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return nil, scheme, rest, false
	}
	account = rest[:j]
	if strings.ContainsAny(account, `\`) || account == "." || account == ".." {
		return nil, scheme, "", false
	}
	for _, p := range strings.Split(strings.Trim(rest[j+1:], "/"), "/") {
		if p == "" {
			continue
		}
		d, err := url.PathUnescape(p)
		if err != nil || d == "." || d == ".." || strings.ContainsAny(d, "/\\\x00") {
			return nil, scheme, account, false
		}
		segs = append(segs, d)
	}
	return segs, scheme, account, len(segs) > 0
}

// Query 是列邮件的过滤条件；零值字段不参与过滤。
type Query struct {
	MailboxID      int64     // 0 = 全部邮箱
	Since, Until   time.Time // 按接收时间，含 Since、不含 Until
	From           string    // 发件人地址或显示名包含（不区分 ASCII 大小写）
	Subject        string    // 主题包含
	Limit          int       // 默认 50，上限 500
	Offset         int
	IncludeDeleted bool
}

// Summary 是列表里的一封邮件（只来自索引，不读 .emlx）。
type Summary struct {
	ID           int64     `json:"id"`
	MailboxID    int64     `json:"mailbox_id"`
	MailboxURL   string    `json:"mailbox_url"`
	Subject      string    `json:"subject"`
	From         string    `json:"from"`
	FromName     string    `json:"from_name,omitempty"`
	DateSent     time.Time `json:"date_sent"`
	DateReceived time.Time `json:"date_received"`
	Read         bool      `json:"read"`
	Flagged      bool      `json:"flagged"`
	Size         int64     `json:"size"`
	Snippet      string    `json:"snippet,omitempty"`
}

// Page 是一页结果；HasMore 为真时用 NextOffset 取下一页。
type Page struct {
	Messages   []Summary `json:"messages"`
	HasMore    bool      `json:"has_more"`
	NextOffset int       `json:"next_offset,omitempty"`
}

const (
	defaultLimit = 50
	maxLimit     = 500
)

func (s *Store) summarySelect() string {
	subj := "''"
	switch {
	case s.cols["subjects"] != nil && s.has("messages", "subject"):
		subj = "COALESCE(s.subject,'')"
		if s.has("messages", "subject_prefix") {
			subj = "COALESCE(m.subject_prefix,'')||" + subj
		}
	case s.has("messages", "subject"):
		subj = "COALESCE(CAST(m.subject AS TEXT),'')"
	}
	col := func(c, def string) string {
		if s.has("messages", c) {
			return "COALESCE(m." + c + "," + def + ")"
		}
		return def
	}
	from, fromName := "''", "''"
	joins := " LEFT JOIN mailboxes mb ON mb.ROWID = m.mailbox"
	if s.cols["subjects"] != nil && s.has("messages", "subject") {
		joins += " LEFT JOIN subjects s ON s.ROWID = m.subject"
	}
	if s.cols["addresses"] != nil && s.has("messages", "sender") {
		joins += " LEFT JOIN addresses a ON a.ROWID = m.sender"
		from = "COALESCE(a.address,'')"
		if s.has("addresses", "comment") {
			fromName = "COALESCE(a.comment,'')"
		}
	}
	snippet := "''"
	if s.has("summaries", "summary") && s.has("messages", "summary") {
		joins += " LEFT JOIN summaries sm ON sm.ROWID = m.summary"
		snippet = "COALESCE(sm.summary,'')"
	}
	return "SELECT m.ROWID, " + col("mailbox", "0") + ", COALESCE(mb.url,''), " + subj + ", " + from + ", " + fromName +
		", " + col("date_sent", "0") + ", " + col("date_received", "0") + ", " + col("read", "0") + ", " +
		col("flagged", "0") + ", " + col("size", "0") + ", " + snippet + " FROM messages m" + joins
}

func scanSummary(sc interface{ Scan(...any) error }, s *Store) (Summary, error) {
	var m Summary
	var sent, recv, read, flagged int64
	err := sc.Scan(&m.ID, &m.MailboxID, &m.MailboxURL, &m.Subject, &m.From, &m.FromName,
		&sent, &recv, &read, &flagged, &m.Size, &m.Snippet)
	m.DateSent, m.DateReceived = s.toTime(sent), s.toTime(recv)
	m.Read, m.Flagged = read != 0, flagged != 0
	return m, err
}

func likeArg(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(v) + "%"
}

// ListMessages 按条件列邮件，按接收时间倒序分页。
func (s *Store) ListMessages(ctx context.Context, q Query) (*Page, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	offset := max(q.Offset, 0)

	var where []string
	var args []any
	if q.MailboxID != 0 {
		where, args = append(where, "m.mailbox = ?"), append(args, q.MailboxID)
	}
	if s.has("messages", "date_received") {
		if !q.Since.IsZero() {
			where, args = append(where, "m.date_received >= ?"), append(args, s.fromTime(q.Since))
		}
		if !q.Until.IsZero() {
			where, args = append(where, "m.date_received < ?"), append(args, s.fromTime(q.Until))
		}
	}
	if !q.IncludeDeleted && s.has("messages", "deleted") {
		where = append(where, "COALESCE(m.deleted,0) = 0")
	}
	if q.From != "" && s.cols["addresses"] != nil && s.has("messages", "sender") {
		cond := `a.address LIKE ? ESCAPE '\'`
		args = append(args, likeArg(q.From))
		if s.has("addresses", "comment") {
			cond = "(" + cond + ` OR a.comment LIKE ? ESCAPE '\')`
			args = append(args, likeArg(q.From))
		}
		where = append(where, cond)
	}
	if q.Subject != "" && s.cols["subjects"] != nil && s.has("messages", "subject") {
		where, args = append(where, `s.subject LIKE ? ESCAPE '\'`), append(args, likeArg(q.Subject))
	}
	sqlText := s.summarySelect()
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	order := "m.ROWID DESC"
	if s.has("messages", "date_received") {
		order = "m.date_received DESC, m.ROWID DESC"
	}
	sqlText += " ORDER BY " + order + " LIMIT ? OFFSET ?"
	args = append(args, limit+1, offset)

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("查询邮件失败: %w", err)
	}
	defer rows.Close()
	page := &Page{Messages: []Summary{}}
	for rows.Next() {
		m, err := scanSummary(rows, s)
		if err != nil {
			return nil, err
		}
		page.Messages = append(page.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Messages) > limit {
		page.Messages = page.Messages[:limit]
		page.HasMore, page.NextOffset = true, offset+limit
	}
	return page, nil
}

func (s *Store) getSummary(ctx context.Context, id int64) (Summary, error) {
	m, err := scanSummary(s.db.QueryRowContext(ctx, s.summarySelect()+" WHERE m.ROWID = ?", id), s)
	if errors.Is(err, sql.ErrNoRows) {
		return m, fmt.Errorf("%w: 邮件 id=%d", ErrNotFound, id)
	}
	return m, err
}

// dbRecipients 从 recipients 表取收件人，type 0=To 1=Cc（Bcc 忽略）。
func (s *Store) dbRecipients(ctx context.Context, id int64) (to, cc []string) {
	if s.cols["recipients"] == nil || s.cols["addresses"] == nil {
		return nil, nil
	}
	order := ""
	if s.has("recipients", "position") {
		order = " ORDER BY r.type, r.position"
	}
	name := "''"
	if s.has("addresses", "comment") {
		name = "COALESCE(a.comment,'')"
	}
	rows, err := s.db.QueryContext(ctx, "SELECT COALESCE(r.type,0), COALESCE(a.address,''), "+name+
		" FROM recipients r JOIN addresses a ON a.ROWID = r.address WHERE r.message = ?"+order, id)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	for rows.Next() {
		var typ int
		var addr, nm string
		if rows.Scan(&typ, &addr, &nm) != nil {
			continue
		}
		v := addr
		if nm != "" {
			v = nm + " <" + addr + ">"
		}
		switch typ {
		case 0:
			to = append(to, v)
		case 1:
			cc = append(cc, v)
		}
	}
	return to, cc
}

// dbAttachments 从 attachments 表取附件名（索引里没有大小）。
func (s *Store) dbAttachments(ctx context.Context, id int64) []Attachment {
	if !s.has("attachments", "name") || !s.has("attachments", "message") {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT COALESCE(name,'') FROM attachments WHERE message = ? ORDER BY ROWID", id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out = append(out, Attachment{Name: n, Size: -1})
		}
	}
	return out
}
