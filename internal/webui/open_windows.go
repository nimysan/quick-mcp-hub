//go:build windows

// 把已经在的那个配置页面窗口找回来。
//
// 为什么按**窗口标题**找，而不是记住我们启动的那个进程号：Chrome 已经在跑时，
// 我们启动的 chrome.exe 只是把命令转给已有实例然后自己退出 —— 窗口属于那个
// 早就存在的进程，记进程号会落空，而「用户电脑上 Chrome 本来就开着」正是最
// 常见的情形。标题是应用模式窗口的 document.title，和是谁开的无关。
//
// 走 golang.org/x/sys/windows 的纯 syscall（托盘已经是这个路子），
// 所以 CGO_ENABLED=0 和五平台交叉编译都不受影响。
package webui

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows         = user32.NewProc("EnumWindows")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")
	procIsIconic            = user32.NewProc("IsIconic")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop    = user32.NewProc("BringWindowToTop")
	procPostMessageW        = user32.NewProc("PostMessageW")
)

const (
	swShow    = 5
	swRestore = 9
	wmClose   = 0x0010
)

var (
	// 枚举不可重入，而且 NewCallback 注册的回调**永不释放**（上限约两千个），
	// 所以回调只建一次，用这把锁串行化并借它传参数。
	focusMu  sync.Mutex
	enumOnce sync.Once
	enumCB   uintptr

	wantTitle string // 以下两个由 focusMu 保护
	foundHWND uintptr
)

// onWindow 是 EnumWindows 的回调：返回 1 继续枚举，0 停下。
func onWindow(hwnd uintptr, _ uintptr) uintptr {
	if visible, _, _ := procIsWindowVisible.Call(hwnd); visible == 0 {
		return 1
	}
	var buf [256]uint16
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return 1
	}
	// 刻意用**全等**而不是包含：浏览器标签页的标题带「 - Google Chrome」这类
	// 后缀，全等能把它排除掉。否则点「查看日志」或者页面在标签页里打开时，
	// 我们会去前置用户整个浏览器窗口 —— 那不是我们的窗口。
	if windows.UTF16ToString(buf[:n]) != wantTitle {
		return 1
	}
	foundHWND = hwnd
	return 0
}

// findWindowLocked 找那个标题的可见窗口，找不到返回 0。调用方必须持有 focusMu。
func findWindowLocked(title string) uintptr {
	wantTitle, foundHWND = title, 0
	enumOnce.Do(func() { enumCB = windows.NewCallback(onWindow) })
	// EnumWindows 在回调返回 0 时回 FALSE，所以返回值不看，只看 foundHWND。
	_, _, _ = procEnumWindows.Call(enumCB, 0)
	return foundHWND
}

// procsReady 确认用到的 user32 入口都能解析。解析不了就当这台机器没有这个
// 能力，退回调用方的原路径，而不是让 syscall 去崩。
func procsReady(procs ...*windows.LazyProc) bool {
	for _, p := range procs {
		if p.Find() != nil {
			return false
		}
	}
	return true
}

func focusConfigWindow(title string) bool {
	if !procsReady(procEnumWindows, procGetWindowTextW, procIsWindowVisible,
		procIsIconic, procShowWindow) {
		return false
	}

	focusMu.Lock()
	defer focusMu.Unlock()

	hwnd := findWindowLocked(title)
	if hwnd == 0 {
		return false
	}

	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		_, _, _ = procShowWindow.Call(hwnd, swRestore)
	} else {
		_, _, _ = procShowWindow.Call(hwnd, swShow)
	}
	// SetForegroundWindow 对一个不在前台的进程常常被系统拒绝（前台锁），
	// 所以它失败不算失败：窗口此时已经从最小化还原、重新可见，用户要的那件事
	// 已经成了。BringWindowToTop 再尽力抬一次，成不成都不改变结论。
	if ok, _, _ := procSetForegroundWindow.Call(hwnd); ok == 0 {
		_, _, _ = procBringWindowToTop.Call(hwnd)
	}
	return true
}

// closeConfigWindow 关掉那个配置页面窗口，关掉了返回 true。
//
// 托盘选「退出 mcp-hub」时用：那个窗口属于**浏览器进程**，不属于我们，
// 所以我们退出之后它会留在屏幕上 —— 而服务已经停了，它变成一个刷新就报
// 「无法访问此网站」的死窗口，任务栏上那个图标也还在（应用模式窗口拿页面
// favicon 当图标，看着就是 mcp-hub 没退）。这是用户反馈「托盘点了退出，
// 配置页面还开着」的根因。
//
// 更难察觉的后果是死窗口的标题和活窗口一模一样：下次启动后托盘点「打开
// 配置页面」会按标题找到那个死的，于是「打开了但打不开」。
//
// 用 PostMessage 而不是 SendMessage：目标是另一个进程，SendMessage 会一直
// 等它处理完，而这是退出路径，不能卡在别人的消息循环上。WM_CLOSE 等价于
// 用户自己点 X —— 浏览器正常关掉这一个窗口，不动它别的窗口，也不杀进程。
//
// 找不到窗口返回 false，这不是错误：用户可能已经自己关了，或者当时走的是
// 默认浏览器的普通标签页那条退路（标题带「 - Google Chrome」后缀，全等匹配
// 不上）—— 后者尤其不能关，那是用户自己的浏览器窗口。
func closeConfigWindow(title string) bool {
	if !procsReady(procEnumWindows, procGetWindowTextW, procIsWindowVisible, procPostMessageW) {
		return false
	}

	focusMu.Lock()
	defer focusMu.Unlock()

	hwnd := findWindowLocked(title)
	if hwnd == 0 {
		return false
	}
	ok, _, _ := procPostMessageW.Call(hwnd, wmClose, 0, 0)
	return ok != 0
}
