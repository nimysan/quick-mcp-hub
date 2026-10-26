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
package webui

import (
	"log"
	"os"
	"os/exec"
	"runtime"
)

// appModeBrowsers 是支持 --app= 的浏览器，按优先级排列。
//
// Windows 上 Edge 几乎一定在，所以那边大概率能拿到干净窗口；macOS 上可能
// 只装了 Safari（没有这个能力），那就退回标签页。
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
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
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

// browserCommand 拼出打开 url 的命令。第二个返回值说明用的是哪条路，
// 便于日志里讲清楚用户将看到什么形态的窗口。
func browserCommand(goos, url string) (argv []string, appMode bool) {
	for _, b := range appModeBrowsers[goos] {
		if exists(b) {
			// --app 去掉地址栏和标签栏；--window-size 给一个够用的初始尺寸。
			return []string{b, "--app=" + url, "--window-size=1100,820"}, true
		}
	}
	if o, ok := fallbackOpener[goos]; ok {
		return append(append([]string{}, o...), url), false
	}
	return nil, false
}

// OpenInBrowser 打开配置页面。
//
// 失败不是错误：服务器照常跑着，用户仍然可以自己访问那个地址。所以这里只
// 记一行日志，绝不让它影响服务本身。
func OpenInBrowser(url string) {
	argv, appMode := browserCommand(runtime.GOOS, url)
	if len(argv) == 0 {
		log.Printf("不知道在 %s 上怎么打开浏览器，请手动访问 %s", runtime.GOOS, url)
		return
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		log.Printf("自动打开窗口失败（请手动访问 %s）: %v", url, err)
		return
	}
	// 不等它退出：应用模式下这个进程就是那个窗口，会一直活着。
	go func() { _ = cmd.Wait() }()
	if appMode {
		log.Printf("已打开独立窗口：%s", url)
	} else {
		log.Printf("已在默认浏览器打开：%s", url)
	}
}
