// 渠道目录：声明式渠道的来源。
//
// 现在只有一层来源（go:embed，编译进二进制，离线可用）。往后加远端下发时
// 在这里叠加「远端拉取 + 验签 + 本地缓存」和「用户私有目录」两层，优先级
// 本地 > 远端 > 内置；Parse 和 Validate 是三层共用的入口，所以下发的内容
// 和内置的内容走完全相同的校验。
package httpchan

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"sync"
)

//go:embed catalog/*.json
var builtin embed.FS

var (
	loadOnce sync.Once
	specs    []Spec
	loadErrs []error
)

// Parse 解析并校验一份渠道声明。远端下发的内容也走这里。
func Parse(data []byte) (Spec, error) {
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(data))
	// 多出来的字段一律报错：下发内容里出现本版本不认识的键，说明目录用了
	// 更新的 schema，静默忽略会让渠道以「少了一半行为」的样子跑起来。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("解析失败: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

func load() {
	entries, err := fs.Glob(builtin, "catalog/*.json")
	if err != nil {
		loadErrs = append(loadErrs, err)
		return
	}
	sort.Strings(entries)
	byID := map[string]string{}   // id -> 来自哪个文件
	byName := map[string]string{} // 显示名 -> 来自哪个文件
	for _, name := range entries {
		base := path.Base(name)
		data, err := builtin.ReadFile(name)
		if err != nil {
			loadErrs = append(loadErrs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		s, err := Parse(data)
		if err != nil {
			loadErrs = append(loadErrs, fmt.Errorf("%s: %w", base, err))
			continue
		}
		// ID 重复是硬错误：它是凭据和配置的定位键，两份声明抢同一个 id
		// 会让后加载的那份静默顶掉前一份，用户的凭据指向就变了。
		if prev, dup := byID[s.ID]; dup {
			loadErrs = append(loadErrs, fmt.Errorf("%s: id %q 与 %s 重复", base, s.ID, prev))
			continue
		}
		// 显示名重复不影响正确性，但页面上会出现两个一模一样的条目，
		// 用户没法分辨该点哪个，所以同样拒绝。
		if prev, dup := byName[s.Name]; dup {
			loadErrs = append(loadErrs, fmt.Errorf("%s: 显示名 %q 与 %s 重复", base, s.Name, prev))
			continue
		}
		byID[s.ID], byName[s.Name] = base, base
		specs = append(specs, s)
	}
}

// All 返回全部可用的声明式渠道。
func All() []Spec {
	loadOnce.Do(load)
	out := make([]Spec, len(specs))
	copy(out, specs)
	return out
}

// Errors 返回加载目录时被跳过的条目及原因。
// 一条坏声明不会让整个程序起不来，但必须看得见。
func Errors() []error {
	loadOnce.Do(load)
	return loadErrs
}

// ByID 按渠道标识取声明。
func ByID(id string) (Spec, bool) {
	for _, s := range All() {
		if s.ID == id {
			return s, true
		}
	}
	return Spec{}, false
}
