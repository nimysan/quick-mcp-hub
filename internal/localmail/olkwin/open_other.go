//go:build !windows

package olkwin

import "os"

// openShared 在非 Windows 上就是普通的只读打开（用于合成夹具测试）。
func openShared(path string) (*os.File, error) { return os.Open(path) }

func isLockErr(error) bool { return false }
