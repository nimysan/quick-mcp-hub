package feishuweb

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// extensionFS 是随二进制分发的浏览器扩展。程序仍然是一个文件：用户在配置
// 页面点「释放扩展」，文件写到本机目录，再在 chrome://extensions 里
// 「加载已解压的扩展程序」。
//
//go:embed extension
var extensionFS embed.FS

// ExtensionFiles 返回扩展文件（根目录就是扩展目录）。
func ExtensionFiles() fs.FS {
	sub, err := fs.Sub(extensionFS, "extension")
	if err != nil {
		panic(err) // 只在嵌入目录改名时发生，测试会先抓到
	}
	return sub
}

// ExtensionDir 是扩展释放到的目录：用户配置目录下的 <namespace>/feishu-web-extension。
func ExtensionDir(namespace string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("定位用户配置目录失败: %w", err)
	}
	return filepath.Join(dir, namespace, "feishu-web-extension"), nil
}

// ExtractExtension 把扩展写到 dir，覆盖同名文件（升级 mcp-hub 后再点一次就是升级扩展）。
// 目录里别的文件不动。
func ExtractExtension(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	src := ExtensionFiles()
	return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	})
}
