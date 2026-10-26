// wasm 插件渠道接进 hub 的那一层：描述符、连通性测试、工具装配。
//
// 和声明式渠道并列成为第三种渠道类型。页面、凭据存放、多账号分派都复用同一套
// 东西 —— 插件渠道唯一特殊的地方是「它会执行第三方代码」，而那件事必须在页面上
// 看得见，所以描述符上带了 ThirdPartyCode 和 AllowedHosts 两个披露字段。
package channels

import (
	"context"
	"fmt"
	"strings"

	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/wasmplug"
)

// KindPlugin 是 wasm 插件渠道。
const KindPlugin = "plugin"

// pluginManifests 每次调用都重新扫磁盘，和目录层的 specs() 一样 ——
// 用户装一个插件之后不必重启页面进程。
func pluginManifests() []wasmplug.Manifest {
	items, errs := wasmplug.Discover(config.Namespace)
	for _, err := range errs {
		// 一个坏插件只让自己失效，其余照常可用。
		pluginWarn(err)
	}
	out := make([]wasmplug.Manifest, 0, len(items))
	for _, it := range items {
		out = append(out, it.Manifest)
	}
	return out
}

// PluginWarnings 收集发现阶段的问题，供页面显示。
var pluginWarnings []string

func pluginWarn(err error) {
	msg := err.Error()
	for _, existing := range pluginWarnings {
		if existing == msg {
			return
		}
	}
	pluginWarnings = append(pluginWarnings, msg)
}

// PluginIssues 返回插件发现阶段的问题列表。
func PluginIssues() []string {
	out := make([]string, len(pluginWarnings))
	copy(out, pluginWarnings)
	return out
}

// pluginDescriptor 把一份插件 manifest 变成页面要的描述符。
func pluginDescriptor(m wasmplug.Manifest) Descriptor {
	fields := []Field{
		{
			Name: m.Account.Name, Label: m.Account.Label, Kind: KindText, Required: true,
			Placeholder: m.Account.Placeholder, Help: m.Account.Help,
		},
		{
			Name: m.Secret.Name, Label: m.Secret.Label, Kind: KindPassword, Required: true,
			Placeholder: m.Secret.Placeholder, Help: m.Secret.Help,
		},
	}
	var writeTools []string
	for _, t := range m.Tools {
		if !t.ReadOnly {
			writeTools = append(writeTools, t.Name)
		}
	}
	if len(writeTools) > 0 {
		fields = append(fields, Field{
			Name: wasmplug.AllowWriteField, Label: "允许写入操作", Kind: KindBool, Default: "false",
			Help: "关闭时这些工具不会注册给模型：" + strings.Join(writeTools, "、"),
		})
	}
	for _, f := range m.Fields {
		fields = append(fields, Field{
			Name: f.Name, Label: f.Label, Kind: KindText, Required: f.Required,
			Default: f.Default, Placeholder: f.Placeholder, Help: f.Help, Advanced: f.Advanced,
		})
	}

	tools := make([]string, 0, len(m.Tools))
	for _, t := range m.Tools {
		tools = append(tools, t.Name)
	}
	writeSwitch := ""
	if len(writeTools) > 0 {
		writeSwitch = wasmplug.AllowWriteField
	}

	return Descriptor{
		Type:        m.ID,
		Name:        m.Name,
		Icon:        m.Icon,
		Kind:        KindPlugin,
		Version:     m.Version,
		Tags:        m.Tags,
		Summary:     m.Summary,
		Integration: m.ID, // 插件各自一个凭据分段，不和别人共用
		// 凭据定位键沿用 manifest 声明的账号字段。
		AccountField: m.Account.Name,
		SecretField:  m.Secret.Name,
		Available:    true,
		Tools:        tools,
		WriteTools:   writeTools,
		WriteSwitch:  writeSwitch,
		// 这两个是插件渠道独有的披露：会执行第三方代码、会访问哪些主机。
		// 必须在用户点「添加」之前看得见。
		ThirdPartyCode: true,
		AllowedHosts:   m.AllowedHosts,
		Fields:         fields,
	}
}

