package feishuweb

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// ErrNotConnected 表示扩展没连上来。
var ErrNotConnected error = &StatusErr{Status: StatusExtensionDisconnected, Msg: "浏览器扩展没有连上 mcp-hub：确认扩展已安装、已配对，并且浏览器开着"}

// Bridge 是配置页面进程里那一端：扩展经 WebSocket 连进来，MCP 进程经
// HTTP 把任务交给它，它转给扩展、等结果、再做拒读复核和转换。
//
// 同一时间只认一个扩展连接，新的顶掉旧的。
type Bridge struct {
	// Token 取当前配对 token。返回空串或错误表示还没配对，所有连接都会被拒。
	Token func() (string, error)
	// Limits 约束块树转换。
	Limits Limits

	mu      sync.Mutex
	conn    *websocket.Conn
	info    ConnInfo
	pending map[string]chan RawResult
	first   *SelfCheck
	last    *SelfCheck
	history []Item

	firstSearch *SearchCheck
	lastSearch  *SearchCheck
}

// ConnInfo 是扩展连上来时自报的信息，只作展示。
type ConnInfo struct {
	Version     string    `json:"version,omitempty"`
	InstallType string    `json:"installType,omitempty"`
	ConnectedAt time.Time `json:"connectedAt"`
}

// Status 是给配置页面看的状态。
type Status struct {
	Connected   bool       `json:"connected"`
	ExtensionID string     `json:"extensionId"`
	Conn        *ConnInfo  `json:"conn,omitempty"`
	FirstCheck  *SelfCheck `json:"firstCheck,omitempty"`
	LastCheck   *SelfCheck `json:"lastCheck,omitempty"`
	// 检索自检：第一次和最近一次。
	FirstSearch *SearchCheck `json:"firstSearch,omitempty"`
	LastSearch  *SearchCheck `json:"lastSearch,omitempty"`
}

// 消息格式。扩展 -> hub：hello / result / ping；hub -> 扩展：welcome / job / pong。
type wireMsg struct {
	Type        string     `json:"type"`
	Token       string     `json:"token,omitempty"`
	Version     string     `json:"version,omitempty"`
	InstallType string     `json:"installType,omitempty"`
	ID          string     `json:"id,omitempty"`
	Job         *Job       `json:"job,omitempty"`
	Result      *RawResult `json:"result,omitempty"`
}

// 一份块树最大 16 MB。整篇 500 块的公开文档序列化后约 200 KB，留足余量。
const maxMessage = 16 << 20

