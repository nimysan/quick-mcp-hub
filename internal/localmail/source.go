// Package localmail 把本机各邮件客户端的只读读取包统一成一个小接口，
// 供 MCP 工具层按 source 参数分派。
//
// 只读是这一层的硬约束：接口里没有任何写方法，适配器只调用各读取包的
// 查询函数；各读取包自己以 SQLite mode=ro + query_only 打开数据库、
// 只用 os.Open 读文件，绝不写邮件客户端的任何文件。
//
// 新增一个来源（例如新版 Outlook 的 hxstore）只需要：
//  1. 写一个适配器文件，实现 Source；
//  2. 在 Sources 里加一条 SourceInfo。
//
// 配置页的开关、工具描述里的来源清单都从 Sources 生成，不需要再改别处。
package localmail

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// 来源标识，即工具参数 source 的取值，也是配置页开关的字段名。
const (
	SourceAppleMail     = "apple_mail"
	SourceOutlookLegacy = "outlook_legacy"
	SourceOutlook       = "outlook" // 新版 Outlook（HxStore），读取包在 internal/localmail/hxstore
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

// ErrUnsupported 表示来源的存储格式本构建不认识（例如邮件客户端升级后格式变了）。
// 这种错误重试没有用，Hub 会停用该来源，直到服务重启。
var ErrUnsupported = errors.New("存储格式不支持")

// ErrNotFound 表示数据目录、文件夹或邮件不存在。
var ErrNotFound = errors.New("未找到")

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
	Open              func(opt OpenOptions) (Source, error)
}

// Sources 是本构建认识的全部来源，按配置页上的显示顺序。
var Sources = []SourceInfo{appleMailInfo, outlookLegacyInfo, outlookInfo}

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
type Hub struct {
	cfg  Config
	mu   sync.Mutex
	open map[string]Source
	dead map[string]error // 格式不支持而停用的来源，不再重试
}

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
	return &Hub{cfg: cfg, open: map[string]Source{}, dead: map[string]error{}}
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

// Source 返回一个已打开的来源，第一次调用时打开。
func (h *Hub) Source(id string) (Source, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.open[id]; ok {
		return s, nil
	}
	if err, ok := h.dead[id]; ok {
		return nil, err
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
	s, err := info.Open(OpenOptions{Dir: sc.Dir, MaxBodyBytes: h.cfg.MaxBodyBytes})
	if err != nil {
		err = Explain(info, err)
		if errors.Is(err, ErrUnsupported) {
			h.dead[id] = err
		}
		return nil, err
	}
	h.open[id] = s
	return s, nil
}

// Search 在一个来源里列邮件，限制条数并截短摘要。
func (h *Hub) Search(ctx context.Context, id string, q Query) (Page, error) {
	s, err := h.Source(id)
	if err != nil {
		return Page{}, err
	}
	q.Limit = ClampLimit(q.Limit, h.cfg.MaxResults)
	if q.Offset < 0 {
		q.Offset = 0
	}
	p, err := s.Search(ctx, q)
	if err != nil {
		return Page{}, h.fail(id, err)
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
	s, err := h.Source(id)
	if err != nil {
		return nil, err
	}
	m, err := s.Get(ctx, msgID)
	if err != nil {
		return nil, h.fail(id, err)
	}
	if b, cut := TruncateUTF8(m.Body, h.cfg.MaxBodyBytes); cut {
		m.Body, m.BodyTruncated = b, true
	}
	return m, nil
}

// Folders 列一个来源的文件夹。
func (h *Hub) Folders(ctx context.Context, id string) ([]Folder, error) {
	s, err := h.Source(id)
	if err != nil {
		return nil, err
	}
	f, err := s.Folders(ctx)
	if err != nil {
		return nil, h.fail(id, err)
	}
	if f == nil {
		f = []Folder{}
	}
	return f, nil
}

// Close 关闭已打开的来源。
func (h *Hub) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var first error
	for id, s := range h.open {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
		delete(h.open, id)
	}
	return first
}

// Explain 把读取包的错误变成用户能据以行动的话。权限错误给出授权步骤，
// 而不是一句 operation not permitted。
func Explain(info SourceInfo, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrAccess) || isPermission(err) {
		return fmt.Errorf("%w：%s 的数据被 macOS 隐私保护拦下。"+
			"请打开「系统设置 → 隐私与安全性 → 完全磁盘访问权限」，"+
			"把启动 mcp-hub 的程序（在 Amazon Quick 里用就是 Amazon Quick）加进去并打开开关，"+
			"然后完全退出 Amazon Quick 再重新打开。本程序不会尝试绕过这项保护。", ErrAccess, info.Name)
	}
	if errors.Is(err, ErrUnsupported) {
		return fmt.Errorf("%w：%s 的本地存储格式这个版本的 mcp-hub 读不了（多半是 %s 升级了）。"+
			"这个来源已停用，其他来源不受影响；请等 mcp-hub 更新。详情：%v", ErrUnsupported, info.Name, info.Name, err)
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w：%s %v", ErrNotFound, info.Name, err)
	}
	return fmt.Errorf("%s: %w", info.Name, err)
}

// fail 解释一次调用的错误；格式不支持时关闭并停用这个来源。
func (h *Hub) fail(id string, err error) error {
	err = explainID(id, err)
	if errors.Is(err, ErrUnsupported) {
		h.mu.Lock()
		if s, ok := h.open[id]; ok {
			s.Close()
			delete(h.open, id)
		}
		h.dead[id] = err
		h.mu.Unlock()
	}
	return err
}

func explainID(id string, err error) error {
	info, ok := SourceByID(id)
	if !ok {
		return err
	}
	return Explain(info, err)
}

// isPermission 认出 TCC 拦截。SQLite 在被拦时只报含糊的
// "unable to open database file"，那种情况由读取包先用 os.Open 探一次转成 fs.ErrPermission。
func isPermission(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	return strings.Contains(err.Error(), "operation not permitted")
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
