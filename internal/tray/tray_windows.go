//go:build windows

package tray

import (
	"sync/atomic"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
)

var started atomic.Bool

// Run 显示托盘图标并阻塞，直到用户选「退出」（或别处调了 Quit）。
func Run(o Options) error {
	ready := func() {
		if len(o.Icon) > 0 {
			systray.SetIcon(o.Icon)
		}
		if o.Tooltip != "" {
			systray.SetTooltip(o.Tooltip)
		}
		// 左键直接打开页面。右键出菜单是 Windows 的默认行为。
		if o.OnOpen != nil {
			systray.SetOnTapped(o.OnOpen)
		}

		open := systray.AddMenuItem("打开配置页面", "")
		var logItem *systray.MenuItem
		if o.ShowLog {
			logItem = systray.AddMenuItem("查看日志", "出问题时把这里的内容发给维护者")
		}
		systray.AddSeparator()
		quit := systray.AddMenuItem("退出 mcp-hub", "")

		go func() {
			for {
				select {
				case <-open.ClickedCh:
					if o.OnOpen != nil {
						o.OnOpen()
					}
				// logItem 为 nil 时这是个 nil channel，select 永远不会选到它。
				case <-clicks(logItem):
					if o.OnLog != nil {
						o.OnLog()
					}
				case <-quit.ClickedCh:
					if o.OnQuit != nil {
						o.OnQuit()
					}
					systray.Quit()
					return
				}
			}
		}()
	}
	started.Store(true)
	systray.Run(ready, func() {})
	return nil
}

func clicks(m *systray.MenuItem) <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.ClickedCh
}

// Quit 让 Run 返回。Run 还没开始时是空操作 —— 启动过程中服务先失败的
// 情况下会走到这里。
func Quit() {
	if started.Load() {
		systray.Quit()
	}
}

// Alert 弹一个系统消息框。
//
// 这是 -H windowsgui 下唯一能把致命错误告诉用户的办法：没有控制台，
// log.Fatalf 只会写进日志文件，而用户看到的是双击之后**什么都没发生**。
func Alert(title, msg string) {
	t, err1 := windows.UTF16PtrFromString(title)
	m, err2 := windows.UTF16PtrFromString(msg)
	if err1 != nil || err2 != nil {
		return
	}
	// MB_OK | MB_ICONERROR | MB_SETFOREGROUND
	_, _ = windows.MessageBox(0, m, t, 0x0|0x10|0x10000)
}
