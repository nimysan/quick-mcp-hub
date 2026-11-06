// 配置页面就绪后自己把窗口打开。
//
// 为什么这件事归程序管，而不是让用户自己去浏览器敲地址：这个程序的用户是
// 财务和运营，不是程序员。「双击一个辅助脚本，然后自己打开浏览器输一个
// 127.0.0.1 的地址」这两步，在他们那里等于产品不存在。
//
// 优先用 Chromium 系的应用模式（--app=）开一个**没有地址栏、没有标签栏**的
// 独立窗口，看起来就是个桌面应用；找不到这类浏览器就退回系统默认浏览器的
// 普通标签页。两条路都不引入 CGO、不引入 WebView 运行时，所以纯 Go 交叉
// 编译和「客户机不装任何东西」这两条都保住。
//
// 「再打开」先找已经在的那个窗口，找不到才新开 —— 见 OpenInBrowser。
package webui

import (
	"log"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// pageTitle 必须和 index.html 的 <title> 一字不差。
//
// 应用模式（--app=）下窗口标题就是 document.title，所以这是我们在系统里
// **认出自己那个窗口**的唯一依据 —— focusWindow 用它。两处写法漂移会让
// 「再打开」悄悄退化成每次新开一个窗口，所以有测试钉着它们相等。
const pageTitle = "mcp-hub · 连接与配置"

// appModeBrowsers 是支持 --app= 的浏览器，按优先级排列。
//
// Windows 上的 %LOCALAPPDATA% 这两条不能省：Chrome 在**没有管理员权限**时
// 默认就装到用户目录，而这正是我们的受众（公司电脑上的财务和运营，安装包
// 本身就是按没有管理员权限设计的）。漏了它们的后果不是报错，是静默退到
// 默认浏览器的普通标签页 —— 用户点「最小化」最小化掉的就成了他整个浏览器
// 窗口，连带他原本开着的其它标签，看起来比窗口消失更像出事了。
var appModeBrowsers = map[string][]string{
	"darwin": {
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	},
	"windows": {
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`%LOCALAPPDATA%\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`%LOCALAPPDATA%\Microsoft\Edge\Application\msedge.exe`,
	},
	"linux": {
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge",
	},
}

// fallbackOpener 是各平台「用默认浏览器打开」的命令。
var fallbackOpener = map[string][]string{
	"darwin":  {"open"},
	"windows": {"rundll32", "url.dll,FileProtocolHandler"},
	"linux":   {"xdg-open"},
}

// exists 判断一个浏览器候选可用。绝对路径看文件，裸名字看 PATH。
// 抽成变量是为了测试里能替换，不依赖这台机器上装了什么。
var exists = func(candidate string) bool {
	if candidate == "" {
		return false
	}
	if candidate[0] == '/' || (len(candidate) > 2 && candidate[1] == ':') {
		st, err := os.Stat(candidate)
		return err == nil && !st.IsDir()
	}
	_, err := exec.LookPath(candidate)
	return err == nil
}

// lookupEnv 抽成变量，测试里能模拟一台没有 LOCALAPPDATA 的机器。
var lookupEnv = os.LookupEnv

// expandEnvPath 展开候选里的 %VAR%。
//
// 变量没有值就返回 false 把这条候选整条丢掉 —— 留一个残缺的
// `\Google\Chrome\Application\chrome.exe` 去 Stat，只会得到误判。
func expandEnvPath(p string) (string, bool) {
	for {
		i := strings.Index(p, "%")
		if i < 0 {
			return p, true
		}
		j := strings.Index(p[i+1:], "%")
		if j < 0 {
			return p, true // 只有一个 %，当普通字符
		}
		name := p[i+1 : i+1+j]
		v, ok := lookupEnv(name)
		if !ok || v == "" {
			return "", false
		}
		p = p[:i] + v + p[i+1+j+1:]
	}
}

// browserCommand 拼出打开 url 的命令。第二个返回值说明用的是哪条路，
// 便于日志里讲清楚用户将看到什么形态的窗口。
func browserCommand(goos, pageURL string) (argv []string, appMode bool) {
	for _, b := range appModeBrowsers[goos] {
		path, ok := expandEnvPath(b)
		if !ok {
			continue
		}
		if exists(path) {
			// --app 去掉地址栏和标签栏；--window-size 给一个够用的初始尺寸。
			return []string{path, "--app=" + pageURL, "--window-size=1100,820"}, true
		}
	}
	if o, ok := fallbackOpener[goos]; ok {
		return append(append([]string{}, o...), pageURL), false
	}
	return nil, false
}

// isPageURL 判断这个地址是不是配置页面本身。
//
// 「再打开」只对页面本身复用已有窗口。/api/log 那类子地址必须每次真开一个
// —— 否则托盘里点「查看日志」会变成把配置页面叫到前面，日志永远打不开。
func isPageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}

// focusWindow 把已经在的那个配置页面窗口还原并叫到前面，成功返回 true。
// 平台实现见 open_windows.go / open_other.go。抽成变量是为了测试能替换。
var focusWindow = focusConfigWindow

// openNew 真的起一个浏览器进程。抽成变量是为了测试这里的分流不用去拉起进程。
var openNew = spawnBrowser

// OpenInBrowser 打开配置页面。
//
// 先试着还原已经在的那个窗口，而不是无条件新起一个 —— 这是用户反馈
// 「点最小化其实是关闭了」的根因：最小化之后他去点托盘，拿到的是一个**新**
// 窗口，被最小化的那个还躺在任务栏里。如果他当时表单填了一半，新窗口是空的，
// 从他的角度看和「刚才那个被关掉了」没有区别。
//
// 失败不是错误：服务器照常跑着，用户仍然可以自己访问那个地址。所以这里只
// 记一行日志，绝不让它影响服务本身。
func OpenInBrowser(pageURL string) {
	if isPageURL(pageURL) && focusWindow(pageTitle) {
		log.Printf("配置页面窗口已经在，把它还原到前面：%s", pageURL)
		return
	}
	openNew(pageURL)
}

func spawnBrowser(pageURL string) {
	argv, appMode := browserCommand(runtime.GOOS, pageURL)
	if len(argv) == 0 {
		log.Printf("不知道在 %s 上怎么打开浏览器，请手动访问 %s", runtime.GOOS, pageURL)
		return
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		log.Printf("自动打开窗口失败（请手动访问 %s）: %v", pageURL, err)
		return
	}
	// 不等它退出：应用模式下这个进程就是那个窗口，会一直活着。
	go func() { _ = cmd.Wait() }()
	if appMode {
		log.Printf("已打开独立窗口：%s", pageURL)
	} else {
		log.Printf("已在默认浏览器打开：%s", pageURL)
	}
}
