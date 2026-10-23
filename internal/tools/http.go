// 把声明式 HTTPS 渠道的工具注册给 MCP。
//
// 邮件那几个工具是泛型 AddTool（编译期结构体 -> schema）；这里的工具在编译期
// 还不存在，参数表来自 Spec 数据，所以走非泛型的 Server.AddTool，自己生成
// JSON Schema 并自己解参数。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nimysan/mcp-hub/internal/httpchan"
)

// RegisterHTTP 把一个渠道实例声明出来的工具挂到 server 上，返回注册的工具数。
//
// 非只读工具只在渠道打开写开关时注册 —— 不注册比注册后拒绝更安全：
// 模型看不到的工具不会被诱导去调。
func RegisterHTTP(s *mcp.Server, rt httpchan.Runtime) int {
	client := new(httpchan.Client)
	n := 0
	for _, t := range rt.Spec.Tools {
		if !t.ReadOnly && !rt.AllowWrite() {
			log.Printf("%s 未注册：%s 的写开关未打开", t.Name, rt.Spec.Name)
			continue
		}
		tool := t // 捕获循环变量
		s.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Title:       tool.Title,
			Description: describe(rt, tool),
			InputSchema: inputSchema(tool),
			Annotations: annotations(tool),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return errorResult(fmt.Errorf("参数不是 JSON 对象: %w", err)), nil
				}
			}
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

// describe 在声明的描述后面补上账号，多账号时模型才分得清调的是哪个。
func describe(rt httpchan.Runtime, t httpchan.Tool) string {
	return fmt.Sprintf("%s（%s 账号 %s）", t.Description, rt.Spec.Name, rt.Account)
}

func annotations(t httpchan.Tool) *mcp.ToolAnnotations {
	if t.ReadOnly {
		return readOnly()
	}
	return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no, OpenWorldHint: &yes}
}

// inputSchema 按参数表生成 JSON Schema。
// 用 map 而不是结构体：Server.AddTool 只要求能 marshal 成 type=object 的 schema。
func inputSchema(t httpchan.Tool) map[string]any {
	props := map[string]any{}
	var required []string
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
			vals := make([]any, len(p.Enum))
			for i, v := range p.Enum {
				vals[i] = v
			}
			prop["enum"] = vals
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

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}
