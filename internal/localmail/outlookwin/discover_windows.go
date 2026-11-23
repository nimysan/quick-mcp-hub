//go:build windows

package outlookwin

import (
	"os"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

// 注册表里 MAPI 配置文件的位置：每个配置文件下有若干 32 位十六进制命名的子键（MAPI 服务段），
// PST 服务的段里有 001f6700（PR_PST_PATH_W），OST 的段里有 001f6610（脱机文件路径）。
// 值是 REG_BINARY 存的 UTF-16LE 字符串。只读，不写注册表。
const (
	regPSTPath = "001f6700"
	regOSTPath = "001f6610"
)

var officeVersions = []string{"16.0", "15.0"} // Outlook 2016/2019/2021/2024/M365 都是 16.0；2013 是 15.0

// DefaultDataFiles 找当前用户的 .pst / .ost：先读注册表里各 Outlook 配置文件登记的路径，
// 再扫默认目录。只列出存在的文件。
func DefaultDataFiles() ([]DataFile, error) {
	var reg []DataFile
	for _, v := range officeVersions {
		reg = append(reg, registryFiles(`Software\Microsoft\Office\`+v+`\Outlook\Profiles`)...)
	}
	home, _ := os.UserHomeDir()
	files := mergeFiles(reg, scanDirs(defaultDirs(home, os.Getenv("LOCALAPPDATA"))))
	out := files[:0]
	for _, f := range files {
		if fi, err := os.Stat(f.Path); err == nil && !fi.IsDir() {
			out = append(out, f)
		}
	}
	return out, nil
}

func registryFiles(base string) []DataFile {
	k, err := registry.OpenKey(registry.CURRENT_USER, base, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	profiles, _ := k.ReadSubKeyNames(-1)
	var out []DataFile
	for _, prof := range profiles {
		pk, err := registry.OpenKey(k, prof, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		sects, _ := pk.ReadSubKeyNames(-1)
		for _, s := range sects {
			sk, err := registry.OpenKey(pk, s, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			for name, kind := range map[string]string{regPSTPath: "pst", regOSTPath: "ost"} {
				if b, _, err := sk.GetBinaryValue(name); err == nil {
					if p := regString(b); p != "" {
						out = append(out, DataFile{Path: p, Kind: kind, Profile: prof, From: "registry"})
					}
				}
			}
			sk.Close()
		}
		pk.Close()
	}
	return out
}

func regString(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return strings.TrimRight(string(utf16.Decode(u)), "\x00")
}
