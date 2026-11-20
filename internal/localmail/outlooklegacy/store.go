// Package outlooklegacy 只读读取旧版 Outlook for Mac（Outlook 15 Profiles）的本地数据：
// Outlook.sqlite 提供元数据，Messages/*.olk15Message 是正文缓存，
// Message Sources/*.olk15MsgSource 是完整 MIME，Message Attachments/*.olk15MsgAttachment 是附件。
//
// 本包绝不写任何 Outlook 文件：SQLite 以 mode=ro + query_only 打开，
// 没有未合并的 WAL 时再加 immutable=1（连 -shm 都不碰）；数据文件只用 os.Open 读。
package outlooklegacy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无 cgo
)

// DefaultDataDir 返回当前用户的默认 Data 目录（Main Profile）。
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Group Containers", "UBF8T346G9.Office",
		"Outlook", "Outlook 15 Profiles", "Main Profile", "Data"), nil
}

// Options 控制打开方式。零值即推荐配置。
type Options struct {
	// Immutable: nil=自动（-wal 不存在或为空时用 immutable=1），否则按指定值。
	Immutable *bool
	// MaxBodyBytes 单封正文（纯文本）上限，<=0 用 256 KiB。
	MaxBodyBytes int
}

// Store 是一个只读的旧版 Outlook 数据源。并发安全（底层 *sql.DB）。
type Store struct {
	db        *sql.DB
	dataDir   string
	immutable bool
	maxBody   int
}

// Open 以只读方式打开 dataDir 下的 Outlook.sqlite。
func Open(dataDir string, opt Options) (*Store, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "Outlook.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("找不到旧版 Outlook 数据库: %w", err)
	}
	imm := walEmpty(dbPath)
	if opt.Immutable != nil {
		imm = *opt.Immutable
	}
	db, err := sql.Open("sqlite", dsn(dbPath, imm))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("打开 Outlook.sqlite 失败: %w", err)
	}
	maxBody := opt.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 256 << 10
	}
	return &Store{db: db, dataDir: dataDir, immutable: imm, maxBody: maxBody}, nil
}

// Immutable 报告本次是否以 immutable=1 打开。
func (s *Store) Immutable() bool { return s.immutable }

// Close 关闭数据库连接。
func (s *Store) Close() error { return s.db.Close() }

func walEmpty(dbPath string) bool {
	fi, err := os.Stat(dbPath + "-wal")
	return errors.Is(err, os.ErrNotExist) || (err == nil && fi.Size() == 0)
}

func dsn(dbPath string, immutable bool) string {
	q := url.Values{}
	q.Set("mode", "ro")
	if immutable {
		q.Set("immutable", "1")
	}
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(3000)")
	u := url.URL{Scheme: "file", Path: dbPath, RawQuery: q.Encode()}
	return u.String()
}

// resolve 把库里的相对路径（Blocks 表是 URL 编码的）映射到 dataDir 下的绝对路径，
// 拒绝逃出 dataDir 的路径。
func (s *Store) resolve(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("空路径")
	}
	if strings.Contains(rel, "%") {
		if u, err := url.PathUnescape(rel); err == nil {
			rel = u
		}
	}
	p := filepath.Join(s.dataDir, filepath.FromSlash(rel))
	if !strings.HasPrefix(p, s.dataDir+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界: %q", rel)
	}
	return p, nil
}

// cocoaZero 是 Outlook 存“无日期”时落下的 2001-01-01（Cocoa 纪元 0 转成的 Unix 秒）。
const cocoaZero = 978307200

