// Package outlookwin 只读读取 Outlook for Windows（经典版）的 .pst / .ost 文件，
// 纯 Go 按 [MS-PST] 实现，不用 cgo，也不依赖 Outlook / MAPI。
//
// 覆盖 ANSI PST、Unicode PST，以及 Outlook 2013 起 OST 用的 4K 页布局（规范之外，按公开样本实测）。
// API 和 outlooklegacy 同形：Folders 列文件夹、List 分页列邮件、Get 取一封。
//
// 本包绝不写也绝不新建任何文件：.pst / .ost 只用 os.Open 打开、ReadAt 读取，
// 也不整份复制到别处。Outlook 运行时通常独占打开数据文件，这时 Open 返回 ErrLocked，
// 不尝试卷影复制之类的绕过手段。
package outlookwin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotFound 表示文件夹或邮件不存在。
	ErrNotFound = errors.New("未找到")
	// ErrUnsupported 表示文件格式不认识或无法读取（例如被 Windows 信息保护加密）。
	ErrUnsupported = errors.New("PST/OST 格式不支持")
	// ErrLocked 表示文件正被其他程序（通常是 Outlook）独占打开。
	ErrLocked = errors.New("文件正被 Outlook 占用")

	errNoNode = errors.New("节点不存在")
)

// 节点 ID 类型（NID 低 5 位）和固定 NID（[MS-PST] 2.2.2.1、2.4.1）。
const (
	nidTypeNormalFolder  = 0x02
	nidTypeNormalMessage = 0x04
	nidTypeAttachment    = 0x05
	nidTypeHierarchy     = 0x0D
	nidTypeContents      = 0x0E

	nidMessageStore   = 0x21
	nidRootFolder     = 0x122
	nidRecipientTable = 0x692
	nidAttachTable    = 0x671
)

// Options 控制打开方式。零值即推荐配置。
type Options struct {
	// MaxBodyBytes 单封正文（纯文本）上限，<=0 用 256 KiB。
	MaxBodyBytes int
	// Codepage 是 ANSI 文件里没标代码页的 8 位字符串用的代码页。0 = 自动：
	// Windows 上取系统 ANSI 代码页（GetACP，中文系统是 936），其他平台按 UTF-8/Windows-1252 猜。
	Codepage int
}

// Folder 是一个文件夹。
type Folder struct {
	ID           int64  `json:"id"`        // 文件夹的 NID
	ParentID     int64  `json:"parent_id"` // 0 表示顶层
	Name         string `json:"name"`
	Path         string `json:"path"` // 从 IPM 子树根开始，"/" 分隔
	Special      string `json:"special,omitempty"`
	Class        string `json:"class,omitempty"` // PidTagContainerClass，例如 IPF.Note
	Mail         bool   `json:"mail"`            // 是否邮件文件夹（日历、联系人等为 false）
	MessageCount int    `json:"message_count"`
	UnreadCount  int    `json:"unread_count"`
}

// Query 是列表条件，零值字段不过滤。
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
	ID            int64     `json:"id"` // 邮件的 NID
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
}

// Page 是一页结果。
type Page struct {
	Items      []Summary `json:"items"`
	Total      int       `json:"total"`
	NextOffset int       `json:"next_offset"` // -1 表示没有下一页
}

// Store 是一个只读打开的 .pst / .ost。并发安全。
type Store struct {
	path      string
	p         *pstFile
	maxBody   int
	defaultCP int
	modTime   time.Time
	size      int64

	loadMu  sync.Mutex
	loaded  bool
	folders []Folder
	byID    map[int64]*Folder

	mu    sync.Mutex
	index map[int64][]Summary // 文件夹 → 邮件（按接收时间倒序），按需建立
}

// Open 以只读方式打开一个 .pst / .ost。
func Open(path string, opt Options) (*Store, error) {
	f, err := openShared(path)
	if err != nil {
		if isSharingViolation(err) {
			return nil, errors.Join(ErrLocked, fmt.Errorf("%s 正被其他程序独占打开（多半是经典版 Outlook 在运行）: %w", path, err))
		}
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	p, err := parseHeader(f, fi.Size())
	if err != nil {
		f.Close()
		if !errors.Is(err, errFormat) {
			return nil, wrap(err)
		}
		return nil, errors.Join(ErrUnsupported, err)
	}
	p.f = f
	maxBody := opt.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 256 << 10
	}
	cp := opt.Codepage
	if cp == 0 {
		cp = systemCodepage()
	}
	return &Store{path: path, p: p, maxBody: maxBody, defaultCP: cp, modTime: fi.ModTime(), size: fi.Size(), index: map[int64][]Summary{}}, nil
}

