// Package napcat 负责把一个已经装好的 NapCat 配置成 mcp-hub 能连的样子。
//
// 为什么需要这一层：NapCat 自己没有「给外部程序用」的一键配置。用户要先找到
// 它的 WebUI 端口（6099）、从 webui.json 里翻出登录密钥、进页面新建一个 HTTP
// 服务器、自己想一个 token、再回来填三个字段。这些全是实现细节，而这个产品的
// 用户是财务和运营。
//
// 这一层只碰磁盘上的配置文件，**不碰 NapCat 的内部 WebUI 接口**：那套接口没有
// 公开文档、跟着版本走，而 NapCat 迭代极快。配置文件的形状是 OneBot 11 的，
// 稳定得多。
//
// 边界（都是刻意的）：
//   - 只改一个**已经存在并且能解析**的 onebot11_*.json，绝不在任意路径创建文件；
//   - 只启动在检测目录里按**固定名单**找到的启动器，绝不执行用户给的任意路径；
//   - 改文件前先备份，且只动 network.httpServers，其余字段原样保留。
package napcat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// DefaultPort 是 NapCat 自带示例配置里的 HTTP 服务器端口。
const DefaultPort = 3000

// Account 是 NapCat 里一个登录过的 QQ 号，以及它的 HTTP 服务器现状。
type Account struct {
	QQ         string `json:"qq"`
	ConfigFile string `json:"configFile"`
	// HTTPEnabled 是配置文件里写的状态；Answering 是端口上真的有人答。
	// 两者会不一致 —— 改了配置还没重启 NapCat 就是这种情况，而那正是
	// 用户最容易困惑的时刻，所以分开报。
	HTTPEnabled bool   `json:"httpEnabled"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	HasToken    bool   `json:"hasToken"`
	Answering   bool   `json:"answering"`
}

// Ready 报告这个账号是否已经可以直接连 —— 端口有人答、而且设了 token。
func (a Account) Ready() bool { return a.Answering && a.HasToken }

// Endpoint 是填进渠道表单的那个地址。
func (a Account) Endpoint() string {
	port := a.Port
	if port == 0 {
		port = DefaultPort
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// Install 是一次检测的结果。
type Install struct {
	Root      string `json:"root"`
	ConfigDir string `json:"configDir"`
	// Launcher 是启动器的绝对路径，空表示这个目录里没找到认识的启动器。
	Launcher string `json:"launcher"`
	// Flavor 区分发行包：两个包的启动器名字和传 QQ 号的方式都不一样。
	Flavor   Flavor    `json:"flavor"`
	Accounts []Account `json:"accounts"`
}

// Flavor 是 NapCat 的发行包形态。
type Flavor string

const (
	// FlavorNode 是 NapCat.Shell.Windows.Node：启动器 napcat.bat，参数 -q <QQ>。
	FlavorNode Flavor = "node"
	// FlavorShell 是 NapCat.Shell：启动器 launcher-win10-user.bat 这类，QQ 号是位置参数。
	FlavorShell Flavor = "shell"
	// FlavorShellUnix 是 Linux/macOS 下的 napcat.sh。
	FlavorShellUnix Flavor = "unix"
)

// launchers 是允许执行的启动器名单。顺序即优先级。
//
// 这是个**白名单而不是搜索**：这个包被配置页面调用，而配置页面是一个没有登录
// 的本机 HTTP 服务。如果允许执行用户给的任意路径，一个恶意网页就能让它运行
// 任何程序。限定在固定名单内，攻击者至少得先在磁盘上放一个叫这个名字的文件
// 并且让目录看起来像一份 NapCat 安装。
var launchers = []struct {
	Name   string
	Flavor Flavor
}{
	{"napcat.bat", FlavorNode},
	{"launcher-win10-user.bat", FlavorShell},
	{"launcher-user.bat", FlavorShell},
	{"launcher.bat", FlavorShell},
	{"napcat.sh", FlavorShellUnix},
}

// configSubdirs 是相对用户给的目录去找 config 的候选位置。
//
// 用户可能把哪一层粘过来都有可能：解压出来的顶层、napcat 子目录、甚至 config
// 本身。三种都接受，比要求他们数层级现实。
var configSubdirs = []string{
	filepath.Join("napcat", "config"),
	"config",
	".",
}

// Detect 检查一个目录是不是 NapCat 的安装位置，并读出它现在的状态。
//
// 只读，不改任何东西。
func Detect(dir string) (*Install, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("没有给目录")
	}
	// 用户从资源管理器复制路径时常带引号。
	dir = strings.Trim(dir, `"'`)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("路径无效: %w", err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("打不开这个目录：%s", abs)
	}
	if !fi.IsDir() {
		// 粘到 exe 或 bat 上是常见手滑，自动退一层比报错有用。
		abs = filepath.Dir(abs)
	}

	in := &Install{Root: abs}
	for _, sub := range configSubdirs {
		cand := filepath.Clean(filepath.Join(abs, sub))
		files, err := filepath.Glob(filepath.Join(cand, "onebot11_*.json"))
		if err != nil || len(files) == 0 {
			continue
		}
		in.ConfigDir = cand
		sort.Strings(files)
		for _, f := range files {
			if a, err := readAccount(f); err == nil {
				in.Accounts = append(in.Accounts, a)
			}
		}
		break
	}
	if in.ConfigDir == "" {
		return nil, fmt.Errorf("这个目录里没找到 NapCat 的配置（要找的是 napcat\\config\\onebot11_<QQ号>.json）。"+
			"确认粘的是 NapCat 解压出来的那个文件夹：%s", abs)
	}
	if len(in.Accounts) == 0 {
		return nil, fmt.Errorf("找到了配置目录 %s，但里面没有登录过的账号。"+
			"先把 NapCat 启动一次、用手机扫码登录，然后再回来", in.ConfigDir)
	}

	// 启动器在根目录找。配置目录可能嵌在 napcat/ 里，但启动器总在顶层。
	for _, l := range launchers {
		p := filepath.Join(abs, l.Name)
		if _, err := os.Stat(p); err == nil {
			in.Launcher, in.Flavor = p, l.Flavor
			break
		}
	}
	return in, nil
}

