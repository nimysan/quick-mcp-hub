//go:build !windows

package outlookwin

import "errors"

// DefaultDataFiles 只在 Windows 上有意义；其他平台上请直接给 Open 文件路径。
func DefaultDataFiles() ([]DataFile, error) {
	return nil, errors.Join(ErrNotFound, errors.New("经典版 Outlook for Windows 的数据文件只在 Windows 上查找"))
}
