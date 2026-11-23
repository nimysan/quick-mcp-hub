// Package olkwin 只读读取新版 Outlook for Windows（olk.exe）的离线缓存。
//
// 新版 Outlook 是 WebView2 应用，离线邮件存在 Chromium 的 IndexedDB 里：
// %LOCALAPPDATA%\Microsoft\Olk\EBWebView\Default\IndexedDB\https_outlook.office.com_0.indexeddb.leveldb。
// 每个账户一个库 owa-offline-data-<账户 UUID>，里面的对象仓库：
//   - folders：文件夹（id、parentFolderId、displayName、distinguishedFolderType、FolderClass、计数）
//   - messages：列表行 ItemRow（主题、发件人、DisplayTo、收发时间、已读、有无附件、摘要、所在文件夹）
//   - messageBodies：完整邮件 Message（正文、收件人、抄送、Message-ID、附件元数据）
//
// 值是 Blink/V8 结构化序列化的明文，没有 DPAPI 或其他加密（2026-11 在 olk 1.2026.1103.200 上实测）。
// 只缓存离线窗口内（默认 30 天）的邮件，附件内容不在这里。
//
// 读取方式：自己解析 LevelDB 文件（不用 goleveldb 的 DB.Open，那会写 LOCK/LOG/MANIFEST），
// 每个文件以允许读写删共享的方式只读打开，读完就关；不碰 LOCK 文件。olk 运行时也能读。
package olkwin

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Options 是打开选项。
type Options struct {
	// MaxBodyBytes 限制 Get 返回的正文字节数，0 表示默认 256 KB。
	MaxBodyBytes int
}

// Folder 是一个文件夹。
type Folder struct {
	ID           int64  `json:"id"`        // 文件夹 ID 的稳定哈希（53 位）
	ParentID     int64  `json:"parent_id"` // 0 表示顶层
	Name         string `json:"name"`
	Path         string `json:"path"` // "/" 分隔；多个账户时以账户地址开头
	Special      string `json:"special,omitempty"`
	Class        string `json:"class,omitempty"`
	Mail         bool   `json:"mail"`
	MessageCount int    `json:"message_count"` // 本地缓存里的封数
	UnreadCount  int    `json:"unread_count"`  // 本地缓存里的未读数
	ServerTotal  int    `json:"server_total"`  // 服务器上的总封数（缓存窗口外的不在本地）
	Account      string `json:"account,omitempty"`
	FolderID     string `json:"folder_id"` // Exchange 文件夹 ID
}

// Query 是 List 的条件。
type Query struct {
	FolderID int64     // 0 = 全部邮件文件夹
	Since    time.Time // 按接收时间，含
	Until    time.Time // 不含
	From     string    // 发件人显示名或地址子串（不区分大小写）
	Subject  string    // 主题子串（不区分大小写）
	Limit    int       // 默认 50，最大 200
	Offset   int
}

// Summary 是列表里的一封邮件。
type Summary struct {
	ID            int64     `json:"id"`      // ItemId 的稳定哈希（53 位，JSON 安全）
	ItemID        string    `json:"item_id"` // Exchange ItemId
	FolderID      int64     `json:"folder_id"`
	Subject       string    `json:"subject"`
	From          string    `json:"from"`
	FromAddress   string    `json:"from_address"`
	To            []string  `json:"to,omitempty"`
	Cc            []string  `json:"cc,omitempty"`
	Received      time.Time `json:"received"`
	Sent          time.Time `json:"sent"`
	HasAttachment bool      `json:"has_attachment"`
	Read          bool      `json:"read"`
	Preview       string    `json:"preview,omitempty"`
	MessageClass  string    `json:"message_class,omitempty"`
	// Incomplete 列出缓存里没有、因而没给出的字段（例如 "received"、"from"、"body"）。不猜。
	Incomplete []string `json:"incomplete,omitempty"`
}

// Page 是 List 的一页结果。
type Page struct {
	Items      []Summary `json:"items"`
	Total      int       `json:"total"`
	NextOffset int       `json:"next_offset"` // -1 表示没有下一页
}

