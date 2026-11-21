package outlookwin

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DataFile 是在本机找到的一个 Outlook 数据文件。
type DataFile struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`              // pst / ost
	Profile string `json:"profile,omitempty"` // 从注册表找到时是 Outlook 配置文件名
	From    string `json:"from"`              // registry / default_dir
}

// defaultDirs 是经典版 Outlook 放数据文件的默认目录（[MS 支持文档]，见报告）：
// Outlook 2013 起新建的 .pst 在「文档\Outlook 文件」，.ost 和更早版本的 .pst 在
// %LOCALAPPDATA%\Microsoft\Outlook。
func defaultDirs(home, localAppData string) []string {
	var dirs []string
	if localAppData != "" {
		dirs = append(dirs, filepath.Join(localAppData, "Microsoft", "Outlook"))
	}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, "Documents", "Outlook Files"))
	}
	return dirs
}

// scanDirs 列出目录里的 .pst / .ost，只看文件名不打开文件。
func scanDirs(dirs []string) []DataFile {
	var out []DataFile
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext == ".pst" || ext == ".ost" {
				out = append(out, DataFile{Path: filepath.Join(d, e.Name()), Kind: ext[1:], From: "default_dir"})
			}
		}
	}
	return out
}

// mergeFiles 按路径去重（不区分大小写，Windows 路径），注册表里的优先，结果按路径排序。
func mergeFiles(lists ...[]DataFile) []DataFile {
	seen := map[string]bool{}
	var out []DataFile
	for _, l := range lists {
		for _, f := range l {
			k := strings.ToLower(filepath.Clean(f.Path))
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Path) < strings.ToLower(out[j].Path) })
	return out
}
