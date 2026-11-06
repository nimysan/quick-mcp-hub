package webui

import (
	"regexp"
	"strings"
	"testing"
)

// 这组测试不依赖本机装了什么浏览器：exists 被替换掉，所以三个平台的分支
// 都能在同一台机器上验。

func withBrowsers(t *testing.T, present ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	orig := exists
	exists = func(c string) bool { return set[c] }
	t.Cleanup(func() { exists = orig })
}

func TestAppModeWhenChromiumPresent(t *testing.T) {
	withBrowsers(t, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
	argv, appMode := browserCommand("darwin", "http://127.0.0.1:7801")
	if !appMode {
		t.Fatalf("装了 Chrome 却没走应用模式: %v", argv)
	}
	if !strings.Contains(strings.Join(argv, " "), "--app=http://127.0.0.1:7801") {
		t.Fatalf("没有用 --app 参数: %v", argv)
	}
}

// TestEdgePickedOnWindows：Windows 上 Edge 几乎一定在，所以那边大概率能
// 拿到干净窗口 —— 这条确认 Edge 也在候选里，不是只认 Chrome。
func TestEdgePickedOnWindows(t *testing.T) {
	withBrowsers(t, `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`)
	argv, appMode := browserCommand("windows", "http://127.0.0.1:7801")
	if !appMode || !strings.Contains(argv[0], "msedge.exe") {
		t.Fatalf("没挑到 Edge: %v appMode=%v", argv, appMode)
	}
}

// TestFallbackWhenOnlySafari：macOS 上只有 Safari 时没有应用模式可用，
// 必须退回默认浏览器，而不是什么都不做。
func TestFallbackWhenOnlySafari(t *testing.T) {
	withBrowsers(t) // 一个 Chromium 都没有
	argv, appMode := browserCommand("darwin", "http://127.0.0.1:7801")
	if appMode {
		t.Fatal("没有 Chromium 却声称走了应用模式")
	}
	if len(argv) != 2 || argv[0] != "open" || argv[1] != "http://127.0.0.1:7801" {
		t.Fatalf("退回命令不对: %v", argv)
	}
}

func TestFallbackPerPlatform(t *testing.T) {
	withBrowsers(t)
	for goos, want := range map[string]string{
		"windows": "rundll32",
		"linux":   "xdg-open",
		"darwin":  "open",
	} {
		argv, _ := browserCommand(goos, "http://x")
		if len(argv) == 0 || argv[0] != want {
			t.Errorf("%s 的退回命令 = %v，期望 %s", goos, argv, want)
		}
	}
}

// TestUnknownPlatformIsQuiet：不认识的平台返回空，由调用方记一行日志，
// 绝不能让它影响服务本身 —— 页面照样能手动访问。
func TestUnknownPlatformIsQuiet(t *testing.T) {
	withBrowsers(t)
	if argv, _ := browserCommand("plan9", "http://x"); argv != nil {
		t.Fatalf("不认识的平台竟然给了命令: %v", argv)
	}
}

// TestPreferenceOrder：同时装了多个时按候选表顺序挑，输出要稳定。
func TestPreferenceOrder(t *testing.T) {
	chrome := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	edge := "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"
	withBrowsers(t, edge, chrome)
	argv, _ := browserCommand("darwin", "http://x")
	if argv[0] != chrome {
		t.Fatalf("顺序不稳定，挑到了 %s", argv[0])
	}
}

// withEnv 替换 lookupEnv，模拟一台只有指定环境变量的机器。
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	orig := lookupEnv
	lookupEnv = func(k string) (string, bool) { v, ok := kv[k]; return v, ok }
	t.Cleanup(func() { lookupEnv = orig })
}

// TestUserProfileChromePickedOnWindows：Chrome 在没有管理员权限时装到用户目录
// —— 公司电脑上的财务和运营就是这种情况。漏了这条候选不会报错，只会静默退到
// 默认浏览器的普通标签页。
func TestUserProfileChromePickedOnWindows(t *testing.T) {
	withEnv(t, map[string]string{"LOCALAPPDATA": `C:\Users\fin\AppData\Local`})
	withBrowsers(t, `C:\Users\fin\AppData\Local\Google\Chrome\Application\chrome.exe`)
	argv, appMode := browserCommand("windows", "http://127.0.0.1:7801")
	if !appMode {
		t.Fatalf("用户目录装的 Chrome 没被认出来，退到了标签页: %v", argv)
	}
	if argv[0] != `C:\Users\fin\AppData\Local\Google\Chrome\Application\chrome.exe` {
		t.Fatalf("挑到的不是用户目录那个: %v", argv)
	}
}

// TestUserProfileEdgePickedOnWindows：Edge 也有用户目录安装（Dev/Beta 常见）。
func TestUserProfileEdgePickedOnWindows(t *testing.T) {
	withEnv(t, map[string]string{"LOCALAPPDATA": `C:\Users\fin\AppData\Local`})
	withBrowsers(t, `C:\Users\fin\AppData\Local\Microsoft\Edge\Application\msedge.exe`)
	argv, appMode := browserCommand("windows", "http://x")
	if !appMode || !strings.Contains(argv[0], `AppData\Local\Microsoft\Edge`) {
		t.Fatalf("没挑到用户目录的 Edge: %v appMode=%v", argv, appMode)
	}
}

