// Package hxstore 只读读取新版 Outlook for Mac 的本地存储 HxStore.hxd。
//
// 本包绝不写、不锁、不移动 Outlook 的任何文件，也不碰 HxStore.lock：
//   - 建索引前先用 clonefile(2)（APFS 克隆，即 `cp -c` 的底层调用，但不会退化成整文件复制）
//     在临时目录做一份时间点快照，扫描完立即删除；克隆失败时直接 os.Open 只读读原文件。
//   - 索引只保留每封邮件的头信息和块偏移；正文在 Get 时按偏移从原文件只读重新解压，
//     并校验块魔数、CRC 和对象 id，块已被 Outlook 改写时重建索引再试一次。
//   - 启动做字段自检，不达标返回 ErrUnsupportedFormat，不返回错乱数据。
//
// 格式没有官方文档，字段槽位是对 Outlook 16.115 的实测值，见 block.go / record.go。
package hxstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileName 是新版 Outlook 存储文件名。
const FileName = "HxStore.hxd"

// DefaultDataDir 返回当前用户默认配置文件（Main Profile）目录，HxStore.hxd 在它下面。
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Group Containers", "UBF8T346G9.Office",
		"Outlook", "Outlook 15 Profiles", "Main Profile"), nil
}

// ErrUnsupportedFormat 表示文件不是本包认识的 HxStore 格式（版本不符或字段自检不达标）。
var ErrUnsupportedFormat = errors.New("新版 Outlook 存储格式不支持")

// ErrNotFound 表示 id 不存在。
var ErrNotFound = errors.New("邮件不存在")

// Options 控制打开方式。零值即推荐配置。
type Options struct {
	// MaxBodyBytes 单封正文（纯文本）上限，<=0 用 256 KiB。
	MaxBodyBytes int
	// SnapshotDir 放 APFS 克隆快照的目录，空 = os.TempDir()。必须和 HxStore.hxd 在同一 APFS 卷上才能克隆。
	SnapshotDir string
	// NoSnapshot 直接只读读原文件，不做克隆。
	NoSnapshot bool
	// RefreshInterval 文件变化后最短多久重建一次索引，<=0 用 60 秒。
	RefreshInterval time.Duration
}

// 字段自检阈值：抽到的邮件里这些字段的成功率低于阈值就判定格式不支持。
const (
	checkMinSubject  = 0.90
	checkMinFromAddr = 0.90
	checkMinSentRecv = 0.90 // 发送时间 <= 接收时间 + 1 天
	maxBadCRCRatio   = 0.05 // 超过就认为读到了 Outlook 正在写的状态，重读一次
)

// UnsupportedFields 是本包始终无法提供的字段（返回零值）。
var UnsupportedFields = []string{"read", "has_attachment", "attachments"}

// Store 是一个只读的新版 Outlook 数据源。并发安全。
type Store struct {
	path    string
	opt     Options
	maxBody int

	mu  sync.RWMutex
	idx *index
	// 最近一次重建的时间，用来限流
	builtAt time.Time
}

// Stats 是最近一次建索引的统计（只有数字，不含内容）。
type Stats struct {
	Version       string        `json:"version"`
	Snapshot      bool          `json:"snapshot"` // 本次是否基于 APFS 克隆
	Candidates    int           `json:"candidates"`
	BlocksOK      int           `json:"blocks_ok"`
	BadCRC        int           `json:"bad_crc"`
	BadHeader     int           `json:"bad_header"`
	BadDecode     int           `json:"bad_decode"`
	RawBytes      int64         `json:"raw_bytes"`
	Versions      int           `json:"versions"`        // 抽到的邮件版本数（去重前）
	DupByObject   int           `json:"dup_by_object"`   // 同对象 id 的旧版本
	DupByMsgID    int           `json:"dup_by_msgid"`    // 同 Message-ID 的旧版本
	RecipFromScan int           `json:"recip_from_scan"` // 收件人来自其他块
	Messages      int           `json:"messages"`
	Folders       int           `json:"folders"`
	FolderNamed   int           `json:"folder_named"`
	Elapsed       time.Duration `json:"elapsed"`
	SubjectRate   float64       `json:"subject_rate"`
	FromAddrRate  float64       `json:"from_addr_rate"`
	SentRecvRate  float64       `json:"sent_recv_rate"`
}

