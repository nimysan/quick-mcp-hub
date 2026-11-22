package localmail

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nimysan/mcp-hub/internal/localmail/outlookwin"
)

var outlookWinInfo = SourceInfo{
	ID:   SourceOutlookClassicWin,
	Name: "经典版 Outlook for Windows",
	Help: "读经典版 Outlook 在这台电脑上的数据文件（.ost / .pst），只含已经同步到本机的部分。" +
		"Outlook 运行时会锁住这些文件：这时工具会提示先完全退出经典版 Outlook 再试，来源不会被停用。" +
		"新版 Outlook（olk）的离线缓存是另一个开关「新版 Outlook for Windows」",
	DirLabel: "Outlook 数据文件",
	DirHelp: "留空自动查找（先看 Outlook 配置文件里登记的文件，再看 %LOCALAPPDATA%\\Microsoft\\Outlook 和「文档\\Outlook Files」）；" +
		"也可以填 .pst / .ost 文件或所在目录的完整路径，多个用英文分号 ; 隔开",
	GOOS: "windows",
	Open: openOutlookWin,
	Fingerprint: func(opt OpenOptions) string {
		paths, err := winPaths(opt.Dir)
		if err != nil {
			return "-"
		}
		return statSig(paths...)
	},
}

// winStore 是 outlookwin.Store 用到的方法，测试里换成假的。
type winStore interface {
	Folders(ctx context.Context) ([]outlookwin.Folder, error)
	List(ctx context.Context, q outlookwin.Query) (outlookwin.Page, error)
	Get(ctx context.Context, id int64) (*outlookwin.Message, error)
	Stale() bool
	Close() error
}

var (
	openWinStore = func(path string, opt outlookwin.Options) (winStore, error) {
		s, err := outlookwin.Open(path, opt)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	discoverWinFiles = outlookwin.DefaultDataFiles
)

// winIdleClose 是没有调用之后多久关掉文件句柄。一直开着句柄，经典版 Outlook 启动时
// 如果要求独占打开就会失败；空闲就关，下一次调用再开（文件夹索引随之重建）。
var winIdleClose = 30 * time.Second

// winPageMax 是 outlookwin.List 单页上限。
const winPageMax = 200

// outlookWinSource 把一台机器上的多个 .pst / .ost 聚合成一个来源。
// ID 的高 32 位是文件序号，低 32 位是文件里的 NID。
type outlookWinSource struct {
	paths   []string
	maxBody int

	mu       sync.Mutex
	stores   []winStore // nil 表示句柄已因空闲关闭；格式不支持而跳过的文件对应 nil
	inflight int
	lastUse  time.Time
	timer    *time.Timer
	closed   bool
}

func openOutlookWin(opt OpenOptions) (Source, error) {
	paths, err := winPaths(opt.Dir)
	if err != nil {
		return nil, err
	}
	o := &outlookWinSource{paths: paths, maxBody: opt.MaxBodyBytes}
	if err := o.openAll(); err != nil {
		return nil, err
	}
	o.lastUse = time.Now()
	o.timer = time.AfterFunc(winIdleClose, o.idle)
	return o, nil
}

// winPaths 解析配置里的路径；留空时自动发现。目录展开成里面的 .pst / .ost。
func winPaths(dir string) ([]string, error) {
	var paths []string
	if strings.TrimSpace(dir) == "" {
		files, err := discoverWinFiles()
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		if len(paths) == 0 {
			return nil, errors.Join(ErrNotFound, errors.New("这台电脑上没找到经典版 Outlook 的 .ost / .pst"+
				"（Outlook 配置文件里没有登记，%LOCALAPPDATA%\\Microsoft\\Outlook 和「文档\\Outlook Files」里也没有）。"+
				"如果数据文件在别处，请在配置里填它的完整路径"))
		}
		return paths, nil
	}
	seen := map[string]bool{}
	for _, p := range strings.Split(dir, ";") {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p == "" {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, errors.Join(ErrNotFound, fmt.Errorf("配置里填的 %s 不存在", p))
			}
			return nil, err
		}
		cand := []string{p}
		if fi.IsDir() {
			cand = nil
			ents, err := os.ReadDir(p)
			if err != nil {
				return nil, err
			}
			for _, e := range ents {
				ext := strings.ToLower(filepath.Ext(e.Name()))
				if !e.IsDir() && (ext == ".pst" || ext == ".ost") {
					cand = append(cand, filepath.Join(p, e.Name()))
				}
			}
		}
		for _, c := range cand {
			k := strings.ToLower(filepath.Clean(c))
			if !seen[k] {
				seen[k] = true
				paths = append(paths, c)
			}
		}
	}
	if len(paths) == 0 {
		return nil, errors.Join(ErrNotFound, fmt.Errorf("配置里填的路径下没有 .pst / .ost：%s", dir))
	}
	return paths, nil
}

