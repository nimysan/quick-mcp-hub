// Package webui 是渠道配置页面：一个只绑 127.0.0.1 的本机 HTTP 服务。
//
// 它和 MCP stdio 服务是**两个进程**：MCP server 由 Quick 这类客户端 spawn，
// 关掉客户端就没了；配置页面需要独立存在，用户随时打开改配置。两者通过
// 磁盘上的 channels.json + 系统密码管理器交换状态，MCP server 靠 channels.json
// 的 mtime 感知变化。
//
// 只绑环回地址：页面能读写凭据和配置，绝不可对局域网暴露。
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nimysan/mcp-hub/internal/applog"
	"github.com/nimysan/mcp-hub/internal/assets"
	"github.com/nimysan/mcp-hub/internal/channels"
	"github.com/nimysan/mcp-hub/internal/clientstat"
	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/mailprovider"
	"github.com/nimysan/mcp-hub/internal/webui/design"
)

//go:embed index.html
var assetsFS embed.FS

// Server 承载渠道配置页面。
type Server struct {
	mgr     *channels.Manager
	mux     *http.ServeMux
	version string

	// LogPath 是本次运行的日志文件，页面和托盘的「查看日志」要用它。
	// 空表示这次没写日志文件，那两个入口就不显示——一个点了没反应的
	// 菜单比没有菜单更糟。
	LogPath string

	// OnReady 在监听端口绑定成功、开始服务之前调用一次，参数是页面地址。
	// 用来自动把窗口打开 —— 放在绑定之后才调，是为了不让用户看到一个指向
	// 还没起来的服务的窗口（那会显示「无法连接」）。
	OnReady func(url string)
}

func New(mgr *channels.Manager, version string) *Server {
	s := &Server{mgr: mgr, version: version, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/favicon.ico", s.handleFavicon)
	// healthz 刻意不过同站闸：它是**单实例探测**用的，由第二次双击起来的那个
	// 进程发起（非浏览器，不带那两个头，本来就会被放过），而且只回版本号。
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	// 每个 /api 端点都过一道同站闸，见 sameOrigin。
	api := func(path string, h http.HandlerFunc) { s.mux.HandleFunc(path, guard(h)) }
	api("/api/descriptors", s.handleDescriptors)
	api("/api/channels", s.handleChannels) // GET 列表 / POST 保存
	api("/api/channels/test", s.handleTest)
	api("/api/channels/check", s.handleCheck)  // 分步检查，见设计稿的「连接检查」
	api("/api/mail/guess", s.handleGuessHosts) // 按邮箱域名猜 IMAP/SMTP 主机
	api("/api/channels/delete", s.handleDelete)
	api("/api/log", s.handleLog)
	// OAuth 设备流：开始授权 / 查状态 / 放弃。轮询由服务端做（见 connect.go），
	// 这里只是把状态端出来。
	api("/api/oauth/start", s.handleOAuthStart)
	api("/api/oauth/status", s.handleOAuthStatus)
	api("/api/oauth/cancel", s.handleOAuthCancel)
	// NapCat 自动配置（见 napcat.go）。
	s.napcatRoutes(api)
}

// healthApp 是 /healthz 回的标记。端口被别的程序占着时，探测要能区分
// 「另一份 mcp-hub」和「某个无关服务」——后者不该被当成自己人。
const healthApp = "mcp-hub"

// handleFavicon 回程序图标。
//
// 没有它，Chromium 的 --app 窗口在标题栏和任务栏里显示的是一个**地球**——
// 对「看起来像个正规软件」这件事，这是最便宜也最显眼的一处。
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(assets.Icon)
}

// handleHealthz 让另一个进程认出「这个端口上跑的是 mcp-hub」。
//
// 用途是单实例：用户第二次双击主程序时，旧行为是绑端口失败——他看到的是
// 一句 address already in use，而在没有控制台的 Windows 上是**什么都看不到**。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"app": healthApp, "version": s.version})
}

