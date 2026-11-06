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
)

const (
	swShow    = 5
	swRestore = 9
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

func focusConfigWindow(title string) bool {
	for _, p := range []*windows.LazyProc{
		procEnumWindows, procGetWindowTextW, procIsWindowVisible,
		procIsIconic, procShowWindow,
	} {
		if p.Find() != nil {
			return false
		}
	}

	focusMu.Lock()
	defer focusMu.Unlock()

	wantTitle, foundHWND = title, 0
	enumOnce.Do(func() { enumCB = windows.NewCallback(onWindow) })
	// EnumWindows 在回调返回 0 时回 FALSE，所以返回值不看，只看 foundHWND。
	_, _, _ = procEnumWindows.Call(enumCB, 0)
	if foundHWND == 0 {
		return false
	}

	if iconic, _, _ := procIsIconic.Call(foundHWND); iconic != 0 {
		_, _, _ = procShowWindow.Call(foundHWND, swRestore)
	} else {
		_, _, _ = procShowWindow.Call(foundHWND, swShow)
	}
	// SetForegroundWindow 对一个不在前台的进程常常被系统拒绝（前台锁），
	// 所以它失败不算失败：窗口此时已经从最小化还原、重新可见，用户要的那件事
	// 已经成了。BringWindowToTop 再尽力抬一次，成不成都不改变结论。
	if ok, _, _ := procSetForegroundWindow.Call(foundHWND); ok == 0 {
		_, _, _ = procBringWindowToTop.Call(foundHWND)
	}
	return true
}