// openAll 打开全部文件。任何一个被占用就整体报 ErrLocked（Outlook 运行时它的文件一般同时被锁，
// 只给一部分邮箱容易让人以为信丢了）；格式不支持的文件跳过，全都不支持才报 ErrUnsupported。
// 调用方持有 o.mu，或者 o 还没有发布。
func (o *outlookWinSource) openAll() error {
	var stores []winStore
	var locked, skipped []string
	var unsupported error
	closeAll := func() {
		for _, s := range stores {
			if s != nil {
				s.Close()
			}
		}
	}
	for _, p := range o.paths {
		s, err := openWinStore(p, outlookwin.Options{MaxBodyBytes: o.maxBody})
		switch {
		case err == nil:
			stores = append(stores, s)
		case errors.Is(err, outlookwin.ErrLocked):
			locked = append(locked, filepath.Base(p))
			stores = append(stores, nil)
		case errors.Is(err, outlookwin.ErrUnsupported):
			skipped = append(skipped, filepath.Base(p))
			unsupported = err
			stores = append(stores, nil)
		case errors.Is(err, fs.ErrNotExist):
			closeAll()
			return errors.Join(ErrNotFound, err)
		default:
			closeAll()
			return err
		}
	}
	if len(locked) > 0 {
		closeAll()
		return errors.Join(ErrLocked, fmt.Errorf("%s 正被其他程序（多半是经典版 Outlook）锁住", strings.Join(locked, "、")))
	}
	if len(skipped) == len(o.paths) {
		closeAll()
		return errors.Join(ErrUnsupported, unsupported)
	}
	// shortcut: 跳过的文件只是不出现在结果里，没有告诉用户；以后可以在文件夹列表里给一条说明。
	o.stores = stores
	return nil
}

// use 取得可用的文件句柄（空闲关掉了就重开）。用完调 done。
func (o *outlookWinSource) use() ([]winStore, func(), error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, nil, errHubClosed
	}
	if o.stores == nil {
		if err := o.openAll(); err != nil {
			return nil, nil, err
		}
	}
	o.inflight++
	stores := o.stores
	return stores, func() {
		o.mu.Lock()
		o.inflight--
		o.lastUse = time.Now()
		if o.inflight == 0 && !o.closed {
			o.timer.Reset(winIdleClose)
		}
		o.mu.Unlock()
	}, nil
}

// idle 在空闲超时后关掉文件句柄。
func (o *outlookWinSource) idle() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.inflight > 0 || o.stores == nil {
		return
	}
	if wait := winIdleClose - time.Since(o.lastUse); wait > 0 {
		o.timer.Reset(wait)
		return
	}
	for _, s := range o.stores {
		if s != nil {
			s.Close()
		}
	}
	o.stores = nil
}

func (o *outlookWinSource) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	if o.timer != nil {
		o.timer.Stop()
	}
	var first error
	for _, s := range o.stores {
		if s != nil {
			if err := s.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	o.stores = nil
	return first
}

// Stale 为真时 Hub 会重开（重新发现文件）：某个文件在打开后被改过。
func (o *outlookWinSource) Stale() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, s := range o.stores {
		if s != nil && s.Stale() {
			return true
		}
	}
	return false
}

func winErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, outlookwin.ErrLocked):
		return errors.Join(ErrLocked, err)
	case errors.Is(err, outlookwin.ErrUnsupported):
		return errors.Join(ErrUnsupported, err)
	case errors.Is(err, outlookwin.ErrNotFound):
		return errors.Join(ErrNotFound, err)
	}
	return err
}

func winID(idx int, nid int64) int64 { return int64(idx)<<32 | nid }

func splitWinID(id int64) (int, int64) { return int(id >> 32), id & 0xFFFFFFFF }

// label 是文件在文件夹路径和账户里的名字：文件名去掉扩展名（.ost 一般就是邮箱地址）。
func (o *outlookWinSource) label(idx int) string {
	b := filepath.Base(o.paths[idx])
	return strings.TrimSuffix(b, filepath.Ext(b))
}

func (o *outlookWinSource) Folders(ctx context.Context) ([]Folder, error) {
	stores, done, err := o.use()
	if err != nil {
		return nil, err
	}
	defer done()
	var out []Folder
	for i, s := range stores {
		if s == nil {
			continue
		}
		fs, err := s.Folders(ctx)
		if err != nil {
			return nil, winErr(err)
		}
		for _, f := range fs {
			if !f.Mail {
				continue // 日历、联系人等不是邮件文件夹
			}
			p := f.Path
			if len(o.paths) > 1 {
				p = o.label(i) + "/" + p
			}
			out = append(out, Folder{
				ID: winID(i, f.ID), Path: p, Account: o.label(i), Special: f.Special,
				Total: int64(f.MessageCount), Unread: int64(f.UnreadCount),
			})
		}
	}
	return out, nil
}

func winQuery(q Query, limit, offset int) outlookwin.Query {
	return outlookwin.Query{Since: q.Since, Until: q.Before, From: q.From, Subject: q.Subject, Limit: limit, Offset: offset}
}