// Close 关闭文件。
func (s *Store) Close() error { return s.p.f.Close() }

// Path 返回打开的文件路径。
func (s *Store) Path() string { return s.path }

// Format 返回文件布局（ansi / unicode / unicode-4k）和是否 OST，给诊断用。
func (s *Store) Format() (layout string, ost bool, crypt int) {
	return s.p.layout.String(), s.p.isOST, int(s.p.crypt)
}

// Stale 报告文件在打开之后是否被改过（Outlook 写入了新内容），调用方应重开。
func (s *Store) Stale() bool {
	fi, err := os.Stat(s.path)
	return err == nil && (fi.Size() != s.size || !fi.ModTime().Equal(s.modTime))
}

// wrap 把内部的格式错误统一包成 ErrUnsupported；Outlook 的文件锁（打开后读时才碰到的
// 字节范围锁，ERROR_LOCK_VIOLATION）包成 ErrLocked，免得调用方把它当成格式问题停用来源。
func wrap(err error) error {
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrLocked) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if isSharingViolation(err) {
		return errors.Join(ErrLocked, fmt.Errorf("文件正被其他程序锁住（多半是经典版 Outlook 在运行）: %w", err))
	}
	if errors.Is(err, errFormat) || errors.Is(err, errNoNode) {
		return errors.Join(ErrUnsupported, err)
	}
	return err
}

// Folders 列出 IPM 子树下的全部文件夹（不含搜索文件夹等内部节点）。
func (s *Store) Folders(ctx context.Context) ([]Folder, error) {
	if err := s.load(); err != nil {
		return nil, wrap(err)
	}
	return append([]Folder(nil), s.folders...), nil
}

func (s *Store) load() error {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	if s.loaded {
		return nil
	}
	// 失败不缓存：Outlook 退出、锁释放之后下一次调用就能成功。
	s.folders, s.byID = nil, nil
	if err := s.loadFolders(); err != nil {
		return err
	}
	s.loaded = true
	return nil
}

func (s *Store) loadFolders() error {
	store, _, err := s.p.openNode(nidMessageStore)
	if err != nil {
		return fmt.Errorf("读消息存储节点: %w", err)
	}
	sp, err := readPC(store)
	if err != nil {
		return fmt.Errorf("读消息存储属性: %w", err)
	}
	special := map[uint32]string{}
	for id, name := range map[uint16]string{
		pidIpmSentMailEntryID: "sent", pidIpmWastebasketEntryID: "trash", pidIpmOutboxEntryID: "outbox",
	} {
		if nid := sp.entryNID(id); nid != 0 {
			special[nid] = name
		}
	}
	top := sp.entryNID(pidIpmSubtreeEntryID)
	if top != 0 {
		if _, ok, _ := s.p.node(top); !ok {
			top = 0
		}
	}
	if top == 0 {
		// OST 的消息存储里没有 PST 式的 EntryID（Exchange 用长期 ID），
		// 从根文件夹往下找名为 IPM_SUBTREE 的文件夹，优先「Root - Mailbox」下的那个（另一个属于公用文件夹）。
		all, err := s.walkFolders(nidRootFolder)
		if err != nil {
			return err
		}
		for _, f := range all {
			if f.Name == "IPM_SUBTREE" && (top == 0 || !strings.HasPrefix(f.Path, "Root - Public")) {
				top = uint32(f.ID)
			}
		}
		if top == 0 {
			s.folders = all
			top = nidRootFolder
		}
	}
	if s.folders == nil {
		f, err := s.walkFolders(top)
		if err != nil {
			return err
		}
		s.folders = f
	}
	// 草稿的 EntryID 存在根文件夹 / IPM 子树根上（各版本放的位置不完全一样，都看一眼）。
	for _, nid := range []uint32{nidRootFolder, top} {
		if n, _, err := s.p.openNode(nid); err == nil {
			if pc, err := readPC(n); err == nil {
				if d := pc.entryNID(pidIpmDraftsEntryID); d != 0 {
					special[d] = "drafts"
				}
			}
		}
	}
	s.byID = map[int64]*Folder{}
	for i := range s.folders {
		f := &s.folders[i]
		f.Special = special[uint32(f.ID)]
		if f.Special == "" && f.ParentID == 0 {
			// shortcut: EntryID 认不出时（OST）按顶层英文名认，本地化的名字认不出；
			// 以后读 receive folder table 和 Exchange 长期 ID。
			f.Special = wellKnownNames[strings.ToLower(f.Name)]
		}
		s.byID[f.ID] = f
	}
	return nil
}

