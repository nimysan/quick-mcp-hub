package wasmplug

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 发现与加载刻意分开：发现只读 manifest（启动时做），加载才编译 wasm。
// 这组测试钉住那条分界，以及「一个坏插件只让自己失效」。

func stagedPlugin(t *testing.T, dir, id string) {
	t.Helper()
	sub := filepath.Join(dir, id)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	module, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("读模块失败（先跑 plugins/build.sh）: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, id+".wasm"), module, 0o644); err != nil {
		t.Fatalf("写模块失败: %v", err)
	}
	raw, err := os.ReadFile(manifestFile)
	if err != nil {
		t.Fatalf("读 manifest 失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析 manifest 失败: %v", err)
	}
	// id 和模块名跟着目录名走，工具名也要换前缀（校验会强制这条）。
	old := m["id"].(string)
	m["id"] = id
	m["module"] = id + ".wasm"
	for _, tv := range m["tools"].([]any) {
		tool := tv.(map[string]any)
		tool["name"] = strings.Replace(tool["name"].(string), old+"_", id+"_", 1)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "manifest.json"), out, 0o644); err != nil {
		t.Fatalf("写 manifest 失败: %v", err)
	}
}

func TestDiscoverReadsManifestsOnly(t *testing.T) {
	dir := t.TempDir()
	stagedPlugin(t, dir, "ghwasm")
	stagedPlugin(t, dir, "second")

	items, errs := DiscoverIn(dir)
	if len(errs) != 0 {
		t.Fatalf("发现阶段报错: %v", errs)
	}
	if len(items) != 2 {
		t.Fatalf("发现了 %d 个插件，期望 2", len(items))
	}
	// 按 id 排序，输出稳定。
	if items[0].Manifest.ID != "ghwasm" || items[1].Manifest.ID != "second" {
		t.Fatalf("顺序不对: %s %s", items[0].Manifest.ID, items[1].Manifest.ID)
	}
	if items[0].Manifest.Tools[0].Name != "ghwasm_whoami" {
		t.Fatalf("工具名不对: %s", items[0].Manifest.Tools[0].Name)
	}
}

func TestDiscoverMissingDirIsNotAnError(t *testing.T) {
	items, errs := DiscoverIn(filepath.Join(t.TempDir(), "nope"))
	if len(items) != 0 || len(errs) != 0 {
		// 没装插件是正常状态。报成错误会让「还没装插件」看起来像坏了。
		t.Fatalf("items=%v errs=%v，期望都为空", items, errs)
	}
}

// TestOneBadPluginDoesNotHideTheRest：装了个坏插件不该让整个程序起不来。
func TestOneBadPluginDoesNotHideTheRest(t *testing.T) {
	dir := t.TempDir()
	stagedPlugin(t, dir, "ghwasm")

	bad := filepath.Join(dir, "broken")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "manifest.json"), []byte(`{"abi":1,"id":"broken"`), 0o644); err != nil {
		t.Fatal(err)
	}

	items, errs := DiscoverIn(dir)
	if len(items) != 1 || items[0].Manifest.ID != "ghwasm" {
		t.Fatalf("好插件没被保留: %v", items)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken") {
		t.Fatalf("坏插件没被单独报告: %v", errs)
	}
}

// TestDirNameMustMatchID：目录名就是插件 id。不一致的话，「改个目录名」
// 就能换掉一个已配好渠道背后的代码，而用户配置是按 id 定位的。
func TestDirNameMustMatchID(t *testing.T) {
	dir := t.TempDir()
	stagedPlugin(t, dir, "ghwasm")
	if err := os.Rename(filepath.Join(dir, "ghwasm"), filepath.Join(dir, "renamed")); err != nil {
		t.Fatal(err)
	}
	items, errs := DiscoverIn(dir)
	if len(items) != 0 {
		t.Fatalf("目录名和 id 不一致竟然通过了: %v", items)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "必须一致") {
		t.Fatalf("errs = %v", errs)
	}
}

