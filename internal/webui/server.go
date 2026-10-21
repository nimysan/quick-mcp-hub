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
	if err := s.mgr.Test(ctx, req.Type, req.Account, req.Values); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}
