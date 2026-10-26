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
	"log"
	"net"
	"net/http"
	"time"

	"github.com/nimysan/mcp-hub/internal/channels"
)

//go:embed index.html
var assets embed.FS

// Server 承载渠道配置页面。
type Server struct {
	mgr     *channels.Manager
	mux     *http.ServeMux
	version string

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
	s.mux.HandleFunc("/api/descriptors", s.handleDescriptors)
	s.mux.HandleFunc("/api/channels", s.handleChannels) // GET 列表 / POST 保存
	s.mux.HandleFunc("/api/channels/test", s.handleTest)
	s.mux.HandleFunc("/api/channels/delete", s.handleDelete)
	// OAuth 设备流：开始授权 / 查状态 / 放弃。轮询由服务端做（见 connect.go），
	// 这里只是把状态端出来。
	s.mux.HandleFunc("/api/oauth/start", s.handleOAuthStart)
	s.mux.HandleFunc("/api/oauth/status", s.handleOAuthStatus)
	s.mux.HandleFunc("/api/oauth/cancel", s.handleOAuthCancel)
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
	data, err := assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 页面是编译进二进制的，升级程序后浏览器必须拿到新的那份：没有这个头
	// 浏览器会按启发式缓存，用户换了新版本却还在用旧页面。
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func (s *Server) handleDescriptors(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     s.version,
		"configPath":  s.mgr.Store().Path(),
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
