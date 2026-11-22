// Package localmail 把本机各邮件客户端的只读读取包统一成一个小接口，
// 供 MCP 工具层按 source 参数分派。
//
// 只读是这一层的硬约束：接口里没有任何写方法，适配器只调用各读取包的
// 查询函数；各读取包自己以 SQLite mode=ro + query_only 打开数据库、
// 只用 os.Open 读文件，绝不写邮件客户端的任何文件。
//
// 新增一个来源（例如新版 Outlook 的 hxstore）只需要：
//  1. 写一个适配器文件，实现 Source；
//  2. 在 allSources 里加一条 SourceInfo，GOOS 填它所在的平台。
//
// 配置页的开关、工具描述里的来源清单都从 Sources 生成，不需要再改别处。
// Sources 只含当前平台的来源：Mac 的三个来源只在 darwin 上出现，
// 经典版和新版 Outlook for Windows 只在 windows 上出现。
package localmail

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// 来源标识，即工具参数 source 的取值，也是配置页开关的字段名。
const (
	SourceAppleMail     = "apple_mail"
	SourceOutlookLegacy = "outlook_legacy"
	SourceOutlook       = "outlook" // 新版 Outlook（HxStore），读取包在 internal/localmail/hxstore
	// SourceOutlookClassicWin 是经典版 Outlook for Windows 的 .pst / .ost，读取包在 internal/localmail/outlookwin。
	SourceOutlookClassicWin = "outlook_classic_win"
	// SourceOutlookNewWin 是新版 Outlook for Windows（olk.exe）的离线缓存，读取包在 internal/localmail/olkwin。
	SourceOutlookNewWin = "outlook_new_win"
)

// 默认值与上限。
const (
	DefaultMaxBodyBytes = 32 << 10 // 和 IMAP 渠道 read_mail 的默认值一致
	DefaultMaxResults   = 50       // 单次列表最多返回几封
	HardMaxResults      = 200      // 配置里也不能超过它（旧版 Outlook 读取包的上限）
	DefaultLimit        = 20
	maxPreviewRunes     = 200 // 列表里的摘要只给开头一段，避免列表本身撑爆上下文
)

// ErrAccess 表示被 macOS 隐私保护（TCC）拦下。用 errors.Is 判断。
var ErrAccess = errors.New("无权读取本机邮件数据")

// ErrFilePermission 表示普通的文件权限不足（EACCES），和隐私保护无关。
var ErrFilePermission = errors.New("文件权限不足")

// ErrUnsupported 表示来源的存储格式本构建不认识（例如邮件客户端升级后格式变了），
// 读取包 panic 时 Hub 也把它记成这个错误。Hub 先重开重试一次，仍然失败才停用该来源；
// 停用后数据文件的大小或修改时间变了（SourceInfo.Fingerprint 变了）会自动再试。
var ErrUnsupported = errors.New("存储格式不支持")

// ErrNotFound 表示数据目录、文件夹或邮件不存在。
var ErrNotFound = errors.New("未找到")

// ErrLocked 表示数据文件正被邮件客户端占用（经典版 Outlook for Windows 运行时锁住 .pst / .ost）。
// 这是暂时的：Hub 不停用来源，用户退出客户端后下一次调用就能读。
var ErrLocked = errors.New("数据文件正被占用")

// Folder 是一个邮件文件夹。
type Folder struct {
	ID      int64  `json:"id"`
	Path    string `json:"path"`
	Account string `json:"account,omitempty"` // 来源内部的账户标识（不是邮箱地址时也照原样给）
	Special string `json:"special,omitempty"` // inbox / sent / drafts …，读取包认得时才有
	Total   int64  `json:"total"`             // -1 表示来源没有这个统计
	Unread  int64  `json:"unread,omitempty"`
	// NameIncomplete 为真表示来源里读不到真名，Path 是生成的标签（如 folder-3）。
	NameIncomplete bool `json:"name_incomplete,omitempty"`
}