func TestMissingModuleIsReported(t *testing.T) {
	dir := t.TempDir()
	stagedPlugin(t, dir, "ghwasm")
	if err := os.Remove(filepath.Join(dir, "ghwasm", "ghwasm.wasm")); err != nil {
		t.Fatal(err)
	}
	items, errs := DiscoverIn(dir)
	if len(items) != 0 {
		t.Fatal("模块缺失竟然算发现成功")
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "模块文件缺失") {
		t.Fatalf("errs = %v", errs)
	}
}

// TestRegistryLoadsLazilyAndReuses：注册表不在建立时编译模块，
// 第一次 Get 才加载，之后复用同一个实例。
func TestRegistryLoadsLazilyAndReuses(t *testing.T) {
	dir := t.TempDir()
	stagedPlugin(t, dir, "ghwasm")
	items, _ := DiscoverIn(dir)

	reg := NewRegistry(items)
	t.Cleanup(func() { reg.Close(context.Background()) })

	// manifest 立刻可用（工具注册只要这个），但还没加载任何模块。
	if man, ok := reg.Manifest("ghwasm"); !ok || len(man.Tools) != 6 {
		t.Fatalf("manifest 不可用: ok=%v", ok)
	}
	if len(reg.loaded) != 0 {
		t.Fatal("建注册表时就加载了模块")
	}

	p1, err := reg.Get(context.Background(), "ghwasm")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	p2, err := reg.Get(context.Background(), "ghwasm")
	if err != nil {
		t.Fatalf("第二次 Get 失败: %v", err)
	}
	if p1 != p2 {
		t.Fatal("同一个插件被加载了两次 —— 每次调用都重编译 3MB 模块会慢上一秒")
	}
	if _, err := reg.Get(context.Background(), "nope"); err == nil {
		t.Fatal("未安装的插件竟然能取到")
	}
}

// TestRegistryRemembersLoadFailure：编译不过的模块不该在每次调用时重试，
// 那只会把一次失败变成持续的卡顿。
func TestRegistryRemembersLoadFailure(t *testing.T) {
	dir := t.TempDir()
	stagedPlugin(t, dir, "ghwasm")
	items, _ := DiscoverIn(dir)
	// 把模块内容换成垃圾：manifest 仍然合法，加载必然失败。
	if err := os.WriteFile(items[0].ModulePath, []byte("not wasm at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(items)
	t.Cleanup(func() { reg.Close(context.Background()) })

	if _, err := reg.Get(context.Background(), "ghwasm"); err == nil {
		t.Fatal("垃圾模块竟然加载成功")
	}
	if _, err := reg.Get(context.Background(), "ghwasm"); err == nil {
		t.Fatal("第二次也该失败")
	}
	if len(reg.loadErr) != 1 {
		t.Fatalf("失败没有被记住: %v", reg.loadErr)
	}
}

// TestInstallCopiesBothFiles：安装要把 manifest 和模块都搬过去，
// 并且 manifest 最后写 —— 否则发现阶段会读到一个指向不存在模块的 manifest。
func TestInstallCopiesBothFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	src := t.TempDir()
	stagedPlugin(t, src, "ghwasm")

	it, err := Install("mcp-hub-test", filepath.Join(src, "ghwasm", "manifest.json"))
	if err != nil {
		t.Fatalf("Install 失败: %v", err)
	}
	if _, err := os.Stat(it.ModulePath); err != nil {
		t.Fatalf("模块没装过去: %v", err)
	}
	if _, err := os.Stat(filepath.Join(it.Dir, "manifest.json")); err != nil {
		t.Fatalf("manifest 没装过去: %v", err)
	}
	// 装完之后 Discover 应当能发现它。
	items, errs := Discover("mcp-hub-test")
	if len(items) != 1 || len(errs) != 0 {
		t.Fatalf("装完发现不到: items=%v errs=%v", items, errs)
	}
}
