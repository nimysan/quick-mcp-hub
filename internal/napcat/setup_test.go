package napcat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeInstall 造一份假的 NapCat 安装。layout 决定 config 嵌在哪一层。
func writeInstall(t *testing.T, layout string, launcher string, cfg map[string]any, qq string) string {
	t.Helper()
	root := t.TempDir()
	cfgDir := filepath.Join(root, layout)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "onebot11_"+qq+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if launcher != "" {
		if err := os.WriteFile(filepath.Join(root, launcher), []byte("echo hi\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// httpServerCfg 是一份带 HTTP 服务器段的配置，外加一个我们不认识的字段 ——
// 用它证明 Configure 不会把别人的设置弄丢。
func httpServerCfg(enable bool, host string, port int, token string) map[string]any {
	return map[string]any{
		"network": map[string]any{
			"httpServers": []any{map[string]any{
				"name":              "HTTP",
				"enable":            enable,
				"host":              host,
				"port":              port,
				"token":             token,
				"messagePostFormat": "array",
				"debug":             false,
			}},
			"websocketServers": []any{},
		},
		"musicSignUrl":   "https://example.invalid/sign",
		"parseMultMsg":   true,
		"someFutureFlag": "由 NapCat 新版本加的，我们不认识它",
	}
}

func TestDetectNodeLayout(t *testing.T) {
	root := writeInstall(t, filepath.Join("napcat", "config"), "napcat.bat",
		httpServerCfg(false, "0.0.0.0", 3000, ""), "10001")

	in, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if in.Flavor != FlavorNode {
		t.Errorf("flavor = %q，想要 node", in.Flavor)
	}
	if filepath.Base(in.Launcher) != "napcat.bat" {
		t.Errorf("launcher = %q", in.Launcher)
	}
	if len(in.Accounts) != 1 || in.Accounts[0].QQ != "10001" {
		t.Fatalf("accounts = %+v", in.Accounts)
	}
	a := in.Accounts[0]
	if a.HTTPEnabled {
		t.Error("配置里 enable 是 false，不该报成已启用")
	}
	if a.HasToken {
		t.Error("配置里 token 是空的，不该报成已设令牌")
	}
	if a.Host != "0.0.0.0" {
		t.Errorf("host = %q，应当原样报出来，好让页面提示这是对局域网开的", a.Host)
	}
}

// 用户粘哪一层都接受：顶层、napcat 子目录、或者 config 本身。
func TestDetectAcceptsEitherLayer(t *testing.T) {
	for _, layout := range []string{filepath.Join("napcat", "config"), "config", "."} {
		root := writeInstall(t, layout, "napcat.bat", httpServerCfg(true, "127.0.0.1", 3000, "abc"), "10002")
		if _, err := Detect(root); err != nil {
			t.Errorf("layout %q: %v", layout, err)
		}
	}
}

// 粘成文件路径（手滑点到 exe / bat）时退一层，而不是报错。
func TestDetectFilePathFallsBackToDir(t *testing.T) {
	root := writeInstall(t, "config", "napcat.bat", httpServerCfg(true, "127.0.0.1", 3000, "t"), "10003")
	in, err := Detect(filepath.Join(root, "napcat.bat"))
	if err != nil {
		t.Fatalf("给文件路径时应当退一层: %v", err)
	}
	if in.Root != root {
		t.Errorf("root = %q，想要 %q", in.Root, root)
	}
}

// 从资源管理器复制的路径常带引号。
func TestDetectTrimsQuotes(t *testing.T) {
	root := writeInstall(t, "config", "", httpServerCfg(true, "127.0.0.1", 3000, "t"), "10004")
	if _, err := Detect(`"` + root + `"`); err != nil {
		t.Errorf("带引号的路径应当被接受: %v", err)
	}
}

func TestDetectRejectsUnrelatedDir(t *testing.T) {
	if _, err := Detect(t.TempDir()); err == nil {
		t.Fatal("空目录应当被拒")
	} else if !strings.Contains(err.Error(), "onebot11") {
		// 报错里要说清该找什么文件，否则用户只知道「失败了」。
		t.Errorf("报错应当点出要找的文件名，得到: %v", err)
	}
}

// 装了但还没扫码登录 —— 这是个独立的状态，报错要和「目录找错了」区分开。
func TestDetectInstalledButNeverLoggedIn(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "napcat", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "napcat.bat"), []byte("x"), 0o755)
	_, err := Detect(root)
	if err == nil || !strings.Contains(err.Error(), "onebot11") {
		t.Fatalf("没有账号文件时应当提示去找配置，得到: %v", err)
	}
}

func TestConfigureEnablesAndGeneratesToken(t *testing.T) {
	root := writeInstall(t, filepath.Join("napcat", "config"), "napcat.bat",
		httpServerCfg(false, "0.0.0.0", 3000, ""), "10005")
	in, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Configure(in, "10005", 0)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if res.TokenKept {
		t.Error("原来没有 token，不该报成沿用")
	}
	if len(res.Token) != 32 {
		t.Errorf("token = %q，想要 32 位十六进制", res.Token)
	}
	if res.Endpoint != "http://127.0.0.1:3000" {
		t.Errorf("endpoint = %q", res.Endpoint)
	}
	if !res.NeedsRestart {
		t.Error("端口上没人答，必须提示重启")
	}

	// 写回的内容：该改的改了。
	raw, err := os.ReadFile(in.Accounts[0].ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("写出来的不是合法 JSON: %v", err)
	}
	srv, ok := firstHTTPServer(doc)
	if !ok {
		t.Fatal("httpServers 段不见了")
	}
	if srv["enable"] != true {
		t.Error("enable 没被打开")
	}
	if srv["host"] != "127.0.0.1" {
		t.Errorf("host = %v，必须收到回环 —— 0.0.0.0 会让同局域网的人连得上", srv["host"])
	}
	if srv["token"] != res.Token {
		t.Error("写进文件的 token 和返回的不一致")
	}

	// 不该改的一个都没动。这是这个函数最重要的性质：那个文件里绝大多数
	// 字段是 NapCat 自己的，我们并不理解它们。
	if doc["musicSignUrl"] != "https://example.invalid/sign" {
		t.Error("无关的顶层字段被弄丢了")
	}
	if doc["someFutureFlag"] == nil {
		t.Error("不认识的字段被弄丢了 —— 新版 NapCat 的设置会这样消失")
	}
	if srv["messagePostFormat"] != "array" {
		t.Error("httpServers 里我们没碰的字段被弄丢了")
	}
	netSec := doc["network"].(map[string]any)
	if _, ok := netSec["websocketServers"]; !ok {
		t.Error("network 下的其他传输段被弄丢了")
	}
}

// 已经有 token 就沿用：用户可能已经把它填到别处了，换掉等于静默弄坏那些。
func TestConfigureKeepsExistingToken(t *testing.T) {
	root := writeInstall(t, "config", "napcat.bat",
		httpServerCfg(true, "127.0.0.1", 3000, "  已经有的令牌  "), "10006")
	in, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Configure(in, "10006", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TokenKept {
		t.Error("应当报成沿用了原有 token")
	}
	if res.Token != "已经有的令牌" {
		t.Errorf("token = %q，应当沿用原值（去掉首尾空白）", res.Token)
	}
}

// 已经完全是目标状态时一个字节都不写：重复点按钮不该堆一串备份文件。
func TestConfigureIsIdempotent(t *testing.T) {
	root := writeInstall(t, "config", "napcat.bat",
		httpServerCfg(true, "127.0.0.1", 3000, "现成的令牌"), "10012")
	in, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(in.Accounts[0].ConfigFile)
	res, err := Configure(in, "10012", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if res.BackupPath != "" {
		t.Errorf("什么都没改却留了备份: %s", res.BackupPath)
	}
	after, _ := os.ReadFile(in.Accounts[0].ConfigFile)
	if string(after) != string(before) {
		t.Error("已经是目标状态，文件不该被重写")
	}
	if res.Token != "现成的令牌" {
		t.Errorf("token = %q", res.Token)
	}
	// 端口上没人答 —— 配置对了但 NapCat 没跑，这仍然要提示。
	if !res.NeedsRestart {
		t.Error("端口没人答时必须提示启动")
	}
}

// host 是 0.0.0.0 时必须改掉，即使 enable 和 token 都已经就绪：
// 那个默认值会让同局域网的人也能用这个 QQ 发消息。
func TestConfigureTightensWildcardHost(t *testing.T) {
	root := writeInstall(t, "config", "napcat.bat",
		httpServerCfg(true, "0.0.0.0", 3000, "现成的令牌"), "10013")
	in, _ := Detect(root)
	res, err := Configure(in, "10013", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.BackupPath == "" {
		t.Error("host 需要改，应当有一次真实写入和备份")
	}
	re, _ := Detect(root)
	a, _ := re.Account("10013")
	if a.Host != "127.0.0.1" {
		t.Errorf("host = %q，必须被收到回环", a.Host)
	}
}

// 配置里完全没有 httpServers 段时要造一个，而不是报错。
func TestConfigureCreatesMissingSection(t *testing.T) {
	root := writeInstall(t, "config", "napcat.bat",
		map[string]any{"parseMultMsg": true}, "10007")
	in, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Configure(in, "10007", 7321)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if res.Port != 7321 || res.Endpoint != "http://127.0.0.1:7321" {
		t.Errorf("port/endpoint = %d / %q", res.Port, res.Endpoint)
	}
	re, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := re.Account("10007")
	if !a.HTTPEnabled || a.Port != 7321 || !a.HasToken {
		t.Errorf("重新检测应当看到新配置，得到 %+v", a)
	}
}

func TestConfigureBacksUpOriginal(t *testing.T) {
	root := writeInstall(t, "config", "napcat.bat", httpServerCfg(false, "0.0.0.0", 3000, ""), "10008")
	in, _ := Detect(root)
	before, _ := os.ReadFile(in.Accounts[0].ConfigFile)
	res, err := Configure(in, "10008", 0)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(res.BackupPath)
	if err != nil {
		t.Fatalf("备份文件读不到: %v", err)
	}
	if string(backup) != string(before) {
		t.Error("备份内容和改之前不一致")
	}
}

// 坏 JSON 必须在动手之前就拒掉：那个文件里有登录态相关的设置，
// 写坏了用户要重新扫码。
func TestConfigureRefusesBrokenJSONWithoutTouchingIt(t *testing.T) {
	root := writeInstall(t, "config", "napcat.bat", httpServerCfg(true, "127.0.0.1", 3000, "t"), "10009")
	in, _ := Detect(root)
	broken := []byte("{ 这不是 JSON")
	if err := os.WriteFile(in.Accounts[0].ConfigFile, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Configure(in, "10009", 0); err == nil {
		t.Fatal("坏 JSON 应当被拒")
	}
	after, _ := os.ReadFile(in.Accounts[0].ConfigFile)
	if string(after) != string(broken) {
		t.Error("被拒之后原文件不该有任何改动")
	}
}

func TestConfigureUnknownAccount(t *testing.T) {
	root := writeInstall(t, "config", "napcat.bat", httpServerCfg(true, "127.0.0.1", 3000, "t"), "10010")
	in, _ := Detect(root)
	if _, err := Configure(in, "99999", 0); err == nil {
		t.Fatal("不存在的账号应当被拒")
	}
}

// 两个发行包传 QQ 号的方式不一样，搞错的后果是又弹一次二维码。
// Windows 的那条在 macOS 上执行不了，但它的形状能钉住。
func TestStartCommandShapes(t *testing.T) {
	cases := []struct {
		name     string
		flavor   Flavor
		root     string
		launcher string
		goos     string
		wantName string
		wantArgs []string
	}{
		{
			name: "node 包在 Windows 上经 cmd.exe start", flavor: FlavorNode,
			root: `C:\NapCat`, launcher: `C:\NapCat\napcat.bat`, goos: "windows",
			wantName: "cmd.exe",
			// CreateProcess 不能直接跑 .bat，必须经 cmd.exe；start 的第一个
			// 参数是窗口标题，漏了它第一个路径会被当成标题。
			wantArgs: []string{"/c", "start", "NapCat", "/D", `C:\NapCat`, `C:\NapCat\napcat.bat`, "-q", "10001"},
		},
		{
			name: "shell 包的 QQ 号是位置参数", flavor: FlavorShell,
			root: `C:\NapCat`, launcher: `C:\NapCat\launcher-win10-user.bat`, goos: "windows",
			wantName: "cmd.exe",
			wantArgs: []string{"/c", "start", "NapCat", "/D", `C:\NapCat`, `C:\NapCat\launcher-win10-user.bat`, "10001"},
		},
		{
			name: "unix 下直接执行", flavor: FlavorShellUnix,
			root: "/opt/napcat", launcher: "/opt/napcat/napcat.sh", goos: "darwin",
			wantName: "/opt/napcat/napcat.sh",
			wantArgs: []string{"-q", "10001"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := &Install{Root: c.root, Launcher: c.launcher, Flavor: c.flavor}
			name, args, err := StartCommand(c.goos, in, "10001")
			if err != nil {
				t.Fatal(err)
			}
			if name != c.wantName {
				t.Errorf("name = %q，想要 %q", name, c.wantName)
			}
			if strings.Join(args, "\x00") != strings.Join(c.wantArgs, "\x00") {
				t.Errorf("args = %q\n    想要 %q", args, c.wantArgs)
			}
		})
	}
}

// 没有启动器就老实说，不去猜一个路径来执行。
func TestStartCommandWithoutLauncher(t *testing.T) {
	_, _, err := StartCommand("windows", &Install{Root: `C:\x`}, "10001")
	if err == nil {
		t.Fatal("没有启动器时应当报错")
	}
	if !strings.Contains(err.Error(), "napcat.bat") {
		t.Errorf("报错应当列出它找过的名字，得到: %v", err)
	}
}

// 启动器只认白名单里的名字。配置页面没有登录，如果能执行任意路径，
// 一个恶意网页就能借它运行任何程序。
func TestDetectIgnoresUnknownExecutable(t *testing.T) {
	root := writeInstall(t, "config", "run-me-instead.bat", httpServerCfg(true, "127.0.0.1", 3000, "t"), "10011")
	in, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if in.Launcher != "" {
		t.Errorf("白名单外的可执行文件不该被当成启动器，得到 %q", in.Launcher)
	}
}

func TestAccountEndpointFallsBackToDefaultPort(t *testing.T) {
	if got := (Account{}).Endpoint(); got != "http://127.0.0.1:3000" {
		t.Errorf("Endpoint() = %q", got)
	}
}
