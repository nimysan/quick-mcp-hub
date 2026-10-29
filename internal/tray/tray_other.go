//go:build !windows

package tray

import "log"

// Run 在没有托盘实现的平台上立刻返回 ErrUnsupported，调用方据此退回到
// 「阻塞等退出信号」。
//
// 刻意不在这里引用 fyne.io/systray：它的 darwin 实现是唯一 import "C" 的
// 文件，一旦引用，CGO_ENABLED=0 的 darwin 交叉编译就会失败。
func Run(Options) error { return ErrUnsupported }

// Quit 空操作。
func Quit() {}

// Alert 在非 Windows 上退回到日志：这些平台跑这个程序时总有一个终端。
func Alert(title, msg string) { log.Printf("%s: %s", title, msg) }
