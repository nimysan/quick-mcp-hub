//go:build windows

package olkwin

import (
	"errors"
	"os"
	"syscall"
)

// openShared 只读打开，共享模式放到最宽（读、写、删除都允许别人同时做）。
// 允许删除共享是关键：olk 压实 LevelDB 时会删除旧的 .ldb / .log，我们拿着句柄也不能挡住它。
// 只用标准库的 syscall，不引入新依赖。
func openShared(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

const (
	errSharingViolation syscall.Errno = 32
	errLockViolation    syscall.Errno = 33
)

func isLockErr(err error) bool {
	return errors.Is(err, errSharingViolation) || errors.Is(err, errLockViolation)
}