// Summary 是列表里的一封邮件，不含正文。
type Summary struct {
	ID            int64     `json:"id"`
	FolderID      int64     `json:"folder_id"`
	Subject       string    `json:"subject"`
	From          string    `json:"from"` // 显示名，没有时是地址
	FromAddress   string    `json:"from_address,omitempty"`
	To            []string  `json:"to,omitempty"`
	Date          time.Time `json:"date"` // 接收时间
	Read          bool      `json:"read"`
	HasAttachment bool      `json:"has_attachment,omitempty"`
	Preview       string    `json:"preview,omitempty"`
	// Incomplete 列出这封信没取到的字段（值为字段名，如 read、recipients、folder_name）。
	// 列在这里的字段是零值，不代表真实状态 —— 例如 read=false 不等于未读。
	Incomplete []string `json:"incomplete,omitempty"`
}

// Attachment 只描述附件，不给内容。
type Attachment struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"` // -1 表示未知
	ContentType string `json:"content_type,omitempty"`
}

// Message 是一封邮件的详情。
type Message struct {
	Summary
	Cc            []string     `json:"cc,omitempty"`
	MessageID     string       `json:"message_id,omitempty"`
	Body          string       `json:"body"`
	BodyTruncated bool         `json:"body_truncated,omitempty"`
	BodyNote      string       `json:"body_note,omitempty"` // 正文不完整或读不出时的说明
	Attachments   []Attachment `json:"attachments,omitempty"`
}

// Query 是列表条件，零值字段不过滤。
type Query struct {
	FolderID int64
	Since    time.Time // 含
	Before   time.Time // 不含
	From     string
	Subject  string
	Limit    int
	Offset   int
}

// Page 是一页结果。
type Page struct {
	Messages   []Summary `json:"messages"`
	HasMore    bool      `json:"has_more"`
	NextOffset int       `json:"next_offset,omitempty"`
	Total      int       `json:"total,omitempty"` // 来源能给出总数时才有
}

// Source 是一个本机邮件来源。实现必须只读、并发安全。
type Source interface {
	Folders(ctx context.Context) ([]Folder, error)
	Search(ctx context.Context, q Query) (Page, error)
	Get(ctx context.Context, id int64) (*Message, error)
	Close() error
}

// OpenOptions 是打开一个来源时的参数。
type OpenOptions struct {
	Dir          string // 数据目录，空串用来源的默认位置
	MaxBodyBytes int
}

// SourceInfo 描述一种来源。
type SourceInfo struct {
	ID   string
	Name string // 给用户看的名字
	// Help 显示在配置页开关下面。
	Help string
	// DirLabel / DirHelp 是「自定义数据目录」高级字段的文案。
	DirLabel, DirHelp string
	// GOOS 是这个来源所在的平台（runtime.GOOS 的取值）。别的平台上不注册、不显示。
	GOOS string
	// ToolNote 非空时，启用了这个来源就附在工具说明后面（例如覆盖范围的限制）。
	ToolNote string
	Open     func(opt OpenOptions) (Source, error)
	// Fingerprint 返回数据文件的大小和修改时间签名，只 stat、不读内容。
	// 来源因格式不支持停用后，签名变了就解除停用；为 nil 时停用到服务重启。
	Fingerprint func(opt OpenOptions) string
}

// allSources 是本构建认识的全部来源，按配置页上的显示顺序。
var allSources = []SourceInfo{appleMailInfo, outlookLegacyInfo, outlookInfo, outlookWinInfo, outlookNewWinInfo}

// Sources 是当前平台上可用的来源。某个平台上一个都没有时为空，本机邮件渠道随之不可添加。
var Sources = SourcesFor(runtime.GOOS)

// SourcesFor 返回平台 goos 上的来源，按显示顺序。
func SourcesFor(goos string) []SourceInfo {
	var out []SourceInfo
	for _, s := range allSources {
		if s.GOOS == goos {
			out = append(out, s)
		}
	}
	return out
}

// SourceByID 按标识取来源。
func SourceByID(id string) (SourceInfo, bool) {
	for _, s := range Sources {
		if s.ID == id {
			return s, true
		}
	}
	return SourceInfo{}, false
}

// Config 是本机邮件渠道的运行配置。
type Config struct {
	Sources      []SourceConfig
	MaxBodyBytes int
	MaxResults   int
}

