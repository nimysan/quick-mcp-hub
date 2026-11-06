//go:build !windows

package webui

// focusConfigWindow 在非 Windows 平台上不做事，调用方据此退回新开一个窗口
// —— 也就是今天的行为。
//
// 刻意不实现：用户反馈的是 Windows。macOS 上再启动一次 `Chrome --app=` 会得到
// 一个新窗口而不是复用，要真复用得走 AppleScript 去点名窗口标题，那是另一套
// 依赖和另一组失败模式，没有对应的反馈就不值得背上。
func focusConfigWindow(string) bool { return false }
