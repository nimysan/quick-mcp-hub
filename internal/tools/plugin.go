// 把 wasm 插件渠道的工具注册给 MCP。
//
// 和声明式渠道同一套形状：参数表来自数据（manifest）所以走非泛型 AddTool，
// 多实例只注册一套工具由 channel_account 选账号，非只读工具要有账号打开写
// 开关才注册。差别只有一处 —— 执行落到沙箱里的第三方代码，而模块是**第一次
// 调用时**才编译的，所以注册阶段不碰 wasm。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nimysan/mcp-hub/internal/wasmplug"
)

// PluginLoader 按插件 id 取已加载的插件（wasmplug.Registry 实现它）。
//
// 取成接口而不是直接依赖 Registry：注册时只要能在调用时拿到插件就够了，
// 测试里可以塞一个现成的实例，不必摆一个磁盘上的插件目录。
type PluginLoader interface {
	Get(ctx context.Context, id string) (*wasmplug.Plugin, error)
}

// RegisterPlugin 把一个插件下的全部实例注册成一套工具，返回注册的工具数。
func RegisterPlugin(s *mcp.Server, loader PluginLoader, man wasmplug.Manifest, insts []wasmplug.Instance) int {
	if len(insts) == 0 {
		return 0
	}
	byAccount := make(map[string]wasmplug.Instance, len(insts))
	accounts := make([]string, 0, len(insts))
	var writable []string
	for _, in := range insts {
		byAccount[in.Account] = in
		accounts = append(accounts, in.Account)
		if in.AllowWrite() {
			writable = append(writable, in.Account)
		}
	}

	n := 0
	for _, t := range man.Tools {
		if !t.ReadOnly && len(writable) == 0 {
			log.Printf("%s 未注册：%s 的所有账号都没打开写开关", t.Name, man.Name)
			continue
		}
		tool := t
		s.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Title:       tool.Title,
			Description: describePlugin(man, tool, accounts, writable),
			InputSchema: pluginInputSchema(tool, accounts),
			Annotations: pluginAnnotations(tool),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return errorResult(fmt.Errorf("参数不是 JSON 对象: %w", err)), nil
				}
			}
			inst, err := pickInstance(byAccount, accounts, args)
			if err != nil {
				return errorResult(err), nil
			}
			delete(args, wasmplug.ReservedAccountParam)
			// 必填项在宿主侧校验：「参数够不够」这件事不该由被审的那一方说。
			if err := requireParams(tool, args); err != nil {
				return errorResult(err), nil
			}

			// 模块在这里才被加载（并在进程内复用）。编译一个 3MB 的 wasm 要
			// 上百毫秒，放在注册阶段会卡住 stdio 握手。
			p, err := loader.Get(ctx, man.ID)
			if err != nil {
				return errorResult(fmt.Errorf("加载插件 %s 失败: %w", man.Name, err)), nil
			}
			out, meta, err := p.Call(ctx, inst, tool.Name, args)
			if err != nil {
				return errorResult(err), nil
			}
			res := structured(out, meta)
			text, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				return errorResult(err), nil
			}
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: string(text)}},
				StructuredContent: res,
			}, nil
		})
		n++
	}
	return n
}

// pickInstance 按 channel_account 参数选实例。单实例时不需要这个参数；
// 多实例时必须明确给出 —— 替模型猜一个账号，在写入类工具上就是选错对象。
func pickInstance(byAccount map[string]wasmplug.Instance, accounts []string, args map[string]any) (wasmplug.Instance, error) {
	if len(accounts) == 1 {
		return byAccount[accounts[0]], nil
	}
	raw, _ := args[wasmplug.ReservedAccountParam].(string)
	acc := strings.TrimSpace(raw)
	if acc == "" {
		return wasmplug.Instance{}, fmt.Errorf("配了多个账号，必须用 %s 指明用哪个：%s",
			wasmplug.ReservedAccountParam, strings.Join(accounts, " / "))
	}
	in, ok := byAccount[acc]
	if !ok {
		return wasmplug.Instance{}, fmt.Errorf("没有账号 %q，可用的是：%s", acc, strings.Join(accounts, " / "))
	}
	return in, nil
}

// requireParams 检查必填参数。
func requireParams(t wasmplug.Tool, args map[string]any) error {
	var missing []string
	for _, p := range t.Params {
		if !p.Required {
			continue
		}
		v, ok := args[p.Name]
		if !ok || v == nil || v == "" {
			missing = append(missing, p.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("参数 %s 必填", strings.Join(missing, "、"))
	}
	return nil
}

func describePlugin(man wasmplug.Manifest, t wasmplug.Tool, accounts, writable []string) string {
	var b strings.Builder
	b.WriteString(t.Description)
	if len(accounts) == 1 {
		fmt.Fprintf(&b, " 当前 %s 账号是 %s。", man.Name, accounts[0])
	} else {
		fmt.Fprintf(&b, " 配了 %d 个 %s 账号，必须用 %s 指定一个：%s。",
			len(accounts), man.Name, wasmplug.ReservedAccountParam, strings.Join(accounts, "、"))
	}
	if !t.ReadOnly && len(writable) < len(accounts) {
		fmt.Fprintf(&b, " 其中只有 %s 允许写入，其他账号会被拒绝。", strings.Join(writable, "、"))
	}
	return b.String()
}

func pluginAnnotations(t wasmplug.Tool) *mcp.ToolAnnotations {
	if t.ReadOnly {
		return readOnly()
	}
	return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no, OpenWorldHint: &yes}
}

func pluginInputSchema(t wasmplug.Tool, accounts []string) map[string]any {
	props := map[string]any{}
	var required []string

	if len(accounts) > 1 {
		props[wasmplug.ReservedAccountParam] = map[string]any{
			"type":        "string",
			"description": "用哪个账号执行",
			"enum":        toAny(accounts),
		}
		required = append(required, wasmplug.ReservedAccountParam)
	}
	for _, p := range t.Params {
		typ := p.Type
		if typ == "" {
			typ = "string"
		}
		prop := map[string]any{"type": typ}
		if p.Description != "" {
			prop["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			prop["enum"] = toAny(p.Enum)
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
