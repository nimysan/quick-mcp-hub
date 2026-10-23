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
	for _, name := range entries {
		data, err := builtin.ReadFile(name)
		if err != nil {
			loadErrs = append(loadErrs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		s, err := Parse(data)
		if err != nil {
			loadErrs = append(loadErrs, fmt.Errorf("%s: %w", path.Base(name), err))
			continue
		}
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

// ByType 按类型取声明。
func ByType(t string) (Spec, bool) {
	for _, s := range All() {
		if s.Type == t {
			return s, true
		}
	}
	return Spec{}, false
}
