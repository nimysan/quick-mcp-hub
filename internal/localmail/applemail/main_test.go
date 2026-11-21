package applemail

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

// 这个读取包只服务 Mac 上的来源（localmail 只在 darwin 上注册它），测试夹具也按 macOS 的
// 路径和 SQLite URI 写的。别的平台上整包跳过，免得把「不在这个平台上用」报成失败。
func TestMain(m *testing.M) {
	if runtime.GOOS != "darwin" {
		fmt.Println("跳过：applemail 只在 macOS 上使用")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
