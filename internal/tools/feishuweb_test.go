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

// 三个工具全部标只读；不是飞书文档的地址不发给扩展；检索如实回 not_implemented。
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

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "feishu_web_search",
		Arguments: map[string]any{"query": "周报"}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, feishuweb.StatusNotImplemented) {
		t.Errorf("检索应当回 not_implemented：%s", text)
	}
}