// Account 按 QQ 号取一个账号。
func (in *Install) Account(qq string) (Account, bool) {
	for _, a := range in.Accounts {
		if a.QQ == qq {
			return a, true
		}
	}
	return Account{}, false
}

// readAccount 从一个 onebot11_<QQ>.json 读出 QQ 号和 HTTP 服务器现状。
func readAccount(path string) (Account, error) {
	base := filepath.Base(path)
	qq := strings.TrimSuffix(strings.TrimPrefix(base, "onebot11_"), ".json")
	if qq == "" {
		return Account{}, fmt.Errorf("文件名里没有 QQ 号: %s", base)
	}
	a := Account{QQ: qq, ConfigFile: path, Port: DefaultPort}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Account{}, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Account{}, fmt.Errorf("%s 不是合法 JSON: %w", base, err)
	}
	if srv, ok := firstHTTPServer(doc); ok {
		a.HTTPEnabled, _ = srv["enable"].(bool)
		a.Host, _ = srv["host"].(string)
		if p, ok := srv["port"].(float64); ok && p > 0 {
			a.Port = int(p)
		}
		tok, _ := srv["token"].(string)
		a.HasToken = strings.TrimSpace(tok) != ""
	}
	a.Answering = answering("127.0.0.1", a.Port)
	return a, nil
}