func unixTime(v sql.NullInt64) time.Time {
	if !v.Valid || v.Int64 <= cocoaZero {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

// ---------- 文件夹 ----------

// Folder 是一个邮件文件夹。
type Folder struct {
	ID           int64  `json:"id"`
	ParentID     int64  `json:"parent_id"` // 负数表示顶层
	Name         string `json:"name"`
	Path         string `json:"path"` // 由 Folder_ParentID 链重建，"/" 分隔
	SpecialType  int    `json:"special_type"`
	Special      string `json:"special,omitempty"` // inbox / sent / drafts ...
	AccountID    int64  `json:"account_id"`
	MessageCount int    `json:"message_count"`
	Placeholder  bool   `json:"placeholder,omitempty"` // Outlook 内部占位文件夹
}

var specialNames = map[int]string{
	1: "inbox", 2: "outbox", 8: "sent", 9: "deleted", 10: "drafts",
	12: "junk", 15: "archive", 99: "on_my_computer", 103: "temporary",
}

// Folders 列出所有邮件类文件夹（Folder_FolderClass=0），按路径排序。
func (s *Store) Folders(ctx context.Context) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT f.Record_RecordID, f.Folder_ParentID, COALESCE(f.Folder_Name,''), COALESCE(f.Folder_SpecialFolderType,0),
       COALESCE(f.Record_AccountUID,0), COALESCE(f.Folder_FolderClass,0),
       (SELECT COUNT(*) FROM Mail m WHERE m.Record_FolderID = f.Record_RecordID)
FROM Folders f`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type raw struct {
		Folder
		class int
	}
	all := map[int64]*raw{}
	var order []int64
	for rows.Next() {
		var r raw
		var parent sql.NullInt64
		if err := rows.Scan(&r.ID, &parent, &r.Name, &r.SpecialType, &r.AccountID, &r.class, &r.MessageCount); err != nil {
			return nil, err
		}
		r.ParentID = parent.Int64
		if !parent.Valid {
			r.ParentID = -1
		}
		r.Special = specialNames[r.SpecialType]
		r.Placeholder = strings.HasPrefix(r.Name, "Placeholder_") && strings.HasSuffix(r.Name, "_Placeholder")
		all[r.ID] = &r
		order = append(order, r.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Folder
	for _, id := range order {
		r := all[id]
		if r.class != 0 {
			continue
		}
		var parts []string
		seen := map[int64]bool{}
		for cur := r; cur != nil && !seen[cur.ID]; cur = all[cur.ParentID] {
			seen[cur.ID] = true
			name := cur.Name
			if name == "" {
				name = fmt.Sprintf("#%d", cur.ID)
			}
			parts = append([]string{name}, parts...)
		}
		r.Path = strings.Join(parts, "/")
		out = append(out, r.Folder)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ---------- 列表 ----------

// Query 是列表过滤条件，零值字段不过滤。
type Query struct {
	FolderID int64     // 0 = 全部文件夹
	Since    time.Time // 按接收时间，含
	Until    time.Time // 不含
	From     string    // 发件人显示名或地址子串（不区分 ASCII 大小写）
	Subject  string    // 主题子串
	Limit    int       // 默认 50，最大 200
	Offset   int
}

// Summary 是列表里的一封邮件（只来自 Outlook.sqlite，不读文件）。
type Summary struct {
	ID                  int64     `json:"id"`
	FolderID            int64     `json:"folder_id"`
	Subject             string    `json:"subject"` // Outlook 的 NormalizedSubject，已去掉 RE:/FW: 前缀
	From                string    `json:"from"`
	FromAddress         string    `json:"from_address"`
	To                  []string  `json:"to,omitempty"`
	Cc                  []string  `json:"cc,omitempty"`
	Received            time.Time `json:"received"`
	Sent                time.Time `json:"sent"`
	HasAttachment       bool      `json:"has_attachment"`
	Read                bool      `json:"read"`
	Preview             string    `json:"preview,omitempty"`
	PartiallyDownloaded bool      `json:"partially_downloaded,omitempty"`
}

// Page 是一页列表结果。
type Page struct {
	Items      []Summary `json:"items"`
	Total      int       `json:"total"`
	NextOffset int       `json:"next_offset"` // -1 表示没有下一页
}

const summaryCols = `Record_RecordID, Record_FolderID, Message_NormalizedSubject, Message_SenderList,
 Message_SenderAddressList, Message_ToRecipientAddressList, Message_CCRecipientAddressList,
 Message_TimeReceived, Message_TimeSent, Message_HasAttachment, Message_ReadFlag, Message_Preview,
 Message_PartiallyDownloaded`

func likeArg(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// List 按条件分页列出邮件，按接收时间倒序。
func (s *Store) List(ctx context.Context, q Query) (Page, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	var where []string
	var args []any
	if q.FolderID != 0 {
		where = append(where, "Record_FolderID = ?")
		args = append(args, q.FolderID)
	}
	if !q.Since.IsZero() {
		where = append(where, "Message_TimeReceived >= ?")
		args = append(args, q.Since.Unix())
	}
	if !q.Until.IsZero() {
		where = append(where, "Message_TimeReceived < ?")
		args = append(args, q.Until.Unix())
	}
	if q.From != "" {
		where = append(where, `(Message_SenderList LIKE ? ESCAPE '\' OR Message_SenderAddressList LIKE ? ESCAPE '\')`)
		args = append(args, likeArg(q.From), likeArg(q.From))
	}
	if q.Subject != "" {
		where = append(where, `Message_NormalizedSubject LIKE ? ESCAPE '\'`)
		args = append(args, likeArg(q.Subject))
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	var p Page
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM Mail"+cond, args...).Scan(&p.Total); err != nil {
		return p, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+summaryCols+" FROM Mail"+cond+
		" ORDER BY Message_TimeReceived DESC, Record_RecordID DESC LIMIT ? OFFSET ?",
		append(args, q.Limit, q.Offset)...)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	p.Items = []Summary{}
	for rows.Next() {
		sm, err := scanSummary(rows)
		if err != nil {
			return p, err
		}
		p.Items = append(p.Items, sm)
	}
	p.NextOffset = -1
	if q.Offset+len(p.Items) < p.Total {
		p.NextOffset = q.Offset + len(p.Items)
	}
	return p, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanSummary(r scanner, extra ...any) (Summary, error) {
	var sm Summary
	var subj, from, fromAddr, to, cc, preview sql.NullString
	var recv, sent sql.NullInt64
	var hasAtt, read, partial sql.NullBool
	dst := []any{&sm.ID, &sm.FolderID, &subj, &from, &fromAddr, &to, &cc, &recv, &sent, &hasAtt, &read, &preview, &partial}
	if err := r.Scan(append(dst, extra...)...); err != nil {
		return sm, err
	}
	sm.Subject = subj.String
	sm.From = strings.TrimSpace(from.String)
	sm.FromAddress = firstAddr(fromAddr.String)
	sm.To = splitAddrs(to.String)
	sm.Cc = splitAddrs(cc.String)
	sm.Received = unixTime(recv)
	sm.Sent = unixTime(sent)
	sm.HasAttachment = hasAtt.Bool
	sm.Read = read.Bool
	sm.Preview = strings.TrimSpace(preview.String)
	sm.PartiallyDownloaded = partial.Bool
	return sm, nil
}

// 地址列表字段格式是 "a@x; b@y; "（实测）。
func splitAddrs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstAddr(s string) string {
	if a := splitAddrs(s); len(a) > 0 {
		return a[0]
	}
	return ""
}