// SourceConfig 是一个已启用的来源。
type SourceConfig struct {
	ID  string
	Dir string
}

// Hub 惰性打开各来源并缓存。打开失败不缓存：用户授权之后重试就能成功，
// 不需要重启服务（macOS 的完全磁盘访问权限通常要重启宿主程序才生效，
// 那一步由用户做，这里不假设）。
//
// 打开按来源各自进行，同一来源同时只打开一次（singleflight），不持有全局锁：
// 新版 Outlook 第一次打开要建索引，不能拖住其他来源。调用方只等到自己的 ctx
// 结束为止；打开本身在后台继续，结果留给下一次调用。
type Hub struct {
	cfg    Config
	mu     sync.Mutex
	open   map[string]*entry    // 已打开或正在打开的来源
	dead   map[string]*disabled // 格式不支持而停用的来源
	closed bool
}

// disabled 是一个停用的来源。数据文件签名和 fp 不同时解除停用。
type disabled struct {
	err  error
	info SourceInfo
	opt  OpenOptions
	fp   string // 空串表示来源没有签名，停用到服务重启
}

// entry 是一个来源的打开结果。ready 关闭之后 src / err 不再改变。
// refs、retired 由 Hub.mu 保护：retired 之后不再发给新调用，
// refs 归零时才关闭 src，正在进行的调用不会用到已关闭的来源。
type entry struct {
	ready   chan struct{}
	src     Source
	err     error
	done    bool // 打开已结束（和 ready 关闭同时设置，在锁内读）
	refs    int
	retired bool
}

// staler 是来源可选实现的接口：Stale 为真表示缓存的连接该重开
// （例如 Apple Mail 以 immutable 打开后 Mail.app 启动了，重开才能读到新邮件）。
type staler interface{ Stale() bool }

var errHubClosed = errors.New("本机邮件服务已关闭")

// NewHub 规范化配置并返回 Hub。不打开任何来源。
func NewHub(cfg Config) *Hub {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.MaxResults <= 0 {
		cfg.MaxResults = DefaultMaxResults
	}
	if cfg.MaxResults > HardMaxResults {
		cfg.MaxResults = HardMaxResults
	}
	return &Hub{cfg: cfg, open: map[string]*entry{}, dead: map[string]*disabled{}}
}

// Config 返回规范化后的配置。
func (h *Hub) Config() Config { return h.cfg }

// Enabled 返回已启用来源的标识，按配置顺序。
func (h *Hub) Enabled() []string {
	out := make([]string, 0, len(h.cfg.Sources))
	for _, s := range h.cfg.Sources {
		out = append(out, s.ID)
	}
	return out
}

// Pick 选来源：只启用了一个时 id 可以为空；启用了多个时必须指明。
func (h *Hub) Pick(id string) (string, error) {
	ids := h.Enabled()
	id = strings.TrimSpace(id)
	if id == "" {
		if len(ids) == 1 {
			return ids[0], nil
		}
		return "", fmt.Errorf("启用了多个来源，必须用 source 指明一个：%s", strings.Join(ids, " / "))
	}
	for _, x := range ids {
		if x == id {
			return id, nil
		}
	}
	return "", fmt.Errorf("来源 %q 未启用，可用的是：%s", id, strings.Join(ids, " / "))
}

// acquire 返回一个已打开的来源，第一次调用时打开。用完必须调 release。
func (h *Hub) acquire(ctx context.Context, id string) (*entry, error) {
	for {
		e, err := h.entryFor(id)
		if err != nil {
			return nil, err
		}
		select {
		case <-e.ready:
		case <-ctx.Done():
			return nil, fmt.Errorf("等待打开来源 %s 时超时或被取消（打开仍在后台进行，稍后重试即可）: %w", id, ctx.Err())
		}
		if e.err != nil {
			return nil, e.err
		}
		h.mu.Lock()
		if !e.retired {
			e.refs++
			h.mu.Unlock()
			return e, nil
		}
		h.mu.Unlock() // 等待期间被停用或换成了新连接：重新取
	}
}