// Store 是某一时刻的只读快照。数据有变化时 Stale 返回 true，由调用方重开。
type Store struct {
	root    string
	maxBody int
	sig     string

	dbs     []*idbStore
	folders []Folder
	byID    map[int64]*Folder
	index   []Summary        // 全部邮件，按接收时间倒序
	byMsg   map[int64]msgRef // 邮件 ID → 正文记录位置
	skipped int              // 解不开、被跳过的记录数

}

type msgRef struct {
	db      *idbStore
	dbID    int64
	bodies  int64 // messageBodies 仓库 ID，0 表示没有
	rec     idbRecord
	hasBody bool
	summary int // 在 index 里的下标
}

// DefaultDir 返回当前用户的新版 Outlook IndexedDB 目录。
func DefaultDir() (string, error) {
	if runtime.GOOS != "windows" {
		return "", fmt.Errorf("%w: 新版 Outlook for Windows 只在 Windows 上", ErrNotFound)
	}
	la := os.Getenv("LOCALAPPDATA")
	if la == "" {
		return "", fmt.Errorf("%w: 没有 LOCALAPPDATA", ErrNotFound)
	}
	return filepath.Join(la, "Microsoft", "Olk", "EBWebView", "Default", "IndexedDB"), nil
}

// Open 读取 IndexedDB 目录（留空用 DefaultDir）下全部 origin 的新版 Outlook 离线数据，并做格式自检。
func Open(root string, opt Options) (*Store, error) {
	if root == "" {
		d, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		root = d
	}
	s := &Store{root: root, maxBody: opt.MaxBodyBytes, byID: map[int64]*Folder{}, byMsg: map[int64]msgRef{}}
	if s.maxBody <= 0 {
		s.maxBody = 256 << 10
	}
	sig, dirs, err := signature(root)
	if err != nil {
		return nil, wrap(err)
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("%w: %s 下没有 IndexedDB 数据（新版 Outlook 没登录账户或没开离线）", ErrNotFound, root)
	}
	s.sig = sig
	for _, dir := range dirs {
		d, err := openIDB(dir, strings.TrimSuffix(dir, ".leveldb")+".blob")
		if err != nil {
			return nil, wrap(err)
		}
		s.dbs = append(s.dbs, d)
	}
	if err := s.build(); err != nil {
		return nil, err
	}
	return s, nil
}

func wrap(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnsupportedFormat), errors.Is(err, ErrNotFound), errors.Is(err, ErrLocked):
		return err
	case isLockErr(err):
		return fmt.Errorf("%w: %v", ErrLocked, err)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, errCorrupt), errors.Is(err, errSnappy), errors.Is(err, errV8):
		return fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
	}
	return err
}

