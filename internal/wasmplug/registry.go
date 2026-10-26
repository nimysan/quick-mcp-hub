// 已安装插件的发现与加载。
//
// 两件事刻意分开：**发现**只读 manifest（便宜，启动时做），**加载**才把 wasm
// 编译进运行时（3MB 的模块要上百毫秒到一秒）。工具注册只需要 manifest，所以
// MCP 启动时不编译任何模块 —— stdio 握手卡在编译上会被客户端判超时。
package wasmplug

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Dir 返回已安装插件的目录：<用户配置目录>/<namespace>/plugins。
func Dir(namespace string) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("定位用户配置目录失败: %w", err)
	}
	return filepath.Join(base, namespace, "plugins"), nil
}

// Installed 是磁盘上一个已安装插件：manifest 加它旁边的模块文件。
type Installed struct {
	Manifest   Manifest
	Dir        string
	ModulePath string
}

// Discover 扫描插件目录。只读 manifest，不加载模块。
//
// 一个坏插件只让自己失效：返回的错误是逐个插件的，调用方照常用其余的。
// 全目录一起失败会让「装了个坏插件」变成「整个程序起不来」。
func Discover(namespace string) ([]Installed, []error) {
	dir, err := Dir(namespace)
	if err != nil {
		return nil, []error{err}
	}
	return DiscoverIn(dir)
}

// DiscoverIn 扫描指定目录，每个子目录一个插件。
func DiscoverIn(dir string) ([]Installed, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 没装插件是正常状态，不是错误
		}
		return nil, []error{fmt.Errorf("读插件目录 %s 失败: %w", dir, err)}
	}
	var out []Installed
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		inst, err := readInstalled(sub)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if inst.Manifest.ID != e.Name() {
			// 目录名就是插件 id：不一致的话「改个目录名」就能换掉一个已配好
			// 渠道背后的代码，而用户配置是按 id 定位的。
			errs = append(errs, fmt.Errorf("插件目录 %s 里的 manifest id 是 %q，两者必须一致",
				e.Name(), inst.Manifest.ID))
			continue
		}
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.ID < out[j].Manifest.ID })
	return out, errs
}

func readInstalled(dir string) (Installed, error) {
	path := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Installed{}, fmt.Errorf("读 %s 失败: %w", path, err)
	}
	man, err := ParseManifest(raw)
	if err != nil {
		return Installed{}, fmt.Errorf("%s: %w", path, err)
	}
	mod := filepath.Join(dir, man.Module)
	if _, err := os.Stat(mod); err != nil {
		return Installed{}, fmt.Errorf("插件 %s 的模块文件缺失: %w", man.ID, err)
	}
	return Installed{Manifest: man, Dir: dir, ModulePath: mod}, nil
}

// ParseManifest 解析并校验一份 manifest。远端下发的内容也走这里。
func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	// 多出来的字段一律报错：出现本版本不认识的键，说明 manifest 用了更新的
	// schema，静默忽略会让插件以「少了一半约束」的样子跑起来 —— 而那些约束
	// 可能正是 allowedHosts 之类的安全项。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("解析失败: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Install 把一份插件（manifest + 模块）装进插件目录。
//
// 它不做签名验证 —— 那是目录下发那一层的事。手工装一个插件等于用户自己
// 承担来源风险，和手工往 catalog 目录放一份声明同理。
func Install(namespace, manifestPath string) (Installed, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return Installed{}, err
	}
	man, err := ParseManifest(raw)
	if err != nil {
		return Installed{}, err
	}
	srcMod := filepath.Join(filepath.Dir(manifestPath), man.Module)
	modData, err := os.ReadFile(srcMod)
	if err != nil {
		return Installed{}, fmt.Errorf("读模块 %s 失败: %w", srcMod, err)
	}

	base, err := Dir(namespace)
	if err != nil {
		return Installed{}, err
	}
	dst := filepath.Join(base, man.ID)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return Installed{}, err
	}
	if err := os.WriteFile(filepath.Join(dst, man.Module), modData, 0o600); err != nil {
		return Installed{}, err
	}
	// manifest 最后写：模块没落地就有 manifest 的话，发现阶段会报「模块缺失」。
	if err := os.WriteFile(filepath.Join(dst, "manifest.json"), raw, 0o600); err != nil {
		return Installed{}, err
	}
	return Installed{Manifest: man, Dir: dst, ModulePath: filepath.Join(dst, man.Module)}, nil
}

// Registry 按需加载已安装的插件，并在进程内复用实例。
//
// 复用是必要的：每次调用都重新编译一个 3MB 模块会让工具调用慢上一秒。
// 代价是一个实例不能并发调用（Plugin.Call 内部串行，见 host.go）。
type Registry struct {
	installed map[string]Installed

	mu      sync.Mutex
	loaded  map[string]*Plugin
	loadErr map[string]error
}

// NewRegistry 用一组已发现的插件建注册表。不加载任何模块。
func NewRegistry(items []Installed) *Registry {
	r := &Registry{
		installed: make(map[string]Installed, len(items)),
		loaded:    map[string]*Plugin{},
		loadErr:   map[string]error{},
	}
	for _, it := range items {
		r.installed[it.Manifest.ID] = it
	}
	return r
}

// Manifest 取一个已安装插件的 manifest（不加载模块）。
func (r *Registry) Manifest(id string) (Manifest, bool) {
	it, ok := r.installed[id]
	return it.Manifest, ok
}

// Manifests 返回全部已安装插件的 manifest，按 id 排序。
func (r *Registry) Manifests() []Manifest {
	out := make([]Manifest, 0, len(r.installed))
	for _, it := range r.installed {
		out = append(out, it.Manifest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get 取（必要时加载）一个插件。
//
// 加载失败会被记住：一个编译不过的模块不该在每次工具调用时都重试编译，
// 那只会把一次失败变成持续的卡顿。
func (r *Registry) Get(ctx context.Context, id string) (*Plugin, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.loaded[id]; ok {
		return p, nil
	}
	if err, ok := r.loadErr[id]; ok {
		return nil, err
	}
	it, ok := r.installed[id]
	if !ok {
		return nil, fmt.Errorf("插件 %s 未安装", id)
	}
	module, err := os.ReadFile(it.ModulePath)
	if err != nil {
		r.loadErr[id] = err
		return nil, err
	}
	p, err := Open(ctx, it.Manifest, module)
	if err != nil {
		r.loadErr[id] = err
		return nil, err
	}
	r.loaded[id] = p
	return p, nil
}

// Close 卸载所有已加载的插件。
func (r *Registry) Close(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.loaded {
		_ = p.Close(ctx)
		delete(r.loaded, id)
	}
}