// handleLog 回日志文件的尾部。
//
// Windows 上的 exe 用 -H windowsgui 编译，没有控制台，所以日志只在文件里。
// 这个端点让「查看日志」在页面和托盘菜单里都能用，不必让用户去找路径。
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if s.LogPath == "" {
		http.Error(w, "这次运行没有写日志文件", http.StatusNotFound)
		return
	}
	text, err := applog.Tail(s.LogPath, 256<<10)
	if err != nil {
		http.Error(w, "读日志失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(text))
}

// ProbeRunning 探测这个端口上是否已经有一份 mcp-hub 在跑，返回它的页面地址。
//
// 超时给得很短：这是启动路径上的一步，用户正在等窗口弹出来。
func ProbeRunning(port int) (string, bool) {
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	c := &http.Client{Timeout: 700 * time.Millisecond}
	resp, err := c.Get(url + "/healthz")
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var out struct{ App string }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<10)).Decode(&out); err != nil {
		return "", false
	}
	// 占着端口的是别的程序时不认——否则会把用户送到一个毫无关系的页面。
	return url, out.App == healthApp
}

// guard 挡住跨站请求。
//
// 这个页面是一个**没有登录**的本机 HTTP 服务：用户在浏览器里打开的任何一个
// 网页都能向 127.0.0.1:7801 发 POST。原先的端点最多改渠道配置，现在还能写
// NapCat 的配置文件、启动一个进程 —— 没有这道闸，一个恶意网页就能借它在
// 用户机器上运行程序。
//
// 浏览器对跨站 fetch 一定带 Origin，对同站 fetch 带 Sec-Fetch-Site: same-origin，
// 而网页脚本**无法伪造**这两个头。两个头都没有时放过：那是 curl 这类非浏览器
// 调用，而同机进程本来就能直接执行代码，不需要绕我们。
func guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeErr(w, http.StatusForbidden, fmt.Errorf("跨站请求已拒绝"))
			return
		}
		h(w, r)
	}
}

func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
		return false
	}
	return true
}

// decodeJSON 读 POST 的 JSON 请求体，顺带把方法检查收在一处。
// 返回 false 表示已经回过错误，调用方直接 return。
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

// Listen 在 127.0.0.1 上监听。port 为 0 时由系统分配，返回实际地址。
func (s *Server) Listen(ctx context.Context, port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("监听 127.0.0.1:%d 失败: %w", port, err)
	}
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	log.Printf("渠道配置页面：http://%s", ln.Addr())
	fmt.Printf("在浏览器打开：http://%s\n", ln.Addr())
	if s.OnReady != nil {
		// 端口已经绑上了，所以这个窗口一打开就能连上。
		s.OnReady(fmt.Sprintf("http://%s", ln.Addr()))
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := assetsFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 设计 token、样式和图标都在构建时注入，而不是另外几个 HTTP 请求：
	// 页面是单文件，少几次往返也就少几种半加载状态（没有 CSS 的一瞬间闪白）。
	// 三样的来源都是 Figma，见 internal/webui/design。
	tok := design.Load()
	page := strings.NewReplacer(
		"/*TOKENS*/", tok.CSSVars(),
		"/*STYLES*/", tok.Stylesheet(),
		"<!--SPRITE-->", tok.IconSprite(),
	).Replace(string(data))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 页面是编译进二进制的，升级程序后浏览器必须拿到新的那份：没有这个头
	// 浏览器会按启发式缓存，用户换了新版本却还在用旧页面。
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(page))
}

// exePath 返回本程序的绝对路径，给页面上的「接入 AI 客户端」用。
//
// 必须是运行时探测而不是写死安装路径：用户可能跑的是解压出来的绿色版
// （放在下载目录里），也可能跑的是安装版。给错路径的后果是客户端报
// 「not found on PATH」，而他完全看不出哪里错了 —— 这件事真实发生过。
//
// EvalSymlinks 是为了 macOS 上通过符号链接启动的情形：客户端 spawn 的是
// 这个路径，所以要给出能真正执行到的那一个。
func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return p
}