// Open 打开 dir/HxStore.hxd（dir 也可以直接是 .hxd 文件路径）并建索引。
func Open(dir string, opt Options) (*Store, error) {
	p, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(p); err != nil {
		return nil, fmt.Errorf("找不到新版 Outlook 存储: %w", err)
	} else if fi.IsDir() {
		p = filepath.Join(p, FileName)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("找不到新版 Outlook 存储: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件", p)
	}
	if opt.RefreshInterval <= 0 {
		opt.RefreshInterval = time.Minute
	}
	s := &Store{path: p, opt: opt, maxBody: opt.MaxBodyBytes}
	if s.maxBody <= 0 {
		s.maxBody = 256 << 10
	}
	if err := s.rebuild(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path 返回 HxStore.hxd 的绝对路径。
func (s *Store) Path() string { return s.path }

// Stats 返回最近一次建索引的统计。
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idx.stats
}

// Close 释放索引。没有常驻的文件句柄。
func (s *Store) Close() error {
	s.mu.Lock()
	s.idx = nil
	s.mu.Unlock()
	return nil
}

// Refresh 文件有变化就立即重建索引（不受 RefreshInterval 限制）。
func (s *Store) Refresh() error {
	fi, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	s.mu.RLock()
	same := s.idx != nil && fi.Size() == s.idx.size && fi.ModTime().Equal(s.idx.mtime)
	s.mu.RUnlock()
	if same {
		return nil
	}
	return s.rebuild()
}

// current 返回当前索引；文件变化且距上次重建超过 RefreshInterval 时先重建。
// 重建失败时继续用旧索引（旧数据好过没有数据），新错误只在没有旧索引时返回。
func (s *Store) current() (*index, error) {
	s.mu.RLock()
	idx, built := s.idx, s.builtAt
	s.mu.RUnlock()
	if idx == nil {
		return nil, errors.New("Store 已关闭")
	}
	if time.Since(built) < s.opt.RefreshInterval {
		return idx, nil
	}
	if fi, err := os.Stat(s.path); err == nil && (fi.Size() != idx.size || !fi.ModTime().Equal(idx.mtime)) {
		if err := s.rebuild(); err == nil {
			s.mu.RLock()
			idx = s.idx
			s.mu.RUnlock()
		}
	}
	return idx, nil
}

func (s *Store) rebuild() error {
	idx, err := s.build()
	// 读到 Outlook 写到一半的状态：CRC 失败率异常，或自检失败，稍等重读一次。
	if err == nil && idx.stats.Candidates > 0 && float64(idx.stats.BadCRC) > maxBadCRCRatio*float64(idx.stats.Candidates) ||
		errors.Is(err, ErrUnsupportedFormat) && !errors.Is(err, errVersion) {
		time.Sleep(retryDelay)
		idx, err = s.build()
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.idx, s.builtAt = idx, time.Now()
	s.mu.Unlock()
	return nil
}

var errVersion = errors.New("文件头或版本字节不符")

// retryDelay 是疑似读到半写状态时重读前的等待。
var retryDelay = 500 * time.Millisecond

// build 做一次快照 + 扫描 + 自检。
func (s *Store) build() (*index, error) {
	t0 := time.Now()
	fi, err := os.Stat(s.path)
	if err != nil {
		return nil, err
	}
	src := s.path
	snapped := false
	if !s.opt.NoSnapshot {
		if snap, cleanup, err := cloneSnapshot(s.path, s.opt.SnapshotDir); err == nil {
			defer cleanup()
			src, snapped = snap, true
		}
	}
	f, err := os.Open(src) // 只读
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sfi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	ver, err := checkHeader(f)
	if err != nil {
		return nil, err
	}
	idx, err := buildIndex(f, sfi.Size())
	if err != nil {
		return nil, err
	}
	idx.size, idx.mtime = fi.Size(), fi.ModTime()
	idx.stats.Version = string(ver)
	idx.stats.Snapshot = snapped
	idx.stats.Elapsed = time.Since(t0)
	if err := idx.selfCheck(); err != nil {
		return nil, err
	}
	return idx, nil
}

func checkHeader(r io.ReaderAt) (byte, error) {
	h := make([]byte, len(fileMagic)+1)
	if _, err := r.ReadAt(h, 0); err != nil {
		return 0, fmt.Errorf("%w: %w（读文件头失败: %v）", ErrUnsupportedFormat, errVersion, err)
	}
	if string(h[:len(fileMagic)]) != fileMagic {
		return 0, fmt.Errorf("%w: %w（不是 Nostromo 容器）", ErrUnsupportedFormat, errVersion)
	}
	if v := h[len(fileMagic)]; v != knownVersion {
		return v, fmt.Errorf("%w: %w（版本字节 %q，只支持 %q）", ErrUnsupportedFormat, errVersion, v, knownVersion)
	}
	return h[len(fileMagic)], nil
}

func (x *index) selfCheck() error {
	st := &x.stats
	n := len(x.mails)
	if st.BlocksOK == 0 {
		return fmt.Errorf("%w: 没有可解压的数据块", ErrUnsupportedFormat)
	}
	if n == 0 {
		return fmt.Errorf("%w: 没有识别出任何邮件记录（%d 个有效块）", ErrUnsupportedFormat, st.BlocksOK)
	}
	var subj, addr, sr int
	for _, m := range x.mails {
		if m.Subject != "" || m.Topic != "" {
			subj++
		}
		if isAddr(m.FromAddr) {
			addr++
		}
		if !m.Sent.IsZero() && !m.Sent.After(m.Received.Add(24*time.Hour)) {
			sr++
		}
	}
	st.SubjectRate = float64(subj) / float64(n)
	st.FromAddrRate = float64(addr) / float64(n)
	st.SentRecvRate = float64(sr) / float64(n)
	if st.SubjectRate < checkMinSubject || st.FromAddrRate < checkMinFromAddr || st.SentRecvRate < checkMinSentRecv {
		return fmt.Errorf("%w: 字段自检不达标（主题 %.1f%%、发件人地址 %.1f%%、时间一致 %.1f%%，%d 封）",
			ErrUnsupportedFormat, 100*st.SubjectRate, 100*st.FromAddrRate, 100*st.SentRecvRate, n)
	}
	return nil
}

// ---------- 索引 ----------

type index struct {
	size   int64
	mtime  time.Time
	mails  []*rawMail          // 按接收时间倒序
	byID   map[uint64]*rawMail // 对象 id -> 邮件
	alias  map[uint64]uint64   // 被去重掉的对象 id -> 保留的对象 id（Get 旧 id 仍能取到）
	names  map[uint64]folderName
	blocks map[uint64][]int64 // 对象 id -> 块偏移（新到旧不保证，按文件顺序）
	folder map[uint64]*Folder
	stats  Stats
}

func buildIndex(r io.ReaderAt, size int64) (*index, error) {
	x := &index{byID: map[uint64]*rawMail{}, alias: map[uint64]uint64{}, names: map[uint64]folderName{},
		blocks: map[uint64][]int64{}, folder: map[uint64]*Folder{}}
	var versions []rawMail
	type recipKey struct {
		id uint64
		t  uint16
	}
	scanned := map[recipKey][]string{}
	st, err := scanBlocks(r, size, func(off int64, id uint64, raw []byte) error {
		if id != 0 {
			x.blocks[id] = append(x.blocks[id], off)
		}
		versions = append(versions, extractMails(raw, off)...)
		scanFolderNames(raw, func(id uint64, n folderName) {
			old := x.names[id]
			if n.rank >= old.rank { // 同等来源取文件里靠后的
				if n.canonical == "" {
					n.canonical = old.canonical
				}
				x.names[id] = n
			} else if n.canonical != "" {
				old.canonical = n.canonical
				x.names[id] = old
			}
		})
		// 收件人子对象可能和邮件主体不在同一块，先全部记下来，最后给缺的邮件兜底。
		scanRecipObjects(raw, func(uint64) bool { return true }, func(id uint64, t uint16, a []string) {
			k := recipKey{id, t}
			scanned[k] = mergeAddrs(scanned[k], a)
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	x.stats.Candidates, x.stats.BlocksOK = st.Candidates, st.OK
	x.stats.BadCRC, x.stats.BadHeader, x.stats.BadDecode, x.stats.RawBytes = st.BadCRC, st.BadHeader, st.BadDecode, st.RawBytes
	x.stats.Versions = len(versions)

	// 1) 同对象 id：取修改时间最新的版本，相同时取文件里靠后的块（copy-on-write 新写的块更可能在后面）。
	for i := range versions {
		m := &versions[i]
		if old := x.byID[m.ID]; old != nil {
			x.stats.DupByObject++
			if !newer(m, old) {
				absorb(old, m)
				continue
			}
			absorb(m, old)
		}
		x.byID[m.ID] = m
	}
	// 2) 同 Message-ID：取最新版本，其余丢弃。
	byMsgID := map[string]*rawMail{}
	for _, m := range x.byID {
		k := strings.ToLower(strings.TrimSpace(m.MsgID))
		if old := byMsgID[k]; old != nil {
			x.stats.DupByMsgID++
			if !newer(m, old) {
				absorb(old, m)
				x.alias[m.ID] = old.ID
				delete(x.byID, m.ID)
				continue
			}
			absorb(m, old)
			x.alias[old.ID] = m.ID
			delete(x.byID, old.ID)
		}
		byMsgID[k] = m
	}
	// 3) 收件人兜底 + 文件夹计数
	for _, m := range x.byID {
		if !m.RecipSeen {
			to, okT := scanned[recipKey{m.ID, typeTo}]
			cc, okC := scanned[recipKey{m.ID, typeCc}]
			if okT || okC {
				m.To, m.Cc, m.RecipSeen = to, cc, true
				x.stats.RecipFromScan++
			}
		}
		if m.FolderID != 0 {
			f := x.folder[m.FolderID]
			if f == nil {
				f = &Folder{ID: int64(m.FolderID), ParentID: -1}
				x.folder[m.FolderID] = f
			}
			f.MessageCount++
		}
		x.mails = append(x.mails, m)
	}
	sort.Slice(x.mails, func(i, j int) bool {
		a, b := x.mails[i], x.mails[j]
		if !a.Received.Equal(b.Received) {
			return a.Received.After(b.Received)
		}
		return a.ID > b.ID
	})
	x.nameFolders()
	x.stats.Messages = len(x.mails)
	x.stats.Folders = len(x.folder)
	return x, nil
}

// absorb 把被丢弃的同一封信（同对象旧版本，或同 Message-ID 的另一个对象）的信息并到保留的版本：
// 保留版本没找到收件人子对象而对方有，就沿用对方的；对方的对象 id 记为正文的备选来源。
// 实测同 Message-ID 的两个对象修改时间、接收时间、主题都相同，只是一个带收件人子对象、一个不带。
func absorb(keep, drop *rawMail) {
	if !keep.RecipSeen && drop.RecipSeen {
		keep.To, keep.Cc, keep.RecipSeen = drop.To, drop.Cc, true
	}
	for _, id := range append([]uint64{drop.BodyObjID, drop.ID}, drop.alt...) {
		if id != 0 && id != keep.ID && id != keep.BodyObjID && !containsID(keep.alt, id) {
			keep.alt = append(keep.alt, id)
		}
	}
}

func containsID(l []uint64, id uint64) bool {
	for _, v := range l {
		if v == id {
			return true
		}
	}
	return false
}

func newer(a, b *rawMail) bool {
	if !a.Modified.Equal(b.Modified) {
		return a.Modified.After(b.Modified)
	}
	return a.BlockOff > b.BlockOff
}

// nameFolders 给文件夹起名：用 scanFolderNames 找到的名字；找不到的
// 用「folder-<序号>（共 N 封）」做可读标签并标 NameIncomplete，序号按邮件数从多到少。
func (x *index) nameFolders() {
	fs := make([]*Folder, 0, len(x.folder))
	for _, f := range x.folder {
		fs = append(fs, f)
	}
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].MessageCount != fs[j].MessageCount {
			return fs[i].MessageCount > fs[j].MessageCount
		}
		return fs[i].ID < fs[j].ID
	})
	for i, f := range fs {
		if n, ok := x.names[uint64(f.ID)]; ok {
			f.Name = n.display
			f.Special = specialNames[n.canonical]
		}
		if f.Name == "" {
			f.Name = fmt.Sprintf("folder-%d（共 %d 封）", i+1, f.MessageCount)
			f.NameIncomplete = true
		} else {
			x.stats.FolderNamed++
		}
		f.Path = f.Name
	}
}

// ---------- 文件夹 ----------

// Folder 是一个邮件文件夹。字段形状与 outlooklegacy.Folder 一致。
type Folder struct {
	ID             int64  `json:"id"`
	ParentID       int64  `json:"parent_id"` // 层级未解析，恒为 -1
	Name           string `json:"name"`
	Path           string `json:"path"`
	SpecialType    int    `json:"special_type"`      // 未解析，恒为 0
	Special        string `json:"special,omitempty"` // inbox / sent / drafts ...，来自存储里的英文规范名
	AccountID      int64  `json:"account_id"`        // 未解析，恒为 0
	MessageCount   int    `json:"message_count"`
	Placeholder    bool   `json:"placeholder,omitempty"`
	NameIncomplete bool   `json:"name_incomplete,omitempty"` // 名称是生成的标签，不是 Outlook 里的真名
}

// Folders 列出至少有一封邮件的文件夹，按邮件数从多到少。
func (s *Store) Folders(ctx context.Context) ([]Folder, error) {
	x, err := s.current()
	if err != nil {
		return nil, err
	}
	out := make([]Folder, 0, len(x.folder))
	for _, f := range x.folder {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MessageCount != out[j].MessageCount {
			return out[i].MessageCount > out[j].MessageCount
		}
		return out[i].ID < out[j].ID
	})
	return out, ctx.Err()
}

// ---------- 列表 ----------

// Query 是列表过滤条件，零值字段不过滤。与 outlooklegacy.Query 同形。
type Query struct {
	FolderID int64     // 0 = 全部文件夹
	Since    time.Time // 按接收时间，含
	Until    time.Time // 不含
	From     string    // 发件人显示名或地址子串（不区分大小写）
	Subject  string    // 主题子串（不区分大小写）
	Limit    int       // 默认 50，最大 200
	Offset   int
}

// 字段缺失标记（Summary.Incomplete / Message.Incomplete 里的值）。
const (
	FieldSubject    = "subject"
	FieldFrom       = "from"
	FieldRecipients = "recipients" // To/Cc 都没找到（不代表真的没有收件人）
	FieldSent       = "sent"
	FieldFolder     = "folder"
	FieldFolderName = "folder_name"
	FieldPreview    = "preview"
	FieldBody       = "body"
)

// Summary 是列表里的一封邮件。字段形状与 outlooklegacy.Summary 一致，另加 Incomplete。
type Summary struct {
	ID                  int64     `json:"id"`
	FolderID            int64     `json:"folder_id"`
	Subject             string    `json:"subject"`
	From                string    `json:"from"`
	FromAddress         string    `json:"from_address"`
	To                  []string  `json:"to,omitempty"`
	Cc                  []string  `json:"cc,omitempty"`
	Received            time.Time `json:"received"`
	Sent                time.Time `json:"sent"`
	HasAttachment       bool      `json:"has_attachment"` // 未解析，恒为 false，见 UnsupportedFields
	Read                bool      `json:"read"`           // 未解析，恒为 false，见 UnsupportedFields
	Preview             string    `json:"preview,omitempty"`
	PartiallyDownloaded bool      `json:"partially_downloaded,omitempty"`
	// Incomplete 列出这封信没取到的字段（Field* 常量），空 = 头信息都取到了。
	Incomplete []string `json:"incomplete,omitempty"`
}

// Page 是一页列表结果。
type Page struct {
	Items      []Summary `json:"items"`
	Total      int       `json:"total"`
	NextOffset int       `json:"next_offset"` // -1 表示没有下一页
}

// List 按条件分页列出邮件，按接收时间倒序。
func (s *Store) List(ctx context.Context, q Query) (Page, error) {
	x, err := s.current()
	if err != nil {
		return Page{}, err
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	from, subj := strings.ToLower(q.From), strings.ToLower(q.Subject)
	p := Page{Items: []Summary{}, NextOffset: -1}
	for _, m := range x.mails {
		if q.FolderID != 0 && int64(m.FolderID) != q.FolderID {
			continue
		}
		if !q.Since.IsZero() && m.Received.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && !m.Received.Before(q.Until) {
			continue
		}
		if from != "" && !strings.Contains(strings.ToLower(m.FromName), from) && !strings.Contains(strings.ToLower(m.FromAddr), from) {
			continue
		}
		if subj != "" && !strings.Contains(strings.ToLower(m.Subject), subj) && !strings.Contains(strings.ToLower(m.Topic), subj) {
			continue
		}
		if p.Total >= q.Offset && len(p.Items) < q.Limit {
			p.Items = append(p.Items, x.summary(m))
		}
		p.Total++
	}
	if q.Offset+len(p.Items) < p.Total {
		p.NextOffset = q.Offset + len(p.Items)
	}
	return p, ctx.Err()
}

func (x *index) summary(m *rawMail) Summary {
	sm := Summary{
		ID: int64(m.ID), FolderID: int64(m.FolderID),
		Subject: m.Subject, From: strings.TrimSpace(m.FromName), FromAddress: strings.TrimSpace(m.FromAddr),
		To: m.To, Cc: m.Cc, Received: m.Received, Sent: m.Sent, Preview: strings.TrimSpace(m.Preview),
	}
	if sm.Subject == "" {
		sm.Subject = m.Topic
	}
	if !isAddr(sm.FromAddress) {
		sm.FromAddress = ""
	}
	add := func(cond bool, f string) {
		if cond {
			sm.Incomplete = append(sm.Incomplete, f)
		}
	}
	add(sm.Subject == "", FieldSubject)
	add(sm.From == "" && sm.FromAddress == "", FieldFrom)
	add(!m.RecipSeen, FieldRecipients)
	add(sm.Sent.IsZero(), FieldSent)
	f := x.folder[m.FolderID]
	add(f == nil, FieldFolder)
	add(f != nil && f.NameIncomplete, FieldFolderName)
	add(sm.Preview == "", FieldPreview)
	return sm
}