// entryFor 取 id 的打开条目，没有就开始打开；缓存的连接过期了就停用它、重新打开。
func (h *Hub) entryFor(id string) (*entry, error) {
	h.mu.Lock()
	d := h.dead[id]
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return nil, errHubClosed
	}
	if d != nil { // 签名在锁外算：要 stat 文件
		if d.fp == "" || fingerprint(d.info, d.opt) == d.fp {
			return nil, d.err
		}
		h.mu.Lock()
		if h.dead[id] == d {
			delete(h.dead, id)
			log.Printf("本机邮件：来源 %s 的数据文件变了，解除停用，重新打开", id)
		}
		h.mu.Unlock()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errHubClosed
	}
	if d := h.dead[id]; d != nil {
		return nil, d.err
	}
	e := h.open[id]
	if e != nil && e.done && e.err == nil {
		if st, ok := e.src.(staler); ok && safeStale(id, st) {
			if s := h.retireLocked(id, e); s != nil {
				go safeClose(id, s) // 没有调用在用它，锁外关闭
			}
			e = nil
		}
	}
	if e != nil {
		return e, nil
	}
	var sc *SourceConfig
	for i := range h.cfg.Sources {
		if h.cfg.Sources[i].ID == id {
			sc = &h.cfg.Sources[i]
		}
	}
	info, ok := SourceByID(id)
	if sc == nil || !ok {
		return nil, fmt.Errorf("来源 %q 未启用", id)
	}
	e = &entry{ready: make(chan struct{})}
	h.open[id] = e
	go h.doOpen(id, info, OpenOptions{Dir: sc.Dir, MaxBodyBytes: h.cfg.MaxBodyBytes}, e)
	return e, nil
}

// release 归还一次 acquire。条目已停用且没人在用时关闭来源。
func (h *Hub) release(e *entry) {
	h.mu.Lock()
	e.refs--
	last := e.retired && e.refs == 0
	h.mu.Unlock()
	if last {
		safeClose("", e.src)
	}
}

// retireLocked 停用一个条目：不再发给新调用。返回现在就可以关闭的来源
// （打开已成功且没有调用在用），否则返回 nil，由最后一个 release 或 doOpen 关闭。
// 调用方持有 h.mu。
func (h *Hub) retireLocked(id string, e *entry) Source {
	if h.open[id] == e {
		delete(h.open, id)
	}
	if e.retired {
		return nil
	}
	e.retired = true
	if e.done && e.src != nil && e.refs == 0 {
		return e.src
	}
	return nil
}

// doOpen 在后台打开一个来源，结果写进 e。格式不支持时重试一次（可能碰巧读到客户端写了一半的数据），
// 仍然不支持才停用。
func (h *Hub) doOpen(id string, info SourceInfo, opt OpenOptions, e *entry) {
	s, err := safeOpen(id, info, opt)
	if err != nil && errors.Is(err, ErrUnsupported) {
		s, err = safeOpen(id, info, opt)
	}
	var d *disabled
	if err != nil {
		err = Explain(info, err)
		if errors.Is(err, ErrUnsupported) {
			d = &disabled{err: err, info: info, opt: opt, fp: fingerprint(info, opt)}
		}
	}
	h.mu.Lock()
	var orphan Source
	if e.retired && err == nil { // 打开期间 Hub 关了：结果没人用，直接关掉
		orphan, s, err = s, nil, errHubClosed
	}
	e.src, e.err, e.done = s, err, true
	if err != nil {
		if h.open[id] == e {
			delete(h.open, id) // 失败不缓存
		}
		if d != nil {
			h.dead[id] = d
		}
	}
	h.mu.Unlock()
	close(e.ready)
	if orphan != nil {
		safeClose(id, orphan)
	}
}

// call 在来源上做一次操作。读取包 panic 时转成格式不支持的错误；格式不支持时
// 停掉这个连接、重开再试一次，仍然不支持才停用来源。
func (h *Hub) call(ctx context.Context, id, op string, fn func(Source) error) error {
	for attempt := 0; ; attempt++ {
		e, err := h.acquire(ctx, id)
		if err != nil {
			return err
		}
		err = guard(id, op, func() error { return fn(e.src) })
		if err != nil && attempt == 0 && errors.Is(err, ErrUnsupported) && ctx.Err() == nil {
			h.mu.Lock()
			s := h.retireLocked(id, e) // 本次调用还持有引用，一般是 nil，由 release 关闭
			h.mu.Unlock()
			if s != nil {
				safeClose(id, s)
			}
			h.release(e)
			continue
		}
		if err != nil {
			err = h.fail(id, e, err)
		}
		h.release(e)
		return err
	}
}

