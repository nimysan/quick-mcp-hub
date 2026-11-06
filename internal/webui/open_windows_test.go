//go:build windows

package webui

import "testing"

// TestUser32ProcsResolve：每个 user32 入口名都必须能解析。
//
// 这条只能在 Windows 上跑，而它正好防住一类在别的平台上**永远发现不了**的
// 错误：proc 名拼错（比如把 PostMessageW 写成 PostMessage）。LazyProc 是
// 惰性的，拼错不会在编译期或启动时暴露，而 Call 一个解析不了的 proc 会 panic
// —— 于是「托盘点退出」会把整个程序崩掉，而不是关掉窗口。
//
// 交叉编译只保证语法正确，CI 的 windows-latest 跑这一条才保证名字对。
func TestUser32ProcsResolve(t *testing.T) {
	for name, p := range map[string]interface{ Find() error }{
		"EnumWindows":         procEnumWindows,
		"GetWindowTextW":      procGetWindowTextW,
		"IsWindowVisible":     procIsWindowVisible,
		"IsIconic":            procIsIconic,
		"ShowWindow":          procShowWindow,
		"SetForegroundWindow": procSetForegroundWindow,
		"BringWindowToTop":    procBringWindowToTop,
		"PostMessageW":        procPostMessageW,
	} {
		if err := p.Find(); err != nil {
			t.Errorf("user32!%s 解析不了: %v", name, err)
		}
	}
}

// TestCloseConfigWindowNoMatchIsQuiet：没有这个标题的窗口时必须安静返回 false。
//
// 这一条在 CI 的 windows runner 上是真的在跑 syscall（那里没有我们的配置页面
// 窗口），所以它同时验了：枚举能跑通、全等匹配没有误命中、找不到时不 panic。
// 误命中的后果是关掉用户自己的某个窗口。
func TestCloseConfigWindowNoMatchIsQuiet(t *testing.T) {
	if closeConfigWindow("mcp-hub 这个标题的窗口不可能存在 ·" + t.Name()) {
		t.Fatal("不存在的标题却报告关掉了窗口 —— 匹配逻辑误命中了别人的窗口")
	}
}
