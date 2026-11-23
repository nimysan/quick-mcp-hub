//go:build windows

package outlookwin

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// openShared 只读打开，并把共享模式放到最宽（读、写、删除都允许别人同时做）：
// 已经以写方式打开文件的 Outlook 只有在我们也允许写共享时才不冲突。
// 如果 Outlook 自己拒绝读共享，这里会得到 ERROR_SHARING_VIOLATION，本包不绕过。
func openShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func isSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// systemCodepage 返回系统 ANSI 代码页（中文 Windows 是 936）。ANSI PST 里没标代码页的字符串按它解。
func systemCodepage() int { return int(windows.GetACP()) }
