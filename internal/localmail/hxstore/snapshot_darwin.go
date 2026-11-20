package hxstore

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// cloneSnapshot 用 clonefile(2) 在 dir 下做 src 的 APFS 克隆，返回快照路径和清理函数。
// 不用 `cp -c`：它在克隆失败（跨卷、非 APFS）时会悄悄退化成整文件复制，磁盘吃不消。
// clonefile 失败直接返回错误，由调用方退回只读读原文件。只读取 src，不改它。
func cloneSnapshot(src, dir string) (string, func(), error) {
	if dir == "" {
		dir = os.TempDir()
	}
	tmp, err := os.MkdirTemp(dir, "hxstore-snap-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	dst := filepath.Join(tmp, FileName)
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY); err != nil {
		cleanup()
		return "", nil, err
	}
	return dst, cleanup, nil
}
