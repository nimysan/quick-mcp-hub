package webui

import (
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
