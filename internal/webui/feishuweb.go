package webui

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/feishuweb"
)

// 飞书网页文档渠道在配置页面这一侧的端点。
//
//	/ext/ws                    浏览器扩展连进来（Origin 固定扩展 ID + 配对 token，见 feishuweb.Bridge）
//	/api/feishu-web/job        MCP 进程交任务（同站闸 + Host 必须是回环）
//	/api/feishu-web/status     页面看连接和首次运行自检
//	/api/feishu-web/pair       生成新的配对码（旧的作废，已连着的扩展被断开）
//	/api/feishu-web/extract    把内嵌的扩展释放到本机目录
func (s *Server) feishuWebRoutes(api func(string, http.HandlerFunc)) {
	s.bridge = &feishuweb.Bridge{Token: func() (string, error) {
		if s.mgr == nil {
			return "", nil
		}
		return s.mgr.PairingToken()
	}}
	if s.mgr != nil {
		s.mgr.BrowserCheck = s.checkBrowser
	}
	// 扩展的 Origin 是 chrome-extension://…，同站闸一定会拒它，所以不过 guard；
	// 它自己精确比对 Origin 和 Host，再要配对 token。
	s.mux.HandleFunc("/ext/ws", s.bridge.HandleWS)
	api("/api/feishu-web/job", loopbackOnly(s.handleFeishuJob))
	api("/api/feishu-web/status", s.handleFeishuStatus)
	api("/api/feishu-web/pair", s.handleFeishuPair)
	api("/api/feishu-web/extract", s.handleFeishuExtract)
}

// checkBrowser 是飞书网页文档渠道的「测试」：扩展连着就算通。
func (s *Server) checkBrowser(context.Context) (any, error) {
	st := s.bridge.Status()
	if !st.Connected {
		return nil, feishuweb.ErrNotConnected
	}
	return "浏览器扩展 " + st.Conn.Version, nil
}

// loopbackOnly 在同站闸之外再要求 Host 是回环地址。这个端点回的是文档正文，
// 要挡住 DNS rebinding：那种情况下 Origin 和 Host 是同一个恶意域名，同站闸会放过。
func loopbackOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !feishuweb.LoopbackHost(r) {
			writeErr(w, http.StatusForbidden, fmt.Errorf("只接受 127.0.0.1 上的请求"))
			return
		}
		h(w, r)
	}
}

func (s *Server) handleFeishuJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"`
		URL  string `json:"url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	res, err := s.bridge.Do(ctx, req.Kind, req.URL)
	if err != nil {
		writeErr(w, http.StatusOK, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleFeishuStatus(w http.ResponseWriter, r *http.Request) {
	tok, _ := s.mgr.PairingToken()
	dir, _ := feishuweb.ExtensionDir(config.Namespace)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       s.bridge.Status(),
		"paired":       tok != "",
		"extensionDir": dir,
	})
}

func (s *Server) handleFeishuPair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok, err := s.mgr.NewPairing()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.bridge.Disconnect()
	// 配对码只在这一次响应里出现，页面上显示给用户复制；之后只存在密码管理器里。
	writeJSON(w, http.StatusOK, map[string]any{"code": tok})
}

func (s *Server) handleFeishuExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 目录由服务端定，不由请求指定：这个端点写文件。
	dir, err := feishuweb.ExtensionDir(config.Namespace)
	if err == nil {
		err = feishuweb.ExtractExtension(dir)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "extensionId": feishuweb.ExtensionID})
}