func (o *outlookWinSource) Search(ctx context.Context, q Query) (Page, error) {
	stores, done, err := o.use()
	if err != nil {
		return Page{}, err
	}
	defer done()
	type item struct {
		idx int
		m   outlookwin.Summary
	}
	var items []item
	total := 0
	need := q.Offset + q.Limit
	if q.FolderID != 0 {
		idx, nid := splitWinID(q.FolderID)
		if idx < 0 || idx >= len(stores) || stores[idx] == nil {
			return Page{}, fmt.Errorf("%w: 文件夹 %d", ErrNotFound, q.FolderID)
		}
		wq := winQuery(q, q.Limit, q.Offset)
		wq.FolderID = nid
		p, err := stores[idx].List(ctx, wq)
		if err != nil {
			return Page{}, winErr(err)
		}
		out := Page{Total: p.Total, Messages: make([]Summary, 0, len(p.Items))}
		if p.NextOffset >= 0 {
			out.HasMore, out.NextOffset = true, p.NextOffset
		}
		for _, m := range p.Items {
			out.Messages = append(out.Messages, winSummary(idx, m))
		}
		return out, nil
	}
	// 跨文件：每个文件取前 offset+limit 封，合并后按时间倒序再切这一页。
	// shortcut: offset 很大时每个文件都要翻到那里，文件多、信多时慢；以后可以改成按文件游标翻页。
	for i, s := range stores {
		if s == nil {
			continue
		}
		got := 0
		for off := 0; got < need; {
			p, err := s.List(ctx, winQuery(q, min(winPageMax, need-got), off))
			if err != nil {
				return Page{}, winErr(err)
			}
			if off == 0 {
				total += p.Total
			}
			for _, m := range p.Items {
				items = append(items, item{i, m})
			}
			got += len(p.Items)
			if p.NextOffset < 0 || len(p.Items) == 0 {
				break
			}
			off = p.NextOffset
		}
	}
	sort.SliceStable(items, func(a, b int) bool {
		da, db := winDate(items[a].m), winDate(items[b].m)
		if !da.Equal(db) {
			return da.After(db)
		}
		return winID(items[a].idx, items[a].m.ID) > winID(items[b].idx, items[b].m.ID)
	})
	out := Page{Total: total, Messages: []Summary{}}
	for i := q.Offset; i < len(items) && i < need; i++ {
		out.Messages = append(out.Messages, winSummary(items[i].idx, items[i].m))
	}
	if need < total {
		out.HasMore, out.NextOffset = true, need
	}
	return out, nil
}

func winDate(m outlookwin.Summary) time.Time {
	if !m.Received.IsZero() {
		return m.Received
	}
	return m.Sent
}

func winSummary(idx int, m outlookwin.Summary) Summary {
	from := m.From
	if from == "" {
		from = m.FromAddress
	}
	s := Summary{
		ID: winID(idx, m.ID), FolderID: winID(idx, m.FolderID), Subject: m.Subject,
		From: from, FromAddress: m.FromAddress, To: m.To, Date: winDate(m),
		Read: m.Read, HasAttachment: m.HasAttachment, Preview: m.Preview,
	}
	if s.Date.IsZero() {
		s.Incomplete = []string{"date"}
	}
	return s
}

func (o *outlookWinSource) Get(ctx context.Context, id int64) (*Message, error) {
	stores, done, err := o.use()
	if err != nil {
		return nil, err
	}
	defer done()
	idx, nid := splitWinID(id)
	if idx < 0 || idx >= len(stores) || stores[idx] == nil {
		return nil, fmt.Errorf("%w: 邮件 %d", ErrNotFound, id)
	}
	m, err := stores[idx].Get(ctx, nid)
	if err != nil {
		return nil, winErr(err)
	}
	out := &Message{
		Summary: winSummary(idx, m.Summary), Cc: m.Cc, MessageID: m.MessageID,
		Body: m.Body, BodyTruncated: m.BodyTruncated,
	}
	if len(m.Recipients) > 0 { // 收件人表比内容表里的显示名串更准
		var to, cc []string
		for _, r := range m.Recipients {
			v := r.Name
			if r.Address != "" && r.Address != r.Name {
				v = strings.TrimSpace(r.Name + " <" + r.Address + ">")
			}
			switch r.Type {
			case "to":
				to = append(to, v)
			case "cc":
				cc = append(cc, v)
			}
		}
		out.To, out.Cc = to, cc
	}
	switch m.BodySource {
	case outlookwin.BodyHTML:
		out.BodyNote = "正文由 HTML 转成纯文本，格式可能和原信不同"
	case outlookwin.BodyRTF:
		out.BodyNote = "正文由 RTF 粗略转成纯文本，表格等格式会丢失"
	case outlookwin.BodyNone:
		out.BodyNote = "数据文件里没有这封信的正文"
	}
	for _, at := range m.Attachments {
		out.Attachments = append(out.Attachments, Attachment{Name: at.Name, Size: at.Size, ContentType: at.ContentType})
	}
	out.HasAttachment = out.HasAttachment || len(out.Attachments) > 0
	return out, nil
}
