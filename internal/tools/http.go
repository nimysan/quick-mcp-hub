// 把声明式 HTTPS 渠道的工具注册给 MCP。
//
// 邮件那几个工具是泛型 AddTool（编译期结构体 -> schema）；这里的工具在编译期
// 还不存在，参数表来自 Spec 数据，所以走非泛型的 Server.AddTool，自己生成
// JSON Schema 并自己解参数。
//
// 多实例的处理方式：一个渠道类型配了几个账号，仍然只注册一套工具，
// 由 account 参数选账号。另一条路（给工具名加账号后缀）会让工具数随账号
// 线性膨胀，而客户端（Amazon Quick）单 server 上限 100 个工具、且新增工具
// 后要用户手动 Sync 才可见 —— 固定的工具名把这两个麻烦都避开了。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nimysan/mcp-hub/internal/httpchan"
)

// accountParam 是多实例时自动加上的参数名。
// 它不能和渠道声明里的参数撞名，Spec.Validate 会拒绝占用它的声明。
const accountParam = httpchan.ReservedAccountParam

// RegisterHTTP 把一个渠道类型下的全部实例注册成一套工具，返回注册的工具数。
//
// 非只读工具只在至少一个实例打开写开关时注册 —— 不注册比注册后拒绝更安全：
// 模型看不到的工具不会被诱导去调。真正调用时再按选中的实例校一次开关，
// 所以「A 账号可写、B 账号只读」是成立的。
func RegisterHTTP(s *mcp.Server, rts []httpchan.Runtime) int {
	if len(rts) == 0 {
		return 0
	}
	spec := rts[0].Spec
	client := new(httpchan.Client)

	byAccount := make(map[string]httpchan.Runtime, len(rts))
	accounts := make([]string, 0, len(rts))
	var writable []string
	for _, rt := range rts {
		byAccount[rt.Account] = rt
		accounts = append(accounts, rt.Account)
		if rt.AllowWrite() {
			writable = append(writable, rt.Account)
		}
	}

	n := 0
	for _, t := range spec.Tools {
		if !t.ReadOnly && len(writable) == 0 {
			log.Printf("%s 未注册：%s 的所有账号都没打开写开关", t.Name, spec.Name)
			continue
		}
		tool := t // 捕获循环变量
		s.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Title:       tool.Title,
			Description: describe(spec, tool, accounts, writable),
			InputSchema: inputSchema(tool, accounts),
			Annotations: annotations(tool),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return errorResult(fmt.Errorf("参数不是 JSON 对象: %w", err)), nil
				}
			}
			rt, err := pickRuntime(byAccount, accounts, args)
			if err != nil {
				return errorResult(err), nil
			}
			// account 是本层的分派参数，不属于渠道声明，不能传给执行器。
			delete(args, accountParam)

			out, err := client.Call(ctx, rt, tool.Name, args)
			if err != nil {
				// 业务错误回进 Content 而不是协议错误，模型才能看见并自我纠正。
				return errorResult(err), nil
			}
			text, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return errorResult(err), nil
			}
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: string(text)}},
				StructuredContent: out,
			}, nil
		})
		n++
	}
	return n
}

// pickRuntime 按 account 参数选实例。单实例时不需要这个参数；
// 多实例时必须明确给出 —— 替模型猜一个默认账号，在写入类工具上就是选错对象。
func pickRuntime(byAccount map[string]httpchan.Runtime, accounts []string, args map[string]any) (httpchan.Runtime, error) {
	if len(accounts) == 1 {
		return byAccount[accounts[0]], nil
	}
	raw, _ := args[accountParam].(string)
	acc := strings.TrimSpace(raw)
	if acc == "" {
		return httpchan.Runtime{}, fmt.Errorf("配了多个账号，必须用 %s 指明用哪个：%s",
			accountParam, strings.Join(accounts, " / "))
	}
	rt, ok := byAccount[acc]
	if !ok {
		return httpchan.Runtime{}, fmt.Errorf("没有账号 %q，可用的是：%s", acc, strings.Join(accounts, " / "))
	}
	return rt, nil
}

// describe 在声明的描述后面补上账号情况。
//
// 写成模型能据以行动的句子，而不是括号里的开发备注：它要能直接读出
// 「要不要传账号、可选哪几个、哪些账号会被拒」。
func describe(spec httpchan.Spec, t httpchan.Tool, accounts, writable []string) string {
	var b strings.Builder
	b.WriteString(t.Description)
	if len(accounts) == 1 {
		fmt.Fprintf(&b, " 当前 %s 账号是 %s。", spec.Name, accounts[0])
	} else {
		fmt.Fprintf(&b, " 配了 %d 个 %s 账号，必须用 %s 指定一个：%s。",
			len(accounts), spec.Name, accountParam, strings.Join(accounts, "、"))
	}
	if !t.ReadOnly && len(writable) < len(accounts) {
		fmt.Fprintf(&b, " 其中只有 %s 允许写入，其他账号会被拒绝。", strings.Join(writable, "、"))
	}
	return b.String()
}

func annotations(t httpchan.Tool) *mcp.ToolAnnotations {
	if t.ReadOnly {
		return readOnly()
	}
	return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no, OpenWorldHint: &yes}
}

// inputSchema 按参数表生成 JSON Schema。
// 用 map 而不是结构体：Server.AddTool 只要求能 marshal 成 type=object 的 schema。
func inputSchema(t httpchan.Tool, accounts []string) map[string]any {
	props := map[string]any{}
	var required []string

	if len(accounts) > 1 {
		props[accountParam] = map[string]any{
			"type":        "string",
			"description": "用哪个账号执行",
			"enum":        toAny(accounts),
		}
		required = append(required, accountParam)
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

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}
