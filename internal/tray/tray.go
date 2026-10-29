// Package tray 提供系统托盘图标，让这个程序像一个普通软件：没有黑窗口，
// 关掉配置页面之后还能从托盘把它找回来。
//
// 为什么是必需的而不是装饰：Windows 上去掉控制台窗口（-H windowsgui）之后，
// 如果再没有托盘图标，用户关掉配置页面就只剩一个**看不见的进程** —— 既没有
// 入口把页面叫回来，也没有办法退出它，只能去任务管理器。所以「去掉黑窗口」
// 和「加托盘」必须一起做。
//
// 只有 Windows 有实现。这是刻意的：
//   - Windows 是客户所在的平台，而 fyne.io/systray 的 Windows 实现走
//     golang.org/x/sys/windows 的纯 syscall，不需要 cgo，所以 CGO_ENABLED=0
//     的五平台交叉编译不受影响；
//   - 它的 darwin 实现是唯一 import "C" 的文件，一旦在 darwin 上也引用，
//     交叉编译立刻要 cgo。macOS 上「像普通软件」的对应物是包成 .app
//     （Dock 图标），那是打包层的事，不是这里。
package tray

import "errors"

// ErrUnsupported 表示这个平台没有托盘实现，调用方应当退回到原来的行为
// （阻塞等退出信号）。
var ErrUnsupported = errors.New("这个平台没有托盘图标")

// Options 是托盘的回调与外观。
type Options struct {
	// Icon 是 .ico 格式的图标字节。
	Icon    []byte
	Tooltip string
	// OnOpen 在左键点击图标、以及选菜单「打开配置页面」时调用。
	OnOpen func()
	// OnLog 在选菜单「查看日志」时调用；ShowLog 为 false 时不显示这一项
	// （日志文件打不开时就不该给一个点了没反应的菜单）。
	OnLog   func()
	ShowLog bool
	// OnQuit 在用户选「退出」时调用，之后 Run 返回。
	OnQuit func()
}