func (s *Server) handleDescriptors(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    s.version,
		"configPath": s.mgr.Store().Path(),
		"logPath":    s.LogPath,
		"exePath":    exePath(),
		// 凭据后端的真实名字（macOS 钥匙串 / Windows 凭据管理器 / Linux Secret
		// Service）。删除确认弹窗要把「凭据存在哪」说清楚，用真名而不是泛指。
		"secretBackend": credstore.BackendName(),
		// 最近一次成功握手的客户端。页面据此决定接入说明默认展开还是收起 ——
		// 用户照着说明在 Quick 里加完连接器之后，否则完全没有反馈。
		"lastClient":  lastClient(),
		"descriptors": channels.All(),
	})
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		views, err := s.mgr.List()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"channels": views})
	case http.MethodPost:
		var req struct {
			Type   string            `json:"type"`
			Values map[string]string `json:"values"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		in, err := s.mgr.Save(req.Type, req.Values)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"saved": in})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Type    string            `json:"type"`
		Account string            `json:"account"`
		Values  map[string]string `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	identity, err := s.mgr.Identify(ctx, req.Type, req.Account, req.Values)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// identity 是对方系统报回来的身份（如 GitHub 的 login），让用户看到
	// 「以谁的身份连上了」。邮件渠道没有这个概念，返回 null。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "identity": identity})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Type    string `json:"type"`
		Account string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.mgr.Delete(req.Type, req.Account); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleOAuthStart 开始一次设备流授权。只回给用户看的码和地址 ——
// 设备码不下发到前端，它等价于一次授权的凭据。
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Type   string            `json:"type"`
		Values map[string]string `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sess, err := s.mgr.StartConnect(req.Type, req.Values)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": sess})
}

func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("session")
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少 session 参数"))
		return
	}
	st, err := s.mgr.ConnectStatus(id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"state": "error", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleOAuthCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.mgr.CancelConnect(req.Session)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

// lastClient 读「最近一次有客户端真的连上来」的记录。
//
// 没有记录时回 nil，页面据此把接入说明默认展开 —— 那时它是页面上唯一重要的
// 东西。记录里的客户端名字是对方自报的，只作展示，不作任何授权判断。
func lastClient() any {
	r, ok := clientstat.Read(config.Namespace)
	if !ok {
		return nil
	}
	return map[string]any{
		"at":      r.At.Format(time.RFC3339),
		"client":  r.Client,
		"version": r.Version,
		"tools":   r.Tools,
	}
}

// handleCheck 分步检查一个渠道：服务可达性、凭据、另一个服务可达性。
//
// 和 handleTest 的区别是粒度。合成一句的话，失败时用户只知道「连不上」——
// 分不清该找 IT、重新生成授权码，还是只是发信那一侧坏了。
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type    string            `json:"type"`
		Account string            `json:"account"`
		Values  map[string]string `json:"values"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// 三步串着做，每步自己还有超时，所以总预算要给够。
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	steps, err := s.mgr.CheckSteps(ctx, req.Type, req.Account, req.Values)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ok := true
	for _, st := range steps {
		if st.State == "fail" {
			ok = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "steps": steps})
}

// handleGuessHosts 按邮箱地址猜 IMAP / SMTP 主机，供表单自动填写。
//
// 「我公司邮箱的 IMAP 主机叫什么」是财务人员答不出的问题，而大多数自建服务器
// 就是 mail.<域名>。猜错的代价很小（同一个表单里改掉），猜对的收益是不用去
// 问 IT。纯计算，不连网络、不碰凭据。
func (s *Server) handleGuessHosts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address string `json:"address"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	imapHost, smtpHost := mailprovider.GuessHosts(req.Address)
	writeJSON(w, http.StatusOK, map[string]any{
		"imapHost": imapHost, "smtpHost": smtpHost,
	})
}
