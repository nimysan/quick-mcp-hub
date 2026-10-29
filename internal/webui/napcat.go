package webui

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/nimysan/mcp-hub/internal/napcat"
)

// NapCat 自动配置的三个端点。
//
// 都重新做一次 Detect，而不是信前端传回来的路径：configure 要写文件、start 要
// 起进程，这两件事的目标必须由服务端自己从「一个看得出是 NapCat 安装的目录」
// 推出来，不能由请求直接指定。
func (s *Server) napcatRoutes(api func(string, http.HandlerFunc)) {
	api("/api/napcat/detect", s.handleNapcatDetect)
	api("/api/napcat/configure", s.handleNapcatConfigure)
	api("/api/napcat/start", s.handleNapcatStart)
}

type napcatReq struct {
	Dir  string `json:"dir"`
	QQ   string `json:"qq"`
	Port int    `json:"port"`
}

func (s *Server) handleNapcatDetect(w http.ResponseWriter, r *http.Request) {
	var req napcatReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// 目录留空时在这个系统的常见位置里找。mac 版的配置被 QQ 的沙箱容器
	// 固定在一个七十来字符的路径上，让用户手打它是荒谬的。
	in, err := detect(req.Dir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	resp := map[string]any{"ok": true, "install": in}
	// 启动命令原样端出去给用户看。两个发行包的启动器名字和传 QQ 号的方式
	// 不一样，而这件事我在 Windows 上没法实测 —— 让他看见我们打算跑什么，
	// 比让他相信我们猜对了好。按**本机**系统算，不是按 Windows 写死。
	if len(in.Accounts) > 0 {
		if name, args, err := napcat.StartCommand(runtime.GOOS, in, in.Accounts[0].QQ); err == nil {
			resp["command"] = name + " " + joinArgs(args)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleNapcatConfigure(w http.ResponseWriter, r *http.Request) {
	var req napcatReq
	if !decodeJSON(w, r, &req) {
		return
	}
	in, err := detect(req.Dir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	res, err := napcat.Configure(in, req.QQ, req.Port)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res, "hasLauncher": in.Launcher != ""})
}

func (s *Server) handleNapcatStart(w http.ResponseWriter, r *http.Request) {
	var req napcatReq
	if !decodeJSON(w, r, &req) {
		return
	}
	in, err := detect(req.Dir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cmdline, started, err := napcat.Start(in, req.QQ)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "command": cmdline})
		return
	}
	a, _ := in.Account(req.QQ)
	port := a.Port
	if port <= 0 {
		port = napcat.DefaultPort
	}
	// 起来需要几秒（要加载 QQ 客户端那一套）。等到端口有人答为止，否则
	// 用户会在「已启动」之后立刻去点测试，然后看到一次连接失败。
	up := napcat.WaitUntilUp(port, 40*time.Second)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "started": started, "answering": up,
		"command": cmdline, "endpoint": fmt.Sprintf("http://127.0.0.1:%d", port),
	})
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// detect 是三个端点共用的入口：给了目录就用它，没给就在这个系统的常见位置里找。
//
// configure 和 start 也走这里，所以「自动找到的那个目录」接着就能用，用户不必
// 先把路径抄回输入框。
func detect(dir string) (*napcat.Install, error) {
	if strings.TrimSpace(dir) == "" {
		return napcat.DetectAuto()
	}
	return napcat.Detect(dir)
}
