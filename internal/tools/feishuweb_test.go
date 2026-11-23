package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nimysan/mcp-hub/internal/feishuweb"
)

type fakeRunner struct{ calls []string }

func (f *fakeRunner) Do(_ context.Context, kind, u string) (feishuweb.Result, error) {
	f.calls = append(f.calls, kind+" "+u)
	return feishuweb.Result{Status: feishuweb.StatusOK, Markdown: "正文"}, nil
}

func (f *fakeRunner) Search(_ context.Context, q, host string) (feishuweb.Result, error) {
	f.calls = append(f.calls, "search "+q+"@"+host)
	return feishuweb.Result{Status: feishuweb.StatusOK, Items: []feishuweb.Item{{Title: "周报", URL: "https://x.feishu.cn/docx/AbCdEf123456", Type: "docx"}}}, nil
}

// 三个工具全部标只读；不是飞书文档的地址不发给扩展；检索的关键词和域名先在这一侧校验。
func TestFeishuWebTools(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	run := &fakeRunner{}
	if n := RegisterFeishuWeb(server, run); n != 3 {
		t.Fatalf("注册了 %d 个工具", n)
	}
	cs := connect(t, server)
	ctx := context.Background()

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s 没标只读", tool.Name)
		}
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "feishu_web_read_doc",
		Arguments: map[string]any{"url": "https://evil.example/docx/AbCdEf123456"}})
	if err != nil || !res.IsError {
		t.Errorf("非飞书地址应当报错：%v %+v", err, res)
	}
	if len(run.calls) != 0 {
		t.Errorf("非飞书地址不该交给扩展：%v", run.calls)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "feishu_web_read_doc",
		Arguments: map[string]any{"url": "https://x.feishu.cn/docx/AbCdEf123456?from=1"}})
	if err != nil || res.IsError {
		t.Fatalf("读取失败：%v %+v", err, res)
	}
	if len(run.calls) != 1 || run.calls[0] != "read_doc https://x.feishu.cn/docx/AbCdEf123456" {
		t.Errorf("交出去的任务不对：%v", run.calls)
	}

	for _, bad := range []map[string]any{
		{"query": "  "},
		{"query": strings.Repeat("长", 101)},
		{"query": "周报", "host": "evil.example"},
		{"query": "周报", "host": "accounts.feishu.cn"},
	} {
		res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "feishu_web_search", Arguments: bad})
		if err != nil || !res.IsError {
			t.Errorf("%v 应当报错：%v %+v", bad, err, res)
		}
	}
	if len(run.calls) != 1 {
		t.Fatalf("不合法的检索不该交给扩展：%v", run.calls)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "feishu_web_search",
		Arguments: map[string]any{"query": " 周报\n 2026 ", "host": "https://X.feishu.cn/docx/abc"}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if got := run.calls[len(run.calls)-1]; got != "search 周报 2026@x.feishu.cn" {
		t.Errorf("交出去的检索不对：%q", got)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, `"type":"docx"`) {
		t.Errorf("检索结果不对：%s", text)
	}
}

type scriptedRunner struct {
	res feishuweb.Result
	err error
}

func (s scriptedRunner) Do(context.Context, string, string) (feishuweb.Result, error) {
	return s.res, s.err
}
func (s scriptedRunner) Search(context.Context, string, string) (feishuweb.Result, error) {
	return s.res, s.err
}

// 统一返回形态（BUG-04）：每条结果都带 structuredContent 和 status；参数 / 环境错误
// isError=true，业务状态 isError=false；recent 和 search 的 items 总是在（BUG-07）。
func TestFeishuWebUniformResults(t *testing.T) {
	ctx := context.Background()
	call := func(run FeishuWebRunner, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
		t.Helper()
		server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
		RegisterFeishuWeb(server, run)
		res, err := connect(t, server).CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("%s %v 没有 structuredContent：%+v", name, args, res)
		}
		return res, sc
	}
	doc := map[string]any{"url": "https://x.feishu.cn/docx/AbCdEf123456"}
	for _, c := range []struct {
		name    string
		run     FeishuWebRunner
		tool    string
		args    map[string]any
		status  string
		isError bool
	}{
		{"假 token", scriptedRunner{res: feishuweb.Result{Status: feishuweb.StatusNotFound, Message: "不存在"}}, "feishu_web_read_doc", doc, "not_found", false},
		{"没权限", scriptedRunner{res: feishuweb.Result{Status: feishuweb.StatusNoPermission, Message: "没权限"}}, "feishu_web_read_doc", doc, "no_permission", false},
		{"拒读", scriptedRunner{res: feishuweb.Result{Status: feishuweb.StatusRefused, Reason: "copy_restricted"}}, "feishu_web_read_doc", doc, "refused", false},
		{"扩展没连", scriptedRunner{err: feishuweb.ErrNotConnected}, "feishu_web_read_doc", doc, "extension_disconnected", true},
		{"配置页面回的失败", scriptedRunner{res: feishuweb.Result{Status: feishuweb.StatusExtensionDisconnected, Message: "没连"}}, "feishu_web_recent", nil, "extension_disconnected", true},
		{"外站地址", scriptedRunner{}, "feishu_web_read_doc", map[string]any{"url": "https://evil.example/docx/AbCdEf123456"}, "invalid_argument", true},
		{"电子表格", scriptedRunner{}, "feishu_web_read_doc", map[string]any{"url": "https://x.feishu.cn/sheets/AbCdEf123456"}, "unsupported_type", false},
		{"recent 空", scriptedRunner{res: feishuweb.Result{Status: feishuweb.StatusOK}}, "feishu_web_recent", nil, "ok", false},
		{"检索认不出结构", scriptedRunner{res: feishuweb.Result{Status: feishuweb.StatusStructureUnknown, Message: "认不出"}}, "feishu_web_search", map[string]any{"query": "周报"}, "structure_unknown", false},
		{"检索 host 不对", scriptedRunner{}, "feishu_web_search", map[string]any{"query": "周报", "host": "www.feishu.cn"}, "invalid_argument", true},
		{"检索关键词空", scriptedRunner{}, "feishu_web_search", map[string]any{"query": " "}, "invalid_argument", true},
	} {
		res, sc := call(c.run, c.tool, c.args)
		if sc["status"] != c.status || res.IsError != c.isError {
			t.Errorf("%s：status=%v isError=%v，期望 %s %v", c.name, sc["status"], res.IsError, c.status, c.isError)
		}
		if c.status != "ok" && c.status != "refused" && sc["message"] == "" {
			t.Errorf("%s：没有 message：%v", c.name, sc)
		}
		if c.tool != "feishu_web_read_doc" {
			if items, ok := sc["items"].([]any); !ok || len(items) != 0 {
				t.Errorf("%s：items 应当是 []：%v", c.name, sc)
			}
		}
	}
}