// signature 列出 LevelDB 目录，并用其中文件的名字、大小、修改时间做快照签名。
// 目录有两种布局：默认存储桶 root/<origin>.indexeddb.leveldb，和 Chromium 新的存储桶布局
// ../WebStorage/<桶号>/IndexedDB/indexeddb.leveldb（olk 1.2026.1103 只把系统库放在这里，以后可能迁移）。
func signature(root string) (string, []string, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	var dirs []string
	for _, e := range ents {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".indexeddb.leveldb") {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	buckets, _ := filepath.Glob(filepath.Join(filepath.Dir(root), "WebStorage", "*", "IndexedDB", "indexeddb.leveldb"))
	sort.Strings(buckets)
	dirs = append(dirs, buckets...)
	for _, dir := range dirs {
		fs, err := os.ReadDir(dir)
		if err != nil {
			return "", nil, err
		}
		for _, f := range fs {
			if f.Name() == "LOCK" || strings.HasPrefix(f.Name(), "LOG") {
				continue
			}
			if info, err := f.Info(); err == nil {
				fmt.Fprintf(&b, "%s/%s:%d:%d;", dir, f.Name(), info.Size(), info.ModTime().UnixNano())
			}
		}
	}
	return b.String(), dirs, nil
}

// Signature 返回 root（留空用 DefaultDir）下 LevelDB 文件的名字、大小、修改时间签名，
// 和 Stale 比较的是同一个值。只列目录、不读文件。
func Signature(root string) (string, error) {
	if root == "" {
		d, err := DefaultDir()
		if err != nil {
			return "", err
		}
		root = d
	}
	sig, _, err := signature(root)
	return sig, err
}

// Stale 报告快照之后数据有没有变（olk 同步时会写 .log，新邮件到了也会变）。
func (s *Store) Stale() bool {
	sig, _, err := signature(s.root)
	return err != nil || sig != s.sig
}

// Close 释放快照。文件在 Open 里已经全部关闭，这里不持有句柄。
func (s *Store) Close() error { return nil }

// Dir 返回 IndexedDB 目录。
func (s *Store) Dir() string { return s.root }

// Skipped 返回解不开、被跳过的记录数（诊断用）。
func (s *Store) Skipped() int { return s.skipped }

// hashID 把 Exchange ID 变成稳定的正整数。只取 53 位：ID 经 JSON 交给模型和 MCP 客户端，
// JavaScript 的数字超过 2^53 会丢精度（真机冒烟里 64 位哈希被客户端改写成了另一个数）。
func hashID(s string) int64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	v := int64(h.Sum64() & (1<<53 - 1))
	if v == 0 {
		v = 1
	}
	return v
}

const accountDBPrefix = "owa-offline-data-"