// PluginGroup 是一个插件和它的全部已启用实例。
type PluginGroup struct {
	Manifest  wasmplug.Manifest
	Instances []wasmplug.Instance
}

// PluginGroups 把所有已启用的插件渠道按插件分组，供 MCP 启动时注册工具用。
//
// 不在这里加载模块：工具注册只需要 manifest，而编译一个 3MB 的 wasm 要上百
// 毫秒到一秒 —— 卡在 stdio 握手上会被客户端判超时。模块在第一次工具调用时
// 才由 Registry 加载。
func (m *Manager) PluginGroups() ([]PluginGroup, error) {
	views, err := m.List()
	if err != nil {
		return nil, err
	}
	var order []string
	byType := map[string][]wasmplug.Instance{}
	mans := map[string]wasmplug.Manifest{}
	for _, v := range views {
		if v.Descriptor.Kind != KindPlugin || !v.Instance.Enabled {
			continue
		}
		man, ok := m.pluginManifest(v.Instance.Type)
		if !ok {
			continue // 配置里有，但插件已被卸载
		}
		inst, err := m.pluginInstance(v.Descriptor, v.Instance.Account, nil)
		if err != nil {
			return nil, err
		}
		if _, seen := byType[v.Instance.Type]; !seen {
			order = append(order, v.Instance.Type)
			mans[v.Instance.Type] = man
		}
		byType[v.Instance.Type] = append(byType[v.Instance.Type], inst)
	}
	out := make([]PluginGroup, 0, len(order))
	for _, typ := range order {
		out = append(out, PluginGroup{Manifest: mans[typ], Instances: byType[typ]})
	}
	return out, nil
}

func (m *Manager) pluginManifest(id string) (wasmplug.Manifest, bool) {
	if m.Plugins != nil {
		if man, ok := m.Plugins.Manifest(id); ok {
			return man, true
		}
	}
	for _, man := range pluginManifests() {
		if man.ID == id {
			return man, true
		}
	}
	return wasmplug.Manifest{}, false
}

// pluginInstance 把一个插件渠道（已保存实例或表单临时值）变成 wasmplug.Instance。
func (m *Manager) pluginInstance(d Descriptor, account string, values map[string]string) (wasmplug.Instance, error) {
	var in Instance
	if len(values) > 0 {
		acc, _, settings, err := d.Validate(values)
		if err != nil {
			return wasmplug.Instance{}, err
		}
		if account == "" {
			account = acc
		}
		in = Instance{Type: d.Type, Account: account, Settings: settings}
	} else {
		got, ok, err := m.store.Get(d.Type, account)
		if err != nil {
			return wasmplug.Instance{}, err
		}
		if !ok {
			return wasmplug.Instance{}, fmt.Errorf("找不到渠道 %s/%s", d.Type, account)
		}
		in = got
	}

	ref := credstore.Ref{Integration: d.Integration, Account: in.Account}
	secret := m.creds.Secret(ref)
	if v, ok := values[d.SecretField]; ok && v != "" {
		secret = credstore.Literal(ref, v)
	}
	return wasmplug.Instance{
		Account:  in.Account,
		Settings: in.Settings,
		Secret:   secret.Value,
	}, nil
}

// testPlugin 验证一个插件渠道实例能否连通，并返回对方系统报回来的身份。
func (m *Manager) testPlugin(ctx context.Context, d Descriptor, account string, values map[string]string) (any, error) {
	if m.Plugins == nil {
		return nil, fmt.Errorf("插件运行时未初始化")
	}
	p, err := m.Plugins.Get(ctx, d.Type)
	if err != nil {
		return nil, err
	}
	inst, err := m.pluginInstance(d, account, values)
	if err != nil {
		return nil, err
	}
	return p.Test(ctx, inst)
}
