package webui

import (
	"context"
	"encoding/json"
	"github.com/nimysan/mcp-hub/internal/channels"
	"github.com/nimysan/mcp-hub/internal/webui/design"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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

// TestPageOnlyReadsRealFields 防一类静默且会被复制的错误。
//
// 真实故障：页面写 d.writeSwitches（复数），服务端端出的是 writeSwitch（单数）。
// 读一个不存在的字段在 JS 里不报错，`undefined || []` 正好得到空数组，于是
// 「当前权限」在所有渠道上永远显示「只读」——哪怕写开关其实是开着的。
// 同一批还抓到 inst.accountField 和 inst.descriptorAccountField 两处。
//
// 这类错误不会抛异常、不会打日志，而且一旦写错就会被复制到每个用它的地方。
// 所以这里把「页面读了哪些字段」和「Go 结构体真的有哪些」对起来。
func TestPageOnlyReadsRealFields(t *testing.T) {
	page := readIndex(t)
	js := page[strings.Index(page, "<script>"):]
	// 注释里会提到字段名（比如上面那段说明），不能算进来
	var code []string
	for _, line := range strings.Split(js, "\n") {
		if t := strings.TrimSpace(line); !strings.HasPrefix(t, "//") {
			code = append(code, line)
		}
	}
	js = strings.Join(code, "\n")

	// descriptor 的 json 字段名，从结构体 tag 里取 —— 手写一份清单会和代码脱节
	allowed := jsonFieldsOf(channels.Descriptor{})
	// 这几个是 /api/descriptors 响应体上的，不属于 Descriptor 本身
	for _, extra := range []string{
		"descriptors", "exePath", "logPath", "configPath", "lastClient",
		"secretBackend", "length", // length 是数组的，不是字段
	} {
		allowed[extra] = true
	}

	re := regexp.MustCompile(`\b(?:d|de|descriptor)\.([a-zA-Z]\w*)`)
	for _, m := range re.FindAllStringSubmatch(js, -1) {
		if !allowed[m[1]] {
			t.Errorf("页面读 d.%s，但 channels.Descriptor 没有这个 json 字段 —— "+
				"JS 读不存在的字段不会报错，会静默得到 undefined", m[1])
		}
	}
}

// jsonFieldsOf 取一个结构体所有 json 标签名。
func jsonFieldsOf(v any) map[string]bool {
	out := map[string]bool{}
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		out[strings.Split(tag, ",")[0]] = true
	}
	return out
}

// TestPageClassesHaveStyles 防「HTML 和 CSS 的类名对不上」这一类静默故障。
//
// 真实故障两处，都出在按稿重写时：样式表写 .adv-t 而页面用 .adv-toggle，于是
// 「高级设置（主机 / 端口）」那行没有任何样式，看不出可点；OAuth 那一整块的
// 五个类（.oauth-box / .lead / .risk-head / .oauth-code / .code-row）干脆整段
// 没搬过来，扫码授权——云之家和 GitHub 的正路——界面是裸的。
//
// 类名对不上不会报错、不会打日志，页面照常渲染，只是长得不对。
func TestPageClassesHaveStyles(t *testing.T) {
	page := readIndex(t)
	sheet := design.Load().Stylesheet()

	styled := map[string]bool{}
	// 必须先剥掉 /* ... */：注释里会提到类名（上面那段说明就同时写了 .adv-t
	// 和 .adv-toggle），不剥的话一个只存在于注释里的名字也算「有样式」，
	// 规则真写错了测试照样放行。
	bare := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(sheet, " ")
	for _, m := range regexp.MustCompile(`\.([a-zA-Z][-\w]*)`).FindAllStringSubmatch(bare, -1) {
		styled[m[1]] = true
	}

	// 只看 class="..." 里的字面量。先按大括号配对剥掉 ${...}：那里面是运行时
	// 拼出来的状态类（on / show / lead ...），名字随分支变，放进来会一直误报。
	// 必须在抓 class="..." 之前剥 —— 模板表达式里有自己的引号
	// （class="oauth-box${f?" lead":""}"），不剥就会把属性值截断，
	// 于是把表达式里的变量名当成类名。
	clean := stripTemplates(page)
	ident := regexp.MustCompile(`[a-zA-Z][-\w]*`)
	for _, m := range regexp.MustCompile(`class="([^"]*)"`).FindAllStringSubmatch(clean, -1) {
		for _, c := range ident.FindAllString(m[1], -1) {
			if !styled[c] {
				t.Errorf("页面用了 class %q，但样式表里没有这个选择器 —— "+
					"类名对不上不会报错，页面只是长得不对", c)
			}
		}
	}
}