// LoopbackHost 报告请求的 Host 头是不是回环地址。
//
// 防 DNS rebinding：恶意网站把自己的域名解析到 127.0.0.1 后，浏览器会认为
// 页面和接口同源，Origin 校验拦不住；但 Host 头仍是那个域名。
func LoopbackHost(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// HandleWS 是扩展的连接入口（/ext/ws）。
func (b *Bridge) HandleWS(w http.ResponseWriter, r *http.Request) {
	if !LoopbackHost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// Origin 必须正好是我们自己的扩展。别的扩展、任何网页都进不来。
	if r.Header.Get("Origin") != ExtensionOrigin {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	// 上面已经精确比对过 Origin，所以这里关掉库自带的同源检查 —— 它按 Host 比，
	// 而扩展的 Origin 和 127.0.0.1 永远不同源。
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(maxMessage)
	ctx := r.Context()

	// 第一条必须是 hello，带配对 token。不放在 URL 里：URL 会进日志。
	helloCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var hello wireMsg
	err = readJSON(helloCtx, c, &hello)
	cancel()
	if err != nil || hello.Type != "hello" || !b.tokenOK(hello.Token) {
		c.Close(websocket.StatusPolicyViolation, "pairing required")
		return
	}
	if err := writeJSON(ctx, c, wireMsg{Type: "welcome"}); err != nil {
		c.CloseNow()
		return
	}
	b.attach(c, ConnInfo{Version: clip(hello.Version, 32), InstallType: clip(hello.InstallType, 32), ConnectedAt: time.Now()})
	log.Printf("飞书网页文档：浏览器扩展已连接（%s）", clip(hello.Version, 32))
	defer b.detach(c)

	for {
		var m wireMsg
		if err := readJSON(ctx, c, &m); err != nil {
			return
		}
		switch m.Type {
		case "ping":
			_ = writeJSON(ctx, c, wireMsg{Type: "pong"})
		case "result":
			if m.Result != nil {
				b.deliver(m.ID, *m.Result)
			}
		}
	}
}

func (b *Bridge) tokenOK(got string) bool {
	if b.Token == nil || got == "" {
		return false
	}
	want, err := b.Token()
	if err != nil || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (b *Bridge) attach(c *websocket.Conn, info ConnInfo) {
	b.mu.Lock()
	old := b.conn
	b.conn, b.info = c, info
	b.mu.Unlock()
	if old != nil {
		old.Close(websocket.StatusGoingAway, "replaced by a newer connection")
	}
}

func (b *Bridge) detach(c *websocket.Conn) {
	b.mu.Lock()
	if b.conn == c {
		b.conn = nil
	}
	b.mu.Unlock()
	c.CloseNow()
}

// Disconnect 断开当前扩展。重新配对时用：旧 token 作废，旧连接也不该留着。
func (b *Bridge) Disconnect() {
	b.mu.Lock()
	c := b.conn
	b.conn = nil
	b.mu.Unlock()
	if c != nil {
		c.Close(websocket.StatusPolicyViolation, "re-paired")
	}
}

func (b *Bridge) deliver(id string, res RawResult) {
	b.mu.Lock()
	ch, ok := b.pending[id]
	delete(b.pending, id)
	b.mu.Unlock()
	if ok {
		ch <- res // 缓冲 1，不会阻塞
	}
}

// Status 返回当前连接和自检状态。
func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := Status{Connected: b.conn != nil, ExtensionID: ExtensionID, FirstCheck: b.first, LastCheck: b.last,
		FirstSearch: b.firstSearch, LastSearch: b.lastSearch}
	if b.conn != nil {
		info := b.info
		st.Conn = &info
	}
	return st
}

// Connected 报告扩展是否连着。
func (b *Bridge) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn != nil
}

// Do 执行一次任务并返回处理好的结果。
func (b *Bridge) Do(ctx context.Context, kind, docURL string) (Result, error) {
	job := Job{Kind: kind}
	switch kind {
	case JobReadDoc:
		u, err := ValidateDocURL(docURL)
		if err != nil {
			return Result{}, err
		}
		job.URL = u
	case JobRecent:
	default:
		return Result{}, argErr("不认识的任务 %q", kind)
	}
	raw, err := b.send(ctx, job)
	if err != nil {
		return Result{}, err
	}
	switch kind {
	case JobReadDoc:
		return b.finishRead(job.URL, raw), nil
	default:
		return b.finishRecent(raw), nil
	}
}

func (b *Bridge) send(ctx context.Context, job Job) (RawResult, error) {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	job.ID = hex.EncodeToString(id)

	ch := make(chan RawResult, 1)
	b.mu.Lock()
	c := b.conn
	if c == nil {
		b.mu.Unlock()
		return RawResult{}, ErrNotConnected
	}
	if b.pending == nil {
		b.pending = map[string]chan RawResult{}
	}
	b.pending[job.ID] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, job.ID)
		b.mu.Unlock()
	}()

	if err := writeJSON(ctx, c, wireMsg{Type: "job", ID: job.ID, Job: &job}); err != nil {
		return RawResult{}, &StatusErr{Status: StatusExtensionDisconnected, Msg: "任务发不到扩展: " + err.Error()}
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return RawResult{}, fmt.Errorf("等扩展回结果超时（页面可能还在加载，或扩展被浏览器挂起了）")
	}
}