var wellKnownNames = map[string]string{
	"inbox": "inbox", "sent items": "sent", "deleted items": "trash",
	"drafts": "drafts", "outbox": "outbox", "junk e-mail": "junk", "junk email": "junk",
}

// walkFolders 从 top 的层级表开始递归列出子文件夹，路径相对 top。
func (s *Store) walkFolders(top uint32) ([]Folder, error) {
	var out []Folder
	seen := map[uint32]bool{}
	var walk func(nid uint32, parent int64, path string, depth int) error
	walk = func(nid uint32, parent int64, path string, depth int) error {
		if depth > 64 || seen[nid] {
			return nil
		}
		seen[nid] = true
		ht, _, err := s.p.openNode(nid&^0x1F | nidTypeHierarchy)
		if errors.Is(err, errNoNode) {
			return nil
		}
		if err != nil {
			return err
		}
		t, err := readTC(ht)
		if err != nil {
			return err
		}
		for _, row := range t.bags() {
			v, ok := row.int(pidLtpRowID)
			if !ok {
				continue
			}
			child := uint32(v)
			fn, _, err := s.p.openNode(child)
			if err != nil {
				continue // 层级表里有、节点却没了：跳过，不让整棵树失败
			}
			pc, err := readPC(fn)
			if err != nil {
				continue
			}
			name := pc.str(pidDisplayName, s.cp(pc))
			f := Folder{
				ID: int64(child), ParentID: parent, Name: name,
				Path:  strings.TrimPrefix(path+"/"+name, "/"),
				Class: pc.str(pidContainerClass, s.cp(pc)),
			}
			f.Mail = f.Class == "" || strings.HasPrefix(f.Class, "IPF.Note") || strings.HasPrefix(f.Class, "IPF.Imap")
			if c, ok := pc.int(pidContentCount); ok {
				f.MessageCount = int(c)
			}
			if c, ok := pc.int(pidContentUnreadCount); ok {
				f.UnreadCount = int(c)
			}
			out = append(out, f)
			if err := walk(child, f.ID, f.Path, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(top, 0, "", 0); err != nil {
		return nil, err
	}
	return out, nil
}

// folderIndex 返回一个文件夹的邮件（按接收时间倒序），第一次调用时从内容表建立。
func (s *Store) folderIndex(f *Folder) ([]Summary, error) {
	s.mu.Lock()
	if idx, ok := s.index[f.ID]; ok {
		s.mu.Unlock()
		return idx, nil
	}
	s.mu.Unlock()
	idx, err := s.buildIndex(f)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.index[f.ID] = idx
	s.mu.Unlock()
	return idx, nil
}

func (s *Store) buildIndex(f *Folder) ([]Summary, error) {
	ct, _, err := s.p.openNode(uint32(f.ID)&^0x1F | nidTypeContents)
	if errors.Is(err, errNoNode) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t, err := readTC(ct)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(t.rows))
	for _, row := range t.bags() {
		v, ok := row.int(pidLtpRowID)
		if !ok || uint32(v)&0x1F != nidTypeNormalMessage {
			continue
		}
		m := summaryOf(row, s.cp(row))
		m.ID, m.FolderID = int64(uint32(v)), f.ID
		out = append(out, m)
	}
	sortSummaries(out)
	return out, nil
}

func sortSummaries(v []Summary) {
	sort.SliceStable(v, func(i, j int) bool {
		if !v[i].Received.Equal(v[j].Received) {
			return v[i].Received.After(v[j].Received)
		}
		return v[i].ID > v[j].ID
	})
}

// summaryOf 从一组属性（内容表的一行或邮件的 PC）取列表字段。
func summaryOf(b propBag, cp int) Summary {
	m := Summary{
		Subject:      normSubject(b.str(pidSubject, cp)),
		From:         b.str(pidSentRepresentingName, cp),
		Received:     b.time(pidDeliveryTime),
		Sent:         b.time(pidClientSubmitTime),
		To:           splitDisplay(b.str(pidDisplayTo, cp)),
		Cc:           splitDisplay(b.str(pidDisplayCc, cp)),
		MessageClass: b.str(pidMessageClass, cp),
	}
	if m.From == "" {
		m.From = b.str(pidSenderName, cp)
	}
	if m.Received.IsZero() {
		m.Received = m.Sent
	}
	if fl, ok := b.int(pidMessageFlags); ok {
		m.Read = fl&msgFlagRead != 0
		m.HasAttachment = fl&msgFlagHasAttach != 0
	}
	if h, ok := b.bool(pidHasAttachments); ok {
		m.HasAttachment = h
	}
	m.FromAddress = senderAddress(b, cp)
	return m
}

// senderAddress 取发件人地址：优先 SMTP 地址，Exchange 内部的 X500 地址（/O=...）不当作邮箱地址。
func senderAddress(b propBag, cp int) string {
	for _, id := range []uint16{pidSenderSmtpAddress, pidSentReprSmtpAddress} {
		if v := b.str(id, cp); v != "" {
			return v
		}
	}
	for _, pair := range [][2]uint16{{pidSenderAddrType, pidSenderEmail}, {pidSentRepresentingType, pidSentRepresentingEmail}} {
		if strings.EqualFold(b.str(pair[0], cp), "SMTP") {
			if v := b.str(pair[1], cp); v != "" {
				return v
			}
		}
	}
	return ""
}

func splitDisplay(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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

// List 按条件分页列邮件，按接收时间倒序。
//
// 列表字段来自各文件夹的内容表，不逐封打开邮件；发件人地址和摘要只给返回的这一页补齐
// （内容表里没有这两列）。按发件人过滤时只匹配内容表里的显示名，这一页之外的地址不查。
func (s *Store) List(ctx context.Context, q Query) (Page, error) {
	if err := s.load(); err != nil {
		return Page{}, wrap(err)
	}
	var folders []*Folder
	if q.FolderID != 0 {
		f, ok := s.byID[q.FolderID]
		if !ok {
			return Page{}, fmt.Errorf("%w: 文件夹 %d", ErrNotFound, q.FolderID)
		}
		folders = []*Folder{f}
	} else {
		for i := range s.folders {
			if s.folders[i].Mail {
				folders = append(folders, &s.folders[i])
			}
		}
	}
	from, subj := strings.ToLower(q.From), strings.ToLower(q.Subject)
	var hits []Summary
	for _, f := range folders {
		if err := ctx.Err(); err != nil {
			return Page{}, err
		}
		idx, err := s.folderIndex(f)
		if err != nil {
			return Page{}, wrap(fmt.Errorf("读文件夹 %q 的内容表: %w", f.Path, err))
		}
		for _, m := range idx {
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
	}
	if len(folders) > 1 {
		sortSummaries(hits)
	}
	limit := clampLimit(q.Limit)
	off := q.Offset
	if off < 0 {
		off = 0
	}
	page := Page{Total: len(hits), NextOffset: -1}
	if off >= len(hits) {
		page.Items = []Summary{}
		return page, nil
	}
	end := off + limit
	if end < len(hits) {
		page.NextOffset = end
	} else {
		end = len(hits)
	}
	page.Items = append([]Summary(nil), hits[off:end]...)
	for i := range page.Items {
		s.enrich(&page.Items[i])
	}
	return page, nil
}

// enrich 打开邮件的 PC 补上内容表里没有的字段。失败时保持原样。
func (s *Store) enrich(m *Summary) {
	n, _, err := s.p.openNode(uint32(m.ID))
	if err != nil {
		return
	}
	pc, err := readPC(n)
	if err != nil {
		return
	}
	cp := s.cp(pc)
	if s.p.layout == layoutANSI && pc.codepage() != 0 {
		// ANSI 文件的内容表行里没有代码页，8 位字符串可能按错的代码页解了；用邮件自己的重算。
		full := summaryOf(pc, cp)
		m.Subject, m.From, m.To, m.Cc = full.Subject, full.From, full.To, full.Cc
	}
	if m.FromAddress == "" {
		m.FromAddress = senderAddress(pc, cp)
	}
	if m.From == "" {
		m.From = summaryOf(pc, cp).From
	}
	if m.Preview == "" {
		body, _ := s.body(pc, cp)
		m.Preview = preview(firstBytes(body, 2048))
	}
}

// cp 返回解 8 位字符串用的代码页：对象自己的，没有就用打开时定的默认值。
func (s *Store) cp(b propBag) int {
	if c := b.codepage(); c != 0 {
		return c
	}
	return s.defaultCP
}

func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}