// build 读文件夹和邮件列表，同时做格式自检。
func (s *Store) build() error {
	type acct struct {
		d                 *idbStore
		db                int64
		folders, messages int64
		bodies            int64
	}
	var accts []acct
	for _, d := range s.dbs {
		for id, name := range d.schema.dbs {
			if !strings.HasPrefix(name, accountDBPrefix) {
				continue
			}
			a := acct{d: d, db: id}
			for sid, sn := range d.schema.stores[id] {
				switch sn {
				case "folders":
					a.folders = sid
				case "messages":
					a.messages = sid
				case "messageBodies":
					a.bodies = sid
				}
			}
			if a.folders == 0 || a.messages == 0 {
				return fmt.Errorf("%w: 库 %s 里没有 folders/messages 对象仓库", ErrUnsupportedFormat, name)
			}
			accts = append(accts, a)
		}
	}
	if len(accts) == 0 {
		return fmt.Errorf("%w: 没有新版 Outlook 账户的离线库（%s*）", ErrNotFound, accountDBPrefix)
	}
	sort.Slice(accts, func(i, j int) bool { return accts[i].db < accts[j].db })

	// 文件夹
	type rawFolder struct {
		f      Folder
		parent string
	}
	byExID := map[string]*rawFolder{}
	var order []string
	accounts := map[string]bool{}
	for _, a := range accts {
		recs := a.d.records(a.db, a.folders)
		bad := 0
		for _, r := range recs {
			v, err := a.d.decode(a.db, a.folders, r)
			o, _ := v.(*jsObject)
			id, _ := o.get("id").(string)
			if err != nil || o == nil || id == "" {
				bad++
				continue
			}
			rf := &rawFolder{parent: str(o, "parentFolderId")}
			rf.f = Folder{
				ID: hashID(id), FolderID: id, Name: str(o, "displayName"), Special: str(o, "distinguishedFolderType"),
				Class: str(o, "FolderClass"), ServerTotal: int(num(o, "totalMessageCount")),
				Account: str(obj(o, "mailboxInfo"), "mailboxSmtpAddress"),
			}
			rf.f.Mail = rf.f.Class == "" || strings.HasPrefix(rf.f.Class, "IPF.Note")
			accounts[rf.f.Account] = true
			byExID[id] = rf
			order = append(order, id)
		}
		if err := selfCheck("folders", len(recs), bad); err != nil {
			return err
		}
		s.skipped += bad
	}
	multi := len(accounts) > 1
	var path func(id string, depth int) string
	path = func(id string, depth int) string {
		rf := byExID[id]
		if p, ok := byExID[rf.parent]; ok && depth < 32 && rf.parent != id && p.f.Special != "msgfolderroot" {
			return path(rf.parent, depth+1) + "/" + rf.f.Name
		}
		if multi && rf.f.Account != "" {
			return rf.f.Account + "/" + rf.f.Name
		}
		return rf.f.Name
	}
	for _, id := range order {
		rf := byExID[id]
		rf.f.Path = path(id, 0)
		if p, ok := byExID[rf.parent]; ok && p.f.Special != "msgfolderroot" {
			rf.f.ParentID = p.f.ID
		}
		s.folders = append(s.folders, rf.f)
	}
	sort.SliceStable(s.folders, func(i, j int) bool { return s.folders[i].Path < s.folders[j].Path })
	for i := range s.folders {
		s.byID[s.folders[i].ID] = &s.folders[i]
	}

	// 邮件：列表行来自 messages；只在 messageBodies 里有的也收进来。
	for _, a := range accts {
		bodyRecs := map[string]idbRecord{}
		if a.bodies != 0 {
			for _, r := range a.d.records(a.db, a.bodies) {
				bodyRecs[r.key] = r
			}
		}
		seen := map[string]bool{}
		recs := a.d.records(a.db, a.messages)
		bad := 0
		add := func(o *jsObject, fromBody bool) {
			m := summaryOf(o)
			if fromBody {
				m.Incomplete = append(m.Incomplete, "preview")
			}
			if seen[m.ItemID] {
				return
			}
			seen[m.ItemID] = true
			ref := msgRef{db: a.d, dbID: a.db, bodies: a.bodies, summary: len(s.index)}
			if br, ok := bodyRecs[m.ItemID]; ok {
				ref.rec, ref.hasBody = br, true
			}
			s.byMsg[m.ID] = ref
			s.index = append(s.index, m)
		}
		for _, r := range recs {
			v, err := a.d.decode(a.db, a.messages, r)
			o, _ := v.(*jsObject)
			if err != nil || o == nil || itemID(o) == "" {
				bad++
				continue
			}
			add(o, false)
		}
		if err := selfCheck("messages", len(recs), bad); err != nil {
			return err
		}
		s.skipped += bad
		for k, r := range bodyRecs {
			if seen[k] {
				continue
			}
			v, err := a.d.decode(a.db, a.bodies, r)
			o, _ := v.(*jsObject)
			if err != nil || o == nil || itemID(o) == "" {
				s.skipped++
				continue
			}
			add(o, true)
		}
	}
	sort.SliceStable(s.index, func(i, j int) bool { return s.index[i].Received.After(s.index[j].Received) })
	for i, m := range s.index {
		ref := s.byMsg[m.ID]
		ref.summary = i
		s.byMsg[m.ID] = ref
		if f, ok := s.byID[m.FolderID]; ok {
			f.MessageCount++
			if !m.Read {
				f.UnreadCount++
			}
		}
	}
	return nil
}

// selfCheck：一个仓库里超过一半的记录解不开，就当格式变了。
func selfCheck(store string, total, bad int) error {
	if total > 0 && bad*2 > total {
		return fmt.Errorf("%w: %s 仓库 %d 条记录里 %d 条解不开", ErrUnsupportedFormat, store, total, bad)
	}
	return nil
}

// Folders 返回全部文件夹，按路径排序。
func (s *Store) Folders(ctx context.Context) ([]Folder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]Folder(nil), s.folders...), nil
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return 50
	case n > 200:
		return 200
	}
	return n
}

