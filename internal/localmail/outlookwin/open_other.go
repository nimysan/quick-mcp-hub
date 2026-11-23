//go:build !windows

package outlookwin

import "os"

// openShared 在非 Windows 上就是 os.Open（只读）。用于在 Mac/Linux 上读拷过来的样本。
func openShared(path string) (*os.File, error) { return os.Open(path) }

func isSharingViolation(error) bool { return false }

func systemCodepage() int { return 0 }