// Search 在一个来源里列邮件，限制条数并截短摘要。
func (h *Hub) Search(ctx context.Context, id string, q Query) (Page, error) {
	q.Limit = ClampLimit(q.Limit, h.cfg.MaxResults)
	if q.Offset < 0 {
		q.Offset = 0
	}
	var p Page
	if err := h.call(ctx, id, "search", func(s Source) (err error) {
		p, err = s.Search(ctx, q)
		return err
	}); err != nil {
		return Page{}, err
	}
	if len(p.Messages) > q.Limit { // 适配器理应已经限好，这里再兜一次
		p.Messages, p.HasMore, p.NextOffset = p.Messages[:q.Limit], true, q.Offset+q.Limit
	}
	for i := range p.Messages {
		p.Messages[i].Preview = truncRunes(p.Messages[i].Preview, maxPreviewRunes)
	}
	if p.Messages == nil {
		p.Messages = []Summary{}
	}
	return p, nil
}

// Get 取一封邮件，正文不超过 MaxBodyBytes。
func (h *Hub) Get(ctx context.Context, id string, msgID int64) (*Message, error) {
	var m *Message
	if err := h.call(ctx, id, "get", func(s Source) (err error) {
		m, err = s.Get(ctx, msgID)
		return err
	}); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, explainID(id, errors.Join(ErrNotFound, fmt.Errorf("邮件 %d", msgID)))
	}
	if b, cut := TruncateUTF8(m.Body, h.cfg.MaxBodyBytes); cut {
		m.Body, m.BodyTruncated = b, true
	}
	return m, nil
}