// TestMachineWideWinsOverUserProfile：两处都装了时输出要稳定，按候选表顺序。
func TestMachineWideWinsOverUserProfile(t *testing.T) {
	withEnv(t, map[string]string{"LOCALAPPDATA": `C:\Users\fin\AppData\Local`})
	withBrowsers(t,
		`C:\Users\fin\AppData\Local\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`)
	argv, _ := browserCommand("windows", "http://x")
	if argv[0] != `C:\Program Files\Google\Chrome\Application\chrome.exe` {
		t.Fatalf("顺序不稳定，挑到了 %s", argv[0])
	}
}

// TestNoLocalAppDataDropsCandidate：变量没有值时整条候选要丢掉。
// 留一个残缺的 `\Google\Chrome\...` 去 Stat 只会得到误判 —— 这里断言
// exists 压根没被问到那条路径。
func TestNoLocalAppDataDropsCandidate(t *testing.T) {
	withEnv(t, map[string]string{}) // 没有 LOCALAPPDATA
	var asked []string
	orig := exists
	exists = func(c string) bool { asked = append(asked, c); return false }
	t.Cleanup(func() { exists = orig })

	argv, appMode := browserCommand("windows", "http://x")
	if appMode || len(argv) == 0 || argv[0] != "rundll32" {
		t.Fatalf("没有浏览器时该退回默认浏览器: %v appMode=%v", argv, appMode)
	}
	for _, a := range asked {
		if strings.Contains(a, "%") || strings.HasPrefix(a, `\Google`) || strings.HasPrefix(a, `\Microsoft`) {
			t.Fatalf("把没展开/残缺的路径拿去判存在了: %q", a)
		}
	}
}

// TestOnlyPageURLReusesWindow：/api/log 这类子地址必须每次真开一个。
// 否则托盘里点「查看日志」会变成把配置页面叫到前面，日志永远打不开。
func TestOnlyPageURLReusesWindow(t *testing.T) {
	for _, c := range []struct {
		url   string
		reuse bool
	}{
		{"http://127.0.0.1:7801", true},
		{"http://127.0.0.1:7801/", true},
		{"http://127.0.0.1:7801/api/log", false},
		{"http://127.0.0.1:7801/?tab=x", false},
		{"http://127.0.0.1:7801/#jira", false},
	} {
		if got := isPageURL(c.url); got != c.reuse {
			t.Errorf("isPageURL(%q) = %v，期望 %v", c.url, got, c.reuse)
		}
	}
}

// TestOpenInBrowserPrefersExistingWindow 钉住用户反馈的那个行为：
// 窗口已经在（比如被最小化了）时，再打开必须还原它，而不是新起一个
// —— 新窗口是空的，用户填了一半的表单看起来就像被关掉了。
func TestOpenInBrowserPrefersExistingWindow(t *testing.T) {
	var focusedWith []string
	var opened []string
	origF, origO := focusWindow, openNew
	focusWindow = func(title string) bool { focusedWith = append(focusedWith, title); return true }
	openNew = func(u string) { opened = append(opened, u) }
	t.Cleanup(func() { focusWindow, openNew = origF, origO })

	OpenInBrowser("http://127.0.0.1:7801")
	if len(opened) != 0 {
		t.Fatalf("已有窗口还新开了一个: %v", opened)
	}
	if len(focusedWith) != 1 || focusedWith[0] != pageTitle {
		t.Fatalf("找窗口用的标题不对: %v", focusedWith)
	}

	// 日志那一路绝不能被窗口复用吞掉。
	OpenInBrowser("http://127.0.0.1:7801/api/log")
	if len(opened) != 1 || opened[0] != "http://127.0.0.1:7801/api/log" {
		t.Fatalf("/api/log 没有真打开: opened=%v", opened)
	}
	if len(focusedWith) != 1 {
		t.Fatalf("/api/log 不该去找已有窗口: %v", focusedWith)
	}
}

// TestOpenInBrowserFallsBackWhenNoWindow：找不到已有窗口就得照旧新开一个
// —— 非 Windows 平台上 focusConfigWindow 永远返回 false，走的就是这条。
func TestOpenInBrowserFallsBackWhenNoWindow(t *testing.T) {
	var opened []string
	origF, origO := focusWindow, openNew
	focusWindow = func(string) bool { return false }
	openNew = func(u string) { opened = append(opened, u) }
	t.Cleanup(func() { focusWindow, openNew = origF, origO })

	OpenInBrowser("http://127.0.0.1:7801")
	if len(opened) != 1 {
		t.Fatalf("没有已有窗口时该新开一个: %v", opened)
	}
}

// TestPageTitleMatchesHTML：应用模式下窗口标题就是 document.title，所以
// pageTitle 是我们在系统里认出自己那个窗口的唯一依据。两处漂移不会报错，
// 只会让「再打开」悄悄退化成每次新开一个窗口。
func TestPageTitleMatchesHTML(t *testing.T) {
	b, err := assetsFS.ReadFile("index.html")
	if err != nil {
		t.Fatalf("读不到 index.html: %v", err)
	}
	m := regexp.MustCompile(`(?s)<title>(.*?)</title>`).FindSubmatch(b)
	if m == nil {
		t.Fatal("index.html 里没有 <title> —— 应用模式窗口会变成无标题，找不回来")
	}
	if got := string(m[1]); got != pageTitle {
		t.Fatalf("pageTitle = %q，index.html 的 <title> = %q —— 必须一字不差", pageTitle, got)
	}
}
