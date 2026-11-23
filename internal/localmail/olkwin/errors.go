package olkwin

import "errors"

var (
	// ErrNotFound 表示数据目录、文件夹或邮件不存在。
	ErrNotFound = errors.New("未找到")
	// ErrUnsupportedFormat 表示新版 Outlook 的 IndexedDB 格式本构建不认识（新版 Outlook 更新后可能变）。
	ErrUnsupportedFormat = errors.New("新版 Outlook for Windows 存储格式不支持")
	// ErrLocked 表示数据文件打不开（共享冲突或字节范围锁）。只读打开 LevelDB 文件时通常不会遇到。
	ErrLocked = errors.New("新版 Outlook 数据文件正被占用")
)