// Folders 列一个来源的文件夹。
func (h *Hub) Folders(ctx context.Context, id string) ([]Folder, error) {
	var f []Folder
	if err := h.call(ctx, id, "folders", func(s Source) (err error) {
		f, err = s.Folders(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	if f == nil {
		f = []Folder{}
	}
	return f, nil
}

// Close 停用所有来源。没有调用在用的立即关闭；正在用的等最后一个调用结束再关；
// 正在打开的打开完成后自动关闭。
func (h *Hub) Close() error {
	h.mu.Lock()
	h.closed = true
	var srcs = map[string]Source{}
	for id, e := range h.open {
		if s := h.retireLocked(id, e); s != nil {
			srcs[id] = s
		}
	}
	h.mu.Unlock()
	var first error
	for id, s := range srcs {
		if err := safeClose(id, s); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Explain 把读取包的错误变成用户能据以行动的话。权限错误给出授权步骤，
// 而不是一句 operation not permitted。
func Explain(info SourceInfo, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrLocked) && info.ID == SourceOutlookNewWin {
		// 新版 Outlook 运行时本来就能读，被锁多半是别的程序（杀毒、备份）独占了文件，等一会儿重试。
		return fmt.Errorf("%w：%s 的数据文件暂时打不开（%s）。新版 Outlook 开着也能读，"+
			"这多半是杀毒或备份软件正独占着文件；这个来源没有停用，稍后重试即可。", ErrLocked, info.Name, cause(err))
	}
	if errors.Is(err, ErrLocked) {
		return fmt.Errorf("%w：%s 的数据文件正被占用（%s）。"+
			"请完全退出经典版 Outlook（关掉窗口后，任务管理器里也不能再有 OUTLOOK.EXE），然后再试一次。"+
			"这个来源没有停用，Outlook 退出后直接重试即可；本程序不会强行读取被锁住的文件。", ErrLocked, info.Name, cause(err))
	}
	if errors.Is(err, ErrAccess) || (runtime.GOOS == "darwin" && isTCC(err)) {
		return fmt.Errorf("%w：%s 的数据被 macOS 隐私保护拦下。"+
			"请打开「系统设置 → 隐私与安全性 → 完全磁盘访问权限」，"+
			"把启动 mcp-hub 的程序（在 Amazon Quick 里用就是 Amazon Quick）加进去并打开开关，"+
			"然后完全退出 Amazon Quick 再重新打开。本程序不会尝试绕过这项保护。", ErrAccess, info.Name)
	}
	if errors.Is(err, fs.ErrPermission) {
		if runtime.GOOS != "darwin" {
			return fmt.Errorf("%w：当前用户没有读取 %s 数据的文件权限（%v）。"+
				"请检查文件的属主和权限，或者在配置里改填正确的路径。", ErrFilePermission, info.Name, err)
		}
		return fmt.Errorf("%w：当前用户没有读取 %s 数据的文件权限（%v）。"+
			"这是普通的文件权限问题，不是 macOS 隐私保护，开「完全磁盘访问权限」解决不了。"+
			"请检查数据目录的属主和权限（例如目录属于别的用户，或被改成了不可读），"+
			"或者在配置里改填正确的数据目录。", ErrFilePermission, info.Name, err)
	}
	if errors.Is(err, errPanic) {
		return fmt.Errorf("%w：读取 %s 的数据时 mcp-hub 内部出错（%w）。重开重试后仍然失败，这个来源已停用，其他来源不受影响；"+
			"「%s」改写数据文件后（例如同步了新邮件）会自动再试。日志里只有出错位置，不含邮件内容，可以发给维护者。",
			ErrUnsupported, info.Name, errPanic, info.Name)
	}
	if errors.Is(err, ErrUnsupported) {
		return fmt.Errorf("%w：%s 的本地存储格式这个版本的 mcp-hub 读不了（多半是「%s」升级了）。"+
			"重开重试后仍然失败，这个来源已停用，其他来源不受影响；"+
			"「%s」改写数据文件后（例如同步了新邮件）会自动再试，否则请等 mcp-hub 更新。详情：%s",
			ErrUnsupported, info.Name, info.Name, info.Name, cause(err))
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w：%s %s", ErrNotFound, info.Name, cause(err))
	}
	return fmt.Errorf("%s: %w", info.Name, err)
}

// fail 解释一次调用的错误；格式不支持时停用这个来源，等正在进行的调用都结束再关闭。
// 调用方（call）已经重开重试过一次。
func (h *Hub) fail(id string, e *entry, err error) error {
	err = explainID(id, err)
	if errors.Is(err, ErrUnsupported) {
		d := &disabled{err: err}
		if info, ok := SourceByID(id); ok {
			d.info, d.opt = info, h.openOptions(id)
			d.fp = fingerprint(info, d.opt) // 锁外 stat
		}
		h.mu.Lock()
		first := h.dead[id] == nil
		h.dead[id] = d
		s := h.retireLocked(id, e) // 本次调用还持有引用，这里一般是 nil，由 release 关闭
		h.mu.Unlock()
		if s != nil {
			safeClose(id, s)
		}
		if first {
			log.Printf("本机邮件：来源 %s 格式不支持，重试后仍失败，已停用（数据文件变了会自动再试）", id)
		}
	}
	return err
}

func (h *Hub) openOptions(id string) OpenOptions {
	opt := OpenOptions{MaxBodyBytes: h.cfg.MaxBodyBytes}
	for _, sc := range h.cfg.Sources {
		if sc.ID == id {
			opt.Dir = sc.Dir
		}
	}
	return opt
}

// errPanic 是读取包 panic 后返回给调用方的原因。panic 值本身可能带数据，不放进错误。
var errPanic = errors.New("读取这个来源时程序内部出错（已写入 mcp-hub 日志）")

// guard 执行 fn，把 panic 转成格式不支持的错误：解析器遇到没见过的数据越界时，
// 只让这一次调用失败，不让整个 mcp-hub 进程退出、带断其他渠道。
func guard(id, op string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logPanic(id, op, r)
			err = errors.Join(ErrUnsupported, errPanic)
		}
	}()
	return fn()
}

// logPanic 记一条日志：来源、操作、panic 类型和发生位置（函数、文件、行号）。
// 不记 panic 值本身（自定义 panic 可能带上邮件数据），只有运行时错误（越界、空指针等）记其说明。
func logPanic(id, op string, r any) {
	kind := fmt.Sprintf("%T", r)
	if re, ok := r.(runtime.Error); ok {
		kind = re.Error()
	}
	pcs := make([]uintptr, 32)
	n := runtime.Callers(3, pcs) // 跳过 Callers、logPanic 和 defer 的函数
	frames := runtime.CallersFrames(pcs[:n])
	var where []string
	for len(where) < 8 {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "runtime.") {
			where = append(where, fmt.Sprintf("%s (%s:%d)", f.Function, filepath.Base(f.File), f.Line))
		}
		if !more {
			break
		}
	}
	if id == "" {
		id = "?"
	}
	log.Printf("本机邮件：来源 %s 在 %s 时 panic（%s），已转成工具错误。位置：%s", id, op, kind, strings.Join(where, " <- "))
}

func safeOpen(id string, info SourceInfo, opt OpenOptions) (s Source, err error) {
	err = guard(id, "open", func() (err error) {
		s, err = info.Open(opt)
		return err
	})
	if err != nil {
		s = nil
	}
	return s, err
}

func safeClose(id string, s Source) error {
	return guard(id, "close", s.Close)
}

// safeStale 在 Stale panic 时返回 true：重开一次。
func safeStale(id string, st staler) (stale bool) {
	if err := guard(id, "stale", func() error { stale = st.Stale(); return nil }); err != nil {
		return true
	}
	return stale
}

// fingerprint 取来源数据文件的签名；来源没有签名或签名函数 panic 时返回空串（不自动恢复）。
func fingerprint(info SourceInfo, opt OpenOptions) (fp string) {
	if info.Fingerprint == nil {
		return ""
	}
	if err := guard(info.ID, "fingerprint", func() error { fp = info.Fingerprint(opt); return nil }); err != nil {
		return ""
	}
	return fp
}

// statSig 是一组文件的大小和修改时间签名；不存在或读不到的文件也记一笔，出现了签名就会变。
func statSig(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", p, fi.Size(), fi.ModTime().UnixNano())
		} else {
			fmt.Fprintf(&b, "%s:-;", p)
		}
	}
	if b.Len() == 0 {
		return "-" // 非空：签名函数存在，只是暂时没有文件
	}
	return b.String()
}

