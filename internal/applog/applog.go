// Package applog 把日志同时写到文件。
//
// 为什么需要它：Windows 上的 exe 用 -H windowsgui 编译之后**没有控制台窗口**
// （那个黑框是业务用户最先抱怨的东西），于是 stderr 没有地方可去。没有这个包，
// 程序出错就是彻底静默。
//
// 两个受众：
//   - 用户。「把那个日志文件发我」比「看终端输出」容易得多，页面上还有一个
//     直接看日志的入口。
//   - MCP 服务模式。那一侧 stderr 是客户端给的管道，客户端未必展示它；
//     多写一份到文件，排查「Quick 里工具列表是空的」这类问题才有据可查。
//
// 仍然同时写 stderr：有控制台时（命令行跑、被客户端 spawn）那是最直接的去处。
// 写失败会被忽略 —— 无效句柄上的写入正是 windowsgui 下的常态。
package applog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// maxBytes 超过这个大小就把旧的挪成 .old 再从头写。
//
// 不做按大小滚动的多份归档：这是一个给单机用户看的日志，留「上一次运行」
// 和「这一次运行」两份就够，再多只是让他不知道该发哪个给我。
const maxBytes = 2 << 20 // 2 MiB

// Dir 返回日志目录，按各系统的惯例放：
//
//	Windows  %LOCALAPPDATA%\mcp-hub\logs
//	macOS    ~/Library/Logs/mcp-hub
//	Linux    $XDG_STATE_HOME/mcp-hub 或 ~/.local/state/mcp-hub
func Dir(namespace string) (string, error) { return dirFor(runtime.GOOS, namespace) }

// dirFor 把系统提成参数，这样三个平台的分支都能在任意一台机器上验。
//
// 这不是为了测试好写而拐的弯：这段逻辑上一次出错正是「只有 Windows 红、
// 另两个平台绿」—— 在 macOS 上开发时，走不到的那条分支等于没有被任何东西
// 看着。项目里 napcat.StartCommand(goos, …) 和 napcat.DefaultDirs(goos, …)
// 用的是同一个做法。
func dirFor(goos, namespace string) (string, error) {
	switch goos {
	case "windows":
		// 直接读 %LOCALAPPDATA%，不走 os.UserCacheDir()：后者在非 Windows 上
		// 读的是别的变量，那样这个分支就只能在 Windows 上测。Windows 上
		// os.UserCacheDir() 做的本来就是读这个变量。
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, namespace, "logs"), nil
		}
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, namespace, "logs"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Logs", namespace), nil
	default:
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			return filepath.Join(xdg, namespace), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "state", namespace), nil
	}
}

// Path 返回日志文件路径。
func Path(namespace string) (string, error) {
	dir, err := Dir(namespace)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, namespace+".log"), nil
}

// Open 打开日志文件并返回「stderr + 文件」的组合 writer，以及文件路径。
//
// 任何一步失败都**不是**致命的：日志写不进去也不该让程序起不来。失败时
// 返回 stderr 和空路径，调用方据此决定要不要在页面上展示日志入口。
func Open(namespace string) (w io.Writer, path string, err error) {
	path, err = Path(namespace)
	if err != nil {
		return os.Stderr, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return os.Stderr, "", err
	}
	// 太大就把上一份挪走，而不是无限增长 —— 用户机器上没人会去清它。
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxBytes {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return os.Stderr, "", err
	}
	return io.MultiWriter(os.Stderr, f), path, nil
}

// Tail 读日志的最后 n 字节，给页面上的「查看日志」用。
//
// 从尾部读而不是整个读进来：这个文件可能有兆级，而排查时要看的总是最后那段。
func Tail(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	truncated := false
	if fi.Size() > n {
		start, truncated = fi.Size()-n, true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	out := string(b)
	if truncated {
		out = fmt.Sprintf("（只显示最后 %d KB，完整文件在 %s）\n\n", n/1024, path) + out
	}
	return out, nil
}