func (b *Bridge) finishRead(docURL string, raw RawResult) Result {
	if raw.Diag != nil {
		d := *raw.Diag
		if d.At.IsZero() {
			d.At = time.Now()
		}
		b.mu.Lock()
		if b.first == nil {
			first := d
			b.first = &first
		}
		b.last = &d
		b.mu.Unlock()
	}
	res := Result{Status: raw.Status, URL: docURL, Title: CleanTitle(raw.Title), Flags: raw.Flags}
	switch raw.Status {
	case StatusOK:
		// 扩展已经判过一次；这里再判，扩展出错时正文也不会漏出去。
		if reason := Gate(raw.Flags); reason != "" {
			if raw.Root != nil {
				log.Printf("飞书网页文档：扩展在拒读条件下带回了正文（%s），已丢弃", reason)
			}
			res.Status, res.Reason, res.Message = StatusRefused, reason, ReasonText(reason)
			return res
		}
		conv := Convert(raw.Root, b.Limits)
		res.Markdown, res.Truncated = conv.Markdown, conv.Truncated
		res.Stats = &conv.Counts
		res.Notice = UntrustedNotice
		_, typ, _ := ResultURL(docURL)
		b.remember(Item{Title: res.Title, URL: docURL, Type: typ, Source: "read_history"})
	case StatusRefused:
		reason := raw.Reason
		if reason == "" {
			reason = ReasonPermissionUnknown
		}
		res.Reason, res.Message = reason, ReasonText(reason)
	case StatusUnsupportedType:
		res.Message = "这个页面不是云文档或知识库正文（幻灯片、电子表格、多维表格暂不支持）"
	case StatusNotFound:
		res.Message = "文档不存在、已被删除，或链接写错了" + detail(raw.Error)
	case StatusNoPermission:
		res.Message = "当前在浏览器里登录的飞书账号没有权限打开这篇文档"
	case StatusLoginRequired:
		res.Message = "浏览器里的飞书没有登录（文档页跳到了登录页）：请先在浏览器里登录飞书"
	case StatusEngineMissing, StatusStructureUnknown:
		res.Status = StatusStructureUnknown
		res.Message = "页面上找不到飞书文档引擎（PageMain）：飞书前端可能改版了，请升级 mcp-hub 和扩展"
	case StatusInvalidArgument:
		res.Message = clip(firstNonEmpty(raw.Error, "地址不对"), 300)
	default:
		res.Status = StatusError
		res.Message = clip(firstNonEmpty(raw.Error, raw.Reason, "扩展没有说明原因"), 300)
	}
	return res
}

// detail 把扩展给的补充说明（只有页面地址，不含内容）挂在消息后面。
func detail(s string) string {
	if s = clip(s, 120); s != "" {
		return "（" + s + "）"
	}
	return ""
}

func (b *Bridge) remember(it Item) {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := []Item{it}
	for _, h := range b.history {
		if h.URL != it.URL && len(kept) < 50 {
			kept = append(kept, h)
		}
	}
	b.history = kept
}

func (b *Bridge) finishRecent(raw RawResult) Result {
	// items 总是在：空结果也是 []，不省略字段。
	res := Result{Status: raw.Status, Items: []Item{}}
	if raw.Status != StatusOK {
		res.Status = StatusError
		res.Message = clip(firstNonEmpty(raw.Error, "扩展没有说明原因"), 300)
		return res
	}
	idx := map[string]int{}
	add := func(it Item) {
		u, typ, ok := ResultURL(it.URL)
		if !ok {
			return
		}
		// 同一篇文档既开着又读过：合成一条，标上 readBefore（用户报告 recent 只见 open_tab）。
		if i, seen := idx[u]; seen {
			if it.Source == "read_history" {
				res.Items[i].ReadBefore = true
			}
			if res.Items[i].Title == "" {
				res.Items[i].Title = CleanTitle(it.Title)
			}
			return
		}
		idx[u] = len(res.Items)
		res.Items = append(res.Items, Item{Title: CleanTitle(it.Title), URL: u, Type: typ, Source: it.Source})
	}
	for _, it := range raw.Items {
		if it.Source != "open_tab" {
			it.Source = "read_history"
		}
		add(it)
	}
	b.mu.Lock()
	hist := append([]Item(nil), b.history...)
	b.mu.Unlock()
	for _, it := range hist {
		it.Source = "read_history"
		add(it)
	}
	if len(res.Items) > 50 {
		res.Items = res.Items[:50]
	}
	return res
}

func readJSON(ctx context.Context, c *websocket.Conn, v any) error {
	_, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, data)
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// NewPairingToken 生成一个新的配对 token（给用户复制进扩展）。
func NewPairingToken() string {
	buf := make([]byte, 18)
	_, _ = rand.Read(buf)
	return "fw-" + hex.EncodeToString(buf)
}