// cause 取读取包给出的原因，放进「详情」。适配器用 errors.Join(本包哨兵, 原错误) 包装，
// 直接 %v 会得到哨兵文本加换行再加原因的两行；这里去掉本包的哨兵，多段用「；」连成一行。
func cause(err error) string {
	j, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return oneLine(err.Error())
	}
	var parts []string
	for _, e := range j.Unwrap() {
		if e == nil || e == ErrUnsupported || e == ErrNotFound || e == ErrAccess || e == ErrFilePermission || e == ErrLocked {
			continue
		}
		parts = append(parts, cause(e))
	}
	if len(parts) == 0 {
		return oneLine(err.Error())
	}
	return strings.Join(parts, "；")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", "；")), " ")
}

func explainID(id string, err error) error {
	info, ok := SourceByID(id)
	if !ok {
		return err
	}
	return Explain(info, err)
}

// isTCC 认出 macOS 隐私保护（TCC）的拦截：系统调用返回 EPERM（"operation not permitted"）。
// 普通的 Unix 权限不足返回 EACCES，由 Explain 另给说明。SQLite 在被拦时只报含糊的
// "unable to open database file"，那种情况由读取包先用 os.Open 探一次，拿到带 errno 的错误。
func isTCC(err error) bool {
	return errors.Is(err, syscall.EPERM) || strings.Contains(err.Error(), "operation not permitted")
}

// ClampLimit 把请求的条数限在 [1, max]，<=0 取 DefaultLimit（不超过 max）。
func ClampLimit(v, max int) int {
	if max <= 0 {
		max = DefaultMaxResults
	}
	if v <= 0 {
		v = DefaultLimit
	}
	if v > max {
		v = max
	}
	return v
}

// TruncateUTF8 按字节截断，不切断 UTF-8 字符。
func TruncateUTF8(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
