package webui

import (
	"context"
	"encoding/json"
	"github.com/nimysan/mcp-hub/internal/webui/design"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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

// TestNoShadowedBrowserGlobals 防一类查不出来的坑。
//
// 真实故障：页面里有个 close() 函数，重写时漏搬了，于是 onclick = close 静默
// 落到 window.close —— 浏览器对非脚本打开的页面直接忽略它。按钮毫无反应，
// 而 node --check 只验语法，这种错它一声不响。
//
// 所以这里不验「close 有没有定义」，而是禁止**用浏览器全局名当函数名**：
// 那样一旦定义丢了，错误会是「未定义」而不是静默调用另一个东西。
func TestNoShadowedBrowserGlobals(t *testing.T) {
	page := readIndex(t)
	// window 上这些名字存在且可调用/可读，被遮蔽时失败是静默的。
	dangerous := []string{
		"close", "open", "print", "stop", "focus", "blur", "alert", "confirm",
		"prompt", "name", "status", "length", "origin", "history", "location",
	}
	for _, n := range dangerous {
		for _, form := range []string{
			"function " + n + "(",
			"const " + n + " =",
			"let " + n + " =",
			"var " + n + " =",
		} {
			if strings.Contains(page, form) {
				t.Errorf("不要用浏览器全局名 %q 作为函数/变量名（出现 %q）："+
					"定义一旦丢失会静默落到 window.%s，语法检查查不出来", n, form, n)
			}
		}
	}
}

// TestHandlersAreDefined 检查页面里挂上去的处理函数都真有定义。
//
// 同一个故障的另一半：onclick = someFn 里的 someFn 要是没定义，浏览器里是
// 运行时报错（或者更糟，撞上同名全局），静态检查看不到。
func TestHandlersAreDefined(t *testing.T) {
	page := readIndex(t)
	// 只看 `.onclick = 标识符;` 这种直接赋值（箭头函数不用查，它就在眼前）。
	re := regexp.MustCompile(`\.onclick\s*=\s*([A-Za-z_$][\w$]*)\s*;`)
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		fn := m[1]
		defined := strings.Contains(page, "function "+fn+"(") ||
			strings.Contains(page, "async function "+fn+"(") ||
			strings.Contains(page, "const "+fn+" =") ||
			strings.Contains(page, "let "+fn+" =")
		if !defined {
			t.Errorf("onclick 指向 %q，但页面里找不到它的定义 —— "+
				"浏览器里这会是运行时报错，或者更糟：撞上一个同名的 window 全局", fn)
		}
	}
}

func readIndex(t *testing.T) string {
	t.Helper()
	b, err := assetsFS.ReadFile("index.html")
	if err != nil {
		t.Fatalf("读 index.html: %v", err)
	}
	return string(b)
}

// TestHiddenAttributeWins 守住一条必须存在的 CSS。
//
// 真实故障：页面用 [hidden] 在列表页和详情页之间切换，而 .wrap 设了
// display:flex。浏览器自带的 [hidden]{display:none} 是 UA 样式，优先级比类
// 选择器低 —— 于是 hidden 根本没生效，两个视图同时显示，点「返回」像是没
// 反应（列表其实渲染在详情页下面）。
//
// 这条规则一旦被删，故障会再次是静默的：没有报错，只是切不动。
func TestHiddenAttributeWins(t *testing.T) {
	sheet := design.Load().Stylesheet()
	// 找规则本身，不是上面注释里提到的那几个字。必须带 !important，
	// 否则同样压不过 .wrap 的 display。
	const rule = "[hidden]{display:none !important}"
	if !strings.Contains(sheet, rule) {
		t.Fatal("app.css 里缺少 " + rule + "：页面用 hidden 切换列表页与详情页，" +
			"而 .wrap 设了 display:flex。UA 自带的 [hidden]{display:none} 优先级更低会被压掉，" +
			"结果两个视图同时显示、点返回像是没反应")
	}
}