// firstHTTPServer 取 network.httpServers 的第一项。
func firstHTTPServer(doc map[string]any) (map[string]any, bool) {
	netSec, ok := doc["network"].(map[string]any)
	if !ok {
		return nil, false
	}
	list, ok := netSec["httpServers"].([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	srv, ok := list[0].(map[string]any)
	return srv, ok
}

// answering 报告端口上是否真的有人在听。
//
// 这是 Configure 之后唯一能区分「配置写对了」和「NapCat 已经按新配置跑起来了」
// 的办法 —— 配置文件是启动时读的，不重启就还是旧的。
func answering(host string, port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Result 是一次 Configure 的结果。
type Result struct {
	Token      string `json:"token"`
	Port       int    `json:"port"`
	Endpoint   string `json:"endpoint"`
	BackupPath string `json:"backupPath"`
	// TokenKept 为真表示配置里本来就有 token，沿用了它而不是换一个新的。
	TokenKept bool `json:"tokenKept"`
	// NeedsRestart 为真表示改完还得重启 NapCat 才生效。
	NeedsRestart bool `json:"needsRestart"`
}

// Configure 把一个账号的 HTTP 服务器打开，绑在回环上，并保证有 token。
//
// 三个刻意的选择：
//
// 沿用已有 token。配置里已经有 token 时不换新的 —— 用户可能已经把它填到
// 别的地方了，换掉等于静默弄坏那些。
//
// 只动 network.httpServers，其余字段按原样写回。这个文件里还有 NapCat 自己
// 的一堆设置，我们不理解的东西不该被我们重写。
//
// 先备份再原子替换。写坏一个配置文件的代价是用户要重新登录，所以不接受
// 「写了一半」这种状态。
func Configure(in *Install, qq string, port int) (*Result, error) {
	a, ok := in.Account(qq)
	if !ok {
		return nil, fmt.Errorf("这份安装里没有账号 %s", qq)
	}
	if port <= 0 {
		port = a.Port
	}
	if port <= 0 {
		port = DefaultPort
	}

	raw, err := os.ReadFile(a.ConfigFile)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("配置文件不是合法 JSON，没有改动它: %w", err)
	}

	netSec, _ := doc["network"].(map[string]any)
	if netSec == nil {
		netSec = map[string]any{}
		doc["network"] = netSec
	}
	list, _ := netSec["httpServers"].([]any)
	var srv map[string]any
	if len(list) > 0 {
		srv, _ = list[0].(map[string]any)
	}
	if srv == nil {
		srv = map[string]any{
			"name":              "HTTP",
			"enableCors":        true,
			"enableWebsocket":   false,
			"messagePostFormat": "array",
			"debug":             false,
		}
		list = append([]any{srv}, list...)
	}

	res := &Result{Port: port}
	if tok, _ := srv["token"].(string); strings.TrimSpace(tok) != "" {
		res.Token, res.TokenKept = strings.TrimSpace(tok), true
	} else {
		tok, err := newToken()
		if err != nil {
			return nil, err
		}
		res.Token = tok
	}

	srv["enable"] = true
	srv["host"] = "127.0.0.1" // OneBot 的默认 0.0.0.0 会让同局域网的人也连得上
	srv["port"] = port
	srv["token"] = res.Token
	netSec["httpServers"] = list

	res.Endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
	// 已经完全是目标状态就一个字节都不写 —— 重复点按钮不该堆一串备份文件，
	// 也不该让「什么都没改」看起来像一次改动。
	alreadySet := a.HTTPEnabled && a.Host == "127.0.0.1" && a.Port == port && a.HasToken
	if alreadySet {
		res.NeedsRestart = !answering("127.0.0.1", port)
		return res, nil
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	// 改之前先留一份：这个文件里有 NapCat 自己的设置，我们只理解其中一小块。
	res.BackupPath = a.ConfigFile + ".mcp-hub-backup-" + time.Now().Format("20060102-150405")
	if err := os.WriteFile(res.BackupPath, raw, 0o600); err != nil {
		return nil, fmt.Errorf("备份失败，没有改动配置: %w", err)
	}
	if err := writeAtomic(a.ConfigFile, out); err != nil {
		return nil, err
	}

	// 配置文件是 NapCat 启动时读的，所以我们刚改的这份一定还没生效。
	res.NeedsRestart = true
	return res, nil
}

// writeAtomic 同目录建临时文件再 rename，避免留下半截文件。
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // rename 成功后这个 Remove 是无害的 no-op
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成令牌失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// StartCommand 算出启动 NapCat 的命令行。
//
// 做成纯函数是为了能测：真正的 exec 在 Windows 上才有意义，而这个项目在
// macOS 上开发。两个发行包传 QQ 号的方式不一样（-q 对位置参数），搞错的
// 后果是又弹一次二维码 —— 所以页面上要把这条命令原样显示出来。
func StartCommand(goos string, in *Install, qq string) (name string, args []string, err error) {
	if in.Launcher == "" {
		return "", nil, fmt.Errorf("这个目录里没找到启动器（napcat.bat / launcher-win10-user.bat / napcat.sh）")
	}
	var self []string
	switch in.Flavor {
	case FlavorNode, FlavorShellUnix:
		self = []string{"-q", qq}
	case FlavorShell:
		self = []string{qq}
	}
	if goos == "windows" {
		// CreateProcess 不能直接执行 .bat，必须经 cmd.exe。借 start 开一个
		// 新控制台窗口：NapCat 的日志和二维码要让用户看得见，而且这样它
		// 不会随配置页面一起退出。start 的第一个引号参数是窗口标题。
		return "cmd.exe", append([]string{"/c", "start", "NapCat", "/D", in.Root, in.Launcher}, self...), nil
	}
	return in.Launcher, self, nil
}

// Start 启动 NapCat。端口上已经有人答时什么都不做 —— 点两次按钮不该起两份。
//
// 不接管生命周期：不守护、不重启、也不在配置页面退出时杀掉它。启动一次是
// 一个动作，托管一个进程是另一回事，后者要处理「谁是 owner」而收益只是省
// 掉一次双击。
func Start(in *Install, qq string) (cmdline string, started bool, err error) {
	a, ok := in.Account(qq)
	if !ok {
		return "", false, fmt.Errorf("这份安装里没有账号 %s", qq)
	}
	name, args, err := StartCommand(runtime.GOOS, in, qq)
	if err != nil {
		return "", false, err
	}
	cmdline = name + " " + strings.Join(args, " ")
	if answering("127.0.0.1", a.Port) {
		return cmdline, false, nil
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = in.Root // 启动脚本通常假设自己的工作目录
	if err := cmd.Start(); err != nil {
		return cmdline, false, fmt.Errorf("启动失败: %w", err)
	}
	// 放手：NapCat 要比配置页面活得长。
	_ = cmd.Process.Release()
	return cmdline, true, nil
}

// WaitUntilUp 轮询端口，等 NapCat 起来。
func WaitUntilUp(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if answering("127.0.0.1", port) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}