// stripTemplates 把 ${...} 整段换成一个空格，支持嵌套大括号。
func stripTemplates(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '{' {
			depth, j := 1, i+2
			for ; j < len(s) && depth > 0; j++ {
				switch s[j] {
				case '{':
					depth++
				case '}':
					depth--
				}
			}
			b.WriteByte(' ')
			i = j - 1
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// TestSpriteHasNoAnonymousSymbol 守住 sprite 的无名条目过滤。
//
// Figma 的提取会带出无名条目（稿子里的开关控件图形就是这样进来的）。不过滤
// 就会生成一个 id="i-" 的 symbol：页面永远引用不到它，但它跟着每个页面发出去。
func TestSpriteHasNoAnonymousSymbol(t *testing.T) {
	if s := design.Load().IconSprite(); strings.Contains(s, `id="i-"`) {
		t.Error(`sprite 里有 id="i-" 的无名 symbol —— ` +
			"IconSprite 必须跳过 tokens.json 里键名为空的条目")
	}
}

// TestOAuthChannelCanBeReauthorized 守住「已保存的 OAuth 连接能重新授权」。
//
// 真实故障：授权块的渲染条件写的是 `d.oauth && !account`，也就是只在**新建**
// 时出现。于是一条已保存的连接授权失效之后（到期、被吊销、对方系统里解除了
// 授权），页面上没有任何地方能重新授权 —— 留在编辑表单里的只有一个令牌
// 输入框，而设备流的令牌是一整段 JSON，用户填不出来。唯一的出路是删掉重建，
// 那会连带删掉系统密码管理器里的凭据，并把写开关这些设置一起丢掉。
//
// 这类故障是静默的：页面不报错，只是少了一个按钮。
func TestOAuthChannelCanBeReauthorized(t *testing.T) {
	page := readIndex(t)

	// 渲染条件不能再把已保存的实例排除掉。
	if strings.Contains(page, "(d.oauth && !account)") {
		t.Error("授权块又变成只在新建时渲染了：已保存的连接将无法重新授权，" +
			"而编辑表单里那个令牌字段是设备流令牌，用户填不出来")
	}
	// 详情页要有入口 —— 连接坏掉时用户看的是那一页（测试连接在那里）。
	if !strings.Contains(page, `id="dreauth"`) {
		t.Error("详情页缺少「重新授权」按钮：那是连接失效时用户会去的页面")
	}
	if !strings.Contains(page, "function reauth(") {
		t.Error("缺少 reauth()：详情页的重新授权按钮会点了没反应")
	}

	// 重新授权必须带上已保存的设置。授权成功后走的是常规保存路径，不带
	// 就等于把写开关重置成默认值 —— 用户会发现发消息的工具悄悄不见了。
	if !strings.Contains(page, "reauth(d.type, i.account, i.settings)") {
		t.Error("重新授权没有带上 i.settings：写开关等设置会被重置成默认值")
	}
	// 授权回来的若是另一个账号，那是新建而不是修复，必须当场说清楚：
	// 凭据按账号名定位，旧那条会原封不动留着，而它仍然是坏的。
	if !strings.Contains(page, "st.account !== expectAccount") {
		t.Error("没有比对授权回来的账号：换账号授权会新建一条连接，" +
			"而用户以为修好了，实际在用的还是坏的那条")
	}
}

// TestPageReadsRealViewFields 把页面读的 v.* 和 channels.View 真有的字段对起来。
//
// 和 TestPageOnlyReadsRealFields 同一类防护，只是那条守 d.*（Descriptor）。
// 授权状态是从 v.auth 读的，而它整条链上的字段名（expiresAt、canRefresh、
// stale）写错任何一个都不会报错：JS 读不存在的字段得到 undefined，于是
// 「授权已失效」这个判断永远为假 —— 页面一直显示「有效」，而连接早就坏了。
func TestPageReadsRealViewFields(t *testing.T) {
	page := readIndex(t)
	js := codeOnly(page)

	allowed := jsonFieldsOf(channels.View{})
	re := regexp.MustCompile(`\bv\.([a-zA-Z]\w*)`)
	for _, m := range re.FindAllStringSubmatch(js, -1) {
		if !allowed[m[1]] {
			t.Errorf("页面读 v.%s，但 channels.View 没有这个 json 字段", m[1])
		}
	}

	// 令牌元数据的字段名单独核一遍：它们只在这两个函数里用，而 a 这个变量名
	// 在页面别处指别的东西（DOM 节点、napcat 探测结果），不能一刀切地扫。
	authFields := jsonFieldsOf(channels.AuthInfo{})
	for _, fn := range []string{"function authPill(", "function authDetail("} {
		i := strings.Index(js, fn)
		if i < 0 {
			t.Fatalf("页面里没有 %s —— 授权状态不会显示", fn)
		}
		body := js[i:]
		if j := strings.Index(body[1:], "\nfunction "); j > 0 {
			body = body[:j]
		}
		for _, m := range regexp.MustCompile(`\ba\.([a-zA-Z]\w*)`).FindAllStringSubmatch(body, -1) {
			if !authFields[m[1]] {
				t.Errorf("%s 里读 a.%s，但 channels.AuthInfo 没有这个 json 字段 —— "+
					"读错字段得到 undefined，授权状态会一直显示成有效", fn, m[1])
			}
		}
	}
}

// TestAuthExpiryIsNeverShownBare 守住一条表述要求，不是一条机制。
//
// 访问令牌本来就只有两小时，而它到期时程序自己会换一张。所以「剩余 1 小时
// 52 分钟」这个数字单独摆出来会让用户得出错误结论 —— 以为每两小时要重新
// 授权一次。凡是显示这个数字的分支，都必须同时说清会不会自动续期。
func TestAuthExpiryIsNeverShownBare(t *testing.T) {
	js := codeOnly(readIndex(t))
	i := strings.Index(js, "function authDetail(")
	if i < 0 {
		t.Fatal("页面里没有 authDetail()")
	}
	body := js[i:]
	if j := strings.Index(body[1:], "\nfunction "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "canRefresh") {
		t.Error("authDetail 没有按 canRefresh 分支：剩余时间会被当成「还要多久重新授权」")
	}
	// 这条断言要盯住「显示剩余时间的那一支」，不是整个函数里随便哪处提到了
	// 续期 —— 过期那一支本来也会提，拿它顶替会让这条检查形同虚设。
	// 这是一条文案要求，所以直接钉住三种状态各自必须出现的那句话。
	//
	// 试过按行、按 return 语句、按出现位置开窗口三种更「聪明」的写法，三种
	// 都能被骗过去：这段文案一处在 return 的模板字符串里、一处在 const 的
	// 三元表达式里，而三元的两个分支彼此相邻，窗口会把隔壁那支的说明算成
	// 自己的。改写文案时这条会红，那时请连着上面这段说明一起读。
	for _, want := range []string{
		"到期前程序会自动续期",         // 能自动续期、还没过期：剩余时间必须配上这句
		"下次调用时程序会自动续期",       // 能自动续期、已经过期：这不是故障
		"没有刷新令牌，所以到期后不会自动续期", // 到期后真的要人来处理
		"到期时不会自动续期",          // 旧令牌没记到期时间：同样要人处理
	} {
		if !strings.Contains(body, want) {
			t.Errorf("authDetail 里缺少「%s」—— 剩余有效期只有连着"+
				"「到期后会怎样」一起说才有意义，裸着给数字会让用户"+
				"以为每两小时都要重新授权一次", want)
		}
	}
	if !strings.Contains(body, "访问令牌还有") {
		t.Error("authDetail 不显示剩余有效期了")
	}

	// 没有到期时间的旧令牌必须单独说。它不会被判为该刷新，所以一过期就死在
	// 原地，而页面若按「有效」显示，用户不会知道它随时会坏。
	if !strings.Contains(body, "没有记到期时间") {
		t.Error("没有处理「令牌没有到期时间」这种旧数据：页面会显示有效，而它一过期就不可用")
	}
}

// codeOnly 去掉注释行 —— 注释里会提到字段名，不该算进字段检查。
func codeOnly(page string) string {
	js := page[strings.Index(page, "<script>"):]
	var code []string
	for _, line := range strings.Split(js, "\n") {
		if s := strings.TrimSpace(line); !strings.HasPrefix(s, "//") {
			code = append(code, line)
		}
	}
	return strings.Join(code, "\n")
}