// List 按条件分页列邮件，按接收时间倒序。没有接收时间的排在最后。
func (s *Store) List(ctx context.Context, q Query) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if q.FolderID != 0 {
		if _, ok := s.byID[q.FolderID]; !ok {
			return Page{}, fmt.Errorf("%w: 文件夹 %d", ErrNotFound, q.FolderID)
		}
	}
	from, subj := strings.ToLower(q.From), strings.ToLower(q.Subject)
	var hits []Summary
	for _, m := range s.index {
		if q.FolderID != 0 {
			if m.FolderID != q.FolderID {
				continue
			}
		} else if f, ok := s.byID[m.FolderID]; ok && !f.Mail {
			continue
		}
		if !q.Since.IsZero() && m.Received.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && !m.Received.Before(q.Until) {
			continue
		}
		if subj != "" && !strings.Contains(strings.ToLower(m.Subject), subj) {
			continue
		}
		if from != "" && !strings.Contains(strings.ToLower(m.From+" "+m.FromAddress), from) {
			continue
		}
		hits = append(hits, m)
	}
	limit := clampLimit(q.Limit)
	off := max(q.Offset, 0)
	page := Page{Total: len(hits), NextOffset: -1, Items: []Summary{}}
	if off >= len(hits) {
		return page, nil
	}
	end := off + limit
	if end < len(hits) {
		page.NextOffset = end
	} else {
		end = len(hits)
	}
	page.Items = append(page.Items, hits[off:end]...)
	return page, nil
}

// --- 从 JS 对象取字段的小工具 ---

func obj(o *jsObject, k string) *jsObject {
	v, _ := o.get(k).(*jsObject)
	return v
}

func str(o *jsObject, k string) string {
	switch v := o.get(k).(type) {
	case string:
		return v
	case *jsObject: // 形如 {Id: "..."} 的 ID 对象
		s, _ := v.get("Id").(string)
		return s
	}
	return ""
}

func num(o *jsObject, k string) float64 {
	v, _ := o.get(k).(float64)
	return v
}

func boolean(o *jsObject, k string) (bool, bool) {
	v, ok := o.get(k).(bool)
	return v, ok
}

func itemID(o *jsObject) string {
	if id := str(o, "ItemId"); id != "" {
		return id
	}
	return str(o, "id")
}

// mailbox 取 {Mailbox: {Name, EmailAddress}}。
func mailbox(o *jsObject) (name, addr string) {
	mb := obj(o, "Mailbox")
	return str(mb, "Name"), str(mb, "EmailAddress")
}

func parseTime(o *jsObject, k string) (time.Time, bool) {
	switch v := o.get(k).(type) {
	case string:
		t, err := time.Parse(time.RFC3339, v)
		return t.UTC(), err == nil
	case time.Time:
		return v, !v.IsZero()
	}
	return time.Time{}, false
}

// summaryOf 从 ItemRow（messages）或 Message（messageBodies）对象取列表字段。两者字段名相同。
func summaryOf(o *jsObject) Summary {
	id := itemID(o)
	m := Summary{ID: hashID(id), ItemID: id, MessageClass: str(o, "ItemClass")}
	var miss []string
	if v, ok := o.get("Subject").(string); ok {
		m.Subject = v
	} else {
		miss = append(miss, "subject")
	}
	if f := obj(o, "From"); f != nil {
		m.From, m.FromAddress = mailbox(f)
	} else if f := obj(o, "Sender"); f != nil {
		m.From, m.FromAddress = mailbox(f)
	}
	if m.From == "" && m.FromAddress == "" {
		miss = append(miss, "from")
	}
	if pf := str(o, "ParentFolderId"); pf != "" {
		m.FolderID = hashID(pf)
	} else {
		miss = append(miss, "folder")
	}
	var ok bool
	if m.Received, ok = parseTime(o, "DateTimeReceived"); !ok {
		miss = append(miss, "received")
	}
	if m.Sent, ok = parseTime(o, "DateTimeSent"); !ok {
		miss = append(miss, "sent")
	}
	if m.HasAttachment, ok = boolean(o, "HasAttachments"); !ok {
		miss = append(miss, "has_attachment")
	}
	if m.Read, ok = boolean(o, "IsRead"); !ok {
		miss = append(miss, "read")
	}
	if to, ok := o.get("DisplayTo").(string); ok {
		m.To = splitDisplay(to)
	}
	if cc, ok := o.get("DisplayCc").(string); ok {
		m.Cc = splitDisplay(cc)
	}
	m.Preview = preview(str(o, "Preview"))
	m.Incomplete = miss
	return m
}

func splitDisplay(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}
