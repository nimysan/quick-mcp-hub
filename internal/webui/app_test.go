package webui

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 没有 favicon 时 Chromium 的 --app 窗口在标题栏和任务栏里显示一个地球。
// 这是「看起来像个正规软件」上最便宜的一处，也最容易在改动中丢掉。
func TestFaviconIsServedAndDeclared(t *testing.T) {
	s := New(nil, "test")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/favicon.ico 状态 %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/x-icon" {
		t.Errorf("Content-Type = %q", got)
	}
	body := rec.Body.Bytes()
	if len(body) < 6 || body[2] != 1 {
		t.Fatalf("回的不是 .ico：% x", body[:min(6, len(body))])
	}

	// 光有路由不够——页面必须声明它，否则浏览器仍然只会去猜。
	html, err := assetsFS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `rel="icon"`) {
		t.Error("页面里没有 rel=\"icon\" 声明，--app 窗口还是会显示地球")
	}
}

// 单实例探测：第二次双击主程序时靠它认出「已经有一份在跑」。
func TestProbeRunningRecognisesOurOwnServer(t *testing.T) {
	s := New(nil, "test")
	port := freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Listen(ctx, port)
	// 等它绑上
	deadline := time.Now().Add(3 * time.Second)
	var ok bool
	var url string
	for time.Now().Before(deadline) {
		if url, ok = ProbeRunning(port); ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ok {
		t.Fatal("探测认不出自己的服务")
	}
	if url == "" || !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("url = %q", url)
	}
}

// 端口上是别的程序时**不能**认成自己：否则会把用户送到一个毫无关系的页面。
func TestProbeRunningRejectsStranger(t *testing.T) {
	stranger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"app":"something-else","version":"9"}`))
	}))
	defer stranger.Close()
	u, err := url.Parse(stranger.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	if _, ok := ProbeRunning(port); ok {
		t.Error("别的程序占着这个端口，不该被认成 mcp-hub")
	}
}

// 没人监听时要干脆地返回 false，而不是卡住——这是启动路径上的一步。
func TestProbeRunningOnDeadPort(t *testing.T) {
	port := freePort(t)
	start := time.Now()
	if _, ok := ProbeRunning(port); ok {
		t.Error("没人监听却报成有")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("探测耗了 %v，启动路径上太久了", d)
	}
}

// Windows 上没有控制台，日志只在文件里，所以这个端点是用户唯一能看到它的地方。
func TestLogEndpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp-hub.log")
	if err := os.WriteFile(path, []byte("第一行\n第二行\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(nil, "test")
	s.LogPath = path

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/log", nil)
	r.Host = "127.0.0.1:7801"
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	s.mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "第二行") {
		t.Errorf("日志内容不对: %q", rec.Body.String())
	}

	// 没有日志文件时回 404，页面据此把入口藏起来。
	s2 := New(nil, "test")
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/api/log", nil)
	r2.Host = "127.0.0.1:7801"
	r2.Header.Set("Sec-Fetch-Site", "same-origin")
	s2.mux.ServeHTTP(rec2, r2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("没有日志文件时状态 %d，想要 404", rec2.Code)
	}
}

// healthz 刻意不过同站闸：探测由另一个进程发起，它不是浏览器。
func TestHealthzIsReachableWithoutBrowserHeaders(t *testing.T) {
	s := New(nil, "v1.2.3")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 %d", rec.Code)
	}
	var out struct{ App, Version string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.App != "mcp-hub" || out.Version != "v1.2.3" {
		t.Errorf("healthz = %+v", out)
	}
}

// freePort 拿一个当下没人用的回环端口。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// 接入说明里的路径必须是**运行时探测**的真实路径，不能是写死的安装目录。
//
// 这条用例的由来：客户装的是解压出来的绿色版（在下载目录里），而给他的是
// 安装版的路径，于是客户端报 `not found on PATH`，他完全看不出哪里错了。
func TestExePathIsResolvedAtRuntime(t *testing.T) {
	got := exePath()
	if got == "" {
		t.Fatal("探测不到自己的路径")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("不是绝对路径: %s", got)
	}
	// 必须真的存在 —— 给一个不存在的路径等于给错。
	if _, err := os.Stat(got); err != nil {
		t.Errorf("路径不存在: %v", err)
	}
	// 不能是写死的安装目录
	for _, hard := range []string{`\Programs\mcp-hub\`, "/Applications/"} {
		if strings.Contains(got, hard) && !strings.Contains(os.Args[0], hard) {
			t.Errorf("看起来是写死的安装路径: %s", got)
		}
	}
}

// 页面要拿得到这个路径，否则那一节只能显示一段没有 command 的 JSON。
func TestDescriptorsCarryExePath(t *testing.T) {
	html, err := assetsFS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"renderOnboard", "d.exePath", "Add Local Connector", `id="obcopy"`} {
		if !strings.Contains(string(html), needle) {
			t.Errorf("页面里缺 %q —— 接入说明那一节不完整", needle)
		}
	}
}
